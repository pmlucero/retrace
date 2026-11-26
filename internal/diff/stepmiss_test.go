package diff

import (
	"encoding/json"
	"testing"

	"github.com/pmlucero/retrace/internal/cassette"
)

func TestTrajectoryStepOf(t *testing.T) {
	// A repeated seq: only pointer identity tells the two events apart.
	d1, d2 := ev(5, "divergence", 0), ev(5, "divergence", 0)
	tr := Trajectory{Steps: []Step{
		{N: 1, Divergences: []cassette.Event{d1}},
		{N: 2, Divergences: []cassette.Event{d2}},
	}}
	other := ev(5, "divergence", 0)
	tests := []struct {
		name string
		e    *cassette.Event
		want int
	}{
		{"nil", nil, 0},
		{"not found", &other, 0},
		{"found in step 1", &tr.Steps[0].Divergences[0], 1},
		{"duplicate seq resolved by identity", &tr.Steps[1].Divergences[0], 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tr.StepOf(tt.e); got != tt.want {
				t.Errorf("StepOf = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestMissUpstream(t *testing.T) {
	tests := []struct {
		name   string
		result json.RawMessage
		want   string
	}{
		{"with upstream", json.RawMessage(`{"upstream":"mcp:orders"}`), "mcp:orders"},
		{"empty upstream", json.RawMessage(`{"upstream":""}`), ""},
		{"no upstream field", json.RawMessage(`{}`), ""},
		{"invalid JSON", json.RawMessage(`{"upstream":`), ""},
		{"absent result", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := ev(1, "divergence", 0)
			e.Result = tt.result
			if got := MissUpstream(e); got != tt.want {
				t.Errorf("MissUpstream = %q, want %q", got, tt.want)
			}
		})
	}
}
