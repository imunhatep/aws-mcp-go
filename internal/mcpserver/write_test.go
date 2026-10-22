package mcpserver

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscloudcontrol "github.com/aws/aws-sdk-go-v2/service/cloudcontrol"
	cctypes "github.com/aws/aws-sdk-go-v2/service/cloudcontrol/types"
	awscfg "github.com/aws/aws-sdk-go-v2/service/configservice/types"
	rgttypes "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi/types"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server/servertest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ptypes "github.com/imunhatep/awslib/provider/types"
	v3 "github.com/imunhatep/awslib/provider/v3"
	"github.com/imunhatep/awslib/service/cloudcontrol"

	"github.com/imunhatep/aws-mcp-go/pkg/errors"
)

// writeToolNames is every tool that exists only with --read-write.
var writeToolNames = []string{
	"cancel_resource_request",
	"create_resource",
	"delete_resource",
	"get_resource_request",
	"list_resource_requests",
	"tag_resources",
	"untag_resources",
	"update_resource",
}

// localStateTools change state on the server host but never AWS, and are
// therefore allowed in read-only mode.
var localStateTools = map[string]bool{"aws_sso_login": true}

// The default is the guarantee: a server started without --read-write lists no
// tool that can change AWS, and every tool it does list says so in its MCP
// annotations, so a client gating on readOnlyHint sees the same thing.
func TestReadOnlyModeRegistersOnlyReadOnlyTools(t *testing.T) {
	srv := NewServer(context.Background(), nil, nil)
	require.False(t, srv.ReadWrite())

	tools := srv.MCPServer().ListTools()
	require.NotEmpty(t, tools)

	for _, name := range writeToolNames {
		assert.NotContains(t, tools, name, "%s must not be registered in read-only mode", name)
	}

	for name, tool := range tools {
		ann := tool.Tool.Annotations

		require.NotNil(t, ann.DestructiveHint, name)
		assert.False(t, *ann.DestructiveHint, "%s is registered in read-only mode, so it must not be destructive", name)

		if localStateTools[name] {
			continue
		}

		require.NotNil(t, ann.ReadOnlyHint, name)
		assert.True(t, *ann.ReadOnlyHint, "%s is registered in read-only mode, so it must be annotated read-only (register it with addReadOnlyTool)", name)
	}
}

func TestReadWriteModeAddsExactlyTheWriteTools(t *testing.T) {
	ro := NewServer(context.Background(), nil, nil).MCPServer().ListTools()
	rwSrv := NewServer(context.Background(), nil, nil, EnableWrites())
	require.True(t, rwSrv.ReadWrite())

	rw := rwSrv.MCPServer().ListTools()

	added := []string{}
	for name := range rw {
		if _, ok := ro[name]; !ok {
			added = append(added, name)
		}
	}

	sort.Strings(added)
	assert.Equal(t, writeToolNames, added)

	for name := range ro {
		assert.Contains(t, rw, name, "read-write mode must keep every read-only tool")
	}

	readOnlyInWriteMode := map[string]bool{"get_resource_request": true, "list_resource_requests": true}

	for _, name := range writeToolNames {
		ann := rw[name].Tool.Annotations
		require.NotNil(t, ann.ReadOnlyHint, name)

		assert.Equal(t, readOnlyInWriteMode[name], *ann.ReadOnlyHint, "%s readOnlyHint", name)
	}

	for _, name := range []string{"delete_resource", "update_resource", "untag_resources"} {
		assert.True(t, *rw[name].Tool.Annotations.DestructiveHint, "%s must be annotated destructive", name)
	}
}

func TestReadWriteInstructionsReachTheClient(t *testing.T) {
	srv := NewServer(context.Background(), nil, nil, EnableWrites())
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
	assert.Equal(t, readWriteInstructions, res.Instructions)
}

func TestInstructionsStateTheMode(t *testing.T) {
	assert.Contains(t, serverInstructions, "read-only")
	assert.NotContains(t, serverInstructions, "create_resource", "read-only instructions must not advertise write tools")

	assert.Contains(t, readWriteInstructions, "--read-write")
	assert.Contains(t, readWriteInstructions, "get_resource_request")
	assert.NotContains(t, readWriteInstructions, "read-only AWS inventory")
	assert.Less(t, len(readWriteInstructions), 2000, "server instructions cost context in every session; move detail into the guide")
}

func callHandler(t *testing.T, handler func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error), args map[string]any) (*mcp.CallToolResult, string) {
	t.Helper()

	req := mcp.CallToolRequest{}
	req.Params.Arguments = args

	res, err := handler(context.Background(), req)
	require.NoError(t, err)
	require.NotEmpty(t, res.Content)

	text, ok := res.Content[0].(mcp.TextContent)
	require.True(t, ok)

	return res, text.Text
}

// Every argument is checked before the pool is reached. The server has a nil
// pool, so a handler that touched AWS first would panic rather than fail.
func TestWriteToolsValidateBeforeCallingAWS(t *testing.T) {
	srv := NewServer(context.Background(), nil, nil, EnableWrites())

	scope := map[string]any{"account_id": "111111111111", "region": "eu-central-1"}
	with := func(extra map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range scope {
			out[k] = v
		}
		for k, v := range extra {
			if v == nil {
				delete(out, k)
				continue
			}
			out[k] = v
		}
		return out
	}

	cases := []struct {
		name    string
		handler func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error)
		args    map[string]any
		want    string
	}{
		{"create without account", srv.handleCreateResource, with(map[string]any{"account_id": nil, "resource_type": "AWS::Logs::LogGroup", "desired_state": `{"LogGroupName":"x"}`}), "account_id is required"},
		{"create bad account", srv.handleCreateResource, with(map[string]any{"account_id": "prod", "resource_type": "AWS::Logs::LogGroup", "desired_state": `{"LogGroupName":"x"}`}), "12-digit"},
		{"create without region", srv.handleCreateResource, with(map[string]any{"region": nil, "resource_type": "AWS::Logs::LogGroup", "desired_state": `{"LogGroupName":"x"}`}), "region is required"},
		{"create unknown region", srv.handleCreateResource, with(map[string]any{"region": "mars-1", "resource_type": "AWS::Logs::LogGroup", "desired_state": `{"LogGroupName":"x"}`}), "unknown region"},
		{"create bad type", srv.handleCreateResource, with(map[string]any{"resource_type": "LogGroup", "desired_state": `{"LogGroupName":"x"}`}), "invalid resource type"},
		{"create state not object", srv.handleCreateResource, with(map[string]any{"resource_type": "AWS::Logs::LogGroup", "desired_state": `["x"]`}), "JSON object"},
		{"create empty state", srv.handleCreateResource, with(map[string]any{"resource_type": "AWS::Logs::LogGroup", "desired_state": map[string]any{}}), "at least one property"},
		{"create wait too long", srv.handleCreateResource, with(map[string]any{"resource_type": "AWS::Logs::LogGroup", "desired_state": `{"LogGroupName":"x"}`, "wait_seconds": 3600}), "at most"},
		{"update without identifier", srv.handleUpdateResource, with(map[string]any{"resource_type": "AWS::Logs::LogGroup", "patch_document": `[{"op":"remove","path":"/RetentionInDays"}]`}), "identifier is required"},
		{"update bad op", srv.handleUpdateResource, with(map[string]any{"resource_type": "AWS::Logs::LogGroup", "identifier": "x", "patch_document": `[{"op":"set","path":"/RetentionInDays","value":1}]`}), "not one of"},
		{"update bad path", srv.handleUpdateResource, with(map[string]any{"resource_type": "AWS::Logs::LogGroup", "identifier": "x", "patch_document": `[{"op":"replace","path":"RetentionInDays","value":1}]`}), "JSON Pointer"},
		{"update missing value", srv.handleUpdateResource, with(map[string]any{"resource_type": "AWS::Logs::LogGroup", "identifier": "x", "patch_document": `[{"op":"replace","path":"/RetentionInDays"}]`}), "needs a value"},
		{"update empty patch", srv.handleUpdateResource, with(map[string]any{"resource_type": "AWS::Logs::LogGroup", "identifier": "x", "patch_document": `[]`}), "at least one operation"},
		{"delete without type", srv.handleDeleteResource, with(map[string]any{"identifier": "x"}), "resource_type is required"},
		{"delete negative wait", srv.handleDeleteResource, with(map[string]any{"resource_type": "AWS::Logs::LogGroup", "identifier": "x", "wait_seconds": -1}), "zero or positive"},
		{"tag other account", srv.handleTagResources, with(map[string]any{"arns": []any{"arn:aws:logs:eu-central-1:222222222222:log-group:x"}, "tags": map[string]any{"a": "b"}}), "not the target account"},
		{"tag other region", srv.handleTagResources, with(map[string]any{"arns": []any{"arn:aws:logs:us-east-1:111111111111:log-group:x"}, "tags": map[string]any{"a": "b"}}), "not the target region"},
		{"tag bad arn", srv.handleTagResources, with(map[string]any{"arns": []any{"log-group:x"}, "tags": map[string]any{"a": "b"}}), "invalid ARN"},
		{"tag reserved key", srv.handleTagResources, with(map[string]any{"arns": []any{"arn:aws:s3:::bucket"}, "tags": map[string]any{"aws:cloudformation:stack-name": "b"}}), "reserved"},
		{"tag non-string value", srv.handleTagResources, with(map[string]any{"arns": []any{"arn:aws:s3:::bucket"}, "tags": map[string]any{"a": 1}}), "must be a string"},
		{"tag without tags", srv.handleTagResources, with(map[string]any{"arns": []any{"arn:aws:s3:::bucket"}}), "tags is required"},
		{"untag without keys", srv.handleUntagResources, with(map[string]any{"arns": []any{"arn:aws:s3:::bucket"}}), "keys is required"},
		{"get without token", srv.handleGetResourceRequest, scope, "request_token is required"},
		{"cancel without token", srv.handleCancelResourceRequest, scope, "request_token is required"},
		{"list bad status", srv.handleListResourceRequests, with(map[string]any{"status": "DONE"}), "unknown status"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, text := callHandler(t, tc.handler, tc.args)
			assert.True(t, res.IsError)
			assert.Contains(t, text, tc.want)
		})
	}
}

// writeSpyPool hands out a fixed set of clients and records how it was asked.
type writeSpyPool struct {
	clients      []*v3.Client
	allCalls     int
	accountCalls int
	lastAccount  ptypes.AwsAccountID
	lastRegions  []ptypes.AwsRegion
}

func (p *writeSpyPool) GetClients(_ ...ptypes.AwsRegion) ([]*v3.Client, error) {
	p.allCalls++
	return p.clients, nil
}

func (p *writeSpyPool) GetAccountClients(accountID ptypes.AwsAccountID, regions ...ptypes.AwsRegion) ([]*v3.Client, error) {
	p.accountCalls++
	p.lastAccount = accountID
	p.lastRegions = regions

	return p.clients, nil
}

func (p *writeSpyPool) ListAccountIDs() ([]ptypes.AwsAccountID, error) {
	return []ptypes.AwsAccountID{"111111111111"}, nil
}

// fakeCloudControl scripts the Cloud Control responses a write sees.
type fakeCloudControl struct {
	accepted    *cctypes.ProgressEvent
	submitErr   error
	final       *cctypes.ProgressEvent
	waitErr     error
	properties  string
	describeErr error

	calls       []string
	waited      time.Duration
	invalidated int
}

func (f *fakeCloudControl) CreateResource(_ awscfg.ResourceType, desiredState string) (*cctypes.ProgressEvent, error) {
	f.calls = append(f.calls, "create:"+desiredState)
	return f.accepted, f.submitErr
}

func (f *fakeCloudControl) UpdateResource(_ awscfg.ResourceType, identifier string, patch string) (*cctypes.ProgressEvent, error) {
	f.calls = append(f.calls, "update:"+identifier+":"+patch)
	return f.accepted, f.submitErr
}

func (f *fakeCloudControl) DeleteResource(_ awscfg.ResourceType, identifier string) (*cctypes.ProgressEvent, error) {
	f.calls = append(f.calls, "delete:"+identifier)
	return f.accepted, f.submitErr
}

func (f *fakeCloudControl) WaitForResourceRequest(token string, maxWait time.Duration) (*cctypes.ProgressEvent, error) {
	f.calls = append(f.calls, "wait:"+token)
	f.waited = maxWait

	return f.final, f.waitErr
}

func (f *fakeCloudControl) DescribeResourceRequest(token string) (*awscloudcontrol.GetResourceRequestStatusOutput, error) {
	f.calls = append(f.calls, "status:"+token)
	return &awscloudcontrol.GetResourceRequestStatusOutput{ProgressEvent: f.final}, nil
}

func (f *fakeCloudControl) CancelResourceRequest(token string) (*cctypes.ProgressEvent, error) {
	f.calls = append(f.calls, "cancel:"+token)
	return f.final, nil
}

func (f *fakeCloudControl) FindResourceRequests(statuses ...cctypes.OperationStatus) ([]cctypes.ProgressEvent, error) {
	f.calls = append(f.calls, "find")
	return []cctypes.ProgressEvent{*f.final, *f.final, *f.final}, nil
}

func (f *fakeCloudControl) DescribeResource(_ awscfg.ResourceType, identifier *string) (*awscloudcontrol.GetResourceOutput, error) {
	f.calls = append(f.calls, "describe:"+aws.ToString(identifier))
	if f.describeErr != nil {
		return nil, f.describeErr
	}

	return &awscloudcontrol.GetResourceOutput{ResourceDescription: &cctypes.ResourceDescription{Properties: aws.String(f.properties)}}, nil
}

func (f *fakeCloudControl) InvalidateLists() { f.invalidated++ }

func event(status cctypes.OperationStatus, identifier string) *cctypes.ProgressEvent {
	return &cctypes.ProgressEvent{
		RequestToken:    aws.String("tok-1"),
		Operation:       cctypes.OperationCreate,
		OperationStatus: status,
		TypeName:        aws.String("AWS::Logs::LogGroup"),
		Identifier:      aws.String(identifier),
	}
}

func writeServer(fake *fakeCloudControl) (*Server, *writeSpyPool) {
	pool := &writeSpyPool{clients: []*v3.Client{{}}}
	srv := NewServer(context.Background(), pool, nil, EnableWrites())
	srv.newCloudControl = func(context.Context, *v3.Client) cloudControlWriter { return fake }

	return srv, pool
}

var logGroupArgs = map[string]any{
	"account_id":    "111111111111",
	"region":        "eu-central-1",
	"resource_type": "AWS::Logs::LogGroup",
	"desired_state": map[string]any{"LogGroupName": "demo", "RetentionInDays": 7},
}

func decodeWrite(t *testing.T, text string) writeResult {
	t.Helper()

	var out writeResult
	require.NoError(t, json.Unmarshal([]byte(text), &out))

	return out
}

func TestCreateResourceWaitsAndReadsBack(t *testing.T) {
	fake := &fakeCloudControl{
		accepted:   event(cctypes.OperationStatusInProgress, ""),
		final:      event(cctypes.OperationStatusSuccess, "demo"),
		properties: `{"LogGroupName":"demo","RetentionInDays":7,"Arn":"arn:aws:logs:eu-central-1:111111111111:log-group:demo:*"}`,
	}
	srv, pool := writeServer(fake)

	res, text := callHandler(t, srv.handleCreateResource, logGroupArgs)
	require.False(t, res.IsError, text)

	out := decodeWrite(t, text)
	assert.True(t, out.Done)
	assert.Equal(t, "SUCCESS", out.Request.Status)
	assert.Equal(t, "demo", out.Request.Identifier)
	assert.Equal(t, "111111111111", out.AccountID)
	assert.Equal(t, "eu-central-1", out.Region)
	assert.Equal(t, "demo", out.Properties["LogGroupName"])

	// The desired state reached AWS as JSON, the type was canonicalized, and
	// the wait used the default bound.
	require.NotEmpty(t, fake.calls)
	assert.True(t, strings.HasPrefix(fake.calls[0], "create:{"), fake.calls[0])
	assert.Contains(t, fake.calls, "describe:demo")
	assert.Equal(t, defaultWriteWait, fake.waited)
	assert.Equal(t, 1, fake.invalidated)

	// Only the named account and region were contacted.
	assert.Equal(t, 1, pool.accountCalls)
	assert.Zero(t, pool.allCalls)
	assert.Equal(t, ptypes.AwsAccountID("111111111111"), pool.lastAccount)
	assert.Equal(t, []ptypes.AwsRegion{"eu-central-1"}, pool.lastRegions)
}

func TestWriteThatFailsInAWSIsAnErrorWithItsCode(t *testing.T) {
	failed := event(cctypes.OperationStatusFailed, "demo")
	failed.ErrorCode = cctypes.HandlerErrorCodeAlreadyExists
	failed.StatusMessage = aws.String("Resource of type 'AWS::Logs::LogGroup' with identifier 'demo' already exists.")

	fake := &fakeCloudControl{
		accepted: event(cctypes.OperationStatusInProgress, ""),
		final:    failed,
		waitErr:  errors.WithStack(&cloudcontrol.RequestFailedError{Event: failed}),
	}
	srv, _ := writeServer(fake)

	res, text := callHandler(t, srv.handleCreateResource, logGroupArgs)
	assert.True(t, res.IsError)

	out := decodeWrite(t, text)
	assert.True(t, out.Done)
	assert.Equal(t, "FAILED", out.Request.Status)
	assert.Equal(t, "AlreadyExists", out.Request.ErrorCode)
	assert.Contains(t, out.Request.StatusMessage, "already exists")
	assert.NotContains(t, fake.calls, "describe:demo", "a failed create has nothing to read back")
}

// An accepted request whose wait ran out may still land. Reporting it as an
// error would invite a retry, and a retried create creates twice.
func TestWriteStillRunningIsNotAnError(t *testing.T) {
	fake := &fakeCloudControl{
		accepted: event(cctypes.OperationStatusPending, ""),
		final:    event(cctypes.OperationStatusInProgress, ""),
		waitErr:  errors.New("exceeded max wait time for ResourceRequestSuccess waiter"),
	}
	srv, _ := writeServer(fake)

	args := map[string]any{}
	for k, v := range logGroupArgs {
		args[k] = v
	}
	args["wait_seconds"] = 5

	res, text := callHandler(t, srv.handleCreateResource, args)
	assert.False(t, res.IsError)

	out := decodeWrite(t, text)
	assert.False(t, out.Done)
	assert.Equal(t, "IN_PROGRESS", out.Request.Status)
	assert.Equal(t, "tok-1", out.Request.RequestToken)
	assert.Equal(t, 5*time.Second, fake.waited)
	assert.Contains(t, strings.Join(out.Warnings, " "), "do not resubmit")
}

func TestWriteWithUnreadableStatusIsNotAnError(t *testing.T) {
	fake := &fakeCloudControl{
		accepted: event(cctypes.OperationStatusPending, ""),
		waitErr:  errors.New("context deadline exceeded"),
	}
	srv, _ := writeServer(fake)

	res, text := callHandler(t, srv.handleCreateResource, logGroupArgs)
	assert.False(t, res.IsError)

	out := decodeWrite(t, text)
	assert.False(t, out.Done)
	assert.Equal(t, "PENDING", out.Request.Status, "the accepted status is all that is known")
	assert.Contains(t, strings.Join(out.Warnings, " "), "do not resubmit")
}

func TestWriteRejectedBeforeAcceptanceIsAnError(t *testing.T) {
	fake := &fakeCloudControl{submitErr: errors.New("AccessDeniedException: not authorized to perform cloudformation:CreateResource")}
	srv, _ := writeServer(fake)

	res, text := callHandler(t, srv.handleCreateResource, logGroupArgs)
	assert.True(t, res.IsError)
	assert.Contains(t, text, "nothing was changed")
	assert.Contains(t, text, "AccessDenied")
	assert.Equal(t, []string{`create:{"LogGroupName":"demo","RetentionInDays":7}`}, fake.calls, "no wait after a rejected submission")
}

func TestWriteWithoutWaitReturnsOnAcceptance(t *testing.T) {
	fake := &fakeCloudControl{accepted: event(cctypes.OperationStatusPending, "")}
	srv, _ := writeServer(fake)

	res, text := callHandler(t, srv.handleDeleteResource, map[string]any{
		"account_id":    "111111111111",
		"region":        "eu-central-1",
		"resource_type": "AWS::Logs::LogGroup",
		"identifier":    "demo",
		"wait_seconds":  0,
	})
	assert.False(t, res.IsError)

	out := decodeWrite(t, text)
	assert.False(t, out.Done)
	assert.Equal(t, []string{"delete:demo"}, fake.calls)
	assert.Contains(t, strings.Join(out.Warnings, " "), "get_resource_request")
}

func TestDeleteDoesNotReadBack(t *testing.T) {
	fake := &fakeCloudControl{
		accepted: event(cctypes.OperationStatusInProgress, "demo"),
		final:    event(cctypes.OperationStatusSuccess, "demo"),
	}
	srv, _ := writeServer(fake)

	res, text := callHandler(t, srv.handleDeleteResource, map[string]any{
		"account_id":    "111111111111",
		"region":        "eu-central-1",
		"resource_type": "AWS::Logs::LogGroup",
		"identifier":    "demo",
	})
	require.False(t, res.IsError, text)

	assert.Equal(t, []string{"delete:demo", "wait:tok-1"}, fake.calls)
	assert.Nil(t, decodeWrite(t, text).Properties)
}

func TestUpdateSendsTheNormalizedPatch(t *testing.T) {
	fake := &fakeCloudControl{
		accepted:   event(cctypes.OperationStatusInProgress, "demo"),
		final:      event(cctypes.OperationStatusSuccess, "demo"),
		properties: `{"LogGroupName":"demo","RetentionInDays":30}`,
	}
	srv, _ := writeServer(fake)

	// A single operation object is accepted and wrapped into a patch array.
	res, text := callHandler(t, srv.handleUpdateResource, map[string]any{
		"account_id":     "111111111111",
		"region":         "eu-central-1",
		"resource_type":  "AWS::Logs::LogGroup",
		"identifier":     "demo",
		"patch_document": map[string]any{"op": "replace", "path": "/RetentionInDays", "value": 30},
	})
	require.False(t, res.IsError, text)

	assert.Equal(t, `update:demo:[{"op":"replace","path":"/RetentionInDays","value":30}]`, fake.calls[0])
	assert.EqualValues(t, 30, decodeWrite(t, text).Properties["RetentionInDays"])
}

func TestWriteRefusesAmbiguousTarget(t *testing.T) {
	fake := &fakeCloudControl{}
	srv, pool := writeServer(fake)
	pool.clients = []*v3.Client{{}, {}}

	res, text := callHandler(t, srv.handleCreateResource, logGroupArgs)
	assert.True(t, res.IsError)
	assert.Contains(t, text, "refusing to write")
	assert.Empty(t, fake.calls, "nothing may be sent when the target is ambiguous")
}

func TestRequestStatusTools(t *testing.T) {
	fake := &fakeCloudControl{final: event(cctypes.OperationStatusSuccess, "demo")}
	srv, _ := writeServer(fake)

	scope := map[string]any{"account_id": "111111111111", "region": "eu-central-1", "request_token": "tok-1"}

	res, text := callHandler(t, srv.handleGetResourceRequest, scope)
	require.False(t, res.IsError, text)
	assert.Contains(t, text, `"done": true`)
	assert.Contains(t, fake.calls, "status:tok-1")

	res, text = callHandler(t, srv.handleCancelResourceRequest, scope)
	require.False(t, res.IsError, text)
	assert.Contains(t, fake.calls, "cancel:tok-1")

	res, text = callHandler(t, srv.handleListResourceRequests, map[string]any{"account_id": "111111111111", "region": "eu-central-1", "limit": 2})
	require.False(t, res.IsError, text)

	var list requestListResult
	require.NoError(t, json.Unmarshal([]byte(text), &list))
	assert.Equal(t, 2, list.Count)
	assert.Equal(t, 3, list.Total)
}

func TestParseRequestStatuses(t *testing.T) {
	got, err := parseRequestStatuses("in-progress, failed,cancel complete")
	require.NoError(t, err)
	assert.Equal(t, []cctypes.OperationStatus{
		cctypes.OperationStatusInProgress,
		cctypes.OperationStatusFailed,
		cctypes.OperationStatusCancelComplete,
	}, got)

	got, err = parseRequestStatuses("")
	require.NoError(t, err)
	assert.Empty(t, got)
}

// fakeTagger scripts a bulk tag write.
type fakeTagger struct {
	failures map[string]rgttypes.FailureInfo
	err      error

	arns []string
	tags map[string]string
	keys []string
}

func (f *fakeTagger) TagResources(arns []string, tags map[string]string) (map[string]rgttypes.FailureInfo, error) {
	f.arns, f.tags = arns, tags
	return f.failures, f.err
}

func (f *fakeTagger) UntagResources(arns []string, keys []string) (map[string]rgttypes.FailureInfo, error) {
	f.arns, f.keys = arns, keys
	return f.failures, f.err
}

func tagServer(tagger *fakeTagger) *Server {
	srv := NewServer(context.Background(), &writeSpyPool{clients: []*v3.Client{{}}}, nil, EnableWrites())
	srv.newTagger = func(context.Context, *v3.Client) resourceTagger { return tagger }

	return srv
}

func TestTagResourcesReportsPartialFailure(t *testing.T) {
	tagger := &fakeTagger{failures: map[string]rgttypes.FailureInfo{
		"arn:aws:s3:::b": {ErrorCode: rgttypes.ErrorCodeInvalidParameterException, ErrorMessage: aws.String("unsupported"), StatusCode: 400},
	}}
	srv := tagServer(tagger)

	res, text := callHandler(t, srv.handleTagResources, map[string]any{
		"account_id": "111111111111",
		"region":     "eu-central-1",
		// Duplicates collapse; global ARNs (no region segment) are accepted.
		"arns": "arn:aws:s3:::a, arn:aws:s3:::b,arn:aws:s3:::a",
		"tags": "environment=prod, creator-team=sre",
	})
	assert.False(t, res.IsError, "a partial failure still changed resources: %s", text)

	assert.Equal(t, []string{"arn:aws:s3:::a", "arn:aws:s3:::b"}, tagger.arns)
	assert.Equal(t, map[string]string{"environment": "prod", "creator-team": "sre"}, tagger.tags)

	var out tagResult
	require.NoError(t, json.Unmarshal([]byte(text), &out))
	assert.Equal(t, 2, out.Requested)
	assert.Equal(t, 1, out.Succeeded)
	require.Len(t, out.Failed, 1)
	assert.Equal(t, "arn:aws:s3:::b", out.Failed[0].Arn)
	assert.Contains(t, strings.Join(out.Warnings, " "), "1 of 2")
}

func TestTagResourcesTotalFailureIsAnError(t *testing.T) {
	tagger := &fakeTagger{failures: map[string]rgttypes.FailureInfo{"arn:aws:s3:::a": {ErrorCode: rgttypes.ErrorCodeInternalServiceException}}}
	srv := tagServer(tagger)

	res, _ := callHandler(t, srv.handleUntagResources, map[string]any{
		"account_id": "111111111111",
		"region":     "eu-central-1",
		"arns":       []any{"arn:aws:s3:::a"},
		"keys":       []any{"temporary"},
	})
	assert.True(t, res.IsError)
	assert.Equal(t, []string{"temporary"}, tagger.keys)
}

func TestTagResourcesStoppedPartWay(t *testing.T) {
	tagger := &fakeTagger{err: errors.New("ThrottlingException")}
	srv := tagServer(tagger)

	res, text := callHandler(t, srv.handleTagResources, map[string]any{
		"account_id": "111111111111",
		"region":     "eu-central-1",
		"arns":       []any{"arn:aws:s3:::a"},
		"tags":       `{"environment":"prod"}`,
	})
	assert.True(t, res.IsError)
	assert.Contains(t, text, "ThrottlingException")
	assert.Contains(t, text, "may already be applied")
}

func TestParseTagArnsCapsTheBatch(t *testing.T) {
	arns := make([]any, 0, maxTagArns+1)
	for i := 0; i <= maxTagArns; i++ {
		arns = append(arns, "arn:aws:s3:::bucket-"+strconv.Itoa(i))
	}

	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{"arns": arns}

	_, err := parseTagArns(req, "111111111111", "eu-central-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at most")
}

func TestParseWriteResourceTypeDisclosesNormalization(t *testing.T) {
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{"resource_type": "aws::sqs::queue"}

	rt, warnings, err := parseWriteResourceType(req)
	require.NoError(t, err)
	assert.Equal(t, awscfg.ResourceType("AWS::SQS::Queue"), rt)
	assert.Contains(t, strings.Join(warnings, " "), `normalized to "AWS::SQS::Queue"`)

	req.Params.Arguments = map[string]any{"resource_type": "AWS::SQS::Queue"}
	_, warnings, err = parseWriteResourceType(req)
	require.NoError(t, err)
	assert.Empty(t, warnings)
}
