package derive

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"slices"
	"testing"

	"github.com/pmlucero/retrace/internal/cassette"
)

// AC-26: events the writer drops while recording are counted in the session,
// leave gaps in the seqs, and reindex restores them and resets the count.
// The drops are forced with a trigger that fails some event writes, so the
// interactions are complete and only their events are lost.
func TestReindexRestoresEventsDroppedWhileRecording(t *testing.T) {
	ctx := context.Background()
	exchanges := scenarioExchanges(t, "json_tool_loop")
	want, _ := record(t, exchanges).Events(ctx)
	if len(want) < 4 {
		t.Fatalf("scenario derives %d events, want at least 4", len(want))
	}

	// Fail the second event, which drops its descendants too, and the last
	// one, which leaves no gap in the stored seqs.
	last := want[len(want)-1].Seq
	failed := []int64{2, last}
	dropped := map[int64]bool{}
	for _, e := range want {
		if slices.Contains(failed, e.Seq) || (e.ParentSeq != nil && dropped[*e.ParentSeq]) {
			dropped[e.Seq] = true
		}
	}
	var path string
	c, _ := recordWith(t, exchanges, func(p string) {
		path = p
		execSQL(t, p, fmt.Sprintf(`CREATE TRIGGER fail_events BEFORE INSERT ON events
			WHEN NEW.seq IN (2, %d) BEGIN SELECT RAISE(ABORT, 'forced failure'); END`, last))
	})

	s, err := c.Session(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s.DroppedEvents != len(dropped) {
		t.Errorf("session dropped_events = %d, want %d", s.DroppedEvents, len(dropped))
	}
	events, err := c.Events(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var stored, wantStored []int64
	for _, e := range events {
		stored = append(stored, e.Seq)
	}
	for _, e := range want {
		if !dropped[e.Seq] {
			wantStored = append(wantStored, e.Seq)
		}
	}
	if !slices.Equal(stored, wantStored) {
		t.Errorf("stored seqs %v, want %v: the missing ones are the dropped events", stored, wantStored)
	}
	interactions, err := c.Interactions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range interactions {
		if cassette.MarksIncomplete(in.Error) {
			t.Errorf("interaction %d is marked incomplete; only events should be lost", in.ID)
		}
	}

	execSQL(t, path, `DROP TRIGGER fail_events`)
	res, err := Reindex(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if res.IncompleteInteractions != 0 {
		t.Errorf("reindex found %d incomplete interactions, want 0", res.IncompleteInteractions)
	}
	rebuilt, _ := c.Events(ctx)
	if !reflect.DeepEqual(rebuilt, want) {
		t.Errorf("reindex wrote:\n%s\nwant the events of a recording with no drops:\n%s", eventsJSONL(t, rebuilt), eventsJSONL(t, want))
	}
	if s, _ := c.Session(ctx); s.DroppedEvents != 0 {
		t.Errorf("session dropped_events after reindex = %d, want 0", s.DroppedEvents)
	}
}

func scenarioExchanges(t *testing.T, name string) []exchange {
	t.Helper()
	for _, sc := range scenarios {
		if sc.name == name {
			return sc.exchanges
		}
	}
	t.Fatalf("no scenario %q", name)
	return nil
}

// execSQL runs stmt on the cassette at path through a second connection.
func execSQL(t *testing.T, path, stmt string) {
	t.Helper()
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(context.Background(), stmt); err != nil {
		t.Fatalf("%s: %v", stmt, err)
	}
}
