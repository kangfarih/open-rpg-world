package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"rpg-world-server/internal/account"
)

// resetServer wires the reset endpoints over a real in-memory account store.
func resetServer(t *testing.T) (*Server, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	account.EnsureTables(db)
	if _, err := account.Create(db, "alice", "secret", "alice@example.com"); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	srv := newTestServer()
	srv.Accounts = account.Provider{DB: db}
	return srv, db
}

func postReset(t *testing.T, srv *Server, path, body string, out any) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if out != nil {
		if err := json.NewDecoder(rec.Body).Decode(out); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
	}
	return rec.Code
}

// TestRequestResetIsAntiEnumeration pins hub api.ts handleRequestReset: a known
// and an unknown email both answer success, and a malformed one answers invalid.
func TestRequestResetIsAntiEnumeration(t *testing.T) {
	srv, _ := resetServer(t)

	var ok struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	if code := postReset(t, srv, "/api/v1/requestReset", `{"email":"alice@example.com"}`, &ok); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if ok.Status != "success" {
		t.Fatalf("known email = %+v, want success", ok)
	}
	postReset(t, srv, "/api/v1/requestReset", `{"email":"nobody@example.com"}`, &ok)
	if ok.Status != "success" {
		t.Fatalf("unknown email = %+v, want success (no enumeration)", ok)
	}
	postReset(t, srv, "/api/v1/requestReset", `{"email":"nope"}`, &ok)
	if ok.Error != "invalid" {
		t.Fatalf("malformed email = %+v, want error=invalid", ok)
	}
	postReset(t, srv, "/api/v1/requestReset", `{`, &ok)
	if ok.Error != "invalid" {
		t.Fatalf("malformed body = %+v, want error=invalid", ok)
	}
}

// TestResetPasswordRoundTrip drives the full flow the client's reset page uses:
// mint a token, consume it, and verify the new password authenticates while the
// old one does not. A consumed/invalid token answers the single 'invalid'.
func TestResetPasswordRoundTrip(t *testing.T) {
	srv, db := resetServer(t)

	id, token, ok := srv.Accounts.CreateResetToken("alice@example.com")
	if !ok {
		t.Fatal("CreateResetToken failed for a known email")
	}

	var res struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	body := `{"id":"` + id + `","token":"` + token + `","password":"newsecret"}`
	if code := postReset(t, srv, "/api/v1/resetPassword", body, &res); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if res.Status != "success" {
		t.Fatalf("reset = %+v, want success", res)
	}
	if _, ok := account.Authenticate(db, "alice", "newsecret"); !ok {
		t.Fatal("new password does not authenticate")
	}
	if _, ok := account.Authenticate(db, "alice", "secret"); ok {
		t.Fatal("old password still authenticates after a reset")
	}

	// The token is single-use.
	postReset(t, srv, "/api/v1/resetPassword", body, &res)
	if res.Status != "invalid" {
		t.Fatalf("reused token = %+v, want invalid", res)
	}
	// Bad inputs answer invalid, and a too-short password never reaches the store.
	postReset(t, srv, "/api/v1/resetPassword", `{"id":"`+id+`","token":"`+token+`","password":"ab"}`, &res)
	if res.Error != "invalid" {
		t.Fatalf("short password = %+v, want error=invalid", res)
	}
	postReset(t, srv, "/api/v1/resetPassword", `{"token":"`+token+`","password":"newsecret"}`, &res)
	if res.Error != "invalid" {
		t.Fatalf("missing id = %+v, want error=invalid", res)
	}
}

// TestRequestResetWithoutProvider pins the disabled surface: no account store
// means the client gets a parsable error instead of a panic.
func TestRequestResetWithoutProvider(t *testing.T) {
	srv := newTestServer()
	var res struct {
		Error string `json:"error"`
	}
	postReset(t, srv, "/api/v1/requestReset", `{"email":"alice@example.com"}`, &res)
	if res.Error != "invalid" {
		t.Fatalf("reset without a provider = %+v, want error=invalid", res)
	}
}
