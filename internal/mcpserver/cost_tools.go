package mcpserver

import (
	"context"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsce "github.com/aws/aws-sdk-go-v2/service/costexplorer"
	cetypes "github.com/aws/aws-sdk-go-v2/service/costexplorer/types"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/rs/zerolog/log"

	"github.com/imunhatep/awslib/service/costexplorer"

	"github.com/imunhatep/aws-mcp-go/pkg/errors"
)

// costFilterItemSchema is the JSON schema of one filter row, shared by every
// tool that filters. It mirrors costFilterSpec.
func costFilterItemSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"type": map[string]any{
				"type":        "string",
				"enum":        []string{"dimension", "tag", "cost_category"},
				"description": "What the row filters on. Optional: a key naming a known dimension defaults to 'dimension', anything else to 'tag'.",
			},
			"key": map[string]any{
				"type":        "string",
				"description": "Dimension name (SERVICE, REGION, LINKED_ACCOUNT, ...), tag key or cost category name.",
			},
			"values": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "Values to match; OR-ed together. Must be the exact value the API reports — use list_cost_dimension_values to discover them.",
			},
			"match_options": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "Operator. Cost and usage queries accept EQUALS and CASE_SENSITIVE on dimensions, plus ABSENT on tags and cost categories. Defaults to EQUALS.",
			},
			"exclude": map[string]any{
				"type":        "boolean",
				"description": "Invert the row — the console's 'Exclude' toggle.",
			},
			"absent": map[string]any{
				"type":        "boolean",
				"description": "Match resources that do not carry the tag key / fall outside the cost category. Values are ignored.",
			},
			"present": map[string]any{
				"type":        "boolean",
				"description": "Match resources carrying the key with any value.",
			},
		},
		"required": []string{"key"},
	}
}

const costFilterDescription = "Filter rows, AND-ed together; the values within a row are OR-ed. Each row is {type, key, values, match_options, exclude, absent, present}. Examples: [{\"key\":\"SERVICE\",\"values\":[\"Amazon Elastic Compute Cloud - Compute\"]}] or [{\"type\":\"tag\",\"key\":\"Team\",\"values\":[\"platform\"]},{\"key\":\"RECORD_TYPE\",\"values\":[\"Credit\",\"Refund\"],\"exclude\":true}]."

const costPeriodDescription = "Named time window: today, yesterday, last_7_days, last_30_days, last_<n>_days, this_month (mtd), last_month, last_3_months, last_<n>_months, this_year (ytd), last_year. 'last_*' windows cover whole elapsed units and exclude today; 'this_*' and 'today' include today, whose costs are still partial. Ignored when start and end are given."

// registerCostTools declares the Cost Explorer tools. They sit alongside the
// resource-listing tools but take a different path into awslib: Cost Explorer is
// a billing API, not an inventory one, so it is queried directly rather than
// through the resource proxy pool.
func (s *Server) registerCostTools() {
	s.mcp.AddTool(
		mcp.NewTool(
			"get_cost_and_usage",
			mcp.WithDescription("Query AWS Cost Explorer for actual spend and usage over a time window, with the same filtering and grouping the Cost Explorer console offers. Returns {total, groups, periods}: groups aggregate the whole window and are ranked by spend (use them for 'what costs the most'), periods split the same data by time (use them for trends). Group by up to 2 dimensions — SERVICE, LINKED_ACCOUNT, REGION, INSTANCE_TYPE, USAGE_TYPE, RECORD_TYPE, PURCHASE_TYPE, OPERATION, TAG:<key>, COST_CATEGORY:<key>. Call list_cost_dimensions for the full vocabulary and list_cost_dimension_values to discover the exact values a filter needs. Results are cached (default 6h)."),
			mcp.WithString("period",
				mcp.Description(costPeriodDescription),
			),
			mcp.WithString("start",
				mcp.Description("Explicit inclusive window start, YYYY-MM-DD (or an RFC3339 timestamp for HOURLY). Must be given together with end; overrides period."),
			),
			mcp.WithString("end",
				mcp.Description("Explicit exclusive window end, YYYY-MM-DD (or an RFC3339 timestamp for HOURLY)."),
			),
			mcp.WithString("granularity",
				mcp.Description("Time bucket size. Defaults to MONTHLY. HOURLY needs Cost Explorer hourly data enabled and only covers the last 14 days."),
				mcp.Enum(string(cetypes.GranularityDaily), string(cetypes.GranularityMonthly), string(cetypes.GranularityHourly)),
			),
			mcp.WithArray("metrics",
				mcp.Description("Cost metrics to report. Defaults to UnblendedCost (what the account is invoiced). Others: AmortizedCost, BlendedCost, NetUnblendedCost, NetAmortizedCost, UsageQuantity, NormalizedUsageAmount. The first metric ranks the groups."),
				mcp.WithStringItems(),
			),
			mcp.WithArray("group_by",
				mcp.Description("Up to 2 groupings: a dimension (SERVICE, LINKED_ACCOUNT, REGION, INSTANCE_TYPE, USAGE_TYPE, RECORD_TYPE, PURCHASE_TYPE, OPERATION, ...), TAG:<key> or COST_CATEGORY:<key>. Omit for the plain total over time."),
				mcp.WithStringItems(),
				mcp.MaxItems(costexplorer.MaxGroupBy),
			),
			mcp.WithArray("filters",
				mcp.Description(costFilterDescription),
				mcp.Items(costFilterItemSchema()),
			),
			mcp.WithString("account_id",
				mcp.Description("Query only this account's Cost Explorer. By default every reachable account is queried and the results summed — see the note in the response. To break spend down by account inside an organization, group_by LINKED_ACCOUNT instead."),
			),
			mcp.WithNumber("limit",
				mcp.Description("Maximum groups to return, ranked by the first metric (default 25, max 1000). group_count and groups_truncated report what was dropped."),
			),
			mcp.WithBoolean("include_periods",
				mcp.Description("Also break each time period down by group. Off by default: periods carry only their totals, which keeps the response small."),
			),
		),
		s.handleGetCostAndUsage,
	)

	s.mcp.AddTool(
		mcp.NewTool(
			"get_cost_forecast",
			mcp.WithDescription("Forecast AWS spend over a future time window using Cost Explorer's model, optionally narrowed by the same filters as get_cost_and_usage. Answers 'what will this month cost'. Returns a total plus a per-period breakdown; forecasts cannot be grouped."),
			mcp.WithString("period",
				mcp.Description("Named future window: this_month (rest of the current month, the default), next_month, next_7_days, next_30_days, next_<n>_days, next_3_months, next_<n>_months. Ignored when start and end are given."),
			),
			mcp.WithString("start",
				mcp.Description("Explicit inclusive window start, YYYY-MM-DD. Must not be later than today. Given together with end."),
			),
			mcp.WithString("end",
				mcp.Description("Explicit exclusive window end, YYYY-MM-DD."),
			),
			mcp.WithString("granularity",
				mcp.Description("Forecast bucket size. Defaults to MONTHLY. DAILY covers at most 3 months ahead, MONTHLY at most 18."),
				mcp.Enum(string(cetypes.GranularityDaily), string(cetypes.GranularityMonthly)),
			),
			mcp.WithString("metric",
				mcp.Description("Metric to forecast, e.g. UNBLENDED_COST (default), AMORTIZED_COST, NET_UNBLENDED_COST. Only one metric per forecast."),
			),
			mcp.WithArray("filters",
				mcp.Description(costFilterDescription+" Forecasts filter on dimensions only — tag and cost category rows are rejected by the API."),
				mcp.Items(costFilterItemSchema()),
			),
			mcp.WithString("account_id",
				mcp.Description("Forecast only this account. By default every reachable account is forecast and the results summed."),
			),
			mcp.WithNumber("prediction_interval_level",
				mcp.Description("Confidence level for the prediction interval, 51-99. Omit for the mean only."),
			),
		),
		s.handleGetCostForecast,
	)

	s.mcp.AddTool(
		mcp.NewTool(
			"list_cost_dimension_values",
			mcp.WithDescription("List the values a Cost Explorer dimension actually took over a time window — the way to discover the exact strings a get_cost_and_usage filter needs (AWS service names like 'Amazon Elastic Compute Cloud - Compute' rarely match what a caller would guess). For LINKED_ACCOUNT the attributes carry the account name."),
			mcp.WithString("dimension",
				mcp.Required(),
				mcp.Description("Dimension to enumerate: SERVICE, LINKED_ACCOUNT, REGION, INSTANCE_TYPE, USAGE_TYPE, RECORD_TYPE, PURCHASE_TYPE, OPERATION, ... Call list_cost_dimensions for the full list."),
			),
			mcp.WithString("period",
				mcp.Description(costPeriodDescription+" Defaults to last_30_days."),
			),
			mcp.WithString("start",
				mcp.Description("Explicit inclusive window start, YYYY-MM-DD. Given together with end."),
			),
			mcp.WithString("end",
				mcp.Description("Explicit exclusive window end, YYYY-MM-DD."),
			),
			mcp.WithString("search",
				mcp.Description("Only return values containing this substring."),
			),
			mcp.WithArray("filters",
				mcp.Description(costFilterDescription),
				mcp.Items(costFilterItemSchema()),
			),
			mcp.WithString("context",
				mcp.Description("Which set of dimensions to answer for. Defaults to COST_AND_USAGE."),
				mcp.Enum(string(cetypes.ContextCostAndUsage), string(cetypes.ContextReservations), string(cetypes.ContextSavingsPlans)),
			),
			mcp.WithString("account_id",
				mcp.Description("Query only this account. By default every reachable account is queried and the values merged."),
			),
			mcp.WithNumber("limit",
				mcp.Description("Maximum values to return (default 100, max 1000)."),
			),
		),
		s.handleListCostDimensionValues,
	)

	s.mcp.AddTool(
		mcp.NewTool(
			"list_cost_dimensions",
			mcp.WithDescription("Describe the Cost Explorer query vocabulary this server accepts: dimensions and their aliases, metrics, granularities, named time periods, match options and the filter row shape. Takes no arguments and makes no AWS calls — call it before building a non-trivial get_cost_and_usage query."),
		),
		s.handleListCostDimensions,
	)
}

// stringListArg reads a list-of-strings argument, accepting the decoded array an
// MCP client normally sends as well as a single comma-separated string, which
// callers produce often enough to be worth handling.
func stringListArg(req mcp.CallToolRequest, key string) []string {
	raw, ok := req.GetArguments()[key]
	if !ok || raw == nil {
		return nil
	}

	if s, isString := raw.(string); isString {
		values := make([]string, 0)
		for entry := range strings.SplitSeq(s, ",") {
			if entry = strings.TrimSpace(entry); entry != "" {
				values = append(values, entry)
			}
		}

		return values
	}

	return req.GetStringSlice(key, nil)
}

// clampCostLimit applies the default and ceiling to a group limit.
func clampCostLimit(limit int) int {
	if limit <= 0 {
		return defaultCostGroupLimit
	}
	if limit > maxCostGroupLimit {
		return maxCostGroupLimit
	}

	return limit
}

func (s *Server) handleGetCostAndUsage(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	window, err := resolveCostWindow(req.GetString("period", ""), req.GetString("start", ""), req.GetString("end", ""), time.Now())
	if err != nil {
		return mcp.NewToolResultErrorFromErr("invalid time period", err), nil
	}

	granularity, err := parseCostGranularity(req.GetString("granularity", ""))
	if err != nil {
		return mcp.NewToolResultErrorFromErr("invalid granularity", err), nil
	}

	metrics, err := parseCostMetrics(stringListArg(req, "metrics"))
	if err != nil {
		return mcp.NewToolResultErrorFromErr("invalid metrics", err), nil
	}

	groupings, err := parseCostGroupBy(stringListArg(req, "group_by"))
	if err != nil {
		return mcp.NewToolResultErrorFromErr("invalid group_by", err), nil
	}

	filters, err := parseCostFilters(req.GetArguments()["filters"])
	if err != nil {
		return mcp.NewToolResultErrorFromErr("invalid filters", err), nil
	}

	query := costexplorer.CostQuery{
		Start:       window.start,
		End:         window.end,
		Granularity: granularity,
		Metrics:     metrics,
		Filters:     filters,
		GroupBy:     groupDefinitions(groupings),
	}

	// Validate before touching AWS: Cost Explorer bills per request, so a query
	// that cannot succeed should not be sent once per account.
	if err := query.Validate(); err != nil {
		return mcp.NewToolResultErrorFromErr("invalid cost query", err), nil
	}

	repos, err := s.costRepositories(req.GetString("account_id", ""))
	if err != nil {
		return mcp.NewToolResultErrorFromErr("failed to initialise aws client", err), nil
	}

	log.Info().
		Str("start", window.startString()).
		Str("end", window.endString()).
		Str("granularity", string(granularity)).
		Strs("metrics", metrics).
		Strs("group_by", groupLabels(groupings)).
		Int("filters", len(filters)).
		Strs("accounts", accountIDs(repos)).
		Msg("[mcpserver.handleGetCostAndUsage] querying cost and usage")

	results := make([]accountCostResult, 0, len(repos))
	for _, r := range repos {
		data, err := r.repo.GetCostAndUsageByQuery(query)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("cost and usage query failed for account "+r.accountID, err), nil
		}

		results = append(results, accountCostResult{accountID: r.accountID, data: data})
	}

	return jsonResult(buildCostResult(
		results,
		window,
		granularity,
		metrics,
		groupings,
		clampCostLimit(req.GetInt("limit", 0)),
		req.GetBool("include_periods", false),
	))
}

func (s *Server) handleGetCostForecast(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	window, err := resolveForecastWindow(req.GetString("period", ""), req.GetString("start", ""), req.GetString("end", ""), time.Now())
	if err != nil {
		return mcp.NewToolResultErrorFromErr("invalid time period", err), nil
	}

	granularity, err := parseCostGranularity(req.GetString("granularity", ""))
	if err != nil {
		return mcp.NewToolResultErrorFromErr("invalid granularity", err), nil
	}

	if granularity == cetypes.GranularityHourly {
		return mcp.NewToolResultErrorf("granularity HOURLY is not supported by cost forecasts; use DAILY or MONTHLY"), nil
	}

	metric, err := parseForecastMetric(req.GetString("metric", ""))
	if err != nil {
		return mcp.NewToolResultErrorFromErr("invalid metric", err), nil
	}

	filters, err := parseCostFilters(req.GetArguments()["filters"])
	if err != nil {
		return mcp.NewToolResultErrorFromErr("invalid filters", err), nil
	}

	intervalLevel := int32(req.GetInt("prediction_interval_level", 0))
	if intervalLevel != 0 && (intervalLevel < 51 || intervalLevel > 99) {
		return mcp.NewToolResultErrorf("prediction_interval_level must be between 51 and 99, got %d", intervalLevel), nil
	}

	input := &awsce.GetCostForecastInput{
		Granularity: granularity,
		Metric:      metric,
		TimePeriod: &cetypes.DateInterval{
			Start: aws.String(window.startString()),
			End:   aws.String(window.endString()),
		},
		Filter: costexplorer.FiltersExpression(filters...),
	}

	if intervalLevel != 0 {
		input.PredictionIntervalLevel = aws.Int32(intervalLevel)
	}

	repos, err := s.costRepositories(req.GetString("account_id", ""))
	if err != nil {
		return mcp.NewToolResultErrorFromErr("failed to initialise aws client", err), nil
	}

	log.Info().
		Str("start", window.startString()).
		Str("end", window.endString()).
		Str("granularity", string(granularity)).
		Str("metric", string(metric)).
		Strs("accounts", accountIDs(repos)).
		Msg("[mcpserver.handleGetCostForecast] forecasting cost")

	forecasts := make([]accountForecast, 0, len(repos))
	for _, r := range repos {
		data, err := r.repo.GetCostForecast(input)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("cost forecast failed for account "+r.accountID, err), nil
		}

		forecasts = append(forecasts, accountForecast{accountID: r.accountID, data: data})
	}

	return jsonResult(buildForecastResult(forecasts, window, granularity, metric, intervalLevel))
}

// resolveForecastWindow mirrors resolveCostWindow for the forward-looking named
// periods a forecast takes.
func resolveForecastWindow(period, start, end string, now time.Time) (costWindow, error) {
	period, start, end = strings.TrimSpace(period), strings.TrimSpace(start), strings.TrimSpace(end)

	if start != "" || end != "" {
		return resolveCostWindow("", start, end, now)
	}

	if period == "" {
		period = "this_month"
	}

	w, ok := namedForecastWindow(period, now)
	if !ok {
		return costWindow{}, errors.Errorf("unknown forecast period %q; call list_cost_dimensions for the named forecast periods, or pass explicit start and end dates", period)
	}

	return w, nil
}

func (s *Server) handleListCostDimensionValues(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	dimension, ok := resolveCostDimension(req.GetString("dimension", ""))
	if !ok {
		return mcp.NewToolResultErrorf("unknown dimension %q; call list_cost_dimensions for valid values", req.GetString("dimension", "")), nil
	}

	period := req.GetString("period", "")
	if period == "" && req.GetString("start", "") == "" {
		period = "last_30_days"
	}

	window, err := resolveCostWindow(period, req.GetString("start", ""), req.GetString("end", ""), time.Now())
	if err != nil {
		return mcp.NewToolResultErrorFromErr("invalid time period", err), nil
	}

	queryContext, err := parseCostContext(req.GetString("context", ""))
	if err != nil {
		return mcp.NewToolResultErrorFromErr("invalid context", err), nil
	}

	filters, err := parseCostFilters(req.GetArguments()["filters"])
	if err != nil {
		return mcp.NewToolResultErrorFromErr("invalid filters", err), nil
	}

	input := &awsce.GetDimensionValuesInput{
		Dimension: dimension,
		Context:   queryContext,
		TimePeriod: &cetypes.DateInterval{
			Start: aws.String(window.startString()),
			End:   aws.String(window.endString()),
		},
		Filter: costexplorer.FiltersExpression(filters...),
	}

	if search := strings.TrimSpace(req.GetString("search", "")); search != "" {
		input.SearchString = aws.String(search)
	}

	repos, err := s.costRepositories(req.GetString("account_id", ""))
	if err != nil {
		return mcp.NewToolResultErrorFromErr("failed to initialise aws client", err), nil
	}

	log.Info().
		Str("dimension", string(dimension)).
		Str("start", window.startString()).
		Str("end", window.endString()).
		Strs("accounts", accountIDs(repos)).
		Msg("[mcpserver.handleListCostDimensionValues] listing dimension values")

	var values []cetypes.DimensionValuesWithAttributes
	for _, r := range repos {
		found, err := r.repo.GetDimensionValues(input)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("dimension values query failed for account "+r.accountID, err), nil
		}

		values = append(values, found...)
	}

	limit := defaultDimensionValueLimit
	if requested := req.GetInt("limit", 0); requested > 0 {
		limit = min(requested, maxDimensionValueLimit)
	}

	merged, total := mergeDimensionValues(values, limit)

	return jsonResult(dimensionValuesResult{
		Dimension: string(dimension),
		Context:   string(queryContext),
		Start:     window.startString(),
		End:       window.endString(),
		Accounts:  accountIDs(repos),
		Count:     len(merged),
		Total:     total,
		Values:    merged,
	})
}

// costVocabulary is the static description of what the cost tools accept,
// returned by list_cost_dimensions.
type costVocabulary struct {
	Dimensions       []string          `json:"dimensions"`
	DimensionAliases map[string]string `json:"dimension_aliases"`
	Metrics          []string          `json:"metrics"`
	ForecastMetrics  []string          `json:"forecast_metrics"`
	Granularities    []string          `json:"granularities"`
	Periods          []string          `json:"periods"`
	ForecastPeriods  []string          `json:"forecast_periods"`
	MatchOptions     []string          `json:"match_options"`
	Contexts         []string          `json:"contexts"`
	GroupBy          costGroupByHelp   `json:"group_by"`
	Filters          costFiltersHelp   `json:"filters"`
}

type costGroupByHelp struct {
	MaxGroupings int      `json:"max_groupings"`
	Forms        []string `json:"forms"`
	Notes        string   `json:"notes"`
}

type costFiltersHelp struct {
	Shape    map[string]any `json:"row_shape"`
	Notes    string         `json:"notes"`
	Examples []any          `json:"examples"`
}

func (s *Server) handleListCostDimensions(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	aliases := make(map[string]string, len(costDimensionAliases))
	for alias, dim := range costDimensionAliases {
		aliases[strings.ToLower(alias)] = string(dim)
	}

	granularities := make([]string, 0)
	for _, g := range cetypes.Granularity("").Values() {
		granularities = append(granularities, string(g))
	}

	contexts := make([]string, 0)
	for _, c := range cetypes.Context("").Values() {
		contexts = append(contexts, string(c))
	}

	return jsonResult(costVocabulary{
		Dimensions:       costDimensionNames(),
		DimensionAliases: aliases,
		Metrics:          costMetricNames(),
		ForecastMetrics:  forecastMetricNames(),
		Granularities:    granularities,
		Periods:          costPeriodNames(),
		ForecastPeriods:  forecastPeriodNames(),
		MatchOptions:     matchOptionNames(),
		Contexts:         contexts,
		GroupBy: costGroupByHelp{
			MaxGroupings: costexplorer.MaxGroupBy,
			Forms:        []string{"<DIMENSION>", "TAG:<key>", "COST_CATEGORY:<key>"},
			Notes:        "Dimension names are case-insensitive and accept the aliases above. Grouped values are reported under the dimension name, 'TAG:<key>' or 'COST_CATEGORY:<key>'; resources missing a grouped tag are reported as '(not set)'.",
		},
		Filters: costFiltersHelp{
			Shape: costFilterItemSchema(),
			Notes: "Rows are AND-ed; values inside a row are OR-ed. get_cost_and_usage accepts EQUALS and CASE_SENSITIVE on dimensions, plus ABSENT on tags and cost categories; the remaining match options belong to cost category rules and are rejected. get_cost_forecast filters on dimensions only.",
			Examples: []any{
				[]map[string]any{{"key": "SERVICE", "values": []string{"Amazon Relational Database Service"}}},
				[]map[string]any{{"key": "RECORD_TYPE", "values": []string{"Credit", "Refund", "Tax"}, "exclude": true}},
				[]map[string]any{{"type": "tag", "key": "Team", "values": []string{"platform"}}, {"type": "tag", "key": "Environment", "absent": true}},
			},
		},
	})
}
