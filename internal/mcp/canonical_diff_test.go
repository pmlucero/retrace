package mcp

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/pmlucero/retrace/internal/jsonvalue"
)

// refCanonicalize is Canonicalize as it was before bodies were decoded once:
// the whole body, ParseBody, then each message's parts decoded again. It is
// the reference the single-decode version must match byte for byte, because
// the form is hashed into persisted fingerprints.
func refCanonicalize(server string, body []byte) []byte {
	whole, err := jsonvalue.Decode(body)
	if err != nil {
		return rawForm(server, body)
	}
	form := &jsonvalue.Object{Members: []jsonvalue.Member{{Key: "server", Value: server}}}
	parsed, err := ParseBody(body)
	if err != nil {
		form.Members = append(form.Members, jsonvalue.Member{Key: "body", Value: whole})
		return jsonvalue.EncodeCanonical(form)
	}
	msgs := make(jsonvalue.Array, len(parsed.Messages))
	for i, m := range parsed.Messages {
		v, err := refCanonicalMessage(m)
		if err != nil {
			return rawForm(server, body)
		}
		msgs[i] = v
	}
	if parsed.Batch {
		form.Members = append(form.Members, jsonvalue.Member{Key: "batch", Value: msgs})
	} else {
		form.Members = append(form.Members, jsonvalue.Member{Key: "message", Value: msgs[0]})
	}
	return jsonvalue.EncodeCanonical(form)
}

func refCanonicalMessage(m Message) (jsonvalue.Value, error) {
	var kind, key string
	var raw []byte
	switch m.Kind {
	case Request:
		kind, key, raw = "request", "params", m.Params
	case Notification:
		kind, key, raw = "notification", "params", m.Params
	case Response:
		kind, key, raw = "response", "result", m.Result
		if m.Error != nil {
			key, raw = "error", m.Error
		}
	default:
		kind, key, raw = "invalid", "element", m.Raw
	}
	isCall := m.Kind == Request || m.Kind == Notification
	obj := &jsonvalue.Object{Members: []jsonvalue.Member{{Key: "kind", Value: kind}}}
	if isCall {
		obj.Members = append(obj.Members, jsonvalue.Member{Key: "method", Value: m.Method})
	}
	if raw == nil {
		return obj, nil
	}
	v, err := jsonvalue.Decode(raw)
	if err != nil {
		return nil, err
	}
	if p, ok := v.(*jsonvalue.Object); ok && isCall {
		p.Members = slices.DeleteFunc(p.Members, func(mb jsonvalue.Member) bool { return mb.Key == "_meta" })
	}
	obj.Members = append(obj.Members, jsonvalue.Member{Key: key, Value: v})
	return obj, nil
}

// canonicalEdgeBodies are bodies that stress classification, duplicate
// members and key matching. Raw strings keep their \u escapes as JSON text.
var canonicalEdgeBodies = []string{
	// duplicates at the top level
	`{"id":1,"method":"a","method":"b","params":{"x":1},"params":{"y":2}}`,
	`{"id":1,"id":"s","method":"a"}`,
	`{"id":"s","id":null,"method":"a"}`,
	`{"id":1,"method":"a","params":{"x":1},"params":null}`,
	`{"id":1,"method":"a","params":[1],"params":{"_meta":1,"k":2}}`,
	`{"id":1,"result":1,"result":null}`,
	`{"id":1,"result":1,"error":{"c":1},"result":null}`,
	`{"id":1,"result":1,"result":2,"error":null}`,
	`{"id":null,"error":{"code":1}}`,
	`{"id":null,"result":{}}`,
	`{"id":true,"result":{}}`,
	`{"id":[1],"result":{}}`,
	`{"id":1.5,"result":{}}`,
	`{"id":-3,"method":"m"}`,
	`{"id":null,"method":"m"}`,
	`{"id":true,"method":"m"}`,
	`{"id":1,"method":null}`,
	`{"method":null,"params":{}}`,
	`{"method":7}`,
	`{"method":"m","result":1}`,
	`{"method":"m","error":null}`,
	`{"id":1,"result":1}`,
	`{"id":1}`,
	`{"result":1}`,
	`{"id":1,"result":1,"error":2}`,
	// params shapes
	`{"id":1,"method":"m","params":null}`,
	`{"id":1,"method":"m","params":[{"_meta":1}]}`,
	`{"id":1,"method":"m","params":"s"}`,
	`{"id":1,"method":"m","params":7}`,
	`{"id":1,"method":"m","params":true}`,
	`{"id":1,"method":"m","params":{}}`,
	`{"id":1,"method":"m","params":{"a":1,"a":2,"a":{"b":1,"b":2}}}`,
	`{"method":"n","params":{"_meta":1,"a":2,"_meta":{"b":3}}}`,
	`{"method":"n","params":{"x":{"_meta":1},"_meta":2}}`,
	`{"id":1,"result":{"_meta":1}}`,
	`{"id":1,"error":{"_meta":1}}`,
	// escaped keys and values
	`{"id":1,"\u006dethod":"m","\u0070arams":{"\u005fmeta":1,"k":2}}`,
	`{"id":1,"method":"m","params":{"_meta":1,"\u005fmeta":2,"k":3}}`,
	`{"id":1,"Method":"m"}`,
	`{"id":1,"METHOD":"m","method":"x"}`,
	`{"id":1,"method":"m\u00e9\ud83d\ude00","params":{}}`,
	`{"\u0069d":1,"result":3}`,
	`{"id":1,"\u0072esult":3,"\u0065rror":null}`,
	// invalid UTF-8 in keys and values
	"{\"id\":1,\"method\":\"m\",\"params\":{\"k\xff\":1,\"_meta\":2}}",
	"{\"id\":1,\"met\xffhod\":\"m\",\"method\":\"z\"}",
	"{\"id\":1,\"method\":\"m\xfe\",\"params\":{}}",
	"{\"id\":1,\"param\xffs\":1,\"method\":\"m\"}",
	// batches
	`[]`,
	"  \r\n\t[ ] ",
	"\n\t [{\"id\":1,\"method\":\"m\"}]",
	` [{"id":1,"method":"m"},{"method":"n","params":{"_meta":1,"a":2}}]`,
	`[{"id":1,"method":"m","method":"z"},{"id":1,"id":2,"result":3}]`,
	`[1,"a",null,true,[],{},[1,2]]`,
	`[{"id":1,"method":"m"},{"b":1,"a":[2]},7,{"id":true,"method":"m"}]`,
	`[{"id":1,"method":"m","result":1},{"id":1}]`,
	`[{"id":null,"error":{}},{"id":null,"method":"m"}]`,
	`[{"id":1,"method":"m","params":null},{"id":2,"method":"m","params":{"_meta":null}}]`,
	`[[{"id":1,"method":"m"}]]`,
	`[{"id":1,"method":"m"}] x`,
	`[{"id":1,"method":"m"},]`,
	// single non-messages and malformed bodies
	`7`, `"s"`, `null`, `true`, `{}`, `{"a":1}`,
	"  {\"id\":1,\"method\":\"m\"}  ",
	`{"id":1,"method":"m"} {}`,
	`{"id":1,"method":"m"}x`,
	`{"id":1,"method":"m"`,
	``, ` `, `[`, `{`,
	"{\"id\":1,\"method\":\"m\"}\x00",
	"\v{\"id\":1,\"method\":\"m\"}",
	`{"id":1,"method":"m","params":{"x":` + strings.Repeat("[", 999) + strings.Repeat("]", 999) + `}}`,
	`{"id":1,"method":"m","params":{"x":` + strings.Repeat("[", 1000) + strings.Repeat("]", 1000) + `}}`,
	`[` + strings.Repeat("[", 999) + strings.Repeat("]", 999) + `]`,
	`{"id":1,"result":` + strings.Repeat("[", 998) + strings.Repeat("]", 998) + `}`,
}

func TestCanonicalizeMatchesReference(t *testing.T) {
	var bodies []string
	for _, tc := range canonicalCases {
		bodies = append(bodies, tc.body)
	}
	bodies = append(bodies, canonicalEdgeBodies...)
	for _, b := range bodies {
		for _, server := range []string{"fs", `we"ird` + "\n"} {
			got := Canonicalize(server, []byte(b))
			want := refCanonicalize(server, []byte(b))
			if !bytes.Equal(got, want) {
				t.Errorf("body %.200q server %q\n got: %.300s\nwant: %.300s", b, server, got, want)
			}
		}
	}
}

// Real JSON escapes must reach the decoder: a Go interpreted string would
// have resolved them before.
func TestCanonicalEdgeBodiesKeepEscapes(t *testing.T) {
	found := false
	for _, b := range canonicalEdgeBodies {
		if strings.Contains(b, `params`) {
			found = true
		}
	}
	if !found {
		t.Fatal("edge case with an escaped params key is missing its backslash")
	}
}

func FuzzCanonicalizeMatchesReference(f *testing.F) {
	for _, tc := range canonicalCases {
		f.Add(tc.body)
	}
	for _, b := range canonicalEdgeBodies {
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, body string) {
		got := Canonicalize("fs", []byte(body))
		want := refCanonicalize("fs", []byte(body))
		if !bytes.Equal(got, want) {
			t.Fatalf("body %q\n got: %s\nwant: %s", body, got, want)
		}
	})
}
