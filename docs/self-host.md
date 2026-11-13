# Run your own rushbin site

Your team's site on your own server: the archive of every drive your Macs catalog, search, decisions, members and
roles, from any browser. The rushbin apps for Mac and Windows connect to it. Four containers on one server: rushbin,
Postgres, the search model and Caddy for HTTPS.

**Install it** (steps 1 to 6), then add what you need: [video from anywhere](#video-from-anywhere),
[email](#email), [nightly backups](#nightly-backups), [Tidy up with AI](#tidy-up-with-ai),
[app updates](#app-updates). Keep it up to date with [Update](#update).

## What you need

- A Linux server with Docker and its Compose plugin: 2 vCPU, 4 GB of RAM and 40 GB of disk are enough for a team.
- A name for the site under a domain you control, like `rushbin.example.com`, and ports 80 and 443 of the server
  open to the internet (Caddy gets the HTTPS certificate by itself).

## Install

### 1. Take the code

```sh
sudo git clone https://github.com/artideinnova/rushbin /opt/rushbin
cd /opt/rushbin/deploy
```

### 2. Point the name at the server

At whoever runs your domain's DNS, add an `A` record for `rushbin.example.com` with the server's address (and an
`AAAA` record if it has IPv6). Check it from the server:

```sh
dig +short rushbin.example.com
```

It prints the server's address. Wait for this before step 4: Caddy needs it to get the certificate.

### 3. Write the settings

One command writes `.env`, readable by root only, with the site's name and two passwords made on the spot (you
never type them):

```sh
sudo sh -c 'umask 077; printf "RUSHBIN_DOMAIN=rushbin.example.com\nPOSTGRES_PASSWORD=%s\nRUSHBIN_APP_PASSWORD=%s\n" "$(openssl rand -hex 24)" "$(openssl rand -hex 32)" > .env'
```

### 4. Start

```sh
sudo ./up.sh
```

The first time takes a few minutes: it builds rushbin and downloads the search model. Then open
`https://rushbin.example.com`: it shows the sign-in page, which says the site has no team yet.

### 5. Create your team

```sh
sudo docker compose exec rushbin /rushbin setup-link
```

It prints a link on your terminal only (it is a secret, never written to a log). Open it, write the team's name,
your name, email and a password: you are the team's owner. The link works once, and only while the site has no team.

### 6. Connect the Macs

Install rushbin on each Mac. On the site, Team, **Connect this Mac** opens the app, which asks to confirm; one click
and it is connected. Invite the others from Team as owner, editor or viewer: each invite is a link that works once.
On Windows, the app's Team tab takes the site's address, your email and your password.

From then on each computer sends the site the drives it catalogs, their previews and what was found in them.

## Video from anywhere

Optional. With an S3-compatible bucket (Cloudflare R2, AWS S3, Backblaze B2, MinIO), each Mac makes a light copy of
every clip (H.264, 1280 px, about 1.4 GB per hour of footage) and uploads it straight to the bucket: members watch
any clip in the browser, with the drive unplugged. Videos never pass through the server. Without a bucket, members
watch only clips whose drive is connected to their own computer.

Write `deploy/storage.env` with an editor (readable by root only, never in a repository):

```
RUSHBIN_S3_ENDPOINT=https://<account>.r2.cloudflarestorage.com
RUSHBIN_S3_REGION=auto
RUSHBIN_S3_BUCKET=my-bucket
RUSHBIN_S3_PREFIX=rushbin/
RUSHBIN_S3_KEY_ID=...
RUSHBIN_S3_SECRET=...
```

For AWS S3 the region is the bucket's (`eu-central-1`, ...) and the endpoint `https://s3.<region>.amazonaws.com`.
Then `sudo ./up.sh rushbin`.

## Email

Optional. Without email, invites and password links are links the owners copy and send themselves. With it, the
site emails them, and members who forgot their password ask for a link on the sign-in page.

rushbin sends through any SMTP server: your organization's mail server, or a provider's (Amazon SES, Postmark,
Mailgun, Google Workspace, Microsoft 365...). Write `deploy/mail.env` with an editor (readable by root only):

```
RUSHBIN_SMTP_HOST=smtp.example.com
RUSHBIN_SMTP_PORT=587
RUSHBIN_SMTP_USER=...
RUSHBIN_SMTP_PASSWORD=...
RUSHBIN_MAIL_FROM=rushbin <no-reply@example.com>
```

Port 587 uses STARTTLS, 465 TLS from the start; the connection is always encrypted, except to a server on this
machine. Leave out the user and the password if your server does not ask for them. Then `sudo ./up.sh rushbin`.
At most 5 emails per address and 100 in all every 15 minutes.

## Nightly backups

Optional, and recommended: a copy of the database every night at 02:30 UTC into a bucket of your choice, checked
after the upload, and kept 30 days. Every Sunday a second job restores the newest copy into a throwaway database and
compares it with the live one. The owners see a red notice on the site when a copy fails or is older than 36 hours.

Write `deploy/backup.env` with an editor (readable by root only), with a key that can read and write that bucket only:

```
RCLONE_CONFIG_R2_TYPE=s3
RCLONE_CONFIG_R2_PROVIDER=Cloudflare
RCLONE_CONFIG_R2_ACCESS_KEY_ID=...
RCLONE_CONFIG_R2_SECRET_ACCESS_KEY=...
RCLONE_CONFIG_R2_ENDPOINT=https://<account>.r2.cloudflarestorage.com
RCLONE_CONFIG_R2_NO_CHECK_BUCKET=true
R2_BUCKET=my-backups
```

(For AWS S3, `RCLONE_CONFIG_R2_PROVIDER=AWS` and its endpoint; the remote keeps the name `r2`.) Then turn both on:

```sh
sudo cp systemd/rushbin-backup.* systemd/rushbin-restore-test.* /etc/systemd/system/
sudo systemctl enable --now rushbin-backup.timer rushbin-restore-test.timer
sudo systemctl start rushbin-backup.service && journalctl -u rushbin-backup.service -n 3
```

To restore a copy: `sudo docker compose exec -T db pg_restore -U postgres -d rushbin --clean < FILE.dump`.

## Tidy up with AI

Optional. Tidy up with AI proposes a clearer folder structure for a drive, from its folder and file names (never
the clips' contents); the app makes only the moves a member ticks. By default each computer uses its member's own AI
key, chosen in the app. To have the site's AI used by everyone instead, write `deploy/anthropic.env` (readable by root
only) with `RUSHBIN_ANTHROPIC_KEY=...`, or for another provider:

```
RUSHBIN_AI_PROVIDER=openai          # anthropic (the default), openai, gemini or compatible
RUSHBIN_AI_KEY=...                  # not needed by an OpenAI-compatible server without keys
RUSHBIN_AI_MODEL=...                # optional
RUSHBIN_AI_URL=...                  # compatible only, like http://ollama:11434/v1
```

Then `sudo ./up.sh rushbin`. Each team gets at most 20 tidy-ups a month; `RUSHBIN_TIDY_CAP=50` in `.env` changes it.
The provider receives your teams' folder and file names: say so to your members.

## App updates

The apps of your teams ask your site whether there is a new version, and the Mac app updates itself from it,
checking the signature of every file. By default your site passes on the versions published at rushbin.io: the apps
then download them from there, and rushbin.io sees the address of the computer that downloads, nothing else.

- To serve them yourself, copy each release's `latest.json` and installer from rushbin.io into `releases/` of your
  video bucket: the apps then never reach rushbin.io.
- To turn updates off, add `RUSHBIN_UPDATES_FROM=off` to `.env`: the apps stay on their version until you install
  a new one.

## Update

```sh
sudo systemctl start rushbin-backup.service      # a copy of the database first, if backups are on
cd /opt/rushbin && sudo git pull && sudo deploy/up.sh
```

`up.sh` rebuilds and restarts the site, then removes the images of this site the update replaced: nothing else on
the server is touched. rushbin updates its database tables by itself when it starts. Point an uptime checker at
`https://rushbin.example.com/health`: it answers 503 when the database is unreachable, a backup failed or the disk is
almost full.
