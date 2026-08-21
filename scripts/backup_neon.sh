#!/usr/bin/env bash
set -euo pipefail

# Usage:
#   NEW_DATABASE_URL='postgresql://...' bash scripts/backup_neon.sh
# or:
#   bash scripts/backup_neon.sh 'postgresql://...'

DB_URL="${1:-${NEW_DATABASE_URL:-${DATABASE_URL:-}}}"
OUT_DIR="${BACKUP_DIR:-$PWD/backups}"
STAMP="$(date +%Y%m%d_%H%M%S)"
OUT_FILE="$OUT_DIR/neon_backup_${STAMP}.dump"

if [[ -z "$DB_URL" ]]; then
  echo "Database URL is not set. Pass it as arg1 or set NEW_DATABASE_URL/DATABASE_URL."
  exit 1
fi

mkdir -p "$OUT_DIR"

# Use local pg_dump if available, fallback to Docker Postgres 18 client.
if command -v pg_dump >/dev/null 2>&1; then
  echo "Using local pg_dump"
  pg_dump --dbname="$DB_URL" -Fc -f "$OUT_FILE"
else
  if ! command -v docker >/dev/null 2>&1; then
    echo "Neither pg_dump nor docker is available. Install one of them."
    exit 1
  fi
  echo "Using docker postgres:18 pg_dump"
  docker run --rm -v "$OUT_DIR:/work" postgres:18 \
    pg_dump --dbname="$DB_URL" -Fc -f "/work/$(basename "$OUT_FILE")"
fi

if [[ ! -s "$OUT_FILE" ]]; then
  echo "Backup file is empty: $OUT_FILE"
  exit 1
fi

echo "Backup created: $OUT_FILE"
ls -lh "$OUT_FILE"
