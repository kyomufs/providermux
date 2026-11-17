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

// TestDisabledRouteDoesNotMakeAModelLive — the service lists a route it has
// switched off for the account under the base name plus "-disabled". The prefix
// rule used to read that as an offer, the model stayed in the catalog, and every
// request for it was answered with 404.
func TestDisabledRouteDoesNotMakeAModelLive(t *testing.T) {
	def := modelDef{
		id: "gemini-4-argon",
		tiers: map[string]tier{
			"low": {wire: "gemini-4-argon-low"},
		},
	}

	disabled := map[string]struct{}{"gemini-4-argon-disabled": {}}
	if def.isLive(disabled) {
		t.Fatal("a disabled route was read as an offer")
	}

	tiered := map[string]struct{}{"gemini-4-argon-disabled": {}, "gemini-4-argon-high": {}}
	if !def.isLive(tiered) {
		t.Fatal("a tier route next to a disabled one is still an offer")
	}

	// Another model's disabled route must not decide this one either.
	if def.isLive(map[string]struct{}{"gemini-4-hydrogen-disabled": {}}) {
		t.Fatal("another model's route made this one live")
	}
}
