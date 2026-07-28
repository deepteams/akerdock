// Package mcp implements the built-in Model Context Protocol server
// (ADR-043, PRD §12): JSON-RPC 2.0 over Streamable HTTP, ten READ-ONLY tools
// scoped to one team. Nothing here mutates state, reads a secret value or
// touches an `*_enc` column — an assistant inventories and diagnoses, it never
// acts.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
)

// ProtocolVersion is the MCP revision this server implements.
const ProtocolVersion = "2025-06-18"

// ServerName and ServerVersion identify the server at initialize.
const ServerName = "akerdock"

// Page bounds: 50 by default, 100 maximum (PRD §12) — an assistant asking for
// "everything" gets a bounded, paginated answer instead of the whole estate.
const (
	DefaultPageSize = 50
	MaxPageSize     = 100
)

// JSON-RPC 2.0 error codes used here (the standard set plus none of our own:
// a tool failure is a tool result flagged isError, not a protocol error).
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

// Request is one JSON-RPC request or notification (no id).
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response is one JSON-RPC response.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError is a protocol-level failure.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Tool is one exposed capability: a name, a human description and the JSON
// Schema of its arguments — what an assistant reads to decide what to call.
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// Handler runs a tool for an authenticated caller. args is the raw arguments
// object; the returned value is marshalled as the tool's text content.
type Handler func(ctx context.Context, teamID int64, args map[string]any) (any, error)

// Server routes JSON-RPC methods to the registered tools.
type Server struct {
	Version string
	tools   []Tool
	handler map[string]Handler
}

// New builds a server with no tools; call Register for each.
func New(version string) *Server {
	return &Server{Version: version, handler: map[string]Handler{}}
}

// Register adds a tool. A duplicate name replaces the previous handler —
// registration happens once at wiring time, never concurrently with serving.
func (s *Server) Register(tool Tool, h Handler) {
	if _, exists := s.handler[tool.Name]; !exists {
		s.tools = append(s.tools, tool)
	}
	s.handler[tool.Name] = h
}

// Tools returns the registered tools, in registration order.
func (s *Server) Tools() []Tool { return s.tools }

// Handle answers one JSON-RPC request. A nil response means the message was a
// notification: nothing to send back.
func (s *Server) Handle(ctx context.Context, teamID int64, raw []byte) *Response {
	var req Request
	if err := json.Unmarshal(raw, &req); err != nil {
		return errorResponse(nil, codeParseError, "invalid JSON")
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		return errorResponse(req.ID, codeInvalidRequest, "not a JSON-RPC 2.0 request")
	}
	// A notification (no id) expects no answer — `notifications/initialized`
	// is the one every client sends right after initialize.
	notification := len(req.ID) == 0

	switch req.Method {
	case "initialize":
		if notification {
			return nil
		}
		return &Response{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
			"protocolVersion": ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": ServerName, "version": s.Version},
			"instructions": "Read-only inventory of an AkerDock instance, scoped to one team. " +
				"Use overview first, then the list_* tools; get_* tools take the uuid a list returned. " +
				"No tool can deploy, restart or reveal a secret.",
		}}
	case "ping":
		if notification {
			return nil
		}
		return &Response{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{}}
	case "tools/list":
		if notification {
			return nil
		}
		return &Response{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{"tools": s.tools}}
	case "tools/call":
		if notification {
			return nil
		}
		return s.call(ctx, teamID, req)
	default:
		if notification {
			return nil // unknown notifications are ignored, per the protocol
		}
		return errorResponse(req.ID, codeMethodNotFound, "unknown method "+req.Method)
	}
}

func (s *Server) call(ctx context.Context, teamID int64, req Request) *Response {
	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil || params.Name == "" {
		return errorResponse(req.ID, codeInvalidParams, "a tool name is required")
	}
	h, ok := s.handler[params.Name]
	if !ok {
		return errorResponse(req.ID, codeInvalidParams, "unknown tool "+params.Name)
	}
	out, err := h(ctx, teamID, params.Arguments)
	if err != nil {
		// A tool failure is a RESULT flagged isError, not a protocol error:
		// the assistant must see what failed and be able to react.
		return &Response{JSONRPC: "2.0", ID: req.ID, Result: toolResult(err.Error(), true)}
	}
	body, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return errorResponse(req.ID, codeInternalError, "cannot encode the tool result")
	}
	return &Response{JSONRPC: "2.0", ID: req.ID, Result: toolResult(string(body), false)}
}

func toolResult(text string, isError bool) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isError,
	}
}

func errorResponse(id json.RawMessage, code int, message string) *Response {
	return &Response{JSONRPC: "2.0", ID: id, Error: &RPCError{Code: code, Message: message}}
}

// PageSize reads the `limit` argument within the documented bounds.
func PageSize(args map[string]any) int32 {
	limit := DefaultPageSize
	if raw, ok := args["limit"]; ok {
		if n, ok := toInt(raw); ok && n > 0 {
			limit = n
		}
	}
	if limit > MaxPageSize {
		limit = MaxPageSize
	}
	return int32(limit)
}

// StringArg reads a string argument; ok is false when absent or empty.
func StringArg(args map[string]any, name string) (string, bool) {
	raw, ok := args[name]
	if !ok {
		return "", false
	}
	s, ok := raw.(string)
	return s, ok && s != ""
}

// RequireUUID reads a required uuid-shaped argument.
func RequireUUID(args map[string]any, name string) (string, error) {
	value, ok := StringArg(args, name)
	if !ok {
		return "", fmt.Errorf("%s is required", name)
	}
	return value, nil
}

func toInt(raw any) (int, bool) {
	switch v := raw.(type) {
	case float64: // every JSON number decodes as float64
		return int(v), true
	case int:
		return v, true
	case json.Number:
		n, err := v.Int64()
		return int(n), err == nil
	default:
		return 0, false
	}
}

// ObjectSchema builds an inputSchema for a tool: properties by name, with the
// listed ones required.
func ObjectSchema(properties map[string]any, required ...string) map[string]any {
	schema := map[string]any{"type": "object", "properties": properties}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

// StringProp and IntProp are the two property shapes the tools need.
func StringProp(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

func IntProp(description string) map[string]any {
	return map[string]any{"type": "integer", "description": description}
}
