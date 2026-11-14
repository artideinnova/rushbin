package main

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

// TestStatusVersion: every reload of the catalog (a drive connected or scanned) moves the version in the status,
// so the page reads the catalog again instead of showing a connected drive as not connected.
func TestStatusVersion(t *testing.T) {
	app := &App{dir: t.TempDir()}
	version := func() float64 {
		t.Helper()
		w := httptest.NewRecorder()
		app.statusHandler(w, httptest.NewRequest("GET", "/api/status", nil))
		var st map[string]any
		must(t, json.Unmarshal(w.Body.Bytes(), &st))
		v, ok := st["version"].(float64)
		if !ok {
			t.Fatalf("no version in %s", w.Body)
		}
		return v
	}
	must(t, app.reload())
	v := version()
	must(t, app.reload())
	if w := version(); w <= v {
		t.Fatalf("version %v after a reload, was %v", w, v)
	}
}
