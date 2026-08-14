package mcpserver

import (
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	cetypes "github.com/aws/aws-sdk-go-v2/service/costexplorer/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/imunhatep/awslib/service/costexplorer"
)

// now is a fixed clock so the relative-period tests do not depend on the day
// they run: a Wednesday in the middle of a month, mid-year.
var now = time.Date(2026, 8, 12, 14, 30, 0, 0, time.UTC)

func TestResolveCostWindow_NamedPeriods(t *testing.T) {
	cases := []struct {
		period     string
		start, end string
	}{
		{"today", "2026-08-12", "2026-08-13"},
		{"yesterday", "2026-08-11", "2026-08-12"},
		// "last_*" windows stop at today, so they only cover fully elapsed days.
		{"last_7_days", "2026-08-05", "2026-08-12"},
		{"last_30_days", "2026-07-13", "2026-08-12"},
		{"last_45_days", "2026-06-28", "2026-08-12"},
		// "this_*" windows run through today, whose costs are still partial.
		{"this_month", "2026-08-01", "2026-08-13"},
		{"mtd", "2026-08-01", "2026-08-13"},
		{"last_month", "2026-07-01", "2026-08-01"},
		{"last_3_months", "2026-05-01", "2026-08-01"},
		{"last_12_months", "2025-08-01", "2026-08-01"},
		{"this_year", "2026-01-01", "2026-08-13"},
		{"last_year", "2025-01-01", "2026-01-01"},
		// Spelling variants fold onto the same window.
		{"Last 30 Days", "2026-07-13", "2026-08-12"},
		{"last-30-days", "2026-07-13", "2026-08-12"},
	}

	for _, tc := range cases {
		w, err := resolveCostWindow(tc.period, "", "", now)
		require.NoErrorf(t, err, "period %q", tc.period)
		assert.Equalf(t, tc.start, w.startString(), "period %q start", tc.period)
		assert.Equalf(t, tc.end, w.endString(), "period %q end", tc.period)
	}
}

func TestResolveCostWindow_DefaultsToLastThreeMonths(t *testing.T) {
	w, err := resolveCostWindow("", "", "", now)
	require.NoError(t, err)
	assert.Equal(t, "2026-05-01", w.startString())
	assert.Equal(t, "2026-08-01", w.endString())
}

func TestResolveCostWindow_ExplicitDates(t *testing.T) {
	w, err := resolveCostWindow("last_month", "2026-03-01", "2026-04-01", now)
	require.NoError(t, err)
	assert.Equal(t, "2026-03-01", w.startString(), "explicit dates override the named period")
	assert.Equal(t, "2026-04-01", w.endString())

	// HOURLY granularity is driven by timestamps rather than plain dates.
	hourly, err := resolveCostWindow("", "2026-08-11T00:00:00Z", "2026-08-12T00:00:00Z", now)
	require.NoError(t, err)
	assert.Equal(t, 0, hourly.start.Hour())
}

func TestResolveCostWindow_Errors(t *testing.T) {
	_, err := resolveCostWindow("", "2026-03-01", "", now)
	assert.ErrorContains(t, err, "given together")

	_, err = resolveCostWindow("", "2026-04-01", "2026-03-01", now)
	assert.ErrorContains(t, err, "must be after")

	_, err = resolveCostWindow("since_forever", "", "", now)
	assert.ErrorContains(t, err, "unknown period")

	_, err = resolveCostWindow("", "yesterday", "today", now)
	assert.ErrorContains(t, err, "invalid date")
}

func TestResolveForecastWindow(t *testing.T) {
	cases := []struct {
		period     string
		start, end string
	}{
		{"", "2026-08-12", "2026-09-01"}, // defaults to the rest of this month
		{"this_month", "2026-08-12", "2026-09-01"},
		{"next_month", "2026-09-01", "2026-10-01"},
		{"next_7_days", "2026-08-12", "2026-08-19"},
		{"next_3_months", "2026-08-12", "2026-11-12"},
	}

	for _, tc := range cases {
		w, err := resolveForecastWindow(tc.period, "", "", now)
		require.NoErrorf(t, err, "period %q", tc.period)
		assert.Equalf(t, tc.start, w.startString(), "period %q start", tc.period)
		assert.Equalf(t, tc.end, w.endString(), "period %q end", tc.period)
	}

	// Historical period names are not forecastable.
	_, err := resolveForecastWindow("last_month", "", "", now)
	assert.ErrorContains(t, err, "unknown forecast period")
}

func TestParseCostGranularity(t *testing.T) {
	g, err := parseCostGranularity("")
	require.NoError(t, err)
	assert.Equal(t, cetypes.GranularityMonthly, g)

	g, err = parseCostGranularity("daily")
	require.NoError(t, err)
	assert.Equal(t, cetypes.GranularityDaily, g)

	_, err = parseCostGranularity("weekly")
	assert.ErrorContains(t, err, "invalid granularity")
}

func TestParseCostMetrics(t *testing.T) {
	metrics, err := parseCostMetrics(nil)
	require.NoError(t, err)
	assert.Equal(t, []string{costexplorer.MetricUnblendedCost}, metrics)

	// Spelling variants and aliases all resolve to the canonical name, and
	// duplicates collapse.
	metrics, err = parseCostMetrics([]string{"unblended_cost", "AMORTIZED_COST", "UnblendedCost", "usage"})
	require.NoError(t, err)
	assert.Equal(t, []string{
		costexplorer.MetricUnblendedCost,
		costexplorer.MetricAmortizedCost,
		costexplorer.MetricUsageQuantity,
	}, metrics)

	_, err = parseCostMetrics([]string{"MonopolyMoney"})
	assert.ErrorContains(t, err, "unknown metric")
}

func TestParseForecastMetric(t *testing.T) {
	m, err := parseForecastMetric("")
	require.NoError(t, err)
	assert.Equal(t, cetypes.MetricUnblendedCost, m)

	// Both the enum spelling and the GetCostAndUsage spelling are accepted.
	for _, raw := range []string{"UNBLENDED_COST", "UnblendedCost", "unblended"} {
		m, err = parseForecastMetric(raw)
		require.NoErrorf(t, err, "metric %q", raw)
		assert.Equal(t, cetypes.MetricUnblendedCost, m)
	}

	_, err = parseForecastMetric("nope")
	assert.ErrorContains(t, err, "unknown forecast metric")
}

func TestParseCostGroupBy(t *testing.T) {
	groupings, err := parseCostGroupBy([]string{"service", "TAG:Team"})
	require.NoError(t, err)
	require.Len(t, groupings, 2)

	assert.Equal(t, "SERVICE", groupings[0].label)
	assert.Equal(t, cetypes.GroupDefinitionTypeDimension, groupings[0].def.Type)
	assert.Equal(t, "SERVICE", aws.ToString(groupings[0].def.Key))

	assert.Equal(t, "TAG:Team", groupings[1].label)
	assert.Equal(t, cetypes.GroupDefinitionTypeTag, groupings[1].def.Type)
	assert.Equal(t, "Team", aws.ToString(groupings[1].def.Key))
	assert.Equal(t, "Team$", groupings[1].prefix)

	// Aliases resolve onto the API's own names.
	groupings, err = parseCostGroupBy([]string{"account", "charge_type"})
	require.NoError(t, err)
	assert.Equal(t, "LINKED_ACCOUNT", groupings[0].label)
	assert.Equal(t, "RECORD_TYPE", groupings[1].label)

	groupings, err = parseCostGroupBy([]string{"cost_category:Team"})
	require.NoError(t, err)
	assert.Equal(t, "COST_CATEGORY:Team", groupings[0].label)
	assert.Equal(t, cetypes.GroupDefinitionTypeCostCategory, groupings[0].def.Type)
}

func TestParseCostGroupBy_Errors(t *testing.T) {
	_, err := parseCostGroupBy([]string{"colour"})
	assert.ErrorContains(t, err, "unknown group_by dimension")

	_, err = parseCostGroupBy([]string{"TAG:"})
	assert.ErrorContains(t, err, "missing a tag key")

	// The API takes at most two groupings.
	_, err = parseCostGroupBy([]string{"SERVICE", "REGION", "TAG:Team"})
	assert.ErrorContains(t, err, "at most 2")
}

func TestParseCostFilters_Rows(t *testing.T) {
	filters, err := parseCostFilters([]any{
		map[string]any{"key": "SERVICE", "values": []any{"Amazon Simple Storage Service"}},
		map[string]any{"type": "tag", "key": "Team", "values": []any{"platform", "data"}},
		map[string]any{"key": "RECORD_TYPE", "values": []any{"Credit"}, "exclude": true},
		map[string]any{"type": "cost_category", "key": "Department", "absent": true},
	})
	require.NoError(t, err)
	require.Len(t, filters, 4)

	dim, ok := filters[0].(costexplorer.DimensionFilter)
	require.True(t, ok, "a key naming a known dimension defaults to a dimension row")
	assert.Equal(t, cetypes.DimensionService, dim.Dimension)
	assert.Equal(t, []string{"Amazon Simple Storage Service"}, dim.Values)

	tag, ok := filters[1].(costexplorer.TagFilter)
	require.True(t, ok)
	assert.Equal(t, "Team", tag.Key)
	assert.Equal(t, []string{"platform", "data"}, tag.Values)

	excluded, ok := filters[2].(costexplorer.DimensionFilter)
	require.True(t, ok)
	assert.True(t, excluded.Exclude)
	assert.NotNil(t, excluded.Expression().Not, "an excluded row renders as a NOT")

	category, ok := filters[3].(costexplorer.CostCategoryFilter)
	require.True(t, ok)
	assert.Equal(t, []cetypes.MatchOption{cetypes.MatchOptionAbsent}, category.MatchOptions)
	assert.Empty(t, category.Values, "ABSENT matches on the key alone")
}

func TestParseCostFilters_PresentIsAbsentNegated(t *testing.T) {
	filters, err := parseCostFilters([]any{
		map[string]any{"type": "tag", "key": "Owner", "present": true},
	})
	require.NoError(t, err)
	require.Len(t, filters, 1)

	tag := filters[0].(costexplorer.TagFilter)
	assert.Equal(t, []cetypes.MatchOption{cetypes.MatchOptionAbsent}, tag.MatchOptions)
	assert.True(t, tag.Exclude, "present is absent negated, since the API has no positive key-exists operator")
}

func TestParseCostFilters_Shapes(t *testing.T) {
	// A JSON string, as some MCP clients send it.
	filters, err := parseCostFilters(`[{"key":"REGION","values":["eu-west-1"]}]`)
	require.NoError(t, err)
	require.Len(t, filters, 1)

	// A single unwrapped row.
	filters, err = parseCostFilters(map[string]any{"key": "REGION", "values": []any{"eu-west-1"}})
	require.NoError(t, err)
	require.Len(t, filters, 1)

	// Nothing at all.
	for _, empty := range []any{nil, "", "[]"} {
		filters, err = parseCostFilters(empty)
		require.NoError(t, err)
		assert.Empty(t, filters)
	}
}

func TestParseCostFilters_Errors(t *testing.T) {
	_, err := parseCostFilters([]any{map[string]any{"key": "SERVICE"}})
	assert.ErrorContains(t, err, "no values", "a row with nothing to match on would be silently dropped by the API")

	_, err = parseCostFilters([]any{map[string]any{"values": []any{"x"}}})
	assert.ErrorContains(t, err, "needs a key")

	_, err = parseCostFilters([]any{map[string]any{"type": "dimension", "key": "Colour", "values": []any{"blue"}}})
	assert.ErrorContains(t, err, "unknown cost dimension")

	_, err = parseCostFilters([]any{map[string]any{"key": "SERVICE", "values": []any{"x"}, "match_options": []any{"REGEX"}}})
	assert.ErrorContains(t, err, "unknown match option")

	_, err = parseCostFilters([]any{map[string]any{"type": "tag", "key": "Team", "absent": true, "present": true}})
	assert.ErrorContains(t, err, "both absent and present")

	_, err = parseCostFilters("{not json")
	assert.ErrorContains(t, err, "invalid filter")
}

// metricValue builds the string-amount shape the Cost Explorer API returns.
func metricValue(amount string) cetypes.MetricValue {
	return cetypes.MetricValue{Amount: aws.String(amount), Unit: aws.String("USD")}
}

func dateInterval(start, end string) *cetypes.DateInterval {
	return &cetypes.DateInterval{Start: aws.String(start), End: aws.String(end)}
}

// groupedResult builds a two-month, service-grouped answer for one account.
func groupedResult() *costexplorer.CostAndUsage {
	return &costexplorer.CostAndUsage{
		ResultsByTime: []cetypes.ResultByTime{
			{
				TimePeriod: dateInterval("2026-06-01", "2026-07-01"),
				Groups: []cetypes.Group{
					{Keys: []string{"Amazon Elastic Compute Cloud - Compute"}, Metrics: map[string]cetypes.MetricValue{"UnblendedCost": metricValue("100.5")}},
					{Keys: []string{"Amazon Simple Storage Service"}, Metrics: map[string]cetypes.MetricValue{"UnblendedCost": metricValue("10.25")}},
				},
			},
			{
				TimePeriod: dateInterval("2026-07-01", "2026-08-01"),
				Estimated:  true,
				Groups: []cetypes.Group{
					{Keys: []string{"Amazon Elastic Compute Cloud - Compute"}, Metrics: map[string]cetypes.MetricValue{"UnblendedCost": metricValue("120.25")}},
					{Keys: []string{"Amazon Relational Database Service"}, Metrics: map[string]cetypes.MetricValue{"UnblendedCost": metricValue("50")}},
				},
			},
		},
	}
}

func TestBuildCostResult_RanksGroupsAcrossTheWindow(t *testing.T) {
	groupings, err := parseCostGroupBy([]string{"SERVICE"})
	require.NoError(t, err)

	window, err := resolveCostWindow("", "2026-06-01", "2026-08-01", now)
	require.NoError(t, err)

	res := buildCostResult(
		[]accountCostResult{{accountID: "111111111111", data: groupedResult()}},
		window,
		cetypes.GranularityMonthly,
		[]string{costexplorer.MetricUnblendedCost},
		groupings,
		10,
		false,
	)

	assert.Equal(t, []string{"SERVICE"}, res.GroupBy)
	assert.Equal(t, []string{"111111111111"}, res.Accounts)
	assert.Empty(t, res.Note, "a single account needs no overlap warning")

	// With a group_by set the API leaves ResultByTime.Total empty, so the total
	// has to come from the groups.
	assert.InDelta(t, 281.0, res.Total["UnblendedCost"].Amount, 0.001)
	assert.Equal(t, "USD", res.Total["UnblendedCost"].Unit)

	require.Len(t, res.Groups, 3)
	assert.Equal(t, "Amazon Elastic Compute Cloud - Compute", res.Groups[0].Keys["SERVICE"])
	assert.InDelta(t, 220.75, res.Groups[0].Metrics["UnblendedCost"].Amount, 0.001, "the same group is summed across periods")
	assert.Equal(t, "Amazon Relational Database Service", res.Groups[1].Keys["SERVICE"])
	assert.Equal(t, "Amazon Simple Storage Service", res.Groups[2].Keys["SERVICE"])

	// Periods carry their own totals but no group breakdown unless asked for.
	require.Len(t, res.Periods, 2)
	assert.Equal(t, "2026-06-01", res.Periods[0].Start)
	assert.InDelta(t, 110.75, res.Periods[0].Total["UnblendedCost"].Amount, 0.001)
	assert.False(t, res.Periods[0].Estimated)
	assert.True(t, res.Periods[1].Estimated)
	assert.Empty(t, res.Periods[0].Groups)
}

func TestBuildCostResult_IncludePeriodsAndLimit(t *testing.T) {
	groupings, _ := parseCostGroupBy([]string{"SERVICE"})
	window, _ := resolveCostWindow("", "2026-06-01", "2026-08-01", now)

	res := buildCostResult(
		[]accountCostResult{{accountID: "111111111111", data: groupedResult()}},
		window,
		cetypes.GranularityMonthly,
		[]string{costexplorer.MetricUnblendedCost},
		groupings,
		1,
		true,
	)

	require.Len(t, res.Groups, 1, "the limit keeps only the costliest group")
	assert.Equal(t, 3, res.GroupCount)
	assert.Equal(t, 2, res.Truncated)

	// Per-period groups are restricted to the same kept set, so the two views
	// agree, while the period total still reflects everything.
	require.Len(t, res.Periods, 2)
	require.Len(t, res.Periods[0].Groups, 1)
	assert.Equal(t, "Amazon Elastic Compute Cloud - Compute", res.Periods[0].Groups[0].Keys["SERVICE"])
	assert.InDelta(t, 110.75, res.Periods[0].Total["UnblendedCost"].Amount, 0.001)
}

func TestBuildCostResult_UngroupedUsesPeriodTotals(t *testing.T) {
	window, _ := resolveCostWindow("", "2026-06-01", "2026-08-01", now)

	data := &costexplorer.CostAndUsage{
		ResultsByTime: []cetypes.ResultByTime{
			{TimePeriod: dateInterval("2026-06-01", "2026-07-01"), Total: map[string]cetypes.MetricValue{"UnblendedCost": metricValue("110.75")}},
			{TimePeriod: dateInterval("2026-07-01", "2026-08-01"), Total: map[string]cetypes.MetricValue{"UnblendedCost": metricValue("170.25")}},
		},
	}

	res := buildCostResult(
		[]accountCostResult{{accountID: "111111111111", data: data}},
		window,
		cetypes.GranularityMonthly,
		[]string{costexplorer.MetricUnblendedCost},
		nil,
		10,
		false,
	)

	assert.InDelta(t, 281.0, res.Total["UnblendedCost"].Amount, 0.001)
	assert.Empty(t, res.Groups)
	require.Len(t, res.Periods, 2)
	assert.InDelta(t, 170.25, res.Periods[1].Total["UnblendedCost"].Amount, 0.001)
}

func TestBuildCostResult_MergesAccounts(t *testing.T) {
	groupings, _ := parseCostGroupBy([]string{"SERVICE"})
	window, _ := resolveCostWindow("", "2026-06-01", "2026-07-01", now)

	second := &costexplorer.CostAndUsage{
		ResultsByTime: []cetypes.ResultByTime{{
			TimePeriod: dateInterval("2026-06-01", "2026-07-01"),
			Groups: []cetypes.Group{
				{Keys: []string{"Amazon Simple Storage Service"}, Metrics: map[string]cetypes.MetricValue{"UnblendedCost": metricValue("7.75")}},
			},
		}},
	}

	res := buildCostResult(
		[]accountCostResult{
			{accountID: "111111111111", data: groupedResult()},
			{accountID: "222222222222", data: second},
		},
		window,
		cetypes.GranularityMonthly,
		[]string{costexplorer.MetricUnblendedCost},
		groupings,
		10,
		false,
	)

	assert.Equal(t, []string{"111111111111", "222222222222"}, res.Accounts)
	assert.Contains(t, res.Note, "overlap", "summing several accounts is called out")

	// Rows from different accounts stay distinct rather than being folded into
	// one another.
	byAccount := map[string]map[string]float64{}
	for _, g := range res.Groups {
		account := g.Keys["account_id"]
		require.NotEmpty(t, account, "every group is attributed to its account")

		if byAccount[account] == nil {
			byAccount[account] = map[string]float64{}
		}
		byAccount[account][g.Keys["SERVICE"]] = g.Metrics["UnblendedCost"].Amount
	}

	assert.InDelta(t, 10.25, byAccount["111111111111"]["Amazon Simple Storage Service"], 0.001)
	assert.InDelta(t, 7.75, byAccount["222222222222"]["Amazon Simple Storage Service"], 0.001)
	assert.InDelta(t, 288.75, res.Total["UnblendedCost"].Amount, 0.001)
}

func TestBuildCostResult_StripsTagKeyPrefix(t *testing.T) {
	groupings, _ := parseCostGroupBy([]string{"TAG:Team"})
	window, _ := resolveCostWindow("", "2026-06-01", "2026-07-01", now)

	data := &costexplorer.CostAndUsage{
		ResultsByTime: []cetypes.ResultByTime{{
			TimePeriod: dateInterval("2026-06-01", "2026-07-01"),
			Groups: []cetypes.Group{
				{Keys: []string{"Team$platform"}, Metrics: map[string]cetypes.MetricValue{"UnblendedCost": metricValue("30")}},
				{Keys: []string{"Team$"}, Metrics: map[string]cetypes.MetricValue{"UnblendedCost": metricValue("5")}},
			},
		}},
	}

	res := buildCostResult(
		[]accountCostResult{{accountID: "111111111111", data: data}},
		window,
		cetypes.GranularityMonthly,
		[]string{costexplorer.MetricUnblendedCost},
		groupings,
		10,
		false,
	)

	require.Len(t, res.Groups, 2)
	assert.Equal(t, "platform", res.Groups[0].Keys["TAG:Team"], "the API's <key>$<value> encoding is unwrapped")
	assert.Equal(t, "(not set)", res.Groups[1].Keys["TAG:Team"], "untagged spend is labelled rather than left blank")
}

func TestMergeDimensionValues(t *testing.T) {
	values := []cetypes.DimensionValuesWithAttributes{
		{Value: aws.String("222222222222"), Attributes: map[string]string{"description": "prod"}},
		{Value: aws.String("111111111111"), Attributes: map[string]string{"description": "dev"}},
		{Value: aws.String("222222222222"), Attributes: map[string]string{"description": "prod"}},
	}

	merged, total := mergeDimensionValues(values, 10)
	require.Len(t, merged, 2, "values seen in several accounts are reported once")
	assert.Equal(t, 2, total)
	assert.Equal(t, "111111111111", merged[0].Value)
	assert.Equal(t, "prod", merged[1].Attributes["description"])

	merged, total = mergeDimensionValues(values, 1)
	assert.Len(t, merged, 1)
	assert.Equal(t, 2, total, "total reports what was found before the limit applied")
}

func TestParseCostContext(t *testing.T) {
	c, err := parseCostContext("")
	require.NoError(t, err)
	assert.Equal(t, cetypes.ContextCostAndUsage, c)

	c, err = parseCostContext("savings_plans")
	require.NoError(t, err)
	assert.Equal(t, cetypes.ContextSavingsPlans, c)

	_, err = parseCostContext("guesswork")
	assert.ErrorContains(t, err, "unknown context")
}
