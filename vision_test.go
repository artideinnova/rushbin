package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestLook covers what rushbin does with Apple Vision's output: the summary of the frames, the suggestions,
// the filters and search, and the replay of vision.jsonl.
func TestLook(t *testing.T) {
	l := summarize([]Frame{
		{Contrast: 0.2, Labels: map[string]float64{"people": 0.9, "sky": 0.95}, Text: []string{" Con buddy "}},
		{Contrast: 0.005, Labels: map[string]float64{"people": 0.7, "child": 0.95, "tattoo": 0.9}, Text: []string{"con BUDDY"}},
	})
	if l.Blank || !slices.Equal(l.Labels, []string{"sky", "people"}) || !slices.Equal(l.Text, []string{"Con buddy"}) || !l.AI || l.Generator != lookGenerator {
		t.Fatalf("summarize = %+v", l)
	}

	c := &Catalog{}
	add := func(id string, dur float64, look *Look) {
		c.list = append(c.list, &Clip{ID: id, Dur: dur, Content: "c:" + id, Look: look})
	}
	add("blank", 8, summarize([]Frame{{Contrast: 0}, {Contrast: 0.009}}))
	add("short", 1.5, summarize([]Frame{{Contrast: 0.3}}))
	add("buddy", 5, l)
	add("good", 5, summarize([]Frame{{Contrast: 0.3}}))
	add("unseen", 5, nil)
	add("title", 8, summarize([]Frame{{Contrast: 0}})) // a graphic, transparent where the previews were taken
	c.list[len(c.list)-1].Alpha = alpha("yuva444p12le")
	c.suggest()
	var got []string
	for _, cl := range c.list {
		if cl.Look.suggestion() == "discard" {
			got = append(got, cl.ID)
		}
	}
	if !slices.Equal(got, []string{"blank", "short"}) {
		t.Fatalf("likely discards = %v", got)
	}
	if w := c.list[0].Look.Why; !slices.Equal(w, []string{"blank image"}) {
		t.Errorf("why = %v", w)
	}
	ids := func(q Query) (r []string) {
		for _, cl := range c.list {
			if c.match(cl, q) {
				r = append(r, cl.ID)
			}
		}
		return
	}
	if r := ids(Query{Flags: []string{"suggest-discard"}}); !slices.Equal(r, []string{"blank", "short"}) {
		t.Errorf("likely discards = %v", r)
	}
	if r := ids(Query{Label: "sky"}); !slices.Equal(r, []string{"buddy"}) {
		t.Errorf("label sky = %v", r)
	}
	if r := ids(Query{Words: []string{"BUDDY"}}); !slices.Equal(r, []string{"buddy"}) { // the name and the text on screen
		t.Errorf("search buddy = %v", r)
	}

	// vision.jsonl: the last line of a content wins, a line cut by a crash ends the replay.
	file := filepath.Join(t.TempDir(), "vision.jsonl")
	must(t, os.WriteFile(file, []byte(`{"thumb":"a","frames":[{"contrast":0}]}
{"thumb":"a","frames":[{"contrast":0.3,"labels":{"sky":0.9}}]}
{"thumb":"b","frames":[]}
{"thumb":"c","fra`), 0o644))
	m, err := loadLooks(file)
	must(t, err)
	if len(m) != 2 || m["a"].Blank || !slices.Equal(m["a"].Labels, []string{"sky"}) || m["b"] != nil {
		t.Fatalf("loadLooks = %v", m)
	}
}

// TestVisionHelper runs this system's vision helper (RUSHBIN_VISION; CI sets it on Windows, where the model
// is downloaded as in the app) on neutral frames (testdata/vision, made by make.sh with the Mac app's helper),
// in a folder with an accented name. Its embeddings must match the Mac's: cosine mean >= 0.98 and min >= 0.93,
// what the ONNX export reached on 200 real previews. Blank frames must be blank, the text on screen read.
func TestVisionHelper(t *testing.T) {
	vis := os.Getenv("RUSHBIN_VISION")
	if vis == "" {
		t.Skip("RUSHBIN_VISION not set")
	}
	a := &App{dir: t.TempDir()}
	args := a.siglip()
	if args == nil {
		t.Fatal("no SigLIP model")
	}
	refs, _ := filepath.Glob(filepath.Join("testdata", "vision", "*", "emb.bin"))
	root := filepath.Join(t.TempDir(), "anteprime è")
	var lines []string
	for _, ref := range refs {
		dir := filepath.Join(root, filepath.Base(filepath.Dir(ref)))
		must(t, os.MkdirAll(dir, 0o755))
		jpgs, _ := filepath.Glob(filepath.Join(filepath.Dir(ref), "*.jpg"))
		for _, j := range jpgs {
			b, err := os.ReadFile(j)
			must(t, err)
			must(t, os.WriteFile(filepath.Join(dir, filepath.Base(j)), b, 0o644))
		}
		lines = append(lines, dir+"\tboth")
	}
	cmd := background(vis, args...)
	cmd.Stdin = strings.NewReader(strings.Join(lines, "\n") + "\n")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	t.Log(strings.TrimSpace(stderr.String()))
	must(t, err)

	parse := func(b []byte) map[string][]Frame { // the frames of each folder, by its name
		m := map[string][]Frame{}
		for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			var v struct {
				Dir    string
				Frames []Frame
			}
			must(t, json.Unmarshal([]byte(l), &v))
			m[filepath.Base(v.Dir)] = v.Frames
		}
		return m
	}
	b, err := os.ReadFile(filepath.Join("testdata", "vision", "mac.jsonl"))
	must(t, err)
	looks, mac := parse(out), parse(b)

	var sum float64
	n, low := 0, 1.0
	for _, ref := range refs {
		name := filepath.Base(filepath.Dir(ref))
		want, err := os.ReadFile(ref)
		must(t, err)
		got, err := os.ReadFile(filepath.Join(root, name, "emb.bin"))
		must(t, err)
		w, err := frames(want)
		must(t, err)
		g, err := frames(got)
		must(t, err)
		if len(g) != len(w) || len(looks[name]) != len(mac[name]) {
			t.Fatalf("%s: %d embeddings and %d looks, the Mac %d and %d", name, len(g), len(looks[name]), len(w), len(mac[name]))
		}
		var row []string
		for i := range w {
			var dot, ng, nw float64
			for j := range w[i] {
				dot += float64(g[i][j]) * float64(w[i][j])
				ng += float64(g[i][j]) * float64(g[i][j])
				nw += float64(w[i][j]) * float64(w[i][j])
			}
			c := dot / math.Sqrt(ng*nw)
			sum, n, low = sum+c, n+1, min(low, c)
			f, m := looks[name][i], mac[name][i]
			row = append(row, fmt.Sprintf("%.4f (contrast %.3f, Mac %.3f)", c, f.Contrast, m.Contrast))
			if (f.Contrast < blankContrast) != (m.Contrast < blankContrast) {
				t.Errorf("%s/%d.jpg: contrast %.3f, the Mac %.3f: not the same blank", name, i+1, f.Contrast, m.Contrast)
			}
		}
		t.Logf("%s: %s; text %q", name, strings.Join(row, " "), summarize(looks[name]).Text)
	}
	mean := sum / float64(n)
	t.Logf("SigLIP 2 against the Mac, %d frames: cosine mean %.4f, min %.4f", n, mean, low)
	if mean < 0.98 || low < 0.93 {
		t.Errorf("cosine mean %.4f (needs 0.98), min %.4f (needs 0.93)", mean, low)
	}
	if l := summarize(looks["text"]); !l.mentions("search by content") || !l.mentions("rushbin 2026") {
		t.Errorf("text on screen = %q", l.Text)
	}
}

// A drive's progress counts a clip without previews as looked at (nothing to look at), so it can reach Ready.
func TestContentDone(t *testing.T) {
	thumbs := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(thumbs, "b"), 0o755))
	must(t, os.WriteFile(filepath.Join(thumbs, "b", "1.jpg"), []byte{0xff}, 0o644))
	c := &Catalog{looks: map[string]*Look{"a": {}}, speech: map[string]*Speech{"a": {}}}
	c.list = []*Clip{{Device: "D", Thumb: "a"}, {Device: "D", Thumb: "b"}, {Device: "D", Thumb: "c"}} // looked at, waiting, no previews
	if got := c.contentDone(thumbs)["D"]; got != [2]int{2, 1} {
		t.Fatalf("contentDone = %v, want looked at 2 (one has no previews), listened to 1", got)
	}
}
