package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// TestReading: a drive is busy while rushbin reads a file on it, and free again when the reading ends; a path on
// no connected drive marks nothing.
func TestReading(t *testing.T) {
	app := &App{dir: t.TempDir(), mounted: map[string]string{"A": "/Volumes/A", "B": "/Volumes/AB"}}
	must(t, app.reload())
	busy := func() []any {
		w := httptest.NewRecorder()
		app.statusHandler(w, httptest.NewRequest("GET", "/api/status", nil))
		var st struct{ Busy []any }
		must(t, json.Unmarshal(w.Body.Bytes(), &st))
		return st.Busy
	}
	done := app.reading("/Volumes/A/Day 1/clip.mov")
	app.reading("/Users/x/clip.mov")()
	if b := busy(); len(b) != 1 || b[0] != "/Volumes/A" {
		t.Fatalf("busy while reading: %v", b)
	}
	done()
	if b := busy(); len(b) != 0 {
		t.Fatalf("busy after reading: %v", b)
	}
}

// TestDeviceRewrite: a drive's device.json rewritten (a scan starting, an option changed) while the catalog
// reloads never makes the drive vanish from it.
func TestDeviceRewrite(t *testing.T) {
	dir := t.TempDir()
	d := filepath.Join(dir, "disks", "A")
	must(t, os.MkdirAll(d, 0o755))
	must(t, os.WriteFile(filepath.Join(d, "files.jsonl"), nil, 0o644))
	b, _ := json.Marshal(Device{UUID: "A", Name: "A", Excluded: []string{}})
	must(t, writeFile(filepath.Join(d, "device.json"), b))
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				writeFile(filepath.Join(d, "device.json"), b)
			}
		}
	}()
	defer close(stop)
	for i := 0; i < 300; i++ {
		c, err := loadCatalog(dir, nil)
		must(t, err)
		if len(c.Overview().Devices) != 1 {
			t.Fatalf("reload %d: the drive vanished", i)
		}
	}
}
