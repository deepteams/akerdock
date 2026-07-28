package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// handle is the test shorthand: one request in, the decoded response out.
func handle(t *testing.T, s *Server, body string) *Response {
	t.Helper()
	return s.Handle(context.Background(), 1, []byte(body))
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
	}
}
