package main

import (
	"bufio"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web
var webFS embed.FS

const trash = "_rushbin_trash"

// App is the engine behind the Mac app: it watches for drives, scans them one at a time and serves the
// interface on 127.0.0.1 to the app window only (random port, secret cookie).
type App struct {
	dir, token, volumes string

	mu  sync.RWMutex // guards cat
	cat *Catalog

	smu        sync.Mutex        // guards the fields below
	mounted    map[string]string // drive ID → mount path, connected now
	seen       map[string]string // mount path → drive ID ("" for volumes we never catalog)
	pending    []Pending
	ignored    map[string]bool
	status     string
	background string // long work that does not stop the app, like the proxies for the team
	signedOut  bool   // the team site no longer accepts this computer's token: the page asks to sign in again
	dlDone     int64  // bytes of the model being downloaded so far (ensureModels)
	dlErr      string // why the last model download failed, "" when it did not
	unreadable []Pending
	failed     map[string]bool // light copies that failed in this session: not tried again until restart (scanner only)
	shareNow   chan struct{}   // a drive changed (name, place, options, gone): the scanner sends it to the team // drives this computer cannot identify, so cannot catalog: said, not skipped silently
	step       int             // 1 reading, 2 duplicates, 3 previews, 4 analysis; 0 when idle
	done       int64           // how far the step is: files, bytes or clips
	total      int64           // 0 when not known in advance
	queue      chan string
	queued     map[string]bool // mount path → a scan of it waits in queue

	wmu    sync.Mutex // serializes decision writes and trash moves
	lastID int64      // the team's last decision this Mac has (guarded by wmu)
	unsent int        // transcripts not sent to the team yet (scanner goroutine only)
	team   *Team      // the team site this Mac sends to, nil when alone (guarded by smu)

	lives *lives
	tidy  map[string][]TidyMove // the last AI tidy plan per drive, applied by index (guarded by smu)

	release *Release // the latest release, nil before asking (guarded by smu)
	relFrom string   // the site that said so: the team's, or artideSite (guarded by smu)

	tmu  sync.Mutex                        // guards tenc
	tenc *textEncoder                      // the text half of SigLIP 2, started by the first search alone
	text func(q string) ([]float32, error) // turns a search into a SigLIP 2 vector alone; nil: embedLocal (tests fake it)

	cond conditions // what the computer is doing now, from the Mac app or Windows (guarded by smu)
}

// conditions pause the heavy work: on battery, in Low Power Mode, when the computer runs hot (all of it),
// on a metered connection like a phone hotspot (the copies for the team).
type conditions struct{ Battery, LowPower, Hot, Metered bool }

// pause says why the heavy work waits now, "" when it can go; upload: it also sends to the team.
func (a *App) pause(upload bool) string {
	a.smu.Lock()
	c := a.cond
	a.smu.Unlock()
	switch {
	case c.Battery:
		return "on battery"
	case c.LowPower:
		return "Low Power Mode is on"
	case c.Hot:
		return "the computer is hot"
	case upload && c.Metered:
		return "on a metered connection"
	}
	return ""
}

// readConditions takes "conditions battery=1 lowpower=0 hot=0 metered=0" and "changed <path>" lines from the
// Mac app until it quits (its end of stdin closes).
func (a *App) readConditions(r io.Reader) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		if p, ok := strings.CutPrefix(sc.Text(), "changed "); ok {
			a.changed(p)
			continue
		}
		f := strings.Fields(sc.Text())
		if len(f) == 0 || f[0] != "conditions" {
			continue
		}
		var c conditions
		for _, kv := range f[1:] {
			k, v, _ := strings.Cut(kv, "=")
			set := v == "1"
			switch k {
			case "battery":
				c.Battery = set
			case "lowpower":
				c.LowPower = set
			case "hot":
				c.Hot = set
			case "metered":
				c.Metered = set
			}
		}
		a.smu.Lock()
		a.cond = c
		a.smu.Unlock()
	}
}

// Pending is a drive connected for the first time, waiting for "Catalog" or "Ignore".
type Pending struct {
	UUID  string `json:"uuid"`
	Name  string `json:"name"`
	Mount string `json:"mount"`
	Size  int64  `json:"size"`
	Free  int64  `json:"free"`
	FS    string `json:"fs,omitempty"`
}

func runApp(dir, addr string) error {
	a := &App{dir: dir, volumes: "/Volumes", mounted: map[string]string{}, seen: map[string]string{},
		ignored: map[string]bool{}, queue: make(chan string, 64), shareNow: make(chan struct{}, 1)}
	if err := os.MkdirAll(filepath.Join(dir, "thumbs"), 0o755); err != nil {
		return err
	}
	if b, err := os.ReadFile(filepath.Join(dir, "ignored.json")); err == nil {
		json.Unmarshal(b, &a.ignored)
	}
	cache, _ := os.UserCacheDir()
	a.lives = newLives(filepath.Join(cache, "rushbin", "live"))
	k := make([]byte, 32)
	rand.Read(k)
	a.token = hex.EncodeToString(k)
	a.loadTeam()
	if err := a.reload(); err != nil {
		return err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	go a.scanner()
	go a.watch()
	go a.teamLoop()
	go a.originalsLoop()
	page := fmt.Sprintf("http://%s/?k=%s", ln.Addr(), a.token)
	if runtime.GOOS == "windows" {
		// No app window on Windows: the page opens in the browser, and the console window is the app.
		fmt.Println("rushbin is running. Keep this window open; close it to stop rushbin.")
		openBrowser(page)
		go a.watchPower()
		go a.updateLoop()
	} else {
		// The Mac app updates itself, from the feed it reads here after LISTEN or before it.
		go a.feedLoop(os.Stdout)
		go func() { a.readConditions(os.Stdin); os.Exit(0) }() // the Mac app quit: the engine stops with it
		fmt.Printf("LISTEN %s\n", page)
	}
	return http.Serve(ln, a.guard(a.routes()))
}

func (a *App) routes() http.Handler {
	m := http.NewServeMux()
	web, _ := fs.Sub(webFS, "web")
	m.Handle("GET /", http.FileServerFS(web))
	m.Handle("GET /thumbs/", http.StripPrefix("/thumbs/", http.FileServer(http.Dir(filepath.Join(a.dir, "thumbs")))))
	m.HandleFunc("GET /api/overview", func(w http.ResponseWriter, r *http.Request) { send(w, a.catalog().Overview()) })
	m.HandleFunc("GET /api/clips", func(w http.ResponseWriter, r *http.Request) { serveClips(w, r, a.catalog()) })
	m.HandleFunc("GET /api/clip", func(w http.ResponseWriter, r *http.Request) { serveClip(w, r, a.catalog()) })
	m.HandleFunc("GET /api/dups", func(w http.ResponseWriter, r *http.Request) { send(w, a.catalog().Dups()) })
	m.HandleFunc("GET /api/status", a.statusHandler)
	m.HandleFunc("GET /api/plan", func(w http.ResponseWriter, r *http.Request) { send(w, a.plan()) })
	m.HandleFunc("GET /api/play", a.play)
	m.HandleFunc("GET /live/{id}/{file}", a.serveLive)
	m.HandleFunc("POST /api/decide", a.decide)
	m.HandleFunc("POST /api/apply", a.apply)
	m.HandleFunc("POST /api/reveal", a.reveal)
	m.HandleFunc("POST /api/drives", a.drives)
	m.HandleFunc("GET /api/folders", a.folders)
	m.HandleFunc("GET /api/me", a.me)
	m.HandleFunc("POST /api/updates", a.setUpdates)
	m.HandleFunc("POST /api/team/connect", a.connect)
	m.HandleFunc("POST /api/team/disconnect", a.disconnect)
	m.HandleFunc("GET /api/proxy/{thumb}", a.proxyPlay)
	m.HandleFunc("GET /api/search", a.search)
	m.HandleFunc("GET /api/compare", func(w http.ResponseWriter, r *http.Request) { serveCompare(w, r, a.catalog()) })
	m.HandleFunc("POST /api/original", a.viaTeam)
	m.HandleFunc("GET /api/original/{id}", a.viaTeam)
	a.tidyRoutes(m)
	return m
}

// guard: only the app window may talk to the engine. The Host check stops DNS rebinding, the secret cookie
// stops other programs and web pages, and every write needs a custom header (no cross-site form can send it).
func (a *App) guard(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if host, _, _ := net.SplitHostPort(r.Host); host != "127.0.0.1" && host != "localhost" {
			http.Error(w, "host not allowed", 403)
			return
		}
		if k := r.URL.Query().Get("k"); r.URL.Path == "/" && k != "" {
			if subtle.ConstantTimeCompare([]byte(k), []byte(a.token)) != 1 {
				http.Error(w, "wrong key", 403)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "rb", Value: a.token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		ck, err := r.Cookie("rb")
		live := strings.HasPrefix(r.URL.Path, "/live/") // keyed by its random session ID (live.go)
		if !live && (err != nil || subtle.ConstantTimeCompare([]byte(ck.Value), []byte(a.token)) != 1) {
			http.Error(w, "open rushbin from its app", 403)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && (r.Header.Get("X-Rushbin") != "1" || r.Header.Get("Origin") != "" && r.Header.Get("Origin") != "http://"+r.Host) {
			http.Error(w, "only from the rushbin window", 403)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; media-src 'self' https:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		h.ServeHTTP(w, r)
	})
}

func send(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func (a *App) catalog() *Catalog {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.cat
}

func (a *App) reload() error {
	a.smu.Lock()
	online := map[string]bool{}
	for id := range a.mounted {
		online[id] = true
	}
	a.smu.Unlock()
	c, err := loadCatalog(a.dir, online)
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.cat = c
	a.mu.Unlock()
	return nil
}

// serveClips and serveClip answer the archive pages, in the app and on the team site.
func serveClips(w http.ResponseWriter, r *http.Request, c *Catalog) {
	q := r.URL.Query()
	send(w, c.Clips(Query{Words: strings.Fields(q.Get("q")), Project: q.Get("project"), Device: q.Get("device"),
		Camera: q.Get("camera"), Folder: q.Get("folder"), Label: q.Get("label"), Flags: q["flag"], Sort: q.Get("sort"), All: q.Get("all") == "1"}))
}

func direct(cl *Clip) bool {
	return (cl.Ext == ".mp4" || cl.Ext == ".mov" || cl.Ext == ".m4v") && (cl.Codec == "h264" || cl.Codec == "hevc")
}

func serveClip(w http.ResponseWriter, r *http.Request, c *Catalog) {
	cl := c.byKey[r.URL.Query().Get("device")+"|"+r.URL.Query().Get("id")]
	if cl == nil {
		w.WriteHeader(404)
		send(w, map[string]string{"error": "This clip is no longer in the catalog. Connect its drive to scan it again."})
		return
	}
	g := c.group(cl.Content)
	d := c.device(cl.Device)
	send(w, struct {
		*Clip
		Decision
		Disk      string     `json:"disk"`
		Mount     string     `json:"mount"`
		Direct    bool       `json:"direct"`
		Verified  bool       `json:"verified"`
		Proxy     bool       `json:"proxy"`
		Speech    *Speech    `json:"speech,omitempty"`
		Locations []Location `json:"locations"`
		Removed   bool       `json:"removed,omitempty"` // its data was removed on request (on the site)
	}{cl, g.Decision, d.Name, d.Mount, direct(cl), g.Verified, c.proxies[cl.Thumb], c.speechOf(cl.Thumb), g.Locations, c.forgotten[cl.Thumb]})
}

// shared: the decision is about what the team site has, a clip on a drive sent to the team (and, for the copy
// to keep, that copy), so it may go there. A drive kept on this computer (Local) sends nothing, not even its paths.
func (c *Catalog) shared(l DecisionLine) bool {
	if l.Kind == "copy" {
		cl := c.byKey[l.Value]
		return cl != nil && !on(c.device(cl.Device).Local)
	}
	return slices.ContainsFunc(c.byCont[l.Content], func(cl *Clip) bool { return !on(c.device(cl.Device).Local) })
}

// valid checks a decision against the catalog: a known content, a known vote, a copy that exists, lines of its
// transcript.
func (c *Catalog) valid(l DecisionLine) bool {
	cs := c.byCont[l.Content]
	switch l.Kind {
	case "vote":
		return len(cs) > 0 && slices.Contains([]string{"", "keep", "discard", "showreel"}, l.Value)
	case "copy":
		return slices.ContainsFunc(cs, func(cl *Clip) bool { return cl.Device+"|"+cl.ID == l.Value })
	case "check":
		return len(cs) > 0 && c.speechOf(cs[0].Thumb) != nil && (l.Value == "" || l.Value == "yes")
	case "hide": // each start is a line of the transcript
		h, ok := starts(l.Value)
		if !ok || len(cs) == 0 || c.speechOf(cs[0].Thumb) == nil {
			return false
		}
		segs := c.speechOf(cs[0].Thumb).Segs
		for _, s := range h {
			if !slices.ContainsFunc(segs, func(g Segment) bool { return g.S == s }) {
				return false
			}
		}
		return true
	}
	return false
}

// decide records a vote (keep, discard, showreel or "" to clear) or the copy to keep. In a team the site
// keeps it first, with who took it; decisions.jsonl is then the Mac's copy of the team's decisions. A decision
// about clips on drives kept on this computer stays here, like one taken alone.
func (a *App) decide(w http.ResponseWriter, r *http.Request) {
	var l DecisionLine
	if json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&l) != nil || !a.catalog().valid(l) {
		http.Error(w, "invalid decision", 400)
		return
	}
	a.wmu.Lock()
	defer a.wmu.Unlock()
	if t := a.teamConf(); t != nil && a.catalog().shared(l) {
		if err := t.call("POST", "/api/sync/decide", DecisionLine{Content: l.Content, Kind: l.Kind, Value: l.Value}, &l); err != nil {
			http.Error(w, "rushbin cannot reach the team site, the decision was not saved: "+err.Error(), 502)
			return
		}
	} else {
		l = DecisionLine{T: time.Now().UTC().Format(time.RFC3339Nano), Content: l.Content, Kind: l.Kind, Value: l.Value}
	}
	if err := a.record(l); err != nil {
		http.Error(w, "could not save the decision: "+err.Error(), 500)
		return
	}
	send(w, l)
}

// record appends a decision to decisions.jsonl and shows it. Callers hold wmu.
func (a *App) record(l DecisionLine) error {
	line, _ := json.Marshal(l)
	f, err := os.OpenFile(filepath.Join(a.dir, "decisions.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(append(line, '\n'))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		a.catalog().decide(l)
		a.lastID = max(a.lastID, l.ID)
	}
	return err
}

// Move is one file the trash would receive, recomputed from the catalog and the drives every time:
// no list of paths ever comes from the page.
type Move struct {
	Content string `json:"content"`
	Name    string `json:"name"`
	Disk    string `json:"disk"`
	Reason  string `json:"reason"` // copy | discarded
	From    string `json:"from"`
	To      string `json:"to"`
	Size    int64  `json:"size"`
	OK      bool   `json:"ok"`
	Why     string `json:"why,omitempty"`
	device  string
}

func (a *App) plan() []Move {
	c := a.catalog()
	ms := []Move{}
	for _, cl := range c.list {
		dec := c.decision(cl.Content)
		key := cl.Device + "|" + cl.ID
		if dec.Vote != "discard" && (dec.Copy == "" || dec.Copy == key) {
			continue
		}
		d := c.device(cl.Device)
		m := Move{Content: cl.Content, Name: cl.Name, Disk: d.Name, Reason: "copy", From: cl.Path, Size: cl.Size, device: cl.Device}
		if dec.Vote == "discard" {
			m.Reason = "discarded"
		}
		m.To = filepath.Join(d.Mount, trash, cl.ID)
		m.Why = a.check(c, m, d.Mount, cl, dec.Copy)
		m.OK = m.Why == ""
		ms = append(ms, m)
	}
	slices.SortFunc(ms, func(x, y Move) int { return strings.Compare(x.Disk+x.From, y.Disk+y.From) })
	return ms
}

func (a *App) check(c *Catalog, m Move, mount string, cl *Clip, keep string) string {
	if _, err := os.Stat(mount); err != nil || !c.device(cl.Device).Online {
		return "Drive not connected"
	}
	rel, err := filepath.Rel(mount, m.From)
	if r := filepath.ToSlash(rel); err != nil || r == ".." || strings.HasPrefix(r, "../") || strings.Contains("/"+r, "/"+trash+"/") {
		return "Path outside the drive"
	}
	if m.Reason == "copy" {
		if !c.group(cl.Content).Verified {
			return "Copies not yet verified byte for byte"
		}
		k := c.byKey[keep]
		if k == nil || !c.device(k.Device).Online || !fileExists(k.Path) {
			return "The copy to keep is on a drive that is not connected"
		}
	}
	st, err := os.Lstat(m.From)
	rec := c.files[m.From]
	if err != nil {
		return "No longer in that place"
	}
	if st.Size() != rec.Size || st.ModTime().UTC().Format(time.RFC3339) != rec.Mtime {
		return "Changed after the scan: it will be scanned again"
	}
	if _, err := os.Lstat(m.To); err == nil {
		return "Already in the trash"
	}
	return ""
}

// apply moves into <drive>/_rushbin_trash what the plan allows (same volume: a rename, nothing is copied
// or deleted), logs each move and queues a rescan of the drives touched.
func (a *App) apply(w http.ResponseWriter, r *http.Request) {
	a.wmu.Lock()
	defer a.wmu.Unlock()
	var moved, failed int
	var bytes int64
	touched := map[string]bool{}
	for _, m := range a.plan() {
		if !m.OK {
			continue
		}
		err := os.MkdirAll(filepath.Dir(m.To), 0o755)
		if err == nil {
			err = os.Rename(m.From, m.To)
		}
		if err != nil {
			log.Println("move failed:", m.From, err)
			failed++
			continue
		}
		line, _ := json.Marshal(map[string]any{"t": time.Now().UTC().Format(time.RFC3339Nano), "from": m.From, "to": m.To, "size": m.Size, "reason": m.Reason, "content": m.Content})
		if f, err := os.OpenFile(filepath.Join(a.dir, "moves.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			f.Write(append(line, '\n'))
			f.Close()
		}
		moved++
		bytes += m.Size
		touched[m.device] = true
	}
	a.smu.Lock()
	for id := range touched {
		a.enqueue(a.mounted[id])
	}
	a.smu.Unlock()
	send(w, map[string]any{"moved": moved, "failed": failed, "bytes": bytes})
}

// reveal shows a cataloged file in the Finder.
func (a *App) reveal(w http.ResponseWriter, r *http.Request) {
	var b struct{ Path string }
	json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&b)
	if _, ok := a.catalog().files[b.Path]; !ok || !fileExists(b.Path) {
		http.Error(w, "not in the catalog or drive not connected", 404)
		return
	}
	revealFile(b.Path)
	send(w, map[string]bool{"ok": true})
}

// play streams a clip from a connected drive: H.264/HEVC MP4 as is (with seeking), anything else
// converted on the fly to HLS with the Mac's hardware encoder (live.go).
func (a *App) play(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	cl := a.catalog().byKey[q.Get("device")+"|"+q.Get("id")]
	if cl == nil {
		http.Error(w, "clip not in the catalog", 404)
		return
	}
	if direct(cl) {
		f, err := os.Open(cl.Path)
		if err != nil {
			http.Error(w, "drive not connected", 404)
			return
		}
		defer f.Close()
		st, _ := f.Stat()
		w.Header().Set("Content-Type", "video/mp4")
		http.ServeContent(w, r, "", st.ModTime(), f)
		return
	}
	if !fileExists(cl.Path) {
		http.Error(w, "drive not connected", 404)
		return
	}
	t, _ := strconv.ParseFloat(q.Get("t"), 64)
	a.startLive(w, r, cl, t)
}

func (a *App) statusHandler(w http.ResponseWriter, r *http.Request) {
	paused, need, content := a.pause(false), a.modelsNeeded(), a.catalog().contentDone(filepath.Join(a.dir, "thumbs"))
	a.smu.Lock()
	defer a.smu.Unlock()
	send(w, map[string]any{"status": a.status, "pending": append([]Pending{}, a.pending...), "step": a.step, "done": a.done, "total": a.total,
		"background": a.background, "unreadable": append([]Pending{}, a.unreadable...), "paused": paused, "content": content,
		"models": map[string]any{"need": need, "done": a.dlDone, "error": a.dlErr}})
}

// drives: "catalog" or "ignore" a new drive, "rename" a drive, or change the folders it skips ("exclude",
// which scans it again if it is connected). Catalog can carry the name and skipped folders chosen first.
func (a *App) drives(w http.ResponseWriter, r *http.Request) {
	var b struct {
		UUID, Action, Name string
		Excluded           *[]string
		Content, Copies    *bool
		Local              *bool
		Location           *string
		Retired            *bool
	}
	if json.NewDecoder(io.LimitReader(r.Body, 65536)).Decode(&b) != nil {
		http.Error(w, "bad request", 400)
		return
	}
	name := strings.TrimSpace(b.Name)
	if b.Excluded != nil {
		for _, x := range *b.Excluded {
			if x == "" || strings.ContainsRune(x, '/') {
				http.Error(w, "folder names only", 400)
				return
			}
		}
	}
	edit := func(mount string) error { // device.json keeps what is not changed here
		return a.editDevice(b.UUID, mount, func(d *Device) {
			if name != "" {
				d.Name = name
			}
			if b.Excluded != nil {
				d.Excluded = append([]string{}, *b.Excluded...)
			}
			if b.Content != nil {
				d.Content = b.Content
			}
			if b.Copies != nil {
				d.Copies = b.Copies
			}
			if b.Local != nil {
				d.Local = b.Local
			}
			if b.Location != nil {
				d.Location = strings.TrimSpace(*b.Location)
			}
			if b.Retired != nil {
				d.Retired = ""
				if *b.Retired {
					d.Retired = time.Now().Format("2006-01-02")
				}
			}
		})
	}
	a.smu.Lock()
	fail := func(msg string, code int) { a.smu.Unlock(); http.Error(w, msg, code) }
	mount := a.mounted[b.UUID]
	again := false // the catalog changes: reloaded before answering, so the page shows it
	switch b.Action {
	case "catalog", "ignore":
		i := slices.IndexFunc(a.pending, func(p Pending) bool { return p.UUID == b.UUID })
		if i < 0 {
			fail("drive not waiting", 404)
			return
		}
		a.pending = slices.Delete(a.pending, i, i+1)
		if b.Action == "ignore" {
			a.ignored[b.UUID] = true
			j, _ := json.Marshal(a.ignored)
			os.WriteFile(filepath.Join(a.dir, "ignored.json"), j, 0o644)
			break
		}
		if err := edit(mount); err != nil {
			fail(err.Error(), 500)
			return
		}
		a.enqueue(mount)
	case "rename", "exclude", "options":
		if !fileExists(filepath.Join(a.dir, "disks", filepath.Base(b.UUID), "device.json")) || b.Action == "rename" && name == "" {
			fail("unknown drive or empty name", 400)
			return
		}
		if err := edit(""); err != nil {
			fail(err.Error(), 500)
			return
		}
		if b.Action == "exclude" && mount != "" {
			a.enqueue(mount)
		}
		again = true
	default:
		fail("unknown action", 400)
		return
	}
	a.smu.Unlock()
	if again {
		a.reload()
		select { // the team site sees the change too (the scanner sends: one share at a time)
		case a.shareNow <- struct{}{}:
		default:
		}
	}
	send(w, map[string]bool{"ok": true})
}

// editDevice changes device.json, creating it for a drive not scanned yet.
func (a *App) editDevice(id, mount string, fn func(*Device)) error {
	dir := filepath.Join(a.dir, "disks", filepath.Base(id))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	d := Device{UUID: id, Mount: mount, Excluded: []string{}}
	if j, err := os.ReadFile(filepath.Join(dir, "device.json")); err == nil {
		json.Unmarshal(j, &d)
	}
	fn(&d)
	j, _ := json.MarshalIndent(d, "", "  ")
	return os.WriteFile(filepath.Join(dir, "device.json"), append(j, '\n'), 0o644)
}

// folders lists the top folders of a connected drive and whether each one is skipped.
func (a *App) folders(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("uuid")
	a.smu.Lock()
	mount := a.mounted[id]
	a.smu.Unlock()
	if mount == "" {
		http.Error(w, "drive not connected", 404)
		return
	}
	var d Device
	if j, err := os.ReadFile(filepath.Join(a.dir, "disks", filepath.Base(id), "device.json")); err == nil {
		json.Unmarshal(j, &d)
	}
	entries, err := os.ReadDir(mount)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	type folder struct {
		Name    string `json:"name"`
		Skipped bool   `json:"skipped"`
	}
	fs := []folder{}
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") && e.Name() != trash {
			fs = append(fs, folder{e.Name(), slices.Contains(d.Excluded, e.Name())})
		}
	}
	send(w, fs)
}

// watch looks at the connected drives (/Volumes on the Mac, the drive letters on Windows) every 3 seconds. A cataloged drive is scanned again when it comes back
// (the catalog stays current by itself); a new one waits for the user's choice.
func (a *App) watch() {
	for {
		a.poll()
		time.Sleep(3 * time.Second)
	}
}

func (a *App) poll() {
	now := map[string]bool{}
	changed := false
	for _, mount := range a.mounts() {
		now[mount] = true
		a.smu.Lock()
		_, known := a.seen[mount]
		a.smu.Unlock()
		if known {
			continue
		}
		d, err := identify(mount)
		a.smu.Lock()
		if err != nil || d.UUID == "" || d.Bus == "Disk Image" { // installers and other disk images
			if err == nil && d.UUID == "" && d.Bus != "Disk Image" { // like NTFS on an MBR disk, on the Mac (scan.go identify)
				a.unreadable = append(a.unreadable, Pending{Name: d.Name, Mount: mount, FS: d.FS})
			}
			a.seen[mount] = ""
			a.smu.Unlock()
			continue
		}
		a.seen[mount] = d.UUID
		a.mounted[d.UUID] = mount
		switch {
		case fileExists(filepath.Join(a.dir, "disks", d.UUID, "files.jsonl")):
			a.enqueue(mount)
		case !a.ignored[d.UUID]:
			a.pending = append(a.pending, Pending{UUID: d.UUID, Name: d.Name, Mount: mount, Size: d.Size, Free: d.Free})
		}
		a.smu.Unlock()
		changed = true
	}
	a.smu.Lock()
	for mount, id := range a.seen {
		if !now[mount] {
			delete(a.seen, mount)
			delete(a.mounted, id)
			a.pending = slices.DeleteFunc(a.pending, func(p Pending) bool { return p.Mount == mount })
			a.unreadable = slices.DeleteFunc(a.unreadable, func(p Pending) bool { return p.Mount == mount })
			changed = true
		}
	}
	a.smu.Unlock()
	if changed {
		a.reload()
	}
}

func (a *App) mounts() []string {
	if runtime.GOOS == "windows" {
		return driveRoots()
	}
	entries, _ := os.ReadDir(a.volumes)
	var out []string
	for _, e := range entries {
		if e.Type()&fs.ModeSymlink == 0 && e.IsDir() { // the startup disk appears as a symlink to /
			out = append(out, filepath.Join(a.volumes, e.Name()))
		}
	}
	return out
}

// enqueue queues a scan of mount unless one already waits, which will see this change too. The caller holds
// smu; it never blocks, so that is safe (the scanner takes smu to report progress).
func (a *App) enqueue(mount string) {
	if a.queued[mount] {
		return
	}
	if a.queued == nil {
		a.queued = map[string]bool{}
	}
	a.queued[mount] = true
	go func() { a.queue <- mount }()
}

// changed is told that something under path changed (FSEvents on the Mac; Windows has no watcher yet, as one
// on a drive's root would block its safe removal: there a drive is scanned again when connected):
// the cataloged drive holding it is scanned again. Changes in the folders the inventory skips are ignored.
func (a *App) changed(path string) {
	a.smu.Lock()
	defer a.smu.Unlock()
	for mount, id := range a.seen {
		rel, err := filepath.Rel(mount, path)
		if id == "" || err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		for _, part := range strings.Split(rel, string(filepath.Separator)) {
			if noise[part] {
				return
			}
		}
		if fileExists(filepath.Join(a.dir, "disks", id, "files.jsonl")) {
			a.enqueue(mount)
		}
		return
	}
}

func (a *App) setStatus(format string, v ...any) {
	a.smu.Lock()
	a.status = fmt.Sprintf(format, v...)
	if a.status == "" {
		a.step, a.done, a.total = 0, 0, 0
	}
	a.smu.Unlock()
}

func (a *App) setProgress(step int, done, total int64) {
	a.smu.Lock()
	a.step, a.done, a.total = step, done, total
	a.smu.Unlock()
}

// scanner catalogs queued drives one at a time (files, details, previews). Between drives it does the heavy
// work the drives were given: looking at the previews (needs no drive), what is said, light copies for the
// team; it waits while pause says so.
func (a *App) scanner() {
	say = func(format string, v ...any) { a.setStatus(format, v...) }
	progress = a.setProgress
	for {
		select { // a drive cataloged, or changed by hand, reaches the team site right away
		case mount := <-a.queue:
			a.catalog1(mount)
			a.share()
			continue
		case <-a.shareNow:
			a.share()
			continue
		default:
		}
		why := a.pause(false)
		if why == "" && a.analyze() {
			a.reload()
			a.share()
			a.setStatus("")
			continue
		}
		// The text model for search by looks alone, before the speech models (text.go).
		if dir := filepath.Join(a.dir, "models"); why == "" && a.wantText() && (!siglipText[0].here(dir) || !siglipText[1].here(dir)) {
			_, err := a.ensureModels(siglipText)
			if err == nil {
				continue
			}
			log.Println("text model:", err) // tried again next round, from where it stopped
		}
		// What is said (quick, and what search needs), sent to the team every 20 clips and at the end.
		if why == "" && a.speechNext() {
			if a.unsent++; a.unsent >= 20 {
				a.unsent = 0
				a.share()
			}
			continue
		}
		if a.unsent > 0 {
			a.unsent = 0
			a.share()
		}
		// One light copy for the team, then the rest again. With nothing to do, wait for a drive, or a
		// minute (the team may have been connected, or the computer plugged in, meanwhile).
		if why = a.pause(true); why != "" {
			a.setBackground("Content search and copies for the team wait: " + why)
		} else {
			a.setBackground("")
		}
		if why != "" || !a.proxyNext() {
			select {
			case mount := <-a.queue:
				a.catalog1(mount)
				a.share()
			case <-a.shareNow:
				a.share()
			case <-time.After(time.Minute):
			}
		}
	}
}

// catalog1 scans one drive, makes its previews and has them analyzed.
func (a *App) catalog1(mount string) {
	a.smu.Lock()
	delete(a.queued, mount) // a change from now on queues another scan
	a.smu.Unlock()
	if mount == "" || !dirExists(mount) {
		return
	}
	id, err := scan(a.dir, mount, "", "", nil)
	if err != nil {
		log.Printf("scan %s: %v", mount, err)
		a.setStatus("Could not catalog %s: %v", filepath.Base(mount), err)
		return
	}
	a.reload()
	a.thumbs(id)
	a.reload()
	a.setStatus("")
}

func dirExists(p string) bool { st, err := os.Stat(p); return err == nil && st.IsDir() }

// fit scales the longest side to at most n pixels. Transparency goes on gray, like an editor's
// checkerboard: JPEG previews and H.264 proxies have none.
func fit(n int, alpha bool) string {
	vf := fmt.Sprintf("scale='if(gte(iw,ih),min(%[1]d,iw),-2)':'if(gte(iw,ih),-2,min(%[1]d,ih))'", n)
	if alpha {
		vf += ",format=rgba,split[a][b];[a]drawbox=c=gray:t=fill:replace=1[bg];[bg][b]overlay"
	}
	return vf
}

// thumbs extracts 5 frames per content (10, 30, 50, 70 and 90% of the clip) as 480 px JPEGs, once per
// content, so copies share them. Up to 6 at a time: about 220 MB each for a 4K MXF.
func (a *App) thumbs(id string) {
	c := a.catalog()
	var todo []*Clip
	done := map[string]bool{}
	for _, cl := range c.list {
		if cl.Device == id && !done[cl.Thumb] && !fileExists(filepath.Join(a.dir, "thumbs", cl.Thumb, "5.jpg")) {
			done[cl.Thumb] = true
			todo = append(todo, cl)
		}
	}
	if len(todo) > 0 {
		a.setStatus("Making previews: 0 of %d", len(todo))
		a.setProgress(3, 0, int64(len(todo)))
	}
	jobs := make(chan *Clip)
	var wg sync.WaitGroup
	var n int
	var nmu sync.Mutex
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for cl := range jobs {
				out := filepath.Join(a.dir, "thumbs", cl.Thumb)
				os.MkdirAll(out, 0o755)
				vf := fit(480, cl.Alpha)
				for k, pct := range []float64{10, 30, 50, 70, 90} {
					at, jpg := strconv.FormatFloat(cl.Dur*pct/100, 'f', 3, 64), filepath.Join(out, fmt.Sprintf("%d.jpg", k+1))
					enc := []string{"-frames:v", "1", "-vf", vf, "-pix_fmt", "yuvj420p", "-q:v", "5", jpg}
					if dec := decoder(cl); dec != "" { // the decoder gives the frame (PPM), FFmpeg makes the JPEG as for every clip
						ff := background(tool("ffmpeg"), append([]string{"-nostdin", "-loglevel", "error", "-y", "-f", "ppm_pipe", "-i", "pipe:3"}, enc...)...)
						if done, err := pipesFrom([]*exec.Cmd{background(tool(dec), "frame", cl.Path, at)})(ff); err == nil {
							ff.Run()
							done() // closes the pipe first: a decoder still writing is not waited for ever
						}
						continue
					}
					args := append([]string{"-nostdin", "-loglevel", "error", "-y", "-threads", "2"}, hwDecode()...)
					background(tool("ffmpeg"), append(args, append([]string{"-ss", at, "-i", cl.Path}, enc...)...)...).Run()
				}
				nmu.Lock()
				n++
				a.setStatus("Making previews: %d of %d", n, len(todo))
				a.setProgress(3, int64(n), int64(len(todo)))
				nmu.Unlock()
			}
		}()
	}
	for _, cl := range todo {
		jobs <- cl
	}
	close(jobs)
	wg.Wait()
}
