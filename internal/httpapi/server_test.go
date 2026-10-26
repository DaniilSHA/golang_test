package httpapi

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"taskmanager/internal/config"
	"taskmanager/internal/service"
)

func TestLimiterRefillAndIsolation(t *testing.T) {
	l := newLimiter(60, 2)
	now := time.Now()
	for i := 0; i < 2; i++ {
		if ok, _ := l.allow("a", now); !ok {
			t.Fatal("initial burst rejected")
		}
	}
	if ok, retry := l.allow("a", now); ok || retry < 1 {
		t.Fatal("limit not enforced")
	}
	if ok, _ := l.allow("b", now); !ok {
		t.Fatal("another key blocked")
	}
	if ok, _ := l.allow("a", now.Add(time.Second)); !ok {
		t.Fatal("bucket did not refill")
	}
	l.allow("new", now.Add(11*time.Minute))
	if _, ok := l.entries["b"]; ok {
		t.Fatal("idle entry was not evicted")
	}
}
func TestHTTPAuthValidationAndRateLimit(t *testing.T) {
	s := service.New(nil, nil, "test-secret-at-least-32-characters")
	h := New(s, slog.New(slog.NewTextHandler(io.Discard, nil)), config.Config{AuthRPM: 10, AuthBurst: 1, APIRPM: 120, APIBurst: 20})
	for _, tc := range []struct {
		method, path, body string
		status             int
	}{
		{"GET", "/api/v1/teams", "", 401},
		{"POST", "/api/v1/register", `{"email":true}`, 400},
		{"POST", "/api/v1/login", `{}`, 429},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.status {
			t.Fatalf("%s %s: %d %s", tc.method, tc.path, rec.Code, rec.Body)
		}
		if rec.Header().Get("X-Request-ID") == "" {
			t.Fatal("missing request id")
		}
		if rec.Code == 429 && rec.Header().Get("Retry-After") == "" {
			t.Fatal("missing retry header")
		}
	}
}
func TestJSONRejectsUnknownFieldsAndTrailingValues(t *testing.T) {
	for _, raw := range []string{`{"extra":1}`, `{} {}`, `{"name":`} {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		var b struct {
			Name string `json:"name"`
		}
		if decode(req, &b) == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}
