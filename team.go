package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Team is the Mac app's link to a team site: the drives it catalogs and the decisions go there, and the
// team's decisions come back. Saved in team.json, readable by this user only.
type Team struct {
	URL   string `json:"url"`
	Token string `json:"token"`
	Name  string `json:"name"`  // the person signed in
	Email string `json:"email"` //
	Team  string `json:"team"`  // the team's name

	wait time.Duration // how long a call may take, when longer than client's 2 minutes (not saved)
}

var client = &http.Client{Timeout: 2 * time.Minute}

// errSignedOut: the site no longer accepts the token (unused for 90 days, password changed, removed from the team).
var errSignedOut = errors.New("signed out of the team site")

// call sends in as JSON (or nothing) and decodes the answer into out.
func (t *Team) call(method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	return t.send(method, path, body, "application/json", out)
}

func (t *Team) send(method, path string, body io.Reader, ctype string, out any) error {
	req, err := http.NewRequest(method, t.URL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", ctype)
	if t.Token != "" {
		req.Header.Set("Authorization", "Bearer "+t.Token)
	}
	c := client
	if t.wait > 0 {
		c = &http.Client{Timeout: t.wait}
	}
	r, err := c.Do(req)
	if err != nil {
		return err
	}
	defer r.Body.Close()
	if r.StatusCode == http.StatusUnauthorized {
		return errSignedOut
	}
	if r.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(r.Body, 512))
		return fmt.Errorf("%s: %s", r.Status, strings.TrimSpace(string(msg)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(r.Body).Decode(out)
}

func (a *App) teamConf() *Team {
	a.smu.Lock()
	defer a.smu.Unlock()
	return a.team
}

func (a *App) loadTeam() {
	var t Team
	if b, err := os.ReadFile(filepath.Join(a.dir, "team.json")); err == nil && json.Unmarshal(b, &t) == nil && t.Token != "" {
		a.team = &t
		go a.follow(&t)
	}
	a.lastID = lastDecisionID(filepath.Join(a.dir, "decisions.jsonl"))
}

// saveTeam writes team.json and makes t the team of this app.
func (a *App) saveTeam(t *Team) error {
	j, _ := json.Marshal(t)
	if err := os.WriteFile(filepath.Join(a.dir, "team.json"), j, 0o600); err != nil {
		return err
	}
	a.smu.Lock()
	a.team = t
	a.smu.Unlock()
	return nil
}

// follow keeps the team's address when the site has moved to another name: its home page then answers with a
// permanent redirect to the new name (RUSHBIN_ALIASES), while the apps' paths still work at the old one.
func (a *App) follow(t *Team) {
	if to := moved(t.URL); to != "" {
		n := *t
		n.URL = to
		a.saveTeam(&n)
	}
}

// moved is the address the site at site moved to, from a permanent redirect of its home page to another
// host; "" when it has not moved or cannot be reached.
func moved(site string) string {
	c := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := c.Get(site + "/")
	if err != nil {
		return ""
	}
	res.Body.Close()
	if res.StatusCode != http.StatusMovedPermanently && res.StatusCode != http.StatusPermanentRedirect {
		return ""
	}
	to, err := res.Location()
	from, _ := url.Parse(site)
	if err != nil || from == nil || to.Host == "" || to.Host == from.Host || (to.Scheme != "https" && !isLocal(to.Hostname())) {
		return ""
	}
	return to.Scheme + "://" + to.Host
}

func lastDecisionID(file string) int64 {
	f, err := os.Open(file)
	if err != nil {
		return 0
	}
	defer f.Close()
	var id int64
	dec := json.NewDecoder(bufio.NewReader(f))
	for {
		var l DecisionLine
		if dec.Decode(&l) != nil {
			return id
		}
		id = max(id, l.ID)
	}
}

// me tells the page whether it runs in the app or on the site, and the team the app is connected to.
func (a *App) me(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"site": false, "windows": runtime.GOOS == "windows", "eula": true, "updates": a.updatesAllowed()}
	if t := a.teamConf(); t != nil {
		a.smu.Lock()
		out["team"] = map[string]any{"url": t.URL, "name": t.Name, "email": t.Email, "team": t.Team, "signed_out": a.signedOut}
		a.smu.Unlock()
	}
	if t := a.updateSite(); t != nil {
		if u := a.update(t); u != nil {
			out["update"] = u
		}
	}
	send(w, out)
}

// connect signs in to a team site, then sends it every drive and the decisions taken alone so far. With a code
// (from rushbin://connect, after the page asked whether to connect) it trades the code instead of a password.
func (a *App) connect(w http.ResponseWriter, r *http.Request) {
	var b struct{ URL, Email, Password, Code string }
	if json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&b) != nil {
		http.Error(w, "bad request", 400)
		return
	}
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(b.URL), "/"))
	if err != nil || u.Host == "" || u.Scheme != "https" && !(u.Scheme == "http" && isLocal(u.Hostname())) {
		http.Error(w, "Write the address of the team site, like https://rushbin.example.com", 400)
		return
	}
	host, _ := os.Hostname() // Windows: the PC name
	if out, err := exec.Command("/usr/sbin/scutil", "--get", "ComputerName").Output(); err == nil && len(bytes.TrimSpace(out)) > 0 {
		host = string(bytes.TrimSpace(out)) // the name in System Settings, like "Dana's MacBook Pro"
	}
	t := &Team{URL: u.String()}
	path, in := "/api/sync/login", map[string]string{"email": b.Email, "password": b.Password, "device": host}
	if b.Code != "" {
		path, in = "/api/sync/pair", map[string]string{"token": b.Code, "device": host}
	}
	var res struct{ Token, Name, Email, Team string }
	if err := t.call("POST", path, in, &res); err != nil {
		http.Error(w, "Could not sign in: "+err.Error(), 403)
		return
	}
	t.Token, t.Name, t.Email, t.Team = res.Token, res.Name, res.Email, res.Team
	if err := a.saveTeam(t); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	a.smu.Lock()
	a.signedOut = false
	a.smu.Unlock()
	go a.share()
	send(w, map[string]bool{"ok": true})
}

func isLocal(h string) bool { return h == "localhost" || h == "127.0.0.1" || h == "::1" }

// disconnect forgets the team on this Mac; the site keeps what it received.
func (a *App) disconnect(w http.ResponseWriter, r *http.Request) {
	t := a.teamConf()
	if t != nil {
		t.call("POST", "/api/sync/logout", nil, nil)
	}
	os.Remove(filepath.Join(a.dir, "team.json"))
	a.smu.Lock()
	a.team, a.signedOut = nil, false
	a.smu.Unlock()
	send(w, map[string]bool{"ok": true})
}

// share sends every drive to the team, except those kept on this computer (Local), which the site drops if it has
// them; then the decisions taken before joining; then takes the team's.
func (a *App) share() {
	t := a.teamConf()
	if t == nil {
		return
	}
	var has struct{ Drives []string }
	if err := t.call("GET", "/api/sync/have", nil, &has); err != nil {
		log.Printf("team: %v", err)
		return
	}
	for _, d := range a.catalog().Devices {
		var err error
		if !on(d.Local) {
			err = a.push(t, d.UUID)
		} else if slices.Contains(has.Drives, d.UUID) { // asked only of a drive the site has: it learns nothing new
			err = t.call("DELETE", "/api/sync/drive/"+url.PathEscape(d.UUID), nil, nil)
		}
		if err != nil {
			log.Printf("team: sending %s: %v", d.Name, err)
			a.setStatus("Could not send %s to the team: %v", d.Name, err)
			return
		}
	}
	a.wmu.Lock()
	if a.lastID == 0 { // decisions taken alone: the team gets the latest of each
		for content, d := range a.catalog().decisions() {
			var hidden []string
			for _, h := range d.Hidden {
				hidden = append(hidden, strconv.FormatFloat(h, 'f', -1, 64))
			}
			checked := map[bool]string{true: "yes"}[d.Checked]
			for _, l := range []DecisionLine{{Content: content, Kind: "vote", Value: d.Vote}, {Content: content, Kind: "copy", Value: d.Copy},
				{Content: content, Kind: "hide", Value: strings.Join(hidden, " ")}, {Content: content, Kind: "check", Value: checked}} {
				if l.Value != "" && a.catalog().shared(l) {
					if err := t.call("POST", "/api/sync/decide", l, nil); err != nil {
						log.Printf("team: decision %s: %v", content, err)
					}
				}
			}
		}
	}
	a.wmu.Unlock()
	a.pull()
	a.setStatus("")
}

// push sends one drive: its catalog files, the previews and what Apple Vision saw that the site lacks.
func (a *App) push(t *Team, id string) error {
	dir := filepath.Join(a.dir, "disks", filepath.Base(id))
	var name string
	for _, d := range a.catalog().Devices {
		if d.UUID == id {
			name = d.Name
		}
	}
	a.setStatus("Sending %s to the team", name)
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		for _, f := range []string{"device.json", "files.jsonl", "fullhash.tsv"} {
			part, _ := mw.CreateFormFile(f, f)
			if src, err := os.Open(filepath.Join(dir, f)); err == nil {
				io.Copy(part, src)
				src.Close()
			}
		}
		pw.CloseWithError(mw.Close())
	}()
	if err := t.send("PUT", "/api/sync/drive/"+url.PathEscape(id), pr, mw.FormDataContentType(), nil); err != nil {
		return err
	}
	var has struct{ Thumbs, Looks, Speech, Emb []string }
	if err := t.call("GET", "/api/sync/have", nil, &has); err != nil {
		return err
	}
	thumbs, looks, speech, emb := set(has.Thumbs), set(has.Looks), set(has.Speech), set(has.Emb)
	c := a.catalog()
	var todo []string
	seen := map[string]bool{}
	for _, cl := range c.list {
		if cl.Device == id && !seen[cl.Thumb] && !thumbs[cl.Thumb] && fileExists(filepath.Join(a.dir, "thumbs", cl.Thumb, "5.jpg")) {
			seen[cl.Thumb] = true
			todo = append(todo, cl.Thumb)
		}
	}
	for i, th := range todo {
		a.setStatus("Sending %s to the team: previews %d of %d", name, i+1, len(todo))
		for k := 1; k <= 5; k++ {
			f, err := os.Open(filepath.Join(a.dir, "thumbs", th, fmt.Sprintf("%d.jpg", k)))
			if err != nil {
				continue
			}
			err = t.send("PUT", fmt.Sprintf("/api/sync/thumbs/%s/%d", th, k), f, "image/jpeg", nil)
			f.Close()
			if err != nil {
				return err
			}
		}
	}
	// What Apple Vision saw, for the previews of this drive the site does not have yet.
	mine := map[string]bool{}
	for _, cl := range c.list {
		if cl.Device == id && !looks[cl.Thumb] {
			mine[cl.Thumb] = true
		}
	}
	var buf bytes.Buffer
	if f, err := os.Open(filepath.Join(a.dir, "vision.jsonl")); err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<16), 1<<24)
		for sc.Scan() {
			var v struct{ Thumb string }
			if json.Unmarshal(sc.Bytes(), &v) == nil && mine[v.Thumb] {
				buf.Write(sc.Bytes())
				buf.WriteByte('\n')
			}
		}
		f.Close()
	}
	if buf.Len() > 0 {
		if err := t.send("POST", "/api/sync/looks", &buf, "application/x-ndjson", nil); err != nil {
			return err
		}
	}
	// What is said, for the clips of this drive the site does not have yet.
	buf.Reset()
	for th := range seenThumbs(c, id) {
		if sp := c.speechOf(th); sp != nil && !speech[th] {
			line, _ := json.Marshal(map[string]any{"thumb": th, "segs": sp.Segs})
			buf.Write(append(line, '\n'))
		}
	}
	if buf.Len() > 0 {
		if err := t.send("POST", "/api/sync/speech", &buf, "application/x-ndjson", nil); err != nil {
			return err
		}
	}
	// How the previews look, for search by content: emb.bin, in batches of 500 clips.
	buf.Reset()
	n := 0
	flush := func() error {
		if buf.Len() == 0 {
			return nil
		}
		err := t.send("POST", "/api/sync/emb", &buf, "application/x-ndjson", nil)
		buf.Reset()
		n = 0
		return err
	}
	for th := range seenThumbs(c, id) {
		if emb[th] {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(a.dir, "thumbs", th, "emb.bin"))
		if err != nil {
			continue
		}
		line, _ := json.Marshal(map[string]string{"thumb": th, "emb": base64.StdEncoding.EncodeToString(raw)})
		buf.Write(append(line, '\n'))
		if n++; n == 500 {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	return flush()
}

// seenThumbs: the contents of one drive.
func seenThumbs(c *Catalog, id string) map[string]bool {
	m := map[string]bool{}
	for _, cl := range c.list {
		if cl.Device == id {
			m[cl.Thumb] = true
		}
	}
	return m
}

func set(xs []string) map[string]bool {
	m := map[string]bool{}
	for _, x := range xs {
		m[x] = true
	}
	return m
}

// pull takes the team's decisions taken since the last one this Mac has.
func (a *App) pull() {
	t := a.teamConf()
	if t == nil {
		return
	}
	a.wmu.Lock()
	defer a.wmu.Unlock()
	var ls []DecisionLine
	err := t.call("GET", fmt.Sprintf("/api/sync/decisions?after=%d", a.lastID), nil, &ls)
	a.smu.Lock()
	a.signedOut = errors.Is(err, errSignedOut) // asked every 15 s: the page knows within that
	a.smu.Unlock()
	if err != nil {
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			log.Println("team: decisions:", err)
		}
		return
	}
	for _, l := range ls {
		if err := a.record(l); err != nil {
			log.Println("team: decisions:", err)
			return
		}
	}
}

// teamLoop keeps the team's decisions current while the app is open.
func (a *App) teamLoop() {
	for {
		a.pull()
		time.Sleep(15 * time.Second)
	}
}

// decisions is a copy of the decisions, safe to read while new ones arrive.
func (c *Catalog) decisions() map[string]Decision {
	c.dmu.RLock()
	defer c.dmu.RUnlock()
	m := make(map[string]Decision, len(c.dec))
	for k, d := range c.dec {
		m[k] = *d
	}
	return m
}

// originalsLoop says which drives are connected here and sends the originals the team asks for, every 15 s.
func (a *App) originalsLoop() {
	for {
		if t := a.teamConf(); t != nil {
			a.serveOriginals(t)
		}
		time.Sleep(15 * time.Second)
	}
}

// viaTeam passes a request of the page to the team site (originals of drives connected elsewhere).
func (a *App) viaTeam(w http.ResponseWriter, r *http.Request) {
	t := a.teamConf()
	if t == nil {
		http.Error(w, "Connect this Mac to a team to get originals from other Macs", 404)
		return
	}
	var out json.RawMessage
	var body io.Reader
	if r.Method == "POST" {
		body = io.LimitReader(r.Body, 8192)
	}
	if err := t.send(r.Method, r.URL.Path, body, "application/json", &out); err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(out)
}
