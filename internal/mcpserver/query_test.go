package mcpserver

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ptypes "github.com/imunhatep/awslib/provider/types"
	"github.com/imunhatep/awslib/resources"
	"github.com/imunhatep/awslib/service"
	"github.com/imunhatep/awslib/service/ec2"

	"github.com/imunhatep/aws-mcp-go/pkg/errors"
)

// instance builds an ec2.Instance test entity with the given name, type and state.
func instance(name, itype, state string) ec2.Instance {
	return ec2.Instance{
		AbstractResource: service.AbstractResource{
			AccountID: ptypes.AwsAccountID("111111111111"),
			Region:    ptypes.AwsRegion("eu-central-1"),
			ID:        "i-" + name,
		},
		Instance: ec2types.Instance{
			InstanceId:   aws.String("i-" + name),
			InstanceType: ec2types.InstanceType(itype),
			State:        &ec2types.InstanceState{Name: ec2types.InstanceStateName(state)},
			Tags: []ec2types.Tag{
				{Key: aws.String("Name"), Value: aws.String(name)},
				{Key: aws.String("env"), Value: aws.String("prod")},
			},
		},
	}
}

func TestResourceFilter_State(t *testing.T) {
	f, err := parseFilter("", "running", "", "")
	require.NoError(t, err)

	run := instance("a", "m5.large", "running")
	stop := instance("b", "m5.large", "stopped")

	assert.True(t, f.matches(run, summaryAttributes(run)))
	assert.False(t, f.matches(stop, summaryAttributes(stop)))
}

func TestResourceFilter_TagAndAttribute(t *testing.T) {
	tagF, err := parseFilter("", "", "env=prod", "")
	require.NoError(t, err)
	r := instance("a", "m5.large", "running")
	assert.True(t, tagF.matches(r, summaryAttributes(r)))

	missF, _ := parseFilter("", "", "env=dev", "")
	assert.False(t, missF.matches(r, summaryAttributes(r)))

	attrF, err := parseFilter("", "", "", "instance_type=m5.large")
	require.NoError(t, err)
	assert.True(t, attrF.matches(r, summaryAttributes(r)))

	attrMiss, _ := parseFilter("", "", "", "instance_type=c5.large")
	assert.False(t, attrMiss.matches(r, summaryAttributes(r)))
}

func TestResourceFilter_Invalid(t *testing.T) {
	_, err := parseFilter("", "", "noequals", "")
	assert.Error(t, err)
	_, err = parseFilter("", "", "", "noequals")
	assert.Error(t, err)
}

func TestPaginate(t *testing.T) {
	items := make([]resourceDTO, 5)
	for i := range items {
		items[i] = resourceDTO{ID: string(rune('a' + i))}
	}

	page := paginate(items, 0, 2)
	assert.Equal(t, 5, page.Total)
	assert.Equal(t, 2, page.Count)
	assert.Equal(t, "2", page.NextCursor)
	assert.Equal(t, "a", page.Items[0].ID)

	last := paginate(items, 4, 2)
	assert.Equal(t, 1, last.Count)
	assert.Empty(t, last.NextCursor, "no next cursor on the final page")

	beyond := paginate(items, 99, 2)
	assert.Equal(t, 0, beyond.Count)
	assert.Empty(t, beyond.NextCursor)
}

func TestClampLimit(t *testing.T) {
	assert.Equal(t, defaultPageLimit, clampLimit(0))
	assert.Equal(t, defaultPageLimit, clampLimit(-5))
	assert.Equal(t, 10, clampLimit(10))
	assert.Equal(t, maxPageLimit, clampLimit(maxPageLimit+1))
}

func TestParseCursor(t *testing.T) {
	n, err := parseCursor("")
	require.NoError(t, err)
	assert.Equal(t, 0, n)

	n, err = parseCursor("42")
	require.NoError(t, err)
	assert.Equal(t, 42, n)

	_, err = parseCursor("-1")
	assert.Error(t, err)
	_, err = parseCursor("abc")
	assert.Error(t, err)
}

func TestAggregate_GroupByStateAndType(t *testing.T) {
	items := []service.ResourceInterface{
		instance("a", "m5.large", "running"),
		instance("b", "m5.large", "running"),
		instance("c", "c5.large", "running"),
		instance("d", "m5.large", "stopped"),
	}

	res := aggregate(items, resourceFilter{}, []string{"state", "attr:instance_type"})
	assert.Equal(t, 4, res.Total)

	// Top bucket must be running m5.large (count 2), sorted first by count desc.
	require.NotEmpty(t, res.Buckets)
	top := res.Buckets[0]
	assert.Equal(t, 2, top.Count)
	assert.Equal(t, "running", top.Group["state"])
	assert.Equal(t, "m5.large", top.Group["attr:instance_type"])
}

func TestAggregate_WithFilter(t *testing.T) {
	items := []service.ResourceInterface{
		instance("a", "m5.large", "running"),
		instance("b", "m5.large", "stopped"),
	}

	f, _ := parseFilter("", "running", "", "")
	res := aggregate(items, f, []string{"state"})
	assert.Equal(t, 1, res.Total, "filter must apply before aggregation")
	require.Len(t, res.Buckets, 1)
	assert.Equal(t, "running", res.Buckets[0].Group["state"])
}

func TestParseGroupBy(t *testing.T) {
	dims, err := parseGroupBy("")
	require.NoError(t, err)
	assert.Equal(t, []string{"state"}, dims, "defaults to state")

	dims, err = parseGroupBy("type, attr:instance_type , tag:env")
	require.NoError(t, err)
	assert.Equal(t, []string{"type", "attr:instance_type", "tag:env"}, dims)

	_, err = parseGroupBy("bogus")
	assert.Error(t, err)
}

func TestParseView(t *testing.T) {
	for _, in := range []string{"", "id", "ID"} {
		v, ok := parseView(in)
		assert.True(t, ok)
		assert.Equal(t, viewID, v)
	}
	v, ok := parseView("summary")
	assert.True(t, ok)
	assert.Equal(t, viewSummary, v)

	v, ok = parseView("raw")
	assert.True(t, ok)
	assert.Equal(t, viewDetail, v)

	_, ok = parseView("bogus")
	assert.False(t, ok)
}

// TestTypedFetchWarnings pins the signal that was missing from list_resources
// and count_resources: a region that could not be queried contributes no rows,
// and without this warning that is indistinguishable from a region that holds
// nothing.
func TestTypedFetchWarnings(t *testing.T) {
	t.Run("silent when everything answered", func(t *testing.T) {
		fetched := typedFetch{scope: queryScope{Accounts: 3, Regions: 10}}

		assert.Nil(t, fetched.warnings(), "a complete sweep needs no qualification")
	})

	t.Run("names the unreachable pairs", func(t *testing.T) {
		fetched := typedFetch{
			scope: queryScope{Accounts: 2, Regions: 3},
			failures: []resources.ProxyFailure{
				{AccountID: "111111111111", Region: "me-south-1", Err: errors.New("region not enabled")},
			},
		}

		got := fetched.warnings()
		require.Len(t, got, 1)
		assert.Contains(t, got[0], "1 of 6 account/region pairs could not be queried")
		assert.Contains(t, got[0], "111111111111/me-south-1")
		assert.Contains(t, got[0], "region not enabled", "the reason is what makes the warning actionable")
	})
}
