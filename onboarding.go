package main

import (
	"cmp"
	"context"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
)

// Getting into a team without typing: the site makes a pairing code for the person signed in, inside a link that
// opens the Mac app (rushbin://connect?site=...&code=...). The app asks before it connects, then trades the code
// for a token of its own, as a sign-in with email and password would give it. A code works once, for pairTTL,
// for that person in that team only; the site keeps only its hash.
const pairTTL = 10 * time.Minute

// pairCode: the link that connects the app on this person's computer to this team.
func (s *Server) pairCode(w http.ResponseWriter, r *http.Request, u *User) {
	tok := token()
	err := s.inTeam(r.Context(), u.Team, func(tx pgx.Tx) error {
		if _, err := tx.Exec(r.Context(), "delete from pairing where team = $1 and person = $2", u.Team, u.ID); err != nil { // the newest works
			return err
		}
		_, err := tx.Exec(r.Context(), "insert into pairing (hash, person, team) values ($1, $2, $3)", hash(tok), u.ID, u.Team)
		return err
	})
	if err != nil {
		fail(w, err)
		return
	}
	send(w, map[string]string{"url": "rushbin://connect?" + url.Values{"site": {siteURL(r)}, "code": {tok}}.Encode()})
}

// pair: the app trades a pairing code (token) for a computer's token, named after device.
func (s *Server) pair(w http.ResponseWriter, r *http.Request) {
	f, ok := readForm(w, r)
	if !ok {
		return
	}
	var person, team int64
	if s.admin.QueryRow(r.Context(), "delete from pairing where hash = $1 and created > now() - $2::interval returning person, team",
		hash(f.Token), seconds(pairTTL)).Scan(&person, &team) != nil {
		http.Error(w, "This link has expired or was already used: on the team site, press Connect this Mac again", 403)
		return
	}
	s.signIn(w, r, person, team, cmp.Or(strings.TrimSpace(f.Device), "Mac"))
}

// offer answers the link an owner made (an invite, a password link) and emails it when the site sends email.
// mailed tells the page whether the owner still has to send it.
func (s *Server) offer(w http.ResponseWriter, r *http.Request, to string, l Letter) {
	mailed := false
	if s.mailer != nil {
		if err := s.mail(r.Context(), to, l); err != nil {
			log.Println("mail:", err)
		} else {
			mailed = true
		}
	}
	send(w, map[string]any{"url": l.Link, "mailed": mailed})
}

// mailedLink: how long a password link sent by email works. An owner's link is handed over by a person and works
// linkDays; one anybody can have sent to an address should not wait in a mailbox for days.
const mailedLink = time.Hour

// forgot emails a link for a new password to whoever writes an address of an account. It answers the same whether
// the address has one or not, and as quickly (the email goes after the answer), so nobody learns who is on the site.
func (s *Server) forgot(w http.ResponseWriter, r *http.Request) {
	if s.mailer == nil {
		http.Error(w, "This site does not send email: ask an owner of your team for a password link", 404)
		return
	}
	f, ok := readForm(w, r)
	if !ok {
		return
	}
	key := "forgot:" + s.clientIP(r)
	if s.limited(5, key) {
		http.Error(w, "Too many requests from here: try again in 15 minutes", 429)
		return
	}
	s.failed(key)
	var person, team int64
	if s.admin.QueryRow(r.Context(), "select m.person, m.team from member m join person p on p.id = m.person where p.email = $1 order by m.created limit 1",
		f.Email).Scan(&person, &team) == nil {
		tok := token()
		err := pgx.BeginFunc(r.Context(), s.admin, func(tx pgx.Tx) error {
			if _, err := tx.Exec(r.Context(), "delete from reset where person = $1", person); err != nil {
				return err
			}
			_, err := tx.Exec(r.Context(), "insert into reset (hash, person, team, mailed) values ($1, $2, $3, true)", hash(tok), person, team)
			return err
		})
		if err != nil {
			fail(w, err)
			return
		}
		l := Letter{Subject: "A new password for rushbin",
			Text:   "Someone, hopefully you, asked for a new rushbin password for this address. If it was not you, ignore this email: nothing changes.",
			Button: "Choose a new password", Link: siteURL(r) + "/login#reset=" + tok,
			After: "The link works once, for 1 hour. Every device signed in with the old password will need to sign in again."}
		go func() {
			if err := s.mail(context.Background(), f.Email, l); err != nil {
				log.Println("mail:", err)
			}
		}()
	}
	send(w, map[string]bool{"ok": true})
}

// ownTeam creates a team owned by email. A new address gets an account with pass; an address that has one gets
// pass as its password, and its sessions end: only who proved the address is theirs (the sign-up email) or who
// runs the site (the setup link) gets here.
func ownTeam(ctx context.Context, tx pgx.Tx, teamName, email, name string, pass []byte, terms *time.Time) (team, person int64, err error) {
	if err = tx.QueryRow(ctx, "insert into team (name, terms) values ($1, $2) returning id", teamName, terms).Scan(&team); err != nil {
		return
	}
	if err = tx.QueryRow(ctx, "insert into person (email, name, pass) values ($1, $2, $3) on conflict (email) do update set pass = excluded.pass returning id",
		email, name, pass).Scan(&person); err != nil {
		return
	}
	if _, err = tx.Exec(ctx, "delete from session where person = $1", person); err != nil {
		return
	}
	_, err = tx.Exec(ctx, "insert into member (team, person, role) values ($1, $2, 'owner')", team, person)
	return
}

// newOwner checks the form that creates a team: its name, its owner's name, email and password.
func newOwner(f *form) error {
	f.TeamName = strings.TrimSpace(f.TeamName)
	switch f.Email = emailOf(f.Email); {
	case f.TeamName == "":
		return httpError{400, "Write the team's name"}
	case f.Name == "":
		return httpError{400, "Write your name"}
	case f.Email == "":
		return httpError{400, "Write a valid email"}
	}
	return goodPassword(f.Password)
}

// firstRun says in the log, without any secret, that the site waits for its first team.
func (s *Server) firstRun(ctx context.Context) {
	if s.setupOpen(ctx) {
		log.Print("No team on this site yet. Whoever runs the server gets the setup link with: docker compose exec rushbin /rushbin setup-link")
	}
}

// setupOpen: the site has no team yet.
func (s *Server) setupOpen(ctx context.Context) bool {
	var some bool
	return s.admin.QueryRow(ctx, "select exists (select 1 from team)").Scan(&some) == nil && !some
}

var errHasTeam = httpError{403, "This site has a team already"}

// setupLink makes a new setup link's token, replacing the last.
func setupLink(ctx context.Context, s *Server) (string, error) {
	tok := token()
	return tok, pgx.BeginFunc(ctx, s.admin, func(tx pgx.Tx) error {
		if !s.setupOpen(ctx) {
			return errHasTeam
		}
		if _, err := tx.Exec(ctx, "delete from setup_link"); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "insert into setup_link (hash) values ($1)", hash(tok))
		return err
	})
}

// setupTeam: the setup link's form, the first team and its owner.
func (s *Server) setupTeam(w http.ResponseWriter, r *http.Request) {
	f, ok := readForm(w, r)
	if !ok {
		return
	}
	if err := newOwner(&f); err != nil {
		fail(w, err)
		return
	}
	pass, _ := bcrypt.GenerateFromPassword([]byte(f.Password), 12)
	err := pgx.BeginFunc(r.Context(), s.admin, func(tx pgx.Tx) error {
		if _, err := tx.Exec(r.Context(), "lock table team in exclusive mode"); err != nil { // two tabs at once: one team
			return err
		}
		var some bool
		if err := tx.QueryRow(r.Context(), "select exists (select 1 from team)").Scan(&some); err != nil || some {
			return cmp.Or(err, error(errHasTeam))
		}
		if tag, err := tx.Exec(r.Context(), "delete from setup_link where hash = $1", hash(f.Token)); err != nil || tag.RowsAffected() != 1 {
			return cmp.Or(err, error(httpError{403, "This setup link is not valid: a newer one replaced it. On the server: docker compose exec rushbin /rushbin setup-link"}))
		}
		team, person, err := ownTeam(r.Context(), tx, f.TeamName, f.Email, f.Name, pass, nil)
		if err != nil {
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
