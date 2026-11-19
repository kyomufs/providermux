package subscriptions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func open(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	return s
}

func TestAMissingFileStartsAnEmptyList(t *testing.T) {
	dir := t.TempDir()
	s := open(t, filepath.Join(dir, "subs.json"))
	list := s.List()
	if len(list.Items) != 0 {
		t.Fatalf("items = %d, want 0", len(list.Items))
	}
	// The rule decides what the tool collapses, and a store that has never been
	// written must still answer. The default is none: the operator asked for
	// every record of a subscription, and collapsing is now a deliberate choice.
	if list.Dedupe != ModeNone {
		t.Errorf("dedupe = %q, want %q", list.Dedupe, ModeNone)
	}
}

func TestAnAddedSubscriptionSurvivesAReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subs.json")
	s := open(t, path)
	added, err := s.Add(Item{URL: "https://sub.example.test/abc", UA: "clash-verge/2.0"})
	if err != nil {
		t.Fatalf("Add returned error: %v", err)
	}
	// The operator leaves the name empty; the host is the name they would type.
	if added.ID != "sub-example-test" {
		t.Errorf("id = %q, want the host", added.ID)
	}

	reopened := open(t, path)
	list := reopened.List()
	if len(list.Items) != 1 || list.Items[0].URL != "https://sub.example.test/abc" {
		t.Fatalf("reopened = %+v", list.Items)
	}
	if list.Items[0].UA != "clash-verge/2.0" {
		t.Errorf("ua = %q, want the recorded agent", list.Items[0].UA)
	}
}

func TestTheStoreIsNotWorldReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subs.json")
	s := open(t, path)
	if _, err := s.Add(Item{URL: "https://sub.example.test/secret-token"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	// A subscription URL is a credential: the file must not be readable by others.
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("permissions = %04o, want 0600", perm)
	}
}

func TestAnAddressWithoutAHostIsRefused(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "subs.json"))
	for _, bad := range []string{"", "clash://x", "ftp://sub.example.test/a", "https://"} {
		if _, err := s.Add(Item{URL: bad}); err == nil {
			t.Errorf("Add(%q) succeeded, want a refusal", bad)
		}
	}
}

func TestTheSameAddressTwiceIsRefused(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "subs.json"))
	if _, err := s.Add(Item{ID: "one", URL: "https://sub.example.test/a"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// Two entries for one address fetch the same nodes twice on every refresh.
	if _, err := s.Add(Item{ID: "two", URL: "https://sub.example.test/a"}); err == nil {
		t.Fatal("the same address was accepted twice")
	}
}

func TestAUsedIDIsRefused(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "subs.json"))
	if _, err := s.Add(Item{ID: "main", URL: "https://a.example.test/x"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := s.Add(Item{ID: "MAIN", URL: "https://b.example.test/x"}); err == nil {
		t.Fatal("a duplicate id was accepted")
	}
}

func TestUpdateReplacesInPlaceAndKeepsTheOrder(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "subs.json"))
	mustAdd(t, s, Item{ID: "one", URL: "https://one.example.test/a"})
	mustAdd(t, s, Item{ID: "two", URL: "https://two.example.test/a"})

	if _, err := s.Update(Item{ID: "one", URL: "https://one.example.test/moved"}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	list := s.List()
	if list.Items[0].ID != "one" || list.Items[0].URL != "https://one.example.test/moved" {
		t.Fatalf("items = %+v", list.Items)
	}
	if list.Items[1].ID != "two" {
		t.Errorf("order changed: %+v", list.Items)
	}
}

func TestUpdateOfAnUnknownIDIsRefused(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "subs.json"))
	if _, err := s.Update(Item{ID: "ghost", URL: "https://ghost.example.test/a"}); err == nil {
		t.Fatal("an unknown id was updated")
	}
}

func TestRemoveDropsOnlyTheNamedEntry(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "subs.json"))
	mustAdd(t, s, Item{ID: "one", URL: "https://one.example.test/a"})
	mustAdd(t, s, Item{ID: "two", URL: "https://two.example.test/a"})

	if err := s.Remove("one"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	list := s.List()
	if len(list.Items) != 1 || list.Items[0].ID != "two" {
		t.Fatalf("items = %+v", list.Items)
	}
	if err := s.Remove("one"); err == nil {
		t.Fatal("removing a missing entry succeeded")
	}
}

func TestADedupeModeIsWrittenAndRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subs.json")
	s := open(t, path)
	if err := s.SetDedupe(ModeNone); err != nil {
		t.Fatalf("SetDedupe: %v", err)
	}
	if got := open(t, path).Dedupe(); got != ModeNone {
		t.Errorf("dedupe = %q, want %q", got, ModeNone)
	}
	if err := s.SetDedupe("everything"); err == nil {
		t.Error("an unknown mode was accepted")
	}
}

func TestRenderWritesTheShapeTheToolReads(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "subs.json"))
	mustAdd(t, s, Item{
		ID:       "main",
		URL:      "https://sub.example.test/a?token=1",
		UA:       "clash-verge/2.0",
		Insecure: true,
		Headers:  map[string]string{"X-Token": "a b"},
	})

	body, err := s.Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	text := string(body)
	for _, want := range []string{
		"sources:\n",
		`  - id: "main"`,
		`    url: "https://sub.example.test/a?token=1"`,
		`    ua: "clash-verge/2.0"`,
		"    insecure: true\n",
		"    headers:\n",
		`      "X-Token": "a b"`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("rendered sources is missing %q:\n%s", want, text)
		}
	}
}

func TestRenderQuotesAValueWithAQuoteInIt(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "subs.json"))
	mustAdd(t, s, Item{ID: `the "one"`, URL: "https://sub.example.test/a"})
	body, err := s.Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(string(body), `  - id: "the \"one\""`) {
		t.Fatalf("quote was not escaped:\n%s", body)
	}
}

func TestAnEmptyListRendersAsAnEmptyList(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "subs.json"))
	body, err := s.Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(string(body), "sources: []\n") {
		t.Fatalf("empty list rendered as:\n%s", body)
	}
}

func TestListCopyDoesNotSeeLaterEdits(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "subs.json"))
	mustAdd(t, s, Item{ID: "one", URL: "https://one.example.test/a", Headers: map[string]string{"A": "1"}})
	grabbed := s.List()
	mustAdd(t, s, Item{ID: "two", URL: "https://two.example.test/a"})

	if len(grabbed.Items) != 1 {
		t.Errorf("the copy grew to %d entries", len(grabbed.Items))
	}
	grabbed.Items[0].Headers["A"] = "changed"
	if s.List().Items[0].Headers["A"] != "1" {
		t.Error("the copy shares the header map with the store")
	}
}

func mustAdd(t *testing.T, s *Store, item Item) {
	t.Helper()
	if _, err := s.Add(item); err != nil {
		t.Fatalf("Add(%s): %v", item.ID, err)
	}
}
