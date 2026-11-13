#!/bin/bash
# Weekly real restore of the newest database copy, run by rushbin-restore-test.timer: it downloads the copy from the bucket,
# restores it into a throwaway Postgres (the image of compose's db, no network), and compares the rows of every table with
# the live database. Writes at the same moment show up as a difference: the timer runs at night, after the copy.
# restore-test.sh FILE restores that dump instead of downloading one (to try it without a bucket).
# The outcome, also of a failure, goes to restore-test.json in rushbin's data folder: the site shows owners a failure.
set -euo pipefail
cd "$(dirname "$0")"
dump=$(mktemp) # readable by this user only, removed at the end
pg="rushbin-restore-test-$$" n=0 step="starting" reason=""
record() {
	local status=$? ok=true f
	rm -f "$dump"
	docker rm -f "$pg" > /dev/null 2>&1 || true
	[ "$status" -eq 0 ] || ok=false reason=${reason:-"failed while $step"}
	f=$(mktemp)
	printf '{"at":"%s","ok":%s,"tables":%d,"reason":"%s"}\n' "$(date -u +%FT%TZ)" "$ok" "$n" "$reason" | tee "$f"
	# rushbin's image has no shell to write it from inside: docker cp, as root, so the file must be readable by all.
	chmod 644 "$f"
	docker compose cp "$f" rushbin:/var/lib/rushbin/restore-test.json || echo "rushbin restore test: the outcome did not reach the site"
	rm -f "$f"
	exit "$status"
}
trap record EXIT

download() { # the newest copy in the bucket
	local bucket name
	bucket=$(sed -n 's/^R2_BUCKET=//p' backup.env)
	name=$(docker run --rm --env-file backup.env rclone/rclone:1.71 lsf --files-only --use-server-modtime --format tp --separator ';' \
		--include '*.dump' "r2:$bucket" | sort | tail -n 1 | cut -d';' -f2)
	[ -n "$name" ]
	echo "rushbin restore test: $name"
	docker run --rm --env-file backup.env rclone/rclone:1.71 cat "r2:$bucket/$name" > "$dump"
}

# Rows of every table, one "table count" line each, sorted the same way on both sides.
counts="select table_name || ' ' || (xpath('/row/c/text()', query_to_xml(format('select count(*) as c from %I', table_name),
  false, true, '')))[1]::text from information_schema.tables where table_schema = 'public' and table_type = 'BASE TABLE'"

if [ $# -gt 0 ]; then
	step="reading $1"
	cp "$1" "$dump"
else
	step="downloading the newest copy"
	download
fi
step="starting a throwaway Postgres"
image=$(sed -n 's/^ *image: \(pgvector\/[^ ]*\).*/\1/p' compose.yml)
docker run -d --rm --name "$pg" --network none -e POSTGRES_HOST_AUTH_METHOD=trust -e POSTGRES_DB=rushbin "$image" > /dev/null
# Over TCP: during its first start the image runs a temporary server on the socket only, then restarts it.
for _ in $(seq 60); do
	docker exec "$pg" pg_isready -q -h 127.0.0.1 && break
	sleep 1
done
docker exec "$pg" pg_isready -q -h 127.0.0.1
step="restoring the copy"
# The role rushbin creates when it starts: roles live outside a dump, the grants in it name this one.
docker exec "$pg" psql -U postgres -d rushbin -qc "create role rushbin_app nologin"
docker exec -i "$pg" pg_restore -U postgres -d rushbin --exit-on-error < "$dump"
step="counting the rows"
copy=$(docker exec "$pg" psql -U postgres -d rushbin -Atc "$counts" | LC_ALL=C sort)
live=$(docker compose exec -T db psql -U postgres -d rushbin -Atc "$counts" | LC_ALL=C sort)
n=$(grep -c . <<< "$copy")
# A table missing on one side has no rows there: one added since the copy, still empty, loses nothing.
diffs=$(LC_ALL=C join -a 1 -a 2 -e 0 -o 0,1.2,2.2 <(echo "$copy") <(echo "$live") |
	awk '$2 != $3 { printf "%s%s: %s in the copy, %s live", sep, $1, $2, $3; sep = "; " }')
echo "rushbin restore test: $n tables restored${diffs:+, rows differ: $diffs}"
[ -z "$diffs" ] || { reason="rows differ: $diffs"; exit 1; }
[ "$n" -gt 0 ] || { reason="the copy has no tables"; exit 1; }
