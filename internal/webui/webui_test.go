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
