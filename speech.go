package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// What is said in the clips, transcribed on the Mac with Whisper (whisper.cpp). The audio is first evened out
// (quiet voices off the microphone come up) and then cut to the parts with a voice (Silero VAD), so music and
// crowd noise give no invented sentences; what Whisper still writes on music is dropped where Apple's sound
// classifier hears no voice (voiced). Measured on the T9 on 5 November 2026: 10 minutes of interview,
// 952 words, the interviewer's question included; 7 s of party noise, nothing.

// Segment is one sentence and where it is in the clip, in seconds.
type Segment struct {
	S float64 `json:"s"`
	E float64 `json:"e"`
	T string  `json:"t"`
}

// Speech is everything said in one content. Generator and AI mark it, wherever it is sent, as made by an AI
// model (docs/ai.md): Whisper may write words nobody said.
type Speech struct {
	Segs      []Segment `json:"segs"`
	Generator string    `json:"generator"`
	AI        bool      `json:"ai_generated"`
	text      string    // lowercase, for search
}

const speechGenerator = "whisper-large-v3-turbo" // the model of speechModels; whisper.cpp runs it

func newSpeech(segs []Segment) *Speech {
	var b strings.Builder
	for _, s := range segs {
		b.WriteString(strings.ToLower(s.T))
		b.WriteByte(' ')
	}
	return &Speech{Segs: segs, Generator: speechGenerator, AI: true, text: b.String()}
}

// said finds the first sentence holding w (lowercase) that is not hidden (its start in hidden), for the result
// to show and the player to start at.
func (s *Speech) said(w string, hidden []float64) *Segment {
	if s == nil || !strings.Contains(s.text, w) {
		return nil
	}
	for i := range s.Segs {
		if strings.Contains(strings.ToLower(s.Segs[i].T), w) && !slices.Contains(hidden, s.Segs[i].S) {
			return &s.Segs[i]
		}
	}
	return nil
}

// said: the first line of cl that holds w (lowercase) and that no member hid as wrong.
func (c *Catalog) said(cl *Clip, w string) *Segment {
	s := c.speechOf(cl.Thumb)
	if s == nil || !strings.Contains(s.text, w) {
		return nil
	}
	return s.said(w, c.decision(cl.Content).Hidden)
}

// model is a file downloaded once from Hugging Face into the catalog's models folder and checked against the
// SHA-256 published there. A .zip is a model folder: unpacked next to it, then removed.
type model struct {
	name, url, sha string
	size           int64
}

// path is where the model is once here: the file, or the folder unpacked from the zip.
func (m model) path(dir string) string { return filepath.Join(dir, strings.TrimSuffix(m.name, ".zip")) }

// here: downloaded and complete (a file of its size, or its unpacked folder).
func (m model) here(dir string) bool {
	st, err := os.Stat(m.path(dir))
	return err == nil && (st.IsDir() || st.Size() == m.size)
}

var speechModels = []model{
	{"ggml-large-v3-turbo.bin", "https://huggingface.co/ggerganov/whisper.cpp/resolve/main/ggml-large-v3-turbo.bin",
		"1fc70f774d38eb169993ac391eea357ef47c88757ef72ee5943879b7e8e2bc69", 1624555275},
	{"ggml-silero-v5.1.2.bin", "https://huggingface.co/ggml-org/whisper-vad/resolve/main/ggml-silero-v5.1.2.bin",
		"29940d98d42b91fbd05ce489f3ecf7c72f0a42f027e4875919a28fb4c04ea2cf", 885098},
}

// ensureModels returns the paths of the models, downloading and checking what is missing. The download shows
// in the status (bytes done of what is missing); a failure stays there until the next try.
func (a *App) ensureModels(ms []model) (paths []string, err error) {
	defer func() {
		if err != nil {
			msg := err.Error()
			if u := (*url.Error)(nil); errors.As(err, &u) {
				msg = u.Err.Error() // without the long address
			}
			a.smu.Lock()
			a.dlErr = msg
			a.smu.Unlock()
		}
	}()
	dir := filepath.Join(a.dir, "models")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	for _, m := range ms {
		p := filepath.Join(dir, m.name)
		paths = append(paths, m.path(dir))
		if m.here(dir) {
			continue // checked when it was downloaded
		}
		if st, err := os.Stat(p); err != nil || st.Size() != m.size { // else a zip downloaded, not unpacked yet
			if err := a.fetch(m, p); err != nil {
				return nil, err
			}
		}
		if m.path(dir) != p {
			if err := unzip(p, m.path(dir)); err != nil {
				return nil, err
			}
			os.Remove(p)
		}
	}
	return paths, nil
}

// fetch downloads model m into p and checks it. What an earlier try left in .part is kept: the download goes
// on from there (Range), not from zero.
func (a *App) fetch(m model, p string) error {
	tmp := p + ".part"
	f, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	h := sha256.New()
	have, err := io.Copy(h, f)
	if err == nil && have < m.size {
		err = a.download(m, f, h, have)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if st, _ := os.Stat(tmp); st == nil || st.Size() != m.size || hex.EncodeToString(h.Sum(nil)) != m.sha {
		os.Remove(tmp) // a wrong file: the next try starts again from zero
		return fmt.Errorf("%s: wrong size or checksum", m.name)
	}
	return os.Rename(tmp, p)
}

// download appends the rest of model m to f from byte have, hashing it into h; a server that answers with the
// whole file instead restarts it.
func (a *App) download(m model, f *os.File, h interface {
	io.Writer
	Reset()
}, have int64) error {
	req, err := http.NewRequest("GET", m.url, nil)
	if err != nil {
		return err
	}
	if have > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", have))
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	switch {
	case res.StatusCode == http.StatusOK && have > 0:
		have = 0
		h.Reset()
		if err := f.Truncate(0); err != nil {
			return err
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
	case res.StatusCode != http.StatusOK && res.StatusCode != http.StatusPartialContent:
		return fmt.Errorf("%s: %s", m.name, res.Status)
	}
	a.smu.Lock()
	a.dlDone, a.dlErr = have, ""
	a.smu.Unlock()
	_, err = io.Copy(io.MultiWriter(f, h, downloaded{a}), io.LimitReader(res.Body, m.size-have))
	return err
}

// downloaded counts the bytes of the model being downloaded, for the status.
type downloaded struct{ a *App }

func (d downloaded) Write(b []byte) (int, error) {
	d.a.smu.Lock()
	d.a.dlDone += int64(len(b))
	d.a.smu.Unlock()
	return len(b), nil
}

// modelsNeeded is what search by content still has to download on this computer, in bytes: the speech models
// where Whisper is (the Mac app), the text model on a Mac in no team, the image model on Windows.
func (a *App) modelsNeeded() (n int64) {
	var ms []model
	if _, err := exec.LookPath(tool("whisper-cli")); err == nil {
		ms = speechModels
	}
	if a.wantText() {
		ms = append(ms[:len(ms):len(ms)], siglipText...)
	}
	if runtime.GOOS == "windows" {
		ms = append(ms[:len(ms):len(ms)], siglipModel)
	}
	for _, m := range ms {
		if !m.here(filepath.Join(a.dir, "models")) {
			n += m.size
		}
	}
	return n
}

// speechNext transcribes one content with a connected copy that has no transcript yet.
// It returns false when there is nothing to do (no whisper-cli, all done, or the model cannot be had).
func (a *App) speechNext() bool {
	whisper := tool("whisper-cli")
	if _, err := exec.LookPath(whisper); err != nil {
		return false // development build without it
	}
	c := a.catalog()
	var todo []*Clip
	seen := map[string]bool{}
	for _, cl := range c.list {
		if _, ok := c.speech[cl.Thumb]; !ok && !seen[cl.Thumb] && on(c.device(cl.Device).Content) && fileExists(cl.Path) {
			seen[cl.Thumb] = true
			todo = append(todo, cl)
		}
	}
	if len(todo) == 0 {
		return false
	}
	models, err := a.ensureModels(speechModels)
	if err != nil {
		log.Println("speech model:", err)
		return false
	}
	sort.Slice(todo, func(i, j int) bool { return todo[i].Project+todo[i].ID < todo[j].Project+todo[j].ID })
	cl := todo[0]
	a.setBackground(fmt.Sprintf("Listening to the clips: %d to go", len(todo)))
	segs, err := transcribe(whisper, models, cl)
	if err != nil {
		log.Printf("speech %s: %v", cl.Path, err)
		segs = []Segment{} // recorded as silent, so it is not tried again on every round
	}
	line, _ := json.Marshal(map[string]any{"thumb": cl.Thumb, "segs": segs})
	f, err := os.OpenFile(filepath.Join(a.dir, "speech.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		log.Println("speech:", err)
		return false
	}
	f.Write(append(line, '\n'))
	f.Close()
	c.setSpeech(cl.Thumb, newSpeech(segs))
	return true
}

// transcribe: the first audio track, evened out, at 16 kHz mono, through Whisper with the voice detector.
// A file without audio gives no segments and no error.
func transcribe(whisper string, models []string, cl *Clip) ([]Segment, error) {
	cache, _ := os.UserCacheDir()
	dir := filepath.Join(cache, "rushbin", "speech")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	wav := filepath.Join(dir, "audio.wav")
	defer os.Remove(wav)
	in, feed := input(cl, 0, false, background)
	ff := background(tool("ffmpeg"), append(append([]string{"-nostdin", "-loglevel", "error", "-y"}, in...),
		"-af", "dynaudnorm", "-ac", "1", "-ar", "16000", "-f", "wav", wav)...)
	done, err := feed(ff)
	if err != nil {
		return nil, err
	}
	out, err := ff.CombinedOutput()
	done()
	if err != nil {
		return nil, fmt.Errorf("ffmpeg: %v: %s", err, strings.TrimSpace(string(out)))
	}
	if st, err := os.Stat(wav); err != nil || st.Size() <= 44 { // a WAV header and no samples: no audio track
		return []Segment{}, nil
	}
	base := filepath.Join(dir, "result")
	defer os.Remove(base + ".json")
	out, err = background(whisper, "-m", models[0], "--vad", "-vm", models[1], "-l", "auto", "-t", "4",
		"-f", wav, "-oj", "-of", base, "-np").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("whisper: %v: %s", err, lastLine(string(out)))
	}
	var r struct {
		Transcription []struct {
			Offsets struct{ From, To float64 }
			Text    string
		}
	}
	b, err := os.ReadFile(base + ".json")
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	segs := []Segment{}
	for _, t := range r.Transcription {
		if s := strings.TrimSpace(t.Text); s != "" {
			segs = append(segs, Segment{S: t.Offsets.From / 1000, E: t.Offsets.To / 1000, T: s})
		}
	}
	return voiced(wav, segs)
}

// minVoice: the lowest voice score (rushbin-vision voice: Apple's sound classifier, speaking or singing) a
// segment needs to be kept. Whisper writes "Thank you.", "*music*" or "Hmm..." on music the voice detector let
// through; there the classifier hears no voice. Measured on 12 November 2026 (large-v3-turbo, the whole file
// classified once, 3 s windows touching the segment and 0.5 s around it) on shoot-a and the T9: the 17 invented
// segments on music without voices score 0.010 to 0.117, the 901 segments of real speech 0.492 to 0.981, the
// 15 sung lines 0.553 to 0.848. 0.3 is about the middle of the gap. It does not catch text invented over real
// voices (crowds, choirs at feasts score 0.666 to 0.874) nor Whisper repeating a line: those need another signal.
const minVoice = 0.3

// voiced keeps the segments in which the Mac hears a voice. SoundAnalysis is Apple's: on Windows, and in a
// development build without the Swift helper, every segment is kept, as before.
func voiced(wav string, segs []Segment) ([]Segment, error) {
	if runtime.GOOS != "darwin" || len(segs) == 0 {
		return segs, nil
	}
	vis := tool("rushbin-vision")
	if _, err := exec.LookPath(vis); err != nil {
		return segs, nil
	}
	var in strings.Builder
	for _, s := range segs {
		fmt.Fprintf(&in, "%g %g\n", s.S, s.E)
	}
	cmd := background(vis, "voice", wav)
	cmd.Stdin = strings.NewReader(in.String())
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("voice: %v", err)
	}
	var scores []float64
	for _, f := range strings.Fields(string(out)) {
		v, err := strconv.ParseFloat(f, 64)
		if err != nil {
			return nil, fmt.Errorf("voice: %v", err)
		}
		scores = append(scores, v)
	}
	return keepVoiced(segs, scores)
}

// keepVoiced keeps the segments whose voice score reaches minVoice; scores come one per segment, in order.
func keepVoiced(segs []Segment, scores []float64) ([]Segment, error) {
	if len(scores) != len(segs) {
		return nil, fmt.Errorf("voice: %d scores for %d segments", len(scores), len(segs))
	}
	kept := []Segment{}
	for i, s := range segs {
		if scores[i] >= minVoice {
			kept = append(kept, s)
		}
	}
	return kept, nil
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	return s[strings.LastIndex(s, "\n")+1:]
}

// loadSpeech replays speech.jsonl: one line per content, the last one wins.
func loadSpeech(file string) (map[string]*Speech, error) {
	m := map[string]*Speech{}
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
			Thumb string
			Segs  []Segment
		}
		if dec.Decode(&v) != nil {
			return m, nil // end of file, or a line cut by a crash
		}
		m[v.Thumb] = newSpeech(v.Segs)
	}
}

// setSpeech records a transcript while the catalog is shown.
func (c *Catalog) setSpeech(thumb string, s *Speech) {
	c.dmu.Lock()
	c.speech[thumb] = s
	c.dmu.Unlock()
}

func (c *Catalog) speechOf(thumb string) *Speech {
	c.dmu.RLock()
	defer c.dmu.RUnlock()
	return c.speech[thumb]
}
