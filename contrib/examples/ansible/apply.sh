#!/bin/sh
# Runs as root after Apply, in the tree of the approved commit.
set -eu
cd "$PULLGATE_TREE"
# ansible writes its temporary files under HOME.
export HOME=/root
exec ansible-playbook --inventory localhost, --connection local site.yml
