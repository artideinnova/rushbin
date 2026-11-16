package main

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Letter is one email: a line of text, one button with its link, and a line under it, in Lang (mailWords).
type Letter struct {
	Lang, Subject, Text, Button, Link, After string
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
	w := mailWords[l.Lang]
	if w == nil {
		w, l.Lang = mailWords["en"], "en"
	}
	return `<!doctype html><html lang="` + l.Lang + `"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
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
<p style="margin:24px 0 0;padding-top:20px;border-top:1px solid #2c2c2e;` + f(400, 12, 18, "#8e8e93") + `">` + e(w["copy"]) + `<br>
<a href="` + link + `" style="color:#ffa477;word-break:break-all">` + link + `</a></p>
</td></tr>
<tr><td style="padding:20px 4px 0;` + f(400, 12, 18, "#6e6e73") + `">` + e(strings.ReplaceAll(w["sent"], "{host}", host(l.Link))) + `</td></tr>
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

// mailLang: the language of an email, from the Accept-Language of the request that makes it, the best of those
// mailWords has (pt-* is pt-BR, other regions their language), else English. The site keeps no language per
// person: the reader's browser at sign-up and for a forgotten password, the admin's for an invite or a password
// link they make, as the closest guess of the language their colleague reads.
func mailLang(r *http.Request) string {
	if l := r.Header.Get("X-Rushbin-Lang"); mailWords[l] != nil { // the language chosen on the page, before the browser's
		return l
	}
	lang, best := "en", 0.0
	for _, part := range strings.Split(r.Header.Get("Accept-Language"), ",") {
		tag, params, _ := strings.Cut(part, ";")
		q := 1.0
		if v, ok := strings.CutPrefix(strings.TrimSpace(params), "q="); ok {
			if q, _ = strconv.ParseFloat(v, 64); q > 1 {
				q = 0 // not a valid weight
			}
		}
		l, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(tag)), "-")
		if l == "pt" {
			l = "pt-BR"
		}
		if mailWords[l] != nil && q > best { // the first of equal weights wins, as the browser listed them
			lang, best = l, q
		}
	}
	return lang
}

// letter: the email kind (signup, forgot, reset, invite) in lang, with its {placeholders} filled by the pairs of v
// (name, value) and {days} the days a link works (linkDays).
func letter(lang, kind, link string, v ...string) Letter {
	pairs := []string{"{days}", strconv.Itoa(linkDays)}
	for i := 0; i+1 < len(v); i += 2 {
		pairs = append(pairs, "{"+v[i]+"}", v[i+1])
	}
	fill, w := strings.NewReplacer(pairs...).Replace, mailWords[lang]
	return Letter{Lang: lang, Subject: fill(w[kind+".subject"]), Text: fill(w[kind+".text"]), Button: fill(w[kind+".button"]),
		Link: link, After: fill(w[kind+".after"])}
}

// mailWords: the site's emails in each language of the pages (web/i18n.js), with the same {placeholders} as English
// (mail_test.go). The register of the app: tu in it, de, es and pt-BR, vous in fr, です・ます in ja.
var mailWords = map[string]map[string]string{
	"en": {
		"signup.subject": "Confirm your email to create {team}",
		"signup.text":    "To create the team {team} on rushbin, confirm that this address is yours.",
		"signup.button":  "Confirm and create the team",
		"signup.after":   "The link works once, for {days} days. If you did not ask for it, ignore this email: no team is created.",
		"forgot.subject": "A new password for rushbin",
		"forgot.text":    "Someone, hopefully you, asked for a new rushbin password for this address. If it was not you, ignore this email: nothing changes.",
		"forgot.button":  "Choose a new password",
		"forgot.after":   "The link works once, for 1 hour. Every device signed in with the old password will need to sign in again.",
		"reset.subject":  "A new password for rushbin",
		"reset.text":     "{name}, an admin of {team}, made you a link to choose a new rushbin password.",
		"reset.button":   "Choose a new password",
		"reset.after":    "The link works once, for {days} days. Every device signed in with the old password will need to sign in again.",
		"invite.subject": "{name} invited you to {team} on rushbin",
		"invite.text":    "{name} invited you to {team} on rushbin as {role}. On the team site you watch and search the footage.",
		"invite.button":  "Join {team}",
		"invite.after":   "The link works once, for {days} days. If you did not expect it, ignore this email.",
		"role.owner":     "an admin",
		"role.editor":    "a member",
		"role.viewer":    "a guest",
		"copy":           "If the button does not open, copy this link into your browser:",
		"sent":           "Sent by the rushbin site {host}.",
	},
	"it": {
		"signup.subject": "Conferma la tua email per creare {team}",
		"signup.text":    "Per creare il team {team} su rushbin, conferma che questo indirizzo è tuo.",
		"signup.button":  "Conferma e crea il team",
		"signup.after":   "Il link funziona una volta, per {days} giorni. Se non l'hai chiesto tu, ignora questa email: nessun team viene creato.",
		"forgot.subject": "Una nuova password per rushbin",
		"forgot.text":    "Qualcuno, speriamo tu, ha chiesto una nuova password di rushbin per questo indirizzo. Se non l'hai chiesta tu, ignora questa email: non cambia niente.",
		"forgot.button":  "Scegli una nuova password",
		"forgot.after":   "Il link funziona una volta, per 1 ora. Ogni dispositivo collegato con la vecchia password dovrà accedere di nuovo.",
		"reset.subject":  "Una nuova password per rushbin",
		"reset.text":     "{name}, amministratore di {team}, ti ha preparato un link per scegliere una nuova password di rushbin.",
		"reset.button":   "Scegli una nuova password",
		"reset.after":    "Il link funziona una volta, per {days} giorni. Ogni dispositivo collegato con la vecchia password dovrà accedere di nuovo.",
		"invite.subject": "{name} ti invita in {team} su rushbin",
		"invite.text":    "{name} ti invita a entrare in {team} su rushbin come {role}. Sul sito del team guardi e cerchi il girato.",
		"invite.button":  "Entra in {team}",
		"invite.after":   "Il link funziona una volta, per {days} giorni. Se non te lo aspettavi, ignora questa email.",
		"role.owner":     "amministratore",
		"role.editor":    "collaboratore",
		"role.viewer":    "ospite",
		"copy":           "Se il pulsante non si apre, copia questo link nel browser:",
		"sent":           "Inviata dal sito rushbin {host}.",
	},
	"es": {
		"signup.subject": "Confirma tu correo electrónico para crear {team}",
		"signup.text":    "Para crear el equipo {team} en rushbin, confirma que esta dirección es tuya.",
		"signup.button":  "Confirmar y crear el equipo",
		"signup.after":   "El enlace funciona una vez, durante {days} días. Si no lo pediste, ignora este correo: no se crea ningún equipo.",
		"forgot.subject": "Una contraseña nueva para rushbin",
		"forgot.text":    "Alguien, esperamos que tú, pidió una contraseña nueva de rushbin para esta dirección. Si no fuiste tú, ignora este correo: no cambia nada.",
		"forgot.button":  "Elegir una contraseña nueva",
		"forgot.after":   "El enlace funciona una vez, durante 1 hora. Cada dispositivo conectado con la contraseña anterior tendrá que iniciar sesión de nuevo.",
		"reset.subject":  "Una contraseña nueva para rushbin",
		"reset.text":     "{name}, administrador de {team}, te preparó un enlace para elegir una contraseña nueva de rushbin.",
		"reset.button":   "Elegir una contraseña nueva",
		"reset.after":    "El enlace funciona una vez, durante {days} días. Cada dispositivo conectado con la contraseña anterior tendrá que iniciar sesión de nuevo.",
		"invite.subject": "{name} te invita a {team} en rushbin",
		"invite.text":    "{name} te invita a unirte a {team} en rushbin como {role}. En el sitio del equipo ves y buscas el material.",
		"invite.button":  "Unirme a {team}",
		"invite.after":   "El enlace funciona una vez, durante {days} días. Si no lo esperabas, ignora este correo.",
		"role.owner":     "administrador",
		"role.editor":    "colaborador",
		"role.viewer":    "invitado",
		"copy":           "Si el botón no se abre, copia este enlace en tu navegador:",
		"sent":           "Enviado por el sitio de rushbin {host}.",
	},
	"fr": {
		"signup.subject": "Confirmez votre e-mail pour créer {team}",
		"signup.text":    "Pour créer l’équipe {team} sur rushbin, confirmez que cette adresse est bien la vôtre.",
		"signup.button":  "Confirmer et créer l’équipe",
		"signup.after":   "Le lien fonctionne une fois, pendant {days} jours. Si vous ne l’avez pas demandé, ignorez cet e-mail : aucune équipe n’est créée.",
		"forgot.subject": "Un nouveau mot de passe pour rushbin",
		"forgot.text":    "Quelqu’un, vous sans doute, a demandé un nouveau mot de passe rushbin pour cette adresse. Si ce n’est pas vous, ignorez cet e-mail : rien ne change.",
		"forgot.button":  "Choisir un nouveau mot de passe",
		"forgot.after":   "Le lien fonctionne une fois, pendant 1 heure. Chaque appareil connecté avec l’ancien mot de passe devra se reconnecter.",
		"reset.subject":  "Un nouveau mot de passe pour rushbin",
		"reset.text":     "{name}, administrateur de {team}, vous a préparé un lien pour choisir un nouveau mot de passe rushbin.",
		"reset.button":   "Choisir un nouveau mot de passe",
		"reset.after":    "Le lien fonctionne une fois, pendant {days} jours. Chaque appareil connecté avec l’ancien mot de passe devra se reconnecter.",
		"invite.subject": "{name} vous invite dans {team} sur rushbin",
		"invite.text":    "{name} vous invite à rejoindre {team} sur rushbin en tant que {role}. Sur le site de l’équipe, vous regardez et recherchez les rushes.",
		"invite.button":  "Rejoindre {team}",
		"invite.after":   "Le lien fonctionne une fois, pendant {days} jours. Si vous ne l’attendiez pas, ignorez cet e-mail.",
		"role.owner":     "administrateur",
		"role.editor":    "collaborateur",
		"role.viewer":    "invité",
		"copy":           "Si le bouton ne s’ouvre pas, copiez ce lien dans votre navigateur :",
		"sent":           "Envoyé par le site rushbin {host}.",
	},
	"de": {
		"signup.subject": "Bestätige deine E-Mail, um {team} zu erstellen",
		"signup.text":    "Um das Team {team} auf rushbin zu erstellen, bestätige, dass diese Adresse dir gehört.",
		"signup.button":  "Bestätigen und Team erstellen",
		"signup.after":   "Der Link funktioniert einmal, {days} Tage lang. Wenn du ihn nicht angefordert hast, ignoriere diese E-Mail: Es wird kein Team erstellt.",
		"forgot.subject": "Ein neues Passwort für rushbin",
		"forgot.text":    "Jemand, hoffentlich du, hat für diese Adresse ein neues rushbin-Passwort angefordert. Wenn du es nicht warst, ignoriere diese E-Mail: Es ändert sich nichts.",
		"forgot.button":  "Neues Passwort wählen",
		"forgot.after":   "Der Link funktioniert einmal, 1 Stunde lang. Jedes Gerät, das mit dem alten Passwort angemeldet ist, muss sich neu anmelden.",
		"reset.subject":  "Ein neues Passwort für rushbin",
		"reset.text":     "{name}, Admin von {team}, hat dir einen Link erstellt, mit dem du ein neues rushbin-Passwort wählst.",
		"reset.button":   "Neues Passwort wählen",
		"reset.after":    "Der Link funktioniert einmal, {days} Tage lang. Jedes Gerät, das mit dem alten Passwort angemeldet ist, muss sich neu anmelden.",
		"invite.subject": "{name} lädt dich zu {team} auf rushbin ein",
		"invite.text":    "{name} lädt dich als {role} zu {team} auf rushbin ein. Auf der Team-Website siehst und durchsuchst du das Footage.",
		"invite.button":  "{team} beitreten",
		"invite.after":   "Der Link funktioniert einmal, {days} Tage lang. Wenn du ihn nicht erwartet hast, ignoriere diese E-Mail.",
		"role.owner":     "Admin",
		"role.editor":    "Mitglied",
		"role.viewer":    "Gast",
		"copy":           "Wenn sich der Button nicht öffnet, kopiere diesen Link in deinen Browser:",
		"sent":           "Gesendet von der rushbin-Website {host}.",
	},
	"pt-BR": {
		"signup.subject": "Confirme o seu e-mail para criar {team}",
		"signup.text":    "Para criar a equipe {team} no rushbin, confirme que este endereço é seu.",
		"signup.button":  "Confirmar e criar a equipe",
		"signup.after":   "O link funciona uma vez, por {days} dias. Se você não pediu, ignore este e-mail: nenhuma equipe é criada.",
		"forgot.subject": "Uma nova senha para o rushbin",
		"forgot.text":    "Alguém, provavelmente você, pediu uma nova senha do rushbin para este endereço. Se não foi você, ignore este e-mail: nada muda.",
		"forgot.button":  "Escolher uma nova senha",
		"forgot.after":   "O link funciona uma vez, por 1 hora. Todo dispositivo conectado com a senha antiga vai precisar entrar de novo.",
		"reset.subject":  "Uma nova senha para o rushbin",
		"reset.text":     "{name}, administrador de {team}, criou para você um link para escolher uma nova senha do rushbin.",
		"reset.button":   "Escolher uma nova senha",
		"reset.after":    "O link funciona uma vez, por {days} dias. Todo dispositivo conectado com a senha antiga vai precisar entrar de novo.",
		"invite.subject": "{name} convidou você para {team} no rushbin",
		"invite.text":    "{name} convidou você para entrar em {team} no rushbin como {role}. No site da equipe você assiste e busca o material bruto.",
		"invite.button":  "Entrar em {team}",
		"invite.after":   "O link funciona uma vez, por {days} dias. Se você não esperava, ignore este e-mail.",
		"role.owner":     "administrador",
		"role.editor":    "colaborador",
		"role.viewer":    "convidado",
		"copy":           "Se o botão não abrir, copie este link no seu navegador:",
		"sent":           "Enviado pelo site do rushbin {host}.",
	},
	"ja": {
		"signup.subject": "{team}を作成するためにメールアドレスを確認してください",
		"signup.text":    "rushbinでチーム「{team}」を作成するには、このアドレスがご自身のものであることを確認してください。",
		"signup.button":  "確認してチームを作成",
		"signup.after":   "リンクは{days}日間、1回だけ有効です。心当たりがない場合は、このメールを無視してください。チームは作成されません。",
		"forgot.subject": "rushbinの新しいパスワード",
		"forgot.text":    "このアドレスでrushbinの新しいパスワードがリクエストされました。心当たりがない場合は、このメールを無視してください。何も変更されません。",
		"forgot.button":  "新しいパスワードを選ぶ",
		"forgot.after":   "リンクは1時間、1回だけ有効です。古いパスワードでサインインしているデバイスは、すべてもう一度サインインが必要です。",
		"reset.subject":  "rushbinの新しいパスワード",
		"reset.text":     "{team}の管理者の{name}さんが、rushbinの新しいパスワードを選ぶためのリンクを作成しました。",
		"reset.button":   "新しいパスワードを選ぶ",
		"reset.after":    "リンクは{days}日間、1回だけ有効です。古いパスワードでサインインしているデバイスは、すべてもう一度サインインが必要です。",
		"invite.subject": "{name}さんからrushbinの{team}に招待されました",
		"invite.text":    "{name}さんから、rushbinの{team}に{role}として招待されました。チームのサイトで素材を見たり検索したりできます。",
		"invite.button":  "{team}に参加",
		"invite.after":   "リンクは{days}日間、1回だけ有効です。心当たりがない場合は、このメールを無視してください。",
		"role.owner":     "管理者",
		"role.editor":    "メンバー",
		"role.viewer":    "ゲスト",
		"copy":           "ボタンが開かない場合は、このリンクをブラウザにコピーしてください：",
		"sent":           "rushbinのサイト{host}から送信されました。",
	},
}
