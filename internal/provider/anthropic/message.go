// Package anthropic reads Anthropic Messages API traffic (ADR 0003): the
// requests, JSON responses and SSE streams that retrace stores, and the price
// table that turns their usage into an estimated cost (spec §7.5).
//
// Parsing is lenient on purpose. Unknown fields, content blocks and SSE events
// are ignored (ADR 0003), and a value that has an unexpected type, such as a
// number replaced by a redaction marker (spec §3.5), is treated as absent.
package anthropic

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Request is what event derivation needs from a Messages API request body.
type Request struct {
	Model string // empty when absent

	// HasUserText reports whether the trailing message is a user message with
	// text, which makes it a user_message event (spec §7.4).
	HasUserText bool
	UserText    string
	// UserKey identifies the trailing user message: a hash of the
	// conversation up to and including it.
	UserKey string
}

// ParseRequest reads a Messages API request body.
func ParseRequest(body []byte) (Request, error) {
	var raw struct {
		Model    json.RawMessage   `json:"model"`
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return Request{}, fmt.Errorf("parsing messages request: %w", err)
	}
	req := Request{Model: stringValue(raw.Model)}
	if len(raw.Messages) == 0 {
		return req, nil
	}
	last := raw.Messages[len(raw.Messages)-1]
	var msg struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	// A malformed trailing message has no user text; the model still counts.
	if json.Unmarshal(last, &msg) != nil || msg.Role != "user" {
		return req, nil
	}
	text, ok := contentText(msg.Content)
	if !ok {
		return req, nil
	}
	key, ok := conversationKey(raw.Messages)
	if !ok {
		return req, nil
	}
	req.HasUserText, req.UserText, req.UserKey = true, text, key
	return req, nil
}

// contentText returns the text of a message's content: the string itself, or
// its text blocks joined with "\n". ok is false when there is no text.
func contentText(content json.RawMessage) (string, bool) {
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		return s, true
	}
	var blocks []struct {
		Type string          `json:"type"`
		Text json.RawMessage `json:"text"`
	}
	if err := json.Unmarshal(content, &blocks); err != nil {
		return "", false
	}
	var texts []string
	for _, b := range blocks {
		if b.Type == "text" {
			texts = append(texts, stringValue(b.Text))
		}
	}
	return strings.Join(texts, "\n"), len(texts) > 0
}

// conversationKey hashes the canonical JSON of messages, so the same
// conversation gets the same key however its JSON was formatted.
// ok is false when a message is not valid JSON.
func conversationKey(messages []json.RawMessage) (key string, ok bool) {
	h := sha256.New()
	for _, m := range messages {
		c, err := canonicalJSON(m)
		if err != nil {
			return "", false
		}
		h.Write(c)
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil)), true
}

// canonicalJSON re-encodes raw with object keys sorted and no insignificant
// whitespace. Numbers keep their text.
func canonicalJSON(raw json.RawMessage) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return json.Marshal(v) // encoding/json sorts map keys
}

// Message is a model response, from a JSON body or reassembled from an SSE
// stream.
type Message struct {
	Model      string
	StopReason string
	Text       string // the text blocks, concatenated
	ToolUses   []ToolUse
	Usage      *Usage // nil when the response has none

	// Error is "<type>: <message>" for an error body or an SSE error event.
	Error string
	// Complete is false for a stream that ended before message_stop.
	Complete bool
}

// ToolUse is one tool_use content block.
type ToolUse struct {
	ID    string
	Name  string
	Input json.RawMessage // compact JSON; a JSON string when the streamed input was not valid JSON
}

// Usage is the token usage of a response. A nil count was absent.
type Usage struct {
	InputTokens              *int64
	OutputTokens             *int64
	CacheReadInputTokens     *int64
	CacheCreationInputTokens *int64
	// CacheCreation5m and CacheCreation1h break cache writes down by TTL,
	// when the API reports it (usage.cache_creation).
	CacheCreation5m *int64
	CacheCreation1h *int64
	ServiceTier     string
	Speed           string
	InferenceGeo    string
}

// ErrNotMessage is returned for a JSON body that is neither a message nor an
// API error.
var ErrNotMessage = errors.New("not a messages API response")

// ParseResponse reads a non-streamed Messages API response body.
func ParseResponse(body []byte) (Message, error) {
	var raw struct {
		Type       string            `json:"type"`
		Model      json.RawMessage   `json:"model"`
		StopReason json.RawMessage   `json:"stop_reason"`
		Content    []json.RawMessage `json:"content"`
		Usage      json.RawMessage   `json:"usage"`
		Error      json.RawMessage   `json:"error"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return Message{}, fmt.Errorf("parsing messages response: %w", err)
	}
	switch raw.Type {
	case "error":
		return Message{Error: apiError(raw.Error), Complete: true}, nil
	case "message":
	default:
		return Message{}, fmt.Errorf("parsing messages response: %w", ErrNotMessage)
	}
	msg := Message{
		Model: stringValue(raw.Model), StopReason: stringValue(raw.StopReason),
		Usage: mergeUsage(nil, raw.Usage), Complete: true,
	}
	var text strings.Builder
	for _, c := range raw.Content {
		var b contentBlock
		if json.Unmarshal(c, &b) != nil {
			continue
		}
		switch b.Type {
		case "text":
			text.WriteString(stringValue(b.Text))
		case "tool_use":
			msg.ToolUses = append(msg.ToolUses, ToolUse{ID: stringValue(b.ID), Name: stringValue(b.Name), Input: toolInput(b.Input)})
		}
	}
	msg.Text = text.String()
	return msg, nil
}

// contentBlock holds the fields of a content block that derivation reads.
type contentBlock struct {
	Type  string          `json:"type"`
	Text  json.RawMessage `json:"text"`
	ID    json.RawMessage `json:"id"`
	Name  json.RawMessage `json:"name"`
	Input json.RawMessage `json:"input"`
}

// apiError formats an error object as "<type>: <message>".
func apiError(raw json.RawMessage) string {
	var e struct {
		Type    json.RawMessage `json:"type"`
		Message json.RawMessage `json:"message"`
	}
	_ = json.Unmarshal(raw, &e)
	typ, msg := stringValue(e.Type), stringValue(e.Message)
	switch {
	case typ == "" && msg == "":
		return "unknown error"
	case msg == "":
		return typ
	case typ == "":
		return msg
	}
	return typ + ": " + msg
}

// toolInput returns input as compact JSON, `{}` when it is missing, or the
// text as a JSON string when it is not valid JSON.
func toolInput(input []byte) json.RawMessage {
	if len(bytes.TrimSpace(input)) == 0 {
		return json.RawMessage(`{}`)
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, input); err != nil {
		s, _ := json.Marshal(string(input))
		return s
	}
	return buf.Bytes()
}

// mergeUsage returns u with the fields present in raw replacing its own. For
// SSE, message_delta usage replaces that of message_start (spec §7.4).
func mergeUsage(u *Usage, raw json.RawMessage) *Usage {
	var f map[string]json.RawMessage
	if json.Unmarshal(raw, &f) != nil || f == nil {
		return u
	}
	if u == nil {
		u = &Usage{}
	}
	setCount(&u.InputTokens, f["input_tokens"])
	setCount(&u.OutputTokens, f["output_tokens"])
	setCount(&u.CacheReadInputTokens, f["cache_read_input_tokens"])
	setCount(&u.CacheCreationInputTokens, f["cache_creation_input_tokens"])
	var cc map[string]json.RawMessage
	if json.Unmarshal(f["cache_creation"], &cc) == nil {
		setCount(&u.CacheCreation5m, cc["ephemeral_5m_input_tokens"])
		setCount(&u.CacheCreation1h, cc["ephemeral_1h_input_tokens"])
	}
	setString(&u.ServiceTier, f["service_tier"])
	setString(&u.Speed, f["speed"])
	setString(&u.InferenceGeo, f["inference_geo"])
	return u
}

// setCount sets *dst when raw is a non-negative integer. Anything else, such
// as null or a redaction marker, leaves it as it was.
func setCount(dst **int64, raw json.RawMessage) {
	var n int64
	if len(raw) == 0 || json.Unmarshal(raw, &n) != nil || n < 0 {
		return
	}
	*dst = &n
}

func setString(dst *string, raw json.RawMessage) {
	if s := stringValue(raw); s != "" {
		*dst = s
	}
}

// stringValue returns raw as a string, or "" when it is not a JSON string.
func stringValue(raw json.RawMessage) string {
	var s string
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}
