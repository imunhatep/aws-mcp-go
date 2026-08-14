package mcpserver_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/imunhatep/aws-mcp-go/internal/mcpserver"
)

// startTestServer spins up the MCP server behind an in-process streamable-HTTP
// test transport and returns a connected, initialized client. A nil ClientPool
// is fine here: the tools exercised (list_resource_types, list_regions) never
// touch AWS.
func startTestServer(t *testing.T) (*client.Client, context.Context) {
	t.Helper()

	srv := mcpserver.NewServer(context.Background(), nil, nil)
	testServer := server.NewTestStreamableHTTPServer(srv.MCPServer())
	t.Cleanup(testServer.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	c, err := client.NewStreamableHttpClient(testServer.URL + "/mcp")
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	require.NoError(t, c.Start(ctx))

	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "test-client", Version: "1.0.0"}
	_, err = c.Initialize(ctx, initReq)
	require.NoError(t, err)

	return c, ctx
}

func TestListTools(t *testing.T) {
	c, ctx := startTestServer(t)

	res, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	require.NoError(t, err)

	names := map[string]bool{}
	for _, tool := range res.Tools {
		names[tool.Name] = true
	}

	for _, want := range []string{
		"list_resource_types", "list_regions", "list_accounts", "list_resources", "count_resources",
		"get_cost_and_usage", "get_cost_forecast", "list_cost_dimension_values", "list_cost_dimensions",
	} {
		assert.Truef(t, names[want], "expected tool %q to be registered", want)
	}
}

func TestListCostDimensionsTool(t *testing.T) {
	c, ctx := startTestServer(t)

	req := mcp.CallToolRequest{}
	req.Params.Name = "list_cost_dimensions"

	res, err := c.CallTool(ctx, req)
	require.NoError(t, err)
	require.False(t, res.IsError, "tool returned an error result")
	require.Len(t, res.Content, 1)

	text, ok := res.Content[0].(mcp.TextContent)
	require.True(t, ok, "expected text content")

	var vocabulary struct {
		Dimensions       []string          `json:"dimensions"`
		DimensionAliases map[string]string `json:"dimension_aliases"`
		Metrics          []string          `json:"metrics"`
		Periods          []string          `json:"periods"`
		MatchOptions     []string          `json:"match_options"`
	}
	require.NoError(t, json.Unmarshal([]byte(text.Text), &vocabulary))

	assert.Contains(t, vocabulary.Dimensions, "SERVICE")
	assert.Contains(t, vocabulary.Dimensions, "LINKED_ACCOUNT")
	assert.Equal(t, "LINKED_ACCOUNT", vocabulary.DimensionAliases["account"])
	assert.Contains(t, vocabulary.Metrics, "UnblendedCost")
	assert.Contains(t, vocabulary.MatchOptions, "EQUALS")
	assert.NotEmpty(t, vocabulary.Periods)
}

// Bad arguments must be rejected before any AWS call, both so the caller gets a
// usable message and so a query that cannot succeed is not billed once per
// account. The nil ClientPool here would panic if a handler reached for it.
func TestCostToolsValidateBeforeCallingAWS(t *testing.T) {
	c, ctx := startTestServer(t)

	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"get_cost_and_usage", map[string]any{"granularity": "WEEKLY"}, "granularity"},
		{"get_cost_and_usage", map[string]any{"period": "since_forever"}, "unknown period"},
		{"get_cost_and_usage", map[string]any{"metrics": []any{"MonopolyMoney"}}, "unknown metric"},
		{"get_cost_and_usage", map[string]any{"group_by": []any{"SERVICE", "REGION", "TAG:Team"}}, "at most 2"},
		{"get_cost_and_usage", map[string]any{"filters": []any{map[string]any{"key": "Colour", "type": "dimension", "values": []any{"blue"}}}}, "unknown cost dimension"},
		// CONTAINS is a valid Expression operator, but GetCostAndUsage rejects it
		// on dimensions — catching it here saves a round trip.
		{"get_cost_and_usage", map[string]any{"filters": []any{map[string]any{"key": "SERVICE", "values": []any{"x"}, "match_options": []any{"CONTAINS"}}}}, "CONTAINS"},
		{"get_cost_forecast", map[string]any{"granularity": "HOURLY"}, "HOURLY"},
		{"get_cost_forecast", map[string]any{"prediction_interval_level": 10}, "between 51 and 99"},
		{"get_cost_forecast", map[string]any{"period": "last_month"}, "unknown forecast period"},
		{"list_cost_dimension_values", map[string]any{"dimension": "Colour"}, "unknown dimension"},
	}

	for _, tc := range cases {
		req := mcp.CallToolRequest{}
		req.Params.Name = tc.name
		req.Params.Arguments = tc.args

		res, err := c.CallTool(ctx, req)
		require.NoErrorf(t, err, "%s %v", tc.name, tc.args)
		require.Truef(t, res.IsError, "%s %v should be rejected", tc.name, tc.args)
		require.Len(t, res.Content, 1)

		text, ok := res.Content[0].(mcp.TextContent)
		require.True(t, ok)
		assert.Containsf(t, text.Text, tc.want, "%s %v", tc.name, tc.args)
	}
}

func TestListResourceTypesTool(t *testing.T) {
	c, ctx := startTestServer(t)

	req := mcp.CallToolRequest{}
	req.Params.Name = "list_resource_types"

	res, err := c.CallTool(ctx, req)
	require.NoError(t, err)
	require.False(t, res.IsError, "tool returned an error result")
	require.Len(t, res.Content, 1)

	text, ok := res.Content[0].(mcp.TextContent)
	require.True(t, ok, "expected text content")

	var infos []struct {
		Type   string `json:"type"`
		URL    string `json:"url"`
		Global bool   `json:"global"`
	}
	require.NoError(t, json.Unmarshal([]byte(text.Text), &infos))
	assert.NotEmpty(t, infos)

	// Spot-check a well-known regional and global type.
	byType := map[string]bool{}
	for _, i := range infos {
		byType[i.Type] = i.Global
	}
	assert.Contains(t, byType, "AWS::EC2::Instance")
	assert.False(t, byType["AWS::EC2::Instance"], "EC2 instance should be regional")
	assert.Contains(t, byType, "AWS::IAM::User")
	assert.True(t, byType["AWS::IAM::User"], "IAM user should be global")
}

func TestListRegionsTool(t *testing.T) {
	c, ctx := startTestServer(t)

	req := mcp.CallToolRequest{}
	req.Params.Name = "list_regions"

	res, err := c.CallTool(ctx, req)
	require.NoError(t, err)
	require.False(t, res.IsError)
	require.Len(t, res.Content, 1)

	text, ok := res.Content[0].(mcp.TextContent)
	require.True(t, ok)

	var regions []struct {
		Region      string `json:"region"`
		Description string `json:"description"`
	}
	require.NoError(t, json.Unmarshal([]byte(text.Text), &regions))
	assert.NotEmpty(t, regions)
}

func TestResolveResourceType(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"AWS::EC2::Instance", "AWS::EC2::Instance", true},
		{"aws::ec2::instance", "AWS::EC2::Instance", true},
		{"aws_ec2_instance", "AWS::EC2::Instance", true},
		{"AWS::S3::Bucket", "AWS::S3::Bucket", true},
		{"AWS::CloudTrail::Trail", "", false}, // in registry but not wired into FindAll
		{"not-a-type", "", false},
	}

	for _, tc := range cases {
		got, ok := mcpserver.ResolveResourceType(tc.in)
		assert.Equalf(t, tc.ok, ok, "ResolveResourceType(%q) ok", tc.in)
		if tc.ok {
			assert.Equalf(t, tc.want, string(got), "ResolveResourceType(%q)", tc.in)
		}
	}
}
