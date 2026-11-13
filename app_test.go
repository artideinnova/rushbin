package main

import (
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// TestApp drives the engine like the app window does: duplicates across two fake drives, a decision on the
// copy to keep, a discard, the trash plan and the moves. Also checks that requests without the key are refused.
func TestApp(t *testing.T) {
	tmp := t.TempDir()
	a, b, cat := filepath.Join(tmp, "A"), filepath.Join(tmp, "B"), filepath.Join(tmp, "catalog")
	must(t, os.MkdirAll(filepath.Join(a, "P"), 0o755))
	must(t, os.MkdirAll(filepath.Join(b, "C"), 0o755))
	ffmpeg(t, "testsrc=duration=2:size=320x240:rate=25", filepath.Join(a, "P", "clip.mp4"))
	ffmpeg(t, "testsrc2=duration=2:size=320x240:rate=25", filepath.Join(a, "P", "other.mp4"))
	copyFile(t, filepath.Join(a, "P", "clip.mp4"), filepath.Join(a, "P", "copy.mp4"))
	copyFile(t, filepath.Join(a, "P", "clip.mp4"), filepath.Join(b, "C", "clip.mp4"))
	scanned(t, cat, a, "A", "", nil)
	scanned(t, cat, b, "B", "", nil)

	app := &App{dir: cat, token: "secret", mounted: map[string]string{"A": a, "B": b}, seen: map[string]string{},
		ignored: map[string]bool{}, queue: make(chan string, 64)}
	must(t, app.reload())
	srv := httptest.NewServer(app.guard(app.routes()))
	defer srv.Close()

	if r, _ := http.Get(srv.URL + "/api/dups"); r.StatusCode != 403 {
		t.Fatalf("without the key: %d, want 403", r.StatusCode)
	}
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	if r, err := c.Get(srv.URL + "/?k=secret"); err != nil || r.StatusCode != 200 {
		t.Fatalf("opening with the key: %v %v", r, err)
	}
	get := func(path string, v any) {
		t.Helper()
		r, err := c.Get(srv.URL + path)
		must(t, err)
		defer r.Body.Close()
		must(t, json.NewDecoder(r.Body).Decode(v))
	}
	post := func(path, body string, header bool) int {
		t.Helper()
		req, _ := http.NewRequest("POST", srv.URL+path, strings.NewReader(body))
		if header {
			req.Header.Set("X-Rushbin", "1")
		}
		r, err := c.Do(req)
		must(t, err)
		r.Body.Close()
		return r.StatusCode
	}

	var dups []Group
	get("/api/dups", &dups)
	if len(dups) != 1 || dups[0].Copies != 3 || !dups[0].Verified {
		t.Fatalf("dups = %+v, want one verified group of 3", dups)
	}
	var cmp Comparison
	get("/api/compare?a=A&b=B", &cmp)
	if cmp.Both.Clips != 1 || cmp.Verified != 1 || cmp.OnlyA.Clips != 1 || cmp.OnlyA.List[0].Name != "other.mp4" || cmp.OnlyB.Clips != 0 ||
		cmp.UniqueA.Clips != 1 || cmp.UniqueB.Clips != 0 {
		t.Fatalf("compare A and B = %+v", cmp)
	}
	// A drive's heavy work turned off shows at once (the catalog is reloaded before the answer).
	if code := post("/api/drives", `{"uuid":"B","action":"options","content":false}`, true); code != 200 {
		t.Fatalf("options: %d", code)
	}
	var ov Catalog
	get("/api/overview", &ov)
	if d := ov.device("B"); d.Content == nil || *d.Content || on(d.Copies) {
		t.Fatalf("drive B after options: content %v, copies %v", d.Content, d.Copies)
	}
	// A retired drive stays listed but no longer counts as a copy: what only A has now includes the clip that
	// B also had, and B lost nothing because A still has it. Marking it back undoes it.
	if code := post("/api/drives", `{"uuid":"B","action":"options","retired":true,"location":" Shelf 2 "}`, true); code != 200 {
		t.Fatalf("retire: %d", code)
	}
	get("/api/overview", &ov)
	if a, b := ov.device("A"), ov.device("B"); a.Unique != 2 || b.Retired == "" || b.Lost != 0 || b.Clips != 1 || b.Location != "Shelf 2" ||
		ov.Devices[len(ov.Devices)-1].UUID != "B" {
		t.Fatalf("after retiring B: A unique %d; B %+v; last drive %s", a.Unique, b, ov.Devices[len(ov.Devices)-1].UUID)
	}
	var retired []Row
	get("/api/clips", &retired)
	if i := slices.IndexFunc(retired, func(r Row) bool { return r.Name != "other.mp4" }); i < 0 || retired[i].Copies != 2 {
		t.Fatalf("clips with B retired: %+v", retired)
	}
	post("/api/drives", `{"uuid":"B","action":"options","retired":false}`, true)
	var back Catalog // a fresh one: an empty "retired" is left out of the JSON
	if get("/api/overview", &back); back.device("B").Retired != "" || back.device("A").Unique != 1 {
		t.Fatalf("B back: %+v, A unique %d", back.device("B"), back.device("A").Unique)
	}
	var rows []Row
	get("/api/clips", &rows)
	if len(rows) != 2 {
		t.Fatalf("unique clips = %d, want 2", len(rows))
	}
	other := rows[slices.IndexFunc(rows, func(r Row) bool { return r.Name == "other.mp4" })]

	if code := post("/api/decide", `{"content":"`+dups[0].Content+`","kind":"copy","value":"B|C/clip.mp4"}`, false); code != 403 {
		t.Fatalf("POST without header: %d, want 403", code)
	}
	if code := post("/api/decide", `{"content":"`+dups[0].Content+`","kind":"copy","value":"B|nowhere.mp4"}`, true); code != 400 {
		t.Fatalf("copy that does not exist: %d, want 400", code)
	}
	for _, d := range []string{
		`{"content":"` + dups[0].Content + `","kind":"copy","value":"B|C/clip.mp4"}`,
		`{"content":"` + other.Content + `","kind":"vote","value":"discard"}`,
	} {
		if code := post("/api/decide", d, true); code != 200 {
			t.Fatalf("decide %s: %d", d, code)
		}
	}
	var plan []Move
	get("/api/plan", &plan)
	var from []string
	for _, m := range plan {
		if !m.OK {
			t.Fatalf("move blocked: %+v", m)
		}
		from = append(from, relSlash(tmp, m.From))
	}
	slices.Sort(from)
	if want := []string{"A/P/clip.mp4", "A/P/copy.mp4", "A/P/other.mp4"}; !slices.Equal(from, want) {
		t.Fatalf("plan = %v, want %v", from, want)
	}
	if code := post("/api/apply", "", true); code != 200 {
		t.Fatalf("apply: %d", code)
	}
	for _, f := range from {
		if fileExists(filepath.Join(tmp, f)) || !fileExists(filepath.Join(tmp, f[:1], trash, f[2:])) {
			t.Errorf("%s was not moved to the trash", f)
		}
	}
	if !fileExists(filepath.Join(b, "C", "clip.mp4")) {
		t.Error("the copy to keep was moved")
	}
	// Decisions survive a restart: they are replayed from decisions.jsonl.
	must(t, app.reload())
	if d := app.catalog().decision(other.Content); d.Vote != "discard" {
		t.Errorf("after reload vote = %q, want discard", d.Vote)
	}
}

// TestCodec: the Mac encodes with VideoToolbox, the hardware encoder.
func TestCodec(t *testing.T) {
	if c := videoCodec(); runtime.GOOS == "darwin" && c[1] != "h264_videotoolbox" || len(c) < 2 {
		t.Fatalf("video codec %v", c)
	}
}

// TestConditions: the heavy work waits on battery; only the copies for the team wait on a metered network.
func TestConditions(t *testing.T) {
	a := &App{}
	a.readConditions(strings.NewReader("conditions battery=0 lowpower=0 hot=0 metered=1\nsomething else\n"))
	if a.pause(false) != "" || a.pause(true) != "on a metered connection" {
		t.Errorf("metered: work %q, copies %q", a.pause(false), a.pause(true))
	}
	a.readConditions(strings.NewReader("conditions battery=1 lowpower=0 hot=0 metered=0\n"))
	if a.pause(false) != "on battery" || a.pause(true) != "on battery" {
		t.Errorf("battery: work %q, copies %q", a.pause(false), a.pause(true))
	}
}

// TestChanged: a change on a cataloged drive queues one scan of it, which absorbs the changes after it until
// it starts; changes elsewhere, in the folders the inventory skips, or on a drive not cataloged queue nothing.
func TestChanged(t *testing.T) {
	cat, vols := t.TempDir(), t.TempDir()
	a, b, c := filepath.Join(vols, "A"), filepath.Join(vols, "B"), filepath.Join(vols, "C")
	must(t, os.MkdirAll(filepath.Join(cat, "disks", "A"), 0o755))
	must(t, os.WriteFile(filepath.Join(cat, "disks", "A", "files.jsonl"), nil, 0o644))
	app := &App{dir: cat, seen: map[string]string{a: "A", b: "", c: "C"}, queue: make(chan string, 64)}
	lines := func(paths ...string) {
		var s strings.Builder
		for _, p := range paths {
			s.WriteString("changed " + p + "\n")
		}
		app.readConditions(strings.NewReader(s.String()))
	}
	lines(filepath.Join(a, trash, "x.mov"), filepath.Join(a, ".Spotlight-V100", "x"), filepath.Join(b, "x.mov"),
		filepath.Join(c, "x.mov"), a+"B", filepath.Join(vols, "x"))
	if len(app.queued) != 0 {
		t.Fatalf("queued %v, want nothing", app.queued)
	}
	lines(filepath.Join(a, "Day 1", "clip one.mov"), a, filepath.Join(a, "Day 2"))
	if len(app.queued) != 1 || !app.queued[a] || <-app.queue != a {
		t.Fatalf("queued %v, want one scan of %s", app.queued, a)
	}
	app.catalog1(a) // starts (the folder is not there, so it ends at once): the next change queues again
	lines(filepath.Join(a, "Day 3"))
	if !app.queued[a] || <-app.queue != a {
		t.Fatalf("after the scan started: queued %v", app.queued)
	}
}

// TestDecisionMarkup: a vote from a team site is HTML in the page (a badge's class), so only the known ones
// are taken: a hostile or broken site cannot put markup there.
func TestDecisionMarkup(t *testing.T) {
	m := map[string]*Decision{}
	DecisionLine{Content: "c", Kind: "vote", Value: `x" onmouseover="alert(1)`}.apply(m)
	DecisionLine{Content: "d", Kind: "vote", Value: "keep"}.apply(m)
	if m["c"] != nil && m["c"].Vote != "" || m["d"] == nil || m["d"].Vote != "keep" {
		t.Fatalf("votes = %+v %+v", m["c"], m["d"])
	}
}
