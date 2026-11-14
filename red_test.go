package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRedClip: a RED clip in several files counts with all of them in the drive's video and in what only it has,
// is named without the _001, and is not read again while its files are unchanged.
func TestRedClip(t *testing.T) {
	recs := `{"Path":"/v/A/C001.RDC/C001_001.R3D","Ext":".r3d","Hash":"h","Size":100,"Mtime":"2026-01-01T00:00:00Z","Dur":5,"Codec":"r3d","Span":300}` + "\n"
	c, err := buildCatalog([]drive{{Device: Device{UUID: "A", Name: "A"}, files: strings.NewReader(recs)}}, nil, nil, nil)
	must(t, err)
	d := c.Overview().Devices[0]
	if d.VideoBytes != 300 || d.UniqueB != 300 || c.list[0].Name != "C001.R3D" {
		t.Fatalf("video %d, only here %d, name %q", d.VideoBytes, d.UniqueB, c.list[0].Name)
	}

	dir := t.TempDir()
	vol, out := filepath.Join(dir, "vol"), filepath.Join(dir, "files.jsonl")
	must(t, os.MkdirAll(vol, 0o755))
	first := filepath.Join(vol, "C001_001.R3D")
	must(t, os.WriteFile(first, make([]byte, 10), 0o644))
	must(t, os.WriteFile(filepath.Join(vol, "C001_002.R3D"), make([]byte, 20), 0o644))
	st, _ := os.Stat(first)
	old := Rec{Path: first, Ext: ".r3d", Hash: "h", Size: 10, Mtime: st.ModTime().UTC().Format(time.RFC3339), Dur: 5, Codec: "r3d", Span: 30, P: probeVersion}
	b, _ := json.Marshal(old)
	must(t, os.WriteFile(out, append(b, '\n'), 0o644))
	read := func() Rec {
		t.Helper()
		_, err := inventory(vol, out, nil)
		must(t, err)
		var r Rec
		must(t, eachRec(out, func(x Rec) {
			if x.Path == first {
				r = x
			}
		}))
		return r
	}
	if r := read(); r.Dur != 5 || r.Span != 30 {
		t.Fatalf("unchanged clip read again: %+v", r)
	}
	must(t, os.WriteFile(filepath.Join(vol, "C001_002.R3D"), make([]byte, 25), 0o644)) // a part changed
	if r := read(); r.Dur == 5 {
		t.Fatalf("changed clip not read again: %+v", r)
	}
}
