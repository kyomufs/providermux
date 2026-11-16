package antigravity

import (
	"reflect"
	"testing"
)

// TestUnknownModelsAreReportedOnce — the panel refreshes far more often than the
// live answer changes, so the same unknown id must not produce a line per tick.
func TestUnknownModelsAreReportedOnce(t *testing.T) {
	c := newLiveCatalog()
	known := func(id string) bool { return id == "known-model" }

	ids := []string{"known-model", "mystery-one", "Mystery-Two"}

	first := c.unseenUnknown("acc", ids, known)
	if want := []string{"mystery-one", "Mystery-Two"}; !reflect.DeepEqual(first, want) {
		t.Fatalf("first call = %v, want %v", first, want)
	}

	if second := c.unseenUnknown("acc", ids, known); len(second) != 0 {
		t.Fatalf("second call repeated %v; the warning must be deduplicated", second)
	}

	// A different account has its own view; the line names the account, so it
	// must not be suppressed by another account's report.
	if other := c.unseenUnknown("other", ids, known); len(other) != 2 {
		t.Fatalf("another account saw %v, want both unknown ids", other)
	}
}
