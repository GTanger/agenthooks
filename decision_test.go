package agenthooks

import "testing"

func TestWithBlockReasonFirstNonEmptyWins(t *testing.T) {
	tests := []struct {
		name       string
		candidates []string
		want       string
	}{
		{"first candidate wins", []string{"custom message", "audit reason"}, "custom message"},
		{"empty first falls through", []string{"", "audit reason"}, "audit reason"},
		{"single candidate", []string{"only"}, "only"},
		{"whitespace counts as present", []string{"  ", "audit reason"}, "  "},
		{"later empties ignored", []string{"", "", "third"}, "third"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Deny("audit").WithBlockReason(tt.candidates...).SystemMessage(); got != tt.want {
				t.Errorf("ToolPreDecision.WithBlockReason(%q).SystemMessage() = %q, want %q", tt.candidates, got, tt.want)
			}
			if got := BlockPrompt("audit").WithBlockReason(tt.candidates...).SystemMessage(); got != tt.want {
				t.Errorf("PromptDecision.WithBlockReason(%q).SystemMessage() = %q, want %q", tt.candidates, got, tt.want)
			}
		})
	}
}

func TestWithBlockReasonAllEmptyLeavesDecisionUnchanged(t *testing.T) {
	d := Deny("audit").WithSystemMessage("existing")
	if got := d.WithBlockReason("", "").SystemMessage(); got != "existing" {
		t.Errorf("all-empty candidates: SystemMessage() = %q, want existing message preserved", got)
	}
	if got := d.WithBlockReason().SystemMessage(); got != "existing" {
		t.Errorf("no candidates: SystemMessage() = %q, want existing message preserved", got)
	}
	p := BlockPrompt("audit").WithBlockReason("", "")
	if got := p.SystemMessage(); got != "" {
		t.Errorf("all-empty candidates on fresh decision: SystemMessage() = %q, want empty", got)
	}
}

func TestWithBlockReasonDoesNotTouchOtherFields(t *testing.T) {
	d := Deny("audit").WithBlockReason("user message")
	if d.Kind() != DecisionDeny {
		t.Errorf("Kind() = %v, want DecisionDeny", d.Kind())
	}
	if d.Reason() != "audit" {
		t.Errorf("Reason() = %q, want audit reason untouched", d.Reason())
	}
	if !d.Blocks() {
		t.Error("Blocks() = false, want true for Deny")
	}
}
