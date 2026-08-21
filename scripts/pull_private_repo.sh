#!/usr/bin/env bash
set -euo pipefail

REPO_DIR="${1:-$PWD}"
KEY_PATH="${2:-$HOME/.ssh/deadlines-api/github_deploy_key}"
BRANCH="${3:-main}"

if [[ ! -d "$REPO_DIR/.git" ]]; then
  echo "Not a git repository: $REPO_DIR"
  exit 1
fi

if [[ ! -f "$KEY_PATH" ]]; then
  echo "Deploy key not found: $KEY_PATH"
  exit 1
fi

mkdir -p "$HOME/.ssh"
chmod 700 "$HOME/.ssh"
ssh-keyscan -t ed25519 github.com >> "$HOME/.ssh/known_hosts" 2>/dev/null || true
chmod 644 "$HOME/.ssh/known_hosts"

export GIT_SSH_COMMAND="ssh -i $KEY_PATH -o IdentitiesOnly=yes"

cd "$REPO_DIR"
git fetch origin "$BRANCH"
git checkout "$BRANCH"
git reset --hard "origin/$BRANCH"

echo "Repository updated to origin/$BRANCH"
