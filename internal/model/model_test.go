package model

import "testing"

func TestParseSeverity(t *testing.T) {
	tests := []struct {
		input string
		want  Severity
	}{
		{"critical", SeverityCritical},
		{"CRITICAL", SeverityCritical},
		{"  High ", SeverityHigh},
		{"medium", SeverityMedium},
		{"low", SeverityLow},
		{"info", SeverityInfo},
		{"unknown", SeverityLow},
		{"", SeverityLow},
	}
	for _, tt := range tests {
		if got := ParseSeverity(tt.input); got != tt.want {
			t.Errorf("ParseSeverity(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestSeverity_MeetsMinimum(t *testing.T) {
	tests := []struct {
		sev  Severity
		min  Severity
		want bool
	}{
		{SeverityCritical, SeverityMedium, true},
		{SeverityHigh, SeverityMedium, true},
		{SeverityMedium, SeverityMedium, true},
		{SeverityLow, SeverityMedium, false},
		{SeverityInfo, SeverityMedium, false},
		{SeverityLow, SeverityLow, true},
		{SeverityInfo, SeverityInfo, true},
		{SeverityCritical, SeverityCritical, true},
		{SeverityHigh, SeverityCritical, false},
	}
	for _, tt := range tests {
		if got := tt.sev.MeetsMinimum(tt.min); got != tt.want {
			t.Errorf("%q.MeetsMinimum(%q) = %v, want %v", tt.sev, tt.min, got, tt.want)
		}
	}
}
