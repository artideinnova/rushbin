package main

import (
	"bytes"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/mail"
	"net/textproto"
	"strings"
	"testing"
)

// fakeMailer: a fake SMTP server on this machine, which takes each email as the site sends it and opens it, text
// and HTML.
func fakeMailer(t *testing.T, mails chan<- sent) *Mailer {
	smtpd, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	t.Cleanup(func() { smtpd.Close() })
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
	return &Mailer{Host: "127.0.0.1", Port: port, From: "rushbin <no-reply@example.com>"}
}

// TestMailerFromEnv: no email without a host; a sender that is not an address stops the site.
func TestMailerFromEnv(t *testing.T) {
	t.Setenv("RUSHBIN_SMTP_HOST", "")
	if m, err := mailerFromEnv(); m != nil || err != nil {
		t.Fatalf("without a host: %v %v", m, err)
	}
	t.Setenv("RUSHBIN_SMTP_HOST", "smtp.example.com")
	t.Setenv("RUSHBIN_MAIL_FROM", "rushbin")
	if _, err := mailerFromEnv(); err == nil || !strings.Contains(err.Error(), "RUSHBIN_MAIL_FROM") {
		t.Fatalf("a sender that is not an address: %v, want an error", err)
	}
}
