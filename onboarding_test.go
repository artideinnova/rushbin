package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/mail"
	"net/textproto"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// TestOnboarding: a self-hosted site from its first run (the setup link), pairing codes for the Mac app, the
// emails through a fake SES, sign-up with email confirmation, and what a site without RUSHBIN_LEGAL never shows.
func TestOnboarding(t *testing.T) {
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
	t.Setenv("RUSHBIN_SMTP_HOST", "smtp.example.com") // email with a sender that is not an address: the site does not start
	t.Setenv("RUSHBIN_MAIL_FROM", "rushbin")
	if _, err := openServer(ctx, u.String(), t.TempDir()); err == nil || !strings.Contains(err.Error(), "RUSHBIN_MAIL_FROM") {
		t.Fatalf("a sender that is not an address: %v, want an error", err)
	}
	t.Setenv("RUSHBIN_SMTP_HOST", "")
	s, err := openServer(ctx, u.String(), t.TempDir())
	must(t, err)
	defer s.db.Close()
	defer s.admin.Close()
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	// A fake SMTP server on this machine: it takes each email as the site sends it and opens it, text and HTML.
	type sent struct{ To, Subject, Text, HTML string }
	mails := make(chan sent, 20)
	smtpd, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	defer smtpd.Close()
	go func() {
		for {
			conn, err := smtpd.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				tp := textproto.NewConn(conn)
				tp.PrintfLine("220 fake")
				var to string
				for {
					line, err := tp.ReadLine()
					if err != nil {
						return
					}
					switch cmd := strings.ToUpper(strings.SplitN(line, " ", 2)[0]); cmd {
					case "EHLO", "HELO":
						tp.PrintfLine("250 fake")
					case "RCPT":
						to = strings.Trim(strings.TrimPrefix(line, "RCPT TO:"), "<>")
						tp.PrintfLine("250 ok")
					case "DATA":
						tp.PrintfLine("354 go")
						body, _ := tp.ReadDotBytes()
						msg, err := mail.ReadMessage(bytes.NewReader(body))
						if err != nil {
							t.Errorf("email: %v", err)
							return
						}
						subject, _ := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
						_, params, _ := mime.ParseMediaType(msg.Header.Get("Content-Type"))
						m := sent{To: to, Subject: subject}
						mr := multipart.NewReader(msg.Body, params["boundary"])
						for {
							p, err := mr.NextPart()
							if err != nil {
								break
							}
							b, _ := io.ReadAll(p) // the reader undoes quoted-printable
							if strings.HasPrefix(p.Header.Get("Content-Type"), "text/html") {
								m.HTML = string(b)
							} else {
								m.Text = string(b)
							}
						}
						mails <- m
						tp.PrintfLine("250 queued")
					case "QUIT":
						tp.PrintfLine("221 bye")
						return
					default:
						tp.PrintfLine("250 ok")
					}
				}
			}()
		}
	}()
	_, port, _ := net.SplitHostPort(smtpd.Addr().String())
	mailer := &Mailer{Host: "127.0.0.1", Port: port, From: "rushbin <no-reply@example.com>"}
	mail := func() sent {
		t.Helper()
		select {
		case m := <-mails:
			return m
		case <-time.After(5 * time.Second):
			t.Fatal("no email")
			return sent{}
		}
	}
	linkIn := func(m sent, kind string) string {
		t.Helper()
		tok := regexp.MustCompile(`#` + kind + `=([0-9a-f]{64})`).FindStringSubmatch(m.Text)
		if tok == nil || strings.Contains(m.HTML, "<img") || !strings.Contains(m.HTML, "#ff681d") {
			t.Fatalf("email to %s: %q / %q", m.To, m.Text, m.HTML)
		}
		return tok[1]
	}

	browser := func() *http.Client { jar, _ := cookiejar.New(nil); return &http.Client{Jar: jar} }
	call := func(c *http.Client, path, body string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest("POST", srv.URL+path, strings.NewReader(body))
		req.Header.Set("X-Rushbin", "1")
		req.Header.Set("Origin", srv.URL)
		r, err := c.Do(req)
		must(t, err)
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		return r.StatusCode, string(b)
	}
	want := func(code int, c *http.Client, path, body string) string {
		t.Helper()
		got, msg := call(c, path, body)
		if got != code {
			t.Fatalf("%s %s: %d %s, want %d", path, body, got, msg, code)
		}
		return msg
	}
	get := func(c *http.Client, path string, v any) {
		t.Helper()
		r, err := c.Get(srv.URL + path)
		must(t, err)
		defer r.Body.Close()
		if r.StatusCode != 200 {
			t.Fatalf("GET %s: %d", path, r.StatusCode)
		}
		must(t, json.NewDecoder(r.Body).Decode(v))
	}
	var site struct{ Setup, Signup, Mail bool }

	// First run: no team. The log says so without the link; the server's admin makes the link on the command line,
	// and a newer link replaces the older. It works once, and only while the site has no team.
	var logged bytes.Buffer
	log.SetOutput(&logged)
	s.firstRun(ctx)
	log.SetOutput(os.Stderr)
	if !strings.Contains(logged.String(), "/rushbin setup-link") || regexp.MustCompile(`[0-9a-f]{64}`).MatchString(logged.String()) {
		t.Fatalf("first-run log %q", logged.String())
	}
	if get(http.DefaultClient, "/api/site", &site); !site.Setup || site.Signup || site.Mail {
		t.Fatalf("site on first run %+v", site)
	}
	ann, bob := browser(), browser()
	setup := `{"team_name":"Studio","name":"Ann","email":"ann@example.com","password":"annannannann","token":"%s"}`
	older, err := setupLink(ctx, s)
	must(t, err)
	link, err := setupLink(ctx, s)
	must(t, err)
	want(403, ann, "/api/auth/setup", strings.Replace(setup, "%s", token(), 1))
	want(403, ann, "/api/auth/setup", strings.Replace(setup, "%s", older, 1))
	want(400, ann, "/api/auth/setup", strings.Replace(strings.Replace(setup, "%s", link, 1), "Studio", " ", 1))
	want(200, ann, "/api/auth/setup", strings.Replace(setup, "%s", link, 1))
	want(403, bob, "/api/auth/setup", strings.Replace(setup, "%s", link, 1))
	if _, err := setupLink(ctx, s); err != errHasTeam {
		t.Fatalf("a setup link on a site with a team: %v", err)
	}
	if get(http.DefaultClient, "/api/site", &site); site.Setup {
		t.Fatal("the setup link still open after the first team")
	}
	var me struct {
		Team, Role string
		ID         int64 `json:"id"`
	}
	if get(ann, "/api/me", &me); me.Team != "Studio" || me.Role != "owner" {
		t.Fatalf("after setup %+v", me)
	}

	// Without email, invites are links for the owner to send.
	var inv struct {
		URL    string
		Mailed bool
	}
	must(t, json.Unmarshal([]byte(want(200, ann, "/api/team/invite", `{"email":"bob@example.com","role":"editor"}`)), &inv))
	if inv.Mailed || !strings.Contains(inv.URL, "#invite=") {
		t.Fatalf("invite without email %+v", inv)
	}
	want(200, bob, "/api/auth/join", `{"token":"`+inv.URL[strings.Index(inv.URL, "=")+1:]+`","name":"Bob","password":"bobbobbobbob"}`)

	// Pairing: the link opens the app with the site and a code; the app trades it once for a computer's token.
	pairLink := func(c *http.Client) (site, code string) {
		t.Helper()
		var p struct{ URL string }
		must(t, json.Unmarshal([]byte(want(200, c, "/api/team/pair", "")), &p))
		l, err := url.Parse(p.URL)
		if err != nil || l.Scheme != "rushbin" || l.Host != "connect" {
			t.Fatalf("pairing link %q", p.URL)
		}
		return l.Query().Get("site"), l.Query().Get("code")
	}
	trade := func(code string) (int, string) { // as the app does: no cookie, no X-Rushbin
		r, err := http.Post(srv.URL+"/api/sync/pair", "application/json", strings.NewReader(`{"token":"`+code+`","device":"Bob's Mac"}`))
		must(t, err)
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		return r.StatusCode, string(b)
	}
	where, code := pairLink(bob)
	if where != srv.URL {
		t.Fatalf("pairing link for %q, want %q", where, srv.URL)
	}
	codeNo, body := trade(code)
	var got struct{ Token, Name, Email, Team string }
	json.Unmarshal([]byte(body), &got)
	if codeNo != 200 || got.Name != "Bob" || got.Email != "bob@example.com" || got.Team != "Studio" || got.Token == "" {
		t.Fatalf("trading a code: %d %s", codeNo, body)
	}
	mac, _ := http.NewRequest("GET", srv.URL+"/api/sync/have", nil)
	mac.Header.Set("Authorization", "Bearer "+got.Token)
	if r, err := http.DefaultClient.Do(mac); err != nil || r.StatusCode != 200 {
		t.Fatalf("the paired Mac's token: %v %v", r, err)
	}
	if c, _ := trade(code); c != 403 {
		t.Fatalf("a code used twice: %d, want 403", c)
	}
	_, code = pairLink(bob)
	_, newer := pairLink(bob)
	if c, _ := trade(code); c != 403 {
		t.Fatalf("an older code after a newer one: %d, want 403", c)
	}
	_, err = s.admin.Exec(ctx, "update pairing set created = now() - $1::interval", seconds(pairTTL+time.Second))
	must(t, err)
	if c, _ := trade(newer); c != 403 {
		t.Fatalf("an expired code: %d, want 403", c)
	}
	s.sweep(ctx, time.Now())
	var left int
	s.admin.QueryRow(ctx, "select count(*) from pairing").Scan(&left)
	if left != 0 {
		t.Fatalf("%d expired codes kept", left)
	}

	// A code is for its team: Bob, also in Other, pairs from Other and gets Other; out of Studio, Studio's code fails.
	other, err := newTeam(ctx, s, "Other", "carl@example.com")
	must(t, err)
	carl := browser()
	want(200, carl, "/api/auth/join", `{"token":"`+other+`","name":"Carl","password":"carlcarlcarl"}`)
	must(t, json.Unmarshal([]byte(want(200, carl, "/api/team/invite", `{"email":"bob@example.com","role":"editor"}`)), &inv))
	want(200, bob, "/api/auth/join", `{"token":"`+inv.URL[strings.Index(inv.URL, "=")+1:]+`","password":"bobbobbobbob"}`) // now in Other
	_, code = pairLink(bob)
	if _, body = trade(code); !strings.Contains(body, `"team":"Other"`) {
		t.Fatalf("a code made in Other: %s", body)
	}
	bobB := browser()
	want(200, bobB, "/api/auth/login", `{"email":"bob@example.com","password":"bobbobbobbob"}`) // opens Studio
	_, code = pairLink(bobB)
	get(ann, "/api/me", &me)
	var team struct{ Members []struct{ ID int64 } }
	get(ann, "/api/team", &team)
	for _, m := range team.Members {
		if m.ID != me.ID {
			want(200, ann, "/api/team/remove", `{"id":`+itoa(m.ID)+`}`)
		}
	}
	if c, msg := trade(code); c != 403 || !strings.Contains(msg, "not in this team") {
		t.Fatalf("a code of a team one left: %d %s", c, msg)
	}

	// Viewers cannot pair a computer, as they cannot sign one in.
	must(t, json.Unmarshal([]byte(want(200, ann, "/api/team/invite", `{"email":"vic@example.com","role":"viewer"}`)), &inv))
	vic := browser()
	want(200, vic, "/api/auth/join", `{"token":"`+inv.URL[strings.Index(inv.URL, "=")+1:]+`","name":"Vic","password":"vicvicvicvic"}`)
	want(403, vic, "/api/team/pair", "")

	// With email: invites and password links are emailed, and anyone can ask for a password link.
	s.mailer = mailer
	must(t, json.Unmarshal([]byte(want(200, ann, "/api/team/invite", `{"email":"dee@example.com","role":"editor"}`)), &inv))
	m := mail()
	if !inv.Mailed || m.To != "dee@example.com" || !strings.Contains(m.Subject, "Ann invited you to Studio") || !strings.Contains(m.Text, inv.URL) {
		t.Fatalf("emailed invite %+v: %+v", inv, m)
	}
	dee := browser()
	want(200, dee, "/api/auth/join", `{"token":"`+linkIn(m, "invite")+`","name":"Dee","password":"deedeedeedee"}`)
	want(200, http.DefaultClient, "/api/auth/forgot", `{"email":"nobody@example.com"}`) // the same answer, and no email
	want(200, http.DefaultClient, "/api/auth/forgot", `{"email":"carl@example.com"}`)
	m = mail()
	if m.To != "carl@example.com" {
		t.Fatalf("password link to %s, want carl@example.com", m.To)
	}
	// A link sent by email works for an hour; one an owner hands over, 7 days.
	_, err = s.admin.Exec(ctx, "update reset set created = now() - interval '61 minutes'")
	must(t, err)
	want(403, browser(), "/api/auth/reset", `{"token":"`+linkIn(m, "reset")+`","password":"carl-new-password"}`)
	want(200, http.DefaultClient, "/api/auth/forgot", `{"email":"carl@example.com"}`)
	m = mail()
	_, err = s.admin.Exec(ctx, "update reset set created = now() - interval '59 minutes'")
	must(t, err)
	want(200, browser(), "/api/auth/reset", `{"token":"`+linkIn(m, "reset")+`","password":"carl-new-password"}`)
	var owners struct{ URL string }
	carl = browser() // the new password ended his sessions
	want(200, carl, "/api/auth/login", `{"email":"carl@example.com","password":"carl-new-password"}`)
	must(t, json.Unmarshal([]byte(want(200, carl, "/api/team/reset", `{"id":`+itoa(carlID(t, s))+`}`)), &owners))
	mail()
	_, err = s.admin.Exec(ctx, "update reset set created = now() - interval '6 days'")
	must(t, err)
	want(200, browser(), "/api/auth/reset", `{"token":"`+owners.URL[strings.Index(owners.URL, "=")+1:]+`","password":"carl-newer-password"}`)
	want(200, browser(), "/api/auth/login", `{"email":"carl@example.com","password":"carl-newer-password"}`)
	for range 4 { // at most 5 emails per address in 15 minutes
		must(t, s.mail(ctx, "x@example.com", Letter{Subject: "s", Text: "t", Button: "b", Link: "https://example.com", After: "a"}))
		mail()
	}
	if err := s.mail(ctx, "X@example.com", Letter{}); err != nil {
		t.Fatal(err)
	}
	mail()
	if err := s.mail(ctx, "x@example.com", Letter{}); err == nil || !strings.Contains(err.Error(), "Too many emails") {
		t.Fatalf("a sixth email: %v", err)
	}
}

func carlID(t *testing.T, s *Server) (id int64) {
	must(t, s.admin.QueryRow(context.Background(), "select id from person where email = 'carl@example.com'").Scan(&id))
	return id
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }
