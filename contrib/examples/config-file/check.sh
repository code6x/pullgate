#!/bin/sh
# Runs as check_user. Reads the new policy file as data, runs nothing from
# the tree. Fails when the file is missing, too large, or not plain text.
set -eu
f="$PULLGATE_TREE/policy.toml"
[ -f "$f" ] && [ ! -L "$f" ] || { echo "policy.toml is missing or a symlink"; exit 1; }
[ "$(wc -c < "$f")" -le 65536 ] || { echo "policy.toml is larger than 64 KiB"; exit 1; }
if LC_ALL=C grep -q '[^[:print:][:space:]]' "$f"; then
  echo "policy.toml holds control characters"
  exit 1
fi
