package mcpserver

import (
	"context"
	"fmt"
	"strings"
	"time"

	sptypes "github.com/aws/aws-sdk-go-v2/service/savingsplans/types"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/rs/zerolog/log"

	ptypes "github.com/imunhatep/awslib/provider/types"
	"github.com/imunhatep/awslib/service/savingsplans"
)

// registerSavingsPlansTools declares the Savings Plans tools. Like the Cost
// Explorer tools they sit outside the resource pipeline — a purchased plan is a
// commitment, not an inventory item — and are answered from awslib's
// service/savingsplans repository directly.
func (s *Server) registerSavingsPlansTools() {
	s.mcp.AddTool(
		mcp.NewTool(
			"list_savings_plans",
			mcp.WithDescription("List the Savings Plans an account has purchased — EC2 Instance, Compute, SageMaker and Database plans — with their commitment, term, payment option and how long they still run. Answers 'which EC2 savings plans do we hold', 'what are we committed to per hour' and 'what expires soon'. Defaults to active plans only, since retired and payment-failed plans stay listed and would overstate coverage. Returns {items, count, total, summary, accounts_queried}; the summary totals the hourly commitment and upfront payment per currency and names the plan expiring next. Plans are account-wide, not regional — the region field is the region an EC2 Instance plan is locked to. Read live, not cached, because a stale answer to 'what do we own' misleads."),
			mcp.WithString("plan_type",
				mcp.Description("Only plans of this type: EC2Instance (accepts 'ec2'), Compute, SageMaker or Database. Omit for every type."),
			),
			mcp.WithString("state",
				mcp.Description("Plan states to include, comma-separated. Defaults to 'active'. Use 'all' for every state, or name them: payment-pending, payment-failed, active, retired, queued, queued-deleted, pending-return, returned."),
			),
			mcp.WithString("region",
				mcp.Description("Only EC2 Instance plans locked to this region (e.g. eu-central-1). Compute plans carry no region and are excluded by this filter."),
			),
			mcp.WithString("instance_family",
				mcp.Description("Only EC2 Instance plans committed to this instance family (e.g. m5, c6i)."),
			),
			mcp.WithNumber("expiring_within_days",
				mcp.Description("Only plans whose commitment ends within this many days. Plans that have already ended, or whose end date is unreadable, are excluded."),
			),
			mcp.WithString("account_id",
				mcp.Description("Query only this account. Scopes the query rather than filtering its output: only this account's credentials are used and only it is called. By default every reachable account is queried and each row carries its account_id."),
			),
			mcp.WithNumber("limit",
				mcp.Description("Maximum plans to return (default 200, max 1000). total and truncated report what was dropped; the summary always covers every matching plan, not just the returned page."),
			),
		),
		s.handleListSavingsPlans,
	)

	s.mcp.AddTool(
		mcp.NewTool(
			"list_savings_plan_rates",
			mcp.WithDescription("Look up what a Savings Plan commitment would cost: the discounted hourly rates on offer for a product in a region, per plan type, payment option and term. Answers 'what would an m5.2xlarge cost under a 3-year EC2 Instance plan in eu-central-1'. These are public prices, not this account's usage — for what is actually being spent use get_cost_and_usage, and for what is already committed use list_savings_plans. region is required, and so is instance_type or instance_family: unfiltered, this is a sweep of thousands of pages of the price list. Defaults to EC2, Linux/UNIX, shared tenancy, and returns every plan type, payment option and term so they can be compared side by side. Results are cached (default 6h) since published prices change rarely."),
			mcp.WithString("region",
				mcp.Required(),
				mcp.Description("AWS region to price, e.g. eu-central-1. This is a query filter, not an endpoint — the Savings Plans API is global, so any reachable account can price any region."),
			),
			mcp.WithString("instance_type",
				mcp.Description("Instance type to price, e.g. m5.2xlarge. Either this or instance_family is required."),
			),
			mcp.WithString("instance_family",
				mcp.Description("Instance family to price, e.g. m5 — returns every size in the family. Either this or instance_type is required."),
			),
			mcp.WithString("product",
				mcp.Description("Product the commitment covers. Defaults to ec2. One of: docdb, documentdb, dynamodb, ec2, elasticache, fargate, fargate-eks, lambda, neptune, opensearch, rds, sagemaker, timestream."),
			),
			mcp.WithString("plan_type",
				mcp.Description("Only rates for this plan type: EC2Instance (accepts 'ec2') commits to one family in one region for the deepest discount; Compute is flexible across region, family and service. Omit to get both, which is the comparison worth making."),
			),
			mcp.WithString("payment_option",
				mcp.Description("Only rates for this payment option: 'No Upfront', 'Partial Upfront' or 'All Upfront' (hyphens and underscores accepted). Omit for all three."),
			),
			mcp.WithString("term",
				mcp.Description("Only rates for this commitment length: 1yr or 3yr. Omit for both."),
			),
			mcp.WithString("product_description",
				mcp.Description("Operating system / product description, e.g. 'Linux/UNIX', 'Windows', 'Red Hat Enterprise Linux'. Defaults to Linux/UNIX; pass 'all' to return every description. EC2 only."),
			),
			mcp.WithString("tenancy",
				mcp.Description("Instance tenancy: 'shared', 'dedicated' or 'host'. Defaults to shared; pass 'all' to return every tenancy. EC2 only."),
			),
			mcp.WithString("account_id",
				mcp.Description("Use this account's credentials for the lookup. Rates are public prices and identical whichever account asks, so only one account is ever queried — this just picks which."),
			),
			mcp.WithNumber("limit",
				mcp.Description("Maximum rates to return (default 200, max 1000), sorted by instance type, plan type, payment option and term."),
			),
		),
		s.handleListSavingsPlanRates,
	)
}

// savingsPlansResult is the list_savings_plans envelope.
//
// AccountsQueried is here for the same reason list_resources_fallback carries
// `queried`: a caller cannot otherwise tell "we hold no plans" from "we asked one
// account out of twelve".
type savingsPlansResult struct {
	Items           []savingsPlanDTO `json:"items"`
	Count           int              `json:"count"`
	Total           int              `json:"total"`
	Truncated       bool             `json:"truncated,omitempty"`
	Summary         planSummary      `json:"summary"`
	AccountsQueried []string         `json:"accounts_queried"`
	Warnings        []string         `json:"warnings,omitempty"`
	Note            string           `json:"note,omitempty"`
}

func (s *Server) handleListSavingsPlans(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	planType, err := parsePlanType(req.GetString("plan_type", ""))
	if err != nil {
		return mcp.NewToolResultErrorFromErr("invalid plan_type", err), nil
	}

	states, err := parsePlanStates(req.GetString("state", ""))
	if err != nil {
		return mcp.NewToolResultErrorFromErr("invalid state", err), nil
	}

	region := strings.TrimSpace(req.GetString("region", ""))
	if region != "" {
		if _, err := resolveRegions(region); err != nil {
			return mcp.NewToolResultErrorFromErr("invalid region", err), nil
		}
	}

	limit := clampSavingsLimit(req.GetInt("limit", 0), defaultPlanLimit, maxPlanLimit)
	expiring := req.GetInt("expiring_within_days", 0)
	if expiring < 0 {
		return mcp.NewToolResultErrorf("expiring_within_days must be positive, got %d", expiring), nil
	}

	filter := planFilter{
		planType:           planType,
		region:             region,
		instanceFamily:     strings.TrimSpace(req.GetString("instance_family", "")),
		expiringWithinDays: expiring,
		now:                time.Now(),
	}

	// Inventory is read live: this tool answers "what are we committed to right
	// now", and the resource cache's default 6h window would answer a different
	// question than the one asked.
	repos, err := s.savingsPlansRepositories(req.GetString("account_id", ""), false)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("failed to initialise aws client", err), nil
	}

	items := []savingsPlanDTO{}
	warnings := []string{}

	for _, repo := range repos {
		plans, err := listPlans(repo.repo, states)
		if err != nil {
			// One account failing must not sink the others, but it must be said:
			// a short inventory that looks complete is how an agent concludes
			// there is no coverage where there is.
			log.Warn().Err(err).Str("account", repo.accountID).Msg("[handleListSavingsPlans] describe savings plans failed")
			warnings = append(warnings, "account "+repo.accountID+": "+err.Error())

			continue
		}

		for _, plan := range plans {
			if !filter.matches(plan) {
				continue
			}

			items = append(items, buildPlanDTO(repo.accountID, plan, filter.now))
		}
	}

	if len(warnings) > 0 && len(items) == 0 {
		return mcp.NewToolResultErrorf("no account could be queried: %s", strings.Join(warnings, " | ")), nil
	}

	sortPlans(items)

	// The summary covers every matching plan; only the rows are capped, so a
	// truncated response still totals the real commitment.
	result := savingsPlansResult{
		Total:           len(items),
		Summary:         summarizePlans(items),
		AccountsQueried: savingsPlansAccountIDs(repos),
		Warnings:        warnings,
	}

	if len(items) > limit {
		items = items[:limit]
		result.Truncated = true
	}

	result.Items = items
	result.Count = len(items)

	if len(repos) > 1 {
		result.Note = fmt.Sprintf("plans from %d accounts are merged; each row's account_id says which account holds it", len(repos))
	}

	return jsonResult(result)
}

// listPlans reads one account's inventory, letting the API filter by state when
// states are named.
func listPlans(repo savingsPlansRepository, states []sptypes.SavingsPlanState) ([]sptypes.SavingsPlan, error) {
	if len(states) == 0 {
		return repo.ListSavingsPlansAll()
	}

	return repo.ListSavingsPlansByStates(states)
}

// savingsPlanRatesResult is the list_savings_plan_rates envelope. Query echoes
// the filters that were actually applied, including the defaults the caller did
// not set — without it, a narrow result looks like a narrow market.
type savingsPlanRatesResult struct {
	Items     []offeringRateDTO `json:"items"`
	Count     int               `json:"count"`
	Total     int               `json:"total"`
	Truncated bool              `json:"truncated,omitempty"`
	Query     rateQueryEcho     `json:"query"`
	AccountID string            `json:"queried_with_account,omitempty"`
	Note      string            `json:"note,omitempty"`
}

// rateQueryEcho is the effective query.
type rateQueryEcho struct {
	Region             string `json:"region"`
	Product            string `json:"product"`
	ServiceCode        string `json:"service_code"`
	InstanceType       string `json:"instance_type,omitempty"`
	InstanceFamily     string `json:"instance_family,omitempty"`
	PlanType           string `json:"plan_type,omitempty"`
	PaymentOption      string `json:"payment_option,omitempty"`
	Term               string `json:"term,omitempty"`
	ProductDescription string `json:"product_description,omitempty"`
	Tenancy            string `json:"tenancy,omitempty"`
}

func (s *Server) handleListSavingsPlanRates(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	region := strings.TrimSpace(req.GetString("region", ""))
	if region == "" {
		return mcp.NewToolResultErrorf("region is required; the Savings Plans price list is per region and an unfiltered lookup sweeps thousands of pages"), nil
	}

	if _, err := resolveRegions(region); err != nil {
		return mcp.NewToolResultErrorFromErr("invalid region", err), nil
	}

	instanceType := strings.TrimSpace(req.GetString("instance_type", ""))
	instanceFamily := strings.TrimSpace(req.GetString("instance_family", ""))

	if instanceType == "" && instanceFamily == "" {
		return mcp.NewToolResultErrorf("instance_type or instance_family is required; a region alone returns every instance type at every term and payment option"), nil
	}

	product, err := parseProduct(req.GetString("product", ""))
	if err != nil {
		return mcp.NewToolResultErrorFromErr("invalid product", err), nil
	}

	planType, err := parsePlanType(req.GetString("plan_type", ""))
	if err != nil {
		return mcp.NewToolResultErrorFromErr("invalid plan_type", err), nil
	}

	paymentOption, err := parsePaymentOption(req.GetString("payment_option", ""))
	if err != nil {
		return mcp.NewToolResultErrorFromErr("invalid payment_option", err), nil
	}

	term, err := parseTermSeconds(req.GetString("term", ""))
	if err != nil {
		return mcp.NewToolResultErrorFromErr("invalid term", err), nil
	}

	limit := clampSavingsLimit(req.GetInt("limit", 0), defaultRateLimit, maxRateLimit)

	// Linux/UNIX on shared tenancy is the overwhelmingly common case, and without
	// a default every lookup returns the same rate once per OS and tenancy
	// combination. "all" is the way back out, and the echoed query says which
	// defaults were applied.
	description := defaultedFilter(req.GetString("product_description", ""), "Linux/UNIX")
	tenancy := defaultedFilter(req.GetString("tenancy", ""), "shared")

	query := savingsplans.OfferingRatesQuery{
		Region:          ptypes.AwsRegion(region),
		Products:        []sptypes.SavingsPlanProductType{product.product},
		ServiceCodes:    []sptypes.SavingsPlanRateServiceCode{product.serviceCode},
		DurationSeconds: term,
	}

	if planType != "" {
		query.PlanTypes = []sptypes.SavingsPlanType{planType}
	}

	if paymentOption != "" {
		query.PaymentOptions = []sptypes.SavingsPlanPaymentOption{paymentOption}
	}

	if instanceType != "" {
		query.InstanceTypes = []string{instanceType}
	}

	if instanceFamily != "" {
		query.InstanceFamilies = []string{instanceFamily}
	}

	// Product description and tenancy are EC2 properties; sending them for
	// another product filters every rate away rather than being ignored.
	if product.product == sptypes.SavingsPlanProductTypeEc2 {
		if description != "" {
			query.ProductDescriptions = []string{description}
		}

		if tenancy != "" {
			query.Tenancies = []string{tenancy}
		}
	}

	// Rates are published prices, identical whichever account asks, so only one
	// repository is used — fanning out would return the same price list once per
	// account. They cache well, unlike inventory.
	repos, err := s.savingsPlansRepositories(req.GetString("account_id", ""), true)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("failed to initialise aws client", err), nil
	}

	repo := repos[0]

	rates, err := repo.repo.ListOfferingRatesByQuery(query)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("failed to list savings plan rates", err), nil
	}

	items := make([]offeringRateDTO, 0, len(rates))
	for _, rate := range rates {
		items = append(items, buildRateDTO(rate))
	}

	sortRates(items)

	result := savingsPlanRatesResult{
		Total:     len(items),
		AccountID: repo.accountID,
		Query: rateQueryEcho{
			Region:             region,
			Product:            string(product.product),
			ServiceCode:        string(product.serviceCode),
			InstanceType:       instanceType,
			InstanceFamily:     instanceFamily,
			PlanType:           string(planType),
			PaymentOption:      string(paymentOption),
			Term:               termName(term),
			ProductDescription: description,
			Tenancy:            tenancy,
		},
	}

	if len(items) > limit {
		items = items[:limit]
		result.Truncated = true
	}

	result.Items = items
	result.Count = len(items)

	if len(items) == 0 {
		result.Note = "no rates matched; check instance_type/instance_family exist in this region, and that product_description and tenancy fit the product (they apply to EC2 only)"
	}

	return jsonResult(result)
}

// defaultedFilter applies a default the caller can opt out of with "all".
func defaultedFilter(raw, fallback string) string {
	raw = strings.TrimSpace(raw)

	switch normalizeSPToken(raw) {
	case "":
		return fallback
	case "all", "any":
		return ""
	default:
		return raw
	}
}

// savingsPlansAccountIDs lists the accounts a set of repositories reports on.
func savingsPlansAccountIDs(repos []accountSavingsPlansRepository) []string {
	ids := make([]string, 0, len(repos))
	for _, repo := range repos {
		ids = append(ids, repo.accountID)
	}

	return ids
}
