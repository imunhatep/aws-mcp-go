package mcpserver

import (
	"context"
	"regexp"
	"strings"

	awscfg "github.com/aws/aws-sdk-go-v2/service/configservice/types"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/rs/zerolog/log"

	ptypes "github.com/imunhatep/awslib/provider/types"
	"github.com/imunhatep/awslib/proxy"
	"github.com/imunhatep/awslib/resources"
	"github.com/imunhatep/awslib/service"
	"github.com/imunhatep/awslib/service/cfg"

	"github.com/imunhatep/aws-mcp-go/pkg/errors"
)

// resourceTypePattern is a shape check on the type name before it reaches AWS,
// so an obviously malformed argument costs nothing instead of one failed API
// call per account and region.
var resourceTypePattern = regexp.MustCompile(`^[A-Za-z0-9]+::[A-Za-z0-9]+::[A-Za-z0-9]+$`)

// ResolveFallbackResourceType resolves the resource_type argument for the
// fallback tool. Unlike ResolveResourceType it is not limited to the types wired
// into awslib's FindAll dispatch — that is the whole point of the fallback — but
// it still canonicalizes wherever it can:
//
//  1. the URL form, for the types awslib knows;
//  2. a case-insensitive match against the ~500-entry Config vocabulary, so
//     "aws::ec2::instance" resolves to the canonical spelling;
//  3. otherwise a well-formed name is passed through verbatim. Cloud Control's
//     registry is larger than the Config vocabulary and there is no table here
//     to canonicalize against, and its TypeName is case-sensitive — so an
//     unknown type is the caller's spelling, exactly as given.
func ResolveFallbackResourceType(raw string) (awscfg.ResourceType, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.Errorf("resource_type is required")
	}

	if rt, ok := cfg.ResourceTypeFromUrl(strings.ToLower(raw)); ok {
		return rt, nil
	}

	for _, rt := range awscfg.ResourceType("").Values() {
		if strings.EqualFold(string(rt), raw) {
			return rt, nil
		}
	}

	if resourceTypePattern.MatchString(raw) {
		return awscfg.ResourceType(raw), nil
	}

	return "", errors.Errorf(
		"invalid resource type %q; expected the CloudFormation form AWS::<Service>::<Resource> (e.g. AWS::Kinesis::Stream) or the URL form (aws_kinesis_stream)",
		raw,
	)
}

// queryScope records what the fallback actually reached out to, so a caller can
// tell an empty answer ("queried 12 accounts, found nothing") apart from a
// narrow one ("queried 1 account"). Cloud Control returns an error for a type
// with no LIST handler rather than an empty list, but a missing IAM grant in
// some accounts still degrades to fewer rows, and the fan-out below logs those
// failures rather than propagating them.
type queryScope struct {
	Source   string `json:"source"`
	Accounts int    `json:"accounts"`
	Regions  int    `json:"regions"`
	Detailed bool   `json:"detailed"`
}

// fallbackResult is the list_resources_fallback envelope. It mirrors listResult
// and adds the scope and warnings, because a generic backend has more ways to
// answer incompletely than a hand-written repository does.
type fallbackResult struct {
	Items      []resourceDTO `json:"items"`
	Count      int           `json:"count"`
	Total      int           `json:"total"`
	NextCursor string        `json:"next_cursor,omitempty"`
	Queried    queryScope    `json:"queried"`
	Warnings   []string      `json:"warnings,omitempty"`
}

// fetchFallbackResources runs the generic Cloud Control path over the pool.
//
// It reuses awslib's provider pipeline unchanged — the generic proxy satisfies
// the same interface as the typed one — so the parallel fan-out, the cache and
// the global-type collapsing all behave exactly as they do for list_resources.
func (s *Server) fetchFallbackResources(
	rt awscfg.ResourceType,
	regions []ptypes.AwsRegion,
	detailed bool,
) ([]service.ResourceInterface, queryScope, error) {
	scope := queryScope{Source: "cloudcontrol", Detailed: detailed}

	clients, err := s.pool.GetClients(regions...)
	if err != nil {
		return nil, scope, errors.WithStack(err)
	}

	accounts := map[ptypes.AwsAccountID]struct{}{}
	regionSet := map[ptypes.AwsRegion]struct{}{}
	for _, client := range clients {
		accounts[client.GetAccountID()] = struct{}{}
		regionSet[client.GetRegion()] = struct{}{}
	}
	scope.Accounts, scope.Regions = len(accounts), len(regionSet)

	proxyPool := proxy.NewGenericRepoProxyPool(s.ctx, clients, detailed)
	if s.cache != nil {
		proxyPool = proxyPool.WithCache(s.cache)
	}

	return resources.NewProvider(rt, proxyPool.List(rt)...).Run().Read(), scope, nil
}

// dedupeFallback removes rows that describe the same resource twice.
//
// The typed path avoids this by listing global types in one region only
// (RepoProxyPool.List collapses them), but that collapsing is driven by
// cfg.ResourceTypeListGlobal() — a curated list that by definition does not
// cover an arbitrary fallback type. So a global type asked for across all
// regions is fetched once per region and every row comes back N times, which
// would silently multiply counts.
//
// The discriminator is the ARN when the resource has one: a global resource's
// ARN carries an empty region segment, so its ARN is identical from every
// region's client and the copies collapse, while a regional resource's ARN
// differs per region and is kept. Without an ARN there is nothing to tell a
// global duplicate from two same-named regional resources (two EKS clusters
// called "prod" in different regions share an identifier), so those fall back
// to account+region+id, which never merges distinct resources.
func dedupeFallback(items []service.ResourceInterface) ([]service.ResourceInterface, int) {
	seen := make(map[string]struct{}, len(items))
	out := make([]service.ResourceInterface, 0, len(items))

	for _, r := range items {
		key := r.GetAccountID().String() + "|" + string(r.GetType()) + "|"
		if arn := r.GetArn(); arn != "" {
			key += arn
		} else {
			key += r.GetRegion().String() + "|" + r.GetId()
		}

		if _, dup := seen[key]; dup {
			continue
		}

		seen[key] = struct{}{}
		out = append(out, r)
	}

	return out, len(items) - len(out)
}

func (s *Server) handleListResourcesFallback(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	rawType := req.GetString("resource_type", "")

	rt, err := ResolveFallbackResourceType(rawType)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("invalid resource_type", err), nil
	}

	view, ok := parseView(req.GetString("view", ""))
	if !ok {
		return mcp.NewToolResultErrorf("invalid view %q; use id, summary or detail", req.GetString("view", "")), nil
	}

	regionArg := req.GetString("region", "")
	regions, err := resolveRegions(regionArg)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("invalid region", err), nil
	}

	filter, err := parseFilter(req.GetString("account_id", ""), req.GetString("state", ""), req.GetString("tag", ""), req.GetString("attribute", ""))
	if err != nil {
		return mcp.NewToolResultErrorFromErr("invalid filter", err), nil
	}

	offset, err := parseCursor(req.GetString("cursor", ""))
	if err != nil {
		return mcp.NewToolResultErrorFromErr("invalid cursor", err), nil
	}
	limit := clampLimit(req.GetInt("limit", 0))

	// A detail view needs the full property bag, which for some types only a
	// per-resource GetResource returns. That is one extra API call per resource,
	// so it is tied to the view rather than offered as a free-standing switch.
	detailed := view == viewDetail

	log.Info().
		Str("type", string(rt)).
		Str("region", regionArg).
		Str("view", string(view)).
		Bool("detailed", detailed).
		Int("regions", len(regions)).
		Int("offset", offset).
		Int("limit", limit).
		Msg("[mcpserver.handleListResourcesFallback] listing resources via cloud control")

	items, scope, err := s.fetchFallbackResources(rt, regions, detailed)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("failed to list resources", err), nil
	}

	items, duplicates := dedupeFallback(items)

	out := make([]resourceDTO, 0, len(items))
	for _, r := range items {
		attrs := summaryAttributes(r)
		if !filter.matches(r, attrs) {
			continue
		}
		out = append(out, buildResourceDTO(r, attrs, view))
	}

	page := paginate(out, offset, limit)

	return jsonResult(fallbackResult{
		Items:      page.Items,
		Count:      page.Count,
		Total:      page.Total,
		NextCursor: page.NextCursor,
		Queried:    scope,
		Warnings:   fallbackWarnings(rt, rawType, scope, duplicates),
	})
}

// fallbackWarnings states what the caller cannot see from the rows alone. An
// inventory answer that is quietly partial is worse than one that says so.
func fallbackWarnings(rt awscfg.ResourceType, rawType string, scope queryScope, duplicates int) []string {
	var warnings []string

	if duplicates > 0 {
		warnings = append(warnings, errors.Errorf(
			"collapsed %d duplicate row(s): this type appears to be global, so every queried region returned it; pass region to query one region directly",
			duplicates,
		).Error())
	}

	// Exact comparison, not EqualFold: the point is to disclose that the
	// spelling was changed, and a case-only change is exactly what gets
	// normalized most often.
	if string(rt) != strings.TrimSpace(rawType) && !strings.Contains(rawType, "_") {
		warnings = append(warnings, errors.Errorf("resource type normalized to %q", string(rt)).Error())
	}

	if isSupported(rt) {
		warnings = append(warnings, errors.Errorf(
			"%q has a dedicated implementation; list_resources returns richer, typed attributes for it and should be preferred",
			string(rt),
		).Error())
	}

	warnings = append(warnings, "answered from the Cloud Control API: attributes are the provider's own property names rather than curated fields, and a resource is missing entirely if the credentials for its account lack the underlying service's read permission (such failures are logged, not returned)")

	if scope.Detailed {
		warnings = append(warnings, "detail view issues one extra GetResource call per resource")
	}

	return warnings
}
