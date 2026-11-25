#!/usr/bin/env bash
# Run the asset publishing integration test (modules/assets/live_test.go)
# against a throwaway local MinIO: publish, dedupe, rollback, verify and prune
# on a real S3 API. Credentials are random and live only in this process tree.
#
#   scripts/drill/assets_minio.sh            # MINIO_BIN=/path/to/minio to override
#
# Other providers (R2, AWS): export GOSTACK_TEST_S3_ENDPOINT/_BUCKET/_ACCESS_KEY/
# _SECRET_KEY/_REGION yourself and run
#   go test -count=1 -run TestLiveAssetsS3 -v ./modules/assets/
set -euo pipefail
cd "$(dirname "$0")/../.."
. scripts/drill/minio_lib.sh

WORK=$(mktemp -d "${TMPDIR:-/tmp}/gostack-assetsdrill.XXXXXX")
cleanup() {
  minio_stop
  rm -rf "$WORK"
}
trap cleanup EXIT

minio_start "$WORK/data" "$WORK/minio.log"
echo "MinIO $("${MINIO_BIN:-minio}" --version 2>/dev/null | head -1) at $MINIO_ENDPOINT" >&2

GOSTACK_TEST_S3_ENDPOINT="$MINIO_ENDPOINT" GOSTACK_TEST_S3_BUCKET=gostack-assets-it \
  GOSTACK_TEST_S3_ACCESS_KEY="$MINIO_KEY" GOSTACK_TEST_S3_SECRET_KEY="$MINIO_SECRET" \
  GOSTACK_TEST_S3_REGION=us-east-1 GOSTACK_TEST_S3_CREATE_BUCKET=1 \
  go test -count=1 -run TestLiveAssetsS3 -v ./modules/assets/
