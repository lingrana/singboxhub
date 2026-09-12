package webui

import (
	"testing"
)

func TestInferModeFromName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"KataBump-Direct", "direct"},
		{"KataBump-Global", "global"},
		{"KataBump-Rule", "rule"},
		{"SomeHost-DIRECT", "direct"},
		{"SomeHost-GLOBAL", "global"},
		{"plain-name", "rule"},
		{"", "rule"},
		{"NoHyphen", "rule"},
		{"Multi-Segment-Direct", "direct"},
	}

	for _, tt := range tests {
		got := inferModeFromName(tt.input)
		if got != tt.want {
			t.Errorf("inferModeFromName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
