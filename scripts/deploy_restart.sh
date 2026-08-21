#!/usr/bin/env bash
set -euo pipefail

REPO_DIR="${1:-$PWD}"
KEY_PATH="${2:-$HOME/.ssh/deadlines-api/github_deploy_key}"
BRANCH="${3:-main}"
SERVICE_NAME="${4:-deadlines-api}"

if [[ ! -d "$REPO_DIR" ]]; then
  echo "Directory does not exist: $REPO_DIR"
  exit 1
fi

if [[ ! -x "$REPO_DIR/scripts/pull_private_repo.sh" ]]; then
  echo "Missing executable script: $REPO_DIR/scripts/pull_private_repo.sh"
  exit 1
fi

echo "[1/4] Pull latest code from private repository"
"$REPO_DIR/scripts/pull_private_repo.sh" "$REPO_DIR" "$KEY_PATH" "$BRANCH"

echo "[2/4] Build Go binary"
cd "$REPO_DIR"
go build -o server .

if [[ ! -x "$REPO_DIR/server" ]]; then
  echo "Build completed, but binary not found: $REPO_DIR/server"
  exit 1
fi

echo "[3/4] Restart systemd service: $SERVICE_NAME"
if [[ $EUID -eq 0 ]]; then
  systemctl restart "$SERVICE_NAME"
  systemctl --no-pager --full status "$SERVICE_NAME" | head -n 20
else
  sudo systemctl restart "$SERVICE_NAME"
  sudo systemctl --no-pager --full status "$SERVICE_NAME" | head -n 20
fi

echo "[4/4] Done"
