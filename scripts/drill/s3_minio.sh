#!/usr/bin/env bash
# Run the S3 adapter integration test (modules/storage/s3_live_test.go)
# against a throwaway local MinIO. Credentials are random and live only in
# this process tree.
#
#   scripts/drill/s3_minio.sh            # MINIO_BIN=/path/to/minio to override
#
# To test another provider instead (R2, AWS), skip this script and export
# GOSTACK_TEST_S3_ENDPOINT/_BUCKET/_ACCESS_KEY/_SECRET_KEY/_REGION yourself:
#   go test -count=1 -run TestLiveS3 -v ./modules/storage/
set -euo pipefail
cd "$(dirname "$0")/../.."
. scripts/drill/minio_lib.sh

WORK=$(mktemp -d "${TMPDIR:-/tmp}/gostack-s3drill.XXXXXX")
cleanup() {
  minio_stop
  rm -rf "$WORK"
}
trap cleanup EXIT

minio_start "$WORK/data" "$WORK/minio.log"
echo "MinIO $("${MINIO_BIN:-minio}" --version 2>/dev/null | head -1) at $MINIO_ENDPOINT" >&2

GOSTACK_TEST_S3_ENDPOINT="$MINIO_ENDPOINT" GOSTACK_TEST_S3_BUCKET=gostack-it \
  GOSTACK_TEST_S3_ACCESS_KEY="$MINIO_KEY" GOSTACK_TEST_S3_SECRET_KEY="$MINIO_SECRET" \
  GOSTACK_TEST_S3_REGION=us-east-1 GOSTACK_TEST_S3_CREATE_BUCKET=1 \
  go test -count=1 -run TestLiveS3 -v ./modules/storage/
