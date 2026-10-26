package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pmlucero/retrace/internal/cassette"
)

// orderDeriver logs the records the writer derives from, in order.
type orderDeriver struct {
	mu   sync.Mutex
	seen []string
}

func (d *orderDeriver) Begin(in cassette.Interaction, _ []byte, _ cassette.EmitFunc) {
	d.log(fmt.Sprintf("begin %d", in.ID))
}

func (d *orderDeriver) Chunk(cassette.Chunk) {}

func (d *orderDeriver) Complete(id int64, _ cassette.Completion, _ []byte, _ cassette.EmitFunc) {
	d.log(fmt.Sprintf("complete %d", id))
}

func (d *orderDeriver) log(s string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.seen = append(d.seen, s)
}

// slowCloseTransport delays closing each response body until release is
// closed, or a timeout passes: it makes the proxy finish an exchange late.
type slowCloseTransport struct {
	release chan struct{}
}

func (s slowCloseTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := http.DefaultTransport.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	resp.Body = slowCloseBody{ReadCloser: resp.Body, release: s.release}
	return resp, nil
}

type slowCloseBody struct {
	io.ReadCloser
	release chan struct{}
}

func (b slowCloseBody) Close() error {
	select {
	case <-b.release:
	case <-time.After(2 * time.Second):
	}
	return b.ReadCloser.Close()
}

// An exchange is completed as soon as the upstream body ends, before its last
// bytes reach the agent, not when the proxy gets around to closing it. So an
// agent's next request, which may depend on the response, is always recorded
// after it, and its events follow (spec §7.4).
func TestProxyCompletesAnExchangeBeforeTheAgentHasIt(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	d := &orderDeriver{}
	h := newHarnessOpts(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) == "second" {
			once.Do(func() { close(release) }) // only now may the first exchange be closed
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"type":"message","content":[]}`)
	}), harnessOptions{deriver: d, transport: slowCloseTransport{release: release}})

	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	for _, body := range []string{"first", "second"} {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost,
			h.server.URL+"/llm/anthropic/v1/messages", strings.NewReader(body))
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
	}
	h.interactions()

	want := []string{"begin 1", "complete 1", "begin 2", "complete 2"}
	if !slices.Equal(d.seen, want) {
		t.Errorf("writer order = %q, want %q", d.seen, want)
	}
}
