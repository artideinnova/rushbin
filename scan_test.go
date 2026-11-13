package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestScan catalogs two fake drives built with FFmpeg: duplicates inside one drive and across drives,
// a sampled-hash collision with different content, excluded and hidden files, rescans and a touched file.
func TestScan(t *testing.T) {
	tmp := t.TempDir()
	a, b, cat := filepath.Join(tmp, "A"), filepath.Join(tmp, "B"), filepath.Join(tmp, "catalog")
	for _, d := range []string{"A/Project", "A/skipme", "A/" + trash, "B/Copy"} {
		must(t, os.MkdirAll(filepath.Join(tmp, d), 0o755))
	}
	clip := filepath.Join(a, "Project", "clip.mp4")
	ffmpeg(t, "testsrc=duration=2:size=320x240:rate=25", clip)
	ffmpeg(t, "testsrc2=duration=2:size=320x240:rate=25", filepath.Join(a, "Project", "other.mp4"))
	for _, p := range []string{"A/Project/copy.mp4", "A/skipme/x.mp4", "A/" + trash + "/clip.mp4", "B/Copy/clip.mp4", "A/._clip.mp4"} {
		copyFile(t, clip, filepath.Join(tmp, p))
	}
	// Same size, first and last MiB: same sampled hash, different content.
	big := bytes.Repeat([]byte{7}, 3<<20)
	must(t, os.WriteFile(filepath.Join(a, "Project", "mid1.mov"), big, 0o644))
	big[len(big)/2] = 8
	must(t, os.WriteFile(filepath.Join(a, "Project", "mid2.mov"), big, 0o644))
	must(t, os.WriteFile(filepath.Join(a, ".DS_Store"), []byte("x"), 0o644))
	must(t, os.WriteFile(filepath.Join(a, "notes.txt"), []byte("x"), 0o644))

	var last [3]int64 // the last progress report: step, done, total
	progress = func(step int, done, total int64) { last = [3]int64{int64(step), done, total} }
	defer func() { progress = func(int, int64, int64) {} }()
	scanned(t, cat, a, "A", "", []string{"skipme"})
	if last[0] != 2 || last[2] == 0 || last[1] != last[2] {
		t.Errorf("last progress = %v, want step 2 with every byte of the possible duplicates read", last)
	}
	recs := records(t, cat, "A")
	want := []string{"Project/clip.mp4", "Project/copy.mp4", "Project/mid1.mov", "Project/mid2.mov", "Project/other.mp4", "notes.txt"}
	var got []string
	for p := range recs {
		got = append(got, relSlash(a, p))
	}
	if slices.Sort(got); !slices.Equal(got, want) {
		t.Fatalf("inventory = %v, want %v", got, want)
	}
	r := recs[clip]
	if r.Codec != "mpeg4" || r.W != 320 || r.H != 240 || r.Dur < 1.9 || r.FPS != "25/1" || r.Hash == "" {
		t.Errorf("clip.mp4 probe = %+v", r)
	}
	if recs[filepath.Join(a, "notes.txt")].Hash != "" {
		t.Error("notes.txt should not be hashed")
	}
	if recs[filepath.Join(a, "Project", "mid1.mov")].Hash != recs[filepath.Join(a, "Project", "mid2.mov")].Hash {
		t.Error("mid1 and mid2 should share the sampled hash")
	}
	checkFullHash(t, cat, "A", a, "Project/clip.mp4", "Project/copy.mp4", "Project/mid1.mov", "Project/mid2.mov")

	scanned(t, cat, b, "B", "LaCie 4TB Milan", nil)
	checkFullHash(t, cat, "B", b, "Copy/clip.mp4")
	// The heavy work chosen for a drive survives scanning it again; not chosen (nil) means on.
	off := false
	(&App{dir: cat}).editDevice("B", b, func(d *Device) { d.Content = &off })
	scanned(t, cat, b, "B", "", nil)
	if d := device(t, cat, "B"); d.Content == nil || *d.Content || on(d.Copies) {
		t.Errorf("options after a new scan: content %v, copies %v", d.Content, d.Copies)
	}
	if d := device(t, cat, "B"); d.Name != "LaCie 4TB Milan" || d.Mount != b || d.Excluded == nil {
		t.Errorf("device B = %+v", d)
	}

	// Rescan without arguments: same exclusions and name, nothing to hash again.
	first := device(t, cat, "A")
	scanned(t, cat, a, "A", "", nil)
	if d := device(t, cat, "A"); !slices.Equal(d.Excluded, []string{"skipme"}) || d.Name != first.Name {
		t.Errorf("rescan device A = %+v, first scan %+v", d, first)
	}
	checkFullHash(t, cat, "A", a, "Project/clip.mp4", "Project/copy.mp4", "Project/mid1.mov", "Project/mid2.mov")

	// A changed modification time makes the old full hash stale.
	later := time.Now().Add(time.Hour)
	must(t, os.Chtimes(filepath.Join(a, "Project", "copy.mp4"), later, later))
	scanned(t, cat, a, "A", "", nil)
	checkFullHash(t, cat, "A", a, "Project/clip.mp4", "Project/copy.mp4", "Project/copy.mp4", "Project/mid1.mov", "Project/mid2.mov")
}

// checkFullHash verifies fullhash.tsv lists exactly these files (relative to root), each with its
// real SHA-256, size and modification time at the time it was hashed.
func checkFullHash(t *testing.T, cat, id, root string, want ...string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(cat, "disks", id, "fullhash.tsv"))
	must(t, err)
	var got []string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		c := strings.Split(line, "\t")
		if len(c) != 4 {
			t.Fatalf("fullhash line %q", line)
		}
		got = append(got, relSlash(root, c[1]))
		data, err := os.ReadFile(c[1])
		must(t, err)
		st, err := os.Stat(c[1])
		must(t, err)
		if c[0] != fmt.Sprintf("%x", sha256.Sum256(data)) || c[2] != fmt.Sprint(st.Size()) {
			t.Errorf("fullhash line %q: wrong hash or size", line)
		}
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("fullhash %s = %v, want %v", id, got, want)
	}
}

func records(t *testing.T, cat, id string) map[string]Rec {
	t.Helper()
	m := map[string]Rec{}
	must(t, eachRec(filepath.Join(cat, "disks", id, "files.jsonl"), func(r Rec) { m[r.Path] = r }))
	return m
}

func device(t *testing.T, cat, id string) Device {
	t.Helper()
	var d Device
	b, err := os.ReadFile(filepath.Join(cat, "disks", id, "device.json"))
	must(t, err)
	must(t, json.Unmarshal(b, &d))
	return d
}

func ffmpeg(t *testing.T, src, out string) {
	t.Helper()
	if b, err := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", src, "-c:v", "mpeg4", out).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v %s", err, b)
	}
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	b, err := os.ReadFile(from)
	must(t, err)
	must(t, os.WriteFile(to, b, 0o644))
}

func scanned(t *testing.T, cat, vol, id, name string, excluded []string) {
	t.Helper()
	if _, err := scan(cat, vol, id, name, excluded); err != nil {
		t.Fatal(err)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// relSlash is p relative to base with "/", as the catalog's clip IDs are on every system.
func relSlash(base, p string) string {
	r, err := filepath.Rel(base, p)
	if err != nil {
		return p
	}
	return filepath.ToSlash(r)
}

// TestFsUUID: the UUIDs macOS showed (diskutil info) for an exFAT and a FAT32 image whose boot sectors held
// these serial numbers and total sectors, measured on macOS 26 (hdiutil create -fs ExFAT / "MS-DOS FAT32").
func TestFsUUID(t *testing.T) {
	for _, c := range []struct {
		serial  uint32
		sectors []uint32
		want    string
	}{
		{0x6AED8E38, nil, "63CF58A9-86DC-35C6-9129-1D68605E5F10"},
		{0x4A841100, []uint32{614400}, "72B6D32F-619C-33C8-A945-F73EC2344447"},
	} {
		if got := fsUUID(c.serial, c.sectors...); got != c.want {
			t.Errorf("fsUUID(%08X, %v) = %s, macOS says %s", c.serial, c.sectors, got, c.want)
		}
	}
}

// TestProbeRED: a RED clip read through rushbin-red (a stand-in here: the real one needs RED's SDK) is its
// _001 file plus the _002 … next to it, and nothing else in the folder.
func TestProbeRED(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in is a shell script")
	}
	bin, dir := t.TempDir(), t.TempDir()
	must(t, os.WriteFile(filepath.Join(bin, "rushbin-red"), []byte("#!/bin/sh\n"+
		`echo '{"frames":881,"fps":25.0,"width":6144,"height":3240,"timecode":"12:56:37:06"}'`+"\n"), 0o755))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for name, size := range map[string]int{"A004_C001_0224UB_001.R3D": 100, "A004_C001_0224UB_002.R3D": 50, "A004_C001_0224UB.RMD": 7,
		"A004_C002_022494_001.R3D": 1000, "A004_C001_0224UB_002.R3D.bak": 9} {
		must(t, os.WriteFile(filepath.Join(dir, name), make([]byte, size), 0o644))
	}
	r := Rec{Path: filepath.Join(dir, "A004_C001_0224UB_001.R3D"), Ext: ".r3d"}
	probeRED(&r)
	if r.Dur != 35.24 || r.W != 6144 || r.H != 3240 || r.FPS != "25000/1000" || r.Codec != "r3d" || r.Span != 150 {
		t.Fatalf("RED clip = %+v", r)
	}
	// Nikon: one file per clip, also when numbered files sit next to it.
	for _, name := range []string{"DSC_0001.R3D", "DSC_0002.R3D", "DSC_0001.NEV"} {
		must(t, os.WriteFile(filepath.Join(dir, name), make([]byte, 10), 0o644))
	}
	n := Rec{Path: filepath.Join(dir, "DSC_0001.R3D"), Ext: ".r3d"}
	probeRED(&n)
	if n.Dur != 35.24 || n.Span != 0 {
		t.Fatalf("Nikon clip = %+v", n)
	}
	for p, want := range map[string]bool{"A004_C001_0224UB_001.R3D": true, "A004_C001_0224UB_002.R3D": false, "a/A001_C001_0101AB_010.r3d": false,
		"DSC_0001.R3D": true, "DSC_0002.R3D": true, "DSC_0001.NEV": true, "clip.r3d": true, "A004_C001_0224UB.RMD": false, "x.mov": false} {
		if redClip(p) != want {
			t.Errorf("redClip(%s) = %v, want %v", p, !want, want)
		}
	}
}

// TestInventoryAgain: a scan again reads only what changed. An unchanged file keeps its record (here marked
// "kept", which no hash is), a changed one is read again, a removed one is gone, a new one is read.
func TestInventoryAgain(t *testing.T) {
	vol, out := t.TempDir(), filepath.Join(t.TempDir(), "files.jsonl")
	for _, n := range []string{"same.mov", "edited.mov", "gone.mov"} {
		must(t, os.WriteFile(filepath.Join(vol, n), []byte(n), 0o644))
	}
	if _, err := inventory(vol, out, nil); err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	must(t, eachRec(out, func(r Rec) { r.Hash = "kept"; json.NewEncoder(&b).Encode(r) }))
	must(t, os.WriteFile(out, b.Bytes(), 0o644))
	must(t, os.WriteFile(filepath.Join(vol, "edited.mov"), []byte("edited, longer"), 0o644))
	must(t, os.Remove(filepath.Join(vol, "gone.mov")))
	must(t, os.WriteFile(filepath.Join(vol, "new.mov"), []byte("new"), 0o644))
	if _, err := inventory(vol, out, nil); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	must(t, eachRec(out, func(r Rec) { got[filepath.Base(r.Path)] = r.Hash }))
	if len(got) != 3 || got["same.mov"] != "kept" || got["edited.mov"] == "kept" || got["edited.mov"] == "" || got["new.mov"] == "" {
		t.Fatalf("hashes after the second scan = %v", got)
	}
}

// TestInputRED: FFmpeg reads an R3D clip from rushbin-red (a stand-in here, as FFmpeg is): the frames from
// the start asked on pipe 3, the sound on pipe 4, or on pipe 3 alone when only the sound is wanted.
func TestInputRED(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in is a shell script")
	}
	bin := t.TempDir()
	log := filepath.Join(bin, "log")
	must(t, os.WriteFile(filepath.Join(bin, "rushbin-red"), []byte("#!/bin/sh\necho \"$*\" >> "+log+"\ncase $1 in\n"+
		`info) echo '{"frames":50,"fps":23.976,"width":6144,"height":3240,"timecode":"","audio":2}';;`+"\n"+
		`frames) printf 'P6 frames ';;`+"\n"+`audio) printf 'RIFF sound';;`+"\nesac\n"), 0o755))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	cl := &Clip{Path: "/r/A001_001.R3D", Ext: ".r3d"}
	for _, c := range []struct {
		video       bool
		args, reads string
		calls       string
	}{
		{true, "-framerate 23.976 -f ppm_pipe -i pipe:3 -f wav -i pipe:4 -map 0:v:0 -map 1:a:0", "P6 frames RIFF sound",
			"info /r/A001_001.R3D\nframes /r/A001_001.R3D 1.500\naudio /r/A001_001.R3D 1.500\n"},
		{false, "-f wav -i pipe:3 -map 0:a:0", "RIFF sound", "info /r/A001_001.R3D\naudio /r/A001_001.R3D 1.500\n"},
	} {
		os.Remove(log)
		args, feed := input(cl, 1.5, c.video, exec.Command)
		if got := strings.Join(args, " "); got != c.args {
			t.Fatalf("video %v: FFmpeg's input = %s", c.video, got)
		}
		ff := exec.Command("/bin/sh", "-c", "cat <&3; [ -e /dev/fd/4 ] && cat <&4") // FFmpeg reads its pipes
		done, err := feed(ff)
		must(t, err)
		out, _ := ff.Output()
		done()
		calls, _ := os.ReadFile(log)
		if string(out) != c.reads || string(calls) != c.calls {
			t.Fatalf("video %v: FFmpeg read %q, rushbin-red ran %q", c.video, out, calls)
		}
	}
}

// TestInventoryLinks: a link on a drive is not cataloged, so another drive's files cannot come in through it.
func TestInventoryLinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("links need an administrator on Windows")
	}
	vol, other, out := t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "files.jsonl")
	must(t, os.WriteFile(filepath.Join(other, "secret.mov"), []byte("x"), 0o644))
	must(t, os.WriteFile(filepath.Join(vol, "mine.mov"), []byte("y"), 0o644))
	must(t, os.Symlink(filepath.Join(other, "secret.mov"), filepath.Join(vol, "link.mov")))
	if _, err := inventory(vol, out, nil); err != nil {
		t.Fatal(err)
	}
	var got []string
	must(t, eachRec(out, func(r Rec) { got = append(got, filepath.Base(r.Path)) }))
	if len(got) != 1 || got[0] != "mine.mov" {
		t.Fatalf("inventory = %v, want only mine.mov", got)
	}
}

// TestInputProResRAW: on the Mac the pictures come from AVFoundation on pipe 3 and the sound from the file,
// with every -map after every input (FFmpeg refuses a -map between two inputs).
func TestInputProResRAW(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("AVFoundation decodes ProRes RAW on the Mac only")
	}
	fps := 25.0
	cl := &Clip{Path: "/x/a.mov", Ext: ".mov", Codec: "prores_raw", FPS: &fps}
	args, _ := input(cl, 2, true, exec.Command)
	if got, want := strings.Join(args, " "), "-framerate 25 -f ppm_pipe -i pipe:3 -ss 2.000 -i /x/a.mov -map 0:v:0 -map 1:a:0?"; got != want {
		t.Fatalf("FFmpeg's input = %s, want %s", got, want)
	}
}
