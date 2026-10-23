package redact

import (
	"net/http"
	"strings"
	"testing"
)

// AC-06 (header level; the record test scans the cassette file's bytes)
func TestHeadersApply(t *testing.T) {
	h := NewHeaders([]string{"X-Tenant"})
	in := http.Header{
		"Authorization":   {"Bearer sk-secret"},
		"X-Api-Key":       {"sk-ant-123"},
		"Cookie":          {"session=1"},
		"Set-Cookie":      {"a=1", "b=2"},
		"X-Mcp-Token":     {"tok"},
		"X-Client-Secret": {"s"},
		"Anthropic-Key":   {"k"},
		"X-Tenant":        {"acme"},
		"Content-Type":    {"application/json"},
		"Anthropic-Beta":  {"tools-2024"},
	}
	out := h.Apply(in)

	for _, name := range []string{"Authorization", "X-Api-Key", "Cookie", "X-Mcp-Token", "X-Client-Secret", "Anthropic-Key", "X-Tenant"} {
		if got := out.Get(name); !strings.HasPrefix(got, "[REDACTED:") || len(got) != len("[REDACTED:12345678]") {
			t.Errorf("%s = %q, want [REDACTED:<8 hex>]", name, got)
		}
	}
	if got := out.Values("Set-Cookie"); len(got) != 2 || got[0] == got[1] {
		t.Errorf("Set-Cookie = %q, want each value redacted with its own hash", got)
	}
	for _, name := range []string{"Content-Type", "Anthropic-Beta"} {
		if out.Get(name) != in.Get(name) {
			t.Errorf("%s = %q, want it kept", name, out.Get(name))
		}
	}
	if in.Get("X-Api-Key") != "sk-ant-123" {
		t.Error("Apply changed its input; the upstream needs the real value")
	}
	if a, b := h.Apply(http.Header{"X-Api-Key": {"same"}}), h.Apply(http.Header{"X-Api-Key": {"same"}}); a.Get("X-Api-Key") != b.Get("X-Api-Key") {
		t.Error("the same value redacts differently; sessions cannot be compared")
	}
}

func TestHeadersNames(t *testing.T) {
	got := strings.Join(NewHeaders([]string{"X-Tenant"}).Names(), ",")
	want := "*-key,*-secret,*-token,authorization,cookie,proxy-authorization,set-cookie,x-api-key,x-tenant"
	if got != want {
		t.Errorf("Names() = %s, want %s", got, want)
	}
}
