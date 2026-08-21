#!/usr/bin/env bash
set -euo pipefail

KEY_DIR="${1:-$HOME/.ssh/deadlines-api}"
KEY_NAME="github_deploy_key"

mkdir -p "$KEY_DIR"
chmod 700 "$KEY_DIR"

if [[ -f "$KEY_DIR/$KEY_NAME" ]]; then
  echo "Key already exists: $KEY_DIR/$KEY_NAME"
  exit 0
fi

ssh-keygen -t ed25519 -C "deadlines-api-deploy-key" -f "$KEY_DIR/$KEY_NAME" -N ""
chmod 600 "$KEY_DIR/$KEY_NAME"
chmod 644 "$KEY_DIR/$KEY_NAME.pub"

echo "Public key (add to GitHub repo Settings -> Deploy keys -> Read-only):"
cat "$KEY_DIR/$KEY_NAME.pub"
