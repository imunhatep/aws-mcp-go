package mcpserver

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	cetypes "github.com/aws/aws-sdk-go-v2/service/costexplorer/types"

	"github.com/imunhatep/aws-mcp-go/pkg/errors"
)

// Cost Anomaly Detection findings come from the same Cost Explorer API as the
// cost tools, but answer a different question: not "what did we spend" but
// "where did spend depart from what the model expected, and why". Each anomaly
// carries every root cause — service, region, usage type, linked account and its
// dollar contribution — where the alert email shows only the top few, and that
// is the reason to read them here.

const (
	defaultAnomalyLimit = 50
	maxAnomalyLimit     = 500

	// anomalyRetentionDays is how far back GetAnomalies answers. A window that
	// starts earlier is not an error, but it is not complete either, and the
	// response says so.
	anomalyRetentionDays = 90

	// topRootCauseServices caps the per-service breakdown in the summary.
	topRootCauseServices = 10
)

// -----------------------------------------------------------------------------
// Time window
// -----------------------------------------------------------------------------

// anomalyWindow is a resolved anomaly query window. Unlike costWindow, end is
// inclusive — the API matches each anomaly's end date against it — and a zero end
// leaves the window open up to today.
type anomalyWindow struct {
	start time.Time
	end   time.Time
}

func (w anomalyWindow) startString() string { return w.start.Format(costDateLayout) }

func (w anomalyWindow) endString() string {
	if w.end.IsZero() {
		return ""
	}

	return w.end.Format(costDateLayout)
}

// resolveAnomalyWindow turns the period / start / end arguments into a window.
//
// Explicit dates win; end is optional and inclusive. Otherwise the named period
// is resolved with the cost tools' vocabulary, defaulting to last_30_days, with
// one deliberate difference: a window that reaches today is left open rather
// than closed at yesterday. For spend, "last 7 days" excludes today because
// today's costs are partial; for anomalies, excluding today hides exactly the
// ones still running, which are the ones being asked about.
func resolveAnomalyWindow(period, start, end string, now time.Time) (anomalyWindow, error) {
	period, start, end = strings.TrimSpace(period), strings.TrimSpace(start), strings.TrimSpace(end)

	if end != "" && start == "" {
		return anomalyWindow{}, errors.New("end needs a start; pass start alone for an open-ended window, or a period")
	}

	if start != "" {
		s, err := parseCostDate(start)
		if err != nil {
			return anomalyWindow{}, err
		}

		w := anomalyWindow{start: truncateDay(s)}

		if end != "" {
			e, err := parseCostDate(end)
			if err != nil {
				return anomalyWindow{}, err
			}

			if truncateDay(e).Before(w.start) {
				return anomalyWindow{}, errors.Errorf("end (%s) must not be before start (%s); end is inclusive", end, start)
			}

			w.end = truncateDay(e)
		}

		return w, nil
	}

	if period == "" {
		period = "last_30_days"
	}

	now = now.UTC()
	today := truncateDay(now)

	if m := relativePeriodPattern.FindStringSubmatch(normalizePeriodName(period)); m != nil && m[1] == "last" && strings.HasPrefix(m[3], "day") {
		n, err := strconv.Atoi(m[2])
		if err == nil && n > 0 {
			return anomalyWindow{start: today.AddDate(0, 0, -n)}, nil
		}
	}

	w, ok := namedCostWindow(period, now)
	if !ok {
		return anomalyWindow{}, errors.Errorf("unknown period %q; use today, yesterday, last_<n>_days, this_month, last_month, last_<n>_months, or pass start (and optionally end)", period)
	}

	if w.end.After(today) {
		return anomalyWindow{start: w.start}, nil
	}

	return anomalyWindow{start: w.start, end: w.end.AddDate(0, 0, -1)}, nil
}

func truncateDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// retentionWarning reports a window that reaches past what the API keeps, so a
// short answer for a long window is not read as a quiet quarter.
func (w anomalyWindow) retentionWarning(now time.Time) string {
	horizon := truncateDay(now).AddDate(0, 0, -anomalyRetentionDays)
	if !w.start.Before(horizon) {
		return ""
	}

	return "Cost Anomaly Detection keeps " + strconv.Itoa(anomalyRetentionDays) + " days of history; anomalies that ended before " + horizon.Format(costDateLayout) + " are not returned"
}

// -----------------------------------------------------------------------------
// Arguments
// -----------------------------------------------------------------------------

// anomalyFeedbackFilter is the resolved feedback argument. The API filters on a
// given feedback value; "none" — never reviewed, the triage question — has no API
// form and is applied to the results instead.
type anomalyFeedbackFilter struct {
	api        cetypes.AnomalyFeedbackType
	unreviewed bool
}

func (f anomalyFeedbackFilter) matches(a cetypes.Anomaly) bool {
	if f.unreviewed {
		return a.Feedback == ""
	}

	return true
}

// parseAnomalyFeedback accepts the API values in any casing and separator style,
// plus "planned" for PLANNED_ACTIVITY and "none" / "unreviewed" for anomalies
// nobody has given feedback on.
func parseAnomalyFeedback(raw string) (anomalyFeedbackFilter, error) {
	name := strings.ToUpper(strings.TrimSpace(raw))
	name = strings.ReplaceAll(name, "-", "_")
	name = strings.ReplaceAll(name, " ", "_")

	switch name {
	case "":
		return anomalyFeedbackFilter{}, nil
	case "NONE", "UNREVIEWED", "NO_FEEDBACK":
		return anomalyFeedbackFilter{unreviewed: true}, nil
	case "PLANNED":
		return anomalyFeedbackFilter{api: cetypes.AnomalyFeedbackTypePlannedActivity}, nil
	}

	for _, v := range cetypes.AnomalyFeedbackType("").Values() {
		if string(v) == name {
			return anomalyFeedbackFilter{api: v}, nil
		}
	}

	return anomalyFeedbackFilter{}, errors.Errorf("unknown feedback %q; use YES, NO, PLANNED_ACTIVITY or none (not yet reviewed)", raw)
}

// parseMonitorArn does a shape check only, so a typo costs no API call.
func parseMonitorArn(raw string) (string, error) {
	arn := strings.TrimSpace(raw)
	if arn == "" {
		return "", nil
	}

	if !strings.HasPrefix(arn, "arn:") || !strings.Contains(arn, ":anomalymonitor/") {
		return "", errors.Errorf("monitor_arn %q is not an anomaly monitor ARN (arn:aws:ce::<account>:anomalymonitor/<id>); call list_cost_anomaly_monitors for them", raw)
	}

	return arn, nil
}

// anomalySearch is a case-insensitive substring match over what an anomaly is
// about: the monitored dimension value and every root cause field.
type anomalySearch string

func (s anomalySearch) matches(a cetypes.Anomaly) bool {
	needle := strings.ToLower(strings.TrimSpace(string(s)))
	if needle == "" {
		return true
	}

	contains := func(v *string) bool { return strings.Contains(strings.ToLower(aws.ToString(v)), needle) }

	if contains(a.DimensionValue) {
		return true
	}

	for _, rc := range a.RootCauses {
		if contains(rc.Service) || contains(rc.Region) || contains(rc.UsageType) || contains(rc.LinkedAccount) || contains(rc.LinkedAccountName) {
			return true
		}
	}

	return false
}

// -----------------------------------------------------------------------------
// Anomaly rows
// -----------------------------------------------------------------------------

// anomalyImpact is the spend departure, in USD.
type anomalyImpact struct {
	Total           float64  `json:"total"`
	Max             float64  `json:"max_daily"`
	ActualSpend     *float64 `json:"actual_spend,omitempty"`
	ExpectedSpend   *float64 `json:"expected_spend,omitempty"`
	TotalPercentage *float64 `json:"total_percentage,omitempty"`
}

type anomalyScore struct {
	Current float64 `json:"current"`
	Max     float64 `json:"max"`
}

type anomalyRootCause struct {
	Service           string  `json:"service,omitempty"`
	Region            string  `json:"region,omitempty"`
	UsageType         string  `json:"usage_type,omitempty"`
	LinkedAccount     string  `json:"linked_account,omitempty"`
	LinkedAccountName string  `json:"linked_account_name,omitempty"`
	Contribution      float64 `json:"contribution"`
}

// anomalyDTO is one anomaly as the tool returns it. AccountID is the account
// whose Cost Explorer reported it — the monitor's owner, which for a management
// account's monitor is not the account the spend happened in; that is the root
// causes' linked_account.
type anomalyDTO struct {
	AnomalyID      string             `json:"anomaly_id"`
	AccountID      string             `json:"account_id"`
	MonitorArn     string             `json:"monitor_arn"`
	DimensionValue string             `json:"dimension_value,omitempty"`
	StartDate      string             `json:"start_date,omitempty"`
	EndDate        string             `json:"end_date,omitempty"`
	Feedback       string             `json:"feedback,omitempty"`
	Impact         anomalyImpact      `json:"impact"`
	Score          *anomalyScore      `json:"score,omitempty"`
	RootCauses     []anomalyRootCause `json:"root_causes"`
}

// anomalyDate keeps the day of an API date; anomaly dates arrive as either
// YYYY-MM-DD or a full timestamp depending on the field and API version.
func anomalyDate(raw *string) string {
	v := aws.ToString(raw)
	if len(v) > len(costDateLayout) && v[len(costDateLayout)] == 'T' {
		return v[:len(costDateLayout)]
	}

	return v
}

func roundPtr(v *float64) *float64 {
	if v == nil {
		return nil
	}

	r := roundAmount(*v)

	return &r
}

func buildAnomalyDTO(accountID string, a cetypes.Anomaly) anomalyDTO {
	dto := anomalyDTO{
		AnomalyID:      aws.ToString(a.AnomalyId),
		AccountID:      accountID,
		MonitorArn:     aws.ToString(a.MonitorArn),
		DimensionValue: aws.ToString(a.DimensionValue),
		StartDate:      anomalyDate(a.AnomalyStartDate),
		EndDate:        anomalyDate(a.AnomalyEndDate),
		Feedback:       string(a.Feedback),
		RootCauses:     make([]anomalyRootCause, 0, len(a.RootCauses)),
	}

	if a.Impact != nil {
		dto.Impact = anomalyImpact{
			Total:           roundAmount(a.Impact.TotalImpact),
			Max:             roundAmount(a.Impact.MaxImpact),
			ActualSpend:     roundPtr(a.Impact.TotalActualSpend),
			ExpectedSpend:   roundPtr(a.Impact.TotalExpectedSpend),
			TotalPercentage: roundPtr(a.Impact.TotalImpactPercentage),
		}
	}

	if a.AnomalyScore != nil {
		dto.Score = &anomalyScore{Current: a.AnomalyScore.CurrentScore, Max: a.AnomalyScore.MaxScore}
	}

	for _, rc := range a.RootCauses {
		row := anomalyRootCause{
			Service:           aws.ToString(rc.Service),
			Region:            aws.ToString(rc.Region),
			UsageType:         aws.ToString(rc.UsageType),
			LinkedAccount:     aws.ToString(rc.LinkedAccount),
			LinkedAccountName: aws.ToString(rc.LinkedAccountName),
		}

		if rc.Impact != nil {
			row.Contribution = roundAmount(rc.Impact.Contribution)
		}

		dto.RootCauses = append(dto.RootCauses, row)
	}

	// Largest contributor first: that is the order an investigation works in.
	sort.SliceStable(dto.RootCauses, func(i, j int) bool {
		return dto.RootCauses[i].Contribution > dto.RootCauses[j].Contribution
	})

	return dto
}

// sortAnomalies ranks by total impact, then most recent end date, then id so
// equal anomalies keep a stable order between calls.
func sortAnomalies(items []anomalyDTO) {
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if a.Impact.Total != b.Impact.Total {
			return a.Impact.Total > b.Impact.Total
		}
		if a.EndDate != b.EndDate {
			return a.EndDate > b.EndDate
		}

		return a.AnomalyID < b.AnomalyID
	})
}

// -----------------------------------------------------------------------------
// Summary
// -----------------------------------------------------------------------------

type anomalyServiceImpact struct {
	Service      string  `json:"service"`
	Contribution float64 `json:"contribution"`
	Anomalies    int     `json:"anomalies"`
}

// anomalySummary covers every matching anomaly, not the returned page, so a
// truncated response still states the real impact.
type anomalySummary struct {
	Anomalies   int            `json:"anomalies"`
	TotalImpact float64        `json:"total_impact"`
	Currency    string         `json:"currency"`
	ByFeedback  map[string]int `json:"by_feedback,omitempty"`
	// TopServices sums root-cause contributions per service. Contributions are
	// the model's attribution and need not add up to the total impact.
	TopServices []anomalyServiceImpact `json:"top_root_cause_services,omitempty"`
}

func summarizeAnomalies(items []anomalyDTO) anomalySummary {
	summary := anomalySummary{Anomalies: len(items), Currency: "USD", ByFeedback: map[string]int{}}

	services := map[string]*anomalyServiceImpact{}

	for _, a := range items {
		summary.TotalImpact += a.Impact.Total

		feedback := a.Feedback
		if feedback == "" {
			feedback = "none"
		}
		summary.ByFeedback[feedback]++

		counted := map[string]bool{}
		for _, rc := range a.RootCauses {
			name := rc.Service
			if name == "" {
				name = "(unattributed)"
			}

			svc, ok := services[name]
			if !ok {
				svc = &anomalyServiceImpact{Service: name}
				services[name] = svc
			}

			svc.Contribution += rc.Contribution
			if !counted[name] {
				counted[name] = true
				svc.Anomalies++
			}
		}
	}

	summary.TotalImpact = roundAmount(summary.TotalImpact)

	if len(summary.ByFeedback) == 0 {
		summary.ByFeedback = nil
	}

	for _, svc := range services {
		svc.Contribution = roundAmount(svc.Contribution)
		summary.TopServices = append(summary.TopServices, *svc)
	}

	sort.Slice(summary.TopServices, func(i, j int) bool {
		a, b := summary.TopServices[i], summary.TopServices[j]
		if a.Contribution != b.Contribution {
			return a.Contribution > b.Contribution
		}

		return a.Service < b.Service
	})

	if len(summary.TopServices) > topRootCauseServices {
		summary.TopServices = summary.TopServices[:topRootCauseServices]
	}

	return summary
}

// -----------------------------------------------------------------------------
// Monitors and subscriptions
// -----------------------------------------------------------------------------

type anomalySubscriber struct {
	Address string `json:"address"`
	Type    string `json:"type"`
	Status  string `json:"status,omitempty"`
}

// anomalySubscriptionDTO is who gets alerted, how often and above what.
type anomalySubscriptionDTO struct {
	SubscriptionArn     string              `json:"subscription_arn"`
	Name                string              `json:"name"`
	Frequency           string              `json:"frequency"`
	MonitorArns         []string            `json:"monitor_arns"`
	Threshold           *float64            `json:"threshold,omitempty"`
	ThresholdExpression any                 `json:"threshold_expression,omitempty"`
	Subscribers         []anomalySubscriber `json:"subscribers"`
}

// anomalyMonitorDTO is what a monitor watches and how it slices spend, with the
// subscriptions that alert on it.
type anomalyMonitorDTO struct {
	MonitorArn            string                   `json:"monitor_arn"`
	AccountID             string                   `json:"account_id"`
	Name                  string                   `json:"name"`
	Type                  string                   `json:"type"`
	Dimension             string                   `json:"dimension,omitempty"`
	Specification         any                      `json:"specification,omitempty"`
	DimensionalValueCount int32                    `json:"dimensional_value_count,omitempty"`
	CreationDate          string                   `json:"creation_date,omitempty"`
	LastEvaluatedDate     string                   `json:"last_evaluated_date,omitempty"`
	LastUpdatedDate       string                   `json:"last_updated_date,omitempty"`
	Subscriptions         []anomalySubscriptionDTO `json:"subscriptions"`
}

func buildMonitorDTO(accountID string, m cetypes.AnomalyMonitor) anomalyMonitorDTO {
	return anomalyMonitorDTO{
		MonitorArn:            aws.ToString(m.MonitorArn),
		AccountID:             accountID,
		Name:                  aws.ToString(m.MonitorName),
		Type:                  string(m.MonitorType),
		Dimension:             string(m.MonitorDimension),
		Specification:         compactExpression(m.MonitorSpecification),
		DimensionalValueCount: m.DimensionalValueCount,
		CreationDate:          aws.ToString(m.CreationDate),
		LastEvaluatedDate:     aws.ToString(m.LastEvaluatedDate),
		LastUpdatedDate:       aws.ToString(m.LastUpdatedDate),
		Subscriptions:         []anomalySubscriptionDTO{},
	}
}

func buildSubscriptionDTO(s cetypes.AnomalySubscription) anomalySubscriptionDTO {
	// Threshold is deprecated upstream in favour of ThresholdExpression, but
	// subscriptions created before the change still carry only the old field, so
	// both are reported.
	dto := anomalySubscriptionDTO{
		SubscriptionArn:     aws.ToString(s.SubscriptionArn),
		Name:                aws.ToString(s.SubscriptionName),
		Frequency:           string(s.Frequency),
		MonitorArns:         s.MonitorArnList,
		Threshold:           s.Threshold,
		ThresholdExpression: compactExpression(s.ThresholdExpression),
		Subscribers:         make([]anomalySubscriber, 0, len(s.Subscribers)),
	}

	if dto.MonitorArns == nil {
		dto.MonitorArns = []string{}
	}

	for _, sub := range s.Subscribers {
		dto.Subscribers = append(dto.Subscribers, anomalySubscriber{
			Address: aws.ToString(sub.Address),
			Type:    string(sub.Type),
			Status:  string(sub.Status),
		})
	}

	return dto
}

// attachSubscriptions files each subscription under every monitor it alerts on,
// and returns the ones whose monitors were not among those listed — kept rather
// than dropped, since an alert nobody can trace to a monitor is worth seeing.
func attachSubscriptions(monitors []anomalyMonitorDTO, subscriptions []anomalySubscriptionDTO) []anomalySubscriptionDTO {
	byArn := make(map[string]int, len(monitors))
	for i, m := range monitors {
		byArn[m.MonitorArn] = i
	}

	unattached := []anomalySubscriptionDTO{}

	for _, sub := range subscriptions {
		attached := false

		for _, arn := range sub.MonitorArns {
			if i, ok := byArn[arn]; ok {
				monitors[i].Subscriptions = append(monitors[i].Subscriptions, sub)
				attached = true
			}
		}

		if !attached {
			unattached = append(unattached, sub)
		}
	}

	return unattached
}

// compactExpression renders a Cost Explorer Expression without the dozen null
// branches the SDK struct marshals to, leaving only the parts that are set.
func compactExpression(e *cetypes.Expression) any {
	if e == nil {
		return nil
	}

	raw, err := json.Marshal(e)
	if err != nil {
		return nil
	}

	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil
	}

	return pruneEmpty(decoded)
}

// pruneEmpty drops nulls, empty strings and empty collections, recursively.
func pruneEmpty(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, child := range t {
			if pruned := pruneEmpty(child); pruned != nil {
				out[k] = pruned
			}
		}
		if len(out) == 0 {
			return nil
		}

		return out
	case []any:
		out := make([]any, 0, len(t))
		for _, child := range t {
			if pruned := pruneEmpty(child); pruned != nil {
				out = append(out, pruned)
			}
		}
		if len(out) == 0 {
			return nil
		}

		return out
	case string:
		if t == "" {
			return nil
		}

		return t
	default:
		return t
	}
}
