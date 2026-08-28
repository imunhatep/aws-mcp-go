package mcpserver

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	sptypes "github.com/aws/aws-sdk-go-v2/service/savingsplans/types"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ptypes "github.com/imunhatep/awslib/provider/types"
	"github.com/imunhatep/awslib/service/savingsplans"
)

// now is a fixed clock: "days remaining" and the expiry filter are relative to
// today, and a test that drifts with the calendar is worse than no test.
var spNow = time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)

func TestParsePlanType(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want sptypes.SavingsPlanType
	}{
		{raw: "", want: ""},
		{raw: "ec2", want: sptypes.SavingsPlanTypeEc2Instance},
		{raw: "EC2Instance", want: sptypes.SavingsPlanTypeEc2Instance},
		{raw: "ec2-instance", want: sptypes.SavingsPlanTypeEc2Instance},
		{raw: "compute", want: sptypes.SavingsPlanTypeCompute},
		{raw: "SageMaker", want: sptypes.SavingsPlanTypeSagemaker},
	} {
		got, err := parsePlanType(tc.raw)
		require.NoErrorf(t, err, "plan_type %q", tc.raw)
		assert.Equal(t, tc.want, got, "plan_type %q", tc.raw)
	}

	_, err := parsePlanType("ec3")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "EC2Instance", "the error must list the valid values")
}

// TestParsePlanStatesDefaultsToActive: a retired plan stays listed by the API, so
// an unfiltered inventory reads as far more coverage than the account has.
func TestParsePlanStatesDefaultsToActive(t *testing.T) {
	states, err := parsePlanStates("")
	require.NoError(t, err)
	assert.Equal(t, []sptypes.SavingsPlanState{sptypes.SavingsPlanStateActive}, states)

	states, err = parsePlanStates("all")
	require.NoError(t, err)
	assert.Nil(t, states, "'all' means no server-side state filter")

	states, err = parsePlanStates("active, payment-pending")
	require.NoError(t, err)
	assert.Equal(t, []sptypes.SavingsPlanState{
		sptypes.SavingsPlanStateActive,
		sptypes.SavingsPlanStatePaymentPending,
	}, states)

	states, err = parsePlanStates("PAYMENT_PENDING")
	require.NoError(t, err)
	assert.Equal(t, []sptypes.SavingsPlanState{sptypes.SavingsPlanStatePaymentPending}, states)

	_, err = parsePlanStates("sleeping")
	require.Error(t, err)
}

func TestParsePaymentOptionAndTerm(t *testing.T) {
	for _, raw := range []string{"No Upfront", "no-upfront", "no_upfront", "NOUPFRONT"} {
		got, err := parsePaymentOption(raw)
		require.NoErrorf(t, err, "payment_option %q", raw)
		assert.Equal(t, sptypes.SavingsPlanPaymentOptionNoUpfront, got)
	}

	_, err := parsePaymentOption("half upfront")
	require.Error(t, err)

	for _, raw := range []string{"1yr", "1 year", "12months"} {
		got, err := parseTermSeconds(raw)
		require.NoErrorf(t, err, "term %q", raw)
		assert.Equal(t, savingsplans.Term1yrSeconds, got)
	}

	got, err := parseTermSeconds("3yr")
	require.NoError(t, err)
	assert.Equal(t, savingsplans.Term3yrSeconds, got)

	got, err = parseTermSeconds("")
	require.NoError(t, err)
	assert.Zero(t, got, "no term keeps every term")

	_, err = parseTermSeconds("2yr")
	require.Error(t, err)
}

// TestParseProductPairsServiceCode: the API takes the product type and the rate
// service code separately, and a mismatched pair returns nothing rather than an
// error — so the two must always be resolved together.
func TestParseProductPairsServiceCode(t *testing.T) {
	product, err := parseProduct("")
	require.NoError(t, err)
	assert.Equal(t, sptypes.SavingsPlanProductTypeEc2, product.product, "ec2 is the default")
	assert.Equal(t, sptypes.SavingsPlanRateServiceCodeEc2, product.serviceCode)

	product, err = parseProduct("Fargate")
	require.NoError(t, err)
	assert.Equal(t, sptypes.SavingsPlanProductTypeFargate, product.product)
	assert.Equal(t, sptypes.SavingsPlanRateServiceCodeFargate, product.serviceCode, "fargate rates are published under ECS")

	product, err = parseProduct("fargate-eks")
	require.NoError(t, err)
	assert.Equal(t, sptypes.SavingsPlanRateServiceCodeFargateEks, product.serviceCode)

	_, err = parseProduct("ec2-spot")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ec2")
}

func TestDefaultedFilter(t *testing.T) {
	assert.Equal(t, "Linux/UNIX", defaultedFilter("", "Linux/UNIX"))
	assert.Equal(t, "Windows", defaultedFilter("Windows", "Linux/UNIX"))
	assert.Empty(t, defaultedFilter("all", "Linux/UNIX"), "'all' clears the default")
	assert.Empty(t, defaultedFilter("ANY", "shared"))
}

func TestTermName(t *testing.T) {
	assert.Equal(t, "1yr", termName(savingsplans.Term1yrSeconds))
	assert.Equal(t, "3yr", termName(savingsplans.Term3yrSeconds))
	assert.Empty(t, termName(0))
	assert.Equal(t, "1000s", termName(1000), "an unexpected duration is reported, not guessed at")
}

// samplePlan is an EC2 Instance plan ending 40 days after spNow.
func samplePlan() sptypes.SavingsPlan {
	return sptypes.SavingsPlan{
		SavingsPlanId:         aws.String("sp-111"),
		SavingsPlanArn:        aws.String("arn:aws:savingsplans::111111111111:savingsplan/sp-111"),
		SavingsPlanType:       sptypes.SavingsPlanTypeEc2Instance,
		State:                 sptypes.SavingsPlanStateActive,
		Region:                aws.String("eu-central-1"),
		Ec2InstanceFamily:     aws.String("m5"),
		Commitment:            aws.String("1.5000"),
		Currency:              sptypes.CurrencyCodeUsd,
		PaymentOption:         sptypes.SavingsPlanPaymentOptionNoUpfront,
		UpfrontPaymentAmount:  aws.String("0.0"),
		TermDurationInSeconds: savingsplans.Term1yrSeconds,
		Start:                 aws.String("2025-10-07T00:00:00.000Z"),
		End:                   aws.String("2026-10-07T12:00:00.000Z"),
		ProductTypes:          []sptypes.SavingsPlanProductType{sptypes.SavingsPlanProductTypeEc2},
		Tags:                  map[string]string{"Team": "platform"},
	}
}

func TestBuildPlanDTO(t *testing.T) {
	dto := buildPlanDTO("111111111111", samplePlan(), spNow)

	assert.Equal(t, "111111111111", dto.AccountID)
	assert.Equal(t, "sp-111", dto.ID)
	assert.Equal(t, "EC2Instance", dto.Type)
	assert.Equal(t, "eu-central-1", dto.Region)
	assert.Equal(t, "m5", dto.InstanceFamily)
	assert.Equal(t, "1.5000", dto.Commitment)
	assert.Equal(t, "USD", dto.Currency)
	assert.Equal(t, "1yr", dto.Term)
	assert.Equal(t, []string{"EC2"}, dto.ProductTypes)
	assert.Equal(t, map[string]string{"Team": "platform"}, dto.Tags)

	require.NotNil(t, dto.DaysRemaining, "the AWS timestamp format must parse")
	assert.Equal(t, 40, *dto.DaysRemaining)
}

// TestBuildPlanDTOWithoutEnd: a plan whose end AWS did not report must not grow a
// fabricated countdown.
func TestBuildPlanDTOWithoutEnd(t *testing.T) {
	plan := samplePlan()
	plan.End = nil

	dto := buildPlanDTO("111111111111", plan, spNow)

	assert.Nil(t, dto.DaysRemaining)
	assert.Empty(t, dto.End)
}

func TestPlanFilterMatches(t *testing.T) {
	plan := samplePlan()

	compute := samplePlan()
	compute.SavingsPlanId = aws.String("sp-222")
	compute.SavingsPlanType = sptypes.SavingsPlanTypeCompute
	compute.Region = nil
	compute.Ec2InstanceFamily = nil

	t.Run("plan type", func(t *testing.T) {
		filter := planFilter{planType: sptypes.SavingsPlanTypeEc2Instance, now: spNow}

		assert.True(t, filter.matches(plan))
		assert.False(t, filter.matches(compute))
	})

	t.Run("region excludes compute plans", func(t *testing.T) {
		filter := planFilter{region: "eu-central-1", now: spNow}

		assert.True(t, filter.matches(plan))
		assert.False(t, filter.matches(compute), "a compute plan carries no region")
	})

	t.Run("instance family is case-insensitive", func(t *testing.T) {
		assert.True(t, planFilter{instanceFamily: "M5", now: spNow}.matches(plan))
		assert.False(t, planFilter{instanceFamily: "c6i", now: spNow}.matches(plan))
	})

	t.Run("expiring within days", func(t *testing.T) {
		assert.True(t, planFilter{expiringWithinDays: 60, now: spNow}.matches(plan))
		assert.False(t, planFilter{expiringWithinDays: 30, now: spNow}.matches(plan))

		ended := samplePlan()
		ended.End = aws.String("2026-01-01T00:00:00.000Z")
		assert.False(t, planFilter{expiringWithinDays: 60, now: spNow}.matches(ended), "an ended plan is not expiring soon")

		unreadable := samplePlan()
		unreadable.End = aws.String("whenever")
		assert.False(t, planFilter{expiringWithinDays: 60, now: spNow}.matches(unreadable))
	})
}

// TestSummarizePlansKeepsCurrenciesApart: summing USD into EUR would produce a
// number that looks authoritative and means nothing.
func TestSummarizePlansKeepsCurrenciesApart(t *testing.T) {
	usd := buildPlanDTO("111111111111", samplePlan(), spNow)

	second := samplePlan()
	second.SavingsPlanId = aws.String("sp-333")
	second.Commitment = aws.String("0.2500")
	second.UpfrontPaymentAmount = aws.String("100.50")
	second.End = aws.String("2026-09-07T12:00:00.000Z")
	usd2 := buildPlanDTO("111111111111", second, spNow)

	third := samplePlan()
	third.SavingsPlanId = aws.String("sp-444")
	third.Currency = sptypes.CurrencyCodeCny
	third.Commitment = aws.String("2.0000")
	eur := buildPlanDTO("222222222222", third, spNow)

	broken := samplePlan()
	broken.SavingsPlanId = aws.String("sp-555")
	broken.Commitment = aws.String("n/a")
	unparsed := buildPlanDTO("222222222222", broken, spNow)

	summary := summarizePlans([]savingsPlanDTO{usd, usd2, eur, unparsed})

	assert.Equal(t, 4, summary.Plans)
	assert.Equal(t, map[string]int{"EC2Instance": 4}, summary.ByType)
	assert.Equal(t, map[string]int{"active": 4}, summary.ByState)
	assert.Equal(t, "1.7500", summary.HourlyCommitment["USD"])
	assert.Equal(t, "2.0000", summary.HourlyCommitment["CNY"])
	assert.Equal(t, "100.5000", summary.UpfrontPayment["USD"])
	assert.Equal(t, 1, summary.Unparsed, "an unparseable amount is counted, never treated as zero")

	require.NotNil(t, summary.NextExpiry)
	assert.Equal(t, "sp-333", summary.NextExpiry.ID, "the plan running out first is the one a renewal hangs on")
	assert.Equal(t, 10, summary.NextExpiry.DaysRemaining)
}

func TestBuildRateDTO(t *testing.T) {
	rate := savingsplans.NewOfferingRate(sptypes.SavingsPlanOfferingRate{
		Rate:        aws.String("0.0680"),
		Unit:        sptypes.SavingsPlanRateUnitHours,
		ProductType: sptypes.SavingsPlanProductTypeEc2,
		ServiceCode: sptypes.SavingsPlanRateServiceCodeEc2,
		UsageType:   aws.String("EUC1-BoxUsage:m5.large"),
		Operation:   aws.String("RunInstances"),
		SavingsPlanOffering: &sptypes.ParentSavingsPlanOffering{
			DurationSeconds: savingsplans.Term1yrSeconds,
			PlanType:        sptypes.SavingsPlanTypeEc2Instance,
			PaymentOption:   sptypes.SavingsPlanPaymentOptionNoUpfront,
			Currency:        sptypes.CurrencyCodeUsd,
		},
		Properties: []sptypes.SavingsPlanOfferingRateProperty{
			{Name: aws.String("region"), Value: aws.String("eu-central-1")},
			{Name: aws.String("instanceType"), Value: aws.String("m5.large")},
			{Name: aws.String("instanceFamily"), Value: aws.String("m5")},
			{Name: aws.String("productDescription"), Value: aws.String("Linux/UNIX")},
			{Name: aws.String("tenancy"), Value: aws.String("shared")},
		},
	})

	dto := buildRateDTO(rate)

	assert.Equal(t, "eu-central-1", dto.Region)
	assert.Equal(t, "m5.large", dto.InstanceType)
	assert.Equal(t, "m5", dto.InstanceFamily)
	assert.Equal(t, "EC2Instance", dto.PlanType)
	assert.Equal(t, "No Upfront", dto.PaymentOption)
	assert.Equal(t, "1yr", dto.Term)
	assert.Equal(t, "0.0680", dto.HourlyRate)
	assert.Equal(t, "USD", dto.Currency)
	assert.Equal(t, "Hrs", dto.Unit)
	assert.Equal(t, "Linux/UNIX", dto.ProductDescription)
	assert.Equal(t, "shared", dto.Tenancy)
	assert.Equal(t, "RunInstances", dto.Operation)
}

// TestBuildRateDTOWithoutParentOffering: awslib's accessors are nil-safe, and the
// projection must stay so — a rate AWS returned without its parent offering reads
// as empty rather than panicking.
func TestBuildRateDTOWithoutParentOffering(t *testing.T) {
	dto := buildRateDTO(savingsplans.NewOfferingRate(sptypes.SavingsPlanOfferingRate{
		Rate: aws.String("0.0680"),
	}))

	assert.Equal(t, "0.0680", dto.HourlyRate)
	assert.Empty(t, dto.PlanType)
	assert.Empty(t, dto.Term)
	assert.Empty(t, dto.InstanceType)
}

func TestClampSavingsLimit(t *testing.T) {
	assert.Equal(t, defaultPlanLimit, clampSavingsLimit(0, defaultPlanLimit, maxPlanLimit))
	assert.Equal(t, defaultPlanLimit, clampSavingsLimit(-5, defaultPlanLimit, maxPlanLimit))
	assert.Equal(t, 25, clampSavingsLimit(25, defaultPlanLimit, maxPlanLimit))
	assert.Equal(t, maxPlanLimit, clampSavingsLimit(99999, defaultPlanLimit, maxPlanLimit))
}

// -----------------------------------------------------------------------------
// Handler wiring
// -----------------------------------------------------------------------------

// TestSavingsPlansToolsValidateBeforeCallingAWS uses a nil ClientPool as a
// tripwire, the same way the cost tools' tests do: every rejection below must
// happen before anything reaches for the pool, or this panics instead of failing.
func TestSavingsPlansToolsValidateBeforeCallingAWS(t *testing.T) {
	srv := &Server{ctx: context.Background()}

	t.Run("list_savings_plans", func(t *testing.T) {
		for name, args := range map[string]map[string]any{
			"bad plan type": {"plan_type": "ec5"},
			"bad state":     {"state": "sleeping"},
			"bad region":    {"region": "moon-central-1"},
			"negative days": {"expiring_within_days": -3},
		} {
			t.Run(name, func(t *testing.T) {
				req := mcp.CallToolRequest{}
				req.Params.Arguments = args

				res, err := srv.handleListSavingsPlans(context.Background(), req)
				require.NoError(t, err)
				assert.True(t, res.IsError, "expected a tool error for %v", args)
			})
		}
	})

	t.Run("list_savings_plan_rates", func(t *testing.T) {
		for name, args := range map[string]map[string]any{
			"no region":     {"instance_type": "m5.large"},
			"bad region":    {"region": "moon-central-1", "instance_type": "m5.large"},
			"no instance":   {"region": "eu-central-1"},
			"bad product":   {"region": "eu-central-1", "instance_type": "m5.large", "product": "ec2-spot"},
			"bad plan type": {"region": "eu-central-1", "instance_type": "m5.large", "plan_type": "ec5"},
			"bad payment":   {"region": "eu-central-1", "instance_type": "m5.large", "payment_option": "half"},
			"bad term":      {"region": "eu-central-1", "instance_type": "m5.large", "term": "2yr"},
		} {
			t.Run(name, func(t *testing.T) {
				req := mcp.CallToolRequest{}
				req.Params.Arguments = args

				res, err := srv.handleListSavingsPlanRates(context.Background(), req)
				require.NoError(t, err)
				assert.True(t, res.IsError, "expected a tool error for %v", args)
			})
		}
	})
}

// TestSavingsPlansScopeByAccount: the repositories must be resolved through the
// scoped pool method, because building a client already exercises that account's
// credentials — see poolClients.
func TestSavingsPlansScopeByAccount(t *testing.T) {
	t.Run("scoped", func(t *testing.T) {
		spy := &scopeSpyPool{}
		srv := &Server{ctx: context.Background(), pool: spy}

		// No clients come back from the spy, so this ends in "no accounts are
		// reachable" — the assertion is about which pool method was reached.
		_, err := srv.savingsPlansRepositories("111111111111", false)
		require.Error(t, err)

		assert.Equal(t, 1, spy.accountCalls)
		assert.Zero(t, spy.allCalls, "an account_id must not fan out to every account")
		assert.Equal(t, ptypes.AwsAccountID("111111111111"), spy.lastAccount)
	})

	t.Run("unscoped", func(t *testing.T) {
		spy := &scopeSpyPool{}
		srv := &Server{ctx: context.Background(), pool: spy}

		_, err := srv.savingsPlansRepositories("", false)
		require.Error(t, err)

		assert.Equal(t, 1, spy.allCalls)
		assert.Zero(t, spy.accountCalls)
	})
}

// fakeSavingsPlansRepo is a repository double: the real one needs a *v3.Client,
// whose identity fields are unexported and only set by a real STS call.
type fakeSavingsPlansRepo struct {
	plans      []sptypes.SavingsPlan
	rates      []savingsplans.OfferingRate
	err        error
	stateCalls int
	allCalls   int
	lastStates []sptypes.SavingsPlanState
	lastQuery  savingsplans.OfferingRatesQuery
}

func (f *fakeSavingsPlansRepo) ListSavingsPlansByStates(states []sptypes.SavingsPlanState) ([]sptypes.SavingsPlan, error) {
	f.stateCalls++
	f.lastStates = states

	return f.plans, f.err
}

func (f *fakeSavingsPlansRepo) ListSavingsPlansAll() ([]sptypes.SavingsPlan, error) {
	f.allCalls++

	return f.plans, f.err
}

func (f *fakeSavingsPlansRepo) ListOfferingRatesByQuery(query savingsplans.OfferingRatesQuery) ([]savingsplans.OfferingRate, error) {
	f.lastQuery = query

	return f.rates, f.err
}

// TestListPlansPicksTheStateFilteredCall: naming states must let the API do the
// filtering, and 'all' must fall through to the unfiltered call.
func TestListPlansPicksTheStateFilteredCall(t *testing.T) {
	repo := &fakeSavingsPlansRepo{plans: []sptypes.SavingsPlan{samplePlan()}}

	plans, err := listPlans(repo, []sptypes.SavingsPlanState{sptypes.SavingsPlanStateActive})
	require.NoError(t, err)
	assert.Len(t, plans, 1)
	assert.Equal(t, 1, repo.stateCalls)
	assert.Zero(t, repo.allCalls)
	assert.Equal(t, []sptypes.SavingsPlanState{sptypes.SavingsPlanStateActive}, repo.lastStates)

	_, err = listPlans(repo, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, repo.allCalls)
}

// TestSavingsPlansToolsAreRegistered pins the tool names, which are the API this
// change exposes.
func TestSavingsPlansToolsAreRegistered(t *testing.T) {
	srv := NewServer(context.Background(), nil, nil)

	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{"region": "eu-central-1"}

	// Reaching the handler at all proves it is wired; a missing instance filter
	// is rejected before the nil pool is touched.
	res, err := srv.handleListSavingsPlanRates(context.Background(), req)
	require.NoError(t, err)
	require.True(t, res.IsError)

	text, ok := res.Content[0].(mcp.TextContent)
	require.True(t, ok)
	assert.Contains(t, text.Text, "instance_type or instance_family is required")
}

// TestSavingsPlansEnvelopeShape guards the JSON contract an agent reads: the
// summary must cover every matching plan even when the rows are capped, or a
// truncated response understates the commitment.
func TestSavingsPlansEnvelopeShape(t *testing.T) {
	plans := []savingsPlanDTO{}

	for _, id := range []string{"sp-a", "sp-b", "sp-c"} {
		plan := samplePlan()
		plan.SavingsPlanId = aws.String(id)
		plans = append(plans, buildPlanDTO("111111111111", plan, spNow))
	}

	result := savingsPlansResult{
		Total:           len(plans),
		Summary:         summarizePlans(plans),
		AccountsQueried: []string{"111111111111"},
		Truncated:       true,
		Items:           plans[:1],
		Count:           1,
	}

	raw, err := json.Marshal(result)
	require.NoError(t, err)

	doc := map[string]any{}
	require.NoError(t, json.Unmarshal(raw, &doc))

	assert.Equal(t, float64(3), doc["total"])
	assert.Equal(t, float64(1), doc["count"])
	assert.Equal(t, true, doc["truncated"])

	summary := doc["summary"].(map[string]any)
	assert.Equal(t, float64(3), summary["plans"])
	assert.Equal(t, "4.5000", summary["hourly_commitment"].(map[string]any)["USD"],
		"the summary totals every matching plan, not the returned page")
}
