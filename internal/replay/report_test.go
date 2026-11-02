package replay

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/pmlucero/retrace/internal/redact"
)

// AC-15: the divergence report shows the first miss and counts them all
// (spec §6.4).
func TestDivergenceReportKeepsTheFirstMiss(t *testing.T) {
	r := newReplayHarness(t, sourceCassette(t, recorded{request: `{"q":"a"}`, body: `{}`}), nil, Options{})
	if _, _, ok := r.h.rc.divergence.Report(); ok {
		t.Fatal("Report before any miss: ok")
	}
	r.post(`{"q":"b"}`)
	r.post(`{"q":"c"}`)
	first, misses, ok := r.h.rc.divergence.Report()
	if !ok || misses != 2 {
		t.Fatalf("Report = %+v, %d, %v; want the first of 2 misses", first, misses, ok)
	}
	diff, err := json.Marshal(first.Diff)
	if err != nil {
		t.Fatal(err)
	}
	if first.Upstream != "anthropic" || first.Request != "POST /v1/messages" || first.NoDiff != "" ||
		string(diff) != `[{"path":"$.q","expected":"a","received":"b"}]` {
		t.Errorf("first miss = %+v, diff %s; want the request with q b", first, diff)
	}
}

// Spec §6.4: a report without a diff says why.
func TestDivergenceReportSaysWhyThereIsNoDiff(t *testing.T) {
	secret, err := redact.ParsePath("$.secret")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		src     []recorded
		rules   *redact.Rules
		before  []string // requests served before the miss
		request string
		want    string
	}{
		{"incomplete", []recorded{{request: `{"q":"a"}`, body: `{}`, incomplete: true}}, nil, nil, `{"q":"a"}`, NoDiffIncomplete},
		{"unredactable", []recorded{{request: `{"q":"a"}`, body: `{}`}}, redact.NewRules(nil, []redact.Path{secret}), nil,
			strings.Repeat("[", 10000) + strings.Repeat("]", 10000), NoDiffUnredactable},
		{"no reference", []recorded{{request: `{"q":"a"}`, body: `{}`}}, nil, []string{`{"q":"a"}`}, `{"q":"a"}`, NoDiffNoReference},
		{"equal", []recorded{{request: `{"q":"a"}`, noBody: true}}, nil, nil, `{"q":"a"}`, NoDiffEqual},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newReplayHarness(t, sourceCassette(t, tc.src...), tc.rules, Options{})
			for _, b := range tc.before {
				if status, _ := r.post(b); status != http.StatusOK {
					t.Fatalf("status %d before the miss", status)
				}
			}
			if status, _ := r.post(tc.request); status != http.StatusUnprocessableEntity {
				t.Fatalf("status %d, want a miss", status)
			}
			first, _, ok := r.h.rc.divergence.Report()
			if !ok || len(first.Diff) != 0 || first.NoDiff != tc.want {
				t.Errorf("first miss = %+v, %v; want no diff because %q", first, ok, tc.want)
			}
		})
	}
}

// A recorded request that cannot be read leaves the miss with no
// reference to compare against.
func TestDivergenceReportOnAnUnreadableReference(t *testing.T) {
	const rec = `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	src := mcpSourceCassette(t, mcpRecorded{request: rec, body: `{}`})
	m := newMCPHarness(t, src, nil)
	ins, err := src.Interactions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	m.h.rc.source = requestFailingSource{Source: m.h.rc.source, requests: map[string]bool{*ins[0].RequestBlob: true}}
	m.post("", `{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	first, _, ok := m.h.rc.divergence.Report()
	if !ok || len(first.Diff) != 0 || first.NoDiff != NoDiffNoReference {
		t.Errorf("first miss = %+v, %v; want no diff because %q", first, ok, NoDiffNoReference)
	}
}

func TestMCPMissReport(t *testing.T) {
	const rec = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_orders","arguments":{"status":"open"}}}`
	m := newMCPHarness(t, mcpSourceCassette(t, mcpRecorded{request: rec, body: `{}`}), nil)
	m.post("", `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"search_orders","arguments":{}}}`)
	first, misses, ok := m.h.rc.divergence.Report()
	if !ok || misses != 1 || first.Upstream != "mcp:"+testServer || first.Request != "tools/call search_orders" || len(first.Diff) != 1 {
		t.Errorf("Report = %+v, %d, %v; want the tools/call miss with its diff", first, misses, ok)
	}
}

func TestRequestName(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, "tools/list"},
		{`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get"}}`, "tools/call get"},
		{`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{}}`, "tools/call"},
		{`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":"x"}`, "tools/call"},
		{`[{"jsonrpc":"2.0","id":1,"method":"a"}]`, "batch"},
		{`not json`, ""},
	} {
		if got := requestName([]byte(tc.body)); got != tc.want {
			t.Errorf("requestName(%s) = %q, want %q", tc.body, got, tc.want)
		}
	}
}
