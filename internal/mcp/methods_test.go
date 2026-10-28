package mcp

import (
	"encoding/json"
	"testing"
)

// Spec §7.4: tool_call takes the tool name and arguments from the params.
func TestParseToolCall(t *testing.T) {
	tests := []struct {
		name, in   string
		tool, args string
		wantErr    bool
	}{
		{name: "name and arguments", in: `{"name":"get_forecast","arguments":{"city":"Lima","days":3}}`, tool: "get_forecast", args: `{"city":"Lima","days":3}`},
		{name: "no arguments", in: `{"name":"now","_meta":{"progressToken":1}}`, tool: "now"},
		{name: "redacted name", in: `{"name":5,"arguments":"[REDACTED:0123abcd]"}`, args: `"[REDACTED:0123abcd]"`},
		{name: "not an object", in: `"[REDACTED:0123abcd]"`, wantErr: true},
		{name: "absent", in: ``, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseToolCall(json.RawMessage(tt.in))
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseToolCall error = %v, want error %t", err, tt.wantErr)
			}
			if got.Name != tt.tool || string(got.Arguments) != tt.args {
				t.Errorf("ParseToolCall = %q %s, want %q %s", got.Name, got.Arguments, tt.tool, tt.args)
			}
		})
	}
}

// Spec §7.4: tool_result takes the result, and isError makes its status error.
func TestParseToolResult(t *testing.T) {
	tests := []struct {
		name, in            string
		content, structured string
		isError, wantErr    bool
	}{
		{name: "content", in: `{"content":[{"type":"text","text":"sunny"}]}`, content: `[{"type":"text","text":"sunny"}]`},
		{name: "tool error", in: `{"content":[],"isError":true}`, content: `[]`, isError: true},
		{name: "structured", in: `{"content":[],"structuredContent":{"t":21}}`, content: `[]`, structured: `{"t":21}`},
		{name: "isError not a bool", in: `{"isError":"yes"}`},
		{name: "not an object", in: `[1]`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseToolResult(json.RawMessage(tt.in))
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseToolResult error = %v, want error %t", err, tt.wantErr)
			}
			if string(got.Content) != tt.content || string(got.StructuredContent) != tt.structured || got.IsError != tt.isError {
				t.Errorf("ParseToolResult = %s %s %t, want %s %s %t",
					got.Content, got.StructuredContent, got.IsError, tt.content, tt.structured, tt.isError)
			}
		})
	}
}

func TestParseError(t *testing.T) {
	tests := []struct {
		name, in string
		code     int64
		message  string
		data     string
		wantErr  bool
	}{
		{name: "full", in: `{"code":-32602,"message":"Unknown tool","data":{"tool":"x"}}`, code: -32602, message: "Unknown tool", data: `{"tool":"x"}`},
		{name: "code not an integer", in: `{"code":"[REDACTED:0123abcd]","message":"m"}`, message: "m"},
		{name: "fractional code", in: `{"code":1.5,"message":"m"}`, message: "m"},
		{name: "not an object", in: `"oops"`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseError(json.RawMessage(tt.in))
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseError error = %v, want error %t", err, tt.wantErr)
			}
			if got.Code != tt.code || got.Message != tt.message || string(got.Data) != tt.data {
				t.Errorf("ParseError = %d %q %s, want %d %q %s", got.Code, got.Message, got.Data, tt.code, tt.message, tt.data)
			}
		})
	}
}

// Spec §3.4, §7.2: the protocol version and server info of initialize.
func TestParseInitializeResult(t *testing.T) {
	in := `{"protocolVersion":"2025-06-18","capabilities":{"tools":{}},"serverInfo":{"name":"weather","version":"1.0.0"}}`
	got, err := ParseInitializeResult(json.RawMessage(in))
	if err != nil {
		t.Fatal(err)
	}
	if got.ProtocolVersion != "2025-06-18" || string(got.ServerInfo) != `{"name":"weather","version":"1.0.0"}` {
		t.Errorf("ParseInitializeResult = %q %s", got.ProtocolVersion, got.ServerInfo)
	}
	if _, err := ParseInitializeResult(json.RawMessage(`[]`)); err == nil {
		t.Error("ParseInitializeResult([]) = nil error, want an error")
	}
}
