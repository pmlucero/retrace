package cassette

import "testing"

func TestModePredicates(t *testing.T) {
	tests := []struct {
		mode                     Mode
		forwardsLLM, forwardsMCP bool
		queuesLLM                bool
	}{
		{ModeRecord, false, false, true},
		{ModeExact, false, false, true},
		{ModeSwapModel, true, false, false},
		{ModeLiveTools, false, true, true},
		{Mode("garbage"), false, false, true},
	}
	for _, tt := range tests {
		t.Run(string(tt.mode), func(t *testing.T) {
			if got := tt.mode.ForwardsLLM(); got != tt.forwardsLLM {
				t.Errorf("ForwardsLLM = %v, want %v", got, tt.forwardsLLM)
			}
			if got := tt.mode.ForwardsMCP(); got != tt.forwardsMCP {
				t.Errorf("ForwardsMCP = %v, want %v", got, tt.forwardsMCP)
			}
			if got := tt.mode.QueuesLLM(); got != tt.queuesLLM {
				t.Errorf("QueuesLLM = %v, want %v", got, tt.queuesLLM)
			}
		})
	}
}
