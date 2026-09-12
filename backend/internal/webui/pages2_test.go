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

func TestImportedNameBase(t *testing.T) {
	tests := []struct {
		parsedName string
		name       string
		want       string
	}{
		{"KataBump-Direct", "fallback", "KataBump"},
		{"KataBump-Global", "fallback", "KataBump"},
		{"", "my-node", "my-node"},
		{"  ", "my-node", "my-node"},
		{"NoHyphen", "fallback", "NoHyphen"},
		{"Multi-Segment-Rule", "fallback", "Multi-Segment"},
	}

	for _, tt := range tests {
		n := nodeSummary{
			ParsedName: tt.parsedName,
			Name:       tt.name,
		}
		got := importedNameBase(n)
		if got != tt.want {
			t.Errorf("importedNameBase(ParsedName=%q, Name=%q) = %q, want %q",
				tt.parsedName, tt.name, got, tt.want)
		}
	}
}
