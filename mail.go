package main

import (
	"bytes"
	"cmp"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"html"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"net/url"
	"os"
	"strings"
	"time"
)

// Mailer sends the site's emails (invites and password links) through an SMTP server of your choice: your own
// mail server or a provider's. RUSHBIN_SMTP_HOST, RUSHBIN_SMTP_PORT (587 with STARTTLS when not set, 465 for TLS
// from the start), RUSHBIN_SMTP_USER and RUSHBIN_SMTP_PASSWORD when the server asks for them, and RUSHBIN_MAIL_FROM.
// The connection is always encrypted, except to a server on this machine. Nil without a host: owners then send
// the links themselves.
type Mailer struct {
	Host, Port, User, Password, From string
}

func mailerFromEnv() (*Mailer, error) {
	m := &Mailer{Host: os.Getenv("RUSHBIN_SMTP_HOST"), Port: cmp.Or(os.Getenv("RUSHBIN_SMTP_PORT"), "587"),
		User: os.Getenv("RUSHBIN_SMTP_USER"), Password: os.Getenv("RUSHBIN_SMTP_PASSWORD"), From: os.Getenv("RUSHBIN_MAIL_FROM")}
	if m.Host == "" {
		return nil, nil
	}
	if _, err := mail.ParseAddress(m.From); err != nil {
		return nil, fmt.Errorf("RUSHBIN_MAIL_FROM, like rushbin <no-reply@example.com>: %w", err)
	}
	return m, nil
}

// Letter is one email: a line of text, one button with its link, and a line under it.
type Letter struct {
	Subject, Text, Button, Link, After string
}

func (m *Mailer) send(ctx context.Context, to string, l Letter) error {
	from, err := mail.ParseAddress(m.From)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	addr, tlsConf := net.JoinHostPort(m.Host, m.Port), &tls.Config{ServerName: m.Host}
	var conn net.Conn
	if m.Port == "465" {
		conn, err = (&tls.Dialer{Config: tlsConf}).DialContext(ctx, "tcp", addr)
	} else {
		conn, err = (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("SMTP: %w", err)
	}
	defer conn.Close()
	if d, ok := ctx.Deadline(); ok {
		conn.SetDeadline(d)
	}
	c, err := smtp.NewClient(conn, m.Host)
	if err != nil {
		return fmt.Errorf("SMTP: %w", err)
	}
	defer c.Close()
	if m.Port != "465" {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(tlsConf); err != nil {
				return fmt.Errorf("SMTP: %w", err)
			}
		} else if !isLocal(m.Host) { // the links would travel in clear
			return errors.New("SMTP: the server offers no STARTTLS")
		}
	}
	if m.User != "" {
		if err := c.Auth(smtp.PlainAuth("", m.User, m.Password, m.Host)); err != nil {
			return fmt.Errorf("SMTP: %w", err)
		}
	}
	if err := c.Mail(from.Address); err != nil {
		return fmt.Errorf("SMTP: %w", err)
	}
	if err := c.Rcpt(to); err != nil {
		return fmt.Errorf("SMTP: %w", err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("SMTP: %w", err)
	}
	if _, err := w.Write(l.message(from, to)); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("SMTP: %w", err)
	}
	return c.Quit()
}

// message: the email as sent, its text and its HTML as alternatives, both quoted-printable.
func (l Letter) message(from *mail.Address, to string) []byte {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for _, p := range [][2]string{{"text/plain", l.plain()}, {"text/html", l.html()}} {
		w, _ := mw.CreatePart(textproto.MIMEHeader{"Content-Type": {p[0] + "; charset=utf-8"}, "Content-Transfer-Encoding": {"quoted-printable"}})
		q := quotedprintable.NewWriter(w)
		q.Write([]byte(p[1]))
		q.Close()
	}
	mw.Close()
	var b bytes.Buffer
	fmt.Fprintf(&b, "From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMessage-ID: <%s@%s>\r\nMIME-Version: 1.0\r\n",
		from.String(), to, mime.QEncoding.Encode("utf-8", l.Subject), time.Now().Format(time.RFC1123Z), token()[:32], from.Address[strings.LastIndex(from.Address, "@")+1:])
	fmt.Fprintf(&b, "Content-Type: multipart/alternative; boundary=%q\r\n\r\n", mw.Boundary())
	b.Write(body.Bytes())
	return b.Bytes()
}

func (l Letter) plain() string {
	return l.Subject + "\n\n" + l.Text + "\n\n" + l.Button + ":\n" + l.Link + "\n\n" + l.After + "\n\n-- \nrushbin, " + host(l.Link) + "\n"
}

func host(link string) string {
	u, err := url.Parse(link)
	if err != nil {
		return ""
	}
	return u.Host
}

// html: the same words, dark like the app, in tables with inline styles as email clients need. No images and
// nothing that reports opening: the mark is text, rush[bin] with orange brackets as in the logo.
func (l Letter) html() string {
	e := html.EscapeString
	f := func(weight, size, height int, color string) string {
		return fmt.Sprintf(`font-family:-apple-system,BlinkMacSystemFont,'Helvetica Neue',Helvetica,Arial,sans-serif;font-weight:%d;font-size:%dpx;line-height:%dpx;color:%s;`, weight, size, height, color)
	}
	link := e(l.Link)
	return `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="color-scheme" content="dark"><meta name="supported-color-schemes" content="dark"><title>` + e(l.Subject) + `</title></head>
<body style="margin:0;padding:0;background:#121214" bgcolor="#121214">
<div style="display:none;max-height:0;overflow:hidden;opacity:0">` + e(l.Text) + `</div>
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" bgcolor="#121214" style="background:#121214"><tr><td align="center" style="padding:40px 16px">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="max-width:520px">
<tr><td style="padding:0 4px 20px;` + f(700, 24, 28, "#f5f5f7") + `letter-spacing:-0.5px">rush<span style="color:#ff681d">[</span>bin<span style="color:#ff681d">]</span></td></tr>
<tr><td bgcolor="#1c1c1e" style="background:#1c1c1e;border:1px solid #2c2c2e;border-radius:20px;padding:32px 28px">
<h1 style="margin:0 0 12px;` + f(600, 22, 28, "#f5f5f7") + `letter-spacing:-0.3px">` + e(l.Subject) + `</h1>
<p style="margin:0 0 28px;` + f(400, 16, 25, "#c7c7cc") + `">` + e(l.Text) + `</p>
<table role="presentation" cellpadding="0" cellspacing="0" border="0"><tr><td bgcolor="#ff681d" style="background:#ff681d;border-radius:12px">
<a href="` + link + `" style="display:inline-block;padding:14px 26px;border-radius:12px;text-decoration:none;` + f(600, 16, 20, "#20120a") + `">` + e(l.Button) + `</a></td></tr></table>
<p style="margin:28px 0 0;` + f(400, 14, 21, "#98989d") + `">` + e(l.After) + `</p>
<p style="margin:24px 0 0;padding-top:20px;border-top:1px solid #2c2c2e;` + f(400, 12, 18, "#8e8e93") + `">If the button does not open, copy this link into your browser:<br>
<a href="` + link + `" style="color:#ffa477;word-break:break-all">` + link + `</a></p>
</td></tr>
<tr><td style="padding:20px 4px 0;` + f(400, 12, 18, "#6e6e73") + `">Sent by the rushbin site ` + e(host(l.Link)) + `.</td></tr>
</table></td></tr></table></body></html>`
}

// mail sends one letter, at most 5 per address and 100 from the site in 15 minutes (counted like failed sign-ins,
// in memory only). Without a mailer it fails: callers check s.mailer first when they have a way without email.
func (s *Server) mail(ctx context.Context, to string, l Letter) error {
	if s.mailer == nil {
		return httpError{503, "This site does not send email"}
	}
	key := "mail:" + strings.ToLower(to)
	if s.limited(5, key) || s.limited(100, "mail") {
		return httpError{429, "Too many emails from this site just now: try again in 15 minutes"}
	}
	s.failed(key, "mail")
	return s.mailer.send(ctx, to, l)
}
