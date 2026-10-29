package cassette

import (
	"encoding/json"
	"strings"
	"time"
)

// TimeLayout is the timestamp format of every time column: RFC 3339, UTC,
// microsecond precision (spec §7.1).
const TimeLayout = "2006-01-02T15:04:05.000000Z07:00"

// FormatTime renders t in TimeLayout, in UTC.
func FormatTime(t time.Time) string {
	return t.UTC().Format(TimeLayout)
}

// Session is the single session row of a cassette (spec §7.2).
type Session struct {
	ID              string
	Mode            string // record, exact, swap-model or live-tools
	ParentSessionID *string
	Status          string // recording, completed, diverged, failed or interrupted
	CreatedAt       time.Time
	EndedAt         *time.Time
	Command         []string
	Cwd             string
	AgentExitCode   *int
	ModelOverride   *string
	OnMiss          string
	MCPServers      json.RawMessage // [{name, url, protocol_version, server_info}]
	Redaction       json.RawMessage
	RetraceVersion  string
	DeriverVersion  int
	PricingVersion  string
	Labels          map[string]string
	// DroppedEvents is the number of events missing from the semantic log
	// because the writer could not write them (spec §7.7). It is set when the
	// session ends, and says nothing while Status is recording.
	DroppedEvents int
}

// RecordingIncomplete is the marker that Interaction.Error contains when
// records of the interaction were dropped instead of written (spec §7.3).
// Readers such as reindex find incomplete interactions by it (spec §7.6), so
// it is defined once, here.
const RecordingIncomplete = "recording incomplete"

// Interaction is one HTTP exchange through the proxy (spec §7.3). It is
// inserted when the request arrives and completed once when the response ends.
type Interaction struct {
	ID                  int64
	Channel             string // llm or mcp
	Upstream            string // anthropic or mcp:<name>
	HTTPMethod          string
	Path                string
	StatusCode          *int
	RequestHeaders      json.RawMessage
	ResponseHeaders     json.RawMessage // nil while in flight
	RequestBlob         *string
	ResponseBlob        *string // nil when streamed
	Streamed            bool
	Fingerprint         *string
	CanonicalBlob       *string
	StartedAt           time.Time
	TTFBMillis          *int64
	DurationMillis      *int64 // nil while in flight
	Provenance          string // recorded, replayed or live
	SourceInteractionID *int64
	Error               *string
}

// MarksIncomplete reports whether err, an interaction's error, contains
// RecordingIncomplete. It is the half of the definition of an incomplete
// recording (spec §7.3) that does not look at the duration. Prefer
// Interaction.Incomplete or Completion.Incomplete, which apply all of it.
func MarksIncomplete(err *string) bool {
	return err != nil && strings.Contains(*err, RecordingIncomplete)
}

// Incomplete reports whether the recording of the interaction is incomplete
// (spec §7.3): its error contains RecordingIncomplete, or it has no duration
// because the recording stopped before it completed, e.g. retrace was killed.
func (in Interaction) Incomplete() bool {
	return in.DurationMillis == nil || MarksIncomplete(in.Error)
}

// Completion holds the fields set when an interaction's response ends.
type Completion struct {
	StatusCode      *int
	ResponseHeaders json.RawMessage
	ResponseBlob    *string
	Streamed        bool
	TTFBMillis      *int64
	DurationMillis  int64
	Error           *string
}

// Incomplete reports whether the recording of the interaction that c
// completes is incomplete (spec §7.3). A completion always has a duration, so
// only its error can say so.
func (c Completion) Incomplete() bool {
	return MarksIncomplete(c.Error)
}

// Chunk is one raw SSE event of a streamed response, with its offset from the
// start of the response (spec §7.3).
type Chunk struct {
	InteractionID int64
	Index         int
	OffsetMillis  int64
	Data          []byte
}

// Event is one entry of the semantic layer (spec §7.4).
type Event struct {
	Seq              int64
	ParentSeq        *int64
	InteractionID    *int64
	Kind             string
	Source           string
	Target           string
	StartedAt        time.Time
	DurationMillis   *int64
	Model            *string
	RequestedModel   *string
	Tool             *string
	Arguments        json.RawMessage
	Result           json.RawMessage
	TokensIn         *int64
	TokensOut        *int64
	TokensCacheRead  *int64
	TokensCacheWrite *int64
	CostUSD          *float64
	Status           string // ok, error or divergence
	Error            *string
	Provenance       string
	DeriverVersion   int
}
