package mcpserver

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/arn"
	awscloudcontrol "github.com/aws/aws-sdk-go-v2/service/cloudcontrol"
	cctypes "github.com/aws/aws-sdk-go-v2/service/cloudcontrol/types"
	awscfg "github.com/aws/aws-sdk-go-v2/service/configservice/types"
	rgttypes "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi/types"
	"github.com/mark3labs/mcp-go/mcp"

	ptypes "github.com/imunhatep/awslib/provider/types"
	v3 "github.com/imunhatep/awslib/provider/v3"
	"github.com/imunhatep/awslib/service/cloudcontrol"
	"github.com/imunhatep/awslib/service/resourcegroupstagging"

	"github.com/imunhatep/aws-mcp-go/pkg/errors"
)

// The write path. Everything here is reachable only from the tools in
// write_tools.go, which are registered only with --read-write.
//
// Three rules shape it, all of them consequences of the caller being a language
// model acting on real infrastructure:
//
//   - A write names its target. account_id and region are required on every
//     write tool, even when the pool reaches a single account: a target the
//     server inferred is a target nobody confirmed. The target is resolved
//     through GetAccountClients, so no other account is contacted, and the
//     client that comes back is checked against it before anything is sent.
//   - Arguments are validated before AWS is touched, as on the cost tools, so a
//     malformed document costs nothing and cannot half-apply.
//   - An accepted request is never reported as a failure. Cloud Control writes
//     are asynchronous; once AWS has accepted one, a wait that times out or a
//     status poll that fails is "not known yet", not "did not happen". Reporting
//     it as an error invites a retry, and a retried create creates twice.

const (
	// defaultWriteWait is how long a write tool waits for its request to reach
	// a terminal status when the caller does not say. Long enough for the
	// common seconds-scale resources (log groups, parameters, tags), short
	// enough that a tool call does not hang on an RDS instance.
	defaultWriteWait = 120 * time.Second
	// maxWriteWait bounds wait_seconds. A longer operation is followed with
	// get_resource_request rather than held open in one tool call.
	maxWriteWait = 15 * time.Minute

	// writePollMin and writePollMax replace the SDK waiter's 5s→120s backoff,
	// which would let a ten-second request report back after two minutes.
	writePollMin = 2 * time.Second
	writePollMax = 15 * time.Second

	// maxTagArns bounds one tag_resources / untag_resources call. awslib
	// batches by 20 underneath; the bound is on blast radius, not on the API.
	maxTagArns = 200
)

// cloudControlWriter is the part of awslib's Cloud Control repository the write
// tools use. It is an interface so the handlers can be tested without AWS, and
// so the cached and uncached repositories are interchangeable.
type cloudControlWriter interface {
	CreateResource(resourceType awscfg.ResourceType, desiredState string) (*cctypes.ProgressEvent, error)
	UpdateResource(resourceType awscfg.ResourceType, identifier string, patchDocument string) (*cctypes.ProgressEvent, error)
	DeleteResource(resourceType awscfg.ResourceType, identifier string) (*cctypes.ProgressEvent, error)
	WaitForResourceRequest(requestToken string, maxWait time.Duration) (*cctypes.ProgressEvent, error)
	DescribeResourceRequest(requestToken string) (*awscloudcontrol.GetResourceRequestStatusOutput, error)
	CancelResourceRequest(requestToken string) (*cctypes.ProgressEvent, error)
	FindResourceRequests(statuses ...cctypes.OperationStatus) ([]cctypes.ProgressEvent, error)
	DescribeResource(resourceType awscfg.ResourceType, identifier *string) (*awscloudcontrol.GetResourceOutput, error)
	// InvalidateLists evicts the cached Cloud Control lists for the target's
	// account and region; a no-op without a cache.
	InvalidateLists()
}

// resourceTagger is the bulk tagging half of the write path.
type resourceTagger interface {
	TagResources(arns []string, tags map[string]string) (map[string]rgttypes.FailureInfo, error)
	UntagResources(arns []string, tagKeys []string) (map[string]rgttypes.FailureInfo, error)
}

// cachedCloudControl adapts the cached repository. Its writes evict the cached
// lists themselves once AWS accepts them; DescribeResource goes to the inner
// repository because a write has to read back what AWS holds now.
type cachedCloudControl struct {
	*cloudcontrol.CloudControlRepositoryCached
}

func (c cachedCloudControl) DescribeResource(resourceType awscfg.ResourceType, identifier *string) (*awscloudcontrol.GetResourceOutput, error) {
	return c.Repository().DescribeResource(resourceType, identifier)
}

// plainCloudControl adapts the uncached repository.
type plainCloudControl struct {
	*cloudcontrol.CloudControlRepository
}

func (plainCloudControl) InvalidateLists() {}

// cloudControlFor builds the Cloud Control writer for one target client.
func (s *Server) cloudControlFor(ctx context.Context, client *v3.Client) cloudControlWriter {
	if s.newCloudControl != nil {
		return s.newCloudControl(ctx, client)
	}

	repo := cloudcontrol.NewCloudControlRepository(ctx, client).WithPollInterval(writePollMin, writePollMax)

	if s.cache != nil {
		return cachedCloudControl{repo.WithCache(s.cache)}
	}

	return plainCloudControl{repo}
}

// taggerFor builds the bulk tagger for one target client.
func (s *Server) taggerFor(ctx context.Context, client *v3.Client) resourceTagger {
	if s.newTagger != nil {
		return s.newTagger(ctx, client)
	}

	return resourcegroupstagging.NewResourceGroupsTaggingRepository(ctx, client)
}

// writeTarget is the single (account, region) a write acts on.
type writeTarget struct {
	accountID string
	region    ptypes.AwsRegion
	client    *v3.Client
}

// parseWriteScope validates the account_id and region arguments every write
// tool requires. It makes no AWS call.
func parseWriteScope(req mcp.CallToolRequest) (string, ptypes.AwsRegion, error) {
	accountID := strings.TrimSpace(req.GetString("account_id", ""))
	if accountID == "" {
		return "", "", errors.New("account_id is required: a write acts on exactly one account and must name it; call list_accounts for the reachable IDs")
	}

	if !accountIDPattern.MatchString(accountID) {
		return "", "", errors.Errorf("invalid account_id %q; expected a 12-digit AWS account ID", accountID)
	}

	regionArg := strings.TrimSpace(req.GetString("region", ""))
	if regionArg == "" {
		return "", "", errors.New("region is required: a write acts on exactly one region and must name it (for a global resource type such as IAM, pass us-east-1)")
	}

	regions, err := resolveRegions(regionArg)
	if err != nil {
		return "", "", err
	}

	return accountID, regions[0], nil
}

// resolveWriteTarget turns a validated scope into the one client the write runs
// on, contacting only that account.
func (s *Server) resolveWriteTarget(ctx context.Context, accountID string, region ptypes.AwsRegion) (writeTarget, error) {
	clients, err := s.poolClients(ctx, accountID, []ptypes.AwsRegion{region})
	if err != nil {
		return writeTarget{}, errors.WithStack(err)
	}

	if len(clients) != 1 {
		return writeTarget{}, errors.Errorf("expected exactly one client for account %s in %s, the pool returned %d; refusing to write", accountID, region, len(clients))
	}

	client := clients[0]

	// Defence in depth: the pools already scope by account, but a write is the
	// one place where a client for the wrong account is not a wrong answer but a
	// change to the wrong infrastructure.
	if got := client.GetAccountID().String(); got != "" && got != accountID {
		return writeTarget{}, errors.Errorf("the client for account %s resolved to account %s; refusing to write", accountID, got)
	}

	if got := client.GetRegion(); got != "" && got != region {
		return writeTarget{}, errors.Errorf("the client for region %s resolved to region %s; refusing to write", region, got)
	}

	return writeTarget{accountID: accountID, region: region, client: client}, nil
}

// parseWriteResourceType resolves resource_type for a Cloud Control write. It
// accepts what list_resources_fallback accepts, since the same registry serves
// both, and reports a normalized spelling so the caller sees what was sent.
func parseWriteResourceType(req mcp.CallToolRequest) (awscfg.ResourceType, []string, error) {
	raw := req.GetString("resource_type", "")

	rt, err := ResolveFallbackResourceType(raw)
	if err != nil {
		return "", nil, err
	}

	var warnings []string
	if string(rt) != strings.TrimSpace(raw) {
		warnings = append(warnings, errors.Errorf("resource type normalized to %q", string(rt)).Error())
	}

	return rt, warnings, nil
}

// parseIdentifier reads a Cloud Control primary identifier. Composite
// identifiers ("a|b") are passed through verbatim.
func parseIdentifier(req mcp.CallToolRequest) (string, error) {
	id := strings.TrimSpace(req.GetString("identifier", ""))
	if id == "" {
		return "", errors.New("identifier is required: the resource's Cloud Control primary identifier, as list_resources_fallback reports it in id")
	}

	return id, nil
}

// parseWait reads wait_seconds: absent means defaultWriteWait, 0 means return
// as soon as AWS has accepted the request.
func parseWait(req mcp.CallToolRequest) (time.Duration, error) {
	if _, ok := req.GetArguments()["wait_seconds"]; !ok {
		return defaultWriteWait, nil
	}

	secs := req.GetInt("wait_seconds", 0)
	if secs < 0 {
		return 0, errors.Errorf("wait_seconds must be zero or positive, got %d", secs)
	}

	wait := time.Duration(secs) * time.Second
	if wait > maxWriteWait {
		return 0, errors.Errorf("wait_seconds must be at most %d; follow a longer operation with get_resource_request", int(maxWriteWait.Seconds()))
	}

	return wait, nil
}

// jsonArgument returns a JSON-valued argument as a JSON string. A model may send
// the document as a JSON string or as the structure itself; both are accepted.
func jsonArgument(req mcp.CallToolRequest, name string) (string, error) {
	raw, ok := req.GetArguments()[name]
	if !ok || raw == nil {
		return "", errors.Errorf("%s is required", name)
	}

	if s, ok := raw.(string); ok {
		s = strings.TrimSpace(s)
		if s == "" {
			return "", errors.Errorf("%s is required", name)
		}

		return s, nil
	}

	b, err := json.Marshal(raw)
	if err != nil {
		return "", errors.Wrapf(err, "%s is not JSON-encodable", name)
	}

	return string(b), nil
}

// parseDesiredState validates a create document: a non-empty JSON object.
func parseDesiredState(req mcp.CallToolRequest) (string, error) {
	doc, err := jsonArgument(req, "desired_state")
	if err != nil {
		return "", err
	}

	var props map[string]any
	if err := json.Unmarshal([]byte(doc), &props); err != nil {
		return "", errors.Wrap(err, "desired_state must be a JSON object of the resource's properties")
	}

	if len(props) == 0 {
		return "", errors.New("desired_state must set at least one property")
	}

	return doc, nil
}

// jsonPatchOps are the RFC 6902 operations Cloud Control accepts.
var jsonPatchOps = map[string]bool{"add": true, "remove": true, "replace": true, "move": true, "copy": true, "test": true}

// parsePatchDocument validates an update document: a non-empty RFC 6902 JSON
// Patch. A single operation object is accepted and wrapped, since a model asked
// for one change tends to send one.
func parsePatchDocument(req mcp.CallToolRequest) (string, error) {
	doc, err := jsonArgument(req, "patch_document")
	if err != nil {
		return "", err
	}

	var ops []map[string]any
	if err := json.Unmarshal([]byte(doc), &ops); err != nil {
		var op map[string]any
		if objErr := json.Unmarshal([]byte(doc), &op); objErr != nil {
			return "", errors.Wrap(err, `patch_document must be an RFC 6902 JSON Patch array, e.g. [{"op":"replace","path":"/RetentionInDays","value":30}]`)
		}

		ops = []map[string]any{op}
	}

	if len(ops) == 0 {
		return "", errors.New("patch_document must contain at least one operation")
	}

	for i, op := range ops {
		name, _ := op["op"].(string)
		if !jsonPatchOps[name] {
			return "", errors.Errorf("patch_document operation %d: op %q is not one of add, remove, replace, move, copy, test", i, name)
		}

		path, _ := op["path"].(string)
		if !strings.HasPrefix(path, "/") {
			return "", errors.Errorf("patch_document operation %d: path %q must be a JSON Pointer starting with /", i, path)
		}

		if _, hasValue := op["value"]; !hasValue && (name == "add" || name == "replace" || name == "test") {
			return "", errors.Errorf("patch_document operation %d: %s needs a value", i, name)
		}
	}

	b, err := json.Marshal(ops)
	if err != nil {
		return "", errors.WithStack(err)
	}

	return string(b), nil
}

// requestDTO is a Cloud Control ProgressEvent as the tools report it.
type requestDTO struct {
	RequestToken  string `json:"request_token,omitempty"`
	Operation     string `json:"operation,omitempty"`
	Status        string `json:"status,omitempty"`
	ResourceType  string `json:"resource_type,omitempty"`
	Identifier    string `json:"identifier,omitempty"`
	ErrorCode     string `json:"error_code,omitempty"`
	StatusMessage string `json:"status_message,omitempty"`
	EventTime     string `json:"event_time,omitempty"`
	RetryAfter    string `json:"retry_after,omitempty"`
}

func newRequestDTO(e *cctypes.ProgressEvent) requestDTO {
	if e == nil {
		return requestDTO{}
	}

	dto := requestDTO{
		RequestToken:  aws.ToString(e.RequestToken),
		Operation:     string(e.Operation),
		Status:        string(e.OperationStatus),
		ResourceType:  aws.ToString(e.TypeName),
		Identifier:    aws.ToString(e.Identifier),
		ErrorCode:     string(e.ErrorCode),
		StatusMessage: aws.ToString(e.StatusMessage),
	}

	if e.EventTime != nil {
		dto.EventTime = e.EventTime.UTC().Format(time.RFC3339)
	}

	if e.RetryAfter != nil {
		dto.RetryAfter = e.RetryAfter.UTC().Format(time.RFC3339)
	}

	return dto
}

// requestDone reports whether a status is terminal.
func requestDone(status string) bool {
	switch cctypes.OperationStatus(status) {
	case cctypes.OperationStatusSuccess, cctypes.OperationStatusFailed, cctypes.OperationStatusCancelComplete:
		return true
	}

	return false
}

// requestFailed reports whether a status is a terminal failure.
func requestFailed(status string) bool {
	switch cctypes.OperationStatus(status) {
	case cctypes.OperationStatusFailed, cctypes.OperationStatusCancelComplete:
		return true
	}

	return false
}

// writeResult is the envelope of every Cloud Control write tool.
type writeResult struct {
	AccountID string     `json:"account_id"`
	Region    string     `json:"region"`
	Request   requestDTO `json:"request"`
	// Done is true once the request reached a terminal status. False means the
	// change may still land: follow request.request_token with
	// get_resource_request, and do not resubmit.
	Done bool `json:"done"`
	// Properties is the resource as AWS holds it after a successful create or
	// update.
	Properties map[string]any `json:"properties,omitempty"`
	Warnings   []string       `json:"warnings,omitempty"`
}

// resourceWrite is one Cloud Control mutation.
type resourceWrite struct {
	resourceType awscfg.ResourceType
	submit       func(w cloudControlWriter) (*cctypes.ProgressEvent, error)
	// describe reads the resource back after success, for create and update.
	describe bool
	wait     time.Duration
	warnings []string
}

// runResourceWrite submits a write, waits for it within op.wait, and reports
// what is known. See the file comment for why only a rejected submission is an
// error result.
func (s *Server) runResourceWrite(ctx context.Context, target writeTarget, op resourceWrite) (*mcp.CallToolResult, error) {
	w := s.cloudControlFor(ctx, target.client)

	out := writeResult{
		AccountID: target.accountID,
		Region:    target.region.String(),
		Warnings:  op.warnings,
	}

	accepted, err := op.submit(w)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("AWS did not accept the request, nothing was changed", err), nil
	}

	if accepted == nil {
		return mcp.NewToolResultError("AWS returned no request status; check list_resource_requests before retrying"), nil
	}

	out.Request = newRequestDTO(accepted)
	out.Done = requestDone(out.Request.Status)

	if !out.Done && op.wait > 0 {
		final, waitErr := w.WaitForResourceRequest(out.Request.RequestToken, op.wait)

		var failed *cloudcontrol.RequestFailedError

		switch {
		case waitErr == nil:
			out.Request = newRequestDTO(final)
		case errors.As(waitErr, &failed):
			out.Request = newRequestDTO(failed.Event)
		case final != nil:
			out.Request = newRequestDTO(final)
			out.Warnings = append(out.Warnings, errors.Errorf("still %s after %s; follow it with get_resource_request and do not resubmit", out.Request.Status, op.wait).Error())
		default:
			out.Warnings = append(out.Warnings, errors.Errorf("the request was accepted but its status could not be read (%v); follow it with get_resource_request and do not resubmit", waitErr).Error())
		}

		out.Done = requestDone(out.Request.Status)

		// The cached repository evicts on acceptance; evict again now that the
		// change has landed, since a list read mid-way is stale too.
		w.InvalidateLists()
	}

	if !out.Done && op.wait == 0 {
		out.Warnings = append(out.Warnings, "returned on acceptance (wait_seconds=0); follow request_token with get_resource_request")
	}

	if out.Request.Status == string(cctypes.OperationStatusSuccess) && op.describe {
		out.Properties, out.Warnings = describeAfterWrite(w, op.resourceType, out.Request.Identifier, out.Warnings)
	}

	out.Warnings = append(out.Warnings, s.writeCacheWarning(true)...)

	res, err := jsonResult(out)
	if err == nil && requestFailed(out.Request.Status) {
		res.IsError = true
	}

	return res, err
}

// describeAfterWrite reads the resource back so the caller sees the properties
// AWS applied, defaults included, rather than the ones it asked for.
func describeAfterWrite(w cloudControlWriter, rt awscfg.ResourceType, identifier string, warnings []string) (map[string]any, []string) {
	if identifier == "" {
		return nil, append(warnings, "the request succeeded but reported no identifier, so the resource could not be read back")
	}

	got, err := w.DescribeResource(rt, aws.String(identifier))
	if err != nil {
		return nil, append(warnings, errors.Errorf("the request succeeded but reading the resource back failed: %v", err).Error())
	}

	if got == nil || got.ResourceDescription == nil || got.ResourceDescription.Properties == nil {
		return nil, warnings
	}

	var props map[string]any
	if err := json.Unmarshal([]byte(aws.ToString(got.ResourceDescription.Properties)), &props); err != nil {
		return nil, append(warnings, errors.Errorf("the resource's properties are not a JSON object: %v", err).Error())
	}

	return props, warnings
}

// writeCacheWarning says which reads may not show a write yet. A Cloud Control
// write evicts the cached Cloud Control lists for its account and region
// (ccEvicted); the typed repositories behind list_resources and count_resources
// have their own cache entries, keyed in a way a write cannot name, so those
// age out on the TTL. A bulk tag write evicts nothing.
func (s *Server) writeCacheWarning(ccEvicted bool) []string {
	if s.cache == nil {
		return nil
	}

	if ccEvicted {
		return []string{"list_resources_fallback lists for this account and region were refreshed; list_resources and count_resources may serve the previous state from cache until it expires"}
	}

	return []string{"list_resources, count_resources and list_resources_fallback may serve the previous tags from cache until it expires"}
}

// parseStringList reads a list argument sent as a JSON array, a JSON array in a
// string, or a comma/newline separated string.
func parseStringList(req mcp.CallToolRequest, name string) ([]string, error) {
	raw, ok := req.GetArguments()[name]
	if !ok || raw == nil {
		return nil, errors.Errorf("%s is required", name)
	}

	var items []string

	switch v := raw.(type) {
	case []any:
		for i, item := range v {
			s, ok := item.(string)
			if !ok {
				return nil, errors.Errorf("%s[%d] must be a string", name, i)
			}
			items = append(items, s)
		}
	case string:
		v = strings.TrimSpace(v)
		if strings.HasPrefix(v, "[") {
			if err := json.Unmarshal([]byte(v), &items); err != nil {
				return nil, errors.Wrapf(err, "%s is not a JSON array of strings", name)
			}
			break
		}

		items = strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == '\n' })
	default:
		return nil, errors.Errorf("%s must be an array of strings", name)
	}

	out := make([]string, 0, len(items))
	seen := map[string]bool{}

	for _, item := range items {
		item = strings.TrimSpace(item)
		if item == "" || seen[item] {
			continue
		}

		seen[item] = true
		out = append(out, item)
	}

	if len(out) == 0 {
		return nil, errors.Errorf("%s must name at least one value", name)
	}

	return out, nil
}

// parseTagArns validates the ARNs of a bulk tag write against its target. An
// ARN that names another account or region is rejected outright rather than
// left for AWS to fail: it is the one mistake that would otherwise be reported
// as a per-resource failure after the rest of the batch was already applied.
func parseTagArns(req mcp.CallToolRequest, accountID string, region ptypes.AwsRegion) ([]string, error) {
	arns, err := parseStringList(req, "arns")
	if err != nil {
		return nil, err
	}

	if len(arns) > maxTagArns {
		return nil, errors.Errorf("at most %d ARNs per call, got %d", maxTagArns, len(arns))
	}

	for _, raw := range arns {
		parsed, err := arn.Parse(raw)
		if err != nil {
			return nil, errors.Errorf("invalid ARN %q", raw)
		}

		if parsed.AccountID != "" && parsed.AccountID != accountID {
			return nil, errors.Errorf("ARN %q belongs to account %s, not the target account %s", raw, parsed.AccountID, accountID)
		}

		if parsed.Region != "" && parsed.Region != region.String() {
			return nil, errors.Errorf("ARN %q is in region %s, not the target region %s; tag each region in its own call", raw, parsed.Region, region)
		}
	}

	return arns, nil
}

// validateTagKey applies the limits AWS enforces on every service, so a bad key
// fails before the batch rather than once per resource.
func validateTagKey(key string) error {
	if key == "" {
		return errors.New("tag keys must not be empty")
	}

	if len(key) > 128 {
		return errors.Errorf("tag key %q is longer than 128 characters", key)
	}

	if strings.HasPrefix(strings.ToLower(key), "aws:") {
		return errors.Errorf("tag key %q uses the reserved aws: prefix", key)
	}

	return nil
}

// parseTags reads the tags argument: a JSON object, a JSON object in a string,
// or "Key=Value,Key2=Value2".
func parseTags(req mcp.CallToolRequest) (map[string]string, error) {
	raw, ok := req.GetArguments()["tags"]
	if !ok || raw == nil {
		return nil, errors.New("tags is required")
	}

	tags := map[string]string{}

	switch v := raw.(type) {
	case map[string]any:
		for k, val := range v {
			s, ok := val.(string)
			if !ok {
				return nil, errors.Errorf("tag %q: value must be a string", k)
			}
			tags[k] = s
		}
	case string:
		v = strings.TrimSpace(v)
		if strings.HasPrefix(v, "{") {
			if err := json.Unmarshal([]byte(v), &tags); err != nil {
				return nil, errors.Wrap(err, "tags is not a JSON object of string values")
			}
			break
		}

		for _, pair := range strings.Split(v, ",") {
			pair = strings.TrimSpace(pair)
			if pair == "" {
				continue
			}

			k, val, found := strings.Cut(pair, "=")
			if !found {
				return nil, errors.Errorf("tag %q must be in Key=Value form", pair)
			}
			tags[strings.TrimSpace(k)] = strings.TrimSpace(val)
		}
	default:
		return nil, errors.New(`tags must be an object, e.g. {"environment":"prod"}`)
	}

	if len(tags) == 0 {
		return nil, errors.New("tags must set at least one tag")
	}

	if len(tags) > 50 {
		return nil, errors.Errorf("at most 50 tags per call, got %d", len(tags))
	}

	for k, v := range tags {
		if err := validateTagKey(k); err != nil {
			return nil, err
		}

		if len(v) > 256 {
			return nil, errors.Errorf("tag %q: value is longer than 256 characters", k)
		}
	}

	return tags, nil
}

// tagFailureDTO is one resource a bulk tag write could not change.
type tagFailureDTO struct {
	Arn          string `json:"arn"`
	ErrorCode    string `json:"error_code,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
	StatusCode   int32  `json:"status_code,omitempty"`
}

// tagResult is the tag_resources / untag_resources envelope.
type tagResult struct {
	AccountID string          `json:"account_id"`
	Region    string          `json:"region"`
	Requested int             `json:"requested"`
	Succeeded int             `json:"succeeded"`
	Failed    []tagFailureDTO `json:"failed,omitempty"`
	Error     string          `json:"error,omitempty"`
	Warnings  []string        `json:"warnings,omitempty"`
}

// buildTagResult shapes a bulk tag write's outcome. The API answers a partial
// failure with success and a failure map, and a hard error after earlier
// batches landed; both are reported with what did change, because a caller
// that sees only "failed" retries a write that was mostly applied.
func (s *Server) buildTagResult(target writeTarget, arns []string, failures map[string]rgttypes.FailureInfo, err error) (*mcp.CallToolResult, error) {
	out := tagResult{
		AccountID: target.accountID,
		Region:    target.region.String(),
		Requested: len(arns),
	}

	for a, f := range failures {
		out.Failed = append(out.Failed, tagFailureDTO{
			Arn:          a,
			ErrorCode:    string(f.ErrorCode),
			ErrorMessage: aws.ToString(f.ErrorMessage),
			StatusCode:   f.StatusCode,
		})
	}

	sort.Slice(out.Failed, func(i, j int) bool { return out.Failed[i].Arn < out.Failed[j].Arn })

	if err != nil {
		// awslib returns the failures of the batches that ran; resources in
		// batches after the failing one were not attempted, so succeeded is
		// unknown rather than requested minus failed.
		out.Error = err.Error()
		out.Warnings = append(out.Warnings, "the write stopped part-way: earlier batches of 20 ARNs may already be applied, so check the resources before retrying")
	} else {
		out.Succeeded = len(arns) - len(out.Failed)
	}

	if len(out.Failed) > 0 {
		out.Warnings = append(out.Warnings, errors.Errorf("%d of %d resources were not changed; see failed", len(out.Failed), len(arns)).Error())
	}

	out.Warnings = append(out.Warnings, s.writeCacheWarning(false)...)

	res, jerr := jsonResult(out)
	if jerr == nil && (err != nil || out.Succeeded == 0) {
		res.IsError = true
	}

	return res, jerr
}
