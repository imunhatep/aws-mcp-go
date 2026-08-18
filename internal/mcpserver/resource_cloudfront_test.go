package mcpserver

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	cftypes "github.com/aws/aws-sdk-go-v2/service/cloudfront/types"
	awscfg "github.com/aws/aws-sdk-go-v2/service/configservice/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ptypes "github.com/imunhatep/awslib/provider/types"
	"github.com/imunhatep/awslib/service"
	ccfg "github.com/imunhatep/awslib/service/cfg"
	"github.com/imunhatep/awslib/service/cloudfront"
)

// TestResolveResourceType_CloudFront pins the CloudFront SaaS Manager types
// into the allowlist. awslib's proxy.RepoProxy.FindAll dispatches both, so
// dropping them from SupportedResourceTypes would make list_resources reject a
// type the library can actually fetch.
func TestResolveResourceType_CloudFront(t *testing.T) {
	for _, raw := range []string{
		"AWS::CloudFront::DistributionTenantSummary",
		"aws_cloudfront_distributiontenantsummary",
		"AWS::CloudFront::ConnectionGroup",
		"aws_cloudfront_connectiongroup",
	} {
		rt, ok := ResolveResourceType(raw)
		require.Truef(t, ok, "expected %q to resolve", raw)
		assert.NotEmpty(t, rt)
	}

	// The full tenant entity is not wired into FindAll — it must stay rejected.
	_, ok := ResolveResourceType(string(ccfg.ResourceTypeCloudFrontDistributionTenant))
	assert.False(t, ok, "full DistributionTenant is not listable")
}

// TestSupportedResourceTypes_CloudFrontIsGlobal guards the flag callers use to
// know a type is not per-region; awslib's RepoProxyPool.List relies on the same
// classification to query one proxy per account instead of fanning out.
func TestSupportedResourceTypes_CloudFrontIsGlobal(t *testing.T) {
	seen := map[string]bool{}
	for _, info := range supportedResourceTypeInfos() {
		seen[info.Type] = info.Global
	}

	for _, rt := range []awscfg.ResourceType{
		ccfg.ResourceTypeCloudFrontDistributionTenantSummary,
		ccfg.ResourceTypeCloudFrontConnectionGroup,
	} {
		global, listed := seen[string(rt)]
		require.Truef(t, listed, "%s missing from list_resource_types", rt)
		assert.Truef(t, global, "%s should be reported as global", rt)
	}
}

func TestSummaryAttributes_DistributionTenantSummary(t *testing.T) {
	e := cloudfront.DistributionTenantSummary{
		AbstractResource: service.AbstractResource{
			AccountID: ptypes.AwsAccountID("111111111111"),
			Region:    ptypes.AwsRegion("us-east-1"),
			ID:        "dt_abc",
			Type:      ccfg.ResourceTypeCloudFrontDistributionTenantSummary,
		},
		DistributionTenantSummary: cftypes.DistributionTenantSummary{
			Id:                aws.String("dt_abc"),
			Name:              aws.String("tenant-one"),
			Status:            aws.String("Deployed"),
			Enabled:           aws.Bool(true),
			DistributionId:    aws.String("EDFDVBD632BHDS5"),
			ConnectionGroupId: aws.String("cg_default"),
			Domains: []cftypes.DomainResult{
				{Domain: aws.String("shop.example.com"), Status: cftypes.DomainStatusActive},
			},
			Customizations: &cftypes.Customizations{
				Certificate: &cftypes.Certificate{Arn: aws.String("arn:aws:acm:us-east-1:111111111111:certificate/xyz")},
			},
		},
	}

	attrs := summaryAttributes(e)
	require.NotNil(t, attrs, "tenant summaries must have curated attributes")

	assert.Equal(t, "Deployed", attrs["state"])
	assert.Equal(t, true, attrs["enabled"])
	assert.Equal(t, "EDFDVBD632BHDS5", attrs["distribution_id"])
	assert.Equal(t, "cg_default", attrs["connection_group_id"])
	assert.Equal(t, []string{"shop.example.com"}, attrs["domains"])
	assert.Equal(t, true, attrs["domains_active"])
	assert.Equal(t, "arn:aws:acm:us-east-1:111111111111:certificate/xyz", attrs["certificate_arn"])

	// state must survive into the thin default view so it stays filterable.
	assert.Equal(t, "Deployed", toResourceDTO(e, viewID).State)
}

func TestSummaryAttributes_ConnectionGroup(t *testing.T) {
	e := cloudfront.ConnectionGroup{
		AbstractResource: service.AbstractResource{
			AccountID: ptypes.AwsAccountID("111111111111"),
			Region:    ptypes.AwsRegion("us-east-1"),
			ID:        "cg_default",
			Type:      ccfg.ResourceTypeCloudFrontConnectionGroup,
		},
		ConnectionGroup: cftypes.ConnectionGroup{
			Id:              aws.String("cg_default"),
			Name:            aws.String("default"),
			Status:          aws.String("Deployed"),
			Enabled:         aws.Bool(true),
			IsDefault:       aws.Bool(true),
			RoutingEndpoint: aws.String("d1234.cloudfront.net"),
		},
	}

	attrs := summaryAttributes(e)
	require.NotNil(t, attrs)

	assert.Equal(t, "Deployed", attrs["state"])
	assert.Equal(t, true, attrs["enabled"])
	assert.Equal(t, true, attrs["is_default"])
	assert.Equal(t, "d1234.cloudfront.net", attrs["routing_endpoint"])
}
