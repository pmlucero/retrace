package anthropic

import (
	"net/http"
	"strings"
	"testing"
)

// AC-14: swap-model rewrites the model of the request (spec §4.2).
func TestRewriteModel(t *testing.T) {
	deep := strings.Repeat("[", 2000) + strings.Repeat("]", 2000)
	for _, tc := range []struct{ name, body, want string }{
		{"replaced", `{"model":"claude-sonnet-5-5","max_tokens":1.50,"messages":[]}`,
			`{"model":"claude-opus-5-5","max_tokens":1.50,"messages":[]}`},
		{"added", `{"max_tokens":16}`, `{"max_tokens":16,"model":"claude-opus-5-5"}`},
		{"non-string model", `{"model":7}`, `{"model":"claude-opus-5-5"}`},
		{"duplicates", `{"model":"a","x":1,"model":"b"}`, `{"model":"claude-opus-5-5","x":1,"model":"claude-opus-5-5"}`},
		{"nested model kept", `{"metadata":{"model":"a"}}`, `{"metadata":{"model":"a"},"model":"claude-opus-5-5"}`},
		{"not an object", `["model"]`, `["model"]`},
		{"not JSON", `model=a`, `model=a`},
		{"empty", ``, ``},
		{"too deep", deep, deep},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(RewriteModel([]byte(tc.body), "claude-opus-5-5")); got != tc.want {
				t.Errorf("RewriteModel(%.60s) = %.80s, want %.80s", tc.body, got, tc.want)
			}
		})
	}
}

func TestModelRewriterPaths(t *testing.T) {
	rw := ModelRewriter("m")
	const body = `{"model":"a"}`
	for _, tc := range []struct {
		method, path string
		rewritten    bool
	}{
		{http.MethodPost, "/v1/messages", true},
		{http.MethodPost, "/v1/messages/count_tokens", true},
		{http.MethodGet, "/v1/messages", false},
		{http.MethodPost, "/v1/messages/batches", false},
		{http.MethodPost, "/v1/complete", false},
		{http.MethodPost, "/v1/messages/", false},
	} {
		got := string(rw(tc.method, tc.path, []byte(body)))
		if rewritten := got != body; rewritten != tc.rewritten {
			t.Errorf("%s %s: body %s, want rewritten %v", tc.method, tc.path, got, tc.rewritten)
		}
	}
}
