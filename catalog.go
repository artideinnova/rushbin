package main

import (
	"bufio"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Catalog is everything the app shows, rebuilt in memory from the per-drive files after every scan.
// ponytail: whole catalog in memory, about 90 MB live heap per 100k clips (BenchmarkScale); move queries to the database if archives outgrow it.
type Catalog struct {
	Devices   []*DeviceInfo
	list      []*Clip
	byKey     map[string]*Clip   // device|id
	byCont    map[string][]*Clip // content → every copy
	files     map[string]Rec     // path → inventory record (size and mtime for the trash checks)
	dmu       sync.RWMutex       // decisions change while pages read the catalog
	dec       map[string]*Decision
	looks     map[string]*Look   // thumb → what Apple Vision saw (nil: analyzed, no frames)
	speech    map[string]*Speech // thumb → what is said (guarded by dmu: it grows while pages read)
	onlineAt  map[string]string  // drive → the team Mac it is connected to now (on the site; guarded by dmu)
	proxies   map[string]bool    // thumb → the team has a proxy (on the site)
	forgotten map[string]bool    // thumb → its data was removed on request (on the site)

	embOnce  sync.Once   // the previews' frame embeddings, read by the first search by looks alone (text.go)
	embThumb []string    // the content of each frame
	embV     [][]float32 // each frame, normalized
}

type DeviceInfo struct {
	Device
	Clips      int     `json:"clips"`
	Seconds    float64 `json:"seconds"`
	VideoBytes int64   `json:"video_bytes"`
	Unique     int     `json:"unique"` // clips with no copy on any other drive
	Lost       int     `json:"lost"`   // a retired drive: its clips with no copy on the drives still here
	LostB      int64   `json:"lost_bytes"`
	UniqueB    int64   `json:"unique_bytes"` // and their size: what would be lost with this drive
	Online     bool    `json:"online"`
}

// Clip is one video file. Copies of the same content share Content (sampled hash).
type Clip struct {
	Device   string     `json:"device"`
	ID       string     `json:"id"` // path relative to the drive
	Path     string     `json:"path"`
	Project  string     `json:"project"`
	Name     string     `json:"name"`
	Ext      string     `json:"ext"`
	Codec    string     `json:"codec"`
	W        int        `json:"w"`
	H        int        `json:"h"`
	FPS      *float64   `json:"fps"`
	Dur      float64    `json:"dur"`
	Size     int64      `json:"size"`
	Content  string     `json:"content"`
	FullHash string     `json:"full_hash,omitempty"`
	Created  *time.Time `json:"created"`
	Thumb    string     `json:"thumb"`
	Alpha    bool       `json:"alpha,omitempty"` // has transparency: graphics, titles, mattes
	Look     *Look      `json:"look,omitempty"`
}

// Decision is the latest vote and kept copy for one content, with who took each and when (in a team), and what
// a member decided about its transcript: the lines hidden as wrong, and whether someone checked it.
type Decision struct {
	Vote      string    `json:"vote,omitempty"` // keep | discard | showreel
	Copy      string    `json:"copy,omitempty"` // device|id of the copy to keep
	VoteBy    string    `json:"vote_by,omitempty"`
	VoteAt    string    `json:"vote_at,omitempty"`
	CopyBy    string    `json:"copy_by,omitempty"`
	CopyAt    string    `json:"copy_at,omitempty"`
	Hidden    []float64 `json:"hidden,omitempty"` // starts (s) of the transcript's lines hidden: not shown, not searched
	Checked   bool      `json:"checked,omitempty"`
	CheckedBy string    `json:"checked_by,omitempty"`
	CheckedAt string    `json:"checked_at,omitempty"`
}

// DecisionLine is one line of decisions.jsonl, and one decision of a team (ID set by the site).
type DecisionLine struct {
	ID      int64  `json:"id,omitempty"`
	T       string `json:"t"`
	Content string `json:"content"`
	Kind    string `json:"kind"` // vote | copy | hide (the starts of every hidden line, space separated) | check ("yes" or "")
	Value   string `json:"value"`
	By      string `json:"by,omitempty"`
}

// apply records a decision line in m.
func (l DecisionLine) apply(m map[string]*Decision) {
	d := m[l.Content]
	if d == nil {
		d = &Decision{}
		m[l.Content] = d
	}
	switch l.Kind {
	case "vote": // only the known votes: they become HTML, and a team site is not trusted with markup
		if l.Value != "" && l.Value != "keep" && l.Value != "discard" && l.Value != "showreel" {
			return
		}
		d.Vote, d.VoteBy, d.VoteAt = l.Value, l.By, l.T
	case "copy":
		d.Copy, d.CopyBy, d.CopyAt = l.Value, l.By, l.T
	case "hide":
		if h, ok := starts(l.Value); ok {
			d.Hidden = h
		}
	case "check":
		if l.Value == "" || l.Value == "yes" {
			d.Checked, d.CheckedBy, d.CheckedAt = l.Value == "yes", l.By, l.T
		}
	}
}

// starts reads the value of a hide decision: the start of each hidden line, in seconds.
func starts(v string) ([]float64, bool) {
	var h []float64
	for _, f := range strings.Fields(v) {
		x, err := strconv.ParseFloat(f, 64)
		if err != nil {
			return nil, false
		}
		h = append(h, x)
	}
	return h, true
}

// decide records a decision taken while the catalog is shown.
func (c *Catalog) decide(l DecisionLine) {
	c.dmu.Lock()
	l.apply(c.dec)
	c.dmu.Unlock()
}

var videoExt = map[string]bool{".mxf": true, ".mp4": true, ".mov": true, ".m4v": true, ".mts": true, ".avi": true, ".mkv": true, ".mpg": true, ".r3d": true, ".nev": true}

// viaRED: an R3D or N-RAW clip, which FFmpeg cannot read: rushbin-red decodes it (probeRED, input).
func viaRED(cl *Clip) bool { return cl.Ext == ".r3d" || cl.Ext == ".nev" }

// decoder is the program that decodes a clip's pictures into PPM frames when FFmpeg cannot do it on the GPU:
// RED's SDK (rushbin-red) for R3D; on the Mac, AVFoundation (rushbin-vision) for ProRes RAW, whose Bayer frames
// VideoToolbox does not hand to FFmpeg (-12905) while it decodes and debayers them for AVFoundation. "": FFmpeg.
func decoder(cl *Clip) string {
	switch {
	case viaRED(cl):
		return "rushbin-red"
	case cl.Codec == "prores_raw" && runtime.GOOS == "darwin":
		return "rushbin-vision"
	}
	return ""
}

// input: FFmpeg's arguments that read a clip from t seconds on and pick its first video stream (with video)
// and its first audio stream, if any. An R3D clip comes from rushbin-red instead, both from t on: its frames
// as a PPM stream on FFmpeg's pipe 3 (with video), then its sound as WAV on the next pipe (when it has sound).
// feed starts rushbin-red for the FFmpeg command, before it starts; done, once FFmpeg has ended, closes the
// pipes and waits for rushbin-red. run starts rushbin-red as FFmpeg is: exec.Command, or background for long jobs.
func input(cl *Clip, t float64, video bool, run func(string, ...string) *exec.Cmd) (args []string, feed func(ff *exec.Cmd) (done func(), err error)) {
	start := strconv.FormatFloat(t, 'f', 3, 64)
	if dec := decoder(cl); dec == "rushbin-vision" { // the pictures from AVFoundation, the sound from the file
		var reds []*exec.Cmd
		if video {
			fps := 25.0
			if cl.FPS != nil {
				fps = *cl.FPS
			}
			reds = append(reds, run(tool(dec), "frames", cl.Path, start))
			args = append(args, "-framerate", strconv.FormatFloat(fps, 'f', -1, 64), "-f", "ppm_pipe", "-i", "pipe:3")
		}
		if t > 0 {
			args = append(args, "-ss", start)
		}
		args = append(args, "-i", cl.Path) // the maps after every input: FFmpeg reads -map as an output option
		if video {
			args = append(args, "-map", "0:v:0")
		}
		return append(args, "-map", fmt.Sprintf("%d:a:0?", len(reds))), pipesFrom(reds)
	}
	if !viaRED(cl) {
		if t > 0 {
			args = []string{"-ss", start}
		}
		args = append(args, "-i", cl.Path)
		if video {
			args = append(args, "-map", "0:v:0")
		}
		return append(args, "-map", "0:a:0?"), func(*exec.Cmd) (func(), error) { return func() {}, nil }
	}
	var c struct {
		FPS   float64
		Audio int
	}
	out, err := exec.Command(tool("rushbin-red"), "info", cl.Path).Output()
	if err == nil {
		err = json.Unmarshal(out, &c)
	}
	if err != nil {
		return nil, func(*exec.Cmd) (func(), error) { return nil, fmt.Errorf("rushbin-red: %v", err) }
	}
	var reds []*exec.Cmd
	var maps []string
	if video {
		reds = append(reds, run(tool("rushbin-red"), "frames", cl.Path, start))
		args = append(args, "-framerate", strconv.FormatFloat(c.FPS, 'f', -1, 64), "-f", "ppm_pipe", "-i", "pipe:3")
		maps = []string{"-map", "0:v:0"}
	}
	if c.Audio > 0 { // FFmpeg's outputs wait in vain for an audio stream without sound
		reds = append(reds, run(tool("rushbin-red"), "audio", cl.Path, start)) // FFmpeg's -ss skips wrong in a WAV pipe
		args = append(args, "-f", "wav", "-i", fmt.Sprintf("pipe:%d", 2+len(reds)))
		maps = append(maps, "-map", fmt.Sprintf("%d:a:0", len(reds)-1))
	}
	args = append(args, maps...)
	return args, pipesFrom(reds)
}

// pipesFrom starts the decoders when FFmpeg starts, each writing into FFmpeg's pipe 3, 4 …
func pipesFrom(reds []*exec.Cmd) func(ff *exec.Cmd) (func(), error) {
	return func(ff *exec.Cmd) (func(), error) {
		var pipes []*os.File
		done := func() {
			for _, r := range pipes {
				r.Close() // rushbin-red stops writing to an FFmpeg that has ended
			}
			for _, red := range reds {
				if red.Process != nil {
					red.Wait()
				}
			}
		}
		for _, red := range reds {
			r, w, err := os.Pipe()
			if err != nil {
				done()
				return nil, err
			}
			pipes = append(pipes, r)
			red.Stdout = w
			err = red.Start()
			w.Close()
			if err != nil {
				done()
				return nil, err
			}
		}
		ff.ExtraFiles = pipes
		return done, nil
	}
}

var notAlnum = regexp.MustCompile(`[^A-Za-z0-9]+`)

// drive is what one drive brings to the catalog: its device.json and its files.jsonl and fullhash.tsv,
// from the drive folders on the Mac or from the team database on the site.
type drive struct {
	Device
	files io.Reader         // nil when not written yet
	valid map[string]string // its fullhash.tsv read (fullHashes): stamp → full hash
}

// loadCatalog reads every drive folder (device.json, files.jsonl, fullhash.tsv), decisions.jsonl and vision.jsonl.
func loadCatalog(dir string, online map[string]bool) (*Catalog, error) {
	devs, err := filepath.Glob(filepath.Join(dir, "disks", "*", "device.json"))
	if err != nil {
		return nil, err
	}
	var ds []drive
	for _, dj := range devs {
		var d drive
		b, err := os.ReadFile(dj)
		if err != nil || json.Unmarshal(b, &d.Device) != nil {
			continue
		}
		f, err := os.Open(filepath.Join(filepath.Dir(dj), "files.jsonl"))
		if err == nil {
			defer f.Close()
			d.files = f
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		f, err = os.Open(filepath.Join(filepath.Dir(dj), "fullhash.tsv"))
		if err == nil {
			d.valid, err = fullHashes(f)
			f.Close()
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		ds = append(ds, d)
	}
	dec, err := loadDecisions(filepath.Join(dir, "decisions.jsonl"))
	if err != nil {
		return nil, err
	}
	looks, err := loadLooks(filepath.Join(dir, "vision.jsonl"))
	if err != nil {
		return nil, err
	}
	speech, err := loadSpeech(filepath.Join(dir, "speech.jsonl"))
	if err != nil {
		return nil, err
	}
	c, err := buildCatalog(ds, online, dec, looks)
	if c != nil {
		c.speech = speech
	}
	return c, err
}

// buildCatalog turns the drives, the decisions and what Apple Vision saw into the catalog the pages show.
func buildCatalog(ds []drive, online map[string]bool, dec map[string]*Decision, looks map[string]*Look) (*Catalog, error) {
	c := &Catalog{byKey: map[string]*Clip{}, byCont: map[string][]*Clip{}, files: map[string]Rec{}, dec: dec, looks: looks, speech: map[string]*Speech{}}
	// Retired drives last: their clips are not in the archive, only counted, and lost if no drive still here has them.
	ds = slices.Clone(ds)
	sort.SliceStable(ds, func(i, j int) bool { return ds[i].Retired == "" && ds[j].Retired != "" })
	for _, dr := range ds {
		lost := map[string]bool{}
		d := DeviceInfo{Device: dr.Device, Online: online[dr.UUID]}
		valid := dr.valid
		if dr.files != nil {
			err := recsFrom(dr.files, "files.jsonl", func(r Rec) {
				c.files[r.Path] = r
				if !videoExt[r.Ext] || r.Dur <= 0 {
					return
				}
				rel := r.Path // the clip's ID: its path on the drive, with "/" on Windows too
				if x, err := filepath.Rel(d.Mount, r.Path); err == nil {
					rel = filepath.ToSlash(x)
				}
				cl := &Clip{Device: d.UUID, ID: rel, Path: r.Path, Project: part(rel, 0), Name: filepath.Base(r.Path),
					Ext: r.Ext, Codec: r.Codec, W: r.W, H: r.H, FPS: fps(r.FPS), Dur: r.Dur, Size: r.Size, Content: "c:" + r.Hash,
					Alpha: alpha(r.PixFmt)}
				if t, err := time.Parse(time.RFC3339, r.Mtime); err == nil {
					cl.FullHash = valid[stamp(r.Path, r.Size, t.Unix())]
				}
				if t, err := time.Parse(time.RFC3339Nano, r.Creation); err == nil {
					cl.Created = &t
				}
				d.Clips++
				d.Seconds += cl.Dur
				d.VideoBytes += cl.Size
				if d.Retired != "" {
					if len(c.byCont[cl.Content]) == 0 && !lost[cl.Content] {
						lost[cl.Content] = true
						d.Lost++
						d.LostB += cl.Size
					}
					return
				}
				if r.Span > 0 {
					cl.Size = r.Span
				}
				cl.Thumb = notAlnum.ReplaceAllString(cl.Content, "_")
				cl.Look = looks[cl.Thumb]
				c.list = append(c.list, cl)
				c.byKey[cl.Device+"|"+cl.ID] = cl
				c.byCont[cl.Content] = append(c.byCont[cl.Content], cl)
			})
			if err != nil {
				return nil, err
			}
		}
		c.Devices = append(c.Devices, &d)
	}
	// Connected drives first, retired ones last, the rest by the latest cataloged.
	sort.SliceStable(c.Devices, func(i, j int) bool {
		a, b := c.Devices[i], c.Devices[j]
		if a.Online != b.Online || (a.Retired == "") != (b.Retired == "") {
			return a.Online && !b.Online || a.Retired == "" && b.Retired != ""
		}
		if a.Scanned != b.Scanned {
			return a.Scanned > b.Scanned
		}
		return a.Name < b.Name
	})
	for _, cs := range c.byCont {
		only := cs[0].Device
		for _, cl := range cs[1:] {
			if cl.Device != only {
				only = ""
			}
		}
		if only != "" {
			d := c.device(only)
			d.Unique++
			d.UniqueB += cs[0].Size
		}
	}
	c.suggest()
	return c, nil
}

// fullHashes maps "path, size, mtime" to the full hash taken at that size and time (fullhash.tsv).
func fullHashes(r io.Reader) (map[string]string, error) {
	m := map[string]string{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<16), 1<<20)
	for sc.Scan() {
		c := strings.Split(sc.Text(), "\t")
		if len(c) != 4 {
			continue
		}
		size, e1 := strconv.ParseInt(c[2], 10, 64)
		mtime, e2 := strconv.ParseInt(c[3], 10, 64)
		if k := stamp(c[1], size, mtime); e1 == nil && e2 == nil && m[k] == "" {
			m[k] = c[0]
		}
	}
	return m, sc.Err()
}

// loadDecisions replays the append-only decision log: the last line for each content and kind wins.
func loadDecisions(file string) (map[string]*Decision, error) {
	m := map[string]*Decision{}
	f, err := os.Open(file)
	if errors.Is(err, fs.ErrNotExist) {
		return m, nil
	} else if err != nil {
		return nil, err
	}
	defer f.Close()
	dec := json.NewDecoder(bufio.NewReader(f))
	for {
		var l DecisionLine
		if err := dec.Decode(&l); err != nil {
			break // end of file, or a line cut by a crash: earlier decisions still count
		}
		l.apply(m)
	}
	return m, nil
}

func (c *Catalog) decision(content string) Decision {
	c.dmu.RLock()
	defer c.dmu.RUnlock()
	if d := c.dec[content]; d != nil {
		return *d
	}
	return Decision{}
}

// alpha: an FFmpeg pixel format with an alpha channel (yuva444p12le, rgba, gbrap, ya8…).
func alpha(pf string) bool {
	return strings.HasPrefix(pf, "yuva") || strings.HasPrefix(pf, "ya") || strings.HasPrefix(pf, "gbrap") ||
		strings.Contains(pf, "rgba") || strings.Contains(pf, "bgra") || strings.Contains(pf, "argb") || strings.Contains(pf, "abgr")
}

// part returns the i-th "/"-separated element of s, or "" (like SQL split_part).
func part(s string, i int) string {
	p := strings.Split(s, "/")
	if i < len(p) {
		return p[i]
	}
	return ""
}

// folder is the first folder inside the project, only for clips at least two levels deep.
func folder(id string) string {
	if strings.Count(id, "/") >= 2 {
		return part(id, 1)
	}
	return ""
}

// fps turns ffprobe's "30000/1001" into 29.97.
func fps(s string) *float64 {
	n, d, ok := strings.Cut(s, "/")
	if !ok || d == "0" {
		return nil
	}
	a, e1 := strconv.ParseFloat(n, 64)
	b, e2 := strconv.ParseFloat(d, 64)
	if e1 != nil || e2 != nil || b == 0 {
		return nil
	}
	v := math.Round(a/b*1000) / 1000
	return &v
}

// Group is one content stored in more than one place.
type Group struct {
	Content    string `json:"content"`
	Name       string `json:"name"`
	Size       int64  `json:"size"`
	Copies     int    `json:"copies"`
	Verified   bool   `json:"verified"` // every copy has the same full hash
	Conflict   bool   `json:"conflict"` // same sample, different content: not duplicates
	BytesExtra int64  `json:"bytes_extra"`
	Thumb      string `json:"thumb"`
	Decision
	Locations []Location `json:"locations"`
}

type Location struct {
	Disk   string `json:"disk"`
	Device string `json:"device"`
	Mount  string `json:"mount,omitempty"`
	ID     string `json:"id"`
	Path   string `json:"path,omitempty"`
	Online bool   `json:"online"`
	At     string `json:"at,omitempty"`    // the team Mac the drive is connected to now (on the site)
	Where  string `json:"where,omitempty"` // where the drive is kept
}

func (c *Catalog) group(content string) Group {
	cs := c.byCont[content]
	g := Group{Content: content, Name: cs[0].Name, Size: cs[0].Size, Copies: len(cs), Thumb: cs[0].Thumb, Decision: c.decision(content)}
	g.Locations = c.locations(content)
	hashes := map[string]bool{}
	missing := false
	for _, cl := range cs {
		if cl.FullHash == "" {
			missing = true
		} else {
			hashes[cl.FullHash] = true
		}
	}
	g.Verified = !missing && len(hashes) == 1
	g.Conflict = len(hashes) > 1
	g.BytesExtra = int64(len(cs)-1) * g.Size
	return g
}

// locations lists every copy of a content, shortest path first (the proposed copy to keep).
func (c *Catalog) locations(content string) []Location {
	cs := append([]*Clip{}, c.byCont[content]...)
	sort.Slice(cs, func(i, j int) bool { return shorter(cs[i].ID, cs[j].ID) })
	var ls []Location
	for _, cl := range cs {
		d := c.device(cl.Device)
		ls = append(ls, Location{Disk: d.Name, Device: cl.Device, Mount: d.Mount, ID: cl.ID, Path: cl.Path, Online: d.Online && fileExists(cl.Path),
			At: c.macOf(cl.Device), Where: d.Location})
	}
	return ls
}

func shorter(a, b string) bool {
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}

// contentDone counts, for each drive, its clips looked at and listened to so far (search by content). A clip
// without previews (in thumbs, the app's folder of them) counts as looked at: there is nothing to look at.
func (c *Catalog) contentDone(thumbs string) map[string][2]int {
	m := map[string][2]int{}
	c.dmu.RLock()
	defer c.dmu.RUnlock()
	for _, cl := range c.list {
		n := m[cl.Device]
		if _, ok := c.looks[cl.Thumb]; ok || !fileExists(filepath.Join(thumbs, cl.Thumb, "1.jpg")) {
			n[0]++
		}
		if _, ok := c.speech[cl.Thumb]; ok {
			n[1]++
		}
		m[cl.Device] = n
	}
	return m
}

func (c *Catalog) device(id string) *DeviceInfo {
	for _, d := range c.Devices {
		if d.UUID == id {
			return d
		}
	}
	return &DeviceInfo{}
}

// Dups lists contents found in more than one place, biggest saving first.
func (c *Catalog) Dups() []Group {
	var gs []Group
	for content, cs := range c.byCont {
		if len(cs) > 1 {
			gs = append(gs, c.group(content))
		}
	}
	sort.Slice(gs, func(i, j int) bool {
		if gs[i].BytesExtra != gs[j].BytesExtra {
			return gs[i].BytesExtra > gs[j].BytesExtra
		}
		return gs[i].Content < gs[j].Content
	})
	return gs
}

// Row is one tile of the archive view.
type Row struct {
	Device  string   `json:"device"`
	Disk    string   `json:"disk"`
	ID      string   `json:"id"`
	Content string   `json:"content"`
	Name    string   `json:"name"`
	Project string   `json:"project"`
	Ext     string   `json:"ext"`
	Codec   string   `json:"codec"`
	W       int      `json:"w"`
	H       int      `json:"h"`
	FPS     *float64 `json:"fps"`
	Dur     float64  `json:"dur"`
	Size    int64    `json:"size"`
	Copies  int      `json:"copies"`
	Thumb   string   `json:"thumb"`
	Said    *Segment `json:"said,omitempty"` // the sentence where a searched word is said
	Seen    bool     `json:"seen,omitempty"` // found by how it looks (search by content)
	Vote    string   `json:"vote,omitempty"`
	Folder  string   `json:"folder,omitempty"`
	Suggest string   `json:"suggest,omitempty"`
	Online  bool     `json:"online,omitempty"` // a copy is on a drive connected now (here, or to a team Mac on the site)
}

// Query filters the archive. Without All, each content appears once: the kept copy, else the shortest path.
type Query struct {
	Words                           []string
	Project, Device, Camera, Folder string
	Label                           string
	Flags                           []string
	Sort                            string
	All                             bool
}

func (c *Catalog) Clips(q Query) []Row {
	var hit []*Clip
	for _, cl := range c.list {
		if c.match(cl, q) {
			hit = append(hit, cl)
		}
	}
	if !q.All {
		best := map[string]*Clip{}
		for _, cl := range hit {
			if b := best[cl.Content]; b == nil || c.preferred(cl, b) {
				best[cl.Content] = cl
			}
		}
		hit = hit[:0]
		for _, cl := range best {
			hit = append(hit, cl)
		}
	}
	order := map[string]func(a, b *Clip) int{
		"duration": func(a, b *Clip) int { return cmp.Compare(b.Dur, a.Dur) },
		"name":     func(a, b *Clip) int { return strings.Compare(a.ID, b.ID) },
		"size":     func(a, b *Clip) int { return cmp.Compare(b.Size, a.Size) },
		"date": func(a, b *Clip) int {
			switch { // newest first, undated last
			case a.Created != nil && b.Created != nil:
				if o := b.Created.Compare(*a.Created); o != 0 {
					return o
				}
			case a.Created != nil:
				return -1
			case b.Created != nil:
				return 1
			}
			return strings.Compare(a.ID, b.ID)
		},
	}[q.Sort]
	if order == nil {
		order = func(a, b *Clip) int {
			return cmp.Or(strings.Compare(a.Project, b.Project), strings.Compare(a.ID, b.ID))
		}
	}
	slices.SortFunc(hit, func(a, b *Clip) int {
		if o := order(a, b); o != 0 {
			return o
		}
		return strings.Compare(a.Device+a.ID, b.Device+b.ID)
	})
	if len(hit) > 5000 {
		hit = hit[:5000]
	}
	rows := make([]Row, 0, len(hit))
	for _, cl := range hit {
		var said *Segment
		for _, w := range q.Words {
			if said = c.said(cl, strings.ToLower(w)); said != nil {
				break
			}
		}
		rows = append(rows, c.row(cl, said))
	}
	return rows
}

func (c *Catalog) row(cl *Clip, said *Segment) Row {
	return Row{Said: said, Device: cl.Device, Disk: c.device(cl.Device).Name, ID: cl.ID, Content: cl.Content, Name: cl.Name,
		Project: cl.Project, Ext: cl.Ext, Codec: cl.Codec, W: cl.W, H: cl.H, FPS: cl.FPS, Dur: math.Round(cl.Dur*100) / 100,
		Size: cl.Size, Copies: len(c.byCont[cl.Content]), Thumb: cl.Thumb, Vote: c.decision(cl.Content).Vote, Folder: folder(cl.ID),
		Suggest: cl.Look.suggestion(), Online: c.device(cl.Device).Online || c.macOf(cl.Device) != ""}
}

// preferred: the copy chosen to keep, else the shortest path.
func (c *Catalog) preferred(a, b *Clip) bool {
	keep := c.decision(a.Content).Copy
	if ka, kb := a.Device+"|"+a.ID == keep, b.Device+"|"+b.ID == keep; ka != kb {
		return ka
	}
	return shorter(a.ID, b.ID)
}

func (c *Catalog) match(cl *Clip, q Query) bool {
	for _, w := range q.Words {
		w = strings.ToLower(w)
		if !strings.Contains(strings.ToLower(cl.ID), w) && !strings.Contains(strings.ToLower(cl.Codec), w) && !cl.Look.mentions(w) &&
			c.said(cl, w) == nil {
			return false
		}
	}
	if q.Project != "" && cl.Project != q.Project || q.Device != "" && cl.Device != q.Device ||
		q.Camera != "" && cl.Codec != q.Camera || q.Folder != "" && part(cl.ID, 1) != q.Folder ||
		q.Label != "" && (cl.Look == nil || !slices.Contains(cl.Look.Labels, q.Label)) {
		return false
	}
	vote := c.decision(cl.Content).Vote
	for _, f := range q.Flags {
		ok := true
		switch f {
		case "copies":
			ok = len(c.byCont[cl.Content]) > 1
		case "vertical":
			ok = cl.H > cl.W
		case "4k":
			ok = max(cl.W, cl.H) >= 3800
		case "undecided":
			ok = vote == ""
		case "keep", "discard", "showreel":
			ok = vote == f
		case "suggest-discard":
			ok = cl.Look.suggestion() == f[len("suggest-"):]
		case "red", "slowmo": // RED clips arrive with the RED module
			ok = false
		}
		if !ok {
			return false
		}
	}
	return true
}

// Overview feeds the home page: drives, projects, cameras and folders for the search suggestions.
type Overview struct {
	Devices  []*DeviceInfo `json:"devices"`
	Projects []Stat        `json:"projects"`
	Cameras  []Stat        `json:"cameras"`
	Folders  []Stat        `json:"folders"`
	Labels   []Stat        `json:"labels"`
}

type Stat struct {
	Project string  `json:"project,omitempty"`
	Folder  string  `json:"folder,omitempty"`
	Label   string  `json:"label,omitempty"`
	N       int     `json:"n"`
	Seconds float64 `json:"seconds"`
	Bytes   int64   `json:"bytes,omitempty"`
}

func (c *Catalog) Overview() Overview {
	o := Overview{Devices: c.Devices}
	if o.Devices == nil {
		o.Devices = []*DeviceInfo{}
	}
	proj, cam, fold, lab := map[string]*Stat{}, map[string]*Stat{}, map[string]*Stat{}, map[string]*Stat{}
	add := func(m map[string]*Stat, k string, s Stat, cl *Clip) {
		st := m[k]
		if st == nil {
			st = new(Stat) // s itself would escape to the heap on every call
			*st = s
			m[k] = st
		}
		st.N++
		st.Seconds += cl.Dur
		st.Bytes += cl.Size
	}
	for _, cl := range c.list {
		add(proj, cl.Project, Stat{Project: cl.Project}, cl)
		add(cam, cl.Codec, Stat{Label: cl.Codec}, cl)
		if f := folder(cl.ID); f != "" {
			add(fold, cl.Project+"/"+f, Stat{Project: cl.Project, Folder: f}, cl)
		}
		if cl.Look != nil {
			for _, l := range cl.Look.Labels {
				add(lab, l, Stat{Label: l}, cl)
			}
		}
	}
	list := func(m map[string]*Stat, less func(a, b Stat) bool) []Stat {
		s := make([]Stat, 0, len(m))
		for _, v := range m {
			s = append(s, *v)
		}
		sort.Slice(s, func(i, j int) bool { return less(s[i], s[j]) })
		return s
	}
	o.Projects = list(proj, func(a, b Stat) bool { return a.Seconds > b.Seconds || a.Seconds == b.Seconds && a.Project < b.Project })
	o.Cameras = list(cam, func(a, b Stat) bool { return a.N > b.N || a.N == b.N && a.Label < b.Label })
	if len(o.Cameras) > 20 {
		o.Cameras = o.Cameras[:20]
	}
	for i := range o.Cameras {
		o.Cameras[i].Bytes = 0
	}
	o.Folders = list(fold, func(a, b Stat) bool {
		if a.Project != b.Project {
			return a.Project < b.Project
		}
		return a.N > b.N || a.N == b.N && a.Folder < b.Folder
	})
	for i := range o.Folders {
		o.Folders[i].Bytes = 0
	}
	o.Labels = list(lab, func(a, b Stat) bool { return a.N > b.N || a.N == b.N && a.Label < b.Label })
	for i := range o.Labels {
		o.Labels[i].Bytes = 0
	}
	return o
}
