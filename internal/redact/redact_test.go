package redact

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// AC-06 (header level; the record test scans the cassette file's bytes)
func TestRulesHeaders(t *testing.T) {
	r := NewRules([]string{"X-Tenant"}, nil)
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

// AC-06: the deny-list of ADR 0011, shared by headers and query parameters
func TestRulesDenyList(t *testing.T) {
	r := NewRules([]string{"X-Tenant"}, nil)
	redacted := []string{
		"Authorization", "Proxy-Authorization", "X-Api-Key", "Cookie", "Set-Cookie",
		"Apikey", "Auth", "Key", "Password", "Secret", "Sig", "Signature", "Token",
		"X-Mcp-Token", "X-Amz-Security-Token", "X-Client-Secret", "Anthropic-Key", "X-Auth", "Proxy-Auth",
		"X-Db-Password", "X-Amz-Signature", "X-Goog-Signature", "X-Amz-Credential", "X-Goog-Credential",
		"X-Apikey", "Xapikey", "X-Tenant",
	}
	kept := []string{
		"Content-Type", "Anthropic-Version", "Anthropic-Beta", "Mcp-Session-Id", "Access-Control-Allow-Credentials",
		"X-Ratelimit-Remaining-Tokens", "Authority", "Monkey", "Tokens", "Location",
	}
	for _, name := range redacted {
		if !r.RedactsHeader(name) {
			t.Errorf("RedactsHeader(%q) = false, want true", name)
		}
		if q := name + "=v"; r.Query(q) == q {
			t.Errorf("Query(%q) kept the value, want it masked", q)
		}
	}
	for _, name := range kept {
		if r.RedactsHeader(name) {
			t.Errorf("RedactsHeader(%q) = true, want false", name)
		}
		if q := name + "=v"; r.Query(q) != q {
			t.Errorf("Query(%q) = %q, want it kept", q, r.Query(q))
		}
	}
}

// AC-06: URLs in header values (ADR 0011, spec §3.5)
func TestRulesHeadersRedactURLValues(t *testing.T) {
	r := NewRules(nil, nil)
	in := http.Header{
		"Location":         {"https://app.example.com/cb?code=c1&access_token=tok1&state=s"},
		"Content-Location": {"https://user:pw@files.example.com/a.txt"},
		"Referer":          {"https://b.s3.amazonaws.com/k?X-Amz-Credential=cred1&X-Amz-Signature=sig1&X-Amz-Expires=60"},
		"Link": {`<https://api.example.com/items?page=2&token=tok2>; rel="next", </items?page=1>; rel="prev"`,
			`<https://u:p@h/x>; rel=alternate`, `<https://h/x?token=t`},
		"X-Next": {"https://h/x?token=custom-header-not-a-url-header"},
		"Plain":  {"/relative/path?page=3"},
	}
	in["Location-Plain"] = []string{"/a?page=1"}
	out := r.Headers(in)
	want := map[string][]string{
		"Location":         {"https://app.example.com/cb?code=c1&access_token=" + mask("tok1") + "&state=s"},
		"Content-Location": {"https://REDACTED@files.example.com/a.txt"},
		"Referer":          {"https://b.s3.amazonaws.com/k?X-Amz-Credential=" + mask("cred1") + "&X-Amz-Signature=" + mask("sig1") + "&X-Amz-Expires=60"},
		"Link": {`<https://api.example.com/items?page=2&token=` + mask("tok2") + `>; rel="next", </items?page=1>; rel="prev"`,
			`<https://REDACTED@h/x>; rel=alternate`, `<https://h/x?token=` + mask("t")},
		"X-Next":         in["X-Next"],
		"Plain":          in["Plain"],
		"Location-Plain": in["Location-Plain"],
	}
	for name, w := range want {
		if got := out[name]; strings.Join(got, "\n") != strings.Join(w, "\n") {
			t.Errorf("%s = %q, want %q", name, got, w)
		}
	}
	if in.Get("Location") != "https://app.example.com/cb?code=c1&access_token=tok1&state=s" {
		t.Error("Headers changed its input; the agent needs the real URL")
	}
	// A value that is not a URL cannot be checked, so it is masked whole.
	if got := r.Headers(http.Header{"Location": {"https://h/%zz?token=t"}}).Get("Location"); got != mask("https://h/%zz?token=t") {
		t.Errorf("unparsable Location = %q, want it masked whole", got)
	}
	// A URL-carrying header on the deny-list is masked whole.
	if got := NewRules([]string{"Referer"}, nil).Headers(http.Header{"Referer": {"https://h/"}}).Get("Referer"); got != mask("https://h/") {
		t.Errorf("Referer on the deny-list = %q, want it masked whole", got)
	}
}

func TestRulesHeaderNames(t *testing.T) {
	got := strings.Join(NewRules([]string{"X-Tenant"}, nil).HeaderNames(), ",")
	want := "*-auth,*-credential,*-key,*-password,*-secret,*-signature,*-token,*apikey," +
		"apikey,auth,authorization,cookie,key,password,proxy-authorization,secret,set-cookie,sig,signature,token,x-api-key,x-tenant"
	if got != want {
		t.Errorf("HeaderNames() = %s, want %s", got, want)
	}
}

// spec §7.2: replay rebuilds the recording's rules from the cassette, and
// any loss would change every fingerprint (spec §5).
func TestRulesMetadataRoundTrip(t *testing.T) {
	email, err := ParsePath("$..email")
	if err != nil {
		t.Fatal(err)
	}
	quoted, err := ParsePath("$['a b'][*]")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []*Rules{
		NewRules(nil, nil),
		NewRules([]string{"X-Tenant"}, []Path{email, quoted}),
	} {
		stored := r.Metadata()
		back, err := RulesFromMetadata(stored)
		if err != nil {
			t.Fatalf("%s: %v", stored, err)
		}
		if got := back.Metadata(); string(got) != string(stored) {
			t.Errorf("round trip of %s gave %s", stored, got)
		}
	}
	if got := string(NewRules(nil, []Path{email}).Metadata()); !strings.HasPrefix(got, `{"body":["$..email"],"headers":["*-auth",`) {
		t.Errorf("Metadata() = %s", got)
	}
}

// ADR 0011: a cassette written with the older defaults is read with the
// current ones, plus its stored names, plus the names run --redact-header adds.
func TestRulesFromMetadataAddsCurrentDefaults(t *testing.T) {
	old := `{"body":["$..email"],"headers":["*-key","*-secret","*-token","authorization","cookie","proxy-authorization","set-cookie","x-api-key","x-tenant"]}`
	r, err := RulesFromMetadata([]byte(old), "X-Run-Only")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"X-Tenant", "X-Run-Only", "Apikey", "X-Amz-Signature"} {
		if !r.RedactsHeader(name) {
			t.Errorf("RedactsHeader(%q) = false", name)
		}
	}
	want := `{"body":["$..email"],"headers":["*-auth","*-credential","*-key","*-password","*-secret","*-signature","*-token","*apikey",` +
		`"apikey","auth","authorization","cookie","key","password","proxy-authorization","secret","set-cookie","sig","signature","token","x-api-key","x-run-only","x-tenant"]}`
	if got := string(r.Metadata()); got != want {
		t.Errorf("Metadata() = %s, want %s", got, want)
	}
}

// spec §4.4: a pattern stored by a newer binary is applied and kept.
func TestRulesFromMetadataKeepsUnknownPatterns(t *testing.T) {
	r, err := RulesFromMetadata([]byte(`{"body":[],"headers":["*-Future"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !r.RedactsHeader("X-Future") || r.Query("x_future=v") == "x_future=v" {
		t.Error("the stored *-future pattern is not applied")
	}
	if got := string(r.Metadata()); !strings.Contains(got, `"*-future"`) {
		t.Errorf("Metadata() = %s, want the stored pattern kept", got)
	}
}

// The metadata comes from a cassette, which is untrusted input.
func TestRulesFromMetadataRejectsInvalidMetadata(t *testing.T) {
	for _, stored := range []string{``, `[]`, `{"body":[1]}`, `{"body":["user.email"]}`} {
		if _, err := RulesFromMetadata([]byte(stored)); err == nil {
			t.Errorf("RulesFromMetadata(%s) succeeded", stored)
		}
	}
}

// spec §3.5: credentials in URLs
func TestRulesQueryAndURL(t *testing.T) {
	r := NewRules(nil, nil)
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

// Spec §4.3: a stored URL that holds a redaction marker needs --mcp.
func TestHasMarker(t *testing.T) {
	r := NewRules(nil, nil)
	secret, err := url.Parse("https://user:pw@mcp.example.com/mcp?api_key=sk-1&x=1")
	if err != nil {
		t.Fatal(err)
	}
	stored, err := url.Parse(r.URL(secret))
	if err != nil {
		t.Fatal(err)
	}
	if !HasMarker(stored) {
		t.Errorf("HasMarker(%s) = false for a URL stored by URL", stored)
	}
	for _, tc := range []struct {
		raw  string
		want bool
	}{
		{"https://REDACTED@h/mcp", true},
		{"https://REDACTED:pw@h/mcp", false},
		{"https://user@h/mcp", false},
		{"https://h/mcp?k=[REDACTED:0a1b2c3d]", true},
		{"https://h/mcp?k=0a1b2c3d]", false},
		{"https://h/mcp?k=[REDACTED:0a1b2c3d", false},
		{"https://h/mcp?k=%5BREDACTED:0A1B2C3D%5D", true},
		{"https://h/mcp?a=1&k=[REDACTED:0a1b2c3d]", true},
		{"https://h/mcp?k=[REDACTED:0a1b2c3]", false},
		{"https://h/mcp?k=[REDACTED:0a1b2c3g]", false},
		{"https://h/mcp?[REDACTED:0a1b2c3d]", false},
		{"https://h/mcp", false},
	} {
		u, err := url.Parse(tc.raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := HasMarker(u); got != tc.want {
			t.Errorf("HasMarker(%s) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}
