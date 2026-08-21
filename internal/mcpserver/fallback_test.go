package mcpserver

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/arn"
	cctypes "github.com/aws/aws-sdk-go-v2/service/cloudcontrol/types"
	awscfg "github.com/aws/aws-sdk-go-v2/service/configservice/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ptypes "github.com/imunhatep/awslib/provider/types"
	"github.com/imunhatep/awslib/service"
	"github.com/imunhatep/awslib/service/cloudcontrol"
)

// sampleFallbackResource builds a generic Cloud Control resource the way awslib
// would after listing an arbitrary type.
func sampleFallbackResource(region, resourceArn string, properties map[string]any) cloudcontrol.Resource {
	return cloudcontrol.Resource{
		AbstractResource: service.AbstractResource{
			AccountID: ptypes.AwsAccountID("111111111111"),
			Region:    ptypes.AwsRegion(region),
			ID:        "res-1",
			ARN:       parseArnOrNil(resourceArn),
			Type:      awscfg.ResourceTypeQueue,
		},
		ResourceDescription: cctypes.ResourceDescription{Identifier: aws.String("res-1")},
		Attributes:          properties,
		Tags:                map[string]string{"Name": "generic-1"},
	}
}

// parseArnOrNil mirrors what awslib does when a resource has no usable ARN.
func parseArnOrNil(raw string) *arn.ARN {
	parsed, err := arn.Parse(raw)
	if err != nil {
		return nil
	}

	return &parsed
}

func TestResolveFallbackResourceType(t *testing.T) {
	tests := map[string]struct {
		raw     string
		want    awscfg.ResourceType
		wantErr bool
	}{
		"canonical":                     {"AWS::EC2::Instance", awscfg.ResourceTypeInstance, false},
		"canonical is case insensitive": {"aws::ec2::instance", awscfg.ResourceTypeInstance, false},
		"url form":                      {"aws_ec2_instance", awscfg.ResourceTypeInstance, false},
		"surrounding space is trimmed":  {"  AWS::EC2::Instance  ", awscfg.ResourceTypeInstance, false},

		// Not in the Config vocabulary, so it cannot be canonicalized — but it
		// is a valid Cloud Control type and must pass through untouched.
		"unknown but well formed passes through": {"AWS::Kinesis::Stream", awscfg.ResourceType("AWS::Kinesis::Stream"), false},
		"third party type passes through":        {"Datadog::Monitors::Monitor", awscfg.ResourceType("Datadog::Monitors::Monitor"), false},

		"empty":            {"", "", true},
		"not a type name":  {"ec2 instances please", "", true},
		"too few segments": {"AWS::EC2", "", true},
		"trailing colons":  {"AWS::EC2::", "", true},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := ResolveFallbackResourceType(tc.raw)

			if tc.wantErr {
				assert.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestResolveFallbackResourceTypeAcceptsUnsupportedTypes is the point of the
// whole feature: a type ResolveResourceType rejects must still resolve here.
func TestResolveFallbackResourceTypeAcceptsUnsupportedTypes(t *testing.T) {
	const raw = "AWS::CloudFront::Distribution"

	_, ok := ResolveResourceType(raw)
	require.False(t, ok, "precondition: the typed path must not support this type")

	rt, err := ResolveFallbackResourceType(raw)
	require.NoError(t, err)
	assert.Equal(t, awscfg.ResourceType(raw), rt)
}

func TestSnakeCase(t *testing.T) {
	tests := map[string]string{
		"BucketName":        "bucket_name",
		"VpcId":             "vpc_id",
		"ARN":               "arn",
		"DBInstanceClass":   "db_instance_class",
		"S3Bucket":          "s3_bucket",
		"ObjectLockEnabled": "object_lock_enabled",
		"State":             "state",
		"Status":            "status",
		"":                  "",
		"already_snake":     "already_snake",
	}

	for in, want := range tests {
		t.Run(in, func(t *testing.T) {
			assert.Equal(t, want, snakeCase(in))
		})
	}
}

func TestGenericAttributes(t *testing.T) {
	attrs := genericAttributes(map[string]any{
		"BucketName":              "example",
		"ObjectLockEnabled":       false,
		"ReplicaCount":            float64(3),
		"Status":                  "ACTIVE",
		"VersioningConfiguration": map[string]any{"Status": "Enabled"},
		"NotificationList":        []any{"a", "b"},
		"AccelerateConfiguration": nil,
		"Tags":                    []any{map[string]any{"Key": "env", "Value": "prod"}},
	})

	require.NotNil(t, attrs)

	// scalars survive, with snake_case keys
	assert.Equal(t, "example", attrs["bucket_name"])
	assert.Equal(t, false, attrs["object_lock_enabled"])
	assert.Equal(t, float64(3), attrs["replica_count"])
	assert.Equal(t, "ACTIVE", attrs["status"])

	// containers, nils and Tags are dropped — raw carries them under view=detail
	assert.NotContains(t, attrs, "versioning_configuration")
	assert.NotContains(t, attrs, "notification_list")
	assert.NotContains(t, attrs, "accelerate_configuration")
	assert.NotContains(t, attrs, "tags")
}

func TestGenericAttributesEmpty(t *testing.T) {
	assert.Nil(t, genericAttributes(nil))
	assert.Nil(t, genericAttributes(map[string]any{}))
	assert.Nil(t, genericAttributes(map[string]any{"Nested": map[string]any{"a": 1}}),
		"a bag with no scalars yields nil rather than an empty map")
}

// TestSummaryAttributesFallsBackToAttributeProvider is the seam that makes the
// whole feature work: one interface case in summaryAttributes gives every
// fallback type the state/tag/attribute filters and the group_by dimensions,
// with no per-type code.
func TestSummaryAttributesFallsBackToAttributeProvider(t *testing.T) {
	r := sampleFallbackResource("eu-central-1", "arn:aws:sqs:eu-central-1:111111111111:q", map[string]any{
		"QueueName":         "q",
		"State":             "ACTIVE",
		"VisibilityTimeout": float64(30),
	})

	attrs := summaryAttributes(r)
	require.NotNil(t, attrs, "a resource carrying its own attributes must not fall through to nil")

	assert.Equal(t, "q", attrs["queue_name"])
	assert.Equal(t, float64(30), attrs["visibility_timeout"])

	// state must be liftable so the thin view and the state filter work
	assert.Equal(t, "ACTIVE", resourceState(attrs))
	assert.Equal(t, "ACTIVE", toResourceDTO(r, viewID).State)
}

// TestFallbackResourceFiltersAndGroups pins that the existing filter and
// group_by machinery works on a fallback resource unchanged.
func TestFallbackResourceFiltersAndGroups(t *testing.T) {
	r := sampleFallbackResource("eu-central-1", "arn:aws:sqs:eu-central-1:111111111111:q", map[string]any{
		"State":        "ACTIVE",
		"ContentBased": true,
	})
	attrs := summaryAttributes(r)

	f, err := parseFilter("", "active", "Name=generic-1", "state=ACTIVE")
	require.NoError(t, err)
	assert.True(t, f.matches(r, attrs), "state, tag and attribute filters must all match a fallback resource")

	miss, err := parseFilter("", "deleting", "", "")
	require.NoError(t, err)
	assert.False(t, miss.matches(r, attrs))

	res := aggregate([]service.ResourceInterface{r}, resourceFilter{}, []string{"state"})
	require.Len(t, res.Buckets, 1)
	assert.Equal(t, 1, res.Buckets[0].Count)
}

// TestDedupeFallbackCollapsesGlobalDuplicates covers the case the typed path
// avoids via its curated global-type list: an arbitrary global type is fetched
// once per region, and every row comes back with the same (region-less) ARN.
func TestDedupeFallbackCollapsesGlobalDuplicates(t *testing.T) {
	const globalArn = "arn:aws:iam::111111111111:role/admin"

	items := []service.ResourceInterface{
		sampleFallbackResource("eu-central-1", globalArn, nil),
		sampleFallbackResource("us-east-1", globalArn, nil),
		sampleFallbackResource("eu-west-1", globalArn, nil),
	}

	out, duplicates := dedupeFallback(items)

	assert.Len(t, out, 1, "the same global resource seen from three regions is one resource")
	assert.Equal(t, 2, duplicates)
}

// TestDedupeFallbackKeepsDistinctRegionalResources is the other half: two
// genuinely different resources must never be merged.
func TestDedupeFallbackKeepsDistinctRegionalResources(t *testing.T) {
	items := []service.ResourceInterface{
		sampleFallbackResource("eu-central-1", "arn:aws:sqs:eu-central-1:111111111111:q", nil),
		sampleFallbackResource("us-east-1", "arn:aws:sqs:us-east-1:111111111111:q", nil),
	}

	out, duplicates := dedupeFallback(items)

	assert.Len(t, out, 2, "regional ARNs differ per region, so both rows are real")
	assert.Zero(t, duplicates)
}

// TestDedupeFallbackWithoutArnKeepsSameIdInDifferentRegions guards the
// conservative branch: with no ARN there is no way to tell a global duplicate
// from two same-named regional resources, so nothing is merged across regions.
func TestDedupeFallbackWithoutArnKeepsSameIdInDifferentRegions(t *testing.T) {
	items := []service.ResourceInterface{
		sampleFallbackResource("eu-central-1", "", nil),
		sampleFallbackResource("us-east-1", "", nil),
		sampleFallbackResource("us-east-1", "", nil), // a true duplicate
	}

	out, duplicates := dedupeFallback(items)

	assert.Len(t, out, 2)
	assert.Equal(t, 1, duplicates)
}

func TestFallbackWarnings(t *testing.T) {
	t.Run("duplicates are reported", func(t *testing.T) {
		got := fallbackWarnings(awscfg.ResourceType("AWS::Kinesis::Stream"), "AWS::Kinesis::Stream", queryScope{}, 4)
		assert.Contains(t, joined(got), "collapsed 4 duplicate")
	})

	t.Run("steers callers to the typed tool for supported types", func(t *testing.T) {
		got := fallbackWarnings(awscfg.ResourceTypeInstance, "AWS::EC2::Instance", queryScope{}, 0)
		assert.Contains(t, joined(got), "list_resources returns richer")
	})

	t.Run("no steer for an unsupported type", func(t *testing.T) {
		got := fallbackWarnings(awscfg.ResourceType("AWS::Kinesis::Stream"), "AWS::Kinesis::Stream", queryScope{}, 0)
		assert.NotContains(t, joined(got), "list_resources returns richer")
	})

	t.Run("normalization is disclosed", func(t *testing.T) {
		got := fallbackWarnings(awscfg.ResourceTypeInstance, "aws::ec2::instance", queryScope{}, 0)
		assert.Contains(t, joined(got), "normalized to")
	})

	t.Run("detail cost is disclosed", func(t *testing.T) {
		got := fallbackWarnings(awscfg.ResourceType("AWS::Kinesis::Stream"), "AWS::Kinesis::Stream", queryScope{Detailed: true}, 0)
		assert.Contains(t, joined(got), "one extra GetResource call")
	})

	t.Run("always states the backend", func(t *testing.T) {
		got := fallbackWarnings(awscfg.ResourceType("AWS::Kinesis::Stream"), "AWS::Kinesis::Stream", queryScope{}, 0)
		assert.NotEmpty(t, got)
		assert.Contains(t, joined(got), "Cloud Control API")
	})
}

func joined(warnings []string) string {
	out := ""
	for _, w := range warnings {
		out += w + "\n"
	}
	return out
}
