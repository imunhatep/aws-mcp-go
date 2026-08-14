package mcpserver

import (
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ptypes "github.com/imunhatep/awslib/provider/types"
	"github.com/imunhatep/awslib/service"
	"github.com/imunhatep/awslib/service/ec2"
)

// sampleInstance builds an ec2.Instance entity carrying the full SDK struct, as
// awslib would after fetching it.
func sampleInstance() ec2.Instance {
	return ec2.Instance{
		AbstractResource: service.AbstractResource{
			AccountID: ptypes.AwsAccountID("111111111111"),
			Region:    ptypes.AwsRegion("eu-north-1"),
			ID:        "i-0123456789abcdef0",
		},
		Instance: ec2types.Instance{
			InstanceId:       aws.String("i-0123456789abcdef0"),
			InstanceType:     ec2types.InstanceType("m5.2xlarge"),
			PrivateIpAddress: aws.String("10.0.0.5"),
			State:            &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
			Tags: []ec2types.Tag{
				{Key: aws.String("Name"), Value: aws.String("web-1")},
			},
		},
	}
}

// TestToResourceDTO_IDView verifies the thin default view: identity + state,
// but no tags, attributes or raw blob.
func TestToResourceDTO_IDView(t *testing.T) {
	dto := toResourceDTO(sampleInstance(), viewID)

	assert.Equal(t, "web-1", dto.Name)
	assert.Equal(t, "running", dto.State, "state must be lifted into the thin view")
	assert.Nil(t, dto.Attributes, "attributes must be omitted in the id view")
	assert.Nil(t, dto.Tags, "tags must be omitted in the id view")
	assert.Nil(t, dto.Raw, "raw must be omitted in the id view")
}

// TestToResourceDTO_SummaryView verifies the summary view surfaces the curated
// attributes and tags but no raw blob.
func TestToResourceDTO_SummaryView(t *testing.T) {
	dto := toResourceDTO(sampleInstance(), viewSummary)

	assert.Equal(t, "running", dto.State)
	assert.Nil(t, dto.Raw, "raw must be omitted in the summary view")

	require.NotNil(t, dto.Attributes, "attributes must be populated in the summary view")
	assert.Equal(t, "m5.2xlarge", dto.Attributes["instance_type"])
	assert.Equal(t, "m5", dto.Attributes["instance_family"])
	assert.Equal(t, "running", dto.Attributes["state"])
	assert.Equal(t, "10.0.0.5", dto.Attributes["private_ip"])
	// Empty optional fields must be omitted, not set to "".
	assert.NotContains(t, dto.Attributes, "public_ip")
}

// TestToResourceDTO_DetailIncludesFullEntity verifies that the detail view embeds
// the full provider-native attributes (instance type, state, IPs) that the
// normalized DTO fields do not surface — generically, via marshalling the
// concrete entity.
func TestToResourceDTO_DetailIncludesFullEntity(t *testing.T) {
	dto := toResourceDTO(sampleInstance(), viewDetail)

	require.NotNil(t, dto.Raw, "raw must be populated in the detail view")

	var raw map[string]any
	require.NoError(t, json.Unmarshal(dto.Raw, &raw))

	assert.Equal(t, "m5.2xlarge", raw["InstanceType"], "instance type must survive in the raw detail")
	assert.Equal(t, "10.0.0.5", raw["PrivateIpAddress"])

	state, ok := raw["State"].(map[string]any)
	require.True(t, ok, "State must be present in raw detail")
	assert.Equal(t, "running", state["Name"])
}
