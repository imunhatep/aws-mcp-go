package mcpserver

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsce "github.com/aws/aws-sdk-go-v2/service/costexplorer"
	cetypes "github.com/aws/aws-sdk-go-v2/service/costexplorer/types"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ptypes "github.com/imunhatep/awslib/provider/types"
	"github.com/imunhatep/awslib/service/costexplorer"

	"github.com/imunhatep/aws-mcp-go/pkg/errors"
)

// The clock is the shared `now` from cost_test.go: 2026-08-12.

func TestResolveAnomalyWindow_NamedPeriods(t *testing.T) {
	cases := []struct {
		period     string
		start, end string
	}{
		// Windows that reach today stay open, so a running anomaly is included.
		{"", "2026-07-13", ""},
		{"last_30_days", "2026-07-13", ""},
		{"Last 7 Days", "2026-08-05", ""},
		{"today", "2026-08-12", ""},
		{"this_month", "2026-08-01", ""},
		{"mtd", "2026-08-01", ""},
		// Windows that end before today close on their last day, inclusive.
		{"yesterday", "2026-08-11", "2026-08-11"},
		{"last_month", "2026-07-01", "2026-07-31"},
		{"last_3_months", "2026-05-01", "2026-07-31"},
	}

	for _, tc := range cases {
		w, err := resolveAnomalyWindow(tc.period, "", "", now)
		require.NoErrorf(t, err, "period %q", tc.period)
		assert.Equalf(t, tc.start, w.startString(), "start of %q", tc.period)
		assert.Equalf(t, tc.end, w.endString(), "end of %q", tc.period)
	}
}

func TestResolveAnomalyWindow_ExplicitDates(t *testing.T) {
	w, err := resolveAnomalyWindow("last_month", "2026-08-01", "", now)
	require.NoError(t, err)
	assert.Equal(t, "2026-08-01", w.startString(), "start overrides period")
	assert.Empty(t, w.endString(), "start alone is an open window")

	w, err = resolveAnomalyWindow("", "2026-08-01", "2026-08-01", now)
	require.NoError(t, err)
	assert.Equal(t, "2026-08-01", w.endString(), "a one-day window is valid: end is inclusive")
}

func TestResolveAnomalyWindow_Errors(t *testing.T) {
	for name, args := range map[string][3]string{
		"end without start": {"", "", "2026-08-01"},
		"end before start":  {"", "2026-08-02", "2026-08-01"},
		"bad date":          {"", "01/08/2026", ""},
		"unknown period":    {"since_forever", "", ""},
		"future period":     {"next_month", "", ""},
	} {
		_, err := resolveAnomalyWindow(args[0], args[1], args[2], now)
		assert.Errorf(t, err, "%s should be rejected", name)
	}
}

func TestAnomalyRetentionWarning(t *testing.T) {
	w, err := resolveAnomalyWindow("last_30_days", "", "", now)
	require.NoError(t, err)
	assert.Empty(t, w.retentionWarning(now))

	w, err = resolveAnomalyWindow("last_6_months", "", "", now)
	require.NoError(t, err)
	assert.Contains(t, w.retentionWarning(now), "90 days")
}

func TestParseAnomalyFeedback(t *testing.T) {
	for raw, want := range map[string]anomalyFeedbackFilter{
		"":                 {},
		"yes":              {api: cetypes.AnomalyFeedbackTypeYes},
		"NO":               {api: cetypes.AnomalyFeedbackTypeNo},
		"planned":          {api: cetypes.AnomalyFeedbackTypePlannedActivity},
		"planned-activity": {api: cetypes.AnomalyFeedbackTypePlannedActivity},
		"none":             {unreviewed: true},
		"unreviewed":       {unreviewed: true},
	} {
		got, err := parseAnomalyFeedback(raw)
		require.NoErrorf(t, err, "feedback %q", raw)
		assert.Equalf(t, want, got, "feedback %q", raw)
	}

	_, err := parseAnomalyFeedback("maybe")
	assert.Error(t, err)
}

func TestAnomalyFeedbackNoneIsAppliedLocally(t *testing.T) {
	f, err := parseAnomalyFeedback("none")
	require.NoError(t, err)

	assert.Empty(t, f.api, "the API has no 'no feedback' filter; it must not be sent")
	assert.True(t, f.matches(cetypes.Anomaly{}))
	assert.False(t, f.matches(cetypes.Anomaly{Feedback: cetypes.AnomalyFeedbackTypeNo}))
}

func TestParseMonitorArn(t *testing.T) {
	arn, err := parseMonitorArn(" arn:aws:ce::111111111111:anomalymonitor/abc-123 ")
	require.NoError(t, err)
	assert.Equal(t, "arn:aws:ce::111111111111:anomalymonitor/abc-123", arn)

	arn, err = parseMonitorArn("")
	require.NoError(t, err)
	assert.Empty(t, arn)

	_, err = parseMonitorArn("abc-123")
	assert.Error(t, err)
}

func sampleAnomaly() cetypes.Anomaly {
	return cetypes.Anomaly{
		AnomalyId:        aws.String("an-1"),
		MonitorArn:       aws.String("arn:aws:ce::111111111111:anomalymonitor/m-1"),
		DimensionValue:   aws.String("Amazon Simple Storage Service"),
		AnomalyStartDate: aws.String("2026-08-05T00:00:00Z"),
		AnomalyEndDate:   aws.String("2026-08-10"),
		Impact: &cetypes.Impact{
			TotalImpact:        412.3456789,
			MaxImpact:          120.5,
			TotalActualSpend:   aws.Float64(900.12345678),
			TotalExpectedSpend: aws.Float64(487.77777777),
		},
		AnomalyScore: &cetypes.AnomalyScore{CurrentScore: 0.4, MaxScore: 0.9},
		RootCauses: []cetypes.RootCause{
			{
				Service:   aws.String("Amazon Simple Storage Service"),
				Region:    aws.String("eu-central-1"),
				UsageType: aws.String("EUC1-Requests-Tier1"),
				Impact:    &cetypes.RootCauseImpact{Contribution: 80},
			},
			{
				Service:           aws.String("Amazon Simple Storage Service"),
				Region:            aws.String("eu-central-1"),
				UsageType:         aws.String("EUC1-TimedStorage-ByteHrs"),
				LinkedAccount:     aws.String("222222222222"),
				LinkedAccountName: aws.String("data-lake"),
				Impact:            &cetypes.RootCauseImpact{Contribution: 300},
			},
		},
	}
}

func TestBuildAnomalyDTO(t *testing.T) {
	dto := buildAnomalyDTO("111111111111", sampleAnomaly())

	assert.Equal(t, "an-1", dto.AnomalyID)
	assert.Equal(t, "111111111111", dto.AccountID)
	assert.Equal(t, "2026-08-05", dto.StartDate, "timestamps are cut to the day")
	assert.Equal(t, "2026-08-10", dto.EndDate)
	assert.Empty(t, dto.Feedback)
	assert.InDelta(t, 412.345679, dto.Impact.Total, 1e-9)
	require.NotNil(t, dto.Impact.ActualSpend)
	assert.InDelta(t, 900.123457, *dto.Impact.ActualSpend, 1e-9)
	require.NotNil(t, dto.Score)

	require.Len(t, dto.RootCauses, 2, "every root cause is kept, not just the top one")
	assert.Equal(t, "EUC1-TimedStorage-ByteHrs", dto.RootCauses[0].UsageType, "largest contribution first")
	assert.Equal(t, "data-lake", dto.RootCauses[0].LinkedAccountName)
}

func TestBuildAnomalyDTOWithoutImpact(t *testing.T) {
	dto := buildAnomalyDTO("111111111111", cetypes.Anomaly{AnomalyId: aws.String("bare")})

	assert.Zero(t, dto.Impact.Total)
	assert.Nil(t, dto.Score)
	assert.NotNil(t, dto.RootCauses, "root_causes renders as [], never null")
}

func TestAnomalySearch(t *testing.T) {
	a := sampleAnomaly()

	for needle, want := range map[string]bool{
		"":                true,
		"simple storage":  true, // dimension value
		"timedstorage":    true, // usage type
		"EU-CENTRAL-1":    true, // region
		"222222222222":    true, // linked account id
		"Data-Lake":       true, // linked account name
		"Amazon DynamoDB": false,
	} {
		assert.Equalf(t, want, anomalySearch(needle).matches(a), "search %q", needle)
	}
}

func TestSortAnomaliesByImpact(t *testing.T) {
	items := []anomalyDTO{
		{AnomalyID: "small", Impact: anomalyImpact{Total: 10}, EndDate: "2026-08-10"},
		{AnomalyID: "big", Impact: anomalyImpact{Total: 500}, EndDate: "2026-08-01"},
		{AnomalyID: "tie-old", Impact: anomalyImpact{Total: 10}, EndDate: "2026-08-01"},
	}

	sortAnomalies(items)

	assert.Equal(t, []string{"big", "small", "tie-old"}, []string{items[0].AnomalyID, items[1].AnomalyID, items[2].AnomalyID})
}

func TestSummarizeAnomalies(t *testing.T) {
	first := buildAnomalyDTO("111111111111", sampleAnomaly())

	second := sampleAnomaly()
	second.AnomalyId = aws.String("an-2")
	second.Feedback = cetypes.AnomalyFeedbackTypeNo
	second.Impact = &cetypes.Impact{TotalImpact: 50}
	second.RootCauses = []cetypes.RootCause{
		{Service: aws.String("Amazon Elastic Compute Cloud - Compute"), Impact: &cetypes.RootCauseImpact{Contribution: 50}},
	}

	summary := summarizeAnomalies([]anomalyDTO{first, buildAnomalyDTO("111111111111", second)})

	assert.Equal(t, 2, summary.Anomalies)
	assert.InDelta(t, 462.345679, summary.TotalImpact, 1e-9)
	assert.Equal(t, "USD", summary.Currency)
	assert.Equal(t, map[string]int{"none": 1, "NO": 1}, summary.ByFeedback)

	require.Len(t, summary.TopServices, 2)
	assert.Equal(t, "Amazon Simple Storage Service", summary.TopServices[0].Service)
	assert.InDelta(t, 380, summary.TopServices[0].Contribution, 1e-9)
	assert.Equal(t, 1, summary.TopServices[0].Anomalies, "two root causes of one anomaly count it once")
}

func TestAttachSubscriptions(t *testing.T) {
	monitors := []anomalyMonitorDTO{
		buildMonitorDTO("111111111111", cetypes.AnomalyMonitor{MonitorArn: aws.String("m-1"), MonitorName: aws.String("services")}),
		buildMonitorDTO("111111111111", cetypes.AnomalyMonitor{MonitorArn: aws.String("m-2"), MonitorName: aws.String("accounts")}),
	}

	subs := []anomalySubscriptionDTO{
		buildSubscriptionDTO(cetypes.AnomalySubscription{
			SubscriptionName: aws.String("both"),
			MonitorArnList:   []string{"m-1", "m-2"},
			Frequency:        cetypes.AnomalySubscriptionFrequencyDaily,
			Subscribers:      []cetypes.Subscriber{{Address: aws.String("finops@example.com"), Type: cetypes.SubscriberTypeEmail}},
		}),
		buildSubscriptionDTO(cetypes.AnomalySubscription{
			SubscriptionName: aws.String("orphan"),
			MonitorArnList:   []string{"m-gone"},
		}),
	}

	unattached := attachSubscriptions(monitors, subs)

	require.Len(t, monitors[0].Subscriptions, 1)
	require.Len(t, monitors[1].Subscriptions, 1)
	assert.Equal(t, "finops@example.com", monitors[0].Subscriptions[0].Subscribers[0].Address)

	require.Len(t, unattached, 1, "a subscription with no listed monitor is reported, not dropped")
	assert.Equal(t, "orphan", unattached[0].Name)
}

func TestCompactExpressionDropsUnsetBranches(t *testing.T) {
	expr := &cetypes.Expression{
		Dimensions: &cetypes.DimensionValues{
			Key:    cetypes.DimensionLinkedAccount,
			Values: []string{"111111111111"},
		},
	}

	raw, err := json.Marshal(compactExpression(expr))
	require.NoError(t, err)
	assert.JSONEq(t, `{"Dimensions":{"Key":"LINKED_ACCOUNT","Values":["111111111111"]}}`, string(raw))

	assert.Nil(t, compactExpression(nil))
}

// fakeAnomalyRepo is a costRepository double for the anomaly calls; the cost
// calls are never reached by these tests.
type fakeAnomalyRepo struct {
	costRepository

	anomalies []cetypes.Anomaly
	err       error
	lastQuery costexplorer.AnomalyQuery
}

func (f *fakeAnomalyRepo) GetAnomaliesByQuery(q costexplorer.AnomalyQuery) ([]cetypes.Anomaly, error) {
	f.lastQuery = q
	return f.anomalies, f.err
}

func (f *fakeAnomalyRepo) GetAnomalyMonitors(_ *awsce.GetAnomalyMonitorsInput) ([]cetypes.AnomalyMonitor, error) {
	return nil, f.err
}

func (f *fakeAnomalyRepo) GetAnomalySubscriptions(_ *awsce.GetAnomalySubscriptionsInput) ([]cetypes.AnomalySubscription, error) {
	return nil, f.err
}

func TestCollectAnomaliesPartialFailureIsAWarning(t *testing.T) {
	repos := []accountCostRepository{
		{accountID: "111111111111", repo: &fakeAnomalyRepo{anomalies: []cetypes.Anomaly{sampleAnomaly()}}},
		{accountID: "222222222222", repo: &fakeAnomalyRepo{err: errors.New("AccessDeniedException")}},
	}

	items, warnings, err := collectAnomalies(repos, costexplorer.AnomalyQuery{}, anomalyFeedbackFilter{}, "")
	require.NoError(t, err)
	assert.Len(t, items, 1)
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], "222222222222")
}

func TestCollectAnomaliesErrorsWhenNoAccountAnswers(t *testing.T) {
	repos := []accountCostRepository{
		{accountID: "111111111111", repo: &fakeAnomalyRepo{err: errors.New("ExpiredToken")}},
	}

	_, _, err := collectAnomalies(repos, costexplorer.AnomalyQuery{}, anomalyFeedbackFilter{}, "")
	require.Error(t, err, "nobody answering must not render as 'no anomalies'")
	assert.Contains(t, err.Error(), "ExpiredToken")
}

func TestCollectAnomaliesFiltersAndDedupes(t *testing.T) {
	reviewed := sampleAnomaly()
	reviewed.AnomalyId = aws.String("an-reviewed")
	reviewed.Feedback = cetypes.AnomalyFeedbackTypeYes

	repo := &fakeAnomalyRepo{anomalies: []cetypes.Anomaly{sampleAnomaly(), sampleAnomaly(), reviewed}}
	repos := []accountCostRepository{{accountID: "111111111111", repo: repo}}

	items, _, err := collectAnomalies(repos, costexplorer.AnomalyQuery{MinTotalImpact: 100}, anomalyFeedbackFilter{unreviewed: true}, "storage")
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.Equal(t, "an-1", items[0].AnomalyID)
	assert.InDelta(t, 100, repo.lastQuery.MinTotalImpact, 0, "API-side filters are passed through")
}

// TestAnomalyToolsValidateBeforeCallingAWS uses a nil ClientPool as a tripwire:
// every rejection must happen before anything reaches for the pool, or this
// panics instead of failing.
func TestAnomalyToolsValidateBeforeCallingAWS(t *testing.T) {
	srv := &Server{ctx: context.Background()}

	for name, args := range map[string]map[string]any{
		"bad period":       {"period": "since_forever"},
		"end before start": {"start": "2026-08-02", "end": "2026-08-01"},
		"end alone":        {"end": "2026-08-01"},
		"bad feedback":     {"feedback": "maybe"},
		"bad monitor":      {"monitor_arn": "monitor-1"},
		"negative impact":  {"min_impact": -5},
	} {
		t.Run(name, func(t *testing.T) {
			req := mcp.CallToolRequest{}
			req.Params.Arguments = args

			res, err := srv.handleListCostAnomalies(context.Background(), req)
			require.NoError(t, err)
			assert.True(t, res.IsError, "expected a tool error for %v", args)
		})
	}
}

// TestAnomalyReposScopeByAccount: the repositories come from costRepositories,
// which must resolve clients through the scoped pool method — see poolClients.
func TestAnomalyReposScopeByAccount(t *testing.T) {
	spy := &scopeSpyPool{}
	srv := &Server{ctx: context.Background(), pool: spy}

	_, err := srv.costRepositories(context.Background(), "111111111111", false)
	require.Error(t, err)

	assert.Equal(t, 1, spy.accountCalls)
	assert.Zero(t, spy.allCalls, "an account_id must not fan out to every account")
	assert.Equal(t, ptypes.AwsAccountID("111111111111"), spy.lastAccount)
}
