package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMoved(t *testing.T) {
	to := httptest.NewServer(http.NotFoundHandler())
	defer to.Close()
	old := httptest.NewServer(http.RedirectHandler(to.URL+"/", http.StatusMovedPermanently))
	defer old.Close()
	login := httptest.NewServer(http.RedirectHandler("/login", http.StatusFound)) // a site that has not moved
	defer login.Close()
	if got := moved(old.URL); got != to.URL {
		t.Errorf("moved site: %q, want %q", got, to.URL)
	}
	if got := moved(login.URL); got != "" {
		t.Errorf("site that has not moved: %q", got)
	}
	if got := moved("http://127.0.0.1:1"); got != "" {
		t.Errorf("unreachable site: %q", got)
	}
	plain := httptest.NewServer(http.RedirectHandler("http://example.com/", http.StatusMovedPermanently))
	defer plain.Close()
	if got := moved(plain.URL); got != "" {
		t.Errorf("move to plain http: %q", got)
	}
}
