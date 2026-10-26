package service

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"taskmanager/internal/domain"
)

func TestJWTValidation(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := New(nil, nil, "test-secret-at-least-32-characters")
	s.Now = func() time.Time { return now }
	for _, tc := range []struct {
		name    string
		method  jwt.SigningMethod
		expires time.Time
		issuer  string
		id      int64
		ok      bool
	}{
		{"valid", jwt.SigningMethodHS256, now.Add(time.Hour), "taskmanager", 1, true},
		{"expired", jwt.SigningMethodHS256, now.Add(-time.Second), "taskmanager", 1, false},
		{"wrong algorithm", jwt.SigningMethodHS384, now.Add(time.Hour), "taskmanager", 1, false},
		{"wrong issuer", jwt.SigningMethodHS256, now.Add(time.Hour), "other", 1, false},
		{"missing user", jwt.SigningMethodHS256, now.Add(time.Hour), "taskmanager", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := jwt.NewWithClaims(tc.method, Claims{UserID: tc.id, RegisteredClaims: jwt.RegisteredClaims{Issuer: tc.issuer, ExpiresAt: jwt.NewNumericDate(tc.expires)}}).SignedString(s.Secret)
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.Authenticate(raw)
			if (err == nil) != tc.ok {
				t.Fatalf("ok=%v err=%v", tc.ok, err)
			}
		})
	}
	for _, raw := range []string{"", "not.a.token"} {
		if _, err := s.Authenticate(raw); err == nil {
			t.Fatal("accepted invalid token")
		}
	}
}
func TestCursorBoundToFilters(t *testing.T) {
	c := domain.Cursor{TeamID: 1, Status: "todo", CreatedAt: time.Now().UTC(), ID: 50}
	b, _ := json.Marshal(c)
	raw := base64.RawURLEncoding.EncodeToString(b)
	if _, err := decodeCursor(domain.TaskFilter{TeamID: 1, Status: "todo", Cursor: raw}); err != nil {
		t.Fatal(err)
	}
	for _, f := range []domain.TaskFilter{{TeamID: 2, Status: "todo", Cursor: raw}, {TeamID: 1, Status: "done", Cursor: raw}, {TeamID: 1, Cursor: "%%%"}} {
		if _, err := decodeCursor(f); err == nil {
			t.Fatalf("accepted cursor %+v", f)
		}
	}
}
