package anthropic

import (
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
// and added at the end when there is none. Every member, duplicates
// included, is set, so the API reads model whichever one it takes. The other
// members keep their order and number text, as body redaction keeps them
// (spec §3.5). A body that is not a JSON object, or is nested too deeply to
// decode, is returned as it is, so the API answers it with its own error.
func RewriteModel(body []byte, model string) []byte {
	v, err := jsonvalue.Decode(body)
	if err != nil {
		return body
	}
	obj, ok := v.(*jsonvalue.Object)
	if !ok {
		return body
	}
	found := false
	for i := range obj.Members {
		if obj.Members[i].Key == "model" {
			obj.Members[i].Value, found = model, true
		}
	}
	if !found {
		obj.Members = append(obj.Members, jsonvalue.Member{Key: "model", Value: model})
	}
	return jsonvalue.Encode(obj)
}
