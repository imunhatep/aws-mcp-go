package mcpserver

import (
	"context"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server/servertest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/imunhatep/aws-mcp-go/skills"
)

// toolNamePattern matches identifiers shaped like this server's tool names.
// Every tool starts with one of these verbs, so a mention of a renamed or
// removed tool in the guide matches and is caught.
var toolNamePattern = regexp.MustCompile(`\b(?:list|count|get|aws|create|update|delete|tag|untag|cancel)_[a-z_]+\b`)

func registeredTools(t *testing.T) map[string]bool {
	t.Helper()

	// Read-write mode registers a superset of read-only mode, and the guide
	// documents both.
	srv := NewServer(context.Background(), nil, nil, EnableWrites())

	names := map[string]bool{}
	for name := range srv.MCPServer().ListTools() {
		names[name] = true
	}

	require.NotEmpty(t, names)

	return names
}

// A tool the guide never mentions is a tool agents learn about only from its
// one-paragraph description. Adding one means documenting it in
// skills/using-aws-mcp/SKILL.md, in the same change.
func TestUsageGuideCoversEveryTool(t *testing.T) {
	missing := []string{}

	for name := range registeredTools(t) {
		if !strings.Contains(skills.UsingAWSMCP, "`"+name+"`") {
			missing = append(missing, name)
		}
	}

	sort.Strings(missing)
	assert.Empty(t, missing, "tools not documented in skills/using-aws-mcp/SKILL.md — add them to the tool map at least")
}

// The reverse: a guide that recommends a tool which no longer exists sends the
// agent into an error. Renaming or removing a tool means updating the guide.
func TestUsageGuideNamesOnlyRealTools(t *testing.T) {
	tools := registeredTools(t)

	for name, text := range map[string]string{
		"skills/using-aws-mcp/SKILL.md": skills.UsingAWSMCP,
		"serverInstructions":            serverInstructions,
	} {
		unknown := map[string]bool{}

		for _, mention := range toolNamePattern.FindAllString(text, -1) {
			if !tools[mention] {
				unknown[mention] = true
			}
		}

		assert.Empty(t, unknown, "%s names tools this server does not register", name)
	}
}

func TestUsageGuideIsALoadableSkill(t *testing.T) {
	// Claude Code reads name and description from the frontmatter; without
	// them the file stops working as a skill.
	require.True(t, strings.HasPrefix(skills.UsingAWSMCP, "---\nname: using-aws-mcp\ndescription: "))

	body := guideBody(skills.UsingAWSMCP)
	assert.True(t, strings.HasPrefix(body, "# Using the aws MCP server"), "the tool output starts at the title, without frontmatter")
	assert.NotContains(t, body, "\nname: using-aws-mcp")
}

// The instructions are in every session's system prompt. They must carry the
// one rule agents kept missing, point at the full guide, and stay short.
func TestServerInstructions(t *testing.T) {
	assert.Contains(t, serverInstructions, "list_resources_fallback")
	assert.Contains(t, serverInstructions, "aws_usage_guide")
	assert.Contains(t, serverInstructions, "warnings")
	assert.Contains(t, serverInstructions, "account_id")
	assert.Less(t, len(serverInstructions), 2000, "server instructions cost context in every session; move detail into the guide")
}

func TestServerInstructionsReachTheClient(t *testing.T) {
	srv := NewServer(context.Background(), nil, nil)
	testServer := servertest.NewTestStreamableHTTPServer(srv.MCPServer())
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

	res, err := c.Initialize(ctx, initReq)
	require.NoError(t, err)
	assert.Equal(t, serverInstructions, res.Instructions)
}

func callGuide(t *testing.T, args map[string]any) (*mcp.CallToolResult, string) {
	t.Helper()

	srv := NewServer(context.Background(), nil, nil)

	req := mcp.CallToolRequest{}
	req.Params.Name = "aws_usage_guide"
	req.Params.Arguments = args

	res, err := srv.handleUsageGuide(context.Background(), req)
	require.NoError(t, err)
	require.NotEmpty(t, res.Content)

	text, ok := res.Content[0].(mcp.TextContent)
	require.True(t, ok)

	return res, text.Text
}

func TestUsageGuideToolReturnsWholeGuide(t *testing.T) {
	res, text := callGuide(t, nil)
	require.False(t, res.IsError)
	assert.Equal(t, guideBody(skills.UsingAWSMCP), text)
}

func TestUsageGuideToolReturnsOneSection(t *testing.T) {
	cases := map[string]string{
		"fallback":        "### `list_resources_fallback` — everything else",
		"Cost-Explorer":   "## Cost Explorer",
		"forecast":        "### `get_cost_forecast`",
		"savings_plans":   "## Savings Plans",
		"reading results": "## Reading results",
		"auth":            "## Auth and troubleshooting",
	}

	for topic, heading := range cases {
		t.Run(topic, func(t *testing.T) {
			res, text := callGuide(t, map[string]any{"topic": topic})
			require.False(t, res.IsError, text)
			assert.True(t, strings.HasPrefix(text, heading+"\n"), "got: %.80s", text)
		})
	}

	// A ## section includes its ### subsections; a ### section stops at the next
	// heading of its level or above.
	_, costs := callGuide(t, map[string]any{"topic": "cost explorer"})
	assert.Contains(t, costs, "### `get_cost_forecast`")
	assert.NotContains(t, costs, "## Savings Plans")

	_, fallback := callGuide(t, map[string]any{"topic": "fallback"})
	assert.NotContains(t, fallback, "## Reading results")
}

func TestUsageGuideToolUnknownTopicListsSections(t *testing.T) {
	res, text := callGuide(t, map[string]any{"topic": "kubernetes"})
	assert.True(t, res.IsError)
	assert.Contains(t, text, "Cost Explorer")
	assert.Contains(t, text, "Auth and troubleshooting")
}

func TestGuideSectionsIgnoreHeadingsInCodeFences(t *testing.T) {
	doc := "# T\n\n## One\n```\n## not a heading\n```\ntext\n## Two\nmore\n"

	sections := guideSections(doc)
	require.Len(t, sections, 2)
	assert.Equal(t, "One", sections[0].title)
	assert.Contains(t, sections[0].body, "## not a heading")
	assert.Equal(t, "Two", sections[1].title)
}

// The error for an uncurated type is where agents used to give up: it pointed
// only at list_resource_types, which by definition does not contain the type.
func TestUnsupportedTypeNamesTheFallback(t *testing.T) {
	srv := NewServer(context.Background(), nil, nil)

	for _, handler := range []func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error){
		srv.handleListResources,
		srv.handleCountResources,
	} {
		req := mcp.CallToolRequest{}
		req.Params.Arguments = map[string]any{"resource_type": "AWS::EC2::NatGateway"}

		// A nil pool is the tripwire: rejecting the type must not touch AWS.
		res, err := handler(context.Background(), req)
		require.NoError(t, err)
		require.True(t, res.IsError)

		text := res.Content[0].(mcp.TextContent).Text
		assert.Contains(t, text, "list_resources_fallback")
		assert.Contains(t, text, `resource_type="AWS::EC2::NatGateway"`)
	}
}
