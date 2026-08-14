package mcpserver

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsce "github.com/aws/aws-sdk-go-v2/service/costexplorer"
	cetypes "github.com/aws/aws-sdk-go-v2/service/costexplorer/types"

	ptypes "github.com/imunhatep/awslib/provider/types"
	"github.com/imunhatep/awslib/service/costexplorer"

	"github.com/imunhatep/aws-mcp-go/pkg/errors"
)

// Bounds on how much a single Cost Explorer answer may return. Cost queries fan
// out over dimensions with unbounded cardinality (USAGE_TYPE, RESOURCE_ID), so
// the tools rank groups by spend and return the head of the list rather than
// everything.
const (
	defaultCostGroupLimit      = 25
	maxCostGroupLimit          = 1000
	defaultDimensionValueLimit = 100
	maxDimensionValueLimit     = 1000
)

// -----------------------------------------------------------------------------
// Repositories
// -----------------------------------------------------------------------------

// costRepository is the slice of awslib's Cost Explorer repository the tools
// use. Both costexplorer.CostExplorerRepository and its cached wrapper satisfy
// it, so caching is a construction-time detail.
type costRepository interface {
	GetCostAndUsageByQuery(q costexplorer.CostQuery) (*costexplorer.CostAndUsage, error)
	GetCostForecast(query *awsce.GetCostForecastInput) (*awsce.GetCostForecastOutput, error)
	GetDimensionValues(query *awsce.GetDimensionValuesInput) ([]cetypes.DimensionValuesWithAttributes, error)
}

// accountCostRepository pairs a repository with the account it reports on, so
// merged results stay attributable.
type accountCostRepository struct {
	accountID string
	repo      costRepository
}

// costRepositories resolves one Cost Explorer repository per account the pool
// can reach, narrowed to a single account when accountID is set.
//
// Cost Explorer is a global service served from us-east-1, so clients are
// requested for that region only — the region of the client has no bearing on
// the data returned, only on which credentials sign the request.
func (s *Server) costRepositories(accountID string) ([]accountCostRepository, error) {
	accountID = strings.TrimSpace(accountID)

	clients, err := s.pool.GetClients(ptypes.DefaultAwsRegion)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	repos := make([]accountCostRepository, 0, len(clients))
	seen := map[string]bool{}

	for _, client := range clients {
		id := client.GetAccountID().String()
		if accountID != "" && id != accountID {
			continue
		}
		if id != "" && seen[id] {
			continue
		}
		seen[id] = true

		repo := costexplorer.NewCostExplorerRepository(s.ctx, client)
		if s.cache != nil {
			repos = append(repos, accountCostRepository{accountID: id, repo: repo.WithCache(s.cache)})
			continue
		}

		repos = append(repos, accountCostRepository{accountID: id, repo: repo})
	}

	if len(repos) == 0 {
		if accountID != "" {
			return nil, errors.Errorf("account %q is not reachable; call list_accounts for the available accounts", accountID)
		}
		return nil, errors.New("no AWS accounts are reachable")
	}

	sort.Slice(repos, func(i, j int) bool { return repos[i].accountID < repos[j].accountID })

	return repos, nil
}

// accountIDs lists the accounts a set of repositories reports on.
func accountIDs(repos []accountCostRepository) []string {
	ids := make([]string, 0, len(repos))
	for _, r := range repos {
		ids = append(ids, r.accountID)
	}
	return ids
}

// multiAccountNote warns that per-account results were summed. Cost Explorer in
// a management account already reports its members' spend, so a pool holding
// both a payer and its members double counts unless a single account is picked.
const multiAccountNote = "each account was queried independently and the results summed; if the pool holds both a management (payer) account and its members their costs overlap — pass account_id to scope the query to one account"

// -----------------------------------------------------------------------------
// Time periods
// -----------------------------------------------------------------------------

// costWindow is a resolved query window. End is exclusive, as the Cost Explorer
// API requires.
type costWindow struct {
	start time.Time
	end   time.Time
}

func (w costWindow) startString() string { return w.start.Format(costDateLayout) }
func (w costWindow) endString() string   { return w.end.Format(costDateLayout) }

const costDateLayout = "2006-01-02"

// costDateLayouts are the input forms accepted for explicit start/end
// arguments. A plain date covers DAILY and MONTHLY; HOURLY granularity needs a
// timestamp.
var costDateLayouts = []string{costDateLayout, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05"}

func parseCostDate(raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	for _, layout := range costDateLayouts {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC(), nil
		}
	}

	return time.Time{}, errors.Errorf("invalid date %q; use YYYY-MM-DD, or an RFC3339 timestamp for HOURLY granularity", raw)
}

// resolveCostWindow turns the period / start / end arguments into a concrete
// window. Explicit start and end win; otherwise the named period is resolved
// relative to now, defaulting to the last three complete months.
func resolveCostWindow(period, start, end string, now time.Time) (costWindow, error) {
	period, start, end = strings.TrimSpace(period), strings.TrimSpace(start), strings.TrimSpace(end)

	if (start == "") != (end == "") {
		return costWindow{}, errors.New("start and end must be given together; pass a period instead to use a named window")
	}

	if start != "" {
		s, err := parseCostDate(start)
		if err != nil {
			return costWindow{}, err
		}

		e, err := parseCostDate(end)
		if err != nil {
			return costWindow{}, err
		}

		if !e.After(s) {
			return costWindow{}, errors.Errorf("end (%s) must be after start (%s); end is exclusive", end, start)
		}

		return costWindow{start: s, end: e}, nil
	}

	if period == "" {
		period = "last_3_months"
	}

	w, ok := namedCostWindow(period, now)
	if !ok {
		return costWindow{}, errors.Errorf("unknown period %q; call list_cost_dimensions for the named periods, or pass explicit start and end dates", period)
	}

	return w, nil
}

// relativePeriodPattern matches the generic last_<n>_days / last_<n>_months and
// next_<n>_days / next_<n>_months forms, so any span is expressible without
// enumerating every name.
var relativePeriodPattern = regexp.MustCompile(`^(last|next)_(\d+)_(days?|months?)$`)

// normalizePeriodName folds the spelling variants a caller may produce
// ("Last 30 Days", "last-30-days") onto one form.
func normalizePeriodName(raw string) string {
	name := strings.ToLower(strings.TrimSpace(raw))
	name = strings.ReplaceAll(name, "-", "_")
	name = strings.ReplaceAll(name, " ", "_")

	return name
}

// namedCostWindow resolves a named historical period relative to now.
//
// Periods named "last_*" cover whole, already-elapsed units and never include
// today, so their results are stable and fully billed. Periods named "this_*",
// plus "today", run up to and including today, whose costs are still partial.
func namedCostWindow(raw string, now time.Time) (costWindow, bool) {
	now = now.UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	tomorrow := today.AddDate(0, 0, 1)
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	yearStart := time.Date(now.Year(), 1, 1, 0, 0, 0, 0, time.UTC)

	name := normalizePeriodName(raw)

	switch name {
	case "today":
		return costWindow{today, tomorrow}, true
	case "yesterday":
		return costWindow{today.AddDate(0, 0, -1), today}, true
	case "this_month", "mtd", "month_to_date":
		return costWindow{monthStart, tomorrow}, true
	case "last_month":
		return costWindow{monthStart.AddDate(0, -1, 0), monthStart}, true
	case "this_year", "ytd", "year_to_date":
		return costWindow{yearStart, tomorrow}, true
	case "last_year":
		return costWindow{yearStart.AddDate(-1, 0, 0), yearStart}, true
	}

	m := relativePeriodPattern.FindStringSubmatch(name)
	if m == nil || m[1] != "last" {
		return costWindow{}, false
	}

	n, err := strconv.Atoi(m[2])
	if err != nil || n <= 0 {
		return costWindow{}, false
	}

	if strings.HasPrefix(m[3], "day") {
		return costWindow{today.AddDate(0, 0, -n), today}, true
	}

	return costWindow{monthStart.AddDate(0, -n, 0), monthStart}, true
}

// namedForecastWindow resolves a named forecast period. Every window starts no
// earlier than today, which GetCostForecast requires.
func namedForecastWindow(raw string, now time.Time) (costWindow, bool) {
	now = now.UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	nextMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, 0)

	name := normalizePeriodName(raw)

	switch name {
	case "this_month", "rest_of_month", "month_to_end":
		return costWindow{today, nextMonth}, true
	case "next_month":
		return costWindow{nextMonth, nextMonth.AddDate(0, 1, 0)}, true
	}

	m := relativePeriodPattern.FindStringSubmatch(name)
	if m == nil || m[1] != "next" {
		return costWindow{}, false
	}

	n, err := strconv.Atoi(m[2])
	if err != nil || n <= 0 {
		return costWindow{}, false
	}

	if strings.HasPrefix(m[3], "day") {
		return costWindow{today, today.AddDate(0, 0, n)}, true
	}

	return costWindow{today, today.AddDate(0, n, 0)}, true
}

// costPeriodNames and forecastPeriodNames document the named windows for the
// list_cost_dimensions tool.
func costPeriodNames() []string {
	return []string{
		"today", "yesterday",
		"last_7_days", "last_14_days", "last_30_days", "last_90_days", "last_<n>_days",
		"this_month (mtd)", "last_month", "last_3_months", "last_6_months", "last_12_months", "last_<n>_months",
		"this_year (ytd)", "last_year",
	}
}

func forecastPeriodNames() []string {
	return []string{
		"this_month", "next_month",
		"next_7_days", "next_30_days", "next_90_days", "next_<n>_days",
		"next_3_months", "next_12_months", "next_<n>_months",
	}
}

// -----------------------------------------------------------------------------
// Granularity and metrics
// -----------------------------------------------------------------------------

// parseCostGranularity resolves the granularity argument, defaulting to MONTHLY
// — the unit most cost questions are asked in, and the one that keeps responses
// smallest.
func parseCostGranularity(raw string) (cetypes.Granularity, error) {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "":
		return cetypes.GranularityMonthly, nil
	case string(cetypes.GranularityDaily):
		return cetypes.GranularityDaily, nil
	case string(cetypes.GranularityMonthly):
		return cetypes.GranularityMonthly, nil
	case string(cetypes.GranularityHourly):
		return cetypes.GranularityHourly, nil
	default:
		return "", errors.Errorf("invalid granularity %q; use DAILY, MONTHLY or HOURLY", raw)
	}
}

// costMetricNames lists the metrics GetCostAndUsage accepts, in the mixed-case
// spelling the API returns them under.
func costMetricNames() []string {
	return []string{
		costexplorer.MetricUnblendedCost,
		costexplorer.MetricAmortizedCost,
		costexplorer.MetricBlendedCost,
		costexplorer.MetricNetUnblendedCost,
		costexplorer.MetricNetAmortizedCost,
		costexplorer.MetricUsageQuantity,
		costexplorer.MetricNormalizedUsage,
	}
}

// costMetricAliases maps the shorthands a caller is likely to use onto the
// canonical metric names. Lookup keys are folded by foldMetricName.
var costMetricAliases = map[string]string{
	"cost":            costexplorer.MetricUnblendedCost,
	"unblended":       costexplorer.MetricUnblendedCost,
	"amortized":       costexplorer.MetricAmortizedCost,
	"blended":         costexplorer.MetricBlendedCost,
	"netunblended":    costexplorer.MetricNetUnblendedCost,
	"netamortized":    costexplorer.MetricNetAmortizedCost,
	"usage":           costexplorer.MetricUsageQuantity,
	"normalizedusage": costexplorer.MetricNormalizedUsage,
}

// foldMetricName strips the separators and case that distinguish
// "NetUnblendedCost", "net_unblended_cost" and "NET_UNBLENDED_COST".
func foldMetricName(raw string) string {
	folded := strings.ToLower(strings.TrimSpace(raw))
	for _, sep := range []string{"_", "-", " "} {
		folded = strings.ReplaceAll(folded, sep, "")
	}

	return folded
}

// parseCostMetrics resolves the requested metrics, defaulting to UnblendedCost
// — the metric that matches what an account is actually invoiced.
func parseCostMetrics(raw []string) ([]string, error) {
	canonical := map[string]string{}
	for _, m := range costMetricNames() {
		canonical[foldMetricName(m)] = m
	}

	metrics := make([]string, 0, len(raw))
	seen := map[string]bool{}

	for _, entry := range raw {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}

		folded := foldMetricName(entry)
		name, ok := canonical[folded]
		if !ok {
			if name, ok = costMetricAliases[folded]; !ok {
				return nil, errors.Errorf("unknown metric %q; use one of %s", entry, strings.Join(costMetricNames(), ", "))
			}
		}

		if !seen[name] {
			seen[name] = true
			metrics = append(metrics, name)
		}
	}

	if len(metrics) == 0 {
		return []string{costexplorer.MetricUnblendedCost}, nil
	}

	return metrics, nil
}

// forecastMetricNames lists the metrics GetCostForecast accepts, which are
// spelled as an upper-snake enum rather than the mixed case GetCostAndUsage uses.
func forecastMetricNames() []string {
	names := make([]string, 0)
	for _, m := range cetypes.Metric("").Values() {
		names = append(names, string(m))
	}

	return names
}

// parseForecastMetric resolves the single metric a forecast is built on,
// accepting either spelling (UNBLENDED_COST or UnblendedCost).
func parseForecastMetric(raw string) (cetypes.Metric, error) {
	if strings.TrimSpace(raw) == "" {
		return cetypes.MetricUnblendedCost, nil
	}

	folded := foldMetricName(raw)
	for _, m := range cetypes.Metric("").Values() {
		if foldMetricName(string(m)) == folded {
			return m, nil
		}
	}

	if alias, ok := costMetricAliases[folded]; ok {
		return parseForecastMetric(alias)
	}

	return "", errors.Errorf("unknown forecast metric %q; use one of %s", raw, strings.Join(forecastMetricNames(), ", "))
}

// -----------------------------------------------------------------------------
// Dimensions, group by and filters
// -----------------------------------------------------------------------------

// costDimensionAliases maps friendly names onto Cost Explorer dimensions. The
// dimensions themselves are accepted verbatim, so this only covers the names a
// caller reaches for that the API does not use.
var costDimensionAliases = map[string]cetypes.Dimension{
	"ACCOUNT":           cetypes.DimensionLinkedAccount,
	"ACCOUNT_ID":        cetypes.DimensionLinkedAccount,
	"ACCOUNT_NAME":      cetypes.DimensionLinkedAccountName,
	"API_OPERATION":     cetypes.DimensionOperation,
	"AVAILABILITY_ZONE": cetypes.DimensionAz,
	"CHARGE_TYPE":       cetypes.DimensionRecordType,
	"ENGINE":            cetypes.DimensionDatabaseEngine,
	"INSTANCE_FAMILY":   cetypes.DimensionInstanceTypeFamily,
	"OS":                cetypes.DimensionOperatingSystem,
	"PURCHASE_OPTION":   cetypes.DimensionPurchaseType,
}

// costDimensionNames lists every dimension the API defines, for the
// list_cost_dimensions tool.
func costDimensionNames() []string {
	names := make([]string, 0)
	for _, d := range cetypes.Dimension("").Values() {
		names = append(names, string(d))
	}
	sort.Strings(names)

	return names
}

// resolveCostDimension accepts a dimension in any casing or separator style, or
// one of the friendly aliases.
func resolveCostDimension(raw string) (cetypes.Dimension, bool) {
	name := strings.ToUpper(strings.TrimSpace(raw))
	name = strings.ReplaceAll(name, "-", "_")
	name = strings.ReplaceAll(name, " ", "_")

	if d, ok := costDimensionAliases[name]; ok {
		return d, true
	}

	for _, d := range cetypes.Dimension("").Values() {
		if string(d) == name {
			return d, true
		}
	}

	return "", false
}

// costGrouping is one resolved group_by entry: the definition sent to the API
// plus the label the grouped values are reported under.
type costGrouping struct {
	def   cetypes.GroupDefinition
	label string
	// prefix is the "<key>$" fragment the API prepends to tag and cost category
	// group keys, stripped before the value is reported.
	prefix string
}

// cutPrefixFold strips a case-insensitive prefix, reporting whether it matched.
func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) < len(prefix) || !strings.EqualFold(s[:len(prefix)], prefix) {
		return s, false
	}

	return s[len(prefix):], true
}

// parseCostGroupBy resolves the group_by arguments into API group definitions.
// Entries are dimensions (SERVICE, service, LINKED_ACCOUNT, ...), tags
// ("TAG:Team") or cost categories ("COST_CATEGORY:Team").
func parseCostGroupBy(raw []string) ([]costGrouping, error) {
	groupings := make([]costGrouping, 0, len(raw))

	for _, entry := range raw {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}

		if key, ok := cutTagPrefix(entry); ok {
			if key == "" {
				return nil, errors.Errorf("group_by entry %q is missing a tag key; use TAG:<key>", entry)
			}

			groupings = append(groupings, costGrouping{
				def:    costexplorer.GroupByTag(key),
				label:  "TAG:" + key,
				prefix: key + "$",
			})

			continue
		}

		if key, ok := cutCostCategoryPrefix(entry); ok {
			if key == "" {
				return nil, errors.Errorf("group_by entry %q is missing a cost category key; use COST_CATEGORY:<key>", entry)
			}

			groupings = append(groupings, costGrouping{
				def:    costexplorer.GroupByCostCategory(key),
				label:  "COST_CATEGORY:" + key,
				prefix: key + "$",
			})

			continue
		}

		dim, ok := resolveCostDimension(entry)
		if !ok {
			return nil, errors.Errorf("unknown group_by dimension %q; call list_cost_dimensions for valid values, or use TAG:<key> / COST_CATEGORY:<key>", entry)
		}

		groupings = append(groupings, costGrouping{
			def:   costexplorer.GroupByDimension(dim),
			label: string(dim),
		})
	}

	if len(groupings) > costexplorer.MaxGroupBy {
		return nil, errors.Errorf("group_by has %d entries; Cost Explorer accepts at most %d per query", len(groupings), costexplorer.MaxGroupBy)
	}

	return groupings, nil
}

func cutTagPrefix(entry string) (string, bool) {
	for _, prefix := range []string{"tag:", "tags:", "tag."} {
		if key, ok := cutPrefixFold(entry, prefix); ok {
			return strings.TrimSpace(key), true
		}
	}

	return entry, false
}

func cutCostCategoryPrefix(entry string) (string, bool) {
	for _, prefix := range []string{"cost_category:", "cost-category:", "costcategory:", "cc:"} {
		if key, ok := cutPrefixFold(entry, prefix); ok {
			return strings.TrimSpace(key), true
		}
	}

	return entry, false
}

// groupDefinitions projects the resolved groupings back into the API type.
func groupDefinitions(groupings []costGrouping) []cetypes.GroupDefinition {
	defs := make([]cetypes.GroupDefinition, 0, len(groupings))
	for _, g := range groupings {
		defs = append(defs, g.def)
	}

	return defs
}

// groupLabels lists the labels grouped values are reported under.
func groupLabels(groupings []costGrouping) []string {
	labels := make([]string, 0, len(groupings))
	for _, g := range groupings {
		labels = append(labels, g.label)
	}

	return labels
}

// costFilterSpec is one filter row as supplied by a caller — the JSON shape of
// the tools' filters argument. Rows are AND-ed together; the values inside a row
// are OR-ed by the API.
type costFilterSpec struct {
	// Type is dimension, tag or cost_category. When empty it is inferred: a key
	// that names a known dimension is a dimension, anything else is a tag.
	Type string `json:"type"`
	// Key is the dimension name (SERVICE), tag key (Team) or cost category name.
	Key string `json:"key"`
	// Values are OR-ed by the API.
	Values []string `json:"values"`
	// MatchOptions selects the operator. GetCostAndUsage accepts EQUALS and
	// CASE_SENSITIVE on dimensions, plus ABSENT on tags and cost categories.
	MatchOptions []string `json:"match_options"`
	// Exclude wraps the row in a NOT — the console's "Exclude" toggle.
	Exclude bool `json:"exclude"`
	// Absent matches resources that do not carry the tag key / fall outside the
	// cost category at all. Values are ignored.
	Absent bool `json:"absent"`
	// Present matches resources carrying the key with any value — Absent negated.
	Present bool `json:"present"`
}

// matchOptionNames lists the operators the API defines, for
// list_cost_dimensions.
func matchOptionNames() []string {
	names := make([]string, 0)
	for _, o := range cetypes.MatchOption("").Values() {
		names = append(names, string(o))
	}

	return names
}

func parseMatchOptions(raw []string) ([]cetypes.MatchOption, error) {
	options := make([]cetypes.MatchOption, 0, len(raw))

	for _, entry := range raw {
		name := strings.ToUpper(strings.TrimSpace(entry))
		name = strings.ReplaceAll(name, "-", "_")
		name = strings.ReplaceAll(name, " ", "_")

		if name == "" {
			continue
		}

		var found bool
		for _, o := range cetypes.MatchOption("").Values() {
			if string(o) == name {
				options = append(options, o)
				found = true

				break
			}
		}

		if !found {
			return nil, errors.Errorf("unknown match option %q; use one of %s", entry, strings.Join(matchOptionNames(), ", "))
		}
	}

	return options, nil
}

// toFilter renders one caller-supplied row as an awslib filter.
func (spec costFilterSpec) toFilter() (costexplorer.Filter, error) {
	key := strings.TrimSpace(spec.Key)
	if key == "" {
		return nil, errors.New("every filter row needs a key (a dimension name, tag key or cost category name)")
	}

	if spec.Absent && spec.Present {
		return nil, errors.Errorf("filter row %q sets both absent and present", key)
	}

	options, err := parseMatchOptions(spec.MatchOptions)
	if err != nil {
		return nil, err
	}

	values := make([]string, 0, len(spec.Values))
	for _, v := range spec.Values {
		if v = strings.TrimSpace(v); v != "" {
			values = append(values, v)
		}
	}

	exclude := spec.Exclude

	// ABSENT matches on the key alone, so any values are meaningless. "Present"
	// is the same test negated, since the API has no positive "key exists"
	// operator.
	if spec.Absent || spec.Present {
		options = []cetypes.MatchOption{cetypes.MatchOptionAbsent}
		values = nil

		if spec.Present {
			exclude = !exclude
		}
	} else if len(values) == 0 {
		return nil, errors.Errorf("filter row %q has no values; supply values, or set absent/present to match on the key alone", key)
	}

	kind := strings.ToLower(strings.TrimSpace(spec.Type))
	kind = strings.ReplaceAll(kind, "-", "_")

	if kind == "" {
		if _, ok := resolveCostDimension(key); ok {
			kind = "dimension"
		} else {
			kind = "tag"
		}
	}

	switch kind {
	case "dimension", "dimensions":
		dim, ok := resolveCostDimension(key)
		if !ok {
			return nil, errors.Errorf("unknown cost dimension %q; call list_cost_dimensions for valid values", key)
		}

		return costexplorer.DimensionFilter{Dimension: dim, Values: values, MatchOptions: options, Exclude: exclude}, nil

	case "tag", "tags":
		return costexplorer.TagFilter{Key: key, Values: values, MatchOptions: options, Exclude: exclude}, nil

	case "cost_category", "cost_categories", "costcategory":
		return costexplorer.CostCategoryFilter{Key: key, Values: values, MatchOptions: options, Exclude: exclude}, nil

	default:
		return nil, errors.Errorf("unknown filter type %q; use dimension, tag or cost_category", spec.Type)
	}
}

// parseCostFilters decodes the filters argument. MCP clients deliver it either
// as a decoded JSON array or as a JSON string, and a single row may arrive
// unwrapped, so all three shapes are accepted.
func parseCostFilters(raw any) ([]costexplorer.Filter, error) {
	specs, err := decodeFilterSpecs(raw)
	if err != nil {
		return nil, err
	}

	filters := make([]costexplorer.Filter, 0, len(specs))
	for _, spec := range specs {
		f, err := spec.toFilter()
		if err != nil {
			return nil, err
		}

		filters = append(filters, f)
	}

	return filters, nil
}

func decodeFilterSpecs(raw any) ([]costFilterSpec, error) {
	if raw == nil {
		return nil, nil
	}

	var encoded []byte

	switch v := raw.(type) {
	case string:
		if strings.TrimSpace(v) == "" {
			return nil, nil
		}
		encoded = []byte(v)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return nil, errors.Errorf("invalid filters argument: %s", err)
		}
		encoded = b
	}

	trimmed := strings.TrimSpace(string(encoded))
	if trimmed == "" || trimmed == "null" || trimmed == "[]" {
		return nil, nil
	}

	// A lone object is treated as a one-row filter list.
	if strings.HasPrefix(trimmed, "{") {
		var spec costFilterSpec
		if err := json.Unmarshal(encoded, &spec); err != nil {
			return nil, errors.Errorf("invalid filter row: %s", err)
		}

		return []costFilterSpec{spec}, nil
	}

	var specs []costFilterSpec
	if err := json.Unmarshal(encoded, &specs); err != nil {
		return nil, errors.Errorf("invalid filters argument: %s; expected a list of {type,key,values,match_options,exclude,absent,present} rows", err)
	}

	return specs, nil
}

// -----------------------------------------------------------------------------
// Result shaping
// -----------------------------------------------------------------------------

// costMetricValue is one metric's amount, parsed out of the string the API
// returns so callers can compare and sum without re-parsing.
type costMetricValue struct {
	Amount float64 `json:"amount"`
	Unit   string  `json:"unit,omitempty"`
}

// costGroupRow is one grouped bucket: the values of every group_by dimension and
// the metrics accumulated for them.
type costGroupRow struct {
	Keys    map[string]string          `json:"keys"`
	Metrics map[string]costMetricValue `json:"metrics"`
}

// costTimeBucket is one time period of the result. Groups are populated only
// when the caller asked for the per-period breakdown.
type costTimeBucket struct {
	Start     string                     `json:"start"`
	End       string                     `json:"end"`
	Estimated bool                       `json:"estimated,omitempty"`
	Total     map[string]costMetricValue `json:"total"`
	Groups    []costGroupRow             `json:"groups,omitempty"`
}

// costResult is the envelope returned by get_cost_and_usage. Groups aggregate
// the whole window and are ranked by spend; Periods carry the same data split by
// time, for trends.
type costResult struct {
	Start       string   `json:"start"`
	End         string   `json:"end"`
	Granularity string   `json:"granularity"`
	Metrics     []string `json:"metrics"`
	GroupBy     []string `json:"group_by,omitempty"`
	Accounts    []string `json:"accounts"`

	Total  map[string]costMetricValue `json:"total"`
	Groups []costGroupRow             `json:"groups,omitempty"`
	// GroupCount is how many groups the window held before ranking; Truncated is
	// how many of them the limit dropped.
	GroupCount int `json:"group_count,omitempty"`
	Truncated  int `json:"groups_truncated,omitempty"`

	Periods []costTimeBucket `json:"periods,omitempty"`
	Note    string           `json:"note,omitempty"`
}

// accountCostResult is one account's raw answer, kept attributable until the
// results are merged.
type accountCostResult struct {
	accountID string
	data      *costexplorer.CostAndUsage
}

// metricAcc accumulates one metric across accounts, periods and groups.
type metricAcc struct {
	amount float64
	unit   string
}

func addMetricValues(dst map[string]*metricAcc, src map[string]cetypes.MetricValue) {
	for name, value := range src {
		acc, ok := dst[name]
		if !ok {
			acc = &metricAcc{}
			dst[name] = acc
		}

		if amount, err := strconv.ParseFloat(aws.ToString(value.Amount), 64); err == nil {
			acc.amount += amount
		}

		if acc.unit == "" {
			acc.unit = aws.ToString(value.Unit)
		}
	}
}

func renderMetrics(acc map[string]*metricAcc) map[string]costMetricValue {
	out := make(map[string]costMetricValue, len(acc))
	for name, a := range acc {
		out[name] = costMetricValue{Amount: roundAmount(a.amount), Unit: a.unit}
	}

	return out
}

// roundAmount trims the float noise that accumulates when summing the API's
// string amounts, which carry ten decimal places.
func roundAmount(v float64) float64 {
	rounded, err := strconv.ParseFloat(strconv.FormatFloat(v, 'f', 6, 64), 64)
	if err != nil {
		return v
	}

	return rounded
}

// groupBucket accumulates one group's metrics across accounts and periods.
type groupBucket struct {
	keys    map[string]string
	metrics map[string]*metricAcc
	order   int
}

// costAccumulator merges the per-account, per-period results into the ranked and
// time-sliced views the tool returns.
type costAccumulator struct {
	groupings    []costGrouping
	multiAccount bool

	windows map[string]*windowBucket
	order   []string

	groups map[string]*groupBucket
}

type windowBucket struct {
	start, end string
	estimated  bool
	total      map[string]*metricAcc
	groups     map[string]*groupBucket
}

func newCostAccumulator(groupings []costGrouping, multiAccount bool) *costAccumulator {
	return &costAccumulator{
		groupings:    groupings,
		multiAccount: multiAccount,
		windows:      map[string]*windowBucket{},
		groups:       map[string]*groupBucket{},
	}
}

// add folds one account's answer into the accumulator.
func (a *costAccumulator) add(accountID string, data *costexplorer.CostAndUsage) {
	if data == nil {
		return
	}

	for _, result := range data.ResultsByTime {
		start, end := aws.ToString(result.TimePeriod.Start), aws.ToString(result.TimePeriod.End)
		windowKey := start + "|" + end

		window, ok := a.windows[windowKey]
		if !ok {
			window = &windowBucket{start: start, end: end, total: map[string]*metricAcc{}, groups: map[string]*groupBucket{}}
			a.windows[windowKey] = window
			a.order = append(a.order, windowKey)
		}

		window.estimated = window.estimated || result.Estimated
		addMetricValues(window.total, result.Total)

		for _, group := range result.Groups {
			keys := a.groupKeys(group, accountID)
			groupKey := renderGroupKey(keys)

			addMetricValues(a.bucket(a.groups, groupKey, keys).metrics, group.Metrics)
			addMetricValues(a.bucket(window.groups, groupKey, keys).metrics, group.Metrics)
		}
	}
}

func (a *costAccumulator) bucket(buckets map[string]*groupBucket, key string, keys map[string]string) *groupBucket {
	b, ok := buckets[key]
	if !ok {
		b = &groupBucket{keys: keys, metrics: map[string]*metricAcc{}, order: len(buckets)}
		buckets[key] = b
	}

	return b
}

// groupKeys labels a returned group's values with the dimensions they belong to,
// adding the account when several were queried so merged rows stay distinct.
func (a *costAccumulator) groupKeys(group cetypes.Group, accountID string) map[string]string {
	keys := make(map[string]string, len(group.Keys)+1)

	for i, value := range group.Keys {
		label := fmt.Sprintf("key_%d", i)
		if i < len(a.groupings) {
			g := a.groupings[i]
			label = g.label

			// Tag and cost category groups come back as "<key>$<value>", with an
			// empty value for resources that do not carry the key.
			if g.prefix != "" {
				if trimmed, ok := strings.CutPrefix(value, g.prefix); ok {
					value = trimmed
					if value == "" {
						value = "(not set)"
					}
				}
			}
		}

		keys[label] = value
	}

	if a.multiAccount && accountID != "" {
		keys["account_id"] = accountID
	}

	return keys
}

// renderGroupKey builds a stable identity for a group from its labelled values.
func renderGroupKey(keys map[string]string) string {
	parts := make([]string, 0, len(keys))
	for label, value := range keys {
		parts = append(parts, label+"\x1f"+value)
	}
	sort.Strings(parts)

	return strings.Join(parts, "\x1e")
}

// rankGroups orders buckets by the primary metric, descending, with a stable
// tie-break so equal-cost groups do not shuffle between calls.
func rankGroups(buckets map[string]*groupBucket, primaryMetric string) []string {
	keys := make([]string, 0, len(buckets))
	for key := range buckets {
		keys = append(keys, key)
	}

	amount := func(key string) float64 {
		if acc, ok := buckets[key].metrics[primaryMetric]; ok {
			return acc.amount
		}

		return 0
	}

	sort.Slice(keys, func(i, j int) bool {
		ai, aj := amount(keys[i]), amount(keys[j])
		if ai != aj {
			return ai > aj
		}

		return keys[i] < keys[j]
	})

	return keys
}

func renderGroupRow(b *groupBucket) costGroupRow {
	return costGroupRow{Keys: b.keys, Metrics: renderMetrics(b.metrics)}
}

// result assembles the final envelope. limit caps how many groups are reported;
// includePeriods adds the per-period group breakdown, which is the expensive
// part of the payload.
func (a *costAccumulator) result(metrics []string, limit int, includePeriods bool) (map[string]costMetricValue, []costGroupRow, int, int, []costTimeBucket) {
	primary := metrics[0]

	ranked := rankGroups(a.groups, primary)
	kept := ranked
	if limit > 0 && len(kept) > limit {
		kept = kept[:limit]
	}

	keptSet := make(map[string]bool, len(kept))
	rows := make([]costGroupRow, 0, len(kept))
	for _, key := range kept {
		keptSet[key] = true
		rows = append(rows, renderGroupRow(a.groups[key]))
	}

	// With no grouping the API reports a period total; with grouping that field
	// comes back empty, so the total is summed from the groups instead.
	total := map[string]*metricAcc{}
	if len(a.groupings) == 0 {
		for _, window := range a.windows {
			for name, acc := range window.total {
				addAcc(total, name, acc)
			}
		}
	} else {
		for _, b := range a.groups {
			for name, acc := range b.metrics {
				addAcc(total, name, acc)
			}
		}
	}

	slices.Sort(a.order)

	periods := make([]costTimeBucket, 0, len(a.order))
	for _, key := range a.order {
		window := a.windows[key]

		periodTotal := window.total
		if len(a.groupings) > 0 {
			periodTotal = map[string]*metricAcc{}
			for _, b := range window.groups {
				for name, acc := range b.metrics {
					addAcc(periodTotal, name, acc)
				}
			}
		}

		bucket := costTimeBucket{
			Start:     window.start,
			End:       window.end,
			Estimated: window.estimated,
			Total:     renderMetrics(periodTotal),
		}

		if includePeriods && len(a.groupings) > 0 {
			for _, groupKey := range rankGroups(window.groups, primary) {
				if !keptSet[groupKey] {
					continue
				}

				bucket.Groups = append(bucket.Groups, renderGroupRow(window.groups[groupKey]))
			}
		}

		periods = append(periods, bucket)
	}

	return renderMetrics(total), rows, len(ranked), len(ranked) - len(kept), periods
}

func addAcc(dst map[string]*metricAcc, name string, src *metricAcc) {
	acc, ok := dst[name]
	if !ok {
		acc = &metricAcc{}
		dst[name] = acc
	}

	acc.amount += src.amount
	if acc.unit == "" {
		acc.unit = src.unit
	}
}

// buildCostResult merges every account's answer into the response envelope.
func buildCostResult(
	results []accountCostResult,
	window costWindow,
	granularity cetypes.Granularity,
	metrics []string,
	groupings []costGrouping,
	limit int,
	includePeriods bool,
) costResult {
	accounts := make([]string, 0, len(results))
	for _, r := range results {
		accounts = append(accounts, r.accountID)
	}

	acc := newCostAccumulator(groupings, len(results) > 1)
	for _, r := range results {
		acc.add(r.accountID, r.data)
	}

	total, groups, groupCount, truncated, periods := acc.result(metrics, limit, includePeriods)

	res := costResult{
		Start:       window.startString(),
		End:         window.endString(),
		Granularity: string(granularity),
		Metrics:     metrics,
		GroupBy:     groupLabels(groupings),
		Accounts:    accounts,
		Total:       total,
		Groups:      groups,
		GroupCount:  groupCount,
		Truncated:   truncated,
		Periods:     periods,
	}

	if len(results) > 1 {
		res.Note = multiAccountNote
	}

	return res
}

// -----------------------------------------------------------------------------
// Forecast result shaping
// -----------------------------------------------------------------------------

// forecastBucket is one forecast period, with the prediction interval when one
// was requested.
type forecastBucket struct {
	Start string  `json:"start"`
	End   string  `json:"end"`
	Mean  float64 `json:"mean"`
	Lower float64 `json:"lower,omitempty"`
	Upper float64 `json:"upper,omitempty"`
}

// forecastAccountTotal breaks a merged forecast back down per account.
type forecastAccountTotal struct {
	AccountID string  `json:"account_id"`
	Amount    float64 `json:"amount"`
}

// forecastResult is the envelope returned by get_cost_forecast.
type forecastResult struct {
	Start       string   `json:"start"`
	End         string   `json:"end"`
	Granularity string   `json:"granularity"`
	Metric      string   `json:"metric"`
	Accounts    []string `json:"accounts"`

	Total                   costMetricValue        `json:"total"`
	PredictionIntervalLevel int32                  `json:"prediction_interval_level,omitempty"`
	ByAccount               []forecastAccountTotal `json:"by_account,omitempty"`
	Periods                 []forecastBucket       `json:"periods,omitempty"`
	Note                    string                 `json:"note,omitempty"`
}

type accountForecast struct {
	accountID string
	data      *awsce.GetCostForecastOutput
}

func parseAmount(raw *string) float64 {
	amount, err := strconv.ParseFloat(aws.ToString(raw), 64)
	if err != nil {
		return 0
	}

	return amount
}

// buildForecastResult merges the per-account forecasts, summing the mean and the
// interval bounds period by period.
func buildForecastResult(
	forecasts []accountForecast,
	window costWindow,
	granularity cetypes.Granularity,
	metric cetypes.Metric,
	intervalLevel int32,
) forecastResult {
	type periodAcc struct {
		start, end         string
		mean, lower, upper float64
	}

	accounts := make([]string, 0, len(forecasts))
	byAccount := make([]forecastAccountTotal, 0, len(forecasts))

	periods := map[string]*periodAcc{}
	var order []string

	var total costMetricValue

	for _, f := range forecasts {
		accounts = append(accounts, f.accountID)
		if f.data == nil {
			continue
		}

		if f.data.Total != nil {
			amount := parseAmount(f.data.Total.Amount)
			total.Amount += amount

			if total.Unit == "" {
				total.Unit = aws.ToString(f.data.Total.Unit)
			}

			byAccount = append(byAccount, forecastAccountTotal{AccountID: f.accountID, Amount: roundAmount(amount)})
		}

		for _, r := range f.data.ForecastResultsByTime {
			start, end := aws.ToString(r.TimePeriod.Start), aws.ToString(r.TimePeriod.End)
			key := start + "|" + end

			p, ok := periods[key]
			if !ok {
				p = &periodAcc{start: start, end: end}
				periods[key] = p
				order = append(order, key)
			}

			p.mean += parseAmount(r.MeanValue)
			p.lower += parseAmount(r.PredictionIntervalLowerBound)
			p.upper += parseAmount(r.PredictionIntervalUpperBound)
		}
	}

	total.Amount = roundAmount(total.Amount)
	sort.Strings(order)

	buckets := make([]forecastBucket, 0, len(order))
	for _, key := range order {
		p := periods[key]
		buckets = append(buckets, forecastBucket{
			Start: p.start,
			End:   p.end,
			Mean:  roundAmount(p.mean),
			Lower: roundAmount(p.lower),
			Upper: roundAmount(p.upper),
		})
	}

	res := forecastResult{
		Start:                   window.startString(),
		End:                     window.endString(),
		Granularity:             string(granularity),
		Metric:                  string(metric),
		Accounts:                accounts,
		Total:                   total,
		PredictionIntervalLevel: intervalLevel,
		Periods:                 buckets,
	}

	if len(forecasts) > 1 {
		res.ByAccount = byAccount
		res.Note = multiAccountNote
	}

	return res
}

// -----------------------------------------------------------------------------
// Dimension values
// -----------------------------------------------------------------------------

// dimensionValue is one value a dimension takes, with the attributes the API
// annotates it with (for LINKED_ACCOUNT, the account name).
type dimensionValue struct {
	Value      string            `json:"value"`
	Attributes map[string]string `json:"attributes,omitempty"`
}

// dimensionValuesResult is the envelope returned by list_cost_dimension_values.
type dimensionValuesResult struct {
	Dimension string           `json:"dimension"`
	Context   string           `json:"context"`
	Start     string           `json:"start"`
	End       string           `json:"end"`
	Accounts  []string         `json:"accounts"`
	Count     int              `json:"count"`
	Total     int              `json:"total"`
	Values    []dimensionValue `json:"values"`
}

// parseCostContext resolves the context argument, which decides which set of
// dimensions GetDimensionValues will answer for.
func parseCostContext(raw string) (cetypes.Context, error) {
	name := strings.ToUpper(strings.TrimSpace(raw))
	name = strings.ReplaceAll(name, "-", "_")
	name = strings.ReplaceAll(name, " ", "_")

	switch name {
	case "":
		return cetypes.ContextCostAndUsage, nil
	case string(cetypes.ContextCostAndUsage):
		return cetypes.ContextCostAndUsage, nil
	case string(cetypes.ContextReservations):
		return cetypes.ContextReservations, nil
	case string(cetypes.ContextSavingsPlans):
		return cetypes.ContextSavingsPlans, nil
	default:
		return "", errors.Errorf("unknown context %q; use COST_AND_USAGE, RESERVATIONS or SAVINGS_PLANS", raw)
	}
}

// mergeDimensionValues de-duplicates the values gathered from every account and
// sorts them, so the list is stable regardless of which account answered first.
func mergeDimensionValues(values []cetypes.DimensionValuesWithAttributes, limit int) ([]dimensionValue, int) {
	seen := map[string]int{}
	merged := make([]dimensionValue, 0, len(values))

	for _, v := range values {
		value := aws.ToString(v.Value)
		if idx, ok := seen[value]; ok {
			for k, attr := range v.Attributes {
				if _, exists := merged[idx].Attributes[k]; !exists {
					if merged[idx].Attributes == nil {
						merged[idx].Attributes = map[string]string{}
					}
					merged[idx].Attributes[k] = attr
				}
			}

			continue
		}

		seen[value] = len(merged)
		merged = append(merged, dimensionValue{Value: value, Attributes: v.Attributes})
	}

	sort.Slice(merged, func(i, j int) bool { return merged[i].Value < merged[j].Value })

	total := len(merged)
	if limit > 0 && len(merged) > limit {
		merged = merged[:limit]
	}

	return merged, total
}
