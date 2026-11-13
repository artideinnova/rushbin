package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// TestDataRights: what the team's data rights promise. An owner exports everything the site holds of the team and
// nothing of another; a drive whose "Send to the team" is off sends nothing and is taken off the site; what is not
// used goes after its term. Needs Postgres, as TestServer.
func TestDataRights(t *testing.T) {
	admin := os.Getenv("RUSHBIN_TEST_DB")
	if admin == "" {
		t.Skip("RUSHBIN_TEST_DB not set")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, admin)
	must(t, err)
	name := "rushbin_test_" + token()[:8]
	_, err = conn.Exec(ctx, "create database "+name)
	must(t, err)
	defer func() { conn.Exec(ctx, "drop database "+name+" with (force)"); conn.Close(ctx) }()
	u, _ := url.Parse(admin)
	u.Path = "/" + name
	t.Setenv("RUSHBIN_APP_PASSWORD", token())
	s, err := openServer(ctx, u.String(), t.TempDir())
	must(t, err)
	defer s.db.Close()
	defer s.admin.Close()

	var smu sync.Mutex
	stored := map[string][]byte{} // the store, by path
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		smu.Lock()
		defer smu.Unlock()
		switch r.Method {
		case "PUT":
			stored[r.URL.Path], _ = io.ReadAll(r.Body)
		case "DELETE":
			delete(stored, r.URL.Path)
		default:
			if _, ok := stored[r.URL.Path]; !ok {
				http.NotFound(w, r)
			}
		}
	}))
	defer fake.Close()
	s.store = &Store{Endpoint: fake.URL, Bucket: "b", KeyID: "k", Secret: "s", Region: "auto"}
	inStore := func(key string) bool { smu.Lock(); defer smu.Unlock(); _, ok := stored["/b/"+key]; return ok }
	put := func(key string) { smu.Lock(); stored["/b/"+key] = []byte("x"); smu.Unlock() }

	// Every request a computer sends, to check what a drive kept on it never sends.
	var lmu sync.Mutex
	var sent strings.Builder
	site := s.routes()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			b, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(b))
			lmu.Lock()
			fmt.Fprintf(&sent, "%s %s %s\n", r.Method, r.URL, b)
			lmu.Unlock()
		}
		site.ServeHTTP(w, r)
	}))
	defer srv.Close()
	call := func(c *http.Client, method, path, body string) (int, []byte) {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		req.Header.Set("X-Rushbin", "1")
		req.Header.Set("Origin", srv.URL)
		r, err := c.Do(req)
		must(t, err)
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		return r.StatusCode, b
	}
	ok := func(code int, b []byte) []byte {
		t.Helper()
		if code != 200 {
			t.Fatalf("%d %s", code, b)
		}
		return b
	}
	browser := func() *http.Client { jar, _ := cookiejar.New(nil); return &http.Client{Jar: jar} }
	join := func(tok, name, pw string) *http.Client {
		c := browser()
		ok(call(c, "POST", "/api/auth/join", `{"token":"`+tok+`","name":"`+name+`","password":"`+pw+`"}`))
		return c
	}
	invite := func(owner *http.Client, email, role string) string {
		b := string(ok(call(owner, "POST", "/api/team/invite", `{"email":"`+email+`","role":"`+role+`"}`)))
		return b[strings.Index(b, "=")+1 : strings.LastIndex(b, `"`)]
	}
	tok, err := newTeam(ctx, s, "Alpha", "ao@a.test")
	must(t, err)
	ao := join(tok, "Anna", "aoaoaoaoao")
	ae := join(invite(ao, "ae@a.test", "editor"), "Elio", "aeaeaeaeae")
	av := join(invite(ao, "av@a.test", "viewer"), "Vera", "avavavavav")
	tok, err = newTeam(ctx, s, "Beta", "bo@b.test")
	must(t, err)
	bo := join(tok, "Bruno", "bobobobobo")

	// Beta's data, marked so that any of it in Alpha's export shows.
	var alpha, beta int64
	must(t, s.admin.QueryRow(ctx, "select id from team where name = 'Alpha'").Scan(&alpha))
	must(t, s.admin.QueryRow(ctx, "select id from team where name = 'Beta'").Scan(&beta))
	must(t, s.inTeam(ctx, beta, func(tx pgx.Tx) error {
		for _, q := range []string{
			`insert into drive (team, uuid, device, files, fullhash) values ($1, 'BETA-DRIVE', '{"name":"BETA-SECRET"}', 'BETA-SECRET', '')`,
			`insert into decision (team, content, kind, value, by_name) values ($1, 'c:BETA-SECRET', 'vote', 'keep', 'Bruno')`,
			`insert into look (team, thumb, frames) values ($1, 'BETA_SECRET', '[]')`,
			`insert into speech (team, thumb, segs) values ($1, 'BETA_SECRET', '[{"s":0,"e":1,"t":"BETA-SECRET"}]')`,
		} {
			if _, err := tx.Exec(ctx, q, beta); err != nil {
				return err
			}
		}
		return nil
	}))
	must(t, os.MkdirAll(s.thumbDir(beta, "BETA_SECRET"), 0o755))
	must(t, os.WriteFile(filepath.Join(s.thumbDir(beta, "BETA_SECRET"), "1.jpg"), []byte("\xff\xd8\xff BETA-SECRET"), 0o644))

	// Elio's Mac has two drives: A, and L, which he will keep to himself. shared.mp4 is on both.
	tmp := t.TempDir()
	cat, drvA, drvL := filepath.Join(tmp, "catalog"), filepath.Join(tmp, "A"), filepath.Join(tmp, "L")
	const local = "LOCAL-DRIVE-UUID"
	must(t, os.MkdirAll(drvA, 0o755))
	must(t, os.MkdirAll(drvL, 0o755))
	ffmpeg(t, "testsrc=duration=1:size=160x120:rate=25", filepath.Join(drvA, "clip.mp4"))
	ffmpeg(t, "testsrc2=duration=1:size=160x120:rate=25", filepath.Join(drvA, "shared.mp4"))
	ffmpeg(t, "smptebars=duration=1:size=160x120:rate=25", filepath.Join(drvL, "private.mp4"))
	copyFile(t, filepath.Join(drvA, "shared.mp4"), filepath.Join(drvL, "shared.mp4"))
	scanned(t, cat, drvA, "A", "", nil)
	scanned(t, cat, drvL, local, "", nil)
	app := &App{dir: cat, mounted: map[string]string{"A": drvA, local: drvL}, seen: map[string]string{}, ignored: map[string]bool{}, queue: make(chan string, 64)}
	must(t, app.reload())
	c := app.catalog()
	clip, shared, private := c.byKey["A|clip.mp4"], c.byKey["A|shared.mp4"], c.byKey[local+"|private.mp4"]
	var emb []byte
	for range 5 * embDim {
		emb = binary.LittleEndian.AppendUint16(emb, toHalf(0.25))
	}
	var vision, speech bytes.Buffer
	for _, cl := range []*Clip{clip, shared, private} {
		dir := filepath.Join(cat, "thumbs", cl.Thumb)
		must(t, os.MkdirAll(dir, 0o755))
		for k := 1; k <= 5; k++ {
			must(t, os.WriteFile(filepath.Join(dir, fmt.Sprintf("%d.jpg", k)), []byte("\xff\xd8\xff "+cl.Name), 0o644))
		}
		must(t, os.WriteFile(filepath.Join(dir, "emb.bin"), emb, 0o644))
		fmt.Fprintf(&vision, `{"thumb":%q,"frames":[{"contrast":0.4,"labels":{"stage":0.8,"child":0.9}}]}`+"\n", cl.Thumb)
		fmt.Fprintf(&speech, `{"thumb":%q,"segs":[{"s":1,"e":2,"t":"words of %s"}]}`+"\n", cl.Thumb, cl.Name)
	}
	must(t, os.WriteFile(filepath.Join(cat, "vision.jsonl"), vision.Bytes(), 0o644))
	must(t, os.WriteFile(filepath.Join(cat, "speech.jsonl"), speech.Bytes(), 0o644))
	must(t, app.reload())
	var login struct{ Token string }
	must(t, json.Unmarshal(ok(call(http.DefaultClient, "POST", "/api/sync/login", `{"email":"ae@a.test","password":"aeaeaeaeae","device":"Elio's Mac"}`)), &login))
	app.team = &Team{URL: srv.URL, Token: login.Token, Name: "Elio", Team: "Alpha"}
	app.share()

	// The team's own records: decisions, an original asked for on L, light copies, a clip removed on request.
	ok(call(ae, "POST", "/api/decide", `{"content":"`+shared.Content+`","kind":"vote","value":"keep"}`))
	ok(call(ae, "POST", "/api/decide", `{"content":"`+private.Content+`","kind":"vote","value":"showreel"}`))
	var req struct{ ID int64 }
	must(t, json.Unmarshal(ok(call(ae, "POST", "/api/original", `{"device":"`+local+`","id":"private.mp4"}`)), &req))
	_, err = s.admin.Exec(ctx, "update request set state = 'ready' where id = $1", req.ID)
	must(t, err)
	put(originalKey(alpha, req.ID, "private.mp4"))
	must(t, s.inTeam(ctx, alpha, func(tx pgx.Tx) error {
		for _, cl := range []*Clip{shared, private} {
			if _, err := tx.Exec(ctx, "insert into proxy (team, thumb, size) values ($1, $2, 100)", alpha, cl.Thumb); err != nil {
				return err
			}
			put(proxyKey(alpha, cl.Thumb))
		}
		return nil
	}))
	ok(call(ao, "POST", "/api/forget/"+clip.Thumb, ""))

	// The export: owners only.
	for who, c := range map[string]*http.Client{"an editor": ae, "a viewer": av} {
		if code, _ := call(c, "GET", "/api/team/export", ""); code != 403 {
			t.Fatalf("%s exports: %d, want 403", who, code)
		}
	}
	if code, _ := call(browser(), "GET", "/api/team/export", ""); code != 401 {
		t.Fatalf("export signed out: %d, want 401", code)
	}
	b := ok(call(ao, "GET", "/api/team/export", ""))
	z, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	must(t, err)
	got := map[string][]byte{}
	for _, f := range z.File {
		r, err := f.Open()
		must(t, err)
		got[f.Name], _ = io.ReadAll(r)
		r.Close()
		if bytes.Contains(got[f.Name], []byte("BETA")) || strings.Contains(f.Name, "BETA") || bytes.Contains(got[f.Name], []byte("bo@b.test")) {
			t.Fatalf("Alpha's export holds Beta's data in %s", f.Name)
		}
	}
	lines := func(name string) []map[string]any {
		t.Helper()
		var out []map[string]any
		for _, l := range bytes.Split(bytes.TrimSpace(got[name]), []byte("\n")) {
			var m map[string]any
			if len(l) > 0 {
				must(t, json.Unmarshal(l, &m))
				out = append(out, m)
			}
		}
		return out
	}
	for _, d := range []string{"A", local} {
		want, _ := os.ReadFile(filepath.Join(cat, "disks", d, "files.jsonl"))
		if !bytes.Equal(got["drives/"+d+"/files.jsonl"], want) || len(got["drives/"+d+"/device.json"]) == 0 {
			t.Fatalf("drive %s: not its catalog as sent", d)
		}
	}
	members := lines("members.jsonl")
	if len(members) != 3 || bytes.Contains(got["members.jsonl"], []byte("$2a$")) || bytes.Contains(got["members.jsonl"], []byte("pass")) {
		t.Fatalf("members: %s", got["members.jsonl"])
	}
	if ds := lines("decisions.jsonl"); len(ds) != 2 || ds[0]["by"] != "Elio" || ds[0]["t"] == nil {
		t.Fatalf("decisions: %s", got["decisions.jsonl"])
	}
	// clip.mp4 was removed on request: only the other two contents' data, and the record of the removal.
	if n := []int{len(lines("looks.jsonl")), len(lines("speech.jsonl")), len(lines("frames.jsonl")), len(lines("originals.jsonl")), len(lines("removed.jsonl"))}; !slices.Equal(n, []int{2, 2, 10, 1, 1}) {
		t.Fatalf("looks, speech, frames, originals, removed: %v", n)
	}
	// AI output says so, and Apple's classes for people by age are never kept (AI Act, docs/ai.md).
	for _, name := range []string{"looks.jsonl", "speech.jsonl"} {
		if l := lines(name)[0]; l["ai_generated"] != true || l["generator"] == "" {
			t.Fatalf("%s not marked as AI output: %v", name, l)
		}
	}
	if strings.Contains(string(got["looks.jsonl"]), "child") || !strings.Contains(string(got["looks.jsonl"]), "stage") {
		t.Fatalf("looks keep a class for people by age: %s", got["looks.jsonl"])
	}
	if v := lines("frames.jsonl")[0]["v"].([]any); len(v) != embDim || v[0] != 0.25 {
		t.Fatalf("a frame vector of %d numbers, first %v", len(v), v[0])
	}
	copies := lines("light-copies.jsonl")
	if len(copies) != 2 || !strings.Contains(copies[0]["url"].(string), "X-Amz-Expires=604800") {
		t.Fatalf("light copies: %s", got["light-copies.jsonl"])
	}
	if !bytes.Equal(got["previews/"+shared.Thumb+"/3.jpg"], []byte("\xff\xd8\xff shared.mp4")) || got["previews/"+clip.Thumb+"/1.jpg"] != nil {
		t.Fatal("previews: want those the site has, not the removed clip's")
	}
	var manifest struct {
		Format int
		Files  []struct{ Path string }
	}
	must(t, json.Unmarshal(got["manifest.json"], &manifest))
	for name := range got { // every file is described, by name or by pattern
		if !slices.ContainsFunc(manifest.Files, func(f struct{ Path string }) bool {
			m, _ := path.Match(strings.NewReplacer("<drive>", "*", "<thumb>", "*", "<1 to 5>", "[1-5]").Replace(f.Path), name)
			return m
		}) {
			t.Fatalf("manifest (format %d) does not describe %s", manifest.Format, name)
		}
	}
	if manifest.Format != 1 || got["README.txt"] == nil {
		t.Fatalf("format %d, README %d bytes", manifest.Format, len(got["README.txt"]))
	}

	// Elio turns off "Send to the team" for L: the site drops the drive and what only it had; shared.mp4 is on A
	// too and stays; decisions stay; the original asked of L goes from the store.
	inApp := func(method, path, body string) int {
		w := httptest.NewRecorder()
		app.routes().ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
		return w.Code
	}
	if code := inApp("POST", "/api/drives", `{"uuid":"`+local+`","action":"options","local":true}`); code != 200 {
		t.Fatalf("turning off Send to the team: %d", code)
	}
	app.share()
	var has struct{ Drives, Thumbs, Looks, Speech, Emb, Proxies []string }
	must(t, app.team.call("GET", "/api/sync/have", nil, &has))
	if slices.Contains(has.Drives, local) || !slices.Contains(has.Drives, "A") {
		t.Fatalf("the site's drives after L was turned off: %v", has.Drives)
	}
	for _, l := range [][]string{has.Thumbs, has.Looks, has.Speech, has.Emb, has.Proxies} {
		if slices.Contains(l, private.Thumb) || !slices.Contains(l, shared.Thumb) {
			t.Fatalf("after L was turned off the site has %+v", has)
		}
	}
	if fileExists(filepath.Join(s.thumbDir(alpha, private.Thumb), "1.jpg")) || inStore(proxyKey(alpha, private.Thumb)) ||
		inStore(originalKey(alpha, req.ID, "private.mp4")) || !inStore(proxyKey(alpha, shared.Thumb)) {
		t.Fatal("the previews or files of what only L had are still on the site, or shared.mp4's light copy went")
	}
	var n int
	ok(call(av, "GET", "/api/clips", ""))
	must(t, s.admin.QueryRow(ctx, "select count(*) from decision where team = $1", alpha).Scan(&n))
	if n != 2 {
		t.Fatalf("%d decisions after L was turned off, want both", n)
	}
	must(t, s.admin.QueryRow(ctx, "select count(*) from look where team = $1", beta).Scan(&n))
	if n != 1 {
		t.Fatal("Beta lost its data when Alpha's drive went")
	}

	// From now on nothing of L leaves the Mac: not a new scan, its decisions, the originals, light copies.
	lmu.Lock()
	sent.Reset()
	lmu.Unlock()
	scanned(t, cat, drvL, local, "", nil) // a new scan keeps the choice
	must(t, app.reload())
	yes := true
	must(t, app.editDevice(local, drvL, func(d *Device) { d.Copies = &yes }))
	must(t, app.reload())
	app.share()
	app.serveOriginals(app.team)
	app.proxyNext()
	if code := inApp("POST", "/api/decide", `{"content":"`+private.Content+`","kind":"vote","value":"discard"}`); code != 200 {
		t.Fatalf("a decision on L alone: %d", code)
	}
	if code := inApp("POST", "/api/decide", `{"content":"`+shared.Content+`","kind":"copy","value":"`+local+`|shared.mp4"}`); code != 200 {
		t.Fatalf("keeping L's copy: %d", code)
	}
	lmu.Lock()
	log := sent.String()
	lmu.Unlock()
	for _, x := range []string{local, private.Thumb, private.Content, "private.mp4"} {
		if strings.Contains(log, x) {
			t.Fatalf("the Mac sent %q of the drive kept on it:\n%s", x, log)
		}
	}
	if !strings.Contains(log, "PUT /api/sync/drive/A") {
		t.Fatalf("drive A was not sent again:\n%s", log)
	}
	if d := app.catalog().decision(private.Content); d.Vote != "discard" {
		t.Fatalf("the decision on L's clip, kept on the Mac: %+v", d)
	}

	// Tidy up with AI never takes L through the site, even when the site has an AI: only the user's own key.
	asked := map[string]int{} // by key
	fakeAI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked[r.Header.Get("x-api-key")]++
		io.WriteString(w, `{"stop_reason":"end_turn","content":[{"type":"text","text":"{\"moves\":[]}"}]}`)
	}))
	defer fakeAI.Close()
	s.ai = AI{Provider: "anthropic", Model: "claude-opus-5-5", URL: fakeAI.URL, Key: "sk-site"}
	tidy := func(method, path, body string) (int, string) {
		w := httptest.NewRecorder()
		app.routes().ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
		return w.Code, w.Body.String()
	}
	if _, b := tidy("GET", "/api/tidy?uuid=A", ""); strings.Contains(b, `"site":""`) {
		t.Fatalf("drive A, sent to the team, does not tidy through the site: %s", b)
	}
	if _, b := tidy("GET", "/api/tidy?uuid="+local, ""); !strings.Contains(b, `"site":""`) {
		t.Fatalf("drive L would tidy through the site: %s", b)
	}
	if code, _ := tidy("POST", "/api/tidy/propose", `{"uuid":"`+local+`","consent":true,"via":"site"}`); code != 400 {
		t.Fatalf("L without a key of the user's own: %d, want 400", code)
	}
	if code, b := tidy("POST", "/api/tidy/ai", `{"provider":"anthropic","url":"`+fakeAI.URL+`","key":"sk-own"}`); code != 200 {
		t.Fatalf("saving the user's own AI: %d %s", code, b)
	}
	if code, _ := tidy("POST", "/api/tidy/propose", `{"uuid":"`+local+`","consent":true,"via":"site"}`); code != 409 {
		t.Fatalf("consent to the site's way for L: %d, want 409", code)
	}
	if code, b := tidy("POST", "/api/tidy/propose", `{"uuid":"`+local+`","consent":true,"via":"Anthropic (Claude)"}`); code != 200 || asked["sk-own"] != 1 || asked["sk-site"] != 0 {
		t.Fatalf("L with the user's own key: %d %s, asked %v", code, b, asked)
	}
	lmu.Lock()
	log = sent.String()
	lmu.Unlock()
	if strings.Contains(log, "/api/sync/tidy") {
		t.Fatalf("a tidy of L went to the site:\n%s", log)
	}
	ok(call(bo, "GET", "/api/me", "")) // Beta's owner is still fine
}

// TestRetention: requests for originals and computers' tokens go after 90 days unused, browser sessions after 30,
// an original's file after 7; seen from the sweep with a clock moved forward. Needs Postgres, as TestServer.
func TestRetention(t *testing.T) {
	admin := os.Getenv("RUSHBIN_TEST_DB")
	if admin == "" {
		t.Skip("RUSHBIN_TEST_DB not set")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, admin)
	must(t, err)
	name := "rushbin_test_" + token()[:8]
	_, err = conn.Exec(ctx, "create database "+name)
	must(t, err)
	defer func() { conn.Exec(ctx, "drop database "+name+" with (force)"); conn.Close(ctx) }()
	u, _ := url.Parse(admin)
	u.Path = "/" + name
	t.Setenv("RUSHBIN_APP_PASSWORD", token())
	s, err := openServer(ctx, u.String(), t.TempDir())
	must(t, err)
	defer s.db.Close()
	defer s.admin.Close()
	deleted := map[string]bool{}
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" {
			deleted[r.URL.Path] = true
		}
	}))
	defer fake.Close()
	s.store = &Store{Endpoint: fake.URL, Bucket: "b", KeyID: "k", Secret: "s", Region: "auto"}

	_, err = newTeam(ctx, s, "T", "o@t.test")
	must(t, err)
	var team, person int64
	must(t, s.admin.QueryRow(ctx, "select id from team").Scan(&team))
	must(t, s.admin.QueryRow(ctx, "insert into person (email, name, pass) values ('o@t.test', 'O', '!') returning id").Scan(&person))
	_, err = s.admin.Exec(ctx, "insert into member (team, person, role) values ($1, $2, 'owner')", team, person)
	must(t, err)
	for _, dev := range []string{"", "Mac"} {
		_, err = s.admin.Exec(ctx, "insert into session (hash, person, team, device) values ($1, $2, $3, $4)", hash(token()), person, team, dev)
		must(t, err)
	}
	var id int64 // an original sent and downloaded
	must(t, s.admin.QueryRow(ctx, `insert into request (team, device, rel, content, size, by_name, state)
		values ($1, 'D', 'P/a.mov', 'c:x', 1, 'O', 'ready') returning id`, team).Scan(&id))
	key := "/b/" + originalKey(team, id, "a.mov")

	count := func(q string) (n int) {
		t.Helper()
		must(t, s.admin.QueryRow(ctx, q).Scan(&n))
		return n
	}
	now := time.Now()
	day := 24 * time.Hour
	s.sweep(ctx, now.Add(6*day))
	if deleted[key] || count("select count(*) from request where state = 'ready'") != 1 || count("select count(*) from session") != 2 {
		t.Fatal("after 6 days something went")
	}
	s.sweep(ctx, now.Add(8*day))
	if !deleted[key] || count("select count(*) from request where state = 'expired'") != 1 {
		t.Fatal("after 8 days the original's file should be gone, its request kept")
	}
	s.sweep(ctx, now.Add(31*day))
	if count("select count(*) from session where device = ''") != 0 || count("select count(*) from session where device <> ''") != 1 {
		t.Fatal("after 31 days unused, the browser's session goes and the computer's token stays")
	}
	delete(deleted, key) // a file the store did not delete at 7 days goes with its request
	s.sweep(ctx, now.Add(89*day))
	if deleted[key] || count("select count(*) from request") != 1 || count("select count(*) from session") != 1 {
		t.Fatal("after 89 days the request or the computer's token went")
	}
	s.sweep(ctx, now.Add(91*day))
	if !deleted[key] || count("select count(*) from request") != 0 || count("select count(*) from session") != 0 {
		t.Fatal("after 91 days unused the request, its file and the computer's token should be gone")
	}
}
