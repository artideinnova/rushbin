#!/bin/bash
# Nightly copy of the team database to an S3-compatible bucket (R2, S3, B2, MinIO), run by rushbin-backup.timer.
# deploy/backup.env (readable by this user only) holds the rclone remote "r2" and R2_BUCKET, the
# bucket (or bucket/folder) of the copies only. One new file per run, never overwritten (on R2 a bucket lock keeps each
# for 29 days); copies uploaded more than 30 days ago are deleted after a good copy, so none is older than 31 days.
# The outcome, also of a failure, goes to backup.json in rushbin's data folder: the site shows owners when it is bad.
set -euo pipefail
cd "$(dirname "$0")"
dump=$(mktemp) # readable by this user only, removed at the end
bytes=0 n=0 step="starting"
record() {
	local status=$? ok=true reason="" f
	rm -f "$dump"
	[ "$status" -eq 0 ] || ok=false reason="failed while $step"
	f=$(mktemp)
	printf '{"at":"%s","ok":%s,"bytes":%d,"tables":%d,"reason":"%s"}\n' "$(date -u +%FT%TZ)" "$ok" "$bytes" "$n" "$reason" | tee "$f"
	# rushbin's image has no shell to write it from inside: docker cp, as root, so the file must be readable by all.
	chmod 644 "$f"
	docker compose cp "$f" rushbin:/var/lib/rushbin/backup.json || echo "rushbin backup: the outcome did not reach the site"
	rm -f "$f"
	exit "$status"
}
trap record EXIT
step="reading backup.env"
bucket=$(sed -n 's/^R2_BUCKET=//p' backup.env)
[ -n "$bucket" ]
key="$(date -u +%Y-%m-%dT%H%M%SZ).dump"
step="dumping the database"
docker compose exec -T db pg_dump -U postgres -Fc rushbin > "$dump"
bytes=$(stat -c %s "$dump")
step="uploading the dump"
docker run --rm -i --env-file backup.env rclone/rclone:1.71 rcat "r2:$bucket/$key" < "$dump"
# Check the copy: download it again, compare it byte for byte, and list the tables in the dump. (pg_restore --list reads
# only the start: fed by the download through a pipe, it closed it early and failed the backup once the dump outgrew the pipe.)
step="downloading the copy again"
sum=$(sha256sum < "$dump")
got=$(docker run --rm --env-file backup.env rclone/rclone:1.71 cat "r2:$bucket/$key" | sha256sum)
step="comparing the copy in the bucket with the dump"
[ "$got" = "$sum" ]
step="listing the tables in the dump"
n=$(docker compose exec -T db pg_restore --list < "$dump" | grep -c "TABLE DATA")
echo "rushbin backup: $key, $bytes bytes, same in the bucket, $n tables with data"
step="checking the dump has tables with data"
[ "$n" -gt 0 ]
# The upload time, not the time rclone keeps in the object: a copy moved from another bucket counts from its move, as the lock.
step="deleting copies older than 30 days"
docker run --rm --env-file backup.env rclone/rclone:1.71 delete "r2:$bucket" --max-depth 1 --include '*.dump' --min-age 30d --use-server-modtime
