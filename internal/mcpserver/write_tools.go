package mcpserver

import (
	"context"
	"strings"

	cctypes "github.com/aws/aws-sdk-go-v2/service/cloudcontrol/types"
	awscfg "github.com/aws/aws-sdk-go-v2/service/configservice/types"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/rs/zerolog/log"

	"github.com/imunhatep/aws-mcp-go/pkg/errors"
)

// The write tools are registered only when the server runs with --read-write
// (registerTools). In the default read-only mode none of them exist, which is a
// stronger guarantee than refusing the call: an agent cannot be talked into a
// write the server never offered.
//
// Resource writes go through the Cloud Control API (awslib's cloudcontrol write
// repository), so they reach the same set of types list_resources_fallback can
// read, by the same CloudFormation names and identifiers. Tags go through the
// Resource Groups Tagging API instead: it is synchronous, batched and keyed by
// ARN, and needs no knowledge of a type's schema. See write.go for the rules
// every write follows.

// writeTool marks a tool as one that changes AWS. destructive and idempotent
// carry the MCP annotation semantics: destructive means the change can remove
// or overwrite something, idempotent that repeating the call changes nothing
// further.
func writeTool(tool mcp.Tool, destructive, idempotent bool) mcp.Tool {
	tool.Annotations.ReadOnlyHint = mcp.ToBoolPtr(false)
	tool.Annotations.DestructiveHint = mcp.ToBoolPtr(destructive)
	tool.Annotations.IdempotentHint = mcp.ToBoolPtr(idempotent)
	tool.Annotations.OpenWorldHint = mcp.ToBoolPtr(true)

	return tool
}

// writeScopeOptions are the account_id and region arguments every write-mode
// tool takes, both required.
func writeScopeOptions() []mcp.ToolOption {
	return []mcp.ToolOption{
		mcp.WithString("account_id",
			mcp.Required(),
			mcp.Description("AWS account ID to act on. Required even when the server reaches a single account: a write must name its target. Only this account is contacted. Call list_accounts for the reachable IDs."),
		),
		mcp.WithString("region",
			mcp.Required(),
			mcp.Description("AWS region to act on (e.g. eu-central-1). Required. For a global resource type such as IAM, pass us-east-1."),
		),
	}
}

func waitOption() mcp.ToolOption {
	return mcp.WithNumber("wait_seconds",
		mcp.Description("How long to wait for the request to finish (default 120, max 900). 0 returns as soon as AWS accepts the request. If the request is still running when the wait ends the result says so with done=false — follow request_token with get_resource_request, never resubmit."),
	)
}

func resourceTypeOption(verb string) mcp.ToolOption {
	return mcp.WithString("resource_type",
		mcp.Required(),
		mcp.Description("Resource type to "+verb+", in CloudFormation form (AWS::Logs::LogGroup) — the same names list_resources_fallback takes. Case-sensitive for types outside the AWS Config vocabulary."),
	)
}

func identifierOption(verb string) mcp.ToolOption {
	return mcp.WithString("identifier",
		mcp.Required(),
		mcp.Description("Primary identifier of the resource to "+verb+" — the id list_resources_fallback reports (a log group name, a bucket name, an instance ID). Composite identifiers are joined with |, exactly as Cloud Control reports them."),
	)
}

// registerWriteTools declares the write-mode tools.
func (s *Server) registerWriteTools() {
	s.mcp.AddTool(writeTool(mcp.NewTool("create_resource", append([]mcp.ToolOption{
		mcp.WithDescription("WRITE — creates a real AWS resource through the Cloud Control API. Only available because this server runs with --read-write. Confirm the account, region and properties with the user first. Returns {account_id, region, request, done, properties, warnings}: request.status SUCCESS with properties as AWS applied them, FAILED with error_code (e.g. AlreadyExists) and status_message, or IN_PROGRESS with done=false when the wait ended first. A create that was accepted is never reported as an error — do not retry it, follow request_token with get_resource_request."),
		resourceTypeOption("create"),
		mcp.WithString("desired_state",
			mcp.Required(),
			mcp.Description(`The resource's properties as a JSON object in the type's CloudFormation schema, e.g. {"LogGroupName":"demo","RetentionInDays":7}. Tags go here too, in the type's own shape (usually [{"Key":"k","Value":"v"}]).`),
		),
		waitOption(),
	}, writeScopeOptions()...)...), false, false), s.handleCreateResource)

	s.mcp.AddTool(writeTool(mcp.NewTool("update_resource", append([]mcp.ToolOption{
		mcp.WithDescription("WRITE — changes a real AWS resource in place through the Cloud Control API, with an RFC 6902 JSON Patch over its current properties. Only available because this server runs with --read-write. Read the resource first (list_resources_fallback view=detail) and confirm the change with the user. Properties the type marks create-only cannot be patched — Cloud Control rejects the update rather than replacing the resource. Returns the same envelope as create_resource."),
		resourceTypeOption("update"),
		identifierOption("update"),
		mcp.WithString("patch_document",
			mcp.Required(),
			mcp.Description(`RFC 6902 JSON Patch, e.g. [{"op":"replace","path":"/RetentionInDays","value":30}]. Operations: add, remove, replace, move, copy, test; paths are JSON Pointers into the resource's properties.`),
		),
		waitOption(),
	}, writeScopeOptions()...)...), true, false), s.handleUpdateResource)

	s.mcp.AddTool(writeTool(mcp.NewTool("delete_resource", append([]mcp.ToolOption{
		mcp.WithDescription("WRITE — permanently deletes a real AWS resource through the Cloud Control API. Irreversible. Only available because this server runs with --read-write. Name the exact resource, account and region to the user and get explicit confirmation before calling. Returns {account_id, region, request, done, warnings}; request.status FAILED with error_code NotFound means nothing was deleted."),
		resourceTypeOption("delete"),
		identifierOption("delete"),
		waitOption(),
	}, writeScopeOptions()...)...), true, true), s.handleDeleteResource)

	s.mcp.AddTool(writeTool(mcp.NewTool("tag_resources", append([]mcp.ToolOption{
		mcp.WithDescription("WRITE — adds or overwrites tags on up to 200 AWS resources by ARN, through the Resource Groups Tagging API. Only available because this server runs with --read-write. Existing values for the given keys are overwritten; other tags are kept. Every ARN must belong to account_id and region (global ARNs with an empty region are accepted). Returns {requested, succeeded, failed:[{arn, error_code, error_message}], warnings} — AWS reports per-resource failures inside a successful response, so read failed."),
		mcp.WithArray("arns",
			mcp.Required(),
			mcp.Description("ARNs of the resources to tag, all in account_id and region."),
			mcp.WithStringItems(),
		),
		mcp.WithObject("tags",
			mcp.Required(),
			mcp.Description(`Tags to set, as an object of string values, e.g. {"environment":"prod","creator-team":"sre"}. At most 50; keys must not start with aws:.`),
		),
	}, writeScopeOptions()...)...), true, true), s.handleTagResources)

	s.mcp.AddTool(writeTool(mcp.NewTool("untag_resources", append([]mcp.ToolOption{
		mcp.WithDescription("WRITE — removes tag keys from up to 200 AWS resources by ARN, through the Resource Groups Tagging API. Only available because this server runs with --read-write. Keys a resource does not carry are not an error. Every ARN must belong to account_id and region. Returns the same envelope as tag_resources."),
		mcp.WithArray("arns",
			mcp.Required(),
			mcp.Description("ARNs of the resources to untag, all in account_id and region."),
			mcp.WithStringItems(),
		),
		mcp.WithArray("keys",
			mcp.Required(),
			mcp.Description("Tag keys to remove."),
			mcp.WithStringItems(),
		),
	}, writeScopeOptions()...)...), true, true), s.handleUntagResources)

	s.mcp.AddTool(readOnlyTool(mcp.NewTool("get_resource_request", append([]mcp.ToolOption{
		mcp.WithDescription("Read the current status of a Cloud Control resource request by the request_token a write returned — how to follow a create, update or delete that came back with done=false. Changes nothing. Returns {account_id, region, request, done}."),
		mcp.WithString("request_token",
			mcp.Required(),
			mcp.Description("request.request_token from create_resource, update_resource, delete_resource or list_resource_requests."),
		),
	}, writeScopeOptions()...)...)), s.handleGetResourceRequest)

	s.mcp.AddTool(readOnlyTool(mcp.NewTool("list_resource_requests", append([]mcp.ToolOption{
		mcp.WithDescription("List the Cloud Control resource requests in one account and region, newest first — every create, update and delete made through Cloud Control by any caller in the last ~7 days, not only this server's. Changes nothing. Use it to check whether a write already went out before retrying one. Returns {account_id, region, items, count, total}."),
		mcp.WithString("status",
			mcp.Description("Optional comma-separated statuses to keep: PENDING, IN_PROGRESS, SUCCESS, FAILED, CANCEL_IN_PROGRESS, CANCEL_COMPLETE (case-insensitive)."),
		),
		mcp.WithNumber("limit",
			mcp.Description("Maximum requests to return (default 50, max 1000)."),
		),
	}, writeScopeOptions()...)...)), s.handleListResourceRequests)

	s.mcp.AddTool(writeTool(mcp.NewTool("cancel_resource_request", append([]mcp.ToolOption{
		mcp.WithDescription("WRITE — cancels a pending or in-progress Cloud Control CREATE or UPDATE request. Only available because this server runs with --read-write. A cancelled create can leave a partly created resource behind; check with list_resources_fallback afterwards. Returns {account_id, region, request, done}."),
		mcp.WithString("request_token",
			mcp.Required(),
			mcp.Description("request.request_token of the request to cancel."),
		),
	}, writeScopeOptions()...)...), true, true), s.handleCancelResourceRequest)
}

func (s *Server) handleCreateResource(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	accountID, region, err := parseWriteScope(req)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("invalid target", err), nil
	}

	rt, warnings, err := parseWriteResourceType(req)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("invalid resource_type", err), nil
	}

	desired, err := parseDesiredState(req)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("invalid desired_state", err), nil
	}

	wait, err := parseWait(req)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("invalid wait_seconds", err), nil
	}

	logWrite("create_resource", accountID, region.String(), rt, "")

	target, err := s.resolveWriteTarget(ctx, accountID, region)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("failed to initialise aws client", err), nil
	}

	return s.runResourceWrite(ctx, target, resourceWrite{
		resourceType: rt,
		submit: func(w cloudControlWriter) (*cctypes.ProgressEvent, error) {
			return w.CreateResource(rt, desired)
		},
		describe: true,
		wait:     wait,
		warnings: warnings,
	})
}

func (s *Server) handleUpdateResource(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	accountID, region, err := parseWriteScope(req)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("invalid target", err), nil
	}

	rt, warnings, err := parseWriteResourceType(req)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("invalid resource_type", err), nil
	}

	identifier, err := parseIdentifier(req)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("invalid identifier", err), nil
	}

	patch, err := parsePatchDocument(req)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("invalid patch_document", err), nil
	}

	wait, err := parseWait(req)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("invalid wait_seconds", err), nil
	}

	logWrite("update_resource", accountID, region.String(), rt, identifier)

	target, err := s.resolveWriteTarget(ctx, accountID, region)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("failed to initialise aws client", err), nil
	}

	return s.runResourceWrite(ctx, target, resourceWrite{
		resourceType: rt,
		submit: func(w cloudControlWriter) (*cctypes.ProgressEvent, error) {
			return w.UpdateResource(rt, identifier, patch)
		},
		describe: true,
		wait:     wait,
		warnings: warnings,
	})
}

func (s *Server) handleDeleteResource(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	accountID, region, err := parseWriteScope(req)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("invalid target", err), nil
	}

	rt, warnings, err := parseWriteResourceType(req)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("invalid resource_type", err), nil
	}

	identifier, err := parseIdentifier(req)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("invalid identifier", err), nil
	}

	wait, err := parseWait(req)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("invalid wait_seconds", err), nil
	}

	logWrite("delete_resource", accountID, region.String(), rt, identifier)

	target, err := s.resolveWriteTarget(ctx, accountID, region)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("failed to initialise aws client", err), nil
	}

	return s.runResourceWrite(ctx, target, resourceWrite{
		resourceType: rt,
		submit: func(w cloudControlWriter) (*cctypes.ProgressEvent, error) {
			return w.DeleteResource(rt, identifier)
		},
		wait:     wait,
		warnings: warnings,
	})
}

func (s *Server) handleTagResources(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	accountID, region, err := parseWriteScope(req)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("invalid target", err), nil
	}

	arns, err := parseTagArns(req, accountID, region)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("invalid arns", err), nil
	}

	tags, err := parseTags(req)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("invalid tags", err), nil
	}

	logWrite("tag_resources", accountID, region.String(), "", strings.Join(arns, ","))

	target, err := s.resolveWriteTarget(ctx, accountID, region)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("failed to initialise aws client", err), nil
	}

	failures, err := s.taggerFor(ctx, target.client).TagResources(arns, tags)

	return s.buildTagResult(target, arns, failures, err)
}

func (s *Server) handleUntagResources(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	accountID, region, err := parseWriteScope(req)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("invalid target", err), nil
	}

	arns, err := parseTagArns(req, accountID, region)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("invalid arns", err), nil
	}

	keys, err := parseStringList(req, "keys")
	if err != nil {
		return mcp.NewToolResultErrorFromErr("invalid keys", err), nil
	}

	for _, k := range keys {
		if err := validateTagKey(k); err != nil {
			return mcp.NewToolResultErrorFromErr("invalid keys", err), nil
		}
	}

	logWrite("untag_resources", accountID, region.String(), "", strings.Join(arns, ","))

	target, err := s.resolveWriteTarget(ctx, accountID, region)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("failed to initialise aws client", err), nil
	}

	failures, err := s.taggerFor(ctx, target.client).UntagResources(arns, keys)

	return s.buildTagResult(target, arns, failures, err)
}

// requestStatusResult is the get_resource_request / cancel_resource_request
// envelope.
type requestStatusResult struct {
	AccountID string     `json:"account_id"`
	Region    string     `json:"region"`
	Request   requestDTO `json:"request"`
	Done      bool       `json:"done"`
}

func parseRequestToken(req mcp.CallToolRequest) (string, error) {
	token := strings.TrimSpace(req.GetString("request_token", ""))
	if token == "" {
		return "", errors.New("request_token is required")
	}

	return token, nil
}

func (s *Server) handleGetResourceRequest(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	accountID, region, err := parseWriteScope(req)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("invalid target", err), nil
	}

	token, err := parseRequestToken(req)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("invalid request_token", err), nil
	}

	target, err := s.resolveWriteTarget(ctx, accountID, region)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("failed to initialise aws client", err), nil
	}

	status, err := s.cloudControlFor(ctx, target.client).DescribeResourceRequest(token)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("failed to read the request status", err), nil
	}

	dto := newRequestDTO(status.ProgressEvent)

	return jsonResult(requestStatusResult{
		AccountID: target.accountID,
		Region:    target.region.String(),
		Request:   dto,
		Done:      requestDone(dto.Status),
	})
}

func (s *Server) handleCancelResourceRequest(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	accountID, region, err := parseWriteScope(req)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("invalid target", err), nil
	}

	token, err := parseRequestToken(req)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("invalid request_token", err), nil
	}

	logWrite("cancel_resource_request", accountID, region.String(), "", token)

	target, err := s.resolveWriteTarget(ctx, accountID, region)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("failed to initialise aws client", err), nil
	}

	event, err := s.cloudControlFor(ctx, target.client).CancelResourceRequest(token)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("AWS did not accept the cancellation", err), nil
	}

	dto := newRequestDTO(event)

	return jsonResult(requestStatusResult{
		AccountID: target.accountID,
		Region:    target.region.String(),
		Request:   dto,
		Done:      requestDone(dto.Status),
	})
}

// requestListResult is the list_resource_requests envelope.
type requestListResult struct {
	AccountID string       `json:"account_id"`
	Region    string       `json:"region"`
	Items     []requestDTO `json:"items"`
	Count     int          `json:"count"`
	Total     int          `json:"total"`
}

// parseRequestStatuses reads the status filter of list_resource_requests.
func parseRequestStatuses(raw string) ([]cctypes.OperationStatus, error) {
	var out []cctypes.OperationStatus

	known := cctypes.OperationStatus("").Values()

	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		normalized := strings.ToUpper(strings.ReplaceAll(strings.ReplaceAll(part, "-", "_"), " ", "_"))

		matched := false
		for _, status := range known {
			if string(status) == normalized {
				out = append(out, status)
				matched = true

				break
			}
		}

		if !matched {
			return nil, errors.Errorf("unknown status %q; use PENDING, IN_PROGRESS, SUCCESS, FAILED, CANCEL_IN_PROGRESS or CANCEL_COMPLETE", part)
		}
	}

	return out, nil
}

func (s *Server) handleListResourceRequests(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	accountID, region, err := parseWriteScope(req)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("invalid target", err), nil
	}

	statuses, err := parseRequestStatuses(req.GetString("status", ""))
	if err != nil {
		return mcp.NewToolResultErrorFromErr("invalid status", err), nil
	}

	limit := clampLimit(req.GetInt("limit", 0))

	target, err := s.resolveWriteTarget(ctx, accountID, region)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("failed to initialise aws client", err), nil
	}

	events, err := s.cloudControlFor(ctx, target.client).FindResourceRequests(statuses...)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("failed to list resource requests", err), nil
	}

	items := make([]requestDTO, 0, min(len(events), limit))
	for i := range events {
		if len(items) == limit {
			break
		}

		items = append(items, newRequestDTO(&events[i]))
	}

	return jsonResult(requestListResult{
		AccountID: target.accountID,
		Region:    target.region.String(),
		Items:     items,
		Count:     len(items),
		Total:     len(events),
	})
}

// logWrite records every write at info level before it is sent, so the server
// log is an audit trail of what an agent asked for even when the call fails.
func logWrite(tool, accountID, region string, rt awscfg.ResourceType, target string) {
	log.Info().
		Str("tool", tool).
		Str("account_id", accountID).
		Str("region", region).
		Str("type", string(rt)).
		Str("target", target).
		Msg("[mcpserver.write] submitting AWS write")
}
