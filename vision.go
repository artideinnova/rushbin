package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// Frame is what Apple Vision saw in one preview frame (macos/vision.swift).
type Frame struct {
	Contrast float64            `json:"contrast"`
	Labels   map[string]float64 `json:"labels,omitempty"`
	Text     []string           `json:"text,omitempty"`
}

// Look sums up the preview frames of one content, with the suggestion rushbin draws from them. Generator and AI
// mark the labels and the text, wherever they are sent, as made by AI models (docs/ai.md).
type Look struct {
	Blank     bool     `json:"blank,omitempty"`   // every frame is one flat color
	Labels    []string `json:"labels,omitempty"`  // most confident first
	Text      []string `json:"text,omitempty"`    // lines read on screen, once each
	Suggest   string   `json:"suggest,omitempty"` // discard, or ""
	Why       []string `json:"why,omitempty"`     // reasons for a discard
	Generator string   `json:"generator"`
	AI        bool     `json:"ai_generated"`
}

// lookGenerator: the helper that made the frames, Apple Vision on the Mac, Windows OCR on Windows (docs/ai.md).
const lookGenerator = "rushbin-vision"

// personLabels are Apple's classes that put people into categories by age or by a body feature, the kind of
// categories that make biometric categorisation (AI Act, art. 3(40) and recital 16). rushbin labels footage to find
// it; it puts no person into such a category, so they are dropped, also from what older versions stored.
var personLabels = map[string]bool{"adult": true, "baby": true, "child": true, "teen": true, "tattoo": true}

// withoutPersonLabels drops personLabels from frames, before the site stores them.
func withoutPersonLabels(fr []Frame) []Frame {
	for _, f := range fr {
		for k := range f.Labels {
			if personLabels[k] {
				delete(f.Labels, k)
			}
		}
	}
	return fr
}

// Suggestions, set by looking at the T9 on 5 November 2026. Every frame below 0.01 contrast means a blank
// image; on the T9 the next clip is at 0.024. Clips with transparency are left out: there the blank frames
// were titles and mattes. Sharpness, exposure and Apple's "utility" flag were left out too: on its flat
// (log) footage they flagged more good clips than bad. Suggestions for the showreel, from Apple's aesthetic
// score, were removed on 5 November 2026: on the T9 they came out at random.
// ponytail: fixed numbers; make them settings if other archives need different ones.
const (
	blankContrast = 0.01
	minDur        = 2.0 // seconds: shorter clips are usually started by mistake
)

// summarize turns the frames of one content into a Look (no suggestion yet: it needs the whole catalog).
func summarize(fr []Frame) *Look {
	if len(fr) == 0 {
		return nil
	}
	l := &Look{Blank: true, Generator: lookGenerator, AI: true}
	conf := map[string]float64{}
	seen := map[string]bool{}
	for _, f := range fr {
		l.Blank = l.Blank && f.Contrast < blankContrast
		for k, v := range f.Labels {
			if !personLabels[k] {
				conf[k] = max(conf[k], v)
			}
		}
		for _, t := range f.Text {
			if t = strings.TrimSpace(t); !seen[strings.ToLower(t)] {
				seen[strings.ToLower(t)] = true
				l.Text = append(l.Text, t)
			}
		}
	}
	for k := range conf {
		l.Labels = append(l.Labels, k)
	}
	sort.Slice(l.Labels, func(i, j int) bool {
		a, b := l.Labels[i], l.Labels[j]
		return conf[a] > conf[b] || conf[a] == conf[b] && a < b
	})
	return l
}

// suggest marks likely discards, with the reasons, once per content.
func (c *Catalog) suggest() {
	done := map[*Look]bool{}
	for _, cl := range c.list {
		l := cl.Look
		if l == nil || done[l] {
			continue
		}
		done[l] = true
		l.Suggest, l.Why = "", nil
		if l.Blank && !cl.Alpha { // a graphic can be transparent where the preview was taken
			l.Why = append(l.Why, "blank image")
		}
		if cl.Dur < minDur {
			l.Why = append(l.Why, "shorter than 2 seconds")
		}
		if len(l.Why) > 0 {
			l.Suggest = "discard"
		}
	}
}

// suggestion is "discard" or "" (also for a clip not analyzed yet).
func (l *Look) suggestion() string {
	if l == nil {
		return ""
	}
	return l.Suggest
}

// mentions: a search word found in the labels or in the text on screen.
func (l *Look) mentions(w string) bool {
	if l == nil {
		return false
	}
	for _, s := range append(append([]string{}, l.Labels...), l.Text...) {
		if strings.Contains(strings.ToLower(s), w) {
			return true
		}
	}
	return false
}

// loadLooks replays vision.jsonl: one line per content, the last one wins.
func loadLooks(file string) (map[string]*Look, error) {
	m := map[string]*Look{}
	f, err := os.Open(file)
	if errors.Is(err, fs.ErrNotExist) {
		return m, nil
	} else if err != nil {
		return nil, err
	}
	defer f.Close()
	dec := json.NewDecoder(bufio.NewReader(f))
	for {
		var v struct {
			Thumb  string
			Frames []Frame
		}
		if dec.Decode(&v) != nil {
			break // end of file, or a line cut by a crash
		}
		m[v.Thumb] = summarize(v.Frames)
	}
	return m, nil
}

// siglipModel is the image half of SigLIP 2 for Windows (ONNX, Apache-2.0), pinned to a commit. Float32: the
// float16 file fails to load on the CPU with onnxruntime 1.24.4, the last with DirectML (a fusion of its graph
// optimizer looks for a cast it inserted), and gives the same embeddings (testdata/vision).
var siglipModel = model{"siglip2-base-patch16-256-vision.onnx",
	"https://huggingface.co/onnx-community/siglip2-base-patch16-256-ONNX/resolve/d1114256522a37ffa257a0a58017348ab0058db2/onnx/vision_model.onnx",
	"f5cb16728a704703f05516ded628397e11dbca4de2eb5db04b0c0bcee988aa7a", 371992072}

// siglip gives the helper its SigLIP 2 image model: on the Mac the Core ML package bundled in the app
// (Resources) and where to compile it for this Mac on first use; on Windows the ONNX file, downloaded once.
// nil without a model (a development build, or the download failed).
func (a *App) siglip() []string {
	if runtime.GOOS == "windows" {
		p, err := a.ensureModels([]model{siglipModel})
		if err != nil {
			log.Println("SigLIP model:", err)
		}
		return p
	}
	if pkg := resource("SigLIP2Image.mlpackage"); pkg != "" {
		return []string{pkg, filepath.Join(a.dir, "models", "SigLIP2Image.mlmodelc")}
	}
	return nil
}

// analyze has the vision helper (Apple Vision on the Mac, windows/vision on Windows) look at the previews not
// seen yet. It needs only the previews, so it also runs
// for drives that are not connected.
func (a *App) analyze() bool {
	vis := tool("rushbin-vision")
	if _, err := exec.LookPath(vis); err != nil {
		return false // development build without the Swift tool
	}
	c := a.catalog()
	var args []string // the SigLIP 2 model, asked for only when a preview needs it: on Windows it is downloaded
	asked := false
	siglip := func() bool {
		if !asked {
			asked, args = true, a.siglip()
		}
		return args != nil
	}
	var dirs, lines []string // dirs: the ones that need Apple Vision, and so a line of vision.jsonl back
	seen := map[string]bool{}
	for _, cl := range c.list {
		dir := filepath.Join(a.dir, "thumbs", cl.Thumb)
		if seen[cl.Thumb] || !on(c.device(cl.Device).Content) || !fileExists(filepath.Join(dir, "1.jpg")) {
			continue
		}
		seen[cl.Thumb] = true
		_, looked := c.looks[cl.Thumb]
		embed := !fileExists(filepath.Join(dir, "emb.bin")) && siglip()
		switch {
		case !looked && embed:
			lines, dirs = append(lines, dir+"\tboth"), append(dirs, dir)
		case !looked:
			lines, dirs = append(lines, dir+"\tlook"), append(dirs, dir)
		case embed:
			lines = append(lines, dir+"\tembed")
		}
	}
	if len(lines) == 0 {
		return false
	}
	out, err := os.OpenFile(filepath.Join(a.dir, "vision.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		log.Println("vision:", err)
		return false
	}
	defer out.Close()
	if len(dirs) > 0 { // lines come back only for these; the embeddings are files
		a.setStatus("Looking at the footage: 0 of %d", len(dirs))
		a.setProgress(4, 0, int64(len(dirs)))
	} else {
		a.setStatus("Preparing search by content: %d clips", len(lines))
	}
	cmd := background(vis, args...)
	cmd.Stdin = strings.NewReader(strings.Join(lines, "\n") + "\n")
	cmd.Stderr = log.Writer() // on Windows: where SigLIP runs (DirectML or the CPU) and the OCR language
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		log.Println("vision:", err)
		return false
	}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1<<16), 1<<24)
	n := 0
	for sc.Scan() {
		var v struct {
			Dir    string          `json:"dir"`
			Frames json.RawMessage `json:"frames"`
		}
		if json.Unmarshal(sc.Bytes(), &v) != nil {
			continue
		}
		line, _ := json.Marshal(map[string]any{"thumb": filepath.Base(v.Dir), "frames": v.Frames})
		out.Write(append(line, '\n'))
		n++
		a.setStatus("Looking at the footage: %d of %d", n, len(dirs))
		a.setProgress(4, int64(n), int64(len(dirs)))
	}
	if err := cmd.Wait(); err != nil {
		log.Println("vision:", err)
	}
	// Done something if a frame was looked at or embedded: a helper that keeps failing is not run in a loop.
	did := n > 0
	for _, l := range lines {
		if dir, ok := strings.CutSuffix(l, "\tembed"); ok && fileExists(filepath.Join(dir, "emb.bin")) {
			did = true
			break
		}
	}
	return did
}
