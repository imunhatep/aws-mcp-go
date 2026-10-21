package mcpserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/rs/zerolog/log"

	"github.com/imunhatep/awslib/provider"
	ptypes "github.com/imunhatep/awslib/provider/types"
	v3 "github.com/imunhatep/awslib/provider/v3"

	"github.com/imunhatep/aws-mcp-go/pkg/errors"
)

// Credential passthrough: the MCP client resolves AWS credentials on its own
// machine — from its ~/.aws/config and the SSO cache `aws sso login` fills — and
// forwards them on every request in CredentialsHeader. The server keeps no
// credentials of its own and never falls back to any: a request without the
// header is refused, because quietly answering with whatever identity the server
// happens to hold is the silent-wrong-answer failure this codebase designs
// against.
//
// On the client side the header is produced by `aws-mcp credentials-headers`,
// registered as the MCP server's headersHelper. Claude Code re-runs that helper
// on every (re)connect and — the property refreshExpiredCredentials relies on —
// when a request is answered with 401/403, then retries the request once. So an
// expired credential set is answered with a 401 rather than a tool error, and
// the client refreshes it with nobody involved.

// CredentialsHeader is the request header carrying forwarded AWS credentials: a
// base64url-encoded PassthroughCredentials document.
const CredentialsHeader = "X-Aws-Credentials"

// PassthroughWireVersion is the PassthroughCredentials document version this
// server reads and the credentials-headers helper writes.
const PassthroughWireVersion = 1

// maxCredentialsHeaderBytes bounds the header before it is decoded. A forwarded
// set is ~1.5 KB per profile, so this leaves room for a few hundred profiles
// while refusing anything that is clearly not a credential document.
const maxCredentialsHeaderBytes = 512 << 10

// credentialExpirySkew is how close to its expiry a forwarded credential is
// treated as already expired by the refresh middleware. An all-region fan-out
// takes a while, and credentials that lapse half-way through it produce a
// partial answer with credential errors in it, rather than a clean refresh.
const credentialExpirySkew = time.Minute

// passthroughIdleTTL drops a forwarded credential set's pool once no request has
// used it for this long. A refreshed set hashes differently and gets a new pool,
// so without this every refresh would leave the previous one behind.
const passthroughIdleTTL = 30 * time.Minute

var accountIDPattern = regexp.MustCompile(`^[0-9]{12}$`)

// errNoPassthroughCredentials is what a request without CredentialsHeader gets.
var errNoPassthroughCredentials = errors.New("this request carries no AWS credentials: the server runs with --credentials-passthrough and only ever uses credentials the MCP client forwards in the " + CredentialsHeader + " header. Register the server with a headersHelper that runs `aws-mcp credentials-headers --profiles <names>` (see docs/authentication.md), then reconnect it")

// PassthroughCredentials is the document carried in CredentialsHeader.
type PassthroughCredentials struct {
	Version int `json:"version"`
	// Profiles are the credential sets to serve, one account each.
	Profiles []PassthroughProfile `json:"profiles"`
	// Errors are profiles the client could not export credentials for. They
	// are forwarded rather than dropped so the server can say which account is
	// missing and how to fix it, instead of answering as if it did not exist.
	Errors []PassthroughError `json:"errors,omitempty"`
}

// PassthroughProfile is one forwarded credential set.
type PassthroughProfile struct {
	// Profile is the client-side profile name, for messages only.
	Profile string `json:"profile,omitempty"`
	// AccountID, when set, lets account-scoped queries skip this set without
	// any AWS call; it is checked against STS on first use.
	AccountID       string    `json:"account_id,omitempty"`
	Region          string    `json:"region,omitempty"`
	AccessKeyID     string    `json:"access_key_id"`
	SecretAccessKey string    `json:"secret_access_key"`
	SessionToken    string    `json:"session_token,omitempty"`
	Expiration      time.Time `json:"expiration,omitzero"`
	// LoginHint is the client-side command that renews this profile's session
	// (e.g. `aws sso login --sso-session corp`).
	LoginHint string `json:"login_hint,omitempty"`
}

// PassthroughError is a profile the client failed to export credentials for.
type PassthroughError struct {
	Profile   string `json:"profile"`
	AccountID string `json:"account_id,omitempty"`
	Error     string `json:"error"`
	LoginHint string `json:"login_hint,omitempty"`
}

// Label names the credential set in messages.
func (p PassthroughProfile) Label() string {
	switch {
	case p.Profile != "":
		return p.Profile
	case p.AccountID != "":
		return "account " + p.AccountID
	default:
		return "(forwarded credentials)"
	}
}

// loginHint is the command that renews this set on the client machine.
func (p PassthroughProfile) loginHint() string {
	if p.LoginHint != "" {
		return p.LoginHint
	}

	if p.Profile != "" {
		return "aws sso login --profile " + p.Profile
	}

	return "aws sso login"
}

// expiresWithin reports whether the set is expired, or will be within d.
// Credentials without an expiry (long-lived access keys) never expire here.
func (p PassthroughProfile) expiresWithin(now time.Time, d time.Duration) bool {
	return !p.Expiration.IsZero() && !now.Add(d).Before(p.Expiration)
}

// EncodePassthroughCredentials serializes a document into the header value.
func EncodePassthroughCredentials(doc PassthroughCredentials) (string, error) {
	doc.Version = PassthroughWireVersion

	raw, err := json.Marshal(doc)
	if err != nil {
		return "", errors.WithStack(err)
	}

	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// DecodePassthroughCredentials parses and validates a header value. Both base64
// alphabets are accepted, padded or not, since a hand-rolled helper is as likely
// to use `base64` from coreutils as the URL-safe form the bundled one writes.
func DecodePassthroughCredentials(value string) (*PassthroughCredentials, error) {
	value = strings.TrimSpace(value)

	if len(value) > maxCredentialsHeaderBytes {
		return nil, errors.Errorf("%s header is %d bytes, over the %d byte limit", CredentialsHeader, len(value), maxCredentialsHeaderBytes)
	}

	trimmed := strings.TrimRight(value, "=")

	raw, err := base64.RawURLEncoding.DecodeString(trimmed)
	if err != nil {
		raw, err = base64.RawStdEncoding.DecodeString(trimmed)
	}

	if err != nil {
		return nil, errors.Errorf("%s header is not valid base64", CredentialsHeader)
	}

	doc := &PassthroughCredentials{}
	if err := json.Unmarshal(raw, doc); err != nil {
		// The JSON error is not echoed: it can quote the offending bytes, and
		// those bytes are credentials.
		return nil, errors.Errorf("%s header does not hold a JSON credential document", CredentialsHeader)
	}

	if err := doc.validate(); err != nil {
		return nil, err
	}

	return doc, nil
}

// validate rejects documents the server cannot serve unambiguously.
func (d *PassthroughCredentials) validate() error {
	if d.Version != PassthroughWireVersion {
		return errors.Errorf("%s document version %d is not supported (this server reads version %d); use the credentials-headers helper from the same aws-mcp release", CredentialsHeader, d.Version, PassthroughWireVersion)
	}

	if len(d.Profiles) == 0 && len(d.Errors) == 0 {
		return errors.Errorf("%s document lists no credentials", CredentialsHeader)
	}

	seen := map[string]bool{}

	for i, p := range d.Profiles {
		if p.AccessKeyID == "" || p.SecretAccessKey == "" {
			return errors.Errorf("%s profile #%d (%s) is missing access_key_id or secret_access_key", CredentialsHeader, i+1, p.Label())
		}

		if p.AccountID != "" && !accountIDPattern.MatchString(p.AccountID) {
			return errors.Errorf("%s profile %s has a malformed account_id %q", CredentialsHeader, p.Label(), p.AccountID)
		}

		if seen[p.Label()] {
			return errors.Errorf("%s lists profile %s twice", CredentialsHeader, p.Label())
		}

		seen[p.Label()] = true
	}

	for _, e := range d.Errors {
		if e.AccountID != "" && !accountIDPattern.MatchString(e.AccountID) {
			return errors.Errorf("%s error entry %s has a malformed account_id %q", CredentialsHeader, e.Profile, e.AccountID)
		}
	}

	return nil
}

// expired returns the labels of the sets that expire within d of now.
func (d *PassthroughCredentials) expired(now time.Time, skew time.Duration) []string {
	labels := []string{}

	for _, p := range d.Profiles {
		if p.expiresWithin(now, skew) {
			labels = append(labels, p.Label())
		}
	}

	return labels
}

// loginHints lists the renewal commands for every profile in the document.
func (d *PassthroughCredentials) loginHints() []string {
	seen := map[string]bool{}
	hints := []string{}

	add := func(hint string) {
		if hint != "" && !seen[hint] {
			seen[hint] = true
			hints = append(hints, hint)
		}
	}

	for _, e := range d.Errors {
		add(forwardedErrorHint(e))
	}

	for _, p := range d.Profiles {
		add(p.loginHint())
	}

	return hints
}

func forwardedErrorHint(e PassthroughError) string {
	if e.LoginHint != "" {
		return e.LoginHint
	}

	return "aws sso login --profile " + e.Profile
}

// forwardedError is the error a profile the client could not export reports.
func forwardedError(e PassthroughError) error {
	return errors.Errorf("aws profile %q: the MCP client's credentials helper could not export credentials (%s); on the client machine run `%s`, then reconnect the MCP server so the helper exports them again", e.Profile, e.Error, forwardedErrorHint(e))
}

// passthroughExpiredError is what an expired set reports once a request gets
// past the refresh middleware anyway — a client that does not re-run its helper
// on 401, or one whose helper handed back the same stale set.
func passthroughExpiredError(p *PassthroughProfile) error {
	return errors.Errorf("aws profile %q: the forwarded credentials expired at %s; reconnect the MCP server so its headers helper exports fresh ones (if that does not help, run `%s` on the client machine first)", p.Label(), p.Expiration.UTC().Format(time.RFC3339), p.loginHint())
}

// explainPassthrough wraps an AWS failure of a forwarded set. Only a credential
// failure gets login advice: a disabled region or a missing IAM grant is not
// fixed by logging in again.
func explainPassthrough(p *PassthroughProfile, err error) error {
	if !IsCredentialFailure(err) {
		return errors.WithStack(err)
	}

	return errors.Wrapf(err, "aws profile %q: AWS rejected the credentials the MCP client forwarded; on the client machine run `%s`, then reconnect the MCP server so its headers helper exports fresh ones", p.Label(), p.loginHint())
}

// passthroughRequest is the parsed CredentialsHeader of one HTTP request.
type passthroughRequest struct {
	present bool
	key     string
	creds   *PassthroughCredentials
	err     error
}

type passthroughContextKey struct{}

// parsePassthroughRequest reads CredentialsHeader. key identifies the credential
// set for pool reuse; it is a digest, so the registry never holds the header.
func parsePassthroughRequest(header http.Header) passthroughRequest {
	value := header.Get(CredentialsHeader)
	if strings.TrimSpace(value) == "" {
		return passthroughRequest{}
	}

	sum := sha256.Sum256([]byte(value))
	req := passthroughRequest{present: true, key: hex.EncodeToString(sum[:])}

	req.creds, req.err = DecodePassthroughCredentials(value)

	return req
}

// withPassthroughRequest is the transport's HTTPContextFunc: it parses the
// request's credentials into the context the tool handlers receive.
func withPassthroughRequest(ctx context.Context, r *http.Request) context.Context {
	return context.WithValue(ctx, passthroughContextKey{}, parsePassthroughRequest(r.Header))
}

func passthroughFromContext(ctx context.Context) (passthroughRequest, bool) {
	if ctx == nil {
		return passthroughRequest{}, false
	}

	req, ok := ctx.Value(passthroughContextKey{}).(passthroughRequest)

	return req, ok
}

// maxPeekBytes bounds how much of a request body is read to find its JSON-RPC
// method. MCP requests are small; anything larger is passed on unread.
const maxPeekBytes = 1 << 20

// refreshExpiredCredentials answers a tools/call whose forwarded credentials are
// expired, or about to be, with 401 instead of passing it on.
//
// Claude Code re-runs a server's headersHelper on 401/403 and retries the call
// once, so this turns an expired SSO role session into a refresh nobody has to
// notice. It is limited to tools/call on purpose: the helper already runs on
// every connect, so initialize carries fresh credentials anyway, and keeping
// 401s off the handshake keeps clients from mistaking this server for an OAuth
// protected resource. For the same reason no WWW-Authenticate challenge is sent.
//
// It cannot loop: after the retry the helper has either exported fresh
// credentials, or reported the profile under `errors` (its SSO session itself
// expired), and neither is answered with a 401 again — the second case reaches
// the tool, which explains which login to run.
func refreshExpiredCredentials(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			next.ServeHTTP(w, r)

			return
		}

		req := parsePassthroughRequest(r.Header)
		if !req.present || req.err != nil {
			// Missing or malformed: a 401 would only re-run a helper that is not
			// configured or is broken. The tool result explains it instead.
			next.ServeHTTP(w, r)

			return
		}

		expired := req.creds.expired(time.Now(), credentialExpirySkew)
		if len(expired) == 0 || !isToolCall(r) {
			next.ServeHTTP(w, r)

			return
		}

		log.Info().
			Strs("profiles", expired).
			Msg("[refreshExpiredCredentials] forwarded aws credentials expired; asking the client to refresh them")

		http.Error(w, fmt.Sprintf("forwarded AWS credentials expired for %s; re-run the headers helper (reconnect the MCP server) to export fresh ones", strings.Join(expired, ", ")), http.StatusUnauthorized)
	})
}

// isToolCall peeks at the JSON-RPC method of a request without consuming it.
func isToolCall(r *http.Request) bool {
	if r.Body == nil {
		return false
	}

	head, err := io.ReadAll(io.LimitReader(r.Body, maxPeekBytes))
	r.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(head), r.Body), r.Body}

	if err != nil {
		return false
	}

	var msg struct {
		Method string `json:"method"`
	}

	if json.Unmarshal(head, &msg) != nil {
		return false
	}

	return msg.Method == "tools/call"
}

// PassthroughPools keeps one ProfilePool per forwarded credential set, so the
// clients, region lookups and failure cache built for a set are reused by every
// request carrying it rather than rebuilt per call.
type PassthroughPools struct {
	ctx        context.Context
	failureTTL time.Duration
	idleTTL    time.Duration
	now        func() time.Time

	mu    sync.Mutex
	pools map[string]*passthroughPool
}

type passthroughPool struct {
	pool     *ProfilePool
	lastUsed time.Time
	// expires is the latest expiry in the set; zero when any credential in it
	// is long-lived, in which case only idleness evicts it.
	expires time.Time
}

// NewPassthroughPools builds an empty registry; pools are created on demand.
func NewPassthroughPools(ctx context.Context) *PassthroughPools {
	return &PassthroughPools{
		ctx:     ctx,
		idleTTL: passthroughIdleTTL,
		now:     time.Now,
		pools:   map[string]*passthroughPool{},
	}
}

// WithFailureTTL sets the client-failure cache TTL every pool is created with.
func (r *PassthroughPools) WithFailureTTL(ttl time.Duration) *PassthroughPools {
	r.failureTTL = ttl

	return r
}

// Pool returns the pool for a credential set, creating it on first sight.
func (r *PassthroughPools) Pool(key string, creds *PassthroughCredentials) ClientPool {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.now()
	r.evict(now)

	if entry, ok := r.pools[key]; ok {
		entry.lastUsed = now

		return entry.pool
	}

	pool := newPassthroughProfilePool(r.ctx, creds)
	if r.failureTTL > 0 {
		pool.WithFailureTTL(r.failureTTL)
	}

	r.pools[key] = &passthroughPool{pool: pool, lastUsed: now, expires: latestExpiry(creds)}

	return pool
}

// evict drops pools that are idle or whose every credential has expired.
func (r *PassthroughPools) evict(now time.Time) {
	for key, entry := range r.pools {
		idle := now.Sub(entry.lastUsed) > r.idleTTL
		expired := !entry.expires.IsZero() && now.After(entry.expires)

		if idle || expired {
			delete(r.pools, key)
		}
	}
}

func (r *PassthroughPools) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return len(r.pools)
}

func latestExpiry(creds *PassthroughCredentials) time.Time {
	latest := time.Time{}

	for _, p := range creds.Profiles {
		if p.Expiration.IsZero() {
			return time.Time{}
		}

		if p.Expiration.After(latest) {
			latest = p.Expiration
		}
	}

	return latest
}

// newPassthroughProfilePool builds a ProfilePool whose entries carry forwarded
// credentials instead of naming a local profile. Reusing ProfilePool keeps every
// multi-account rule in one place: lazy STS resolution, account scoping without
// AWS calls, (account, region) dedupe, and "nobody authenticated" being an error
// rather than an empty answer.
func newPassthroughProfilePool(ctx context.Context, creds *PassthroughCredentials) *ProfilePool {
	entries := make([]*profileEntry, 0, len(creds.Profiles)+len(creds.Errors))

	for i := range creds.Profiles {
		forwarded := creds.Profiles[i]

		entries = append(entries, &profileEntry{
			name:      forwarded.Label(),
			auth:      ProfileAuth{Name: forwarded.Label(), Region: forwarded.Region},
			accountID: ptypes.AwsAccountID(forwarded.AccountID),
			creds:     &forwarded,
		})
	}

	// Profiles the client could not export stay in the pool as permanently
	// failed entries, so a query reports them instead of silently covering
	// fewer accounts than the user has.
	for _, failed := range creds.Errors {
		entries = append(entries, &profileEntry{
			name:      failed.Profile,
			auth:      ProfileAuth{Name: failed.Profile},
			accountID: ptypes.AwsAccountID(failed.AccountID),
			err:       forwardedError(failed),
			loginHint: forwardedErrorHint(failed),
			pinnedErr: true,
		})
	}

	sort.SliceStable(entries, func(i, j int) bool { return entries[i].name < entries[j].name })

	return &ProfilePool{ctx: ctx, entries: entries}
}

// passthroughConfigOptions builds the SDK config for a forwarded set.
//
// The shared config and credentials files are switched off: in passthrough mode
// the server's own ~/.aws must not leak into a request made on the caller's
// behalf, and the explicit credentials provider outranks AWS_ACCESS_KEY_ID in the
// environment. AWS_PROFILE cannot be neutralised from here — the SDK makes any
// value of it a hard requirement that the profile exist — which is why
// ClearServerProfile runs before a passthrough server starts.
func passthroughConfigOptions(p *PassthroughProfile) []func(*awsconfig.LoadOptions) error {
	return []func(*awsconfig.LoadOptions) error{
		// MaxAttempts goes inside the retryer: alongside a custom Retryer the
		// SDK ignores WithRetryMaxAttempts — see v3.DefaultAwsClientProviders.
		awsconfig.WithRetryer(func() aws.Retryer {
			return retry.NewStandard(func(o *retry.StandardOptions) {
				o.MaxAttempts = v3.AwsRetryAttempts
				o.MaxBackoff = v3.AwsRetryMaxBackoffDelay
			})
		}),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(p.AccessKeyID, p.SecretAccessKey, p.SessionToken)),
		awsconfig.WithSharedConfigFiles([]string{}),
		awsconfig.WithSharedCredentialsFiles([]string{}),
	}
}

// ClearServerProfile unsets AWS_PROFILE and reports the value it held.
//
// A passthrough server has no credentials of its own, so the variable has no use
// there — and it is harmful: the SDK fails config loading whenever AWS_PROFILE
// names a profile its config files do not define, and passthrough clients load
// with those files switched off. Left set, it would fail every request.
func ClearServerProfile() string {
	profile := os.Getenv("AWS_PROFILE")
	if profile != "" {
		_ = os.Unsetenv("AWS_PROFILE")
	}

	return profile
}

// buildPassthrough resolves one forwarded set into a client pool. The STS call
// inside DefaultClient both proves the credentials work and pins the account.
func (p *ProfilePool) buildPassthrough(entry *profileEntry) (regionClientPool, ProfileAuth, ptypes.AwsAccountID, error) {
	forwarded := entry.creds
	builder := v3.NewClientBuilder(p.ctx, passthroughConfigOptions(forwarded)...)

	client, err := builder.DefaultClient()
	if err != nil {
		return nil, entry.auth, "", explainPassthrough(forwarded, err)
	}

	accountID := client.GetAccountID()

	// A declared account that STS contradicts would make account scoping lie:
	// queries for the declared account would run against another one.
	if forwarded.AccountID != "" && accountID.String() != forwarded.AccountID {
		return nil, entry.auth, "", errors.Errorf("aws profile %q: the forwarded credentials belong to account %s, not the declared %s", forwarded.Label(), accountID, forwarded.AccountID)
	}

	log.Info().
		Str("profile", forwarded.Label()).
		Str("account", accountID.String()).
		Msg("[ProfilePool] forwarded credentials resolved")

	pool := provider.NewClientPool(p.ctx, builder)
	if p.failureTTL > 0 {
		pool.WithFailureTTL(p.failureTTL)
	}

	return pool, entry.auth, accountID, nil
}
