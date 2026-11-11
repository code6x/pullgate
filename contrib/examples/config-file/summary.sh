#!/bin/sh
# Runs as check_user. Shows what changes in the policy file, from the two
# trees only; it does not read the live file in /etc.
set -u
new="$PULLGATE_TREE/policy.toml"
if [ -z "$PULLGATE_OLD_TREE" ]; then
  echo "first apply: policy.toml has $(wc -l < "$new") lines"
  exit 0
fi
diff -u "$PULLGATE_OLD_TREE/policy.toml" "$new"
# diff exits 1 when the files differ; that is not a failure here.
[ $? -le 1 ]
