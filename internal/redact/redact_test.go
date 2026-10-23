package redact

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// AC-06 (header level; the record test scans the cassette file's bytes)
func TestRulesHeaders(t *testing.T) {
	r := NewRules([]string{"X-Tenant"})
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
	out := r.Headers(in)

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
		t.Error("Headers changed its input; the upstream needs the real value")
	}
	if a, b := r.Headers(http.Header{"X-Api-Key": {"same"}}), r.Headers(http.Header{"X-Api-Key": {"same"}}); a.Get("X-Api-Key") != b.Get("X-Api-Key") {
		t.Error("the same value redacts differently; sessions cannot be compared")
	}
}

func TestRulesHeaderNames(t *testing.T) {
	got := strings.Join(NewRules([]string{"X-Tenant"}).HeaderNames(), ",")
	want := "*-key,*-secret,*-token,authorization,cookie,proxy-authorization,set-cookie,x-api-key,x-tenant"
	if got != want {
		t.Errorf("HeaderNames() = %s, want %s", got, want)
	}
}

// spec §3.5: credentials in URLs
func TestRulesQueryAndURL(t *testing.T) {
	r := NewRules(nil)
	m := mask("s3cr3t")
	tests := []struct{ in, want string }{
		{"", ""},
		{"beta=true", "beta=true"},
		{"api_key=s3cr3t&beta=true", "api_key=" + m + "&beta=true"},
		{"access_token=s3cr3t", "access_token=" + m},
		{"X-Api-Key=s3cr3t", "X-Api-Key=" + m},
		{"key=s3cr3t&key=s3cr3t", "key=" + m + "&key=" + m},
		{"sig=s3cr3t&page=2&flag", "sig=" + m + "&page=2&flag"},
		{"monkey=1&tokens=2", "monkey=1&tokens=2"},
		{"api%5Fkey=s3cr3t", "api%5Fkey=" + m},
	}
	for _, tt := range tests {
		if got := r.Query(tt.in); got != tt.want {
			t.Errorf("Query(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}

	u, _ := url.Parse("https://user:pass@mcp.example.com/mcp?token=s3cr3t&region=eu")
	if got, want := r.URL(u), "https://REDACTED@mcp.example.com/mcp?token="+m+"&region=eu"; got != want {
		t.Errorf("URL = %q, want %q", got, want)
	}
	if u.User.Username() != "user" || u.RawQuery != "token=s3cr3t&region=eu" {
		t.Error("URL changed its input; the upstream needs the real URL")
	}
}
