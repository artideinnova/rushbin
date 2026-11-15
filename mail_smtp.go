package main

import (
	"bytes"
	"cmp"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
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
