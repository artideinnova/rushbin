package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestTokenizer: the token ids of SigLIP 2's tokenizer.model (RUSHBIN_SIGLIP_TOKENIZER) are the ones of the
// site's transformers 5.22.1 (deploy/text) on English and Italian searches with accents, punctuation, runs of
// spaces, tabs, emoji, other scripts, tags, and searches cut at 64 tokens and at 300 characters.
func TestTokenizer(t *testing.T) {
	file := os.Getenv("RUSHBIN_SIGLIP_TOKENIZER")
	if file == "" {
		t.Skip("RUSHBIN_SIGLIP_TOKENIZER not set")
	}
	tok, err := loadTokenizer(file)
	must(t, err)
	b, err := os.ReadFile(filepath.Join("testdata", "siglip-text.json"))
	must(t, err)
	var cases []struct {
		Q   string
		IDs []int32
	}
	must(t, json.Unmarshal(b, &cases))
	for _, c := range cases {
		want := append(c.IDs, make([]int32, 64-len(c.IDs))...) // padded with <pad>, 0
		if got := tok.ids(c.Q); !slices.Equal(got, want) {
			t.Errorf("%q:\n got %v\nwant %v", c.Q, got, want)
		}
	}
}

// TestSearchAlone: a Mac in no team searches by looks itself (a fake text encoder here) and merges them with
// the words like the site: the clip found both ways first, the one only seen after it.
func TestSearchAlone(t *testing.T) {
	tmp := t.TempDir()
	vol, cat := filepath.Join(tmp, "A"), filepath.Join(tmp, "catalog")
	must(t, os.MkdirAll(vol, 0o755))
	ffmpeg(t, "testsrc=duration=2:size=320x240:rate=25", filepath.Join(vol, "clip.mp4"))
	ffmpeg(t, "testsrc2=duration=2:size=320x240:rate=25", filepath.Join(vol, "other.mp4"))
	scanned(t, cat, vol, "A", "", nil)
	vec := make([]float32, embDim)
	for i := range vec {
		vec[i] = float32((i*7)%13-6) / 40
	}
	app := &App{dir: cat, text: func(string) ([]float32, error) { return vec, nil }}
	must(t, app.reload())
	for _, cl := range app.catalog().list { // other.mp4 looks like the search, clip.mp4 hardly
		var emb []byte
		for k := 0; k < 5; k++ {
			for i, x := range vec {
				if cl.Name == "clip.mp4" && i%2 == 0 {
					x = -x
				}
				emb = binary.LittleEndian.AppendUint16(emb, toHalf(x))
			}
		}
		must(t, os.MkdirAll(filepath.Join(cat, "thumbs", cl.Thumb), 0o755))
		must(t, os.WriteFile(filepath.Join(cat, "thumbs", cl.Thumb, "emb.bin"), emb, 0o644))
	}

	var rows []Row
	rec := httptest.NewRecorder()
	app.search(rec, httptest.NewRequest("GET", "/api/search?q=other", nil))
	must(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	if len(rows) != 2 || rows[0].Name != "other.mp4" || !rows[0].Seen || rows[1].Name != "clip.mp4" || !rows[1].Seen {
		t.Fatalf("search alone = %+v, want other.mp4 (words and looks), then clip.mp4 (looks)", rows)
	}
}

// A model that is a folder comes as a zip: unpacked next to it, the zip removed, and then it is here.
func TestModelZip(t *testing.T) {
	var buf bytes.Buffer
	z := zip.NewWriter(&buf)
	w, _ := z.Create("M.mlmodelc/weights/weight.bin")
	w.Write([]byte("weights"))
	must(t, z.Close())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(buf.Bytes()) }))
	defer srv.Close()
	sum := sha256.Sum256(buf.Bytes())
	a := &App{dir: t.TempDir()}
	m := model{"M.mlmodelc.zip", srv.URL, hex.EncodeToString(sum[:]), int64(buf.Len())}
	p, err := a.ensureModels([]model{m})
	must(t, err)
	dir := filepath.Join(a.dir, "models")
	if b, err := os.ReadFile(filepath.Join(p[0], "weights", "weight.bin")); err != nil || string(b) != "weights" ||
		fileExists(filepath.Join(dir, m.name)) || !m.here(dir) {
		t.Fatalf("unpacked %v: %q %v", p, b, err)
	}

	var bad bytes.Buffer // an archive that writes outside its folder is refused
	z = zip.NewWriter(&bad)
	z.Create("M.mlmodelc/../../evil")
	must(t, z.Close())
	must(t, os.WriteFile(filepath.Join(dir, "bad.zip"), bad.Bytes(), 0o644))
	if err := unzip(filepath.Join(dir, "bad.zip"), filepath.Join(dir, "M2.mlmodelc")); err == nil {
		t.Fatal("an archive escaping its folder was unpacked")
	}
}
