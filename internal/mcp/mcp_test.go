package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/store"
)

// handle is the test shorthand: one request in, the decoded response out.
func handle(t *testing.T, s *Server, body string) *Response {
	t.Helper()
	return handleAs(t, s, []string{string(auth.PermRoot)}, body)
}

func handleAs(t *testing.T, s *Server, permissions []string, body string) *Response {
	t.Helper()
	return s.Handle(context.Background(), 1, permissions, []byte(body))
}

func TestInitializeAdvertisesToolsAndVersion(t *testing.T) {
	s := New("1.2.3")
	resp := handle(t, s, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if resp == nil || resp.Error != nil {
		t.Fatalf("initialize failed: %+v", resp)
	}
	result := resp.Result.(map[string]any)
	if result["protocolVersion"] != ProtocolVersion {
		t.Fatalf("protocolVersion = %v, want %s", result["protocolVersion"], ProtocolVersion)
	}
	info := result["serverInfo"].(map[string]any)
	if info["name"] != ServerName || info["version"] != "1.2.3" {
		t.Fatalf("serverInfo = %+v", info)
	}
	if _, ok := result["capabilities"].(map[string]any)["tools"]; !ok {
		t.Fatalf("the server must advertise the tools capability: %+v", result)
	}
}

// A notification (no id) must produce NO response — sending one back is a
// protocol violation that some clients treat as fatal.
func TestNotificationGetsNoResponse(t *testing.T) {
	s := New("test")
	for _, body := range []string{
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","method":"initialize","params":{}}`,
	} {
		if resp := handle(t, s, body); resp != nil {
			t.Fatalf("notification %s produced a response: %+v", body, resp)
		}
	}
}

func TestToolsListAndCall(t *testing.T) {
	s := New("test")
	s.Register(Tool{
		Name: "echo", Description: "echoes", InputSchema: ObjectSchema(map[string]any{"who": StringProp("who")}),
	}, func(_ context.Context, teamID int64, args map[string]any) (any, error) {
		who, _ := StringArg(args, "who")
		return map[string]any{"team": teamID, "hello": who}, nil
	})

	list := handle(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	tools := list.Result.(map[string]any)["tools"].([]Tool)
	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("tools/list = %+v", tools)
	}

	call := handle(t, s, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{"who":"world"}}}`)
	result := call.Result.(map[string]any)
	if result["isError"] != false {
		t.Fatalf("call flagged as error: %+v", result)
	}
	text := result["content"].([]map[string]any)[0]["text"].(string)
	if !strings.Contains(text, `"hello": "world"`) || !strings.Contains(text, `"team": 1`) {
		t.Fatalf("tool result = %s", text)
	}
}

func TestToolsAreFilteredAndEnforcedByPermission(t *testing.T) {
	s := New("test")
	called := false
	s.Register(Tool{
		Name: "servers", InputSchema: ObjectSchema(nil),
		RequiredPermissions: []auth.Permission{auth.PermServersRead},
	}, func(context.Context, int64, map[string]any) (any, error) {
		called = true
		return map[string]any{"kind": "server"}, nil
	})
	s.Register(Tool{
		Name: "applications", InputSchema: ObjectSchema(nil),
		RequiredPermissions: []auth.Permission{auth.PermApplicationsRead},
	}, func(context.Context, int64, map[string]any) (any, error) {
		return map[string]any{"kind": "application"}, nil
	})

	appOnly := []string{string(auth.PermApplicationsRead)}
	list := handleAs(t, s, appOnly, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	tools := list.Result.(map[string]any)["tools"].([]Tool)
	if len(tools) != 1 || tools[0].Name != "applications" {
		t.Fatalf("application-only tools/list = %+v", tools)
	}

	denied := handleAs(t, s, appOnly,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"servers"}}`)
	if denied.Error == nil || denied.Error.Code != codeInvalidParams {
		t.Fatalf("forbidden tool call = %+v, want unavailable-tool error", denied)
	}
	if called {
		t.Fatal("forbidden server handler was executed")
	}

	allowed := handleAs(t, s, appOnly,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"applications"}}`)
	if allowed.Error != nil || allowed.Result.(map[string]any)["isError"] != false {
		t.Fatalf("allowed application call failed: %+v", allowed)
	}
}

// A failing tool is a RESULT flagged isError, never a protocol error: the
// assistant must see the message and be able to react.
func TestToolFailureIsAResultNotAProtocolError(t *testing.T) {
	s := New("test")
	s.Register(Tool{Name: "boom", InputSchema: ObjectSchema(nil)},
		func(context.Context, int64, map[string]any) (any, error) {
			return nil, context.DeadlineExceeded
		})
	resp := handle(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"boom"}}`)
	if resp.Error != nil {
		t.Fatalf("a tool failure must not be a protocol error: %+v", resp.Error)
	}
	if resp.Result.(map[string]any)["isError"] != true {
		t.Fatalf("result not flagged isError: %+v", resp.Result)
	}
}

func TestProtocolErrors(t *testing.T) {
	s := New("test")
	cases := map[string]int{
		`not json`: codeParseError,
		`{"jsonrpc":"1.0","id":1,"method":"ping"}`:                                  codeInvalidRequest,
		`{"jsonrpc":"2.0","id":1,"method":"nope"}`:                                  codeMethodNotFound,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{}}`:                codeInvalidParams,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"absent"}}`: codeInvalidParams,
	}
	for body, want := range cases {
		resp := handle(t, s, body)
		if resp == nil || resp.Error == nil || resp.Error.Code != want {
			t.Fatalf("%s → %+v, want error code %d", body, resp, want)
		}
	}
}

func TestPingAnswersEmptyResult(t *testing.T) {
	s := New("test")
	resp := handle(t, s, `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	if resp == nil || resp.Error != nil {
		t.Fatalf("ping → %+v", resp)
	}
	if result, ok := resp.Result.(map[string]any); !ok || len(result) != 0 {
		t.Fatalf("ping result = %+v, want an empty object", resp.Result)
	}
}

// Known methods sent WITHOUT an id are notifications too: none may answer, and
// a tools/call notification must not even run the tool.
func TestKnownMethodNotificationsStaySilent(t *testing.T) {
	s := New("test")
	called := false
	s.Register(Tool{Name: "echo", InputSchema: ObjectSchema(nil)},
		func(context.Context, int64, map[string]any) (any, error) {
			called = true
			return map[string]any{}, nil
		})
	for _, body := range []string{
		`{"jsonrpc":"2.0","method":"ping"}`,
		`{"jsonrpc":"2.0","method":"tools/list"}`,
		`{"jsonrpc":"2.0","method":"tools/call","params":{"name":"echo"}}`,
	} {
		if resp := handle(t, s, body); resp != nil {
			t.Fatalf("notification %s produced a response: %+v", body, resp)
		}
	}
	if called {
		t.Fatal("a tools/call notification must not execute the tool")
	}
}

// A tool returning a value JSON cannot encode is the one internal error the
// call path can produce.
func TestUnencodableToolResultIsInternalError(t *testing.T) {
	s := New("test")
	s.Register(Tool{Name: "chan", InputSchema: ObjectSchema(nil)},
		func(context.Context, int64, map[string]any) (any, error) {
			return make(chan int), nil // json.Marshal cannot encode a channel
		})
	resp := handle(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"chan"}}`)
	if resp == nil || resp.Error == nil || resp.Error.Code != codeInternalError {
		t.Fatalf("unencodable result → %+v, want internal error", resp)
	}
}

func TestPageSizeBounds(t *testing.T) {
	if got := PageSize(nil); got != DefaultPageSize {
		t.Fatalf("no limit → %d, want the default %d", got, DefaultPageSize)
	}
	// JSON numbers decode as float64 — the shape the server actually receives.
	var args map[string]any
	_ = json.Unmarshal([]byte(`{"limit": 500}`), &args)
	if got := PageSize(args); got != MaxPageSize {
		t.Fatalf("limit 500 → %d, want the cap %d", got, MaxPageSize)
	}
	_ = json.Unmarshal([]byte(`{"limit": 7}`), &args)
	if got := PageSize(args); got != 7 {
		t.Fatalf("limit 7 → %d", got)
	}
	_ = json.Unmarshal([]byte(`{"limit": -3}`), &args)
	if got := PageSize(args); got != DefaultPageSize {
		t.Fatalf("negative limit → %d, want the default", got)
	}
}

// PageSize accepts the number shapes a caller may hand-build (int and
// json.Number besides the float64 JSON produces) and ignores everything else.
func TestPageSizeAcceptsEveryNumberShape(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
		want int32
	}{
		{"go int", map[string]any{"limit": 9}, 9},
		{"json.Number", map[string]any{"limit": json.Number("11")}, 11},
		{"invalid json.Number", map[string]any{"limit": json.Number("x")}, DefaultPageSize},
		{"not a number", map[string]any{"limit": "twelve"}, DefaultPageSize},
	}
	for _, tc := range cases {
		if got := PageSize(tc.args); got != tc.want {
			t.Fatalf("%s → %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestStringArg(t *testing.T) {
	args := map[string]any{"name": "web", "empty": "", "number": 3.0}
	if v, ok := StringArg(args, "name"); !ok || v != "web" {
		t.Fatalf("name → (%q, %v)", v, ok)
	}
	for _, key := range []string{"absent", "empty", "number"} {
		if _, ok := StringArg(args, key); ok {
			t.Fatalf("%s must not read as a usable string", key)
		}
	}
}

func TestRequireUUID(t *testing.T) {
	if _, err := RequireUUID(map[string]any{}, "uuid"); err == nil || !strings.Contains(err.Error(), "uuid is required") {
		t.Fatalf("missing uuid → %v", err)
	}
	value, err := RequireUUID(map[string]any{"uuid": "abc"}, "uuid")
	if err != nil || value != "abc" {
		t.Fatalf("present uuid → (%q, %v)", value, err)
	}
}

// Every registered tool must be read-only by NAME as well as by behavior: a
// tool called deploy/restart/delete would contradict ADR-043 whatever it does.
func TestNoMutatingToolNames(t *testing.T) {
	s := New("test")
	RegisterTools(s, nil) // registration touches no store
	if len(s.Tools()) != 10 {
		t.Fatalf("registered %d tools, want the 10 of PRD §12", len(s.Tools()))
	}
	forbidden := []string{"deploy", "restart", "stop", "delete", "create", "update", "reveal", "secret", "rotate"}
	for _, tool := range s.Tools() {
		for _, word := range forbidden {
			if strings.Contains(tool.Name, word) {
				t.Fatalf("tool %q looks mutating — MCP is read-only (ADR-043)", tool.Name)
			}
		}
		if tool.Description == "" || tool.InputSchema == nil {
			t.Fatalf("tool %q must document itself: an assistant reads this to decide", tool.Name)
		}
		if len(tool.RequiredPermissions) == 0 {
			t.Fatalf("tool %q has no RBAC permission", tool.Name)
		}
	}
}

func TestRegisteredToolsFollowTeamRoles(t *testing.T) {
	s := New("test")
	RegisterTools(s, nil)
	list := func(permissions []string) []Tool {
		t.Helper()
		resp := handleAs(t, s, permissions, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
		return resp.Result.(map[string]any)["tools"].([]Tool)
	}

	// reviewer holds the read-only path to previews (ADR-059): it discovers the
	// project/application inventory tools — and nothing touching servers,
	// databases or compose stacks.
	reviewer := auth.ExpandGranular(auth.PermissionsForRole(store.TeamRoleReviewer))
	reviewerTools := list(reviewer)
	if len(reviewerTools) != 3 ||
		reviewerTools[0].Name != "list_projects" ||
		reviewerTools[1].Name != "list_applications" ||
		reviewerTools[2].Name != "get_application" {
		t.Fatalf("reviewer tools = %+v", reviewerTools)
	}

	appOnly := auth.ExpandGranular([]string{string(auth.PermApplicationsRead)})
	tools := list(appOnly)
	if len(tools) != 2 || tools[0].Name != "list_applications" || tools[1].Name != "get_application" {
		t.Fatalf("application-only role tools = %+v", tools)
	}

	member := auth.ExpandGranular(auth.PermissionsForRole(store.TeamRoleMember))
	if tools := list(member); len(tools) != 10 {
		t.Fatalf("member discovered %d tools, want all 10", len(tools))
	}
}
