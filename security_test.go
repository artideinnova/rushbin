package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// TestSecurity: two teams on one site, and what one member may and may not do to the other team, to their own
// team and to the site. Each part is a finding of the security review of November 2026 that stays fixed.
// Needs Postgres, as TestServer.
func TestSecurity(t *testing.T) {
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

	// A store that keeps what is put in it, with its content type, as S3 and R2 do.
	var smu sync.Mutex
	stored, types := map[string][]byte{}, map[string]string{}
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		smu.Lock()
		defer smu.Unlock()
		switch r.Method {
		case "PUT":
			stored[r.URL.Path], _ = io.ReadAll(r.Body)
			types[r.URL.Path] = r.Header.Get("Content-Type")
		case "DELETE":
			delete(stored, r.URL.Path)
		case "HEAD", "GET":
			b, ok := stored[r.URL.Path]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", types[r.URL.Path])
			w.Header().Set("Content-Length", fmt.Sprint(len(b)))
			w.Write(b)
		}
	}))
	defer fake.Close()
	s.store = &Store{Endpoint: fake.URL, Bucket: "b", KeyID: "k", Secret: "s", Region: "auto"}
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	type hdr = map[string]string
	do := func(c *http.Client, method, path string, body io.Reader, h hdr) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, body)
		for k, v := range h {
			req.Header.Set(k, v)
		}
		r, err := c.Do(req)
		must(t, err)
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		return r.StatusCode, strings.TrimSpace(string(b))
	}
	from := func(ip string) hdr { // a browser on this site, from an address (the test's peer is this machine)
		return hdr{"X-Rushbin": "1", "Origin": srv.URL, "Content-Type": "application/json", "X-Forwarded-For": ip}
	}
	site := from("192.0.2.1")
	call := func(c *http.Client, method, path, body string) (int, string) {
		t.Helper()
		return do(c, method, path, strings.NewReader(body), site)
	}
	app := func(tok, method, path, body string) (int, string) {
		t.Helper()
		return do(http.DefaultClient, method, path, strings.NewReader(body), hdr{"Authorization": "Bearer " + tok})
	}
	want := func(code int) func(int, string) {
		return func(got int, body string) {
			t.Helper()
			if got != code {
				t.Fatalf("%d %s, want %d", got, body, code)
			}
		}
	}
	browser := func() *http.Client { jar, _ := cookiejar.New(nil); return &http.Client{Jar: jar} }
	link := func(body string) string { return body[strings.Index(body, "=")+1 : strings.LastIndex(body, `"`)] }
	join := func(c *http.Client, tok, name, pw string) {
		t.Helper()
		want(200)(call(c, "POST", "/api/auth/join", `{"token":"`+tok+`","name":"`+name+`","password":"`+pw+`"}`))
	}
	invite := func(owner *http.Client, email, role string) string {
		t.Helper()
		code, b := call(owner, "POST", "/api/team/invite", `{"email":"`+email+`","role":"`+role+`"}`)
		want(200)(code, b)
		return link(b)
	}
	macLogin := func(email, pw string) string {
		t.Helper()
		code, b := do(http.DefaultClient, "POST", "/api/sync/login", strings.NewReader(`{"email":"`+email+`","password":"`+pw+`","device":"Mac"}`), nil)
		want(200)(code, b)
		var r struct{ Token string }
		json.Unmarshal([]byte(b), &r)
		return r.Token
	}
	idOf := func(email string) (id int64) {
		s.admin.QueryRow(ctx, "select id from person where email = $1", email).Scan(&id)
		return
	}
	rec := func(path, hash string) string {
		return `{"Path":"/Volumes/X/` + path + `","Ext":".mp4","Hash":"` + hash + `","Size":1000,"Mtime":"2026-01-01T00:00:00Z","Dur":5,"W":1920,"H":1080,"Codec":"h264","FPS":"25"}` + "\n"
	}
	putDrive := func(tok, uuid string, parts map[string]string) (int, string) {
		t.Helper()
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		if _, ok := parts["device.json"]; !ok {
			parts["device.json"] = `{"uuid":"` + uuid + `","name":"` + uuid + `","mount":"/Volumes/X"}`
		}
		for k, v := range parts {
			p, _ := mw.CreateFormFile(k, k)
			p.Write([]byte(v))
		}
		mw.Close()
		return do(http.DefaultClient, "PUT", "/api/sync/drive/"+uuid, &buf, hdr{"Authorization": "Bearer " + tok, "Content-Type": mw.FormDataContentType()})
	}

	// Team A: owner ao, editor ae, viewer av. Team B: owner bo, editor be, an invite waiting.
	ao, ae, av, bo, be := browser(), browser(), browser(), browser(), browser()
	tok, _ := newTeam(ctx, s, "Alpha", "ao@a.test")
	join(ao, tok, "Ao", "aoaoaoaoao")
	join(ae, invite(ao, "ae@a.test", "editor"), "Ae", "aeaeaeaeae")
	join(av, invite(ao, "av@a.test", "viewer"), "Av", "avavavavav")
	tok, _ = newTeam(ctx, s, "Beta", "bo@b.test")
	join(bo, tok, "Bo", "bobobobobo")
	join(be, invite(bo, "be@b.test", "editor"), "Be", "bebebebebe")
	invite(bo, "pending@b.test", "viewer")
	aMac, bMac := macLogin("ae@a.test", "aeaeaeaeae"), macLogin("be@b.test", "bebebebebe")
	var teamA, teamB int64
	s.admin.QueryRow(ctx, "select id from team where name = 'Alpha'").Scan(&teamA)
	s.admin.QueryRow(ctx, "select id from team where name = 'Beta'").Scan(&teamB)
	want(200)(putDrive(aMac, "ADRIVE", map[string]string{"files.jsonl": rec("PA/a.mp4", "ahash")}))
	want(200)(putDrive(bMac, "BDRIVE", map[string]string{"files.jsonl": rec("PB/b.mp4", "bhash")}))
	bth := "c_bhash"
	want(200)(do(http.DefaultClient, "PUT", "/api/sync/thumbs/"+bth+"/3", strings.NewReader("\xff\xd8\xffB"), hdr{"Authorization": "Bearer " + bMac}))
	want(200)(app(bMac, "POST", "/api/sync/speech", `{"thumb":"`+bth+`","segs":[{"s":1,"e":2,"t":"bwords"}]}`))
	code, b := call(be, "POST", "/api/original", `{"device":"BDRIVE","id":"PB/b.mp4"}`)
	want(200)(code, b)
	var breq struct{ ID int64 }
	json.Unmarshal([]byte(b), &breq)
	var bMacID int64
	s.admin.QueryRow(ctx, "select id from session where team = $1 and device <> ''", teamB).Scan(&bMacID)

	t.Run("another team's ids", func(t *testing.T) {
		for _, c := range []struct {
			who             *http.Client
			method, path, b string
			code            int
		}{
			{ao, "GET", "/api/clip?device=BDRIVE&id=PB/b.mp4", "", 404},
			{ao, "GET", "/thumbs/" + bth + "/3.jpg", "", 404},
			{ae, "POST", "/api/decide", `{"content":"c:bhash","kind":"vote","value":"discard"}`, 400},
			{ao, "POST", "/api/forget/" + bth, "", 404},
			{ao, "POST", "/api/team/drive/remove", `{"uuid":"BDRIVE"}`, 404},
			{ao, "POST", "/api/team/mac/remove", fmt.Sprintf(`{"id":%d}`, bMacID), 404},
			{ao, "POST", "/api/team/role", fmt.Sprintf(`{"id":%d,"role":"viewer"}`, idOf("bo@b.test")), 404},
			{ao, "POST", "/api/team/remove", fmt.Sprintf(`{"id":%d}`, idOf("bo@b.test")), 404},
			{ao, "POST", "/api/team/reset", fmt.Sprintf(`{"id":%d}`, idOf("bo@b.test")), 404},
			{ao, "GET", fmt.Sprintf("/api/original/%d", breq.ID), "", 404},
			{ao, "GET", "/api/proxy/" + bth, "", 404},
			{ao, "POST", "/api/me/team", fmt.Sprintf(`{"team":%d}`, teamB), 403},
			{ao, "GET", "/api/compare?a=BDRIVE&b=ADRIVE", "", 400},
		} {
			if code, b := call(c.who, c.method, c.path, c.b); code != c.code {
				t.Errorf("%s %s: %d %s, want %d", c.method, c.path, code, b, c.code)
			}
		}
		want(409)(app(aMac, "POST", fmt.Sprintf("/api/sync/requests/%d/claim", breq.ID), ""))
		want(404)(app(aMac, "POST", fmt.Sprintf("/api/sync/requests/%d/sent", breq.ID), `{"error":"x"}`))
		call(ao, "POST", "/api/team/revoke", `{"email":"pending@b.test"}`)
		for q, b := range map[string]string{"/api/sync/requests?drives=BDRIVE": "[]", "/api/sync/decisions?after=0": "[]"} {
			if _, got := app(aMac, "GET", q, ""); got != b {
				t.Errorf("%s: %s, want %s", q, got, b)
			}
		}
		if _, got := call(ao, "GET", "/api/search?q=bwords", ""); got != "[]" {
			t.Errorf("search finds team B's words: %s", got)
		}
		var n int
		s.admin.QueryRow(ctx, `select (select count(*) from invite where team = $1) + (select count(*) from request where team = $1 and state = 'waiting')
			+ (select count(*) from drive where team = $1)`, teamB).Scan(&n)
		if n != 3 {
			t.Errorf("team B's invite, request and drive: %d of 3 left", n)
		}
	})

	// The team pool signs in as rushbin_app, which cannot become another role, and sees only its team's people.
	t.Run("the team pool's role", func(t *testing.T) {
		err := s.inTeam(ctx, teamA, func(tx pgx.Tx) error {
			var who string
			var people int
			tx.QueryRow(ctx, "select current_user, (select count(*) from person)").Scan(&who, &people)
			if who != "rushbin_app" || people != 3 {
				t.Errorf("as %s, %d people visible, want rushbin_app and team A's 3", who, people)
			}
			_, err := tx.Exec(ctx, "set local role postgres")
			return err
		})
		if err == nil {
			t.Fatal("the team pool became postgres")
		}
	})

	// Failed sign-ins count per email and address, and per address: others failing elsewhere lock nobody out.
	t.Run("sign-in limits", func(t *testing.T) {
		login := func(email, pw, ip string) int {
			code, _ := do(browser(), "POST", "/api/auth/login", strings.NewReader(`{"email":"`+email+`","password":"`+pw+`"}`), from(ip))
			return code
		}
		fails := func(n int, email func(int) string, ip func(int) string) { // at once: each is a bcrypt of cost 12
			var wg sync.WaitGroup
			for i := range n {
				wg.Go(func() { login(email(i), "wrongwrong", ip(i)) })
			}
			wg.Wait()
		}
		av := func(int) string { return "av@a.test" }
		fails(10, av, func(i int) string { return fmt.Sprintf("203.0.113.%d", i) })
		if code := login("av@a.test", "avavavavav", "198.51.100.7"); code != 200 {
			t.Errorf("av after 10 failures from other addresses: %d, want 200", code)
		}
		fails(10, av, func(int) string { return "198.51.100.8" })
		if code := login("av@a.test", "avavavavav", "198.51.100.8"); code != 429 {
			t.Errorf("av after 10 failures from the same address: %d, want 429", code)
		}
		fails(30, func(i int) string { return fmt.Sprintf("x%d@x.test", i) }, func(int) string { return "198.51.100.9" })
		if code := login("ao@a.test", "aoaoaoaoao", "198.51.100.9"); code != 429 {
			t.Errorf("an address after 30 failures on 30 emails: %d, want 429", code)
		}
	})

	// What a Mac sends is checked against what the catalog reads; rows from before, which were not, are left out.
	t.Run("bad team data", func(t *testing.T) {
		want(400)(app(aMac, "POST", "/api/sync/looks", `{"thumb":"c_ahash","frames":"oops"}`))
		want(400)(app(aMac, "POST", "/api/sync/speech", `{"thumb":"c_ahash","segs":"oops"}`))
		want(200)(app(aMac, "POST", "/api/sync/speech", `{"thumb":"c_ahash","segs":[]}`)) // a clip where nothing is said
		want(400)(putDrive(aMac, "ADRIVE", map[string]string{"files.jsonl": rec("PA/a.mp4", "ahash"), "fullhash.tsv": strings.Repeat("x", 2<<20) + "\n"}))
		want(400)(putDrive(aMac, "ADRIVE", map[string]string{"files.jsonl": rec("PA/a.mp4", "ahash"), "other": "x"}))
		want(400)(putDrive(aMac, "ADRIVE", map[string]string{"files.jsonl": "{"}))
		left, _ := os.ReadDir(s.dir)
		for _, e := range left {
			if strings.HasPrefix(e.Name(), ".drive-") {
				t.Errorf("upload left behind: %s", e.Name())
			}
		}
		// Written as an older site let them through.
		for _, q := range []string{`insert into look (team, thumb, frames) values ($1, 'c_ahash', '"oops"')`,
			`insert into speech (team, thumb, segs) values ($1, 'c_x', '1')`,
			`insert into drive (team, uuid, device, files, fullhash) values ($1, 'BAD', '{"uuid":"BAD"}', '', convert_to(repeat('x', 2100000), 'UTF8'))`} {
			_, err := s.admin.Exec(ctx, q, teamA)
			must(t, err)
		}
		s.changed(teamA)
		code, b := call(av, "GET", "/api/overview", "")
		if code != 200 || !strings.Contains(b, "ADRIVE") || strings.Contains(b, `"BAD"`) {
			t.Fatalf("overview of a team with bad rows: %d %.200s", code, b)
		}
		s.admin.Exec(ctx, "delete from look where team = $1", teamA)
		s.admin.Exec(ctx, "delete from drive where team = $1 and uuid = 'BAD'", teamA)
		s.changed(teamA)
	})

	// One team's catalog build makes no other team wait.
	t.Run("catalog builds per team", func(t *testing.T) {
		_, err := s.catalog(ctx, teamA)
		must(t, err)
		s.changed(teamA)
		s.changed(teamB)
		s.mu.Lock()
		lockA := s.building[teamA]
		s.mu.Unlock()
		lockA.Lock() // team A's build, in progress
		done := make(chan error)
		go func() { _, err := s.catalog(ctx, teamB); done <- err }()
		select {
		case err := <-done:
			must(t, err)
		case <-time.After(10 * time.Second):
			t.Error("team B waits for team A's build")
		}
		lockA.Unlock()
	})

	t.Run("sessions", func(t *testing.T) {
		// A computer's token ends after 90 days unused.
		s.admin.Exec(ctx, "update session set seen = now() - interval '89 days' where hash = $1", hash(aMac))
		want(200)(app(aMac, "GET", "/api/sync/have", ""))
		s.admin.Exec(ctx, "update session set seen = now() - interval '91 days' where hash = $1", hash(aMac))
		want(401)(app(aMac, "GET", "/api/sync/have", ""))
		// Sign out everywhere: browsers and computers.
		mac := macLogin("ae@a.test", "aeaeaeaeae")
		want(200)(call(ae, "POST", "/api/me/logout", ""))
		want(401)(app(mac, "GET", "/api/sync/have", ""))
		want(401)(call(ae, "GET", "/api/me", ""))
		// Only the newest password link works.
		_, r1 := call(ao, "POST", "/api/team/reset", fmt.Sprintf(`{"id":%d}`, idOf("av@a.test")))
		_, r2 := call(ao, "POST", "/api/team/reset", fmt.Sprintf(`{"id":%d}`, idOf("av@a.test")))
		want(403)(call(browser(), "POST", "/api/auth/reset", `{"token":"`+link(r1)+`","password":"newnewnew1"}`))
		want(200)(call(browser(), "POST", "/api/auth/reset", `{"token":"`+link(r2)+`","password":"newnewnew2"}`))
	})

	t.Run("invite email", func(t *testing.T) {
		invite(ao, "Bob <Bob@A.test>", "viewer")
		_, b := call(ao, "GET", "/api/team", "")
		if !strings.Contains(b, `"email":"bob@a.test"`) || strings.Contains(b, "Bob <") {
			t.Fatalf("invites: %s", b)
		}
	})

	// Only the light copy of one of the team's clips, uploaded as video/mp4, is recorded; anything else goes.
	t.Run("proxy uploads", func(t *testing.T) {
		upload := func(th, ctype, body string) (int, string) {
			_, b := app(bMac, "POST", "/api/sync/proxy/"+th, "")
			var up struct{ URL string }
			json.Unmarshal([]byte(b), &up)
			req, _ := http.NewRequest("PUT", up.URL, strings.NewReader(body))
			req.Header.Set("Content-Type", ctype)
			r, err := http.DefaultClient.Do(req)
			must(t, err)
			r.Body.Close()
			return app(bMac, "POST", "/api/sync/proxy/"+th+"/done", "")
		}
		inStore := func(th string) bool {
			smu.Lock()
			defer smu.Unlock()
			_, ok := stored["/b/"+proxyKey(teamB, th)]
			return ok
		}
		want(400)(upload(bth, "text/html", "<script>alert(1)</script>"))
		want(400)(upload("c_notinthecatalog", "video/mp4", "mp4"))
		if inStore(bth) || inStore("c_notinthecatalog") {
			t.Error("a refused upload stays in the store")
		}
		want(200)(upload(bth, "video/mp4", "mp4"))
		want(200)(call(be, "GET", "/api/proxy/"+bth, ""))
	})
}

// TestStoreErrors: when the store cannot be reached, what failed is said without the signed link, which the
// error of the HTTP client would carry into requests' notes and the logs.
func TestStoreErrors(t *testing.T) {
	s := &Store{Endpoint: "http://127.0.0.1:1", Bucket: "b", KeyID: "AKIDTEST", Secret: "SECRETTEST", Region: "auto"}
	_, err := s.do(context.Background(), "POST", "teams/1/originals/2/a.mov", nil, [2]string{"uploadId", "u"})
	if err == nil || strings.Contains(err.Error(), "X-Amz-") || strings.Contains(err.Error(), "?") || !strings.Contains(err.Error(), "teams/1/originals/2/a.mov") {
		t.Fatalf("store error: %v", err)
	}
}

// TestAppGuard: the app's engine takes writes of every method only from its own window.
func TestAppGuard(t *testing.T) {
	a := &App{token: "tok"}
	h := a.guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	for _, c := range []struct {
		method, host, cookie, xr, origin string
		code                             int
	}{
		{"GET", "evil.test:5000", "tok", "", "", 403}, // DNS rebinding
		{"GET", "127.0.0.1:5000", "", "", "", 403},
		{"GET", "127.0.0.1:5000", "tok", "", "", 200},
		{"POST", "127.0.0.1:5000", "tok", "", "", 403},
		{"PUT", "127.0.0.1:5000", "tok", "", "", 403},
		{"DELETE", "127.0.0.1:5000", "tok", "1", "http://127.0.0.1:6000", 403},
		{"PUT", "127.0.0.1:5000", "tok", "1", "http://127.0.0.1:5000", 200},
	} {
		req := httptest.NewRequest(c.method, "http://"+c.host+"/api/x", nil)
		if c.cookie != "" {
			req.AddCookie(&http.Cookie{Name: "rb", Value: c.cookie})
		}
		if c.xr != "" {
			req.Header.Set("X-Rushbin", c.xr)
		}
		if c.origin != "" {
			req.Header.Set("Origin", c.origin)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != c.code {
			t.Errorf("%s %s cookie %q header %q origin %q: %d, want %d", c.method, c.host, c.cookie, c.xr, c.origin, w.Code, c.code)
		}
	}
}
