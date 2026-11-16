package main

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Consolidating a project: the drive chosen gets every file of the project that only other drives have, at the same
// place inside its own folder of the project. Nothing is moved or deleted: once that drive is read again the files on
// the other drives are copies, and Duplicati takes them to the trash after the check byte for byte.

type consCopy struct {
	From, To string
	Dev      string // the drive of From
	Size     int64
	Mtime    string
}

// consolidation: what drive to lacks of project and where each file goes there (inside the project's folder that
// holds most of it there, else one named as the project); offline counts what only drives not connected have.
// A clip is the same by its content, any other file by its name and size, as in the proposal (Overview).
func consolidation(c *Catalog, project, to string) (todo []consCopy, offline int, err error) {
	dst := c.device(to)
	if dst.Mount == "" || !dst.Online {
		return nil, 0, errors.New("Drive not connected")
	}
	clipOf, tops := map[string]string{}, map[string]string{} // path → content; drive|top folder → project
	for _, cl := range c.list {
		clipOf[cl.Path] = cl.Content
		tops[cl.Device+"|"+part(cl.ID, 0)] = cl.Project
	}
	key := func(p string, r Rec) string {
		return cmp.Or(clipOf[p], "n:"+filepath.Base(p)+"|"+strconv.FormatInt(r.Size, 10))
	}
	type src struct {
		d    *DeviceInfo
		rest string // inside the project's folder
		p    string
		r    Rec
	}
	have, held, srcs := map[string]bool{}, map[string]int64{}, []src{}
	for p, r := range c.files {
		for _, d := range c.Devices {
			rel, e := filepath.Rel(d.Mount, p)
			if d.Mount == "" || e != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				continue
			}
			top, rest, in := strings.Cut(filepath.ToSlash(rel), "/")
			if in && tops[d.UUID+"|"+top] == project {
				if d.UUID == to {
					have[key(p, r)], held[top] = true, held[top]+r.Size
				} else {
					srcs = append(srcs, src{d, rest, p, r})
				}
			}
			break
		}
	}
	dir, most := project, int64(-1)
	for top, n := range held {
		if n > most || n == most && top < dir {
			dir, most = top, n
		}
	}
	slices.SortFunc(srcs, func(a, b src) int { // connected drives first: a file both have comes from one connected
		if a.d.Online != b.d.Online {
			return map[bool]int{true: -1, false: 1}[a.d.Online]
		}
		return strings.Compare(a.d.Name+"/"+a.rest, b.d.Name+"/"+b.rest)
	})
	seen := map[string]bool{}
	for _, s := range srcs {
		k := key(s.p, s.r)
		if have[k] || seen[k] {
			continue
		}
		seen[k] = true
		if !s.d.Online {
			offline++
			continue
		}
		todo = append(todo, consCopy{Dev: s.d.UUID, From: s.p, To: filepath.Join(dst.Mount, dir, filepath.FromSlash(s.rest)), Size: s.r.Size, Mtime: s.r.Mtime})
	}
	return todo, offline, nil
}

var errExists = errors.New("Something is already there")

// copyChecked copies from to to through a hidden file next to it, then reads the copy back: it takes its name only
// when it holds the same bytes. The original's time is kept, as Finder does. Something already at to stays. Each
// byte written also goes to progress.
// That is the check byte for byte Duplicati asks for, done once: it returns the lines of fullhash.tsv for the
// original, read whole, and for the copy, when it was read back from the drive and not from the system's memory
// (noCache), "" otherwise: there Duplicati reads it again.
// ponytail: the drive's own cache may answer the read back; reading it again a day later is the stronger check.
func copyChecked(from, to string, mtime time.Time, progress io.Writer) (orig, cp string, err error) {
	if _, err := os.Lstat(to); err == nil {
		return "", "", errExists
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return "", "", err
	}
	in, err := os.Open(from)
	if err != nil {
		return "", "", err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(to), "."+filepath.Base(to)+"-rushbin-*")
	if err != nil {
		return "", "", err
	}
	defer os.Remove(tmp.Name())
	noCache(tmp) // what is written does not stay in the system's memory either
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(tmp, progress), io.TeeReader(in, h))
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", "", err
	}
	r, direct, err := openFromDrive(tmp.Name())
	if err != nil {
		return "", "", err
	}
	back := sha256.New()
	_, err = io.Copy(back, r)
	if cerr := r.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", "", err
	}
	sum := h.Sum(nil)
	if !bytes.Equal(sum, back.Sum(nil)) {
		return "", "", errors.New("the copy differs from the original")
	}
	if !mtime.IsZero() {
		os.Chtimes(tmp.Name(), mtime, mtime)
	}
	if _, err := os.Lstat(to); err == nil { // arrived meanwhile
		return "", "", errExists
	}
	if err := os.Rename(tmp.Name(), to); err != nil {
		return "", "", err
	}
	line := func(p string) string { // size and time as the drive keeps them: exFAT rounds the time
		st, err := os.Stat(p)
		if err != nil {
			return ""
		}
		return fmt.Sprintf("%x\t%s", sum, stamp(p, st.Size(), st.ModTime().Unix()))
	}
	orig = line(from)
	if direct {
		cp = line(to)
	}
	return orig, cp, nil
}

// addHashes appends lines to the fullhash.tsv of drive id.
func (a *App) addHashes(id string, lines ...string) error {
	return appendLines(filepath.Join(a.dir, "disks", filepath.Base(id), "fullhash.tsv"), lines...)
}

// moveJob is the consolidation going on, or the last one until the page lets it go. It is kept in move.json, so a
// pause, a drive unplugged or the app closed lose nothing: Made lists what this move wrote, and only that is what
// Cancel takes away again.
type moveJob struct {
	Project string         `json:"project"`
	To      string         `json:"to"`    // the drive's ID
	Drive   string         `json:"drive"` // its name
	Backup  bool           `json:"backup"`
	State   string         `json:"state"` // running, paused, waiting (drive gone or app closed), done, cancelled
	Files   int            `json:"files"`
	Bytes   int64          `json:"bytes"`
	Copied  int            `json:"copied"`
	Failed  int            `json:"failed"`
	Why     string         `json:"why,omitempty"`
	Made    []madeFile     `json:"made"`
	Dirs    []string       `json:"dirs,omitempty"`    // folders it made, each after the one holding it
	Before  map[string]int `json:"before,omitempty"`  // clips that had another drive chosen, by drive name (keepProject)
	Kept    bool           `json:"kept"`              // each clip of the project now keeps its copy on To
	Changed int            `json:"changed,omitempty"` // Cancel: files changed since written, left where they are
}

type madeFile struct {
	Path  string `json:"path"`
	Size  int64  `json:"size"`
	Mtime int64  `json:"mtime"`
}

const moveFile = "move.json"

// saveMove writes a.move down. Callers hold smu.
func (a *App) saveMove() {
	if a.move == nil {
		os.Remove(filepath.Join(a.dir, moveFile))
		return
	}
	j, _ := json.Marshal(a.move)
	if err := writeFile(filepath.Join(a.dir, moveFile), j); err != nil {
		log.Println("move.json:", err)
	}
}

// loadMove takes up the move the app had when it closed: one still running then waits to be resumed or cancelled.
func (a *App) loadMove() {
	var m moveJob
	if b, err := os.ReadFile(filepath.Join(a.dir, moveFile)); err != nil || json.Unmarshal(b, &m) != nil {
		return
	}
	if m.State == "running" {
		m.State, m.Why = "waiting", "closed"
	}
	a.move = &m
}

var errStopped = errors.New("stopped")

// stopper counts the bytes written and stops the copy when the page paused or cancelled the move.
type stopper struct {
	n   *atomic.Int64
	ctl *atomic.Int32 // 0 go on, 1 pause, 2 cancel
}

func (s stopper) Write(b []byte) (int, error) {
	if s.ctl.Load() != 0 {
		return 0, errStopped
	}
	s.n.Add(int64(len(b)))
	return len(b), nil
}

// consolidate: {"project", "to", "backup", "dry"}. Dry answers what the move would copy and free, for the page to
// ask; otherwise it starts it, one at a time, after checking it fits. Backup keeps the copies on the other drives
// as the project's backup (the switch in Duplicati).
func (a *App) consolidate(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Project, To string
		Backup, Dry bool
	}
	json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&b)
	c := a.catalog()
	todo, offline, err := consolidation(c, b.Project, b.To)
	if err != nil {
		http.Error(w, err.Error(), 409)
		return
	}
	var need int64
	for _, t := range todo {
		need += t.Size
	}
	dst, free := c.device(b.To), int64(-1)
	if d, err := identifyVolume(dst.Mount); err == nil {
		free = d.Free
	}
	out := map[string]any{"files": len(todo), "bytes": need, "offline": offline, "free": free, "frees": frees(c, b.Project, b.To, b.Backup)}
	if b.Dry {
		send(w, out)
		return
	}
	if free >= 0 && need > free {
		http.Error(w, "Not enough free space on the drive", 409)
		return
	}
	if len(todo) == 0 {
		http.Error(w, "Nothing of the project is missing on that drive", 409)
		return
	}
	if err := a.saveBackup(b.Project, b.Backup); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	a.smu.Lock()
	if a.copyTo != "" || a.move != nil && (a.move.State == "paused" || a.move.State == "waiting") {
		a.smu.Unlock()
		http.Error(w, "A project is already being consolidated", 409)
		return
	}
	a.move = &moveJob{Project: b.Project, To: dst.UUID, Drive: dst.Name, Backup: b.Backup, State: "running", Files: len(todo), Bytes: need}
	a.copyTo = dst.Mount
	a.moveCtl.Store(0)
	a.saveMove()
	a.smu.Unlock()
	go a.runMove(todo)
	send(w, out)
}

// frees: what the other drives would free once the project is on drive to: the copies elsewhere of each clip that
// is there, or will be (it has a copy on a connected drive), less the backup when the project keeps one.
func frees(c *Catalog, project, to string, backup bool) int64 {
	var n int64
	seen := map[string]bool{}
	for _, cl := range c.list {
		if cl.Project != project || seen[cl.Content] {
			continue
		}
		seen[cl.Content] = true
		others, reaches := 0, false
		for _, l := range c.locations(cl.Content) {
			if l.Device != to {
				others++
			}
			reaches = reaches || l.Device == to || l.Online
		}
		if !reaches {
			continue
		}
		if backup && others > 0 {
			others--
		}
		n += int64(others) * cl.Size
	}
	return n
}

// moveControl: "pause", "resume", "cancel" the move, or "dismiss" the last one once done or cancelled.
func (a *App) moveControl(w http.ResponseWriter, r *http.Request) {
	var err error
	switch r.PathValue("do") {
	case "pause":
		a.moveCtl.Store(1)
	case "resume":
		var todo []consCopy
		if todo, err = a.resumeMove(); err == nil {
			go a.runMove(todo)
		}
	case "cancel":
		a.smu.Lock()
		running := a.copyTo != ""
		a.smu.Unlock()
		if running {
			a.moveCtl.Store(2) // runMove cancels once the file being copied stops
		} else {
			err = a.cancelMove()
		}
	case "dismiss":
		a.smu.Lock()
		if a.move != nil && (a.move.State == "done" || a.move.State == "cancelled") {
			a.move = nil
			a.saveMove()
		}
		a.smu.Unlock()
	default:
		http.Error(w, "unknown", 404)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), 409)
		return
	}
	send(w, map[string]bool{"ok": true})
}

// resumeMove gets a paused or waiting move going again: what is still missing, less what this move already wrote
// (the drive has not been read since).
func (a *App) resumeMove() ([]consCopy, error) {
	a.smu.Lock()
	defer a.smu.Unlock()
	m := a.move
	if m == nil || a.copyTo != "" || m.State != "paused" && m.State != "waiting" {
		return nil, errors.New("Nothing to resume")
	}
	todo, _, err := consolidation(a.catalog(), m.Project, m.To)
	if err != nil {
		return nil, err
	}
	made := map[string]bool{}
	for _, f := range m.Made {
		made[f.Path] = true
	}
	todo = slices.DeleteFunc(todo, func(c consCopy) bool { return made[c.To] && fileExists(c.To) })
	m.State, m.Why, m.Files, m.Bytes = "running", "", m.Copied+len(todo), 0
	for _, f := range m.Made {
		m.Bytes += f.Size
	}
	for _, c := range todo {
		m.Bytes += c.Size
	}
	a.copyTo = a.catalog().device(m.To).Mount
	a.moveCtl.Store(0)
	a.saveMove()
	return todo, nil
}

func (a *App) runMove(todo []consCopy) {
	a.smu.Lock()
	m, mount := a.move, a.copyTo
	a.smu.Unlock()
	defer a.reading(mount)()
	var total, base int64 // the page estimates the time left from the bytes (one file can be 2 GB, the next 2 KB)
	dirs := map[string]bool{}
	for _, c := range todo {
		total += c.Size
		dirs[filepath.Dir(c.To)] = true
	}
	cleanTmp(dirs) // what a copy cut short by the app closing left
	a.copyDone.Store(0)
	a.copyTotal.Store(total)
	stop := stopper{&a.copyDone, &a.moveCtl}
	for _, c := range todo {
		if a.moveCtl.Load() != 0 {
			break
		}
		made := missingDirs(filepath.Dir(c.To))
		t, _ := time.Parse(time.RFC3339, c.Mtime)
		done := a.reading(c.From)
		orig, cp, err := copyChecked(c.From, c.To, t, stop)
		done()
		base += c.Size
		a.copyDone.Store(base) // a file skipped counts as done: what is left is what the estimate needs
		a.smu.Lock()
		for _, d := range made {
			if dirExists(d) {
				m.Dirs = append(m.Dirs, d)
			}
		}
		gone := false
		switch {
		case err == nil:
			if st, e := os.Stat(c.To); e == nil {
				m.Made = append(m.Made, madeFile{c.To, st.Size(), st.ModTime().Unix()})
			}
			m.Copied++
		case errors.Is(err, errStopped):
		case !dirExists(mount): // the drive went: the rest would fail too
			m.State, m.Why, gone = "waiting", "drive", true
		default:
			m.Failed++
			m.Why = err.Error()
		}
		a.saveMove()
		a.smu.Unlock()
		if err == nil { // the check byte for byte is done: Duplicati does not read these two files again
			a.addHashes(c.Dev, orig)
			a.addHashes(m.To, cp)
		}
		if gone {
			break
		}
	}
	ctl := a.moveCtl.Load()
	a.smu.Lock()
	switch {
	case m.State == "waiting":
	case ctl == 1:
		m.State = "paused"
	case ctl == 2:
	default:
		m.State = "done"
		if m.Failed == 0 {
			if a.after == nil {
				a.after = map[string]func(){}
			}
			a.after[mount] = func() { a.keepProject(m.Project, m.To) } // once the drive read shows the copies
		}
	}
	a.copyTo = ""
	a.copyTotal.Store(0)
	if ctl != 2 && m.State != "waiting" {
		a.enqueue(mount) // once, now: during the copy its own writes did not ask for it (App.changed)
	}
	a.saveMove()
	a.smu.Unlock()
	if ctl == 2 {
		if err := a.cancelMove(); err != nil {
			log.Println("cancel move:", err)
		}
	}
}

// cancelMove takes away what the move wrote, each file only if it is still as written, the hidden ones a copy cut
// short left, and the folders it made once empty: the drive is as before, nothing else is touched.
func (a *App) cancelMove() error {
	a.smu.Lock()
	defer a.smu.Unlock()
	m := a.move
	if m == nil || m.State == "done" || m.State == "cancelled" {
		return errors.New("Nothing to cancel")
	}
	d := a.catalog().device(m.To)
	if !d.Online || !dirExists(d.Mount) {
		return errors.New("Connect the drive to cancel")
	}
	dirs := map[string]bool{}
	for _, f := range m.Made {
		dirs[filepath.Dir(f.Path)] = true
		if st, err := os.Lstat(f.Path); err == nil && st.Mode().IsRegular() && st.Size() == f.Size && st.ModTime().Unix() == f.Mtime {
			os.Remove(f.Path)
		} else if err == nil {
			m.Changed++
		}
	}
	for _, dir := range m.Dirs {
		dirs[dir] = true
	}
	cleanTmp(dirs)
	for i := len(m.Dirs) - 1; i >= 0; i-- {
		os.Remove(m.Dirs[i]) // only an empty folder goes
	}
	m.State, m.Why = "cancelled", ""
	a.saveMove()
	a.enqueue(d.Mount)
	return nil
}

// missingDirs lists the folders dir needs that are not there yet, each after the one holding it.
func missingDirs(dir string) (out []string) {
	for d := dir; !dirExists(d) && filepath.Dir(d) != d; d = filepath.Dir(d) {
		out = append([]string{d}, out...)
	}
	return out
}

// cleanTmp removes in dirs the hidden files copyChecked writes into before naming them: left only by a copy
// cut short when the app closed.
func cleanTmp(dirs map[string]bool) {
	for dir := range dirs {
		ps, _ := filepath.Glob(filepath.Join(dir, ".*-rushbin-*"))
		for _, p := range ps {
			if st, err := os.Lstat(p); err == nil && st.Mode().IsRegular() {
				os.Remove(p)
			}
		}
	}
}

// keepProject: the whole project is now on drive id, so each of its clips keeps the copy there, as "Project drive"
// in Duplicati would choose; choices of another drive made before are replaced, and the move says how many.
func (a *App) keepProject(project, id string) {
	c := a.catalog()
	var ls []DecisionLine
	seen, before := map[string]bool{}, map[string]int{} // the drives chosen before, and for how many clips
	for _, cl := range c.list {
		if cl.Project != project || seen[cl.Content] {
			continue
		}
		seen[cl.Content] = true
		locs := c.locations(cl.Content)
		i := slices.IndexFunc(locs, func(l Location) bool { return l.Device == id })
		cur := c.decision(cl.Content).Copy
		if i < 0 || len(locs) < 2 || strings.HasPrefix(cur, id+"|") {
			continue
		}
		if cur != "" {
			before[c.device(strings.SplitN(cur, "|", 2)[0]).Name]++
		}
		ls = append(ls, DecisionLine{Content: cl.Content, Kind: "copy", Value: id + "|" + locs[i].ID})
	}
	var err error
	for len(ls) > 0 && err == nil { // the site takes up to 10000 at a time (readDecisions)
		n := min(len(ls), 10000)
		_, err = a.saveDecisions(ls[:n])
		ls = ls[n:]
	}
	a.smu.Lock()
	if m := a.move; m != nil && m.Project == project && m.To == id {
		m.Before, m.Kept = before, err == nil
		if err != nil {
			m.Why = err.Error()
		}
		a.saveMove()
	}
	a.smu.Unlock()
}
