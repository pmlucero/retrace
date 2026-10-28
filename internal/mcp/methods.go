package mcp

import (
	"encoding/json"
	"fmt"
)

// The methods whose content event derivation and session metadata read
// (spec §3.4, §7.4). Every other method is opaque.
const (
	MethodInitialize = "initialize"
	MethodToolsCall  = "tools/call"
)

// ToolCall is what derivation needs from the params of a tools/call request.
type ToolCall struct {
	Name      string          // empty when absent
	Arguments json.RawMessage // nil when absent
}

// ParseToolCall reads the params of a tools/call request.
func ParseToolCall(params json.RawMessage) (ToolCall, error) {
	var raw struct {
		Name      json.RawMessage `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &raw); err != nil {
		return ToolCall{}, fmt.Errorf("parsing tools/call params: %w", err)
	}
	return ToolCall{Name: stringValue(raw.Name), Arguments: raw.Arguments}, nil
}

// ToolResult is what derivation needs from the result of a tools/call
// response.
type ToolResult struct {
	Content           json.RawMessage // nil when absent
	StructuredContent json.RawMessage // nil when absent
	// IsError reports a tool execution error, as opposed to a JSON-RPC
	// error, which has no result.
	IsError bool
}

// ParseToolResult reads the result of a tools/call response.
func ParseToolResult(result json.RawMessage) (ToolResult, error) {
	var raw struct {
		Content           json.RawMessage `json:"content"`
		StructuredContent json.RawMessage `json:"structuredContent"`
		IsError           json.RawMessage `json:"isError"`
	}
	if err := json.Unmarshal(result, &raw); err != nil {
		return ToolResult{}, fmt.Errorf("parsing tools/call result: %w", err)
	}
	var isError bool
	_ = json.Unmarshal(raw.IsError, &isError) // not a bool: not an error
	return ToolResult{Content: raw.Content, StructuredContent: raw.StructuredContent, IsError: isError}, nil
}

// RPCError is a JSON-RPC error object.
type RPCError struct {
	Code    int64           // 0 when absent or not an integer
	Message string          // empty when absent
	Data    json.RawMessage // nil when absent
}

// ParseError reads the error member of a JSON-RPC response.
func ParseError(errObj json.RawMessage) (RPCError, error) {
	var raw struct {
		Code    json.RawMessage `json:"code"`
		Message json.RawMessage `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(errObj, &raw); err != nil {
		return RPCError{}, fmt.Errorf("parsing JSON-RPC error: %w", err)
	}
	var code int64
	_ = json.Unmarshal(raw.Code, &code) // not an integer: 0
	return RPCError{Code: code, Message: stringValue(raw.Message), Data: raw.Data}, nil
}

// InitializeResult is what the session metadata keeps from the result of
// initialize (spec §7.2).
type InitializeResult struct {
	ProtocolVersion string          // empty when absent
	ServerInfo      json.RawMessage // nil when absent
}

// ParseInitializeResult reads the result of an initialize response.
func ParseInitializeResult(result json.RawMessage) (InitializeResult, error) {
	var raw struct {
		ProtocolVersion json.RawMessage `json:"protocolVersion"`
		ServerInfo      json.RawMessage `json:"serverInfo"`
	}
	if err := json.Unmarshal(result, &raw); err != nil {
		return InitializeResult{}, fmt.Errorf("parsing initialize result: %w", err)
	}
	return InitializeResult{ProtocolVersion: stringValue(raw.ProtocolVersion), ServerInfo: raw.ServerInfo}, nil
}

// stringValue returns v as a string, or "" when it is not a JSON string.
func stringValue(v json.RawMessage) string {
	var s string
	if json.Unmarshal(v, &s) != nil {
		return ""
	}
	return s
}
