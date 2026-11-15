package main

import (
	"context"
	"fmt"
	"html"
	"net/url"
	"strings"
)

// Letter is one email: a line of text, one button with its link, and a line under it.
type Letter struct {
	Subject, Text, Button, Link, After string
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
