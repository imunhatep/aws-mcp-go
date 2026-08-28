package mcpserver

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	sptypes "github.com/aws/aws-sdk-go-v2/service/savingsplans/types"

	ptypes "github.com/imunhatep/awslib/provider/types"
	"github.com/imunhatep/awslib/service/savingsplans"

	"github.com/imunhatep/aws-mcp-go/pkg/errors"
)

// Savings Plans are neither resources nor cost records, so they bypass both the
// resource proxy pipeline and Cost Explorer. A purchased plan is a contract and
// an offering rate is a price; awslib models them in service/savingsplans, and
// this file drives that repository.
//
// The API is partition-global — every client resolves to
// savingsplans.amazonaws.com whatever region it was built for — so, exactly like
// Cost Explorer, the client's region decides only which credentials sign the
// request. Inventory is account-wide, and for rates the region is a *query
// filter*. Nothing here may infer a region from the client it holds.

const (
	// defaultPlanLimit bounds the inventory response. An account holds tens of
	// plans, not thousands, so this is a guard rather than real pagination.
	defaultPlanLimit = 200
	maxPlanLimit     = 1000

	// defaultRateLimit bounds a rate lookup. One region and one instance family
	// already crosses a hundred rows once terms, payment options and plan types
	// are multiplied out.
	defaultRateLimit = 200
	maxRateLimit     = 1000
)

// -----------------------------------------------------------------------------
// Repositories
// -----------------------------------------------------------------------------

// savingsPlansRepository is the slice of awslib's Savings Plans repository the
// tools use. Both savingsplans.SavingsPlansRepository and its cached wrapper
// satisfy it, so caching stays a construction-time detail.
type savingsPlansRepository interface {
	ListSavingsPlansByStates(states []sptypes.SavingsPlanState) ([]sptypes.SavingsPlan, error)
	ListSavingsPlansAll() ([]sptypes.SavingsPlan, error)
	ListOfferingRatesByQuery(query savingsplans.OfferingRatesQuery) ([]savingsplans.OfferingRate, error)
}

// accountSavingsPlansRepository pairs a repository with the account it reports
// on, so merged results stay attributable.
type accountSavingsPlansRepository struct {
	accountID string
	repo      savingsPlansRepository
}

// savingsPlansRepositories resolves one repository per account the pool can
// reach, narrowed to a single account when accountID is set.
//
// Scoping goes through poolClients rather than filtering afterwards: building a
// client assumes that account's role or exercises that profile's credentials, so
// an account_id applied to the results would already have touched every other
// account. Clients are requested for DefaultAwsRegion only — one per account is
// enough for a partition-global API, and asking per region would multiply
// requests without finding a single extra plan.
//
// cached selects the caching wrapper. Rates are published prices and cache well;
// inventory answers "what do we own right now", where a stale answer misleads
// rather than merely lags — see the call sites.
func (s *Server) savingsPlansRepositories(accountID string, cached bool) ([]accountSavingsPlansRepository, error) {
	accountID = strings.TrimSpace(accountID)

	clients, err := s.poolClients(accountID, []ptypes.AwsRegion{ptypes.DefaultAwsRegion})
	if err != nil {
		return nil, errors.WithStack(err)
	}

	repos := make([]accountSavingsPlansRepository, 0, len(clients))
	seen := map[string]bool{}

	for _, client := range clients {
		id := client.GetAccountID().String()
		if id != "" && seen[id] {
			continue
		}

		seen[id] = true

		repo := savingsplans.NewSavingsPlansRepository(s.ctx, client)

		if cached && s.cache != nil {
			repos = append(repos, accountSavingsPlansRepository{accountID: id, repo: repo.WithCache(s.cache)})

			continue
		}

		repos = append(repos, accountSavingsPlansRepository{accountID: id, repo: repo})
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

// -----------------------------------------------------------------------------
// Argument parsing
// -----------------------------------------------------------------------------

// clampSavingsLimit bounds a row limit, mirroring clampLimit but with the
// per-tool defaults these responses need: inventory rows are small and rate rows
// are smaller, so both allow more than a resource page.
func clampSavingsLimit(limit, fallback, max int) int {
	if limit <= 0 {
		return fallback
	}

	if limit > max {
		return max
	}

	return limit
}

// planTypeAliases lets a caller say "ec2" for the plan type whose canonical
// spelling is "EC2Instance". The tools are driven by a language model, so the
// obvious spelling has to work.
//
// Keys are matched through normalizeSPToken, so they must be written in its
// normalized form — lower case, no spaces, hyphens or underscores. A key spelled
// "ec2-instance" would be unreachable, which a lookup table hides rather than
// reports.
var planTypeAliases = map[string]sptypes.SavingsPlanType{
	"ec2":         sptypes.SavingsPlanTypeEc2Instance,
	"ec2instance": sptypes.SavingsPlanTypeEc2Instance,
	"compute":     sptypes.SavingsPlanTypeCompute,
	"sagemaker":   sptypes.SavingsPlanTypeSagemaker,
	"database":    sptypes.SavingsPlanTypeDatabase,
}

// parsePlanType resolves a savings plan type, case- and punctuation-insensitively.
func parsePlanType(raw string) (sptypes.SavingsPlanType, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}

	if planType, ok := planTypeAliases[normalizeSPToken(raw)]; ok {
		return planType, nil
	}

	return "", errors.Errorf("unknown plan_type %q; use one of %s", raw, joinSPValues(sptypes.SavingsPlanType("").Values()))
}

// parsePlanStates resolves the state filter. The empty value means "active",
// because a plan stays listed after it retires: an unfiltered inventory reads as
// far more coverage than the account actually has.
func parsePlanStates(raw string) ([]sptypes.SavingsPlanState, error) {
	raw = strings.TrimSpace(raw)

	switch normalizeSPToken(raw) {
	case "":
		return []sptypes.SavingsPlanState{savingsplans.SavingsPlanStateActive}, nil
	case "all", "any":
		return nil, nil
	}

	states := []sptypes.SavingsPlanState{}

	for entry := range strings.SplitSeq(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}

		matched := false

		for _, candidate := range sptypes.SavingsPlanState("").Values() {
			if normalizeSPToken(string(candidate)) == normalizeSPToken(entry) {
				states = append(states, candidate)
				matched = true

				break
			}
		}

		if !matched {
			return nil, errors.Errorf("unknown state %q; use 'all' or one of %s", entry, joinSPValues(sptypes.SavingsPlanState("").Values()))
		}
	}

	if len(states) == 0 {
		return nil, errors.Errorf("state is empty; use 'all' or one of %s", joinSPValues(sptypes.SavingsPlanState("").Values()))
	}

	return states, nil
}

// parsePaymentOption resolves the payment option, accepting "no-upfront" and
// "no_upfront" for the API's "No Upfront".
func parsePaymentOption(raw string) (sptypes.SavingsPlanPaymentOption, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}

	for _, candidate := range sptypes.SavingsPlanPaymentOption("").Values() {
		if normalizeSPToken(string(candidate)) == normalizeSPToken(raw) {
			return candidate, nil
		}
	}

	return "", errors.Errorf("unknown payment_option %q; use one of %s", raw, joinSPValues(sptypes.SavingsPlanPaymentOption("").Values()))
}

// parseTermSeconds resolves the commitment length. Savings Plans are sold at one
// or three years only, so the argument is a term rather than a duration.
func parseTermSeconds(raw string) (int64, error) {
	switch normalizeSPToken(raw) {
	case "":
		return 0, nil
	case "1yr", "1year", "1y", "oneyear", "12months":
		return savingsplans.Term1yrSeconds, nil
	case "3yr", "3years", "3year", "3y", "threeyears", "36months":
		return savingsplans.Term3yrSeconds, nil
	}

	return 0, errors.Errorf("unknown term %q; use 1yr or 3yr", raw)
}

// spProduct pairs a product type with the rate service code that names the same
// service. The API takes both, and a mismatched pair silently returns nothing,
// so callers pick one product and this table keeps the two in step.
type spProduct struct {
	product     sptypes.SavingsPlanProductType
	serviceCode sptypes.SavingsPlanRateServiceCode
}

// Keys are the names a caller passes, and they double as the vocabulary listed in
// the error message — so they stay readable here and are matched through
// normalizeSPToken by parseProduct rather than being looked up directly.
var spProducts = map[string]spProduct{
	"ec2": {sptypes.SavingsPlanProductTypeEc2, sptypes.SavingsPlanRateServiceCodeEc2},
	// Fargate rates are published under ECS; the EKS service code
	// (SavingsPlanRateServiceCodeFargateEks) prices the same commitment for
	// Fargate-on-EKS, which is a separate lookup rather than an alternative.
	"fargate":     {sptypes.SavingsPlanProductTypeFargate, sptypes.SavingsPlanRateServiceCodeFargate},
	"fargate-eks": {sptypes.SavingsPlanProductTypeFargate, sptypes.SavingsPlanRateServiceCodeFargateEks},
	"lambda":      {sptypes.SavingsPlanProductTypeLambda, sptypes.SavingsPlanRateServiceCodeLambda},
	"sagemaker":   {sptypes.SavingsPlanProductTypeSagemaker, sptypes.SavingsPlanRateServiceCodeSagemaker},
	"rds":         {sptypes.SavingsPlanProductTypeRds, sptypes.SavingsPlanRateServiceCodeRds},
	"dynamodb":    {sptypes.SavingsPlanProductTypeDynamodb, sptypes.SavingsPlanRateServiceCodeDynamodb},
	"elasticache": {sptypes.SavingsPlanProductTypeElasticache, sptypes.SavingsPlanRateServiceCodeElasticache},
	"opensearch":  {sptypes.SavingsPlanProductTypeOpensearch, sptypes.SavingsPlanRateServiceCodeEs},
	"documentdb":  {sptypes.SavingsPlanProductTypeDocdb, sptypes.SavingsPlanRateServiceCodeDocdb},
	"docdb":       {sptypes.SavingsPlanProductTypeDocdb, sptypes.SavingsPlanRateServiceCodeDocdb},
	"neptune":     {sptypes.SavingsPlanProductTypeNeptune, sptypes.SavingsPlanRateServiceCodeNeptune},
	"timestream":  {sptypes.SavingsPlanProductTypeTimestream, sptypes.SavingsPlanRateServiceCodeTimestream},
}

// spProductNames lists the supported products for error messages.
func spProductNames() string {
	names := make([]string, 0, len(spProducts))
	for name := range spProducts {
		names = append(names, name)
	}

	sort.Strings(names)

	return strings.Join(names, ", ")
}

// parseProduct resolves the product a rate lookup prices. It defaults to EC2:
// that is what these plans are bought for most of the time, and it keeps the
// product and the service code — which the API takes separately — consistent.
func parseProduct(raw string) (spProduct, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return spProducts["ec2"], nil
	}

	wanted := normalizeSPToken(raw)

	for name, product := range spProducts {
		if normalizeSPToken(name) == wanted {
			return product, nil
		}
	}

	return spProduct{}, errors.Errorf("unknown product %q; use one of %s", raw, spProductNames())
}

// normalizeSPToken lowercases and strips the separators the same value is
// spelled with in different places: "No Upfront", "no-upfront", "no_upfront".
func normalizeSPToken(raw string) string {
	replacer := strings.NewReplacer(" ", "", "-", "", "_", "")

	return replacer.Replace(strings.ToLower(strings.TrimSpace(raw)))
}

// joinSPValues renders an enum's values for an error message.
func joinSPValues[T ~string](values []T) string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, string(value))
	}

	return strings.Join(out, ", ")
}

// -----------------------------------------------------------------------------
// Purchased plans
// -----------------------------------------------------------------------------

// planFilter narrows the inventory after the API call. Only the state is a
// server-side filter — DescribeSavingsPlans takes no others — so everything else
// is matched here, against the fields the plan itself carries.
type planFilter struct {
	planType           sptypes.SavingsPlanType
	region             string
	instanceFamily     string
	expiringWithinDays int
	now                time.Time
}

// matches reports whether a plan survives the filter.
func (f planFilter) matches(plan sptypes.SavingsPlan) bool {
	if f.planType != "" && plan.SavingsPlanType != f.planType {
		return false
	}

	if f.region != "" && !strings.EqualFold(aws.ToString(plan.Region), f.region) {
		return false
	}

	if f.instanceFamily != "" && !strings.EqualFold(aws.ToString(plan.Ec2InstanceFamily), f.instanceFamily) {
		return false
	}

	if f.expiringWithinDays > 0 {
		end, ok := parsePlanTime(aws.ToString(plan.End))
		if !ok {
			return false
		}

		remaining := end.Sub(f.now)
		if remaining < 0 || remaining > time.Duration(f.expiringWithinDays)*24*time.Hour {
			return false
		}
	}

	return true
}

// savingsPlanDTO is one purchased plan as the tool reports it.
type savingsPlanDTO struct {
	AccountID string `json:"account_id"`
	ID        string `json:"savings_plan_id"`
	ARN       string `json:"arn,omitempty"`
	Type      string `json:"type"`
	State     string `json:"state"`
	// Region and InstanceFamily are set for EC2 Instance plans, which commit to
	// one family in one region; Compute plans carry neither.
	Region         string `json:"region,omitempty"`
	InstanceFamily string `json:"ec2_instance_family,omitempty"`
	// Commitment is the hourly spend the plan commits to, in Currency.
	Commitment             string   `json:"hourly_commitment,omitempty"`
	Currency               string   `json:"currency,omitempty"`
	PaymentOption          string   `json:"payment_option,omitempty"`
	UpfrontPaymentAmount   string   `json:"upfront_payment,omitempty"`
	RecurringPaymentAmount string   `json:"recurring_payment,omitempty"`
	Term                   string   `json:"term,omitempty"`
	Start                  string   `json:"start,omitempty"`
	End                    string   `json:"end,omitempty"`
	DaysRemaining          *int     `json:"days_remaining,omitempty"`
	ProductTypes           []string `json:"product_types,omitempty"`
	OfferingID             string   `json:"offering_id,omitempty"`
	Description            string   `json:"description,omitempty"`

	Tags map[string]string `json:"tags,omitempty"`
}

// buildPlanDTO projects an SDK plan, adding the two things a caller would
// otherwise have to compute: the term as a name rather than a second count, and
// how long the commitment still has to run.
func buildPlanDTO(accountID string, plan sptypes.SavingsPlan, now time.Time) savingsPlanDTO {
	dto := savingsPlanDTO{
		AccountID:              accountID,
		ID:                     aws.ToString(plan.SavingsPlanId),
		ARN:                    aws.ToString(plan.SavingsPlanArn),
		Type:                   string(plan.SavingsPlanType),
		State:                  string(plan.State),
		Region:                 aws.ToString(plan.Region),
		InstanceFamily:         aws.ToString(plan.Ec2InstanceFamily),
		Commitment:             aws.ToString(plan.Commitment),
		Currency:               string(plan.Currency),
		PaymentOption:          string(plan.PaymentOption),
		UpfrontPaymentAmount:   aws.ToString(plan.UpfrontPaymentAmount),
		RecurringPaymentAmount: aws.ToString(plan.RecurringPaymentAmount),
		Term:                   termName(plan.TermDurationInSeconds),
		Start:                  aws.ToString(plan.Start),
		End:                    aws.ToString(plan.End),
		OfferingID:             aws.ToString(plan.OfferingId),
		Description:            aws.ToString(plan.Description),
		Tags:                   plan.Tags,
	}

	for _, product := range plan.ProductTypes {
		dto.ProductTypes = append(dto.ProductTypes, string(product))
	}

	if end, ok := parsePlanTime(dto.End); ok {
		days := int(end.Sub(now).Hours() / 24)
		dto.DaysRemaining = &days
	}

	return dto
}

// termName renders a commitment length as the term it was sold as. An unexpected
// duration is reported in seconds rather than guessed at.
func termName(seconds int64) string {
	switch seconds {
	case 0:
		return ""
	case savingsplans.Term1yrSeconds:
		return "1yr"
	case savingsplans.Term3yrSeconds:
		return "3yr"
	default:
		return strconv.FormatInt(seconds, 10) + "s"
	}
}

// parsePlanTime parses the timestamps DescribeSavingsPlans returns as strings.
func parsePlanTime(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}

	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z0700", "2006-01-02"} {
		if parsed, err := time.Parse(layout, raw); err == nil {
			return parsed, true
		}
	}

	return time.Time{}, false
}

// planSummary aggregates an inventory into the numbers a caller would otherwise
// have to add up: how many plans, in which states and types, and how much they
// commit to per hour.
type planSummary struct {
	Plans            int                `json:"plans"`
	ByType           map[string]int     `json:"by_type,omitempty"`
	ByState          map[string]int     `json:"by_state,omitempty"`
	ByRegion         map[string]int     `json:"by_region,omitempty"`
	HourlyCommitment map[string]string  `json:"hourly_commitment,omitempty"`
	UpfrontPayment   map[string]string  `json:"upfront_payment,omitempty"`
	Unparsed         int                `json:"amounts_unparsed,omitempty"`
	NextExpiry       *savingsPlanExpiry `json:"next_expiry,omitempty"`
}

// savingsPlanExpiry names the plan that runs out first, which is the one a
// renewal decision hangs on.
type savingsPlanExpiry struct {
	ID            string `json:"savings_plan_id"`
	AccountID     string `json:"account_id,omitempty"`
	End           string `json:"end"`
	DaysRemaining int    `json:"days_remaining"`
}

// summarizePlans totals the commitments per currency.
//
// Amounts are summed per currency and never across: adding USD to EUR would
// produce a number that looks authoritative and means nothing. An amount that
// does not parse is counted in Unparsed rather than silently treated as zero.
func summarizePlans(plans []savingsPlanDTO) planSummary {
	summary := planSummary{
		Plans:    len(plans),
		ByType:   map[string]int{},
		ByState:  map[string]int{},
		ByRegion: map[string]int{},
	}

	hourly := map[string]float64{}
	upfront := map[string]float64{}

	for _, plan := range plans {
		if plan.Type != "" {
			summary.ByType[plan.Type]++
		}

		if plan.State != "" {
			summary.ByState[plan.State]++
		}

		if plan.Region != "" {
			summary.ByRegion[plan.Region]++
		}

		currency := plan.Currency
		if currency == "" {
			currency = "unknown"
		}

		if plan.Commitment != "" {
			if amount, err := strconv.ParseFloat(plan.Commitment, 64); err == nil {
				hourly[currency] += amount
			} else {
				summary.Unparsed++
			}
		}

		if plan.UpfrontPaymentAmount != "" {
			if amount, err := strconv.ParseFloat(plan.UpfrontPaymentAmount, 64); err == nil {
				upfront[currency] += amount
			} else {
				summary.Unparsed++
			}
		}

		if plan.DaysRemaining == nil {
			continue
		}

		if summary.NextExpiry == nil || *plan.DaysRemaining < summary.NextExpiry.DaysRemaining {
			summary.NextExpiry = &savingsPlanExpiry{
				ID:            plan.ID,
				AccountID:     plan.AccountID,
				End:           plan.End,
				DaysRemaining: *plan.DaysRemaining,
			}
		}
	}

	summary.HourlyCommitment = formatAmounts(hourly)
	summary.UpfrontPayment = formatAmounts(upfront)

	return summary
}

// formatAmounts renders summed amounts to four decimals — hourly commitments run
// to fractions of a cent — dropping the map entirely when nothing summed.
func formatAmounts(amounts map[string]float64) map[string]string {
	if len(amounts) == 0 {
		return nil
	}

	out := make(map[string]string, len(amounts))
	for currency, amount := range amounts {
		out[currency] = strconv.FormatFloat(amount, 'f', 4, 64)
	}

	return out
}

// sortPlans orders the inventory stably: account, then type, region and ID.
func sortPlans(plans []savingsPlanDTO) {
	sort.Slice(plans, func(i, j int) bool {
		left, right := plans[i], plans[j]

		if left.AccountID != right.AccountID {
			return left.AccountID < right.AccountID
		}

		if left.Type != right.Type {
			return left.Type < right.Type
		}

		if left.Region != right.Region {
			return left.Region < right.Region
		}

		return left.ID < right.ID
	})
}

// -----------------------------------------------------------------------------
// Offering rates
// -----------------------------------------------------------------------------

// offeringRateDTO is one rate on offer.
type offeringRateDTO struct {
	Region             string `json:"region,omitempty"`
	InstanceType       string `json:"instance_type,omitempty"`
	InstanceFamily     string `json:"instance_family,omitempty"`
	PlanType           string `json:"plan_type,omitempty"`
	PaymentOption      string `json:"payment_option,omitempty"`
	Term               string `json:"term,omitempty"`
	HourlyRate         string `json:"hourly_rate,omitempty"`
	Currency           string `json:"currency,omitempty"`
	Unit               string `json:"unit,omitempty"`
	ProductType        string `json:"product_type,omitempty"`
	ServiceCode        string `json:"service_code,omitempty"`
	ProductDescription string `json:"product_description,omitempty"`
	Tenancy            string `json:"tenancy,omitempty"`
	UsageType          string `json:"usage_type,omitempty"`
	Operation          string `json:"operation,omitempty"`
}

// buildRateDTO projects a rate. Every accessor used here is nil-safe in awslib: a
// rate whose parent offering AWS omitted reads as empty rather than panicking.
func buildRateDTO(rate savingsplans.OfferingRate) offeringRateDTO {
	return offeringRateDTO{
		Region:             rate.Region(),
		InstanceType:       rate.InstanceType(),
		InstanceFamily:     rate.InstanceFamily(),
		PlanType:           string(rate.PlanType()),
		PaymentOption:      string(rate.PaymentOption()),
		Term:               termName(rate.DurationSeconds()),
		HourlyRate:         rate.HourlyRate(),
		Currency:           string(rate.Currency()),
		Unit:               string(rate.Unit),
		ProductType:        string(rate.ProductType),
		ServiceCode:        string(rate.ServiceCode),
		ProductDescription: rate.ProductDescription(),
		Tenancy:            rate.Tenancy(),
		UsageType:          aws.ToString(rate.UsageType),
		Operation:          aws.ToString(rate.Operation),
	}
}

// sortRates orders rates so a comparison reads naturally: by instance type, then
// plan type, payment option and term.
func sortRates(rates []offeringRateDTO) {
	sort.Slice(rates, func(i, j int) bool {
		left, right := rates[i], rates[j]

		if left.InstanceType != right.InstanceType {
			return left.InstanceType < right.InstanceType
		}

		if left.PlanType != right.PlanType {
			return left.PlanType < right.PlanType
		}

		if left.PaymentOption != right.PaymentOption {
			return left.PaymentOption < right.PaymentOption
		}

		if left.Term != right.Term {
			return left.Term < right.Term
		}

		return left.HourlyRate < right.HourlyRate
	})
}
