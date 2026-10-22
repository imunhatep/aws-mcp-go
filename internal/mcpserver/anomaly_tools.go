package mcpserver

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsce "github.com/aws/aws-sdk-go-v2/service/costexplorer"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/rs/zerolog/log"

	"github.com/imunhatep/awslib/service/costexplorer"

	"github.com/imunhatep/aws-mcp-go/pkg/errors"
)

// registerAnomalyTools declares the Cost Anomaly Detection tools. They read
// through the Cost Explorer repository like the cost tools do, but uncached: an
// anomaly that is still running keeps moving its end date and impact, so the
// default 6h window would answer "what was anomalous this morning".
func (s *Server) registerAnomalyTools() {
	s.addReadOnlyTool(
		mcp.NewTool(
			"list_cost_anomalies",
			mcp.WithDescription("List the spend anomalies AWS Cost Anomaly Detection found, each with its total and peak impact in USD, actual versus expected spend, and ALL of its root causes (service, region, usage type, linked account, dollar contribution) — the alert email shows only the top few. Answers 'what caused this cost anomaly', 'which anomalies hit us this month' and 'what has nobody triaged'. The window matches each anomaly's END date: an anomaly that began before the window but is still running is included. History goes back 90 days. Returns {items, count, total, summary, window, accounts_queried}, items ranked by total impact; the summary totals impact across every match, not just the returned page. Read live, not cached. Then use get_cost_and_usage filtered on a root cause's SERVICE / USAGE_TYPE / REGION for the daily trend, and list_resources to find what is behind it."),
			mcp.WithString("period",
				mcp.Description("Named window: today, yesterday, last_<n>_days (default last_30_days, runs up to and including today), this_month, last_month, last_<n>_months. Ignored when start is given."),
			),
			mcp.WithString("start",
				mcp.Description("Explicit first day of the window, YYYY-MM-DD. Overrides period."),
			),
			mcp.WithString("end",
				mcp.Description("Explicit LAST day of the window, YYYY-MM-DD, inclusive (unlike get_cost_and_usage, whose end is exclusive). Omit for an open window up to today. Needs start."),
			),
			mcp.WithNumber("min_impact",
				mcp.Description("Only anomalies whose total impact (actual minus expected spend, USD) is at least this much. Applied by the API."),
			),
			mcp.WithString("feedback",
				mcp.Description("Only anomalies with this feedback: YES (a real anomaly), NO (false positive), PLANNED_ACTIVITY, or 'none' for anomalies nobody has reviewed yet."),
			),
			mcp.WithString("monitor_arn",
				mcp.Description("Only anomalies found by this monitor. list_cost_anomaly_monitors lists the ARNs."),
			),
			mcp.WithString("search",
				mcp.Description("Only anomalies whose monitored value or any root cause (service, region, usage type, linked account id or name) contains this text, case-insensitive. Service names are Cost Explorer's, e.g. 'Amazon Simple Storage Service', 'EC2 - Other'."),
			),
			mcp.WithString("account_id",
				mcp.Description("Query only this account's Cost Anomaly Detection. Scopes the query rather than filtering its output: only this account is called. By default every reachable account is queried; each row's account_id is the account that owns the monitor, and the root causes' linked_account is where the spend happened."),
			),
			mcp.WithNumber("limit",
				mcp.Description("Maximum anomalies to return, ranked by total impact (default 50, max 500). total and truncated report what was dropped."),
			),
		),
		s.handleListCostAnomalies,
	)

	s.addReadOnlyTool(
		mcp.NewTool(
			"list_cost_anomaly_monitors",
			mcp.WithDescription("List the Cost Anomaly Detection monitors — what each watches (AWS services, linked accounts, a cost category or tag) and when it last evaluated — with the alert subscriptions on each: who is notified, how often and above which threshold. Answers 'is this spend monitored at all', 'who got this alert' and 'why did nobody get alerted'. Returns {monitors, unattached_subscriptions, accounts_queried}. Read live, not cached."),
			mcp.WithString("account_id",
				mcp.Description("Query only this account. By default every reachable account is queried and each monitor carries its account_id."),
			),
		),
		s.handleListCostAnomalyMonitors,
	)
}

// anomalyWindowEcho is the effective window. End is absent for an open window.
type anomalyWindowEcho struct {
	Start string `json:"start"`
	End   string `json:"end,omitempty"`
	// Matches says which date the window is compared against, because it is not
	// the obvious one.
	Matches string `json:"matches"`
}

// costAnomaliesResult is the list_cost_anomalies envelope. AccountsQueried and
// Warnings are here for the reason they are everywhere else: "no anomalies" and
// "we could not ask" must not look the same.
type costAnomaliesResult struct {
	Items           []anomalyDTO      `json:"items"`
	Count           int               `json:"count"`
	Total           int               `json:"total"`
	Truncated       bool              `json:"truncated,omitempty"`
	Summary         anomalySummary    `json:"summary"`
	Window          anomalyWindowEcho `json:"window"`
	AccountsQueried []string          `json:"accounts_queried"`
	Warnings        []string          `json:"warnings,omitempty"`
	Note            string            `json:"note,omitempty"`
}

func clampAnomalyLimit(limit int) int {
	if limit <= 0 {
		return defaultAnomalyLimit
	}

	return min(limit, maxAnomalyLimit)
}

func (s *Server) handleListCostAnomalies(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	now := time.Now()

	window, err := resolveAnomalyWindow(req.GetString("period", ""), req.GetString("start", ""), req.GetString("end", ""), now)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("invalid time period", err), nil
	}

	feedback, err := parseAnomalyFeedback(req.GetString("feedback", ""))
	if err != nil {
		return mcp.NewToolResultErrorFromErr("invalid feedback", err), nil
	}

	monitorArn, err := parseMonitorArn(req.GetString("monitor_arn", ""))
	if err != nil {
		return mcp.NewToolResultErrorFromErr("invalid monitor_arn", err), nil
	}

	minImpact := req.GetFloat("min_impact", 0)
	if minImpact < 0 {
		return mcp.NewToolResultErrorf("min_impact must not be negative, got %v", minImpact), nil
	}

	search := anomalySearch(req.GetString("search", ""))
	limit := clampAnomalyLimit(req.GetInt("limit", 0))

	query := costexplorer.AnomalyQuery{
		Start:          window.start,
		End:            window.end,
		MonitorArn:     monitorArn,
		Feedback:       feedback.api,
		MinTotalImpact: minImpact,
	}

	// Validate before touching AWS, as the cost tools do: a query that cannot
	// succeed should not be sent once per account.
	if err := query.Validate(); err != nil {
		return mcp.NewToolResultErrorFromErr("invalid anomaly query", err), nil
	}

	repos, err := s.costRepositories(ctx, req.GetString("account_id", ""), false)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("failed to initialise aws client", err), nil
	}

	log.Info().
		Str("start", window.startString()).
		Str("end", window.endString()).
		Str("monitor", monitorArn).
		Float64("min_impact", minImpact).
		Strs("accounts", accountIDs(repos)).
		Msg("[mcpserver.handleListCostAnomalies] listing cost anomalies")

	items, warnings, err := collectAnomalies(repos, query, feedback, search)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("failed to list cost anomalies", err), nil
	}

	if w := window.retentionWarning(now); w != "" {
		warnings = append(warnings, w)
	}

	sortAnomalies(items)

	result := costAnomaliesResult{
		Total:   len(items),
		Summary: summarizeAnomalies(items),
		Window: anomalyWindowEcho{
			Start:   window.startString(),
			End:     window.endString(),
			Matches: "anomaly end date",
		},
		AccountsQueried: accountIDs(repos),
		Warnings:        warnings,
	}

	if len(items) > limit {
		items = items[:limit]
		result.Truncated = true
	}

	result.Items = items
	result.Count = len(items)

	switch {
	case len(repos) > 1:
		result.Note = fmt.Sprintf("anomalies from %d accounts are merged; if the pool holds a management account and its members, both may have monitors covering the same spend — check each row's account_id and its root causes' linked_account before adding impacts up", len(repos))
	case len(items) == 0:
		result.Note = "no anomalies matched; if this is unexpected, list_cost_anomaly_monitors shows whether any monitor covers this spend"
	}

	return jsonResult(result)
}

// collectAnomalies queries every repository and keeps the anomalies that pass the
// client-side filters. One account failing does not fail the others — it becomes
// a warning — but if no account answers it is an error: "nobody could be asked"
// must not render as "nothing was anomalous".
func collectAnomalies(
	repos []accountCostRepository,
	query costexplorer.AnomalyQuery,
	feedback anomalyFeedbackFilter,
	search anomalySearch,
) ([]anomalyDTO, []string, error) {
	items := []anomalyDTO{}
	warnings := []string{}
	seen := map[string]bool{}
	answered := 0

	for _, r := range repos {
		anomalies, err := r.repo.GetAnomaliesByQuery(query)
		if err != nil {
			log.Warn().Err(err).Str("account", r.accountID).Msg("[mcpserver.collectAnomalies] get anomalies failed")
			warnings = append(warnings, "account "+r.accountID+": "+err.Error())

			continue
		}

		answered++

		for _, a := range anomalies {
			if !feedback.matches(a) || !search.matches(a) {
				continue
			}

			// An anomaly id belongs to one monitor, which belongs to one account,
			// so this only guards against the same account answering twice.
			id := aws.ToString(a.AnomalyId)
			if id != "" && seen[id] {
				continue
			}
			seen[id] = true

			items = append(items, buildAnomalyDTO(r.accountID, a))
		}
	}

	if answered == 0 && len(repos) > 0 {
		return nil, warnings, errors.Errorf("no account could be queried: %s", strings.Join(warnings, " | "))
	}

	return items, warnings, nil
}

// costAnomalyMonitorsResult is the list_cost_anomaly_monitors envelope.
type costAnomalyMonitorsResult struct {
	Monitors                []anomalyMonitorDTO      `json:"monitors"`
	Count                   int                      `json:"count"`
	UnattachedSubscriptions []anomalySubscriptionDTO `json:"unattached_subscriptions,omitempty"`
	AccountsQueried         []string                 `json:"accounts_queried"`
	Warnings                []string                 `json:"warnings,omitempty"`
	Note                    string                   `json:"note,omitempty"`
}

func (s *Server) handleListCostAnomalyMonitors(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	repos, err := s.costRepositories(ctx, req.GetString("account_id", ""), false)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("failed to initialise aws client", err), nil
	}

	log.Info().
		Strs("accounts", accountIDs(repos)).
		Msg("[mcpserver.handleListCostAnomalyMonitors] listing anomaly monitors")

	monitors := []anomalyMonitorDTO{}
	unattached := []anomalySubscriptionDTO{}
	warnings := []string{}
	answered := 0

	for _, r := range repos {
		found, err := r.repo.GetAnomalyMonitors(&awsce.GetAnomalyMonitorsInput{})
		if err != nil {
			log.Warn().Err(err).Str("account", r.accountID).Msg("[mcpserver.handleListCostAnomalyMonitors] get anomaly monitors failed")
			warnings = append(warnings, "account "+r.accountID+": "+err.Error())

			continue
		}

		answered++

		accountMonitors := make([]anomalyMonitorDTO, 0, len(found))
		for _, m := range found {
			accountMonitors = append(accountMonitors, buildMonitorDTO(r.accountID, m))
		}

		// Subscriptions are secondary: without them the monitors still answer
		// "is this watched", so a failure here is a warning, not a lost account.
		subs, err := r.repo.GetAnomalySubscriptions(&awsce.GetAnomalySubscriptionsInput{})
		if err != nil {
			log.Warn().Err(err).Str("account", r.accountID).Msg("[mcpserver.handleListCostAnomalyMonitors] get anomaly subscriptions failed")
			warnings = append(warnings, "account "+r.accountID+": subscriptions could not be read, so monitors show none: "+err.Error())
		} else {
			dtos := make([]anomalySubscriptionDTO, 0, len(subs))
			for _, sub := range subs {
				dtos = append(dtos, buildSubscriptionDTO(sub))
			}

			unattached = append(unattached, attachSubscriptions(accountMonitors, dtos)...)
		}

		monitors = append(monitors, accountMonitors...)
	}

	if answered == 0 {
		return mcp.NewToolResultErrorf("no account could be queried: %s", strings.Join(warnings, " | ")), nil
	}

	sort.SliceStable(monitors, func(i, j int) bool {
		if monitors[i].AccountID != monitors[j].AccountID {
			return monitors[i].AccountID < monitors[j].AccountID
		}

		return monitors[i].Name < monitors[j].Name
	})

	result := costAnomalyMonitorsResult{
		Monitors:                monitors,
		Count:                   len(monitors),
		UnattachedSubscriptions: unattached,
		AccountsQueried:         accountIDs(repos),
		Warnings:                warnings,
	}

	if len(monitors) == 0 {
		result.Note = "no anomaly monitors are configured in the queried accounts, so Cost Anomaly Detection is not watching their spend; list_cost_anomalies will return nothing for them"
	}

	return jsonResult(result)
}
