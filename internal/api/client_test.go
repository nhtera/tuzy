package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
)

func TestErrorsAreTyped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tzy_x" {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"error":{"code":"unauthorized","message":"not logged in: run ` + "`tuzy login`" + `"}}`))
			return
		}
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":{"code":"rate_limited","message":"slow down"}}`))
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)

	_, err := New(u, "", "tuzy/test").Me(context.Background())
	if !IsCode(err, "unauthorized") || err.Error() != "not logged in: run `tuzy login`" {
		t.Fatalf("err = %v", err)
	}
	_, err = New(u, "tzy_x", "").ListTokens(context.Background())
	var e *Error
	if !IsCode(err, "rate_limited") || !asErr(err, &e) || e.RetryAfter.Seconds() != 60 {
		t.Fatalf("err = %#v", err)
	}
}

func asErr(err error, target **Error) bool {
	e, ok := err.(*Error)
	*target = e
	return ok
}

func TestLoginRoundTrip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		switch r.URL.Path {
		case "/api/v1/auth/email/start":
			_ = json.NewEncoder(w).Encode(map[string]any{"login_id": "abc", "expires_in": 600})
		case "/api/v1/auth/email/verify":
			if in["code"] != "123456" || in["login_id"] != "abc" || in["label"] != "mbp (darwin)" {
				w.WriteHeader(400)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "tzy_new", "user": map[string]string{"id": "usr_1", "email": "a@b.c"}})
		}
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	c := New(u, "", "")
	st, err := c.StartLogin(context.Background(), "a@b.c")
	if err != nil || st.LoginID != "abc" {
		t.Fatalf("start: %v %+v", err, st)
	}
	tok, user, err := c.VerifyLogin(context.Background(), st.LoginID, "123456", "mbp (darwin)")
	if err != nil || tok != "tzy_new" || user.Email != "a@b.c" {
		t.Fatalf("verify: %v %q %+v", err, tok, user)
	}
}

func TestNeverFollowsRedirects(t *testing.T) {
	var leaked atomic.Bool
	evil := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			leaked.Store(true)
		}
	}))
	defer evil.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, evil.URL+"/steal", http.StatusFound)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	if _, err := New(u, "tzy_secret", "").Me(context.Background()); err == nil {
		t.Fatal("expected an error for a redirect response")
	}
	if leaked.Load() {
		t.Fatal("Authorization was forwarded through a redirect")
	}
}
