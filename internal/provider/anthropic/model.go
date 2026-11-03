package anthropic

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/pmlucero/retrace/internal/jsonvalue"
)

// modelPaths are the Messages API endpoints whose request body names the
// model (spec §4.2).
var modelPaths = map[string]bool{"/v1/messages": true, "/v1/messages/count_tokens": true}

// ModelRewriter returns the rewrite of swap-model (spec §4.2): for a POST to
// an endpoint whose body names the model, the body with its top-level model
// member set to model; for any other request, the body as it is. It has the
// signature of proxy.Route.Rewrite, whose path is relative to the route,
// without the query.
func ModelRewriter(model string) func(method, path string, body []byte) []byte {
	return func(method, path string, body []byte) []byte {
		if method != http.MethodPost || !modelPaths[path] {
			return body
		}
		return RewriteModel(body, model)
	}
}

// RewriteModel returns body with its top-level model member set to model,
// and added at the end when there is none. Every such member, duplicates
// included, is set, so the API reads model whichever one it takes. The new
// value is spliced into the original bytes, so everything else is forwarded
// byte for byte (spec §4.2). A body that is not one JSON object is returned
// as it is, so the API answers it with its own error.
func RewriteModel(body []byte, model string) []byte {
	value := jsonvalue.Encode(model)
	dec := json.NewDecoder(bytes.NewReader(body))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return body
	}
	type span struct{ start, end int64 }
	var models []span
	members := 0
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return body
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return body
		}
		if key == "model" {
			end := dec.InputOffset()
			models = append(models, span{end - int64(len(raw)), end})
		}
		members++
	}
	if _, err := dec.Token(); err != nil { // the closing brace
		return body
	}
	closing := dec.InputOffset() - 1
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return body // more than one JSON value
	}

	var out bytes.Buffer
	at := int64(0)
	for _, m := range models {
		out.Write(body[at:m.start])
		out.Write(value)
		at = m.end
	}
	if len(models) == 0 {
		out.Write(body[:closing])
		if members > 0 {
			out.WriteByte(',')
		}
		out.WriteString(`"model":`)
		out.Write(value)
		at = closing
	}
	out.Write(body[at:])
	return out.Bytes()
}
