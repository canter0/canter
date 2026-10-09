#!/bin/sh
set -eu
umask 077

stamp=$(date -u +%Y%m%dT%H%M%SZ)
generated=false
if [ "$#" -eq 0 ]; then
  archive=$(mktemp "/var/lib/canter/postgres-${stamp}.XXXXXX.dump")
  key="${stamp}.dump"
  generated=true
elif [ "$#" -eq 2 ]; then
  archive=$1
  key=$2
else
  echo "Usage: postgres-backup.sh [archive key]" >&2
  exit 1
fi
verified=
cleanup() {
  [ -z "$verified" ] || rm -f "$verified"
  if [ "$generated" = true ]; then rm -f "$archive"; fi
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM

if [ "$generated" = true ]; then
  pg_dump --format=custom --compress=9 --no-owner --no-privileges --file="$archive" "$CANTER_DATABASE_URL"
fi
pg_restore --list "$archive" >/dev/null
digest=$(sha256sum "$archive" | cut -d ' ' -f 1)

export AWS_ACCESS_KEY_ID="${CANTER_M1_ACCESS_KEY:?}"
export AWS_SECRET_ACCESS_KEY="${CANTER_M1_SECRET_KEY:?}"
export AWS_DEFAULT_REGION="${CANTER_M1_REGION:?}"
export AWS_PAGER=""
destination="s3://${CANTER_M1_BUCKET:?}/ops/control-plane/postgres/${key}"
aws --endpoint-url "$CANTER_M1_ENDPOINT" s3 cp \
  "$archive" "$destination" --metadata "sha256=${digest}" --only-show-errors

# Verify actual object bytes, including multipart uploads, before success.
verified=$(mktemp "${archive}.verify.XXXXXX")
aws --endpoint-url "$CANTER_M1_ENDPOINT" s3 cp "$destination" "$verified" --only-show-errors
if ! cmp -s "$archive" "$verified"; then
  echo "R2 backup verification failed" >&2
  exit 1
fi
pg_restore --list "$verified" >/dev/null
if [ "$generated" = false ]; then
  # The deployment restore test must read bytes retrieved from R2.
  mv "$verified" "$archive"
fi
printf '%s\n' "$destination"
