package control

import (
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"testing"

	"providermux/internal/subscriptions"
)

// subsUnderTest builds a handler over a real store in a temporary directory:
// the handlers only talk to the store's methods, so nothing here is faked.
func subsUnderTest(t *testing.T) (*Handler, *subscriptions.Store) {
	t.Helper()
	store, err := subscriptions.Open(filepath.Join(t.TempDir(), "subs.json"))
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	return &Handler{Subscriptions: store}, store
}

func decodeSubs(t *testing.T, rec interface{ Bytes() []byte }) SubscriptionState {
	t.Helper()
	var state SubscriptionState
	if err := json.Unmarshal(rec.Bytes(), &state); err != nil {
		t.Fatalf("decoding the answer: %v: %s", err, rec.Bytes())
	}
	return state
}

func TestAnEmptyListIsShownAsAList(t *testing.T) {
	h, _ := subsUnderTest(t)

	rec := call(t, h, http.MethodGet, "subscriptions", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status is %d, want 200: %s", rec.Code, rec.Body)
	}
	state := decodeSubs(t, rec.Body)
	// The panel draws a list; an absent field would render as "undefined".
	if state.Items == nil {
		t.Error("items is null, want an empty array")
	}
	if state.Dedupe != subscriptions.ModeNone {
		t.Errorf("dedupe is %q, want %q", state.Dedupe, subscriptions.ModeNone)
	}
	if state.Store == "" {
		t.Error("the answer does not say where the list lives")
	}
}

func TestASubscriptionIsAddedThenShown(t *testing.T) {
	h, _ := subsUnderTest(t)

	rec := call(t, h, http.MethodPost, "subscriptions", `{"url":"https://sub.example.test/a","ua":"clash-verge/2.0"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status is %d, want 200: %s", rec.Code, rec.Body)
	}
	state := decodeSubs(t, rec.Body)
	if len(state.Items) != 1 {
		t.Fatalf("%d items, want 1", len(state.Items))
	}
	if state.Items[0].URL != "https://sub.example.test/a" {
		t.Errorf("url is %q", state.Items[0].URL)
	}
	if state.Items[0].ID == "" {
		t.Error("the added entry has no id")
	}
}

func TestABadAddressIsRefusedWithAReason(t *testing.T) {
	h, _ := subsUnderTest(t)

	rec := call(t, h, http.MethodPost, "subscriptions", `{"url":"clash://sub.example.test/a"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status is %d, want 400: %s", rec.Code, rec.Body)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding the refusal: %v", err)
	}
	if body["error"] == "" {
		t.Error("the refusal has no message")
	}
}

func TestAnEntryIsReplacedByName(t *testing.T) {
	h, _ := subsUnderTest(t)
	call(t, h, http.MethodPost, "subscriptions", `{"id":"main","url":"https://sub.example.test/a"}`)

	rec := call(t, h, http.MethodPut, "subscriptions", `{"id":"main","url":"https://sub.example.test/moved"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status is %d, want 200: %s", rec.Code, rec.Body)
	}
	state := decodeSubs(t, rec.Body)
	if len(state.Items) != 1 || state.Items[0].URL != "https://sub.example.test/moved" {
		t.Fatalf("items = %+v", state.Items)
	}
}

func TestDeletingAnUnknownEntryIsNotFound(t *testing.T) {
	h, _ := subsUnderTest(t)

	rec := call(t, h, http.MethodDelete, "subscriptions?id=ghost", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status is %d, want 404: %s", rec.Code, rec.Body)
	}
}

func TestAnEntryIsRemoved(t *testing.T) {
	h, store := subsUnderTest(t)
	call(t, h, http.MethodPost, "subscriptions", `{"id":"main","url":"https://sub.example.test/a"}`)

	rec := call(t, h, http.MethodDelete, "subscriptions?id="+url.QueryEscape("main"), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status is %d, want 200: %s", rec.Code, rec.Body)
	}
	if n := len(store.List().Items); n != 0 {
		t.Errorf("%d items left, want 0", n)
	}
}

func TestTheDuplicateRuleIsChangedFromThePanel(t *testing.T) {
	h, store := subsUnderTest(t)

	rec := call(t, h, http.MethodPut, "subscriptions/mode", `{"dedupe":"none"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status is %d, want 200: %s", rec.Code, rec.Body)
	}
	// The mode is what makes the builder keep every record instead of collapsing
	// it into one node per server, so it has to reach the store.
	if got := store.Dedupe(); got != subscriptions.ModeNone {
		t.Errorf("stored dedupe is %q, want %q", got, subscriptions.ModeNone)
	}

	rec = call(t, h, http.MethodPut, "subscriptions/mode", `{"dedupe":"everything"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("an unknown mode was accepted: %d %s", rec.Code, rec.Body)
	}
}

func TestADeploymentWithoutAListAnswersNotImplemented(t *testing.T) {
	h := &Handler{}

	for _, route := range []string{"subscriptions", "subscriptions/mode"} {
		rec := call(t, h, http.MethodGet, route, "")
		if rec.Code != http.StatusNotImplemented {
			t.Errorf("%s: status is %d, want 501", route, rec.Code)
		}
	}
}
