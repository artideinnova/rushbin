#!/bin/sh
# Updates the site after new code is in /opt/rushbin (git archive, or git pull), then removes what the update leaves
# behind, about 12 GB after a few updates otherwise: the images of this site it replaced. Only those: they carry this
# Compose project's label and have no name any more. Nothing of other programs on the server is touched, not even
# their build cache. Run on the server:
#   sudo deploy/up.sh            (all services)      sudo deploy/up.sh rushbin      (the site only)
set -eu
cd "$(dirname "$0")"
docker compose up -d --build "$@"
# Caddy reads the Caddyfile it mounted when it started: a new one (unpacked as a new file) needs a restart.
if ! docker compose exec -T caddy cat /etc/caddy/Caddyfile | cmp -s - Caddyfile; then docker compose restart caddy; fi
docker image prune -f --filter "label=com.docker.compose.project=$(docker compose config | sed -n 's/^name: //p')" >/dev/null
df -h / | tail -1
