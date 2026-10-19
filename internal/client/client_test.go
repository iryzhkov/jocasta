package client

import (
	"context"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func config(t *testing.T, url string) Config {
	t.Helper()
	p := filepath.Join(t.TempDir(), "token")
	if e := os.WriteFile(p, []byte("test-secret\n"), 0600); e != nil {
		t.Fatal(e)
	}
	return Config{URL: url, TokenFile: p, AllowLoopbackHTTP: true}
}
func TestTLSAndEndpointSafety(t *testing.T) {
	for _, u := range []string{"http://homelab:8080", "http://192.168.1.2", "https://user:password@example.com", "ftp://localhost", "https://example.com?q=x"} {
		if _, e := New(config(t, u)); e == nil {
			t.Fatalf("accepted unsafe endpoint %s", u)
		}
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-secret" {
			t.Error("missing bearer")
		}
		io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()
	conf := config(t, srv.URL)
	c, e := New(conf)
	if e != nil {
		t.Fatal(e)
	}
	var out map[string]bool
	if e = c.Call(context.Background(), "GET", "/v1/get", nil, nil, "", &out); e == nil {
		t.Fatal("untrusted TLS certificate accepted")
	}
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if e = os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0600); e != nil {
		t.Fatal(e)
	}
	conf.CAFile = ca
	c, e = New(conf)
	if e != nil {
		t.Fatal(e)
	}
	if e = c.Call(context.Background(), "GET", "/v1/get", nil, nil, "", &out); e != nil || !out["ok"] {
		t.Fatalf("private CA did not work: %v", e)
	}
}
func TestNoRedirectAndStableRetry(t *testing.T) {
	reached := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	c, e := New(config(t, redirect.URL))
	if e != nil {
		t.Fatal(e)
	}
	if e = c.Call(context.Background(), "GET", "/v1/get", nil, nil, "", nil); e == nil || reached {
		t.Fatal("redirect followed")
	}
	attempts := 0
	var first []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		b, _ := io.ReadAll(r.Body)
		if attempts == 1 {
			first = b
		}
		if string(first) != string(b) || r.Header.Get("Idempotency-Key") != "request-123" {
			t.Error("retry changed request")
		}
		if attempts == 1 {
			h, _, e := w.(http.Hijacker).Hijack()
			if e != nil {
				t.Error(e)
				return
			}
			h.Close()
			return
		}
		io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()
	c, e = New(config(t, srv.URL))
	if e != nil {
		t.Fatal(e)
	}
	var out any
	e = c.Call(context.Background(), "POST", "/v1/mutate", nil, map[string]string{"content": "exact\r\n"}, "request-123", &out)
	if e != nil || attempts != 2 {
		t.Fatalf("lost-response retry failed: %v, attempts %d", e, attempts)
	}
}
func TestCredentialNotInErrors(t *testing.T) {
	c, e := New(config(t, "https://127.0.0.1:1"))
	if e != nil {
		t.Fatal(e)
	}
	e = c.Call(context.Background(), "GET", "/v1/get", nil, nil, "", nil)
	if e == nil || strings.Contains(e.Error(), "test-secret") {
		t.Fatalf("bad error %v", e)
	}
}
