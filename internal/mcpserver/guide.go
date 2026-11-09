package mcpserver

import (
	"context"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/imunhatep/aws-mcp-go/skills"
)

// The usage guide is how an agent learns the parts of this server that tool
// descriptions are too short to carry — above all that a type missing from
// list_resource_types is served by list_resources_fallback rather than being
// unsupported. It reaches the agent two ways:
//
//   - serverInstructions, sent in the initialize response, which clients such as
//     Claude Code put in the system prompt. It costs context in every session,
//     so it holds only the rules an agent must never miss, and points at the
//     guide for the rest.
//   - the aws_usage_guide tool, which returns skills/using-aws-mcp/SKILL.md —
//     embedded, so the guide always matches the binary serving it.
//
// TestUsageGuideCoversEveryTool and TestUsageGuideNamesOnlyRealTools keep both
// in step with the registered tools: adding or renaming a tool without updating
// the guide fails the build.

// serverInstructions is kept short on purpose; see above. TestServerInstructions
// caps its length.
const serverInstructions = `aws-mcp: read-only AWS inventory, Cost Explorer, Cost Anomaly Detection and Savings Plans across the accounts this server reaches. No tool here changes AWS.

` + instructionRules

// readWriteInstructions replace serverInstructions when the server runs with
// --read-write. The extra rules are the ones an agent must not learn only after
// a write has gone out.
const readWriteInstructions = `aws-mcp: AWS inventory, Cost Explorer, Cost Anomaly Detection and Savings Plans across the accounts this server reaches — started with --read-write, so write tools are enabled and can change real infrastructure.

` + instructionRules + `
- Writes (create_resource, update_resource, delete_resource, tag_resources, untag_resources, cancel_resource_request) act on exactly one account and region. Confirm the target account, region and identifier with the user before calling one, never act on an inferred target, and read the returned status: IN_PROGRESS means not done yet — poll get_resource_request. Call aws_usage_guide with topic 'writes' first.`

// instructionRules are the rules both modes share.
const instructionRules = `- list_resource_types lists the curated types for list_resources and count_resources. ANY other type (NAT gateways, KMS keys, IAM roles, ECR, MSK, OpenSearch, Kinesis, ...) is served by list_resources_fallback through Cloud Control, using its CloudFormation name (e.g. AWS::EC2::NatGateway). Never call a type unsupported until list_resources_fallback has returned an error for it.
- Read queried and warnings in every resource response before treating an empty list or a count as complete, and report unreachable accounts or regions.
- Pass account_id to scope a query to one account: only that account is contacted. Pass region whenever the question allows. Prefer count_resources for "how many" and "break down by".
- The default view=id omits IPs and every other attribute; use view=summary for public_ip/private_ip, origins and the like before concluding they are unavailable.
- Cost Explorer bills per request: get exact filter values from list_cost_dimension_values before filtering get_cost_and_usage.
- On an authentication error, call aws_auth_status; it says which login fixes it.
- Before a non-trivial fallback, Cost Explorer, cost anomaly or Savings Plans query, call aws_usage_guide (optionally with a topic) for the full guide.`

// registerGuideTools declares aws_usage_guide.
func (s *Server) registerGuideTools() {
	s.addReadOnlyTool(
		mcp.NewTool(
			"aws_usage_guide",
			mcp.WithDescription("Return the usage guide for this server: which resource tool to use (including list_resources_fallback for types list_resource_types does not list), how to read queried/warnings, account scoping, Cost Explorer, cost anomaly and Savings Plans recipes, and auth troubleshooting. Makes no AWS calls. Pass topic to get one section instead of the whole guide (about 4k tokens)."),
			mcp.WithString("topic",
				mcp.Description("Optional section to return, matched case-insensitively against the guide's headings, e.g. 'fallback', 'cost explorer', 'forecast', 'cost anomalies', 'savings plans', 'reading results', 'auth'. Omit for the whole guide; an unknown topic returns the list of sections."),
			),
		),
		s.handleUsageGuide,
	)
}

func (s *Server) handleUsageGuide(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	doc := guideBody(skills.UsingAWSMCP)

	topic := strings.TrimSpace(req.GetString("topic", ""))
	if topic == "" {
		return mcp.NewToolResultText(doc), nil
	}

	sections := guideSections(doc)

	if section, ok := findGuideSection(sections, topic); ok {
		return mcp.NewToolResultText(section.body), nil
	}

	titles := make([]string, 0, len(sections))
	for _, section := range sections {
		titles = append(titles, section.title)
	}

	return mcp.NewToolResultErrorf("no guide section matches %q; sections: %s", topic, strings.Join(titles, "; ")), nil
}

// guideBody strips the YAML frontmatter: it is for skill loaders, and an agent
// reading the guide through the tool has no use for it.
func guideBody(doc string) string {
	if !strings.HasPrefix(doc, "---\n") {
		return doc
	}

	if _, body, ok := strings.Cut(doc[len("---\n"):], "\n---\n"); ok {
		return strings.TrimLeft(body, "\n")
	}

	return doc
}

// guideSection is one ## or ### section of the guide, heading included, running
// to the next heading of the same or a higher level.
type guideSection struct {
	level int
	title string
	body  string
}

// guideSections splits the guide at its ## and ### headings, skipping lines
// inside code fences.
func guideSections(doc string) []guideSection {
	lines := strings.Split(doc, "\n")

	type heading struct {
		line  int
		level int
		title string
	}

	headings := []heading{}
	inFence := false

	for i, line := range lines {
		if strings.HasPrefix(line, "```") {
			inFence = !inFence

			continue
		}

		if inFence {
			continue
		}

		switch {
		case strings.HasPrefix(line, "### "):
			headings = append(headings, heading{line: i, level: 3, title: strings.TrimPrefix(line, "### ")})
		case strings.HasPrefix(line, "## "):
			headings = append(headings, heading{line: i, level: 2, title: strings.TrimPrefix(line, "## ")})
		}
	}

	sections := make([]guideSection, 0, len(headings))

	for i, h := range headings {
		end := len(lines)

		for _, next := range headings[i+1:] {
			if next.level <= h.level {
				end = next.line

				break
			}
		}

		sections = append(sections, guideSection{
			level: h.level,
			title: h.title,
			body:  strings.TrimRight(strings.Join(lines[h.line:end], "\n"), "\n") + "\n",
		})
	}

	return sections
}

// findGuideSection returns the first section whose heading contains the topic,
// ignoring case, backticks and the separators an agent might use instead of
// spaces.
func findGuideSection(sections []guideSection, topic string) (guideSection, bool) {
	normalize := func(s string) string {
		s = strings.ToLower(s)
		s = strings.NewReplacer("`", "", "_", " ", "-", " ").Replace(s)

		return strings.Join(strings.Fields(s), " ")
	}

	want := normalize(topic)

	for _, section := range sections {
		if strings.Contains(normalize(section.title), want) {
			return section, true
		}
	}

	return guideSection{}, false
}
