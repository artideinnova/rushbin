package main

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
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
	direct := noCache(tmp)
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(tmp, progress), io.TeeReader(in, h))
	if err == nil {
		err = tmp.Sync()
	}
	if err == nil {
		_, err = tmp.Seek(0, 0)
	}
	back := sha256.New()
	if err == nil {
		_, err = io.Copy(back, tmp)
	}
	if cerr := tmp.Close(); err == nil {
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

// addHashes appends lines to the fullhash.tsv of drive id (as fullHash and carryHashes do).
func (a *App) addHashes(id string, lines ...string) error {
	f, err := os.OpenFile(filepath.Join(a.dir, "disks", filepath.Base(id), "fullhash.tsv"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	for _, l := range lines {
		if l == "" {
			continue
		}
		if _, err := fmt.Fprintln(f, l); err != nil {
			f.Close()
			return err
		}
	}
	return f.Close()
}

// consolidate starts the copy of what the drive lacks of a project, after checking it fits; it runs in the
// background, one at a time, and the drive is read again at the end.
func (a *App) consolidate(w http.ResponseWriter, r *http.Request) {
	var b struct{ Project, To string }
	json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&b)
	todo, offline, err := consolidation(a.catalog(), b.Project, b.To)
	if err != nil {
		http.Error(w, err.Error(), 409)
		return
	}
	var need int64
	for _, c := range todo {
		need += c.Size
	}
	dst := a.catalog().device(b.To)
	if d, err := identifyVolume(dst.Mount); err == nil && need > d.Free {
		http.Error(w, "Not enough free space on the drive", 409)
		return
	}
	a.smu.Lock()
	busy := a.copyTo != ""
	if !busy && len(todo) > 0 {
		a.copyTo = dst.Mount
	}
	a.smu.Unlock()
	if busy {
		http.Error(w, "A project is already being copied", 409)
		return
	}
	if len(todo) > 0 {
		go a.runConsolidation(b.Project, dst.UUID, dst.Name, dst.Mount, todo)
	}
	send(w, map[string]any{"files": len(todo), "bytes": need, "offline": offline})
}

func (a *App) runConsolidation(project, id, drive, mount string, todo []consCopy) {
	done := a.reading(mount)
	defer done()
	say := func(s string) {
		a.smu.Lock()
		a.copyNote = s
		a.smu.Unlock()
	}
	var total, base int64 // the page estimates the time left from the bytes (one file can be 2 GB, the next 2 KB)
	for _, c := range todo {
		total += c.Size
	}
	a.copyDone.Store(0)
	a.copyTotal.Store(total)
	defer a.copyTotal.Store(0)
	copied, failed, why := 0, 0, ""
	for i, c := range todo {
		say(fmt.Sprintf("Copying %s to %s: %d of %d files", project, drive, i+1, len(todo)))
		t, _ := time.Parse(time.RFC3339, c.Mtime)
		stop := a.reading(c.From)
		orig, cp, err := copyChecked(c.From, c.To, t, counter{&a.copyDone})
		stop()
		if err == nil { // the check byte for byte is done: Duplicati does not read these two files again
			a.addHashes(c.Dev, orig)
			a.addHashes(id, cp)
		}
		base += c.Size
		a.copyDone.Store(base) // a file skipped or cut short counts as done: what is left is what the estimate needs
		if err != nil {
			failed++
			why = err.Error()
			if !fileExists(mount) { // the drive went: the rest would fail too
				failed += len(todo) - i - 1
				break
			}
			continue
		}
		copied++
	}
	a.smu.Lock()
	if failed > 0 {
		a.copyNote = fmt.Sprintf("%s on %s: %d files copied, %d not: %s", project, drive, copied, failed, why)
	} else {
		a.copyNote = fmt.Sprintf("%s copied to %s: %d files", project, drive, copied)
		if a.after == nil {
			a.after = map[string]func(){}
		}
		a.after[mount] = func() { a.keepProject(project, id, drive, copied) } // once the drive read shows the copies
	}
	a.copyTo = ""
	a.enqueue(mount) // once, now: during the copy its own writes did not ask for it (App.changed)
	a.smu.Unlock()
}

// keepProject: the whole project is now on drive id, so each of its clips keeps the copy there, as "Keep on" in
// Duplicati would choose; choices of another drive made before are replaced, and the note says how many.
func (a *App) keepProject(project, id, drive string, copied int) {
	c := a.catalog()
	var ls []DecisionLine
	changed, seen := 0, map[string]bool{}
	for _, cl := range c.list {
		if cl.Project != project || seen[cl.Content] {
			continue
		}
		seen[cl.Content] = true
		locs := c.locations(cl.Content) // shortest path first
		i := slices.IndexFunc(locs, func(l Location) bool { return l.Device == id })
		cur := c.decision(cl.Content).Copy
		if i < 0 || len(locs) < 2 || strings.HasPrefix(cur, id+"|") {
			continue
		}
		if cur != "" {
			changed++
		}
		ls = append(ls, DecisionLine{Content: cl.Content, Kind: "copy", Value: id + "|" + locs[i].ID})
	}
	var err error
	for len(ls) > 0 && err == nil { // the site takes up to 10000 at a time (readDecisions)
		n := min(len(ls), 10000)
		_, err = a.saveDecisions(ls[:n])
		ls = ls[n:]
	}
	note := fmt.Sprintf("%s copied to %s: %d files", project, drive, copied)
	if changed > 0 {
		note = fmt.Sprintf("%s copied to %s: %d files; %d clips that had another drive chosen now keep it", project, drive, copied, changed)
	}
	if err != nil {
		note = fmt.Sprintf("%s copied to %s: %d files; keeping it there for the whole project was not saved: %s", project, drive, copied, err)
	}
	a.smu.Lock()
	a.copyNote = note
	a.smu.Unlock()
}
