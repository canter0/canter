#!/bin/sh
set -eu
umask 077

stamp=$(date -u +%Y%m%dT%H%M%SZ)
archive=$(mktemp "/var/lib/canter/postgres-${stamp}.XXXXXX.dump")
cleanup() { rm -f "$archive"; }
trap cleanup EXIT INT TERM

pg_dump --format=custom --compress=9 --no-owner --no-privileges --file="$archive" "$CANTER_DATABASE_URL"

export AWS_ACCESS_KEY_ID="$CANTER_M1_ACCESS_KEY"
export AWS_SECRET_ACCESS_KEY="$CANTER_M1_SECRET_KEY"
export AWS_DEFAULT_REGION="$CANTER_M1_REGION"
aws --endpoint-url "$CANTER_M1_ENDPOINT" s3 cp \
  "$archive" "s3://${CANTER_M1_BUCKET}/ops/control-plane/postgres/${stamp}.dump" \
  --only-show-errors
