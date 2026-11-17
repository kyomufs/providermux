package antigravity

import (
	"strings"
	"testing"
)

// TestTheRefusedSentenceIsRephrased — the service answers this one sentence with
// 429 while it serves the same instruction without it, so it must not reach the
// wire as written.
func TestTheRefusedSentenceIsRephrased(t *testing.T) {
	system := "You are a coding agent.\n" + refusedSystemSentence + "\nWork carefully."

	got := acceptableSystem(system)
	if strings.Contains(got, refusedSystemSentence) {
		t.Fatalf("the refused sentence survived: %q", got)
	}
	if !strings.Contains(got, replacedSystemSentence) {
		t.Fatalf("the replacement is missing: %q", got)
	}
	if !strings.Contains(got, "You are a coding agent.") || !strings.Contains(got, "Work carefully.") {
		t.Fatalf("neighbouring instruction was altered: %q", got)
	}
}

// TestSystemWithoutTheSentencesIsUntouched — everything else travels as written.
func TestSystemWithoutTheSentenceIsUntouched(t *testing.T) {
	for _, system := range []string{
		"",
		"You are a helpful assistant.",
		"The browser provides no implicit DOM.",
		"The browser hands over no page structure.",
	} {
		if got := acceptableSystem(system); got != system {
			t.Fatalf("acceptableSystem(%q) = %q, want it unchanged", system, got)
		}
	}
}
