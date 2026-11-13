package main

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/mail"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

// Server is the team site: the members of a team see the catalogs its Macs send and decide together, and
// every decision keeps who took it. One site can host many teams. Each team's rows are kept apart twice,
// and each barrier holds alone (TestServer checks both): every query filters on the team of the request,
// and Postgres itself shows a request only its team's rows (row level security for rushbin_app).
type Server struct {
	admin    *pgxpool.Pool // sign-in, invites and resets: they come before the team is known
	db       *pgxpool.Pool // everything else, as rushbin_app, inside inTeam
	dir      string        // previews, under teams/<team>/thumbs
	store    *Store        // where the proxies are (S3-compatible), nil without one
	releases *Store        // where latest.json and the installers are (releaseStore), nil without one
	upstream string        // without releases of its own: the site whose releases it passes on (updatesFrom), "" for none
	textURL  string        // the text encoder for search by content (deploy/text), "" without one
	ai       AI            // the site's own AI for Tidy up with AI (siteAI), none without one: its key goes only to that provider
	tidyCap  int           // tidy-ups with AI per team per calendar month
	online   online        // which Mac has which drive connected now
	rel      releases      // the store's latest.json, cached
	proxy    string        // the reverse proxy whose X-Forwarded-For is believed (RUSHBIN_PROXY), "" for one on this machine
	drives   chan struct{} // one drive's catalog at a time goes from disk to the database (putDrive)
	mailer   *Mailer       // the site's email (SMTP), nil without one: owners then send the links themselves

	mu       sync.Mutex // the maps below; never held while a catalog is built
	cats     map[int64]*Catalog
	building map[int64]*sync.Mutex // team → held while its catalog is built: one team's build waits for no other team
	vers     map[int64]int64       // team → how many times its data changed: the pages refresh when it moves

	fmu   sync.Mutex
	fails map[string][]time.Time // failed sign-ins per email and address, and per address
}

// Tables carrying a team column get row level security; person is shared by the teams someone is in.
var teamTables = []string{"member", "session", "invite", "reset", "pairing", "drive", "look", "decision", "proxy", "speech", "frame", "request", "forgotten", "tidy_run"}

const schema = `
create extension if not exists vector;
create table if not exists team (id bigserial primary key, name text not null, created timestamptz not null default now());
create table if not exists person (id bigserial primary key, email text not null unique, name text not null,
  pass text not null, created timestamptz not null default now());
create table if not exists member (team bigint not null references team on delete cascade,
  person bigint not null references person on delete cascade,
  role text not null check (role in ('owner', 'editor', 'viewer')), created timestamptz not null default now(),
  primary key (team, person));
create table if not exists session (id bigserial primary key, hash bytea not null unique,
  person bigint not null references person on delete cascade, team bigint not null references team on delete cascade,
  device text not null default '', created timestamptz not null default now(), seen timestamptz not null default now());
create table if not exists invite (hash bytea primary key, team bigint not null references team on delete cascade,
  email text not null, role text not null check (role in ('owner', 'editor', 'viewer')),
  created timestamptz not null default now());
create table if not exists reset (hash bytea primary key, person bigint not null references person on delete cascade,
  team bigint not null references team on delete cascade, created timestamptz not null default now());
alter table reset add column if not exists mailed boolean not null default false;
alter table team add column if not exists terms timestamptz;
create table if not exists pairing (hash bytea primary key, person bigint not null references person on delete cascade,
  team bigint not null references team on delete cascade, created timestamptz not null default now());
create table if not exists setup_link (hash bytea primary key, created timestamptz not null default now());
create table if not exists drive (team bigint not null references team on delete cascade, uuid text not null,
  device jsonb not null, files bytea not null, fullhash bytea not null, updated timestamptz not null default now(),
  primary key (team, uuid));
create table if not exists look (team bigint not null references team on delete cascade, thumb text not null,
  frames jsonb not null, primary key (team, thumb));
create table if not exists decision (id bigserial primary key, team bigint not null references team on delete cascade,
  content text not null, kind text not null, value text not null, by_name text not null,
  person bigint references person on delete set null, t timestamptz not null default now());
create index if not exists decision_team on decision (team, id);
create table if not exists proxy (team bigint not null references team on delete cascade, thumb text not null,
  size bigint not null, created timestamptz not null default now(), primary key (team, thumb));
create table if not exists speech (team bigint not null references team on delete cascade, thumb text not null,
  segs jsonb not null, primary key (team, thumb));
create table if not exists frame (team bigint not null references team on delete cascade, thumb text not null,
  k smallint not null, v halfvec(768) not null, primary key (team, thumb, k));
create index if not exists frame_v on frame using hnsw (v halfvec_cosine_ops);
create table if not exists request (id bigserial primary key, team bigint not null references team on delete cascade,
  device text not null, rel text not null, content text not null, size bigint not null,
  person bigint references person on delete set null, by_name text not null, state text not null default 'waiting',
  sha256 text, verified boolean, note text, created timestamptz not null default now(), updated timestamptz not null default now());
create table if not exists forgotten (team bigint not null references team on delete cascade, thumb text not null,
  person bigint references person on delete set null, by_name text not null, t timestamptz not null default now(),
  primary key (team, thumb));
-- Apple's classes for people by age or body feature (personLabels in vision.go) are never kept: once, what older
-- versions stored loses them (no row matches afterwards).
update look set frames = (select jsonb_agg(case when f ? 'labels' then jsonb_set(f, '{labels}', (f->'labels') - array['adult','baby','child','teen','tattoo']) else f end)
  from jsonb_array_elements(frames) f) where frames::text ~ '"(adult|baby|child|teen|tattoo)"';
create table if not exists tidy_run (team bigint not null references team on delete cascade, t timestamptz not null default now());
create index if not exists tidy_run_team on tidy_run (team, t);

do $$ begin create role rushbin_app nologin; exception when duplicate_object then null; end $$;
grant usage on schema public to rushbin_app;
grant select, update, delete on team to rushbin_app;
grant select, insert, update, delete on member, session, invite, reset, pairing, drive, look, decision, proxy, speech, frame, request, forgotten to rushbin_app;
grant select, insert on tidy_run to rushbin_app;
grant usage on all sequences in schema public to rushbin_app;
grant select (id, email, name) on person to rushbin_app;
alter table team enable row level security;
alter table team force row level security;
drop policy if exists own_team on team;
create policy own_team on team using (id = nullif(current_setting('rushbin.team', true), '')::bigint);
alter table person enable row level security;
alter table person force row level security;
drop policy if exists own_team on person;
create policy own_team on person using (exists (select 1 from member m where m.person = person.id));
`

// After a transaction a custom setting reads ” rather than NULL (Postgres 17), hence nullif: no team, no rows.
func teamPolicies() string {
	var b strings.Builder
	for _, t := range teamTables {
		fmt.Fprintf(&b, `alter table %[1]s enable row level security;
alter table %[1]s force row level security;
drop policy if exists own_team on %[1]s;
create policy own_team on %[1]s using (team = nullif(current_setting('rushbin.team', true), '')::bigint);
`, t)
	}
	return b.String()
}

// User is the person behind a request, in the team of their session (cookie or app token).
type User struct {
	Session, ID, Team int64
	Email, Name       string
	TeamName, Role    string
	Device            string // the Mac's name for an app token, "" in a browser
}

// may: viewers look and search; editors also decide, connect Macs and tidy drives with AI; owners also manage the team.
func (u *User) may(action string) bool {
	switch action {
	case "decide", "send", "tidy":
		return u.Role == "owner" || u.Role == "editor"
	case "manage":
		return u.Role == "owner"
	}
	return false
}

var roles = []string{"owner", "editor", "viewer"}

// How long the site keeps what is no longer used, each counted from its last use; sweep deletes what is past it.
const (
	webSession = 30 * 24 * time.Hour
	// A Mac's or PC's token ends after 90 days unused: a lost or forgotten computer does not keep a way into the
	// team for ever. The app then asks to sign in again.
	deviceSession = 90 * 24 * time.Hour
	linkDays      = 7 // invites and password resets
)

// openServer prepares the database (tables, role, policies) and the two connection pools. The team pool signs in
// as rushbin_app with its own password (RUSHBIN_APP_PASSWORD, set by whoever runs the site): a role that cannot
// become another, so the policies hold for every query it sends, even one that would try to change role.
func openServer(ctx context.Context, dbURL, dir string) (*Server, error) {
	pass := os.Getenv("RUSHBIN_APP_PASSWORD")
	if len(pass) < 16 || strings.ContainsFunc(pass, func(r rune) bool { return r < '!' || r > '~' }) {
		return nil, errors.New("RUSHBIN_APP_PASSWORD: 16 or more letters, digits or symbols (openssl rand -hex 32 makes one)")
	}
	admin, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		return nil, err
	}
	// The password goes to Postgres as a SCRAM verifier, never in clear: no statement or log line holds it.
	if _, err := admin.Exec(ctx, schema+teamPolicies()+"alter role rushbin_app login password '"+scramVerifier(pass)+"';"); err != nil {
		return nil, fmt.Errorf("database: %w", err)
	}
	cfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		return nil, err
	}
	cfg.ConnConfig.User, cfg.ConnConfig.Password = "rushbin_app", pass
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	ai, err := siteAI()
	if err != nil {
		return nil, err
	}
	tidyCap, err := strconv.Atoi(os.Getenv("RUSHBIN_TIDY_CAP"))
	if err != nil || tidyCap < 1 {
		tidyCap = 20
	}
	store := storeFromEnv("RUSHBIN_S3_")
	mailer, err := mailerFromEnv()
	if err != nil {
		return nil, err
	}
	return &Server{admin: admin, db: db, dir: dir, store: store, releases: releaseStore(store), upstream: updatesFrom(), textURL: strings.TrimRight(os.Getenv("RUSHBIN_TEXT_URL"), "/"),
		ai: ai, tidyCap: tidyCap, mailer: mailer,
		proxy: os.Getenv("RUSHBIN_PROXY"), drives: make(chan struct{}, 1),
		cats: map[int64]*Catalog{}, building: map[int64]*sync.Mutex{}, vers: map[int64]int64{}, fails: map[string][]time.Time{}}, nil
}

// scramVerifier is what Postgres keeps of a password (SCRAM-SHA-256, RFC 5802 and 7677), made here so that the
// password itself never reaches the server. The password is printable ASCII, which SASLprep leaves as it is.
func scramVerifier(pass string) string {
	salt := make([]byte, 16)
	rand.Read(salt)
	const iter = 4096 // Postgres's own default
	salted, _ := pbkdf2.Key(sha256.New, pass, salt, iter, 32)
	stored := sha256.Sum256(hmacSHA256(salted, "Client Key"))
	b64 := base64.StdEncoding.EncodeToString
	return fmt.Sprintf("SCRAM-SHA-256$%d:%s$%s:%s", iter, b64(salt), b64(stored[:]), b64(hmacSHA256(salted, "Server Key")))
}

func runServer(ctx context.Context, dbURL, dir, addr string) error {
	s, err := openServer(ctx, dbURL, dir)
	if err != nil {
		return err
	}
	go s.expire()
	s.firstRun(ctx)
	log.Printf("rushbin site on %s", addr)
	return http.ListenAndServe(addr, s.routes())
}

// emailOf is the address alone, in lower case, of what someone typed ("Bob <b@x.com>" is b@x.com), "" if none.
func emailOf(s string) string {
	a, err := mail.ParseAddress(s)
	if err != nil {
		return ""
	}
	return strings.ToLower(a.Address)
}

// newTeam creates a team and returns the invite of its first owner. Run by whoever runs the site
// (rushbin newteam); the first team can also come from the setup link (rushbin setup-link).
func newTeam(ctx context.Context, s *Server, name, email string) (string, error) {
	if email = emailOf(email); email == "" || strings.TrimSpace(name) == "" {
		return "", errors.New("give the team's name and its owner's email")
	}
	tok := token()
	err := pgx.BeginFunc(ctx, s.admin, func(tx pgx.Tx) error {
		var team int64
		if err := tx.QueryRow(ctx, "insert into team (name) values ($1) returning id", strings.TrimSpace(name)).Scan(&team); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "insert into invite (hash, team, email, role) values ($1, $2, $3, 'owner')", hash(tok), team, email)
		return err
	})
	return tok, err
}

// inTeam runs fn in a transaction where Postgres shows only the rows of team.
func (s *Server) inTeam(ctx context.Context, team int64, fn func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "select set_config('rushbin.team', $1, true)", strconv.FormatInt(team, 10)); err != nil {
			return err
		}
		return fn(tx)
	})
}

func seconds(d time.Duration) string { return fmt.Sprintf("%d seconds", int(d.Seconds())) }

func token() string {
	b := make([]byte, 32)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func hash(tok string) []byte { h := sha256.Sum256([]byte(tok)); return h[:] }

// httpError carries the status a handler wants to answer with out of a transaction.
type httpError struct {
	code int
	msg  string
}

func (e httpError) Error() string { return e.msg }

func fail(w http.ResponseWriter, err error) {
	var he httpError
	if errors.As(err, &he) {
		http.Error(w, he.msg, he.code)
		return
	}
	log.Println("site:", err)
	http.Error(w, "something went wrong on the site", 500)
}

func (s *Server) routes() http.Handler {
	m := http.NewServeMux()
	web, _ := fs.Sub(webFS, "web")
	page := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { http.ServeFileFS(w, r, web, name) }
	}
	m.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		if s.user(r) == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		page("index.html")(w, r)
	})
	m.HandleFunc("GET /login", page("login.html"))
	m.HandleFunc("GET /robots.txt", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "User-agent: *\nDisallow: /\n") // a team's site is private: nothing to index
	})
	for _, f := range []string{"logo.svg", "wordmark.svg", "apple-touch-icon.png", "icon-192.png", "icon-512.png", "manifest.json", "i18n.js"} {
		m.HandleFunc("GET /"+f, page(f)) // public: the browser asks for them before signing in
	}
	m.Handle("GET /i18n/", http.FileServerFS(web)) // public: the words of each language (web/i18n.js)
	m.HandleFunc("GET /api/site", func(w http.ResponseWriter, r *http.Request) {
		send(w, map[string]any{"tidy": s.ai.ready(), "ai": s.ai.To(), // tidy: the apps use the site's AI, ai: who receives a description
			"mail": s.mailer != nil, "setup": s.setupOpen(r.Context())})
	})
	m.HandleFunc("GET /api/version", s.serveVersion) // public: the apps ask before they update
	m.HandleFunc("GET /health", s.health)            // public: the uptime check
	m.HandleFunc("GET /download/{os}", s.download)
	m.HandleFunc("GET /appcast.xml", s.appcast) // public: the Mac app's updater
	m.HandleFunc("POST /api/auth/login", s.login)
	m.HandleFunc("POST /api/sync/login", s.login)
	m.HandleFunc("POST /api/sync/pair", s.pair)
	m.HandleFunc("POST /api/auth/forgot", s.forgot)
	m.HandleFunc("POST /api/auth/setup", s.setupTeam)
	m.HandleFunc("POST /api/auth/join", s.join)
	m.HandleFunc("POST /api/auth/reset", s.useReset)
	m.HandleFunc("POST /api/auth/logout", s.logout)
	m.HandleFunc("POST /api/sync/logout", s.logout)

	// in: signed in, and allowed to do action ("" for looking).
	in := func(action string, h func(http.ResponseWriter, *http.Request, *User)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			u := s.user(r)
			if u == nil {
				http.Error(w, "sign in again", 401)
				return
			}
			if action != "" && !u.may(action) {
				http.Error(w, map[string]string{"decide": "You can only look in this team: ask an owner to make you an editor",
					"send": "Only editors and owners can send drives to the team", "tidy": "Only editors and owners can tidy drives with AI",
					"manage": "Only the team's owners can do this"}[action], 403)
				return
			}
			h(w, r, u)
		}
	}
	cat := func(h func(http.ResponseWriter, *http.Request, *Catalog)) http.HandlerFunc {
		return in("", func(w http.ResponseWriter, r *http.Request, u *User) {
			c, err := s.catalog(r.Context(), u.Team)
			if err != nil {
				fail(w, err)
				return
			}
			c.setOnlineAt(s.online.at(u.Team))
			h(w, r, c)
		})
	}
	m.HandleFunc("GET /api/me", in("", s.me))
	m.HandleFunc("POST /api/me/name", in("", s.rename))
	m.HandleFunc("POST /api/me/password", in("", s.password))
	m.HandleFunc("POST /api/me/logout", in("", s.logoutEverywhere))
	m.HandleFunc("POST /api/me/team", in("", s.switchTeam))
	m.HandleFunc("GET /api/overview", cat(func(w http.ResponseWriter, r *http.Request, c *Catalog) { send(w, c.Overview()) }))
	m.HandleFunc("GET /api/clips", cat(serveClips))
	m.HandleFunc("GET /api/clip", cat(serveClip))
	m.HandleFunc("GET /api/dups", cat(func(w http.ResponseWriter, r *http.Request, c *Catalog) { send(w, c.Dups()) }))
	m.HandleFunc("GET /api/status", in("", func(w http.ResponseWriter, r *http.Request, u *User) {
		s.mu.Lock()
		v := s.vers[u.Team]
		s.mu.Unlock()
		send(w, map[string]any{"status": "", "pending": []Pending{}, "version": v}) // the page refreshes when it moves
	}))
	m.HandleFunc("GET /api/plan", in("", func(w http.ResponseWriter, r *http.Request, u *User) { send(w, []Move{}) })) // only the Macs move files
	m.HandleFunc("POST /api/decide", in("decide", s.decide))
	m.HandleFunc("POST /api/sync/decide", in("decide", s.decide))
	m.HandleFunc("GET /thumbs/{thumb}/{file}", in("", s.thumb))
	m.HandleFunc("GET /api/team", in("", s.teamPage))
	m.HandleFunc("POST /api/team/rename", in("manage", s.renameTeam))
	m.HandleFunc("POST /api/team/delete", in("manage", s.deleteTeam))
	m.HandleFunc("GET /api/team/export", in("manage", s.exportTeam))
	m.HandleFunc("POST /api/team/role", in("manage", s.setRole))
	m.HandleFunc("POST /api/team/remove", in("manage", s.remove))
	m.HandleFunc("POST /api/team/reset", in("manage", s.makeReset))
	m.HandleFunc("POST /api/team/invite", in("manage", s.invite))
	m.HandleFunc("POST /api/team/pair", in("send", s.pairCode))
	m.HandleFunc("POST /api/team/revoke", in("manage", s.revokeInvite))
	m.HandleFunc("POST /api/team/drive/remove", in("manage", s.removeDrive))
	m.HandleFunc("POST /api/forget/{thumb}", in("manage", s.forget))
	m.HandleFunc("POST /api/team/mac/remove", in("", s.removeMac))
	m.HandleFunc("PUT /api/sync/drive/{uuid}", in("send", s.putDrive))
	m.HandleFunc("DELETE /api/sync/drive/{uuid}", in("send", s.removeDrive))
	m.HandleFunc("GET /api/sync/have", in("send", s.have))
	m.HandleFunc("PUT /api/sync/thumbs/{thumb}/{k}", in("send", s.putThumb))
	m.HandleFunc("POST /api/sync/looks", in("send", s.putLooks))
	m.HandleFunc("POST /api/sync/speech", in("send", s.putSpeech))
	m.HandleFunc("POST /api/sync/emb", in("send", s.putEmb))
	m.HandleFunc("GET /api/search", in("", s.search))
	m.HandleFunc("GET /api/compare", cat(serveCompare))
	m.HandleFunc("POST /api/sync/online", in("send", s.putOnline))
	m.HandleFunc("GET /api/sync/requests", in("send", s.pendingFor))
	m.HandleFunc("POST /api/sync/requests/{id}/claim", in("send", s.claim))
	m.HandleFunc("POST /api/sync/requests/{id}/sent", in("send", s.sent))
	m.HandleFunc("POST /api/original", in("decide", s.askOriginal))
	m.HandleFunc("GET /api/original/{id}", in("", s.showOriginal))
	m.HandleFunc("GET /api/sync/decisions", in("", s.decisionsAfter))
	m.HandleFunc("POST /api/sync/proxy/{thumb}", in("send", s.proxyUpload))
	m.HandleFunc("POST /api/sync/proxy/{thumb}/done", in("send", s.proxyDone))
	m.HandleFunc("GET /api/proxy/{thumb}", in("", s.proxyPlay))
	m.HandleFunc("POST /api/sync/tidy", in("tidy", s.tidy))
	return s.guard(m)
}

// csp: only the site itself.
func (s *Server) csp() string {
	return "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data:" +
		"; media-src 'self' https:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"
}

// guard: pages and the browser API need the session cookie; writes from a browser also need the custom
// header and the site's own origin (no other site can send them). The app signs every request with its token.
func (s *Server) guard(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// Every page and file is checked again on each visit, so after an update nobody sees an old version; the
		// handlers that must never be kept (previews, streams) say so themselves.
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", s.csp())
		// The app's sign-in and pairing set no cookie and answer only the caller, so they need neither.
		app := strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || r.URL.Path == "/api/sync/login" || r.URL.Path == "/api/sync/pair"
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !app {
			o, _ := url.Parse(r.Header.Get("Origin"))
			if r.Header.Get("X-Rushbin") != "1" || o == nil || o.Host != r.Host {
				http.Error(w, "only from the rushbin site", 403)
				return
			}
		}
		h.ServeHTTP(w, r)
	})
}

func (s *Server) user(r *http.Request) *User {
	tok, app := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !app {
		ck, err := r.Cookie("rbs")
		if err != nil {
			return nil
		}
		tok = ck.Value
	}
	var u User
	err := s.admin.QueryRow(r.Context(), `update session s set seen = now() from person p, member m, team t
		where s.hash = $1 and p.id = s.person and m.person = s.person and m.team = s.team and t.id = s.team
		  and s.seen > now() - (case when s.device = '' then $2 else $3 end)::interval
		returning s.id, p.id, p.email, p.name, s.team, t.name, m.role, s.device`, hash(tok), seconds(webSession), seconds(deviceSession)).
		Scan(&u.Session, &u.ID, &u.Email, &u.Name, &u.Team, &u.TeamName, &u.Role, &u.Device)
	if err != nil || app != (u.Device != "") {
		return nil
	}
	return &u
}

// startSession signs a person in to a team: a cookie for the browser, a token for the app (device set).
func (s *Server) startSession(w http.ResponseWriter, r *http.Request, q execer, person, team int64, device string) (string, error) {
	tok := token()
	if _, err := q.Exec(r.Context(), "insert into session (hash, person, team, device) values ($1, $2, $3, $4)", hash(tok), person, team, device); err != nil {
		return "", err
	}
	if device == "" {
		http.SetCookie(w, &http.Cookie{Name: "rbs", Value: tok, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode,
			Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https", MaxAge: int(webSession.Seconds())})
	}
	return tok, nil
}

// execer is a pool or a transaction.
type execer interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

type form struct {
	Name, Email, Password, Old, Token, Device, Role, UUID string
	ID, Team                                              int64
	TeamName                                              string `json:"team_name"` // a new team's
	Accept                                                bool   // the terms and the DPA, at sign-up
}

func readForm(w http.ResponseWriter, r *http.Request) (form, bool) {
	var f form
	if json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&f) != nil {
		http.Error(w, "bad request", 400)
		return f, false
	}
	f.Email = strings.ToLower(strings.TrimSpace(f.Email))
	f.Name = strings.TrimSpace(f.Name)
	return f, true
}

func goodPassword(p string) error {
	if len(p) < 10 || len(p) > 72 { // bcrypt reads at most 72 bytes
		return httpError{400, "The password needs 10 to 72 characters"}
	}
	return nil
}

var noAccount = sync.OnceValue(func() []byte { h, _ := bcrypt.GenerateFromPassword([]byte(token()), 12); return h })

// limited: n or more failures in 15 minutes under one of keys.
// ponytail: in memory, per server process; a restart forgets the counts.
func (s *Server) limited(n int, keys ...string) bool {
	s.fmu.Lock()
	defer s.fmu.Unlock()
	for _, k := range keys {
		recent := s.fails[k][:0]
		for _, t := range s.fails[k] {
			if time.Since(t) < 15*time.Minute {
				recent = append(recent, t)
			}
		}
		if len(recent) == 0 {
			delete(s.fails, k) // nothing kept past 15 minutes, as the privacy policy says, and no growth
		} else {
			s.fails[k] = recent
		}
		if len(recent) >= n {
			return true
		}
	}
	return false
}

// clientIP: the address the reverse proxy saw, last in X-Forwarded-For (Caddy replaces what clients send), when
// the request comes from that proxy: RUSHBIN_PROXY (an address or a host name, caddy in deploy/compose.yml), or
// this machine when it is not set. Any other peer is the client itself, whatever header it sends.
func (s *Server) clientIP(r *http.Request) string {
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	peer, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	peer = peer.Unmap()
	trusted := peer.IsLoopback()
	if s.proxy != "" { // looked up each time: the proxy's container may get a new address when it restarts
		addrs, _ := net.DefaultResolver.LookupNetIP(r.Context(), "ip", s.proxy)
		trusted = slices.ContainsFunc(addrs, func(a netip.Addr) bool { return a.Unmap() == peer })
	}
	if xs := strings.Split(r.Header.Get("X-Forwarded-For"), ","); trusted && strings.TrimSpace(xs[len(xs)-1]) != "" {
		return strings.TrimSpace(xs[len(xs)-1])
	}
	return ip
}

func (s *Server) failed(keys ...string) {
	s.fmu.Lock()
	defer s.fmu.Unlock()
	for _, k := range keys {
		s.fails[k] = append(s.fails[k], time.Now())
	}
}

var errWrongPassword = httpError{403, "Wrong email or password"}

// checkPassword returns the person with this email if the password is theirs, counting failures.
func (s *Server) checkPassword(r *http.Request, email, password string) (int64, error) {
	if len(email) > 254 { // longer than any address can be (RFC 5321)
		return 0, httpError{400, "Wrong email or password"}
	}
	// Counted per email and address, so that nobody locks someone else out by failing from elsewhere, and per
	// address, so that one address guesses at most 30 times in 15 minutes over all emails (an office behind one
	// address: three people mistyping their whole allowance). No ceiling per email alone: it would bring back the
	// lockout for anyone with a few addresses.
	ip := "ip:" + s.clientIP(r)
	if s.limited(10, "email:"+email+"|"+ip) || s.limited(30, ip) {
		return 0, httpError{429, "Too many attempts: try again in 15 minutes"}
	}
	keys := []string{"email:" + email + "|" + ip, ip}
	var id int64
	var pass []byte
	err := s.admin.QueryRow(r.Context(), "select id, pass from person where email = $1", email).Scan(&id, &pass)
	if err != nil {
		pass = noAccount() // an unknown email takes as long as a wrong password
	}
	if bcrypt.CompareHashAndPassword(pass, []byte(password)) != nil || err != nil {
		s.failed(keys...)
		return 0, errWrongPassword
	}
	return id, nil
}

// login: email and password, for the browser (cookie) or for the app (token, with the Mac's name).
// It opens the team the person joined first; the Team tab switches to the others.
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	f, ok := readForm(w, r)
	if !ok {
		return
	}
	id, err := s.checkPassword(r, f.Email, f.Password)
	if err != nil {
		fail(w, err)
		return
	}
	var team int64
	if s.admin.QueryRow(r.Context(), "select team from member where person = $1 order by created limit 1", id).Scan(&team) != nil {
		http.Error(w, "You are not in any team any more", 403)
		return
	}
	device := ""
	if r.URL.Path == "/api/sync/login" {
		device = cmp.Or(strings.TrimSpace(f.Device), "Mac")
	}
	s.signIn(w, r, id, team, device)
}

// signIn opens a session of person in team: a cookie for the browser, a token for a computer named device.
func (s *Server) signIn(w http.ResponseWriter, r *http.Request, person, team int64, device string) {
	var name, email, teamName, role string
	if s.admin.QueryRow(r.Context(), `select p.name, p.email, t.name, m.role from member m join person p on p.id = m.person
		join team t on t.id = m.team where m.person = $1 and m.team = $2`, person, team).Scan(&name, &email, &teamName, &role) != nil {
		http.Error(w, "You are not in this team any more", 403)
		return
	}
	if device != "" && role == "viewer" {
		http.Error(w, "Only editors and owners can connect a Mac: ask an owner of "+teamName, 403)
		return
	}
	tok, err := s.startSession(w, r, s.admin, person, team, device)
	if err != nil {
		fail(w, err)
		return
	}
	if device != "" {
		send(w, map[string]string{"token": tok, "name": name, "email": email, "team": teamName})
		return
	}
	send(w, map[string]bool{"ok": true})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	tok, app := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !app {
		if ck, err := r.Cookie("rbs"); err == nil {
			tok = ck.Value
		}
		http.SetCookie(w, &http.Cookie{Name: "rbs", Path: "/", MaxAge: -1})
	}
	s.admin.Exec(r.Context(), "delete from session where hash = $1", hash(tok))
	send(w, map[string]bool{"ok": true})
}

// logoutEverywhere ends every session of the person: browsers and computers, in every team.
func (s *Server) logoutEverywhere(w http.ResponseWriter, r *http.Request, u *User) {
	if _, err := s.admin.Exec(r.Context(), "delete from session where person = $1", u.ID); err != nil {
		fail(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "rbs", Path: "/", MaxAge: -1})
	send(w, map[string]bool{"ok": true})
}

// join: whoever opens an invite enters the team with the role chosen by the owner. A new person picks a
// name and a password; someone already in another team signs in with theirs. An invite is a link that may have
// been passed on by hand, so it proves no address: the first to open an invite holds it, until no team holds them
// (dropLeftover).
func (s *Server) join(w http.ResponseWriter, r *http.Request) {
	f, ok := readForm(w, r)
	if !ok {
		return
	}
	err := pgx.BeginFunc(r.Context(), s.admin, func(tx pgx.Tx) error {
		var team int64
		var email, role string
		err := tx.QueryRow(r.Context(), "delete from invite where hash = $1 and created > now() - $2::interval returning team, email, role",
			hash(f.Token), fmt.Sprintf("%d days", linkDays)).Scan(&team, &email, &role)
		if err != nil {
			return httpError{403, "This invite is not valid any more: ask for a new one"}
		}
		var person int64
		if tx.QueryRow(r.Context(), "select id from person where email = $1", email).Scan(&person) == nil {
			if person, err = s.checkPassword(r, email, f.Password); err == errWrongPassword {
				return httpError{403, "This email already has a rushbin account: write its password. If you never made one, someone else opened an invite for your address: tell whoever runs this site"}
			} else if err != nil {
				return err
			}
		} else {
			if f.Name == "" {
				return httpError{400, "Write your name"}
			}
			if err := goodPassword(f.Password); err != nil {
				return err
			}
			pass, _ := bcrypt.GenerateFromPassword([]byte(f.Password), 12)
			if err := tx.QueryRow(r.Context(), "insert into person (email, name, pass) values ($1, $2, $3) returning id", email, f.Name, pass).Scan(&person); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(r.Context(), "insert into member (team, person, role) values ($1, $2, $3) on conflict do nothing", team, person, role); err != nil {
			return err
		}
		_, err = s.startSession(w, r, tx, person, team, "")
		return err
	})
	if err != nil {
		fail(w, err)
		return
	}
	send(w, map[string]bool{"ok": true})
}

// A password is the person's, for every team they are in: an owner may reset it only for someone who is in
// their team alone, or an owner of one team could take over the account and, with it, the person's other teams.
var errOtherTeams = httpError{403, "This person is also in another team: only they can change their password, from their profile"}

func onlyIn(ctx context.Context, db interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, person, team int64) bool {
	var ok bool
	db.QueryRow(ctx, `select exists(select 1 from member where person = $1 and team = $2)
		and not exists(select 1 from member where person = $1 and team <> $2)`, person, team).Scan(&ok)
	return ok
}

// useReset sets a new password from a link an owner made, or one emailed to the person (forgot). Every session
// of that person ends, Macs too.
func (s *Server) useReset(w http.ResponseWriter, r *http.Request) {
	f, ok := readForm(w, r)
	if !ok {
		return
	}
	err := pgx.BeginFunc(r.Context(), s.admin, func(tx pgx.Tx) error {
		if err := goodPassword(f.Password); err != nil {
			return err
		}
		var person, team int64
		var mailed bool
		err := tx.QueryRow(r.Context(), `delete from reset where hash = $1
			and created > now() - (case when mailed then $2 else $3 end)::interval returning person, team, mailed`,
			hash(f.Token), seconds(mailedLink), fmt.Sprintf("%d days", linkDays)).Scan(&person, &team, &mailed)
		if err != nil {
			return httpError{403, "This link is not valid any more: ask for a new one"}
		}
		if !mailed && !onlyIn(r.Context(), tx, person, team) { // joined another team after an owner made the link
			return errOtherTeams
		}
		pass, _ := bcrypt.GenerateFromPassword([]byte(f.Password), 12)
		if _, err := tx.Exec(r.Context(), "update person set pass = $1 where id = $2", pass, person); err != nil {
			return err
		}
		if _, err := tx.Exec(r.Context(), "delete from session where person = $1", person); err != nil {
			return err
		}
		if _, err := tx.Exec(r.Context(), "delete from reset where person = $1", person); err != nil { // any other link for them
			return err
		}
		_, err = s.startSession(w, r, tx, person, team, "")
		return err
	})
	if err != nil {
		fail(w, err)
		return
	}
	send(w, map[string]bool{"ok": true})
}

// me: who is signed in, their role and the teams they can switch to.
func (s *Server) me(w http.ResponseWriter, r *http.Request, u *User) {
	type team struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	teams := []team{}
	rows, _ := s.admin.Query(r.Context(), "select t.id, t.name from member m join team t on t.id = m.team where m.person = $1 order by m.created", u.ID)
	for rows.Next() {
		var t team
		if rows.Scan(&t.ID, &t.Name) == nil {
			teams = append(teams, t)
		}
	}
	me := map[string]any{"site": true, "id": u.ID, "name": u.Name, "email": u.Email, "team": u.TeamName, "team_id": u.Team,
		"role": u.Role, "owner": u.Role == "owner", "teams": teams}
	if u.Role == "owner" {
		me["backup"] = s.backupAlarm(time.Now())
	}
	send(w, me)
}

// health answers "ok" when the site, its database, its nightly copies and its disk are fine, else 503 with what is
// not (never the details, the page is public). An external check asks every 30 s and mails when it fails.
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	switch {
	case s.admin.Ping(ctx) != nil:
		http.Error(w, "database", 503)
	case s.backupAlarm(time.Now()) != "":
		http.Error(w, "backup", 503)
	case s.diskLow():
		http.Error(w, "disk", 503)
	default:
		w.Write([]byte("ok\n"))
	}
}

// minFree: below this share of free disk the site is not healthy, long before the database and previews stop.
const minFree = 0.10

// diskLow: the disk of the site's data folder (previews, and on the same disk the database) is nearly full.
func (s *Server) diskLow() bool {
	free, err := freeShare(s.dir)
	return err == nil && free < minFree
}

// backupAlarm: what is wrong with the database copies, "" when nothing is. deploy/backup.sh and deploy/restore-test.sh
// record their outcome in the data folder (backup.json, restore-test.json); a site without them has no alarm.
func (s *Server) backupAlarm(now time.Time) string {
	var alarms []string
	if o, found := readOutcome(filepath.Join(s.dir, "backup.json")); found && !o.OK {
		alarms = append(alarms, "The last database copy failed: "+o.Reason)
	} else if found && now.Sub(o.At) > 36*time.Hour {
		alarms = append(alarms, "No database copy since "+o.At.UTC().Format("2 Jan 15:04 UTC"))
	}
	if o, found := readOutcome(filepath.Join(s.dir, "restore-test.json")); found && !o.OK {
		alarms = append(alarms, "The last restore test of a database copy failed: "+o.Reason)
	}
	return strings.Join(alarms, ". ")
}

type outcome struct {
	At     time.Time `json:"at"`
	OK     bool      `json:"ok"`
	Reason string    `json:"reason"`
}

func readOutcome(path string) (o outcome, found bool) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return o, false
	}
	if err == nil {
		err = json.Unmarshal(data, &o)
	}
	if err != nil {
		return outcome{Reason: "its record cannot be read (" + err.Error() + ")"}, true
	}
	return o, true
}

func (s *Server) rename(w http.ResponseWriter, r *http.Request, u *User) {
	f, ok := readForm(w, r)
	if !ok {
		return
	}
	if f.Name == "" {
		http.Error(w, "Write your name", 400)
		return
	}
	s.admin.Exec(r.Context(), "update person set name = $1 where id = $2", f.Name, u.ID)
	send(w, map[string]bool{"ok": true})
}

// password changes one's own password; the other sessions end, this one stays.
func (s *Server) password(w http.ResponseWriter, r *http.Request, u *User) {
	f, ok := readForm(w, r)
	if !ok {
		return
	}
	if _, err := s.checkPassword(r, u.Email, f.Old); err != nil {
		fail(w, err)
		return
	}
	if err := goodPassword(f.Password); err != nil {
		fail(w, err)
		return
	}
	pass, _ := bcrypt.GenerateFromPassword([]byte(f.Password), 12)
	s.admin.Exec(r.Context(), "update person set pass = $1 where id = $2", pass, u.ID)
	s.admin.Exec(r.Context(), "delete from session where person = $1 and id <> $2", u.ID, u.Session)
	send(w, map[string]bool{"ok": true})
}

func (s *Server) switchTeam(w http.ResponseWriter, r *http.Request, u *User) {
	f, ok := readForm(w, r)
	if !ok {
		return
	}
	tag, err := s.admin.Exec(r.Context(), "update session set team = $1 where id = $2 and device = '' and exists (select 1 from member where team = $1 and person = $3)",
		f.Team, u.Session, u.ID)
	if err != nil || tag.RowsAffected() != 1 {
		http.Error(w, "You are not in that team", 403)
		return
	}
	send(w, map[string]bool{"ok": true})
}

// teamPage: members with their role and Macs, open invites. Read through the team's rows only.
func (s *Server) teamPage(w http.ResponseWriter, r *http.Request, u *User) {
	type mac struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	type member struct {
		ID    int64  `json:"id"`
		Name  string `json:"name"`
		Email string `json:"email"`
		Role  string `json:"role"`
		Macs  []mac  `json:"macs"`
	}
	out := map[string]any{"name": u.TeamName, "me": u.ID}
	err := s.inTeam(r.Context(), u.Team, func(tx pgx.Tx) error {
		ms := []*member{}
		byID := map[int64]*member{}
		rows, _ := tx.Query(r.Context(), "select p.id, p.name, p.email, m.role from member m join person p on p.id = m.person where m.team = $1 order by p.name", u.Team)
		for rows.Next() {
			m := &member{Macs: []mac{}}
			if err := rows.Scan(&m.ID, &m.Name, &m.Email, &m.Role); err != nil {
				return err
			}
			ms = append(ms, m)
			byID[m.ID] = m
		}
		rows, _ = tx.Query(r.Context(), "select id, person, device from session where team = $1 and device <> '' order by device", u.Team)
		for rows.Next() {
			var c mac
			var p int64
			if err := rows.Scan(&c.ID, &p, &c.Name); err != nil {
				return err
			}
			if m := byID[p]; m != nil {
				m.Macs = append(m.Macs, c)
			}
		}
		invites := []map[string]any{}
		rows, _ = tx.Query(r.Context(), "select email, role, created from invite where team = $1 and created > now() - $2::interval order by created", u.Team, fmt.Sprintf("%d days", linkDays))
		for rows.Next() {
			var e, role string
			var t time.Time
			if err := rows.Scan(&e, &role, &t); err != nil {
				return err
			}
			invites = append(invites, map[string]any{"email": e, "role": role, "created": t})
		}
		out["members"], out["invites"] = ms, invites
		return rows.Err()
	})
	if err != nil {
		fail(w, err)
		return
	}
	send(w, out)
}

func (s *Server) renameTeam(w http.ResponseWriter, r *http.Request, u *User) {
	f, ok := readForm(w, r)
	if !ok {
		return
	}
	if f.Name == "" {
		http.Error(w, "Write the team's name", 400)
		return
	}
	err := s.inTeam(r.Context(), u.Team, func(tx pgx.Tx) error {
		_, err := tx.Exec(r.Context(), "update team set name = $1 where id = $2", f.Name, u.Team)
		return err
	})
	if err != nil {
		fail(w, err)
		return
	}
	send(w, map[string]bool{"ok": true})
}

// deleteTeam erases the team: members, catalogs, decisions and previews. The owner types its name to confirm.
func (s *Server) deleteTeam(w http.ResponseWriter, r *http.Request, u *User) {
	f, ok := readForm(w, r)
	if !ok {
		return
	}
	if f.Name != u.TeamName {
		http.Error(w, "Type the team's name exactly to delete it", 400)
		return
	}
	var objects []string // its light copies and originals in the store go with it
	err := s.inTeam(r.Context(), u.Team, func(tx pgx.Tx) error {
		rows, err := tx.Query(r.Context(), "select thumb from proxy")
		if err != nil {
			return err
		}
		for rows.Next() {
			var th string
			if rows.Scan(&th) == nil {
				objects = append(objects, proxyKey(u.Team, th))
			}
		}
		rows, err = tx.Query(r.Context(), "select id, rel from request")
		if err != nil {
			return err
		}
		for rows.Next() {
			var id int64
			var rel string
			if rows.Scan(&id, &rel) == nil {
				objects = append(objects, originalKey(u.Team, id, filepath.Base(rel)))
			}
		}
		_, err = tx.Exec(r.Context(), "delete from team where id = $1", u.Team)
		return err
	})
	if err != nil {
		fail(w, err)
		return
	}
	if err := s.deleteObjects(r.Context(), objects); err != nil {
		log.Printf("delete team %d: %v", u.Team, err)
	}
	s.dropLeftover(r.Context())
	os.RemoveAll(filepath.Join(s.dir, "teams", strconv.FormatInt(u.Team, 10)))
	s.changed(u.Team)
	http.SetCookie(w, &http.Cookie{Name: "rbs", Path: "/", MaxAge: -1})
	send(w, map[string]bool{"ok": true})
}

// ownersLeft fails when a change would leave the team without an owner.
func ownersLeft(ctx context.Context, tx pgx.Tx, team int64) error {
	var n int
	if err := tx.QueryRow(ctx, "select count(*) from member where team = $1 and role = 'owner'", team).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return httpError{400, "The team needs at least one owner"}
	}
	return nil
}

func (s *Server) setRole(w http.ResponseWriter, r *http.Request, u *User) {
	f, ok := readForm(w, r)
	if !ok {
		return
	}
	if !slices.Contains(roles, f.Role) {
		http.Error(w, "unknown role", 400)
		return
	}
	err := s.inTeam(r.Context(), u.Team, func(tx pgx.Tx) error {
		tag, err := tx.Exec(r.Context(), "update member set role = $1 where team = $2 and person = $3", f.Role, u.Team, f.ID)
		if err != nil || tag.RowsAffected() != 1 {
			return httpError{404, "Not a member of this team"}
		}
		if f.Role == "viewer" { // a viewer's Macs stop sending
			if _, err := tx.Exec(r.Context(), "delete from session where team = $1 and person = $2 and device <> ''", u.Team, f.ID); err != nil {
				return err
			}
		}
		return ownersLeft(r.Context(), tx, u.Team)
	})
	if err != nil {
		fail(w, err)
		return
	}
	send(w, map[string]bool{"ok": true})
}

// remove takes someone out of this team: their sessions here end, their decisions keep their name.
// Someone left in no team loses the account (dropLeftover).
func (s *Server) remove(w http.ResponseWriter, r *http.Request, u *User) {
	f, ok := readForm(w, r)
	if !ok {
		return
	}
	err := s.inTeam(r.Context(), u.Team, func(tx pgx.Tx) error {
		tag, err := tx.Exec(r.Context(), "delete from member where team = $1 and person = $2", u.Team, f.ID)
		if err != nil || tag.RowsAffected() != 1 {
			return httpError{404, "Not a member of this team"}
		}
		if _, err := tx.Exec(r.Context(), "delete from session where team = $1 and person = $2", u.Team, f.ID); err != nil {
			return err
		}
		return ownersLeft(r.Context(), tx, u.Team)
	})
	if err != nil {
		fail(w, err)
		return
	}
	s.dropLeftover(r.Context())
	send(w, map[string]bool{"ok": true})
}

// dropLeftover deletes the accounts no team holds any more. An account stands only on the invites teams made
// for its email, so when the last team lets it go the address is free for the next invite: one opened by
// someone else through a leaked link no longer occupies it for good. A failure waits for the next call.
func (s *Server) dropLeftover(ctx context.Context) {
	s.admin.Exec(ctx, "delete from person p where not exists (select 1 from member m where m.person = p.id)")
}

// siteURL is the address of this site as the browser sees it, for the links owners send.
func siteURL(r *http.Request) string {
	scheme := "https"
	if r.TLS == nil && r.Header.Get("X-Forwarded-Proto") != "https" && isLocal(strings.Split(r.Host, ":")[0]) {
		scheme = "http"
	}
	return scheme + "://" + r.Host
}

// makeReset: a link that sets a new password for a member, valid 7 days and once. The site emails it when it
// can, else the owner sends it.
func (s *Server) makeReset(w http.ResponseWriter, r *http.Request, u *User) {
	f, ok := readForm(w, r)
	if !ok {
		return
	}
	tok := token()
	var email string
	err := s.inTeam(r.Context(), u.Team, func(tx pgx.Tx) error {
		if tx.QueryRow(r.Context(), "select p.email from member m join person p on p.id = m.person where m.team = $1 and m.person = $2", u.Team, f.ID).Scan(&email) != nil {
			return httpError{404, "Not a member of this team"}
		}
		if !onlyIn(r.Context(), s.admin, f.ID, u.Team) {
			return errOtherTeams
		}
		if _, err := tx.Exec(r.Context(), "delete from reset where person = $1", f.ID); err != nil { // only the newest link works
			return err
		}
		_, err := tx.Exec(r.Context(), "insert into reset (hash, person, team) values ($1, $2, $3)", hash(tok), f.ID, u.Team)
		return err
	})
	if err != nil {
		fail(w, err)
		return
	}
	s.offer(w, r, email, Letter{Subject: "A new password for rushbin", Text: u.Name + ", an owner of " + u.TeamName + ", made you a link to choose a new rushbin password.",
		Button: "Choose a new password", Link: siteURL(r) + "/login#reset=" + tok,
		After: fmt.Sprintf("The link works once, for %d days. Every device signed in with the old password will need to sign in again.", linkDays)})
}

// invite makes a link for one email and one role, valid 7 days and once. The site emails it when it can, else the
// owner sends it.
func (s *Server) invite(w http.ResponseWriter, r *http.Request, u *User) {
	f, ok := readForm(w, r)
	if !ok {
		return
	}
	if f.Email = emailOf(f.Email); f.Email == "" || !slices.Contains(roles, f.Role) {
		http.Error(w, "Write a valid email and choose a role", 400)
		return
	}
	tok := token()
	err := s.inTeam(r.Context(), u.Team, func(tx pgx.Tx) error {
		var n int
		tx.QueryRow(r.Context(), "select count(*) from member m join person p on p.id = m.person where m.team = $1 and p.email = $2", u.Team, f.Email).Scan(&n)
		if n > 0 {
			return httpError{400, "This person is already in the team"}
		}
		if _, err := tx.Exec(r.Context(), "delete from invite where team = $1 and email = $2", u.Team, f.Email); err != nil { // the newest link wins
			return err
		}
		_, err := tx.Exec(r.Context(), "insert into invite (hash, team, email, role) values ($1, $2, $3, $4)", hash(tok), u.Team, f.Email, f.Role)
		return err
	})
	if err != nil {
		fail(w, err)
		return
	}
	s.offer(w, r, f.Email, Letter{Subject: u.Name + " invited you to " + u.TeamName + " on rushbin",
		Text:   u.Name + " invited you to " + u.TeamName + ", the team's video archive on rushbin, as " + map[string]string{"owner": "an owner", "editor": "an editor", "viewer": "a viewer"}[f.Role] + ".",
		Button: "Join " + u.TeamName, Link: siteURL(r) + "/login#invite=" + tok,
		After: fmt.Sprintf("The link works once, for %d days. If you did not expect it, ignore this email.", linkDays)})
}

func (s *Server) revokeInvite(w http.ResponseWriter, r *http.Request, u *User) {
	f, ok := readForm(w, r)
	if !ok {
		return
	}
	err := s.inTeam(r.Context(), u.Team, func(tx pgx.Tx) error {
		_, err := tx.Exec(r.Context(), "delete from invite where team = $1 and email = $2", u.Team, f.Email)
		return err
	})
	if err != nil {
		fail(w, err)
		return
	}
	send(w, map[string]bool{"ok": true})
}

// removeDrive takes a drive out of the team: an owner from the site, or a Mac whose drive is no longer sent
// ("Send to the team" off). A Mac that sends it again brings it back.
func (s *Server) removeDrive(w http.ResponseWriter, r *http.Request, u *User) {
	uuid := r.PathValue("uuid") // a Mac: DELETE /api/sync/drive/{uuid}
	if uuid == "" {
		f, ok := readForm(w, r)
		if !ok {
			return
		}
		uuid = f.UUID
	}
	if err := s.dropDrive(r.Context(), u.Team, uuid); err != nil {
		fail(w, err)
		return
	}
	s.changed(u.Team)
	send(w, map[string]bool{"ok": true})
}

// dropDrive deletes a drive's catalog and what the site holds of the clips no other drive of the team has: their
// previews, what Apple Vision saw, what is said, search vectors and light copies; and the originals asked of the
// drive (the records stay, expired, until the retention deletes them; the files go now). Decisions stay: they are
// the team's, by content, with who took them; other drives may hold the same clips, and if the drive comes back
// they apply again. Held like a removal on request (forget), so nothing a Mac sends meanwhile is half taken.
func (s *Server) dropDrive(ctx context.Context, team int64, uuid string) error {
	var objects []string
	err := s.inTeam(ctx, team, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "select pg_advisory_xact_lock($1)", team); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, "delete from drive where team = $1 and uuid = $2", team, uuid)
		if err != nil {
			return err
		} else if tag.RowsAffected() != 1 {
			return httpError{404, "No such drive in this team"}
		}
		kept := map[string]bool{} // the contents the other drives still have
		rows, _ := tx.Query(ctx, "select files from drive where team = $1", team)
		for rows.Next() {
			var files []byte
			if err := rows.Scan(&files); err != nil {
				return err
			}
			recsFrom(bytes.NewReader(files), "files.jsonl", func(r Rec) { kept[notAlnum.ReplaceAllString("c:"+r.Hash, "_")] = true })
		}
		if rows.Err() != nil {
			return rows.Err()
		}
		keep := make([]string, 0, len(kept)) // never nil: "= any(NULL)" would keep everything
		for th := range kept {
			keep = append(keep, th)
		}
		for _, t := range []string{"look", "speech", "frame"} {
			if _, err := tx.Exec(ctx, "delete from "+t+" where team = $1 and not thumb = any($2)", team, keep); err != nil {
				return err
			}
		}
		rows, _ = tx.Query(ctx, "delete from proxy where team = $1 and not thumb = any($2) returning thumb", team, keep)
		for rows.Next() {
			var th string
			if err := rows.Scan(&th); err != nil {
				return err
			}
			objects = append(objects, proxyKey(team, th))
		}
		if rows.Err() != nil {
			return rows.Err()
		}
		rows, _ = tx.Query(ctx, `update request set state = 'expired', note = 'The drive was removed from the team', updated = now()
			where team = $1 and device = $2 and state <> 'expired' returning id, rel`, team, uuid)
		for rows.Next() {
			var id int64
			var rel string
			if err := rows.Scan(&id, &rel); err != nil {
				return err
			}
			objects = append(objects, originalKey(team, id, filepath.Base(rel)))
		}
		if rows.Err() != nil {
			return rows.Err()
		}
		// The previews go while the lock is held: a preview a Mac sends now waits, and is not deleted after it arrives.
		es, _ := os.ReadDir(filepath.Join(s.dir, "teams", strconv.FormatInt(team, 10), "thumbs"))
		for _, e := range es {
			if !kept[e.Name()] {
				if err := os.RemoveAll(s.thumbDir(team, e.Name())); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	return s.deleteObjects(ctx, objects)
}

// forget erases what the site holds of one content, for a person filmed in it who asked to be removed: its
// previews, what Apple Vision saw, what is said, its search vectors, its light copy and any original in the
// store. The clip stays in the catalog (its file is still on the drive), marked removed, and the record of
// who removed it and when makes the sync ignore whatever a Mac sends of it later. A Mac sending in the same
// instant waits for the removal, or the removal for it (syncing): nothing it sends outlives the removal.
func (s *Server) forget(w http.ResponseWriter, r *http.Request, u *User) {
	th := r.PathValue("thumb")
	c, err := s.catalog(r.Context(), u.Team)
	if err != nil {
		fail(w, err)
		return
	}
	i := slices.IndexFunc(c.list, func(cl *Clip) bool { return cl.Thumb == th })
	if i < 0 {
		http.Error(w, "No such clip in this team", 404)
		return
	}
	objects := []string{proxyKey(u.Team, th)}
	err = s.inTeam(r.Context(), u.Team, func(tx pgx.Tx) error {
		if _, err := tx.Exec(r.Context(), "select pg_advisory_xact_lock($1)", u.Team); err != nil {
			return err
		}
		if _, err := tx.Exec(r.Context(), "insert into forgotten (team, thumb, person, by_name) values ($1, $2, $3, $4) on conflict do nothing",
			u.Team, th, u.ID, u.Name); err != nil {
			return err
		}
		for _, t := range []string{"look", "speech", "frame", "proxy"} {
			if _, err := tx.Exec(r.Context(), "delete from "+t+" where team = $1 and thumb = $2", u.Team, th); err != nil {
				return err
			}
		}
		// Every original ever asked for, also those already expired: deleting again is harmless, and a retry finds them all.
		rows, _ := tx.Query(r.Context(), `update request set state = 'expired', note = 'Removed on request', updated = now()
			where team = $1 and content = $2 returning id, rel`, u.Team, c.list[i].Content)
		for rows.Next() {
			var id int64
			var rel string
			if err := rows.Scan(&id, &rel); err != nil {
				return err
			}
			objects = append(objects, originalKey(u.Team, id, filepath.Base(rel)))
		}
		return rows.Err()
	})
	if err != nil {
		fail(w, err)
		return
	}
	s.changed(u.Team)
	if err := errors.Join(os.RemoveAll(s.thumbDir(u.Team, th)), s.deleteObjects(r.Context(), objects)); err != nil {
		log.Printf("forget %s in team %d: %v", th, u.Team, err)
		http.Error(w, "Removed from the catalog, but some files could not be deleted: try again", 502)
		return
	}
	send(w, map[string]bool{"ok": true})
}

// deleteObjects deletes keys from the store, all of them even when some fail.
func (s *Server) deleteObjects(ctx context.Context, keys []string) error {
	var errs []error
	for _, k := range keys {
		if s.store == nil {
			break
		}
		res, err := s.store.do(ctx, "DELETE", k, nil)
		if err == nil {
			res.Body.Close()
		}
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// syncing runs fn, a Mac sending what it has of contents, holding the team's lock shared: a removal (forget)
// holds it alone, so it waits for the sends in progress and the sends after it see its record. The only
// advisory lock of rushbin, keyed by the team.
func (s *Server) syncing(ctx context.Context, team int64, fn func(pgx.Tx) error) error {
	return s.inTeam(ctx, team, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "select pg_advisory_xact_lock_shared($1)", team); err != nil {
			return err
		}
		return fn(tx)
	})
}

// isForgotten: the content was removed on request, so nothing of it is taken any more.
func isForgotten(ctx context.Context, tx pgx.Tx, team int64, th string) (yes bool, err error) {
	err = tx.QueryRow(ctx, "select exists (select 1 from forgotten where team = $1 and thumb = $2)", team, th).Scan(&yes)
	return yes, err
}

// removeMac disconnects one Mac: its owner's own, or any for a team owner.
func (s *Server) removeMac(w http.ResponseWriter, r *http.Request, u *User) {
	f, ok := readForm(w, r)
	if !ok {
		return
	}
	err := s.inTeam(r.Context(), u.Team, func(tx pgx.Tx) error {
		tag, err := tx.Exec(r.Context(), "delete from session where team = $1 and id = $2 and device <> '' and (person = $3 or $4)", u.Team, f.ID, u.ID, u.may("manage"))
		if err == nil && tag.RowsAffected() != 1 {
			return httpError{404, "No such Mac, or not yours"}
		}
		return err
	})
	if err != nil {
		fail(w, err)
		return
	}
	send(w, map[string]bool{"ok": true})
}

// catalog builds a team's catalog from what its Macs sent, once, then keeps it until they send more. Each team
// builds under a lock of its own, so a big catalog makes only its own team wait. A row that cannot be read (data
// from an older site, which checked less) is left out and logged, and the rest of the team's catalog stands.
// ponytail: every drive's files.jsonl is parsed again on each change; keep parsed drives if teams get big.
func (s *Server) catalog(ctx context.Context, team int64) (*Catalog, error) {
	s.mu.Lock()
	c, lock := s.cats[team], s.building[team]
	if lock == nil {
		lock = &sync.Mutex{}
		s.building[team] = lock
	}
	s.mu.Unlock()
	if c != nil {
		return c, nil
	}
	lock.Lock()
	defer lock.Unlock()
	s.mu.Lock()
	c, v := s.cats[team], s.vers[team]
	s.mu.Unlock()
	if c != nil { // built while this request waited
		return c, nil
	}
	skip := func(what string, err error) { log.Printf("catalog of team %d: %s left out: %v", team, what, err) }
	err := s.inTeam(ctx, team, func(tx pgx.Tx) error {
		var ds []drive
		rows, _ := tx.Query(ctx, "select uuid, device, files, fullhash from drive where team = $1", team)
		for rows.Next() {
			var uuid string
			var dev, files, full []byte
			if err := rows.Scan(&uuid, &dev, &files, &full); err != nil {
				return err
			}
			d := drive{files: bytes.NewReader(files)}
			var err error
			if err = json.Unmarshal(dev, &d.Device); err == nil {
				d.valid, err = fullHashes(bytes.NewReader(full))
			}
			if err != nil {
				skip("drive "+uuid, err)
				continue
			}
			ds = append(ds, d)
		}
		if rows.Err() != nil {
			return rows.Err()
		}
		looks := map[string]*Look{}
		rows, _ = tx.Query(ctx, "select thumb, frames from look where team = $1", team)
		for rows.Next() {
			var th string
			var raw []byte
			var fr []Frame
			if err := rows.Scan(&th, &raw); err != nil {
				return err
			}
			if err := json.Unmarshal(raw, &fr); err != nil {
				skip("what Vision saw in "+th, err)
				continue
			}
			looks[th] = summarize(fr)
		}
		ls, err := decisionLines(ctx, tx, team, 0)
		if err != nil {
			return err
		}
		dec := map[string]*Decision{}
		for _, l := range ls {
			l.apply(dec)
		}
		if c, err = buildCatalog(ds, nil, dec, looks); err != nil {
			return err
		}
		rows, _ = tx.Query(ctx, "select thumb, segs from speech where team = $1", team)
		for rows.Next() {
			var th string
			var raw []byte
			var segs []Segment
			if err := rows.Scan(&th, &raw); err != nil {
				return err
			}
			if err := json.Unmarshal(raw, &segs); err != nil {
				skip("what is said in "+th, err)
				continue
			}
			c.speech[th] = newSpeech(segs)
		}
		if rows.Err() != nil {
			return rows.Err()
		}
		c.proxies, c.forgotten = map[string]bool{}, map[string]bool{}
		for q, m := range map[string]map[string]bool{"select thumb from proxy where team = $1": c.proxies, "select thumb from forgotten where team = $1": c.forgotten} {
			rows, _ = tx.Query(ctx, q, team)
			for rows.Next() {
				var th string
				if err := rows.Scan(&th); err != nil {
					return err
				}
				m[th] = true
			}
			if rows.Err() != nil {
				return rows.Err()
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.vers[team] == v { // nothing changed while it was built: else the next request builds again
		s.cats[team] = c
	}
	s.mu.Unlock()
	return c, nil
}

func (s *Server) changed(team int64) {
	s.mu.Lock()
	delete(s.cats, team)
	s.vers[team]++
	s.mu.Unlock()
}

// bump tells the team's open pages that its data moved (they refresh at their next status poll).
func (s *Server) bump(team int64) {
	s.mu.Lock()
	s.vers[team]++
	s.mu.Unlock()
}

func decisionLines(ctx context.Context, tx pgx.Tx, team, after int64) ([]DecisionLine, error) {
	ls := []DecisionLine{}
	rows, _ := tx.Query(ctx, "select id, t, content, kind, value, by_name from decision where team = $1 and id > $2 order by id", team, after)
	for rows.Next() {
		var l DecisionLine
		var t time.Time
		if err := rows.Scan(&l.ID, &t, &l.Content, &l.Kind, &l.Value, &l.By); err != nil {
			return nil, err
		}
		l.T = t.UTC().Format(time.RFC3339Nano)
		ls = append(ls, l)
	}
	return ls, rows.Err()
}

// decide records a decision of a team member, from the site or from a Mac, with their name.
func (s *Server) decide(w http.ResponseWriter, r *http.Request, u *User) {
	var l DecisionLine
	if json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&l) != nil {
		http.Error(w, "bad request", 400)
		return
	}
	c, err := s.catalog(r.Context(), u.Team)
	if err != nil || !c.valid(l) {
		http.Error(w, "invalid decision", 400)
		return
	}
	var t time.Time
	err = s.inTeam(r.Context(), u.Team, func(tx pgx.Tx) error {
		return tx.QueryRow(r.Context(), "insert into decision (team, content, kind, value, by_name, person) values ($1, $2, $3, $4, $5, $6) returning id, t",
			u.Team, l.Content, l.Kind, l.Value, u.Name, u.ID).Scan(&l.ID, &t)
	})
	if err != nil {
		fail(w, err)
		return
	}
	l.T, l.By = t.UTC().Format(time.RFC3339Nano), u.Name
	c.decide(l)
	s.bump(u.Team)
	send(w, l)
}

func (s *Server) decisionsAfter(w http.ResponseWriter, r *http.Request, u *User) {
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	var ls []DecisionLine
	err := s.inTeam(r.Context(), u.Team, func(tx pgx.Tx) (err error) { ls, err = decisionLines(r.Context(), tx, u.Team, after); return })
	if err != nil {
		fail(w, err)
		return
	}
	send(w, ls)
}

var thumbName = regexp.MustCompile(`^[A-Za-z0-9_]{1,100}$`)

func (s *Server) thumbDir(team int64, th string) string {
	return filepath.Join(s.dir, "teams", strconv.FormatInt(team, 10), "thumbs", th)
}

func (s *Server) thumb(w http.ResponseWriter, r *http.Request, u *User) {
	th, f := r.PathValue("thumb"), r.PathValue("file")
	if !thumbName.MatchString(th) || len(f) != 5 || f[0] < '1' || f[0] > '5' || f[1:] != ".jpg" {
		http.NotFound(w, r)
		return
	}
	// Asked again on each view (304 while unchanged): a preview removed on request, or of a team someone left,
	// is gone from every browser at once.
	w.Header().Set("Cache-Control", "private, no-cache")
	http.ServeFile(w, r, filepath.Join(s.thumbDir(u.Team, th), f))
}

// putDrive replaces what the team knows of one drive with the catalog files a Mac sends. The parts go to disk as
// they arrive and are checked there, so a slow or large upload holds no memory; then one drive at a time, for the
// whole site, goes into the database, which takes it whole in memory.
func (s *Server) putDrive(w http.ResponseWriter, r *http.Request, u *User) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<30) // ponytail: one drive's catalog up to 1 GB (about 3 million files)
	mr, err := r.MultipartReader()
	if err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	tmp, err := os.MkdirTemp(s.dir, ".drive-")
	if err != nil {
		fail(w, err)
		return
	}
	defer os.RemoveAll(tmp)
	part := func(name string) string { return filepath.Join(tmp, name) }
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		} else if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if n := p.FormName(); n != "device.json" && n != "files.jsonl" && n != "fullhash.tsv" {
			http.Error(w, "unknown part "+n, 400)
			return
		}
		f, err := os.Create(part(p.FormName()))
		if err == nil {
			_, err = io.Copy(f, p)
			err = errors.Join(err, f.Close())
		}
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
	}
	var d Device
	if b, err := os.ReadFile(part("device.json")); err != nil || json.Unmarshal(b, &d) != nil || d.UUID != r.PathValue("uuid") {
		http.Error(w, "device.json does not match the drive", 400)
		return
	}
	for name, check := range map[string]func(io.Reader) error{
		"files.jsonl":  func(f io.Reader) error { return recsFrom(f, "files.jsonl", func(Rec) {}) },
		"fullhash.tsv": func(f io.Reader) error { _, err := fullHashes(f); return err },
	} {
		f, err := os.Open(part(name))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		} else if err == nil {
			err = check(bufio.NewReader(f))
			f.Close()
		}
		if err != nil {
			http.Error(w, name+": "+err.Error(), 400)
			return
		}
	}
	select {
	case s.drives <- struct{}{}:
		defer func() { <-s.drives }()
	case <-r.Context().Done():
		return
	}
	read := func(name string) []byte { // a part not sent is empty, not null
		if b, _ := os.ReadFile(part(name)); b != nil {
			return b
		}
		return []byte{}
	}
	err = s.inTeam(r.Context(), u.Team, func(tx pgx.Tx) error {
		_, err := tx.Exec(r.Context(), `insert into drive (team, uuid, device, files, fullhash) values ($1, $2, $3, $4, $5)
			on conflict (team, uuid) do update set device = $3, files = $4, fullhash = $5, updated = now()`,
			u.Team, d.UUID, read("device.json"), read("files.jsonl"), read("fullhash.tsv"))
		return err
	})
	if err != nil {
		fail(w, err)
		return
	}
	s.changed(u.Team)
	send(w, map[string]bool{"ok": true})
}

// have lists the drives, the previews (all 5 frames) and the analyses the site has, so a Mac sends only the rest.
// A content removed on request is in every list: the Macs never send it again.
func (s *Server) have(w http.ResponseWriter, r *http.Request, u *User) {
	thumbs := []string{}
	es, _ := os.ReadDir(filepath.Join(s.dir, "teams", strconv.FormatInt(u.Team, 10), "thumbs"))
	for _, e := range es {
		if fileExists(filepath.Join(s.thumbDir(u.Team, e.Name()), "5.jpg")) {
			thumbs = append(thumbs, e.Name())
		}
	}
	drives, looks, speech, emb := []string{}, []string{}, []string{}, []string{}
	var proxies []string // nil without a store: the Macs then make none
	if s.store != nil {
		proxies = []string{}
	}
	err := s.inTeam(r.Context(), u.Team, func(tx pgx.Tx) error {
		for q, list := range map[string]*[]string{"select uuid from drive where team = $1": &drives,
			"select thumb from look where team = $1 union select thumb from forgotten where team = $1":   &looks,
			"select thumb from proxy where team = $1 union select thumb from forgotten where team = $1":  &proxies,
			"select thumb from speech where team = $1 union select thumb from forgotten where team = $1": &speech,
			"select thumb from frame where team = $1 union select thumb from forgotten where team = $1":  &emb,
			"select thumb from forgotten where team = $1":                                                &thumbs} {
			if *list == nil {
				continue
			}
			rows, _ := tx.Query(r.Context(), q, u.Team)
			for rows.Next() {
				var th string
				if err := rows.Scan(&th); err != nil {
					return err
				}
				*list = append(*list, th)
			}
			if rows.Err() != nil {
				return rows.Err()
			}
		}
		return nil
	})
	if err != nil {
		fail(w, err)
		return
	}
	send(w, map[string][]string{"drives": drives, "thumbs": thumbs, "looks": looks, "proxies": proxies, "speech": speech, "emb": emb})
}

func (s *Server) putThumb(w http.ResponseWriter, r *http.Request, u *User) {
	th, k := r.PathValue("thumb"), r.PathValue("k")
	if !thumbName.MatchString(th) || len(k) != 1 || k[0] < '1' || k[0] > '5' {
		http.Error(w, "bad preview name", 400)
		return
	}
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20))
	if err != nil || !bytes.HasPrefix(b, []byte{0xff, 0xd8, 0xff}) { // JPEG only
		http.Error(w, "not a JPEG preview", 400)
		return
	}
	err = s.syncing(r.Context(), u.Team, func(tx pgx.Tx) error {
		if gone, err := isForgotten(r.Context(), tx, u.Team, th); err != nil || gone {
			return err // removed on request: taken and dropped, so the Mac goes on with the rest
		}
		dir := s.thumbDir(u.Team, th)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		tmp := filepath.Join(dir, "."+k+".tmp")
		if err := os.WriteFile(tmp, b, 0o644); err != nil {
			return err
		}
		return os.Rename(tmp, filepath.Join(dir, k+".jpg"))
	})
	if err != nil {
		fail(w, err)
		return
	}
	send(w, map[string]bool{"ok": true})
}

// putLooks stores what Apple Vision saw on a Mac, one line per content (vision.jsonl).
func (s *Server) putLooks(w http.ResponseWriter, r *http.Request, u *User) {
	ok := s.syncRows(w, r, u, "bad analysis line", func(dec *json.Decoder, b *pgx.Batch) error {
		var v struct { // decoded into what the catalog reads, and kept as decoded: nothing it cannot read is stored
			Thumb  string  `json:"thumb"`
			Frames []Frame `json:"frames"`
		}
		if err := dec.Decode(&v); err != nil {
			return err
		}
		if !thumbName.MatchString(v.Thumb) {
			return errBadLine
		}
		frames, _ := json.Marshal(withoutPersonLabels(v.Frames))
		b.Queue(`insert into look (team, thumb, frames) select $1, $2, $3 where not exists (select 1 from forgotten where team = $1 and thumb = $2)
			on conflict (team, thumb) do update set frames = $3`, u.Team, v.Thumb, frames)
		return nil
	})
	if ok {
		s.changed(u.Team)
		send(w, map[string]bool{"ok": true})
	}
}

// filesOnly serves the files of fsys and nothing for a folder: no listing of what is there.
func filesOnly(fsys fs.FS) http.Handler {
	files := http.FileServerFS(fsys)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/") || r.URL.Path == "" {
			http.NotFound(w, r)
			return
		}
		files.ServeHTTP(w, r)
	})
}

// errBadLine: a line of a Mac's body the site cannot read; the whole body is refused (400) and nothing is kept.
var errBadLine = errors.New("bad line")

// syncBatch statements go to Postgres at a time: a Mac's body is read and written as it comes, so its size does not
// pile up in memory (a body of tiny lines once held millions of queued statements).
const syncBatch = 500

// syncRows reads a Mac's NDJSON body line by line inside one syncing transaction; line decodes one line and queues
// its statements, or says it is bad (then 400 with the message bad, and the transaction keeps nothing). It answers
// the error itself and reports whether the body was taken.
func (s *Server) syncRows(w http.ResponseWriter, r *http.Request, u *User, bad string, line func(*json.Decoder, *pgx.Batch) error) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<20))
	err := s.syncing(r.Context(), u.Team, func(tx pgx.Tx) error {
		b := &pgx.Batch{}
		flush := func() error {
			if b.Len() == 0 {
				return nil
			}
			err := tx.SendBatch(r.Context(), b).Close()
			b = &pgx.Batch{}
			return err
		}
		for {
			switch err := line(dec, b); {
			case err == io.EOF:
				return flush()
			case err != nil:
				return httpError{400, bad}
			case b.Len() >= syncBatch:
				if err := flush(); err != nil {
					return err
				}
			}
		}
	})
	if err != nil {
		fail(w, err)
	}
	return err == nil
}

// proxyKey is where a team's proxy of one content lives in the store.
func proxyKey(team int64, thumb string) string {
	return fmt.Sprintf("teams/%d/proxy/%s.mp4", team, thumb)
}

// proxyUpload gives a Mac a link to upload one proxy straight to the store, valid one hour.
func (s *Server) proxyUpload(w http.ResponseWriter, r *http.Request, u *User) {
	th := r.PathValue("thumb")
	if s.store == nil || !thumbName.MatchString(th) {
		http.Error(w, "this site keeps no videos", 404)
		return
	}
	var gone bool
	if err := s.inTeam(r.Context(), u.Team, func(tx pgx.Tx) (err error) { gone, err = isForgotten(r.Context(), tx, u.Team, th); return }); err != nil || gone {
		http.Error(w, "This clip's data was removed on request", 410) // a link given just before a removal: proxyDone deletes what it uploads
		return
	}
	send(w, map[string]string{"url": s.store.presign("PUT", proxyKey(u.Team, th), time.Hour, time.Now())})
}

// proxyDone records a proxy once the store has it, checked with a HEAD request: the light copy of a clip of the
// team's catalog, stored as video/mp4 (as the apps upload it). Anything else is deleted, never recorded: only
// recorded proxies get links to watch them, so nothing else an upload link was used for reaches a browser.
func (s *Server) proxyDone(w http.ResponseWriter, r *http.Request, u *User) {
	th := r.PathValue("thumb")
	if s.store == nil || !thumbName.MatchString(th) {
		http.Error(w, "this site keeps no videos", 404)
		return
	}
	res, err := s.store.do(r.Context(), "HEAD", proxyKey(u.Team, th), nil)
	if err != nil {
		http.Error(w, "the video is not in the store", 400)
		return
	}
	res.Body.Close()
	c, err := s.catalog(r.Context(), u.Team)
	if err != nil {
		fail(w, err)
		return
	}
	if !slices.ContainsFunc(c.list, func(cl *Clip) bool { return cl.Thumb == th }) || res.Header.Get("Content-Type") != "video/mp4" {
		s.deleteObjects(r.Context(), []string{proxyKey(u.Team, th)})
		http.Error(w, "Not the light copy of a clip of this team: deleted", 400)
		return
	}
	var gone bool
	err = s.syncing(r.Context(), u.Team, func(tx pgx.Tx) (err error) {
		if gone, err = isForgotten(r.Context(), tx, u.Team, th); err != nil || gone {
			return err
		}
		_, err = tx.Exec(r.Context(), `insert into proxy (team, thumb, size) values ($1, $2, $3)
			on conflict (team, thumb) do update set size = $3, created = now()`, u.Team, th, res.ContentLength)
		return err
	})
	if err != nil {
		fail(w, err)
		return
	}
	if gone { // uploaded with a link given before the removal
		s.deleteObjects(r.Context(), []string{proxyKey(u.Team, th)})
		http.Error(w, "This clip's data was removed on request", 410)
		return
	}
	s.changed(u.Team)
	send(w, map[string]bool{"ok": true})
}

// proxyPlay gives a member a link to watch a proxy from the store, valid one hour.
func (s *Server) proxyPlay(w http.ResponseWriter, r *http.Request, u *User) {
	th := r.PathValue("thumb")
	c, err := s.catalog(r.Context(), u.Team)
	if err != nil || s.store == nil || !c.proxies[th] {
		http.Error(w, "No video of this clip for the team yet", 404)
		return
	}
	send(w, map[string]string{"url": s.store.presign("GET", proxyKey(u.Team, th), time.Hour, time.Now())})
}

// putSpeech stores what is said in clips, transcribed on a Mac, one line per content (speech.jsonl).
func (s *Server) putSpeech(w http.ResponseWriter, r *http.Request, u *User) {
	ok := s.syncRows(w, r, u, "bad transcript line", func(dec *json.Decoder, b *pgx.Batch) error {
		var v struct { // as in putLooks
			Thumb string    `json:"thumb"`
			Segs  []Segment `json:"segs"`
		}
		if err := dec.Decode(&v); err != nil {
			return err
		}
		if !thumbName.MatchString(v.Thumb) || v.Segs == nil {
			return errBadLine
		}
		segs, _ := json.Marshal(v.Segs)
		b.Queue(`insert into speech (team, thumb, segs) select $1, $2, $3 where not exists (select 1 from forgotten where team = $1 and thumb = $2)
			on conflict (team, thumb) do update set segs = $3`, u.Team, v.Thumb, segs)
		return nil
	})
	if ok {
		s.changed(u.Team)
		send(w, map[string]bool{"ok": true})
	}
}
