#!/bin/sh
# The README install as written, and one full loop under the real units
# (issue #27, DESIGN.md section 14).
#
# 1. It installs the binary with the README "install-test:binary" block,
#    or checks the binary that install.sh installed (--installed).
# 2. It runs the README "install-test:setup" block: the users, the
#    directories and the units of contrib/systemd/.
# 3. It writes the configs, a local bare repo as the remote, and a fake
#    Telegram on 127.0.0.1 that taps Apply on every question.
# 4. It runs the README "install-test:start" block, with NAME=config-repo.
# 5. A good commit: the run asks (as root, check and summary as the check
#    user), the fake taps Apply, the listener (user pullgate) writes the
#    approval, pullgate-apply@ applies it, and `pullgate status` shows it.
# 6. It restarts the listener and checks that it comes back.
# 7. A commit whose apply fails: the rollback, and its notice reaches the
#    fake Telegram.
# 8. Every unit the README enables is enabled and active.
# After each loop, every message root wrote must have been read and sent
# by the listener: the outbox is empty, and the listener logged no error
# and no warning.
#
# It changes the system: it installs /usr/local/bin/pullgate, adds users,
# and writes to /etc, /usr/local/lib, /srv and /var. Run it only as root,
# on a machine you throw away, such as a CI runner.
#
# Usage: sudo sh test/units/full-loop.sh PATH-TO-PULLGATE-BINARY
#        sudo sh test/units/full-loop.sh --installed X.Y.Z
set -eu

if [ "$(id -u)" -ne 0 ]; then
	echo "full-loop.sh: run it as root, on a machine you throw away" >&2
	exit 1
fi
usage() {
	echo "usage: full-loop.sh PATH-TO-PULLGATE-BINARY | full-loop.sh --installed X.Y.Z" >&2
	exit 64
}
installed=""
binary=""
case "${1:-}" in
--installed)
	[ $# -eq 2 ] || usage
	installed="$2"
	;;
"") usage ;;
*)
	[ $# -eq 1 ] && [ -f "$1" ] || usage
	binary="$(cd "$(dirname "$1")" && pwd)/$(basename "$1")"
	;;
esac

here="$(cd "$(dirname "$0")" && pwd)"
top="$(cd "$here/../.." && pwd)"
name=config-repo
port=18444
fail=0
spool="/var/spool/pullgate/$name"
lib=/usr/local/lib/pullgate-loop
out=/var/lib/pullgate-loop
src=/srv/pullgate-loop

err() {
	echo "FAIL: $*" >&2
	fail=1
}
die() {
	echo "FAIL: $*" >&2
	exit 1
}
step() {
	echo
	echo "=== $*"
}

# readme NAME: run the README block NAME as written, with NAME=config-repo.
readme() {
	cmds="$(sh "$here/readme-block.sh" "$1" "$top/README.md")" || die "README.md has no install-test:$1 block"
	echo "$cmds" | sed 's/^/README> /'
	echo "$cmds" | sed "s/NAME/$name/g" | sh -eu || die "the README block install-test:$1 failed (see the output above)"
}

# wait_text TEXT SECONDS: wait until the fake Telegram got a message with TEXT.
wait_text() {
	i=0
	while [ "$i" -lt "$2" ]; do
		if grep -q -F -- "$1" "$texts" 2>/dev/null; then
			return 0
		fi
		i=$((i + 1))
		sleep 1
	done
	return 1
}

# outbox_empty: wait until the listener took every file from the outbox.
outbox_empty() {
	i=0
	while [ "$i" -lt 30 ]; do
		if [ -z "$(ls -A "$spool/outbox")" ]; then
			return 0
		fi
		i=$((i + 1))
		sleep 1
	done
	return 1
}

# unit_quiet UNIT: the unit logged no error and no warning.
unit_quiet() {
	journalctl --sync || true
	if journalctl --no-pager -o cat -u "$1" | grep -E 'level=(ERROR|WARN)'; then
		err "$1 logged an error or a warning (above)"
	fi
}

# listener_read_all WHEN: every message root wrote was read and sent.
listener_read_all() {
	if ! outbox_empty; then
		ls -la "$spool/outbox" >&2
		err "$1: the listener did not take every message from the outbox"
	fi
	unit_quiet pullgate-listen.service
	if grep -q -F 'could not be read' "$texts"; then
		err "$1: the listener could not read a message that root wrote"
	fi
}

# applied_sha: the applied SHA that `pullgate status` shows.
applied_sha() {
	pullgate status "$name" | sed -n 's/^applied: *//p'
}

cd "$top"

step "1. The binary"
if [ -n "$binary" ]; then
	bdir="$(mktemp -d)"
	cp "$binary" "$bdir/pullgate"
	(cd "$bdir" && readme binary)
	rm -rf "$bdir"
else
	got="$(/usr/local/bin/pullgate --version || true)"
	if [ "$got" != "pullgate $installed" ]; then
		die "/usr/local/bin/pullgate --version says '$got', want 'pullgate $installed'"
	fi
fi
pullgate --version

step "2. The README setup: users, directories, units"
readme setup
want() {
	got="$(stat -c '%U %G %a' "$1" 2>/dev/null || echo missing)"
	if [ "$got" != "$2" ]; then
		err "$1: got '$got', want '$2'"
	fi
}
want /usr/local/bin/pullgate "root root 755"
want /etc/pullgate "root root 755"
want /var/spool/pullgate "root root 755"
want /var/lib/pullgate "root root 711"
check_user="pullgate-check-$name"
id pullgate >/dev/null || die "the README setup made no user pullgate"
id "$check_user" >/dev/null || die "the README setup made no user $check_user"

step "3. The configs, the remote and the fake Telegram"
# A local bare repo, owned by root, with one commit.
rm -rf "$src"
install -d -m 0755 -o root -g root "$src"
git init -q --bare "$src/$name.git"
git -C "$src/$name.git" symbolic-ref HEAD refs/heads/main
work="$(mktemp -d)"
git -C "$work" init -q
git -C "$work" checkout -q -b main
commit() {
	printf '%s\n' "$1" >"$work/policy.toml"
	git -C "$work" add policy.toml
	git -C "$work" -c user.name=test -c user.email=test@example.com commit -q -m "$2"
	git -C "$work" push -q "$src/$name.git" main
	git -C "$work" rev-parse HEAD
}
sha1="$(commit 'a = 1' 'a good commit')"

# The commands. check and summary run as the check user; apply as root.
install -d -m 0755 -o root -g root "$lib"
install -d -m 0755 -o root -g root "$out"
cat >"$lib/check.sh" <<'EOF'
#!/bin/sh
# Runs as the check user: it must be able to read the tree.
set -eu
test -f "$PULLGATE_TREE/policy.toml"
test "$(id -un)" != root
EOF
cat >"$lib/summary.sh" <<'EOF'
#!/bin/sh
set -eu
echo "loop-summary by $(id -un)"
EOF
cat >"$lib/apply.sh" <<'EOF'
#!/bin/sh
# The apply fails for a policy that starts with "fail".
set -eu
if grep -q '^fail' "$PULLGATE_TREE/policy.toml"; then
	echo "apply.sh: this policy fails on purpose" >&2
	exit 1
fi
install -m 0644 "$PULLGATE_TREE/policy.toml" /var/lib/pullgate-loop/policy.toml
printf '%s\n' "$PULLGATE_SHA" >/var/lib/pullgate-loop/applied-sha
EOF
chown root:root "$lib"/*.sh
chmod 0755 "$lib"/*.sh

cat >"/etc/pullgate/$name.toml" <<EOF
mode = "ask"
on_failure = "rollback"
host = "example-host"

[repo]
url = "$src/$name.git"
branch = "main"

[commands]
check = ["$lib/check.sh"]
summary = ["$lib/summary.sh"]
apply = ["$lib/apply.sh"]
check_user = "$check_user"

[watch]
paths = ["policy.toml"]

[listener]
user = "pullgate"
EOF
chown root:root "/etc/pullgate/$name.toml"
chmod 0600 "/etc/pullgate/$name.toml"

printf '123:loop-test-token-made-by-the-script\n' >/etc/pullgate/bot-token
chown root:root /etc/pullgate/bot-token
chmod 0600 /etc/pullgate/bot-token
cat >/etc/pullgate/listener.toml <<EOF
owner_id = 4242
chat_id = 4242
instances = ["$name"]
api_url = "http://127.0.0.1:$port"
EOF
chown root:pullgate /etc/pullgate/listener.toml
chmod 0640 /etc/pullgate/listener.toml

tglog="$(mktemp)"
texts="$(mktemp)"
python3 -I "$here/faketg.py" "$port" "$tglog" --tap "$texts" &
tgpid=$!
trap 'kill "$tgpid" 2>/dev/null || true; rm -rf "$work"' EXIT
i=0
until python3 -I -c 'import socket, sys; socket.create_connection(("127.0.0.1", int(sys.argv[1])), 1)' "$port" 2>/dev/null; do
	i=$((i + 1))
	[ "$i" -lt 20 ] || die "the fake Telegram did not start on 127.0.0.1:$port"
	sleep 0.5
done

step "4. The README start: enable --now"
readme start

step "5. A good commit: ask, tap, apply"
# The timer may have started a run already; start waits for it.
systemctl start "pullgate-run@$name.service" || err "pullgate-run@$name.service failed"
if ! wait_text "✅ Applied" 90; then
	journalctl --no-pager -o cat -u "pullgate-run@$name.service" -u pullgate-listen.service -u "pullgate-apply@$name.service" || true
	die "no 'Applied' message reached the fake Telegram"
fi
grep -q -F "loop-summary by $check_user" "$texts" || err "the question has no summary from the check user $check_user"
journalctl --sync || true
journalctl --no-pager -o cat -u "pullgate-run@$name.service" | grep -q 'msg=asked ' || err "the run did not log 'asked'"
journalctl --no-pager -o cat -u "pullgate-apply@$name.service" | grep -q 'msg=applied ' || err "pullgate-apply@$name.service did not log 'applied'"
[ "$(cat "$out/applied-sha" 2>/dev/null)" = "$sha1" ] || err "apply.sh did not apply $sha1"
[ "$(applied_sha)" = "$sha1" ] || err "pullgate status shows applied '$(applied_sha)', want $sha1"
pullgate status "$name"
want "$spool/outbox" "root pullgate 770"
want "$spool/inbox" "pullgate pullgate 700"
listener_read_all "after the good commit"

step "6. Restart the listener"
before="$(grep -c -x getUpdates "$tglog" || true)"
systemctl restart pullgate-listen.service || err "systemctl restart pullgate-listen.service failed"
i=0
while [ "$i" -lt 30 ]; do
	now="$(grep -c -x getUpdates "$tglog" || true)"
	if systemctl is-active --quiet pullgate-listen.service && [ "$now" -gt $((before + 1)) ]; then
		break
	fi
	i=$((i + 1))
	sleep 1
done
systemctl is-active --quiet pullgate-listen.service || err "the listener is not active after a restart"
[ "$now" -gt $((before + 1)) ] || err "the listener does not poll Telegram after a restart"

step "7. A commit whose apply fails: rollback"
sha2="$(commit 'fail = true' 'a commit whose apply fails')"
systemctl start "pullgate-run@$name.service" || err "pullgate-run@$name.service failed"
if ! wait_text "Rolled back to" 90; then
	journalctl --no-pager -o cat -u "pullgate-run@$name.service" -u pullgate-listen.service -u "pullgate-apply@$name.service" || true
	die "no 'Rolled back' notice reached the fake Telegram"
fi
[ "$(cat "$out/policy.toml" 2>/dev/null)" = "a = 1" ] || err "the rollback did not put back the policy of $sha1"
[ "$(applied_sha)" = "$sha1" ] || err "after the rollback, pullgate status shows applied '$(applied_sha)', want $sha1"
pullgate status "$name" | grep -q "^tip mark: *failed" || err "pullgate status does not show the tip $sha2 as failed"
pullgate status "$name"
listener_read_all "after the rollback"
unit_quiet "pullgate-run@$name.service"

step "8. Every unit the README enables"
units="$(sh "$here/readme-block.sh" start "$top/README.md" | sed -e 's/#.*//' -e "s/NAME/$name/g" |
	tr -s ' \t' '\n' | grep -E '^pullgate-[A-Za-z0-9@._-]+\.(service|path|timer)$' | sort -u)"
n=0
for u in $units; do
	n=$((n + 1))
	state="$(systemctl is-enabled "$u" 2>/dev/null || true)"
	[ "$state" = enabled ] || err "$u: systemctl is-enabled says '$state', want 'enabled'"
	state="$(systemctl is-active "$u" 2>/dev/null || true)"
	[ "$state" = active ] || err "$u: systemctl is-active says '$state', want 'active'"
	echo "$u: enabled, active"
done
[ "$n" -ge 4 ] || err "found $n units in the README start block, want at least 4"

if [ "$fail" -ne 0 ]; then
	exit 1
fi
echo
echo "ok: the README install and a full loop work under the units of contrib/systemd/"
