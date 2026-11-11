#!/bin/sh
# Runs as root after Apply. Safe to run again on an older tree: it only
# puts one file in place and reloads. A rollback runs it with the last
# good tree.
set -eu
src="$PULLGATE_TREE/policy.toml"
dst=/etc/example-broker/policy.toml
[ -f "$src" ] && [ ! -L "$src" ] || { echo "policy.toml is missing or a symlink"; exit 1; }
install -m 0640 -o root -g example-broker "$src" "$dst.new"
mv -f "$dst.new" "$dst"
systemctl reload example-broker.service
