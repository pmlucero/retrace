package mcp

import (
	"fmt"
	"strings"
	"testing"
)

func BenchmarkCanonicalize(b *testing.B) {
	var big strings.Builder
	big.WriteString(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"_meta":{"progressToken":"p"},"name":"write_file","arguments":{"items":[`)
	for i := 0; big.Len() < 100_000; i++ {
		if i > 0 {
			big.WriteByte(',')
		}
		fmt.Fprintf(&big, `{"id":%d,"name":"item-%d","tags":["a","b","c"],"score":%d.5,"ok":true}`, i, i, i)
	}
	big.WriteString(`]}}}`)
	cases := []struct{ name, body string }{
		{"small", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read_file","arguments":{"path":"/tmp/a"}}}`},
		{"batch", `[{"jsonrpc":"2.0","id":1,"method":"tools/list"},{"jsonrpc":"2.0","method":"notifications/progress","params":{"_meta":{"x":1},"progress":1}},{"jsonrpc":"2.0","id":"s-1","result":{"roots":[]}},{"jsonrpc":"2.0","id":"s-2","error":{"code":-1,"message":"no"}}]`},
		{"large", big.String()},
	}
	for _, tc := range cases {
		body := []byte(tc.body)
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(body)))
			for b.Loop() {
				Canonicalize("fs", body)
			}
		})
	}
}
