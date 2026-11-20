package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWebUIHandler(t *testing.T) {
	handler := Handler()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d", rec.Code, http.StatusOK)
	}

	ct := rec.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type = %q, want text/html", ct)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "providermux") {
		t.Fatalf("expected body to contain 'providermux', got %q", body)
	}
}

// panelBody renders the embedded document once, so structural assertions run
// against the same bytes the browser receives.
func panelBody(t *testing.T) string {
	t.Helper()
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d", rec.Code, http.StatusOK)
	}
	return rec.Body.String()
}

func TestPanelRoutes(t *testing.T) {
	body := panelBody(t)
	for _, route := range []string{"overview", "providers", "models", "network", "settings"} {
		if !strings.Contains(body, `data-route="`+route+`"`) {
			t.Errorf("route %q is not reachable from the shell", route)
		}
	}
	for _, gone := range []string{"quota", "exit"} {
		if strings.Contains(body, `data-route="`+gone+`"`) {
			t.Errorf("route %q still has its own nav entry", gone)
		}
	}
	if !strings.Contains(body, `ROUTE_ALIASES = { quota: "providers", exit: "network" }`) {
		t.Error("route aliases are missing")
	}
}

func TestSingleRefreshControl(t *testing.T) {
	if n := strings.Count(panelBody(t), `data-act="refresh"`); n != 1 {
		t.Fatalf("refresh controls = %d, want exactly one global control", n)
	}
}

func TestQuotaLivesInProviderCard(t *testing.T) {
	body := panelBody(t)
	for _, want := range []string{
		`data-open-key="prov:`,
		`QUOTA_TTL`,
		`p.id === "antigravity" ? quotaSect()`,
		`ensureQuota`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("panel is missing %q", want)
		}
	}
	if strings.Contains(body, `href="#/quota"`) {
		t.Error("quota is still linked as a separate screen")
	}
}

func TestNoInlineEventHandlers(t *testing.T) {
	body := panelBody(t)
	for _, attr := range []string{"onclick=", "onsubmit=", "onchange=", "oninput="} {
		if strings.Contains(body, attr) {
			t.Errorf("inline handler %q found; the panel wires events in one listener", attr)
		}
	}
}
