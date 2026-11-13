package main

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestEnsureModels: a download counts its bytes for the status, and a failed one says why until the next try.
func TestEnsureModels(t *testing.T) {
	body := strings.Repeat("model", 1000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ok.bin" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(body))
	}))
	defer srv.Close()
	sum := sha256.Sum256([]byte(body))
	a := &App{dir: t.TempDir()}
	_, err := a.ensureModels([]model{{"gone.bin", srv.URL + "/gone.bin", "", 1}})
	if err == nil || a.dlErr != "gone.bin: 404 Not Found" {
		t.Fatalf("missing model: err %v, status %q", err, a.dlErr)
	}
	p, err := a.ensureModels([]model{{"ok.bin", srv.URL + "/ok.bin", hex.EncodeToString(sum[:]), int64(len(body))}})
	if err != nil || a.dlErr != "" || a.dlDone != int64(len(body)) || !fileExists(p[0]) {
		t.Fatalf("download: err %v, status %q, %d bytes", err, a.dlErr, a.dlDone)
	}

	// A download cut halfway goes on from where it stopped: the second try asks only for the rest.
	var ranges []string
	cut := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ranges = append(ranges, r.Header.Get("Range"))
		if len(ranges) == 1 {
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			w.Write([]byte(body[:2000]))
			w.(http.Flusher).Flush()
			panic(http.ErrAbortHandler) // the connection drops
		}
		http.ServeContent(w, r, "big.bin", time.Time{}, strings.NewReader(body))
	}))
	defer cut.Close()
	big := model{"big.bin", cut.URL + "/big.bin", hex.EncodeToString(sum[:]), int64(len(body))}
	if _, err := a.ensureModels([]model{big}); err == nil {
		t.Fatal("a cut download looked complete")
	}
	if p, err := a.ensureModels([]model{big}); err != nil || !fileExists(p[0]) || len(ranges) != 2 || ranges[1] != "bytes=2000-" {
		t.Fatalf("resume: err %v, ranges %q", err, ranges)
	}
}

// TestSpeech: what is said is searchable, and the result points at the sentence and its time.
// With whisper-cli and the models (RUSHBIN_SPEECH_MODELS), it also transcribes a real clip, with what it must
// hear (RUSHBIN_SPEECH_SAYS, optional): on a Mac large-v3-turbo on Italian speech, in CI tiny on whisper.cpp's
// samples/jfk.wav.
func TestSpeech(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "speech.jsonl")
	must(t, os.WriteFile(file, []byte(`{"thumb":"a","segs":[{"s":1.5,"e":4,"t":"Benvenuti alla cantata dei pastori"},{"s":4,"e":9,"t":"Peppe Barra racconta"}]}
{"thumb":"b","segs":[]}
`), 0o644))
	m, err := loadSpeech(file)
	must(t, err)
	c := &Catalog{byCont: map[string][]*Clip{}, speech: m}
	c.list = []*Clip{{ID: "P/one.mov", Thumb: "a", Content: "c:a"}, {ID: "P/two.mov", Thumb: "b", Content: "c:b"}}
	rows := c.Clips(Query{Words: []string{"Barra"}, All: true})
	if len(rows) != 1 || rows[0].Said == nil || rows[0].Said.S != 4 || rows[0].Said.T != "Peppe Barra racconta" {
		t.Fatalf("search in what is said = %+v", rows)
	}
	if rows := c.Clips(Query{Words: []string{"tamburo"}, All: true}); len(rows) != 0 {
		t.Fatalf("a word nobody says found %d clips", len(rows))
	}

	models := filepath.SplitList(os.Getenv("RUSHBIN_SPEECH_MODELS")) // the Whisper model, then ggml-silero-v5.1.2.bin
	audio := os.Getenv("RUSHBIN_SPEECH_SAMPLE")                      // a clip with speech
	if len(models) != 2 || audio == "" {
		t.Skip("RUSHBIN_SPEECH_MODELS and RUSHBIN_SPEECH_SAMPLE not set")
	}
	segs, err := transcribe(tool("whisper-cli"), models, &Clip{Path: audio})
	must(t, err)
	if len(segs) == 0 || segs[0].E <= segs[0].S {
		t.Fatalf("transcript = %+v", segs)
	}
	t.Logf("%d sentences, first at %.1f s: %s", len(segs), segs[0].S, segs[0].T)
	if w := strings.ToLower(os.Getenv("RUSHBIN_SPEECH_SAYS")); newSpeech(segs).said(w, nil) == nil {
		t.Fatalf("%q not heard in %+v", w, segs)
	}
}

// TestKeepVoiced: a segment stays only where the sound classifier heard a voice, and scores that do not match
// the segments are an error, not a silent clip.
func TestKeepVoiced(t *testing.T) {
	segs := []Segment{{0, 1, "Thank you."}, {2, 5, "This blade has a dark past."}, {6, 7, "Come be my shadow"}}
	kept, err := keepVoiced(segs, []float64{0.0392, 0.9732, minVoice}) // BBB_A_004, Sintel A001_C006, at the line
	if err != nil || len(kept) != 2 || kept[0].T != segs[1].T || kept[1].T != segs[2].T {
		t.Fatalf("kept %v, %v", kept, err)
	}
	if _, err := keepVoiced(segs, []float64{1}); err == nil {
		t.Fatal("1 score for 3 segments accepted")
	}
}
