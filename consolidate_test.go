package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestConsolidate: the drive chosen gets what only the others have of the project, inside its own folder of it,
// each file once; what only a drive not connected has is counted; nothing already there is overwritten.
func TestConsolidate(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	c := &Catalog{byCont: map[string][]*Clip{}, files: map[string]Rec{}, dec: map[string]*Decision{}}
	c.Devices = []*DeviceInfo{{Device: Device{UUID: "A", Name: "T9", Mount: a}, Online: true},
		{Device: Device{UUID: "B", Name: "LaCie", Mount: b}, Online: true}, {Device: Device{UUID: "C", Name: "Old", Mount: "/Volumes/Old"}}}
	mtime := time.Date(2026, 2, 27, 16, 59, 55, 0, time.UTC)
	file := func(dev, mount, rel, content, data string) {
		p := filepath.Join(mount, filepath.FromSlash(rel))
		if mount != "/Volumes/Old" {
			must(t, os.MkdirAll(filepath.Dir(p), 0o755))
			must(t, os.WriteFile(p, []byte(data), 0o644))
		}
		c.files[p] = Rec{Path: p, Size: int64(len(data)), Mtime: mtime.Format(time.RFC3339)}
		if content != "" {
			cl := &Clip{Device: dev, ID: rel, Path: p, Project: "Sanremo2026", Content: content, Size: int64(len(data))}
			c.list = append(c.list, cl)
			c.byCont[content] = append(c.byCont[content], cl)
		}
	}
	file("A", a, "Sanremo2026/Footage/a.mov", "c:a", "aaaa")
	file("B", b, "BILLBOARD - SANREMO 2026/Footage/a.mov", "c:a", "aaaa") // A has it
	file("B", b, "BILLBOARD - SANREMO 2026/Footage/b.mov", "c:b", "bbbbbb")
	file("B", b, "BILLBOARD - SANREMO 2026/Audio/s.wav", "", "sound")
	file("B", b, "BILLBOARD - SANREMO 2026/Footage/c.mov", "c:c", "cc")
	file("C", "/Volumes/Old", "Sanremo/x.mov", "c:x", "xxx") // only on a drive not connected
	file("C", "/Volumes/Old", "Sanremo/b.mov", "c:b", "bbbbbb")
	must(t, os.MkdirAll(filepath.Join(a, "Sanremo2026", "Footage"), 0o755))
	must(t, os.WriteFile(filepath.Join(a, "Sanremo2026", "Footage", "c.mov"), []byte("other"), 0o644)) // another file by that name

	todo, offline, err := consolidation(c, "Sanremo2026", "A")
	must(t, err)
	want := map[string]string{filepath.Join(a, "Sanremo2026", "Footage", "b.mov"): "bbbbbb", filepath.Join(a, "Sanremo2026", "Audio", "s.wav"): "sound",
		filepath.Join(a, "Sanremo2026", "Footage", "c.mov"): "cc"}
	if len(todo) != 3 || offline != 1 {
		t.Fatalf("to copy %+v, %d only offline; want b.mov, s.wav and c.mov from LaCie, 1 offline", todo, offline)
	}
	for _, cp := range todo {
		if _, ok := want[cp.To]; !ok || filepath.Dir(cp.From) == filepath.Dir(cp.To) {
			t.Fatalf("copy %+v goes to the wrong place", cp)
		}
		orig, back, err := copyChecked(cp.From, cp.To, mtime, io.Discard)
		if filepath.Base(cp.To) == "c.mov" {
			if !errors.Is(err, errExists) {
				t.Fatalf("over another file by the same name: %v", err)
			}
			continue
		}
		must(t, err)
		got, _ := os.ReadFile(cp.To)
		st, _ := os.Stat(cp.To)
		if !bytes.Equal(got, []byte(want[cp.To])) || !st.ModTime().Equal(mtime) {
			t.Fatalf("%s: %q at %v", cp.To, got, st.ModTime())
		}
		sum := fmt.Sprintf("%x\t", sha256.Sum256(got)) // the check done while copying: Duplicati does not read them again
		src, _ := os.Stat(cp.From)
		if orig != sum+stamp(cp.From, src.Size(), src.ModTime().Unix()) || runtime.GOOS == "darwin" && back != sum+stamp(cp.To, st.Size(), mtime.Unix()) {
			t.Fatalf("hashes %q and %q for %s", orig, back, cp.To)
		}
	}
	if got, _ := os.ReadFile(filepath.Join(a, "Sanremo2026", "Footage", "c.mov")); string(got) != "other" {
		t.Fatal("a file already on the drive was overwritten")
	}
	if left, _ := filepath.Glob(filepath.Join(a, "Sanremo2026", "*", ".*")); len(left) != 0 {
		t.Fatalf("hidden files left behind: %v", left)
	}
	if _, _, err := consolidation(c, "Sanremo2026", "C"); err == nil {
		t.Fatal("copying to a drive not connected")
	}

	// Once T9 is read again with the copies, each clip of the project keeps its copy there: a.mov was kept on the
	// LaCie by an earlier choice, which changes; b.mov had none; c.mov is only on the LaCie, nothing to choose.
	app := &App{dir: t.TempDir(), cat: c}
	c.decide(DecisionLine{Content: "c:a", Kind: "copy", Value: "B|BILLBOARD - SANREMO 2026/Footage/a.mov"})
	cl := &Clip{Device: "A", ID: "Sanremo2026/Footage/b.mov", Path: filepath.Join(a, "Sanremo2026", "Footage", "b.mov"), Project: "Sanremo2026", Content: "c:b", Size: 6}
	c.list, c.byCont["c:b"] = append(c.list, cl), append(c.byCont["c:b"], cl)
	app.keepProject("Sanremo2026", "A", "T9", 2)
	if c.decision("c:a").Copy != "A|Sanremo2026/Footage/a.mov" || c.decision("c:b").Copy != "A|Sanremo2026/Footage/b.mov" || c.decision("c:c").Copy != "" {
		t.Fatalf("kept: a %q, b %q, c %q", c.decision("c:a").Copy, c.decision("c:b").Copy, c.decision("c:c").Copy)
	}
	if app.copyNote != "Sanremo2026 is all on T9: 2 files copied. For 1 clips you had chosen LaCie: now this is the project drive" {
		t.Fatalf("note %q", app.copyNote)
	}
	if j, _ := json.Marshal((&Catalog{byCont: map[string][]*Clip{}}).Dups()); string(j) != "[]" { // the page reads .length
		t.Fatalf("no duplicates as %s", j)
	}
}

// TestFollowMoves: a clip moved by hand on the drive keeps its check byte for byte; a copy made by hand is read
// again, and so is a move when more came than went (which one is the copy cannot be told).
func TestFollowMoves(t *testing.T) {
	dir := t.TempDir()
	rec := func(p, h string) Rec { return Rec{Path: p, Hash: h, Size: 9, Mtime: "2026-02-27T16:59:55Z"} }
	var b strings.Builder
	for _, r := range []Rec{rec("/V/moved/a.mxf", "a"), rec("/V/b.mxf", "b"), rec("/V/copy/b.mxf", "b"), rec("/V/x/c.mxf", "c"), rec("/V/y/c.mxf", "c")} {
		j, _ := json.Marshal(r)
		b.Write(append(j, '\n'))
	}
	must(t, os.WriteFile(filepath.Join(dir, "files.jsonl"), []byte(b.String()), 0o644))
	at := time.Date(2026, 2, 27, 16, 59, 55, 0, time.UTC).Unix()
	full := ""
	for _, p := range []string{"/V/a.mxf", "/V/b.mxf", "/V/c.mxf"} {
		full += "sum-" + filepath.Base(p) + "\t" + stamp(p, 9, at) + "\n"
	}
	must(t, os.WriteFile(filepath.Join(dir, "fullhash.tsv"), []byte(full), 0o644))
	before := map[string]Rec{"/V/a.mxf": rec("/V/a.mxf", "a"), "/V/b.mxf": rec("/V/b.mxf", "b"), "/V/c.mxf": rec("/V/c.mxf", "c")}
	must(t, followMoves(dir, before))
	f, _ := os.Open(filepath.Join(dir, "fullhash.tsv"))
	got, _ := fullHashes(f)
	f.Close()
	if got[stamp("/V/moved/a.mxf", 9, at)] != "sum-a.mxf" || got[stamp("/V/copy/b.mxf", 9, at)] != "" || got[stamp("/V/x/c.mxf", 9, at)] != "" {
		t.Fatalf("checks after the moves: %v", got)
	}
}

// TestCopyNoRescan: while a project is copied to a drive, its own writes do not ask to read the drive again (one
// reading per file written); a change on another drive still does, and so does one there once the copy ends.
func TestCopyNoRescan(t *testing.T) {
	dir, a, b := t.TempDir(), t.TempDir(), t.TempDir()
	for _, id := range []string{"A", "B"} {
		must(t, os.MkdirAll(filepath.Join(dir, "disks", id), 0o755))
		must(t, os.WriteFile(filepath.Join(dir, "disks", id, "files.jsonl"), nil, 0o644))
	}
	app := &App{dir: dir, seen: map[string]string{a: "A", b: "B"}, queue: make(chan string, 8), copyTo: a}
	app.changed(filepath.Join(a, "P", "clip.mxf"))
	app.changed(filepath.Join(b, "Q", "clip.mov"))
	if app.queued[a] || !app.queued[b] {
		t.Fatalf("queued during the copy to A: %v", app.queued)
	}
	app.copyTo = ""
	if app.changed(filepath.Join(a, "P", "clip.mxf")); !app.queued[a] {
		t.Fatal("a change on A after the copy is not read")
	}
}
