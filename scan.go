package main

import (
	"bufio"
	"crypto/md5"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Device is one drive, identified by its volume UUID so a renamed drive is still recognized.
type Device struct {
	UUID     string   `json:"uuid"`
	Name     string   `json:"name"`
	Mount    string   `json:"mount"`
	FS       string   `json:"fs"`
	Bus      string   `json:"bus"`
	Media    string   `json:"media"`
	Size     int64    `json:"size"`
	Free     int64    `json:"free"`
	Scanned  string   `json:"scanned"`
	Excluded []string `json:"excluded"`
	// The heavy work, chosen per drive when it is first connected (nil: not chosen,
	// so off). Content: speech, on-screen text and images, for search by content. Copies: light copies for
	// the team. The catalog itself (files, details, previews) always runs.
	Content *bool `json:"content,omitempty"`
	Copies  *bool `json:"copies,omitempty"`
	// Local: kept on this computer, out of the team ("Send to the team" off): nothing of the drive goes to the
	// team site, and what the site had of it is removed (share). Off unless chosen, so drives sent before stay sent.
	Local *bool `json:"local,omitempty"`
	// Location: where the drive is kept (a shelf, a box, a person), free text. Retired: the day it was marked
	// as gone (broken, lost, erased): it stays listed with what was lost, but no longer counts as a copy.
	Location string `json:"location,omitempty"`
	Retired  string `json:"retired,omitempty"`
}

func on(b *bool) bool { return b != nil && *b } // off unless chosen: privacy and the computer's work by default

// Rec is one file of the inventory (one line of files.jsonl).
type Rec struct {
	Path, Ext, Hash      string
	Size                 int64
	Mtime                string
	Dur                  float64
	W, H                 int
	Codec, FPS, Creation string
	Bitrate              int64
	PixFmt               string `json:",omitempty"`
	Span                 int64  `json:",omitempty"` // a clip split in files (RED): the size of all of them
}

var probeExt = map[string]bool{".mxf": true, ".mp4": true, ".mov": true, ".m4v": true, ".avi": true, ".mkv": true, ".mts": true, ".mpg": true, ".wav": true}

// say reports progress; the app replaces it to show the scan state in its window.
// ponytail: one global, fine while drives are scanned one at a time.
var say = func(format string, a ...any) { fmt.Printf(format+"\n", a...) }

// progress reports how far a step of the scan is: 1 reading the drive (files read, total unknown), 2 checking
// duplicates (bytes read of the total). The app shows it as a bar.
var progress = func(step int, done, total int64) {}

// tool finds an FFmpeg program next to this executable (inside the app bundle) or on the PATH.
func tool(name string) string {
	if exe, err := os.Executable(); err == nil {
		for _, p := range []string{filepath.Join(filepath.Dir(exe), name), filepath.Join(filepath.Dir(exe), name+".exe")} {
			if fileExists(p) {
				return p
			}
		}
	}
	return name
}

func fileExists(p string) bool { st, err := os.Stat(p); return err == nil && !st.IsDir() }

// resource is a file or folder in the app's Resources, next to Contents/MacOS; "" outside the app.
func resource(name string) string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	p := filepath.Join(filepath.Dir(exe), "..", "Resources", name)
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

// scan catalogs one drive and returns its ID.
func scan(catalog, vol, id, name string, excluded []string) (string, error) {
	if _, err := exec.LookPath(tool("ffprobe")); err != nil {
		return "", errors.New("ffprobe not found: install FFmpeg")
	}
	vol = strings.TrimSuffix(vol, "/")
	if st, err := os.Stat(vol); err != nil || !st.IsDir() {
		return "", fmt.Errorf("drive not found: %s (is it connected and mounted?)", vol)
	}
	// A folder that is not a volume (or a system where the UUID is not read yet) needs -id.
	d, err := identify(vol)
	if id != "" {
		d.UUID = id
	} else if err != nil {
		return "", fmt.Errorf("%w: pass -id to catalog a folder that is not a volume", err)
	} else if d.UUID == "" {
		return "", errors.New("cannot read the volume UUID on this system: pass -id")
	}
	dir := filepath.Join(catalog, "disks", d.UUID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	var prev Device
	if b, err := os.ReadFile(filepath.Join(dir, "device.json")); err == nil {
		json.Unmarshal(b, &prev)
	}
	if len(excluded) == 0 {
		excluded = prev.Excluded
	}
	switch {
	case name != "":
		d.Name = name
	case prev.Name != "":
		d.Name = prev.Name
	case d.Name == "":
		d.Name = filepath.Base(vol)
	}
	d.Excluded = append([]string{}, excluded...)                                                    // [] rather than null in JSON
	d.Content, d.Copies, d.Local, d.Location = prev.Content, prev.Copies, prev.Local, prev.Location // Retired is not kept: the drive is here
	d.Scanned = time.Now().UTC().Format("2006-01-02T15:04:05Z")
	b, _ := json.MarshalIndent(d, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "device.json"), append(b, '\n'), 0o644); err != nil {
		return "", err
	}
	say("Reading %s", d.Name)
	progress(1, 0, 0)

	start := time.Now()
	n, err := inventory(vol, filepath.Join(dir, "files.jsonl"), excluded)
	if err != nil {
		return "", err
	}
	say("%s: %d files read in %s", d.Name, n, time.Since(start).Round(time.Second))
	return d.UUID, nil
}

// checkDuplicates reads byte for byte every file of drive id whose sample matches a file anywhere in the catalog
// (the command line; the app reads only the copies the trash waits for, when asked: App.verify).
func checkDuplicates(catalog, id, name string) error {
	todo, err := candidates(catalog, id)
	if err != nil {
		return err
	}
	say("%s: checking %d possible duplicates byte for byte", name, len(todo))
	return fullHash(todo, filepath.Join(catalog, "disks", id, "fullhash.tsv"))
}

// identify reads the volume identity: diskutil on macOS, the volume serial number on Windows.
// ponytail: Linux has no app, so no identity there: the caller passes -id.
func identify(vol string) (Device, error) {
	d := Device{Mount: vol}
	if runtime.GOOS == "windows" {
		return identifyWindows(vol)
	}
	if runtime.GOOS != "darwin" {
		return d, nil
	}
	info, err := exec.Command("/usr/sbin/diskutil", "info", "-plist", vol).Output()
	if err != nil {
		return d, fmt.Errorf("diskutil info %s: %w", vol, err)
	}
	conv := exec.Command("/usr/bin/plutil", "-convert", "json", "-o", "-", "-")
	conv.Stdin = strings.NewReader(string(info))
	js, err := conv.Output()
	if err != nil {
		return d, fmt.Errorf("plutil: %w", err)
	}
	var p struct {
		VolumeUUID, DiskUUID, VolumeName, FilesystemName, BusProtocol, MediaName string
		TotalSize, FreeSpace                                                     int64
	}
	if err := json.Unmarshal(js, &p); err != nil {
		return d, err
	}
	d.UUID, d.Name, d.FS, d.Bus, d.Media, d.Size, d.Free = p.VolumeUUID, p.VolumeName, p.FilesystemName, p.BusProtocol, p.MediaName, p.TotalSize, p.FreeSpace
	if d.UUID == "" {
		// NTFS has no volume UUID on the Mac, and its serial number is on the raw disk, readable only by root:
		// the GPT partition's own GUID, which Windows reads too. NTFS on an MBR disk stays unidentified.
		d.UUID = p.DiskUUID
	}
	return d, nil
}

// fsUUID is the UUID macOS gives a FAT or exFAT volume (Apple's msdosfs, msdosfs_generate_volume_uuid): a
// version 3 UUID of the serial number in the boot sector, little endian, and for FAT of its total sectors.
// Windows reads the same serial number, so it computes the same identity and a drive is one on both.
func fsUUID(serial uint32, sectors ...uint32) string {
	b := []byte{0xB3, 0xE2, 0x0F, 0x39, 0xF2, 0x92, 0x11, 0xD6, 0x97, 0xA4, 0x00, 0x30, 0x65, 0x43, 0xEC, 0xAC}
	b = binary.LittleEndian.AppendUint32(b, serial)
	for _, s := range sectors {
		b = binary.LittleEndian.AppendUint32(b, s)
	}
	h := md5.Sum(b)
	h[6] = h[6]&0x0f | 0x30
	h[8] = h[8]&0x3f | 0x80
	return fmt.Sprintf("%X-%X-%X-%X-%X", h[0:4], h[4:6], h[6:8], h[8:10], h[10:])
}

// noise are the folders no drive is read in: macOS bookkeeping, and trash (what rushbin moved aside).
var noise = map[string]bool{".Spotlight-V100": true, ".Trashes": true, ".fseventsd": true, ".TemporaryItems": true, trash: true}

// inventory walks the drive in parallel (stat, sampled hash, ffprobe) and writes one JSON line per file.
// The file is replaced only when the walk is complete, so an interrupted scan keeps the previous inventory.
func inventory(root, out string, excluded []string) (int, error) {
	skip := maps.Clone(noise)
	for _, x := range excluded {
		skip[x] = true
	}
	// The previous inventory: a file with the same path, size and modification time (the key of fullhash.tsv
	// too) is not read again. An unreadable one is read from scratch.
	prev := map[string]Rec{}
	if eachRec(out, func(r Rec) { prev[r.Path] = r }) != nil {
		clear(prev)
	}
	tmp, err := os.CreateTemp(filepath.Dir(out), ".files-*.jsonl")
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp.Name())

	paths, recs := make(chan string, 4096), make(chan Rec, 4096)
	go func() {
		filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // unreadable folder: skip it, keep the rest
			}
			if d.IsDir() {
				if skip[d.Name()] {
					return fs.SkipDir
				}
				return nil
			}
			if strings.HasPrefix(d.Name(), "._") || d.Name() == ".DS_Store" || !d.Type().IsRegular() {
				return nil // also links: a link on a client's drive must not bring in another drive's files
			}
			paths <- p
			return nil
		})
		close(paths)
	}()
	var wg sync.WaitGroup
	for i := 0; i < runtime.NumCPU()*2; i++ { // ponytail: up to 2×CPU ffprobe at once, measured fine on 18 cores
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range paths {
				st, err := os.Stat(p)
				if err != nil {
					continue
				}
				e := strings.ToLower(filepath.Ext(p))
				r := Rec{Path: p, Ext: e, Size: st.Size(), Mtime: st.ModTime().UTC().Format(time.RFC3339)}
				red := redClip(p)
				// A RED clip is read again every time: its record covers the _002 … files next to it too.
				if old, ok := prev[p]; ok && old.Size == r.Size && old.Mtime == r.Mtime && !red {
					recs <- old
					continue
				}
				if probeExt[e] || e == ".r3d" || e == ".nev" || (e == ".ts" && st.Size() > 50<<20) {
					r.Hash = sampleHash(p, r.Size)
				}
				if probeExt[e] {
					probe(&r)
				}
				if red {
					probeRED(&r)
				}
				recs <- r
			}
		}()
	}
	go func() { wg.Wait(); close(recs) }()

	w := bufio.NewWriterSize(tmp, 1<<20)
	enc := json.NewEncoder(w)
	n := 0
	for r := range recs {
		enc.Encode(r)
		n++
		progress(1, int64(n), 0)
	}
	if err := w.Flush(); err != nil {
		return 0, err
	}
	if err := tmp.Chmod(0o644); err != nil { // CreateTemp makes it 0600
		return 0, err
	}
	if err := tmp.Close(); err != nil {
		return 0, err
	}
	return n, os.Rename(tmp.Name(), out)
}

// sampleHash fingerprints a file from its size and its first and last MiB.
// Equal fingerprints only mark candidates: duplicates are confirmed by the full hash.
func sampleHash(p string, size int64) string {
	f, err := os.Open(p)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	fmt.Fprintf(h, "%d", size)
	const n = 1 << 20
	io.CopyN(h, f, n)
	if size > 2*n {
		f.Seek(-n, io.SeekEnd)
		io.CopyN(h, f, n)
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// redClip: a file rushbin-red reads as a clip. A RED clip is split in files _001, _002 … and starts at its
// _001; a Nikon clip is one file, R3D NE (DSC_0001.R3D, four digits) or N-RAW (DSC_0001.NEV).
func redClip(p string) bool {
	n := strings.ToLower(filepath.Base(p))
	switch filepath.Ext(n) {
	case ".nev":
		return true
	case ".r3d":
		stem := strings.TrimSuffix(n, ".r3d")
		i := strings.LastIndexByte(stem, '_')
		_, err := strconv.Atoi(stem[i+1:])
		return stem[i+1:] == "001" || i < 0 || len(stem)-i-1 != 3 || err != nil // not a _002 … part
	}
	return false
}

// probeRED reads a RED or Nikon clip (redClip) with rushbin-red, built on RED's R3D SDK, whose license keeps it
// out of this repository; without it these files stay plain files. A RED clip is its _001 file and the _002,
// _003 … next to it.
func probeRED(r *Rec) {
	out, err := exec.Command(tool("rushbin-red"), "info", r.Path).Output()
	if err != nil {
		return
	}
	var c struct {
		Frames        int
		FPS           float64
		Width, Height int
	}
	if json.Unmarshal(out, &c) != nil || c.FPS <= 0 || c.Frames <= 0 {
		return
	}
	r.Dur, r.W, r.H, r.Codec = float64(c.Frames)/c.FPS, c.Width, c.Height, map[string]string{".r3d": "r3d", ".nev": "n-raw"}[r.Ext]
	r.FPS = fmt.Sprintf("%d/1000", int(math.Round(c.FPS*1000)))
	name := filepath.Base(r.Path)
	if !strings.HasSuffix(strings.ToLower(name), "_001.r3d") {
		return // one file
	}
	prefix := strings.ToLower(name[:len(name)-len("001.r3d")])
	files, _ := os.ReadDir(filepath.Dir(r.Path))
	for _, f := range files {
		n := strings.ToLower(f.Name())
		if rest, ok := strings.CutPrefix(n, prefix); ok && len(rest) == len("001.r3d") && strings.HasSuffix(rest, ".r3d") {
			if _, err := strconv.Atoi(rest[:3]); err == nil {
				if info, err := f.Info(); err == nil {
					r.Span += info.Size()
				}
			}
		}
	}
}

func probe(r *Rec) {
	out, err := exec.Command(tool("ffprobe"), "-v", "error", "-print_format", "json", "-show_format", "-show_streams", r.Path).Output()
	if err != nil {
		return
	}
	var p struct {
		Format struct {
			Duration string `json:"duration"`
			BitRate  string `json:"bit_rate"`
			Tags     struct {
				CreationTime string `json:"creation_time"`
			} `json:"tags"`
		} `json:"format"`
		Streams []struct {
			CodecType  string `json:"codec_type"`
			CodecName  string `json:"codec_name"`
			RFrameRate string `json:"r_frame_rate"`
			PixFmt     string `json:"pix_fmt"`
			Width      int    `json:"width"`
			Height     int    `json:"height"`
		} `json:"streams"`
	}
	if json.Unmarshal(out, &p) != nil {
		return
	}
	r.Dur, _ = strconv.ParseFloat(p.Format.Duration, 64)
	r.Bitrate, _ = strconv.ParseInt(p.Format.BitRate, 10, 64)
	r.Creation = p.Format.Tags.CreationTime
	for _, s := range p.Streams {
		if s.CodecType == "video" {
			r.Codec, r.FPS, r.W, r.H, r.PixFmt = s.CodecName, s.RFrameRate, s.Width, s.Height, s.PixFmt
			break
		}
	}
}

// candidates lists the files of drive id whose sampled hash appears more than once in the whole catalog
// (any drive) and that have no full hash taken at their current size and modification time.
func candidates(catalog, id string) ([]string, error) {
	devs, err := filepath.Glob(filepath.Join(catalog, "disks", "*", "device.json"))
	if err != nil {
		return nil, err
	}
	seen := map[string]int{}
	var mine []Rec
	for _, dj := range devs {
		dir := filepath.Dir(dj)
		err := eachRec(filepath.Join(dir, "files.jsonl"), func(r Rec) {
			if r.Hash == "" {
				return
			}
			seen[r.Hash]++
			if filepath.Base(dir) == id {
				mine = append(mine, r)
			}
		})
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
	}
	valid, err := validHashes(filepath.Join(catalog, "disks", id, "fullhash.tsv"))
	if err != nil {
		return nil, err
	}
	var todo []string
	for _, r := range mine {
		t, err := time.Parse(time.RFC3339, r.Mtime)
		if seen[r.Hash] > 1 && (err != nil || !valid[stamp(r.Path, r.Size, t.Unix())]) {
			todo = append(todo, r.Path)
		}
	}
	return todo, nil
}

func eachRec(file string, fn func(Rec)) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	return recsFrom(f, file, fn)
}

// recsFrom reads files.jsonl lines; name is only for the error message.
func recsFrom(r io.Reader, file string, fn func(Rec)) error {
	dec := json.NewDecoder(bufio.NewReader(r))
	for {
		var r Rec
		if err := dec.Decode(&r); err == io.EOF {
			return nil
		} else if err != nil {
			return fmt.Errorf("%s: %w", file, err)
		}
		fn(r)
	}
}

func stamp(path string, size, mtime int64) string {
	return fmt.Sprintf("%s\t%d\t%d", path, size, mtime)
}

// validHashes reads fullhash.tsv (hash, path, size, mtime). A hash is valid only for the size and
// modification time the file had when it was read; lines without them come from an older format.
func validHashes(file string) (map[string]bool, error) {
	valid := map[string]bool{}
	f, err := os.Open(file)
	if errors.Is(err, fs.ErrNotExist) {
		return valid, nil
	} else if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<16), 1<<20)
	for sc.Scan() {
		c := strings.Split(sc.Text(), "\t")
		if len(c) != 4 {
			continue
		}
		size, e1 := strconv.ParseInt(c[2], 10, 64)
		mtime, e2 := strconv.ParseInt(c[3], 10, 64)
		if e1 == nil && e2 == nil {
			valid[stamp(c[1], size, mtime)] = true
		}
	}
	return valid, sc.Err()
}

// fullHash appends "sha256 \t path \t size \t mtime" to out for each file, reading 8 files at a time.
func fullHash(paths []string, out string) error {
	f, err := os.OpenFile(out, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	var total int64
	for _, p := range paths {
		if st, err := os.Stat(p); err == nil {
			total += st.Size()
		}
	}
	var read atomic.Int64 // bytes hashed so far, reported every second
	progress(2, 0, total)
	tick, stop, stopped := time.NewTicker(time.Second), make(chan struct{}), make(chan struct{})
	defer func() { // the last report is the final count; none after the next step has started
		tick.Stop()
		close(stop)
		<-stopped
		progress(2, read.Load(), total)
	}()
	go func() {
		defer close(stopped)
		for {
			select {
			case <-tick.C:
				progress(2, read.Load(), total)
			case <-stop:
				return
			}
		}
	}()
	jobs, res := make(chan string), make(chan string)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ { // ponytail: 8 parallel reads, the drive is the limit, not the CPU
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range jobs {
				if line, err := hashFile(p, &read); err == nil {
					res <- line
				} else {
					fmt.Fprintln(os.Stderr, "rushbin:", err)
				}
			}
		}()
	}
	go func() {
		for _, p := range paths {
			jobs <- p
		}
		close(jobs)
		wg.Wait()
		close(res)
	}()
	var werr error
	for line := range res {
		if _, err := fmt.Fprintln(f, line); err != nil && werr == nil {
			werr = err
		}
	}
	if err := f.Close(); werr == nil {
		werr = err
	}
	return werr
}

func hashFile(p string, read *atomic.Int64) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat() // size and mtime travel with the hash: if the file changes, the hash is stale
	if err != nil {
		return "", err
	}
	h := sha256.New()
	if _, err := io.Copy(h, io.TeeReader(f, counter{read})); err != nil {
		return "", fmt.Errorf("%s: %w", p, err)
	}
	return fmt.Sprintf("%x\t%s", h.Sum(nil), stamp(p, st.Size(), st.ModTime().Unix())), nil
}

// counter adds the bytes written through it to n.
type counter struct{ n *atomic.Int64 }

func (c counter) Write(b []byte) (int, error) { c.n.Add(int64(len(b))); return len(b), nil }
