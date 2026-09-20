#!/usr/bin/env bash
# MinIO blob closure (kept for the historical entry point): delegates to
# s3-storage-e2e.sh with the minio store profile.
#
# Usage: bash deploy/scripts/minio-storage-e2e.sh
set -euo pipefail
exec bash "$(dirname "$0")/s3-storage-e2e.sh" minio
