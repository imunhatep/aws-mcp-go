package mcpserver

import (
	"context"
	"encoding/json"
	"sort"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/rs/zerolog/log"

	ptypes "github.com/imunhatep/awslib/provider/types"
)

// registerTools declares every MCP tool the server exposes.
func (s *Server) registerTools() {
	s.mcp.AddTool(
		mcp.NewTool(
			"list_resource_types",
			mcp.WithDescription("List the AWS resource types this server can enumerate. Returns each type in both canonical (AWS::EC2::Instance) and URL (aws_ec2_instance) form, and whether it is a global (non-regional) resource. Use one of these values as the resource_type argument to list_resources."),
		),
		s.handleListResourceTypes,
	)

	s.mcp.AddTool(
		mcp.NewTool(
			"list_regions",
			mcp.WithDescription("List the AWS regions known to this server, with human-readable descriptions. Any region name may be passed as the optional region argument to list_resources."),
		),
		s.handleListRegions,
	)

	s.mcp.AddTool(
		mcp.NewTool(
			"list_accounts",
			mcp.WithDescription("List the AWS account IDs this server can reach. In local mode this is the single account of the active AWS credentials/SSO profile; in assume-role mode it is every account whose role can be assumed."),
		),
		s.handleListAccounts,
	)

	s.mcp.AddTool(
		mcp.NewTool(
			"list_resources",
			mcp.WithDescription("List AWS resources of a given type across accounts and regions. Results are cached (default 6h) and paginated. Use list_resource_types for valid resource_type values. Each row carries identity fields (account_id, region, type, arn, id, name, state); the view argument controls richness and filters narrow results server-side. Returns an object {items, count, total, next_cursor}. For 'how many / group by' questions prefer count_resources, which returns tiny aggregates instead of every row."),
			mcp.WithString("resource_type",
				mcp.Required(),
				mcp.Description("Resource type to list, canonical (AWS::EC2::Instance) or URL form (aws_ec2_instance)."),
			),
			mcp.WithString("region",
				mcp.Description("Optional AWS region (e.g. eu-central-1). If omitted, all known regions are queried. Ignored for global resource types."),
			),
			mcp.WithString("account_id",
				mcp.Description("Optional AWS account ID. Scopes the query to that one account — only its credentials are used and only it is called, rather than querying every reachable account and filtering the rows. An account this server cannot reach is an error, not an empty result. Call list_accounts for the reachable IDs."),
			),
			mcp.WithString("view",
				mcp.Description("Response richness per row: 'id' (default, thin: identity + state), 'summary' (adds tags and curated attributes like instance_type/engine/dns_name), or 'detail' (adds a raw field with the full provider-native entity). Use detail only with a region + filters so the result stays small."),
			),
			mcp.WithString("state",
				mcp.Description("Filter by lifecycle state/status, case-insensitive (e.g. 'running' for EC2, 'available' for RDS). Matched against each resource's state field."),
			),
			mcp.WithString("tag",
				mcp.Description("Filter by tag, in 'Key=Value' form (e.g. 'environment=preprod')."),
			),
			mcp.WithString("attribute",
				mcp.Description("Filter by a curated attribute, in 'key=value' form (e.g. 'instance_type=m5.2xlarge', 'engine=postgres')."),
			),
			mcp.WithNumber("limit",
				mcp.Description("Maximum rows to return in this page (default 50, max 1000)."),
			),
			mcp.WithString("cursor",
				mcp.Description("Opaque pagination cursor from a previous response's next_cursor; omit for the first page."),
			),
		),
		s.handleListResources,
	)

	s.mcp.AddTool(
		mcp.NewTool(
			"count_resources",
			mcp.WithDescription("Aggregate AWS resources of a given type into group counts instead of listing every row — the efficient way to answer 'how many' and 'break down by' questions (by state, instance type, engine, tag, region, account). Returns {total, group_by, buckets:[{group, count}]}, sorted by descending count. Shares the same filters as list_resources."),
			mcp.WithString("resource_type",
				mcp.Required(),
				mcp.Description("Resource type to count, canonical (AWS::EC2::Instance) or URL form (aws_ec2_instance)."),
			),
			mcp.WithString("group_by",
				mcp.Description("Comma-separated dimensions to group by: type, state, region, account_id, tag:<key>, attr:<key> (e.g. 'state,attr:instance_type'). Defaults to 'state'."),
			),
			mcp.WithString("region",
				mcp.Description("Optional AWS region. If omitted, all known regions are queried. Ignored for global resource types."),
			),
			mcp.WithString("account_id",
				mcp.Description("Optional AWS account ID. Scopes the query to that one account rather than counting across every reachable account. An unreachable account is an error, not a zero count."),
			),
			mcp.WithString("state",
				mcp.Description("Optional lifecycle state/status filter, case-insensitive (e.g. 'running')."),
			),
			mcp.WithString("tag",
				mcp.Description("Optional tag filter, in 'Key=Value' form."),
			),
			mcp.WithString("attribute",
				mcp.Description("Optional curated-attribute filter, in 'key=value' form."),
			),
		),
		s.handleCountResources,
	)

	s.mcp.AddTool(
		mcp.NewTool(
			"list_resources_fallback",
			mcp.WithDescription("Last-resort lister for AWS resource types that list_resources does not support. Answers from the AWS Cloud Control API, which can enumerate almost any AWS::Service::Resource type without a dedicated implementation. Prefer list_resources whenever list_resource_types includes the type: it returns curated, typed attributes, while this tool returns the provider's own raw property names, cannot always determine an ARN, and reports no creation time. Types whose Cloud Control registry entry has no LIST handler return an error rather than an empty list. Returns {items, count, total, next_cursor, queried, warnings} — read warnings, they qualify the answer."),
			mcp.WithString("resource_type",
				mcp.Required(),
				mcp.Description("Resource type in CloudFormation form (AWS::Kinesis::Stream). Case-insensitive for well-known types; for anything else the exact CloudFormation spelling is required because the Cloud Control type name is case-sensitive. The URL form (aws_kinesis_stream) also works for types list_resource_types knows."),
			),
			mcp.WithString("region",
				mcp.Description("Optional AWS region (e.g. eu-central-1). If omitted, all known regions are queried — pass a region for a global resource type, otherwise it is fetched once per region and the duplicates are collapsed (reported in warnings)."),
			),
			mcp.WithString("account_id",
				mcp.Description("Optional AWS account ID. Scopes the query to that one account — only it is called, which also bounds the per-resource detail calls. An unreachable account is an error, not an empty result."),
			),
			mcp.WithString("view",
				mcp.Description("Response richness per row: 'id' (default, thin: identity + state), 'summary' (adds tags and the resource's top-level scalar properties as snake_case attributes), or 'detail' (adds raw with the full property bag). Note detail costs one extra AWS call per resource, so pair it with a region and filters."),
			),
			mcp.WithString("state",
				mcp.Description("Filter by lifecycle state/status, case-insensitive. Matched against the resource's State/Status property when it has one."),
			),
			mcp.WithString("tag",
				mcp.Description("Filter by tag, in 'Key=Value' form. Only works for types that expose a CloudFormation-style Tags list."),
			),
			mcp.WithString("attribute",
				mcp.Description("Filter by a property, in 'key=value' form with the key in snake_case (e.g. 'engine_version=8.0'). Use view=summary first to discover which keys a type actually returns."),
			),
			mcp.WithNumber("limit",
				mcp.Description("Maximum rows to return in this page (default 50, max 1000)."),
			),
			mcp.WithString("cursor",
				mcp.Description("Opaque pagination cursor from a previous response's next_cursor; omit for the first page."),
			),
		),
		s.handleListResourcesFallback,
	)

	s.registerCostTools()
}

func (s *Server) handleListResourceTypes(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return jsonResult(supportedResourceTypeInfos())
}

func (s *Server) handleListRegions(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	type regionInfo struct {
		Region      string `json:"region"`
		Description string `json:"description"`
	}

	data := ptypes.GetAwsRegionData()
	infos := make([]regionInfo, 0, len(data))
	for region, meta := range data {
		infos = append(infos, regionInfo{Region: region.String(), Description: meta["description"]})
	}

	sort.Slice(infos, func(i, j int) bool { return infos[i].Region < infos[j].Region })

	return jsonResult(infos)
}

func (s *Server) handleListAccounts(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// Ensure the pool has resolved at least the default client so local-mode
	// pools (which populate account IDs lazily) report a non-empty result.
	if _, err := s.pool.GetClients(ptypes.DefaultAwsRegion); err != nil {
		return mcp.NewToolResultErrorFromErr("failed to initialise aws client", err), nil
	}

	ids, err := s.pool.ListAccountIDs()
	if err != nil {
		return mcp.NewToolResultErrorFromErr("failed to list account ids", err), nil
	}

	accounts := make([]string, 0, len(ids))
	for _, id := range ids {
		accounts = append(accounts, id.String())
	}
	sort.Strings(accounts)

	return jsonResult(accounts)
}

func (s *Server) handleListResources(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	rt, ok := ResolveResourceType(req.GetString("resource_type", ""))
	if !ok {
		return mcp.NewToolResultErrorf("unknown or unsupported resource type %q; call list_resource_types for valid values", req.GetString("resource_type", "")), nil
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

	log.Info().
		Str("type", string(rt)).
		Str("region", regionArg).
		Str("view", string(view)).
		Str("state", filter.state).
		Int("regions", len(regions)).
		Int("offset", offset).
		Int("limit", limit).
		Msg("[mcpserver.handleListResources] listing resources")

	items, err := s.fetchResources(rt, regions, req.GetString("account_id", ""))
	if err != nil {
		return mcp.NewToolResultErrorFromErr("failed to list resources", err), nil
	}

	out := make([]resourceDTO, 0, len(items))
	for _, r := range items {
		attrs := summaryAttributes(r)
		if !filter.matches(r, attrs) {
			continue
		}
		out = append(out, buildResourceDTO(r, attrs, view))
	}

	return jsonResult(paginate(out, offset, limit))
}

func (s *Server) handleCountResources(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	rt, ok := ResolveResourceType(req.GetString("resource_type", ""))
	if !ok {
		return mcp.NewToolResultErrorf("unknown or unsupported resource type %q; call list_resource_types for valid values", req.GetString("resource_type", "")), nil
	}

	dims, err := parseGroupBy(req.GetString("group_by", ""))
	if err != nil {
		return mcp.NewToolResultErrorFromErr("invalid group_by", err), nil
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

	log.Info().
		Str("type", string(rt)).
		Str("region", regionArg).
		Strs("group_by", dims).
		Int("regions", len(regions)).
		Msg("[mcpserver.handleCountResources] counting resources")

	items, err := s.fetchResources(rt, regions, req.GetString("account_id", ""))
	if err != nil {
		return mcp.NewToolResultErrorFromErr("failed to count resources", err), nil
	}

	return jsonResult(aggregate(items, filter, dims))
}

// jsonResult marshals v to indented JSON and wraps it as a tool text result.
func jsonResult(v any) (*mcp.CallToolResult, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return mcp.NewToolResultErrorFromErr("failed to marshal result", err), nil
	}

	return mcp.NewToolResultText(string(b)), nil
}
