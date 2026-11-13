package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// TestServer runs two teams on one site, as a hosted rushbin would: Studio (owner Dana, editor Bea,
// viewer Carlo) and Other (owner Olga, Dana invited as viewer). It checks that Postgres keeps the teams
// apart even for a query without a team filter, what each role may do, decisions travelling both ways
// with who took them, and every management action.
// Needs Postgres: RUSHBIN_TEST_DB is an admin URL, for example postgres://postgres@127.0.0.1:54329/postgres.
func TestServer(t *testing.T) {
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
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	// The uptime check: ok while the database answers and the nightly copies are fine, 503 "backup" when not.
	health := func() (int, string) {
		res, err := http.Get(srv.URL + "/health")
		must(t, err)
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, strings.TrimSpace(string(b))
	}
	if code, body := health(); code != 200 || body != "ok" {
		t.Fatalf("health %d %q", code, body)
	}
	must(t, os.WriteFile(filepath.Join(s.dir, "backup.json"), []byte(`{"at":"2026-11-01T02:30:00Z","ok":true}`), 0o644))
	if code, body := health(); code != 503 || body != "backup" {
		t.Fatalf("health with an old copy %d %q", code, body)
	}
	must(t, os.Remove(filepath.Join(s.dir, "backup.json")))

	browser := func() *http.Client { jar, _ := cookiejar.New(nil); return &http.Client{Jar: jar} }
	call := func(c *http.Client, method, path, body string, header bool) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		if header {
			req.Header.Set("X-Rushbin", "1")
			req.Header.Set("Origin", srv.URL)
		}
		r, err := c.Do(req)
		must(t, err)
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		return r.StatusCode, string(b)
	}
	ok := func(code int, body string) string {
		t.Helper()
		if code != 200 {
			t.Fatalf("%d %s", code, body)
		}
		return body
	}
	want := func(code int, c *http.Client, method, path, body string) {
		t.Helper()
		if got, msg := call(c, method, path, body, true); got != code {
			t.Fatalf("%s %s: %d %s, want %d", method, path, got, msg, code)
		}
	}
	jsonOf := func(body string, v any) { t.Helper(); must(t, json.Unmarshal([]byte(body), v)) }
	link := func(body, kind string) string {
		var r struct{ URL string }
		jsonOf(body, &r)
		return r.URL[strings.Index(r.URL, "#"+kind+"=")+len(kind)+2:]
	}
	dana, bea, carlo, olga, nobody := browser(), browser(), browser(), browser(), browser()

	// Teams are made by whoever runs the site; each owner joins through the printed link.
	tok, err := newTeam(ctx, s, "Studio", "d@studio.example")
	must(t, err)
	ok(call(dana, "POST", "/api/auth/join", `{"token":"`+tok+`","name":"Dana","password":"0123456789"}`, true))
	want(403, nobody, "POST", "/api/auth/join", `{"token":"`+tok+`","name":"X","password":"0123456789"}`) // once only
	tok, err = newTeam(ctx, s, "Other", "o@other.com")
	must(t, err)
	ok(call(olga, "POST", "/api/auth/join", `{"token":"`+tok+`","name":"Olga","password":"olgaolgaolga"}`, true))

	if code, _ := call(dana, "POST", "/api/team/invite", `{"email":"b@studio.example","role":"editor"}`, false); code != 403 {
		t.Fatalf("POST without the header: %d, want 403", code)
	}
	tok = link(ok(call(dana, "POST", "/api/team/invite", `{"email":"b@studio.example","role":"editor"}`, true)), "invite")
	ok(call(bea, "POST", "/api/auth/join", `{"token":"`+tok+`","name":"Bea","password":"abcdefghij"}`, true))
	tok = link(ok(call(dana, "POST", "/api/team/invite", `{"email":"c@studio.example","role":"viewer"}`, true)), "invite")
	ok(call(carlo, "POST", "/api/auth/join", `{"token":"`+tok+`","name":"Carlo","password":"carlocarlo"}`, true))
	// Dana is also invited to Other: he keeps his password, and can switch.
	tok = link(ok(call(olga, "POST", "/api/team/invite", `{"email":"d@studio.example","role":"viewer"}`, true)), "invite")
	if code, msg := call(nobody, "POST", "/api/auth/join", `{"token":"`+tok+`","password":"not his password"}`, true); code != 403 || !strings.Contains(msg, "already has a rushbin account") {
		t.Fatalf("joining with another's address: %d %s, want 403 telling the address is taken", code, msg)
	}
	tok = link(ok(call(olga, "POST", "/api/team/invite", `{"email":"d@studio.example","role":"viewer"}`, true)), "invite")
	ok(call(browser(), "POST", "/api/auth/join", `{"token":"`+tok+`","password":"0123456789"}`, true))

	// What each role may not do.
	want(403, bea, "POST", "/api/team/invite", `{"email":"x@studio.example","role":"viewer"}`)
	want(403, bea, "POST", "/api/team/role", `{"id":1,"role":"owner"}`)
	want(403, carlo, "POST", "/api/team/rename", `{"name":"Mine"}`)
	if code, _ := call(http.DefaultClient, "POST", "/api/sync/login", `{"email":"c@studio.example","password":"carlocarlo","device":"Carlo's Mac"}`, false); code != 403 {
		t.Fatalf("a viewer connecting a Mac: %d, want 403", code)
	}
	if code, _ := call(nobody, "GET", "/api/clips", "", false); code != 401 {
		t.Fatalf("clips without signing in: %d, want 401", code)
	}

	// Dana's Mac sends a drive with two clips (one copied), a preview and its analysis.
	tmp := t.TempDir()
	drv, cat := filepath.Join(tmp, "A"), filepath.Join(tmp, "catalog")
	must(t, os.MkdirAll(filepath.Join(drv, "P"), 0o755))
	ffmpeg(t, "testsrc=duration=2:size=320x240:rate=25", filepath.Join(drv, "P", "clip.mp4"))
	ffmpeg(t, "testsrc2=duration=2:size=320x240:rate=25", filepath.Join(drv, "P", "other.mp4"))
	copyFile(t, filepath.Join(drv, "P", "clip.mp4"), filepath.Join(drv, "P", "copy.mp4"))
	scanned(t, cat, drv, "A", "", nil)
	app := &App{dir: cat, mounted: map[string]string{"A": drv}, seen: map[string]string{}, ignored: map[string]bool{}, queue: make(chan string, 64)}
	yes := true // light copies for the team are chosen per drive (off unless chosen)
	must(t, app.editDevice("A", drv, func(d *Device) { d.Copies = &yes }))
	must(t, app.reload())
	th := app.catalog().byKey["A|P/clip.mp4"].Thumb // the clip whose original is asked for below
	must(t, os.MkdirAll(filepath.Join(cat, "thumbs", th), 0o755))
	for k := 1; k <= 5; k++ {
		must(t, os.WriteFile(filepath.Join(cat, "thumbs", th, fmt.Sprintf("%d.jpg", k)), []byte("\xff\xd8\xff fake jpeg"), 0o644))
	}
	must(t, os.WriteFile(filepath.Join(cat, "vision.jsonl"), []byte(`{"thumb":"`+th+`","frames":[{"contrast":0.3,"labels":{"crowd":0.9}}]}`+"\n"), 0o644))
	must(t, app.reload())
	var login struct{ Token, Name, Team string }
	jsonOf(ok(call(http.DefaultClient, "POST", "/api/sync/login", `{"email":"d@studio.example","password":"0123456789","device":"Studio Mac"}`, false)), &login)
	if login.Name != "Dana" || login.Team != "Studio" {
		t.Fatalf("app sign-in = %+v", login)
	}
	app.team = &Team{URL: srv.URL, Token: login.Token, Name: login.Name, Team: login.Team}
	app.share()

	// Tidy up with AI through the site's AI: the Mac sends the description, the site asks its AI (a fake Anthropic
	// here) and answers the moves, which the Mac checks against the drive. The key reaches only the provider; editors
	// and owners only; at most tidyCap a month per team.
	const siteKey = "sk-ant-site-key"
	asked := 0
	fakeAI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != siteKey {
			w.WriteHeader(401)
			io.WriteString(w, `{"type":"error","error":{"message":"invalid x-api-key"}}`)
			return
		}
		asked++
		io.WriteString(w, `{"stop_reason":"end_turn","content":[{"type":"text",
			"text":"{\"moves\":[{\"from\":\"P\",\"to\":\"Shoot/P\",\"reason\":\"one project\"},{\"from\":\"Q\",\"to\":\"../out\",\"reason\":\"bad\"}]}"}]}`)
	}))
	defer fakeAI.Close()
	inApp := func(method, path, body string) (int, string) { // the app's own engine, as its page calls it
		w := httptest.NewRecorder()
		app.routes().ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
		return w.Code, w.Body.String()
	}
	if code, body := inApp("GET", "/api/tidy", ""); code != 200 || !strings.Contains(body, `"site":""`) {
		t.Fatalf("a team site without a key: %d %s, want the personal key", code, body)
	}
	if code, _ := inApp("POST", "/api/tidy/propose", `{"uuid":"A","consent":true,"via":"key"}`); code != 400 {
		t.Fatalf("a team site without a key and no personal key: %d, want 400", code)
	}
	want(404, bea, "POST", "/api/sync/tidy", `{"desc":"Drive: 1 files"}`)
	s.ai, s.tidyCap = AI{Provider: "anthropic", Model: "claude-opus-5-5", URL: fakeAI.URL, Key: siteKey}, 2
	if code, body := inApp("GET", "/api/tidy", ""); code != 200 || !strings.Contains(body, `"site":"`+strings.TrimPrefix(srv.URL, "http://")+`"`) {
		t.Fatalf("a team site with a key: %d %s", code, body)
	}
	if code, _ := inApp("POST", "/api/tidy/propose", `{"uuid":"A","consent":true,"via":"Anthropic (Claude)"}`); code != 409 || asked != 0 {
		t.Fatalf("consent to the personal key's way: %d, asked %d, want 409 and nothing sent", code, asked)
	}
	code, plan := inApp("POST", "/api/tidy/propose", `{"uuid":"A","consent":true,"via":"site"}`)
	var moves []TidyMove
	jsonOf(plan, &moves)
	if code != 200 || asked != 1 || len(moves) != 2 || !moves[0].OK || moves[1].OK || strings.Contains(plan, siteKey) {
		t.Fatalf("propose through the site: %d %s, asked %d", code, plan, asked)
	}
	want(403, carlo, "POST", "/api/sync/tidy", `{"desc":"Drive: 1 files"}`) // a viewer
	if body := ok(call(bea, "POST", "/api/sync/tidy", `{"desc":"Drive: 1 files"}`, true)); asked != 2 || strings.Contains(body, siteKey) {
		t.Fatalf("Bea, an editor: %s, asked %d", body, asked)
	}
	if code, msg := inApp("POST", "/api/tidy/propose", `{"uuid":"A","via":"site"}`); code != 502 || asked != 2 || !strings.Contains(msg, "used its 2 tidy-ups with AI for this month") {
		t.Fatalf("past the month's cap: %d %s, asked %d", code, msg, asked)
	}
	ok(call(olga, "POST", "/api/sync/tidy", `{"desc":"Drive: 1 files"}`, true)) // Other counts its own
	if body := ok(call(nobody, "GET", "/api/site", "", false)); !strings.Contains(body, `"tidy":true`) || !strings.Contains(body, `"ai":"Anthropic (Claude)"`) || strings.Contains(body, siteKey) {
		t.Fatalf("/api/site: %s", body)
	}
	s.ai = AI{}

	// Search by content: the Mac sends its frame embeddings and what is said; the site embeds the words of a
	// search (a fake text encoder here, answering with the first frame's own vector) and merges both.
	vec := make([]float32, embDim)
	for i := range vec {
		vec[i] = float32((i*7)%13-6) / 40
	}
	var emb []byte
	for k := 0; k < 5; k++ {
		for _, x := range vec {
			emb = binary.LittleEndian.AppendUint16(emb, toHalf(x))
		}
	}
	must(t, os.WriteFile(filepath.Join(cat, "thumbs", th, "emb.bin"), emb, 0o644))
	app.catalog().setSpeech(app.catalog().list[1].Thumb, newSpeech([]Segment{{S: 3, E: 5, T: "Benvenuti a Sanremo"}}))
	textEnc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		send(w, map[string][]float32{"v": vec})
	}))
	defer textEnc.Close()
	s.textURL = textEnc.URL
	app.share()
	var found []Row
	jsonOf(ok(call(carlo, "GET", "/api/search?q=pubblico+in+festa", "", false)), &found)
	if len(found) == 0 || found[0].Thumb != th || !found[0].Seen {
		t.Fatalf("search by looks = %+v, want the clip whose frames match first", found)
	}
	jsonOf(ok(call(carlo, "GET", "/api/search?q=sanremo", "", false)), &found)
	if len(found) == 0 || found[0].Said == nil || found[0].Said.S != 3 {
		t.Fatalf("search in what is said = %+v", found)
	}
	// A wrong line: a viewer cannot hide it; an editor hides it, search skips it and the Mac learns it; shown again,
	// it is found again. The transcript says, wherever it goes, that an AI wrote it; who checked it is kept.
	sp := app.catalog().list[1]
	hide := func(v string) string { return `{"content":"` + sp.Content + `","kind":"hide","value":"` + v + `"}` }
	want(403, carlo, "POST", "/api/decide", hide("3"))
	want(400, bea, "POST", "/api/decide", hide("4")) // no line starts at 4 s
	want(200, bea, "POST", "/api/decide", hide("3"))
	said := func() bool { // found by what is said (the fake text encoder finds every search by looks too)
		found = nil // json fills the old rows in place: a Said left out would stay
		jsonOf(ok(call(carlo, "GET", "/api/search?q=sanremo", "", false)), &found)
		return slices.ContainsFunc(found, func(r Row) bool { return r.Said != nil })
	}
	if said() {
		t.Fatalf("a hidden line is still found: %+v", found)
	}
	want(200, bea, "POST", "/api/decide", `{"content":"`+sp.Content+`","kind":"check","value":"yes"}`)
	var sheet struct {
		Hidden    []float64
		Checked   bool
		CheckedBy string `json:"checked_by"`
		Speech    struct {
			Generator string
			AI        bool `json:"ai_generated"`
		}
	}
	jsonOf(ok(call(carlo, "GET", "/api/clip?device="+url.QueryEscape(sp.Device)+"&id="+url.QueryEscape(sp.ID), "", false)), &sheet)
	if !slices.Equal(sheet.Hidden, []float64{3}) || !sheet.Checked || sheet.CheckedBy != "Bea" || !sheet.Speech.AI || sheet.Speech.Generator != speechGenerator {
		t.Fatalf("the clip sheet: %+v", sheet)
	}
	app.pull()
	if d := app.catalog().decision(sp.Content); !slices.Equal(d.Hidden, []float64{3}) || !d.Checked || app.catalog().said(sp, "sanremo") != nil {
		t.Fatalf("the Mac's copy of the team's decisions: %+v", d)
	}
	want(200, bea, "POST", "/api/decide", hide(""))
	if !said() {
		t.Fatalf("a line shown again is not found: %+v", found)
	}
	if n := countJSON(t, ok(call(olga, "GET", "/api/search?q=pubblico+in+festa", "", false))); n != 0 {
		t.Fatalf("Olga's search finds %d of Studio's clips", n)
	}

	// Proxies: the Mac makes one per content and uploads it to the store (a fake S3 here) with links
	// the site signs; members watch from the store, other teams cannot.
	stored, types, parts, aborted := map[string][]byte{}, map[string]string{}, map[string][]byte{}, 0 // parts by ETag
	fakeS3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("X-Amz-Signature") == "" {
			http.Error(w, "unsigned", 403)
			return
		}
		switch {
		case r.Method == "POST" && q.Has("uploads"):
			fmt.Fprint(w, `<InitiateMultipartUploadResult><UploadId>up-`+r.URL.Path+`</UploadId></InitiateMultipartUploadResult>`)
		case r.Method == "PUT" && q.Has("partNumber"):
			etag := fmt.Sprintf(`"%s-%s"`, q.Get("uploadId"), q.Get("partNumber"))
			parts[etag], _ = io.ReadAll(r.Body)
			w.Header().Set("ETag", etag)
		case r.Method == "POST" && q.Get("uploadId") == "up-"+r.URL.Path:
			var in struct{ Part []struct{ ETag string } }
			xml.NewDecoder(r.Body).Decode(&in)
			var all []byte
			for _, p := range in.Part {
				all = append(all, parts[p.ETag]...)
			}
			stored[r.URL.Path] = all
		case r.Method == "DELETE" && q.Has("uploadId"):
			aborted++
		case r.Method == "DELETE":
			delete(stored, r.URL.Path)
		case r.Method == "PUT":
			stored[r.URL.Path], _ = io.ReadAll(r.Body)
			types[r.URL.Path] = r.Header.Get("Content-Type") // kept and answered, as S3 and R2 do
		case r.Method == "HEAD" || r.Method == "GET":
			b, ok := stored[r.URL.Path]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Length", fmt.Sprint(len(b)))
			w.Header().Set("Content-Type", types[r.URL.Path])
			w.Write(b)
		}
	}))
	defer fakeS3.Close()
	s.store = &Store{Endpoint: fakeS3.URL, Region: "auto", Bucket: "bucket", Prefix: "rushbin/", KeyID: "k", Secret: "s"}
	if !app.proxyNext() || !app.proxyNext() || app.proxyNext() {
		t.Fatal("want two proxies made, then nothing left")
	}
	if len(stored) != 2 {
		t.Fatalf("store has %d files, want 2", len(stored))
	}
	var play struct{ URL string }
	jsonOf(ok(call(carlo, "GET", "/api/proxy/"+th, "", false)), &play)
	if r, err := http.Get(play.URL); err != nil || r.StatusCode != 200 {
		t.Fatalf("watching the proxy: %v %v", r, err)
	} else if b, _ := io.ReadAll(r.Body); len(b) < 12 || string(b[4:8]) != "ftyp" {
		t.Fatal("the proxy is not an MP4")
	}
	want(403, carlo, "POST", "/api/sync/proxy/"+th, "")
	if code, _ := call(olga, "GET", "/api/proxy/"+th, "", false); code != 404 {
		t.Fatalf("Olga watching Studio's proxy: %d, want 404", code)
	}

	// An original from the Mac that has the drive: Bea asks, Dana's Mac sends it to the store, Bea downloads it.
	want(403, carlo, "POST", "/api/original", `{"device":"A","id":"P/clip.mp4"}`) // viewers cannot
	var req struct{ ID int64 }
	jsonOf(ok(call(bea, "POST", "/api/original", `{"device":"A","id":"P/clip.mp4"}`, true)), &req)
	var orig Request
	jsonOf(ok(call(bea, "GET", fmt.Sprintf("/api/original/%d", req.ID), "", false)), &orig)
	if orig.State != "waiting" || orig.Mac != "" {
		t.Fatalf("before any Mac said anything: %+v", orig)
	}
	app.serveOriginals(app.team)
	jsonOf(ok(call(bea, "GET", fmt.Sprintf("/api/original/%d", req.ID), "", false)), &orig)
	want_, _ := os.ReadFile(filepath.Join(drv, "P", "clip.mp4"))
	sum := sha256.Sum256(want_)
	if orig.State != "ready" || orig.Mac != "Studio Mac" || orig.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("after the Mac sent it: %+v", orig)
	}
	if !strings.Contains(orig.URL, "response-content-disposition=") {
		t.Fatalf("the link does not name the file: %s", orig.URL)
	}
	if r, err := http.Get(orig.URL); err != nil || r.StatusCode != 200 {
		t.Fatalf("downloading: %v %v", r, err)
	} else if got, _ := io.ReadAll(r.Body); !bytes.Equal(got, want_) {
		t.Fatal("the downloaded original differs from the file on the drive")
	}
	if code, _ := call(olga, "GET", fmt.Sprintf("/api/original/%d", req.ID), "", false); code != 404 {
		t.Fatalf("Olga sees Studio's request: %d", code)
	}

	// Over one PUT's limit (here 1 byte) the original goes in parts (here of 1000 bytes): the site starts
	// the multipart upload, the Mac sends part after part, the site joins them and checks the size.
	maxPut, partSize = 1, 1000
	defer func() { maxPut, partSize = 5<<30, 64<<20 }()
	jsonOf(ok(call(bea, "POST", "/api/original", `{"device":"A","id":"P/clip.mp4"}`, true)), &req)
	app.serveOriginals(app.team)
	jsonOf(ok(call(bea, "GET", fmt.Sprintf("/api/original/%d", req.ID), "", false)), &orig)
	if n, _ := partsOf(int64(len(want_))); orig.State != "ready" || n < 2 || len(parts) != int(n) {
		t.Fatalf("after the Mac sent it in %d parts: %+v", len(parts), orig)
	}
	if r, err := http.Get(orig.URL); err != nil || r.StatusCode != 200 {
		t.Fatalf("downloading: %v %v", r, err)
	} else if got, _ := io.ReadAll(r.Body); !bytes.Equal(got, want_) {
		t.Fatal("the original joined from parts differs from the file on the drive")
	}
	// A Mac that declares the right hash but sends one part only: the site's own size check fails it.
	// A Mac that reports an error: the site drops the parts.
	for _, sentBody := range []func(up Upload, etag string) map[string]any{
		func(up Upload, etag string) map[string]any {
			return map[string]any{"sha256": orig.SHA256, "upload_id": up.UploadID, "etags": []string{etag}}
		},
		func(up Upload, etag string) map[string]any {
			return map[string]any{"error": "part 2 of 9: upload: 500", "upload_id": up.UploadID}
		},
	} {
		jsonOf(ok(call(bea, "POST", "/api/original", `{"device":"A","id":"P/clip.mp4"}`, true)), &req)
		var up Upload
		if err := app.team.call("POST", fmt.Sprintf("/api/sync/requests/%d/claim", req.ID), nil, &up); err != nil || len(up.Parts) < 2 {
			t.Fatalf("claim: %v %+v", err, up)
		}
		put, _ := http.NewRequest("PUT", up.Parts[0], bytes.NewReader(want_[:partSize]))
		res, err := http.DefaultClient.Do(put)
		if err != nil {
			t.Fatal(err)
		}
		app.team.call("POST", fmt.Sprintf("/api/sync/requests/%d/sent", req.ID), sentBody(up, res.Header.Get("ETag")), nil)
		orig = Request{}
		jsonOf(ok(call(bea, "GET", fmt.Sprintf("/api/original/%d", req.ID), "", false)), &orig)
		if orig.State != "failed" || orig.URL != "" {
			t.Fatalf("a short or failed upload: %+v", orig)
		}
	}
	if aborted != 1 {
		t.Fatalf("aborted %d multipart uploads, want 1", aborted)
	}

	count := func(c *http.Client, path string) int {
		t.Helper()
		var rows []json.RawMessage
		jsonOf(ok(call(c, "GET", path, "", false)), &rows)
		return len(rows)
	}
	for who, c := range map[string]*http.Client{"Dana": dana, "Bea": bea, "Carlo": carlo} {
		if n := count(c, "/api/clips"); n != 2 {
			t.Fatalf("%s sees %d clips, want 2", who, n)
		}
	}
	if body := ok(call(carlo, "GET", "/thumbs/"+th+"/3.jpg", "", false)); !strings.HasPrefix(body, "\xff\xd8\xff") {
		t.Fatal("Carlo does not get the preview")
	}
	if n := count(carlo, "/api/clips?label=crowd"); n != 1 {
		t.Fatalf("the analysis did not reach the team: %d clips with crowd", n)
	}

	// The other team sees nothing of Studio, and neither does a query that forgets its team.
	if n := count(olga, "/api/clips"); n != 0 {
		t.Fatalf("Olga sees %d of Studio's clips", n)
	}
	if code, _ := call(olga, "GET", "/thumbs/"+th+"/3.jpg", "", false); code != 404 {
		t.Fatalf("Olga gets Studio's preview: %d", code)
	}
	var artide, other int64
	must(t, s.admin.QueryRow(ctx, "select id from team where name = 'Studio'").Scan(&artide))
	must(t, s.admin.QueryRow(ctx, "select id from team where name = 'Other'").Scan(&other))
	var n int
	must(t, s.db.QueryRow(ctx, "select count(*) from drive").Scan(&n)) // no team set
	if n != 0 {
		t.Fatalf("without a team, rushbin_app reads %d drives", n)
	}
	must(t, s.inTeam(ctx, other, func(tx pgx.Tx) error { return tx.QueryRow(ctx, "select count(*) from drive").Scan(&n) }))
	if n != 0 {
		t.Fatalf("as Other, an unfiltered query reads %d drives", n)
	}
	must(t, s.inTeam(ctx, other, func(tx pgx.Tx) error { return tx.QueryRow(ctx, "select count(*) from tidy_run").Scan(&n) }))
	if n != 1 {
		t.Fatalf("as Other, an unfiltered query reads %d tidy-ups, want its own 1", n)
	}
	if err := s.inTeam(ctx, other, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "insert into decision (team, content, kind, value, by_name) values ($1, 'c:x', 'vote', 'keep', 'X')", artide)
		return err
	}); err == nil {
		t.Fatal("as Other, writing a decision into Studio worked")
	}

	// Someone filmed in clip.mp4 asks to be removed: only an owner of its team removes what the site holds of
	// it; the clip stays listed, marked removed, and whatever a Mac sends of it later is dropped.
	ndjson := func(path, line string) {
		t.Helper()
		must(t, app.team.send("POST", path, strings.NewReader(line+"\n"), "application/x-ndjson", nil))
	}
	ndjson("/api/sync/speech", `{"thumb":"`+th+`","segs":[{"s":1,"e":2,"t":"I am Mario"}]}`)
	// A body bigger than one batch is written in batches, all of it; a bad line anywhere keeps nothing of it.
	var many strings.Builder
	for i := range 3*syncBatch + 7 {
		fmt.Fprintf(&many, `{"thumb":"c_many%d","segs":[{"s":0,"e":1,"t":"line %d"}]}`+"\n", i, i)
	}
	must(t, app.team.send("POST", "/api/sync/speech", strings.NewReader(many.String()), "application/x-ndjson", nil))
	if err := app.team.send("POST", "/api/sync/speech", strings.NewReader(`{"thumb":"c_bad_after","segs":[]}`+"\n{oops\n"), "application/x-ndjson", nil); err == nil {
		t.Fatal("a body with a bad line was taken")
	}
	var nMany, nBad int
	must(t, s.inTeam(ctx, artide, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `select (select count(*) from speech where thumb like 'c_many%'), (select count(*) from speech where thumb = 'c_bad_after')`).Scan(&nMany, &nBad)
	}))
	if nMany != 3*syncBatch+7 || nBad != 0 {
		t.Fatalf("batched sync kept %d of %d lines, and %d of a refused body", nMany, 3*syncBatch+7, nBad)
	}
	must(t, s.inTeam(ctx, artide, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "delete from speech where thumb like 'c_many%'")
		return err
	}))
	held := func() (n int) { // its rows in the database
		t.Helper()
		must(t, s.inTeam(ctx, artide, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `select (select count(*) from look where thumb = $1) + (select count(*) from speech where thumb = $1)
				+ (select count(*) from frame where thumb = $1) + (select count(*) from proxy where thumb = $1)`, th).Scan(&n)
		}))
		return n
	}
	inStore := func() (n int) { // its light copy and originals
		for k := range stored {
			if strings.Contains(k, "/proxy/"+th) || strings.Contains(k, "/originals/") {
				n++
			}
		}
		return n
	}
	if n, m := held(), inStore(); n != 8 || m < 3 { // 1 look, 1 transcript, 5 frames, 1 proxy; the proxy and 2 originals
		t.Fatalf("before the removal %d rows and %d objects", n, m)
	}
	want(403, bea, "POST", "/api/forget/"+th, "")
	want(403, carlo, "POST", "/api/forget/"+th, "")
	want(404, olga, "POST", "/api/forget/"+th, "") // not a clip of her team
	// A Mac sending what it has of the clip in the very instant of the removal: the removal waits for it, then
	// takes what it sent too.
	inSend, release, done := make(chan struct{}), make(chan struct{}), make(chan int)
	go s.syncing(ctx, artide, func(tx pgx.Tx) error {
		close(inSend)
		<-release
		_, err := tx.Exec(ctx, `insert into speech (team, thumb, segs) values ($1, $2, '[]') on conflict (team, thumb) do update set segs = '[]'`, artide, th)
		return err
	})
	<-inSend
	go func() { code, _ := call(dana, "POST", "/api/forget/"+th, "", true); done <- code }()
	select {
	case code := <-done:
		t.Fatalf("the removal ended (%d) while a Mac was sending the clip", code)
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	if code := <-done; code != 200 {
		t.Fatalf("the removal answered %d", code)
	}
	if n, m := held(), inStore(); n != 0 || m != 0 {
		t.Fatalf("after the removal %d rows and %d objects are left", n, m)
	}
	if _, err := os.Stat(s.thumbDir(artide, th)); !os.IsNotExist(err) {
		t.Fatalf("the previews are still on the site: %v", err)
	}
	var removed struct {
		Removed bool
		Proxy   bool
		Look    *Look
		Speech  *Speech
	}
	jsonOf(ok(call(carlo, "GET", "/api/clip?device=A&id=P/clip.mp4", "", false)), &removed)
	if !removed.Removed || removed.Proxy || removed.Look != nil || removed.Speech != nil || count(carlo, "/api/clips") != 2 {
		t.Fatalf("the removed clip shows %+v", removed)
	}
	want(410, bea, "POST", "/api/original", `{"device":"A","id":"P/clip.mp4"}`)
	var has struct{ Thumbs, Looks, Speech, Emb, Proxies []string }
	must(t, app.team.call("GET", "/api/sync/have", nil, &has))
	for _, l := range [][]string{has.Thumbs, has.Looks, has.Speech, has.Emb, has.Proxies} {
		if !slices.Contains(l, th) {
			t.Fatalf("have = %+v, want the removed clip in every list", has)
		}
	}
	// A Mac that sends it anyway: taken and dropped, so it goes on with the rest.
	app.share()
	ndjson("/api/sync/looks", `{"thumb":"`+th+`","frames":[{"contrast":0.3}]}`)
	ndjson("/api/sync/speech", `{"thumb":"`+th+`","segs":[{"s":1,"e":2,"t":"I am Mario"}]}`)
	ndjson("/api/sync/emb", `{"thumb":"`+th+`","emb":"`+base64.StdEncoding.EncodeToString(emb)+`"}`)
	must(t, app.team.send("PUT", "/api/sync/thumbs/"+th+"/1", strings.NewReader("\xff\xd8\xff fake jpeg"), "image/jpeg", nil))
	if err := app.team.call("POST", "/api/sync/proxy/"+th, nil, nil); err == nil || !strings.Contains(err.Error(), "410") {
		t.Fatalf("a proxy upload link for the removed clip: %v, want 410", err)
	}
	if app.proxyNext() {
		t.Fatal("the Mac makes a proxy of the removed clip")
	}
	if n := held(); n != 0 || fileExists(filepath.Join(s.thumbDir(artide, th), "1.jpg")) {
		t.Fatalf("after a Mac sent the removed clip again: %d rows, preview %v", n, fileExists(filepath.Join(s.thumbDir(artide, th), "1.jpg")))
	}

	// Bea discards on the site; Dana sees who did it, on the site and in his app. Carlo, a viewer, cannot decide.
	var rows []Row
	jsonOf(ok(call(bea, "GET", "/api/clips", "", false)), &rows)
	var other4 Row
	for _, r := range rows {
		if r.Name == "other.mp4" {
			other4 = r
		}
	}
	decide := `{"content":"` + other4.Content + `","kind":"vote","value":"discard"}`
	want(403, carlo, "POST", "/api/decide", decide)
	version := func() float64 {
		var st struct{ Version float64 }
		json.Unmarshal([]byte(ok(call(bea, "GET", "/api/status", "", false))), &st)
		return st.Version
	}
	before := version()
	ok(call(bea, "POST", "/api/decide", decide, true))
	if after := version(); after <= before { // the team's pages refresh when their data moves
		t.Fatalf("status version %v after a decision, %v before", after, before)
	}
	var clip struct {
		Vote   string `json:"vote"`
		VoteBy string `json:"vote_by"`
	}
	jsonOf(ok(call(dana, "GET", "/api/clip?device=A&id=P/other.mp4", "", false)), &clip)
	if clip.Vote != "discard" || clip.VoteBy != "Bea" {
		t.Fatalf("Dana sees %+v, want discard by Bea", clip)
	}
	app.pull()
	if d := app.catalog().decision(other4.Content); d.Vote != "discard" || d.VoteBy != "Bea" {
		t.Fatalf("Dana's app has %+v, want discard by Bea", d)
	}
	rec := httptest.NewRecorder()
	clipC := app.catalog().byKey["A|P/clip.mp4"].Content
	app.decide(rec, httptest.NewRequest("POST", "/api/decide", strings.NewReader(`{"content":"`+clipC+`","kind":"copy","value":"A|P/clip.mp4"}`)))
	if rec.Code != 200 {
		t.Fatalf("decision from the app: %d %s", rec.Code, rec.Body)
	}
	var dups []Group
	jsonOf(ok(call(bea, "GET", "/api/dups", "", false)), &dups)
	if len(dups) != 1 || dups[0].Copy != "A|P/clip.mp4" || dups[0].CopyBy != "Dana" {
		t.Fatalf("Bea sees %+v, want the copy kept by Dana", dups)
	}

	// Managing the team.
	var team struct {
		Members []struct {
			ID   int64
			Name string
			Role string
			Macs []struct {
				ID   int64
				Name string
			}
		}
	}
	jsonOf(ok(call(dana, "GET", "/api/team", "", false)), &team)
	ids := map[string]int64{}
	for _, m := range team.Members {
		ids[m.Name] = m.ID
		if m.Name == "Dana" && (len(m.Macs) != 1 || m.Macs[0].Name != "Studio Mac") {
			t.Fatalf("Dana's Macs = %+v", m.Macs)
		}
	}
	if len(team.Members) != 3 {
		t.Fatalf("Artide has %d members, want 3 (not Olga)", len(team.Members))
	}
	want(400, dana, "POST", "/api/team/role", fmt.Sprintf(`{"id":%d,"role":"viewer"}`, ids["Dana"])) // the last owner
	ok(call(dana, "POST", "/api/team/role", fmt.Sprintf(`{"id":%d,"role":"editor"}`, ids["Carlo"]), true))
	ok(call(carlo, "POST", "/api/decide", `{"content":"`+other4.Content+`","kind":"vote","value":"keep"}`, true))
	ok(call(dana, "POST", "/api/team/rename", `{"name":"Artide Studio"}`, true))
	var me struct {
		Team  string
		Teams []struct{ ID int64 }
	}
	jsonOf(ok(call(bea, "GET", "/api/me", "", false)), &me)
	if me.Team != "Artide Studio" {
		t.Fatalf("after renaming Bea sees team %q", me.Team)
	}
	// A failed database copy (deploy/backup.sh): owners see it, the others do not.
	must(t, os.WriteFile(filepath.Join(s.dir, "backup.json"), []byte(`{"at":"2026-11-06T02:31:00Z","ok":false,"reason":"failed while uploading the dump"}`), 0o644))
	if m := ok(call(bea, "GET", "/api/me", "", false)); strings.Contains(m, `"backup"`) {
		t.Fatalf("Bea, not an owner, sees the backup alarm: %s", m)
	}
	if m := ok(call(dana, "GET", "/api/me", "", false)); !strings.Contains(m, `"backup":"The last database copy failed: failed while uploading the dump"`) {
		t.Fatalf("Dana, an owner, does not see the backup alarm: %s", m)
	}
	must(t, os.Remove(filepath.Join(s.dir, "backup.json")))
	jsonOf(ok(call(dana, "GET", "/api/me", "", false)), &me)
	if len(me.Teams) != 2 {
		t.Fatalf("Dana is in %d teams, want 2", len(me.Teams))
	}
	ok(call(dana, "POST", "/api/me/team", fmt.Sprintf(`{"team":%d}`, other), true))
	if n := count(dana, "/api/clips"); n != 0 {
		t.Fatalf("Dana in Other sees %d clips", n)
	}
	want(403, dana, "POST", "/api/decide", decide) // a viewer in Other
	ok(call(dana, "POST", "/api/me/team", fmt.Sprintf(`{"team":%d}`, artide), true))
	want(403, bea, "POST", "/api/me/team", fmt.Sprintf(`{"team":%d}`, other)) // not her team

	// A password link for Bea: her old session ends, the new password works.
	tok = link(ok(call(dana, "POST", "/api/team/reset", fmt.Sprintf(`{"id":%d}`, ids["Bea"]), true)), "reset")
	want(404, olga, "POST", "/api/team/reset", fmt.Sprintf(`{"id":%d}`, ids["Bea"])) // Bea does not exist for Other
	// Dana is in Other too: its owner must not be able to set his password, or she would get his Artide.
	want(403, olga, "POST", "/api/team/reset", fmt.Sprintf(`{"id":%d}`, ids["Dana"]))
	ok(call(browser(), "POST", "/api/auth/reset", `{"token":"`+tok+`","password":"newpassword"}`, true))
	if code, _ := call(bea, "GET", "/api/clips", "", false); code != 401 {
		t.Fatalf("Bea's old session after the reset: %d, want 401", code)
	}
	ok(call(bea, "POST", "/api/auth/login", `{"email":"b@studio.example","password":"newpassword"}`, true))
	// A cancelled invite cannot be used; a renamed person decides with the new name.
	tok = link(ok(call(dana, "POST", "/api/team/invite", `{"email":"e@studio.example","role":"viewer"}`, true)), "invite")
	ok(call(dana, "POST", "/api/team/revoke", `{"email":"e@studio.example"}`, true))
	want(403, nobody, "POST", "/api/auth/join", `{"token":"`+tok+`","name":"E","password":"eeeeeeeeee"}`)
	ok(call(bea, "POST", "/api/me/name", `{"name":"Beatrice"}`, true))
	var line DecisionLine
	jsonOf(ok(call(bea, "POST", "/api/decide", decide, true)), &line)
	if line.By != "Beatrice" {
		t.Fatalf("decision by %q, want Beatrice", line.By)
	}
	// The owner disconnects Dana's Mac: its token stops working.
	for _, m := range team.Members {
		if m.Name == "Dana" {
			ok(call(dana, "POST", "/api/team/mac/remove", fmt.Sprintf(`{"id":%d}`, m.Macs[0].ID), true))
		}
	}
	if err := app.team.call("GET", "/api/sync/decisions?after=0", nil, nil); !errors.Is(err, errSignedOut) {
		t.Fatalf("the disconnected Mac: %v, want signed out", err)
	}
	if app.pull(); !app.signedOut { // the page then asks to sign in again
		t.Fatal("the app does not know it is signed out")
	}
	// Removing a member and a drive.
	ok(call(dana, "POST", "/api/team/remove", fmt.Sprintf(`{"id":%d}`, ids["Carlo"]), true))
	if code, _ := call(carlo, "GET", "/api/clips", "", false); code != 401 {
		t.Fatalf("Carlo after removal: %d, want 401", code)
	}
	// Carlo was in Artide only: his account goes with it, and his address is free for whoever is invited next,
	// so an address taken through a leaked invite is freed by removing whoever opened it.
	tok = link(ok(call(dana, "POST", "/api/team/invite", `{"email":"c@studio.example","role":"viewer"}`, true)), "invite")
	ok(call(browser(), "POST", "/api/auth/join", `{"token":"`+tok+`","name":"Carla","password":"carlacarla"}`, true))
	want(403, nobody, "POST", "/api/auth/login", `{"email":"c@studio.example","password":"carlocarlo"}`)
	ok(call(dana, "POST", "/api/team/drive/remove", `{"uuid":"A"}`, true))
	if n := count(bea, "/api/clips"); n != 0 {
		t.Fatalf("after removing the drive Bea sees %d clips", n)
	}
	// Olga deletes her team: Dana stays in Artide only, Artide keeps its decisions.
	want(400, olga, "POST", "/api/team/delete", `{"name":"other"}`)
	ok(call(olga, "POST", "/api/team/delete", `{"name":"Other"}`, true))
	if code, _ := call(olga, "GET", "/api/me", "", false); code != 401 {
		t.Fatalf("Olga after deleting her team: %d, want 401", code)
	}
	jsonOf(ok(call(dana, "GET", "/api/me", "", false)), &me)
	if len(me.Teams) != 1 {
		t.Fatalf("Dana is in %d teams after Other was deleted", len(me.Teams))
	}
	var left int
	must(t, s.admin.QueryRow(ctx, "select count(*) from person p where not exists (select 1 from member where person = p.id)").Scan(&left))
	if left != 0 {
		t.Fatalf("%d accounts in no team after Other was deleted, Olga's among them", left)
	}
	if n := count(dana, "/api/sync/decisions?after=0"); n != 7 { // 4 votes and copies, 3 about the transcript
		t.Fatalf("Artide has %d decisions, want 7", n)
	}

	// Wrong passwords: refused, and after 10 attempts the email waits. Counted from zero: the earlier wrong
	// password at Dana's join came from the same address.
	s.fails = map[string][]time.Time{}
	for i := 0; i < 10; i++ {
		want(403, nobody, "POST", "/api/auth/login", `{"email":"d@studio.example","password":"wrongwrong"}`)
	}
	want(429, nobody, "POST", "/api/auth/login", `{"email":"d@studio.example","password":"0123456789"}`)
}

// toHalf encodes a float32 in IEEE 754 binary16 (normal numbers, as the test vectors are).
func toHalf(f float32) uint16 {
	b := math.Float32bits(f)
	sign := uint16(b>>16) & 0x8000
	if f == 0 {
		return sign
	}
	exp := int((b>>23)&0xff) - 127 + 15
	return sign | uint16(exp)<<10 | uint16((b>>13)&0x3ff)
}

func countJSON(t *testing.T, body string) int {
	var rows []json.RawMessage
	must(t, json.Unmarshal([]byte(body), &rows))
	return len(rows)
}

// TestClientIP: behind Caddy every request comes from Caddy, so the sign-in limit counts the address Caddy saw;
// a client on the internet, or another machine of the private network, cannot choose its address with the header.
func TestClientIP(t *testing.T) {
	for _, c := range []struct{ proxy, remote, xff, want string }{
		{"172.18.0.5", "172.18.0.5:4000", "203.0.113.7", "203.0.113.7"},
		{"172.18.0.5", "172.18.0.9:4000", "203.0.113.7", "172.18.0.9"}, // another container, not the proxy
		{"", "172.18.0.5:4000", "203.0.113.7", "172.18.0.5"},           // no proxy set: only this machine is believed
		{"", "127.0.0.1:4000", "1.2.3.4, 203.0.113.7", "203.0.113.7"},
		{"", "127.0.0.1:4000", "", "127.0.0.1"},
		{"localhost", "127.0.0.1:4000", "203.0.113.7", "203.0.113.7"}, // a host name, looked up
		{"", "198.51.100.2:4000", "1.2.3.4", "198.51.100.2"},
	} {
		r := httptest.NewRequest("POST", "/", nil)
		r.RemoteAddr = c.remote
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		if got := (&Server{proxy: c.proxy}).clientIP(r); got != c.want {
			t.Errorf("proxy %q, %s with %q: %s, want %s", c.proxy, c.remote, c.xff, got, c.want)
		}
	}
}

func TestBackupAlarm(t *testing.T) {
	s := &Server{dir: t.TempDir()}
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(s.dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	check := func(want string) {
		t.Helper()
		if got := s.backupAlarm(now); got != want {
			t.Fatalf("alarm %q, want %q", got, want)
		}
	}
	check("") // no records: backups not set up on this site
	write("backup.json", `{"at":"2026-11-06T02:31:00Z","ok":true,"bytes":123,"tables":9,"reason":""}`)
	check("")
	write("backup.json", `{"at":"2026-11-04T02:31:00Z","ok":true,"bytes":123,"tables":9,"reason":""}`)
	check("No database copy since 4 Nov 02:31 UTC")
	write("backup.json", `{"at":"2026-11-06T02:31:00Z","ok":false,"bytes":0,"tables":0,"reason":"failed while uploading the dump"}`)
	write("restore-test.json", `{"at":"2026-11-06T03:00:00Z","ok":false,"tables":9,"reason":"drive: 3 in the copy, 4 live"}`)
	check("The last database copy failed: failed while uploading the dump. The last restore test of a database copy failed: drive: 3 in the copy, 4 live")
	write("backup.json", `{"at":`)
	write("restore-test.json", `{"at":"2026-11-06T03:00:00Z","ok":true}`)
	check("The last database copy failed: its record cannot be read (unexpected end of JSON input)")
}
