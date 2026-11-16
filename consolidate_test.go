package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
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
	app := &App{dir: t.TempDir(), cat: c, move: &moveJob{Project: "Sanremo2026", To: "A", State: "done"}}
	c.decide(DecisionLine{Content: "c:a", Kind: "copy", Value: "B|BILLBOARD - SANREMO 2026/Footage/a.mov"})
	cl := &Clip{Device: "A", ID: "Sanremo2026/Footage/b.mov", Path: filepath.Join(a, "Sanremo2026", "Footage", "b.mov"), Project: "Sanremo2026", Content: "c:b", Size: 6}
	c.list, c.byCont["c:b"] = append(c.list, cl), append(c.byCont["c:b"], cl)
	app.keepProject("Sanremo2026", "A")
	if c.decision("c:a").Copy != "A|Sanremo2026/Footage/a.mov" || c.decision("c:b").Copy != "A|Sanremo2026/Footage/b.mov" || c.decision("c:c").Copy != "" {
		t.Fatalf("kept: a %q, b %q, c %q", c.decision("c:a").Copy, c.decision("c:b").Copy, c.decision("c:c").Copy)
	}
	if m := app.move; !m.Kept || len(m.Before) != 1 || m.Before["LaCie"] != 1 { // the page says: for 1 clip you had chosen LaCie
		t.Fatalf("move after keeping the project: %+v", m)
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

// TestMove: a move paused before it starts copies nothing; resumed, it copies what is missing into folders it makes;
// cancelled, it takes away only what it wrote and is unchanged, the hidden file a copy cut short left, and its
// folders once empty: what was on the drive before stays.
func TestMove(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	c := &Catalog{byCont: map[string][]*Clip{}, files: map[string]Rec{}, dec: map[string]*Decision{}}
	c.Devices = []*DeviceInfo{{Device: Device{UUID: "A", Name: "T9", Mount: a}, Online: true}, {Device: Device{UUID: "B", Name: "LaCie", Mount: b}, Online: true}}
	mtime := time.Date(2026, 2, 27, 16, 59, 55, 0, time.UTC)
	for i, rel := range []string{"P/x.mov", "P/Footage/y.mov", "P/Footage/z.mov"} {
		dev, mount := "B", b
		if i == 0 { // the project's folder on T9: where the rest goes
			dev, mount = "A", a
		}
		p := filepath.Join(mount, filepath.FromSlash(rel))
		must(t, os.MkdirAll(filepath.Dir(p), 0o755))
		must(t, os.WriteFile(p, []byte(rel), 0o644))
		must(t, os.Chtimes(p, mtime, mtime))
		c.files[p] = Rec{Path: p, Size: int64(len(rel)), Mtime: mtime.Format(time.RFC3339)}
		cl := &Clip{Device: dev, ID: rel, Path: p, Project: "P", Content: "c:" + rel, Size: int64(len(rel))}
		c.list, c.byCont[cl.Content] = append(c.list, cl), []*Clip{cl}
	}
	app := &App{dir: t.TempDir(), cat: c, queue: make(chan string, 8)}
	todo, _, err := consolidation(c, "P", "A")
	must(t, err)
	app.move, app.copyTo = &moveJob{Project: "P", To: "A", Drive: "T9", State: "running", Files: len(todo)}, a
	app.moveCtl.Store(1)
	app.runMove(todo)
	if m := app.move; m.State != "paused" || m.Copied != 0 || app.copyTo != "" {
		t.Fatalf("paused before the first file: %+v", m)
	}
	if todo, err = app.resumeMove(); err != nil || len(todo) != 2 {
		t.Fatalf("resume: %v, %d to copy", err, len(todo))
	}
	app.runMove(todo)
	m := app.move
	if m.State != "done" || m.Copied != 2 || len(m.Made) != 2 || !slices.Equal(m.Dirs, []string{filepath.Join(a, "P", "Footage")}) {
		t.Fatalf("resumed: %+v", m)
	}
	m.State = "paused" // as if paused after these two: cancel takes them away
	z := filepath.Join(a, "P", "Footage", "z.mov")
	must(t, os.WriteFile(z, []byte("changed since"), 0o644))
	must(t, os.WriteFile(filepath.Join(a, "P", "Footage", ".w.mov-rushbin-1"), []byte("half"), 0o644))
	must(t, app.cancelMove())
	left, _ := filepath.Glob(filepath.Join(a, "P", "Footage", "*"))
	hidden, _ := filepath.Glob(filepath.Join(a, "P", "Footage", ".*"))
	if m.State != "cancelled" || m.Changed != 1 || !slices.Equal(left, []string{z}) || len(hidden) != 0 || !fileExists(filepath.Join(a, "P", "x.mov")) {
		t.Fatalf("cancelled: %+v, left %v %v", m, left, hidden)
	}
	must(t, os.Remove(z))
	m.State = "paused"
	must(t, app.cancelMove())
	if dirExists(filepath.Join(a, "P", "Footage")) || !dirExists(filepath.Join(a, "P")) {
		t.Fatal("the folder the move made stays once empty, or the project's own went")
	}
}

// TestAutoVerify: the duplicates of a project the page stopped are not read; once it asks again, both copies are
// read whole and their checksums kept.
func TestAutoVerify(t *testing.T) {
	a, b, dir := t.TempDir(), t.TempDir(), t.TempDir()
	c := &Catalog{byCont: map[string][]*Clip{}, files: map[string]Rec{}, dec: map[string]*Decision{}}
	c.Devices = []*DeviceInfo{{Device: Device{UUID: "A", Name: "T9", Mount: a}, Online: true}, {Device: Device{UUID: "B", Name: "LaCie", Mount: b}, Online: true}}
	for _, d := range []struct{ id, mount string }{{"A", a}, {"B", b}} {
		p := filepath.Join(d.mount, "P", "x.mov")
		must(t, os.MkdirAll(filepath.Dir(p), 0o755))
		must(t, os.WriteFile(p, []byte("same"), 0o644))
		must(t, os.MkdirAll(filepath.Join(dir, "disks", d.id), 0o755))
		cl := &Clip{Device: d.id, ID: "P/x.mov", Path: p, Project: "P", Content: "c:x", Size: 4}
		c.list, c.byCont["c:x"] = append(c.list, cl), append(c.byCont["c:x"], cl)
	}
	app := &App{dir: dir, cat: c, verifyNow: make(chan struct{}, 1)}
	must(t, app.setVerify("P", false))
	if app.autoVerify() {
		t.Fatal("verified a project the page stopped")
	}
	must(t, app.setVerify("P", true))
	if !app.autoVerify() {
		t.Fatal("nothing verified once asked again")
	}
	for _, id := range []string{"A", "B"} {
		if b, _ := os.ReadFile(filepath.Join(dir, "disks", id, "fullhash.tsv")); !strings.Contains(string(b), "P/x.mov") {
			t.Fatalf("%s: no checksum kept: %q", id, b)
		}
	}
}

// TestFrees: a clip only on a drive not connected never reaches the drive chosen, so its copies free nothing.
func TestFrees(t *testing.T) {
	c := &Catalog{byCont: map[string][]*Clip{}, dec: map[string]*Decision{}}
	c.Devices = []*DeviceInfo{{Device: Device{UUID: "A", Name: "T9"}, Online: true}, {Device: Device{UUID: "B", Name: "LaCie"}}, {Device: Device{UUID: "C", Name: "Old"}}}
	add := func(dev, content string) {
		cl := &Clip{Device: dev, ID: content + ".mov", Path: "/x/" + dev + "/" + content, Project: "P", Content: content, Size: 10}
		c.list, c.byCont[content] = append(c.list, cl), append(c.byCont[content], cl)
	}
	add("A", "on-a") // on T9 already: its copy on LaCie frees
	add("B", "on-a")
	add("B", "off") // only on drives not connected: it stays where it is
	add("C", "off")
	if got := frees(c, "P", "A", false); got != 10 {
		t.Fatalf("frees %d, want 10", got)
	}
	if got := frees(c, "P", "A", true); got != 0 {
		t.Fatalf("with the backup frees %d, want 0", got)
	}
}

// TestEmptyTrash: what a drive's trash holds is counted, then deleted for good only there: the rest of the drive
// stays, and a drive not connected is refused.
func TestEmptyTrash(t *testing.T) {
	a, dir := t.TempDir(), t.TempDir()
	must(t, os.MkdirAll(filepath.Join(a, trash, "P"), 0o755))
	must(t, os.WriteFile(filepath.Join(a, trash, "P", "x.mov"), []byte("12345"), 0o644))
	must(t, os.WriteFile(filepath.Join(a, "keep.mov"), []byte("k"), 0o644))
	c := &Catalog{byCont: map[string][]*Clip{}}
	c.Devices = []*DeviceInfo{{Device: Device{UUID: "A", Name: "T9", Mount: a}, Online: true}, {Device: Device{UUID: "B", Name: "LaCie", Mount: "/Volumes/nowhere"}}}
	app := &App{dir: dir, cat: c}
	rec := httptest.NewRecorder()
	app.trashes(rec, httptest.NewRequest("GET", "/api/trash", nil))
	if got := strings.TrimSpace(rec.Body.String()); got != `[{"bytes":5,"drive":"T9","files":1,"uuid":"A"}]` {
		t.Fatalf("trash listed as %s", got)
	}
	rec = httptest.NewRecorder()
	app.emptyTrash(rec, httptest.NewRequest("POST", "/api/trash/empty", strings.NewReader(`{"uuid":"B"}`)))
	if rec.Code != 409 {
		t.Fatalf("emptied a drive not connected: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	app.emptyTrash(rec, httptest.NewRequest("POST", "/api/trash/empty", strings.NewReader(`{"uuid":"A"}`)))
	if rec.Code != 200 || dirExists(filepath.Join(a, trash)) || !fileExists(filepath.Join(a, "keep.mov")) {
		t.Fatalf("empty: %d, trash still there %v, rest gone %v", rec.Code, dirExists(filepath.Join(a, trash)), !fileExists(filepath.Join(a, "keep.mov")))
	}
}
