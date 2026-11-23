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
# 8. With --update-to: /update from the installed build to a newer one,
#    through a fake GitHub (a fake curl and a fake gh): the question, the
#    tap, the install with the checks of install.sh, a changed unit put
#    back, a drop-in listed, and the report from the new listener.
# 9. Every unit the README enables is enabled and active.
# 10. It runs the README "install-test:doctor" block: pullgate doctor must
#    find no problem.
# After each loop, every message root wrote must have been read and sent
# by the listener: the outbox is empty, and the listener logged no error
# and no warning.
#
# It changes the system: it installs /usr/local/bin/pullgate, adds users,
# and writes to /etc, /usr/local/lib, /srv and /var. Run it only as root,
# on a machine you throw away, such as a CI runner.
#
# Usage: sudo sh test/units/full-loop.sh PATH-TO-PULLGATE-BINARY [--update-to PATH-TO-NEWER-BINARY]
#        sudo sh test/units/full-loop.sh --installed X.Y.Z
#
# With --update-to, both binaries must have a release version (built with
# -ldflags "-X main.version=X.Y.Z"), the second one newer. Then the script
# moves /usr/bin/curl aside for a fake one, and puts it back at the end.
set -eu

if [ "$(id -u)" -ne 0 ]; then
	echo "full-loop.sh: run it as root, on a machine you throw away" >&2
	exit 1
fi
usage() {
	echo "usage: full-loop.sh PATH-TO-PULLGATE-BINARY [--update-to PATH-TO-NEWER-BINARY] | full-loop.sh --installed X.Y.Z" >&2
	exit 64
}
installed=""
binary=""
update_to=""
if [ $# -eq 3 ] && [ "$2" = --update-to ]; then
	[ -f "$3" ] || usage
	update_to="$(cd "$(dirname "$3")" && pwd)/$(basename "$3")"
	set -- "$1"
fi
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
	printf '%s\n' "$cmds" | sed 's/^/README> /'
	printf '%s\n' "$cmds" | sed "s/NAME/$name/g" | sh -eu || die "the README block install-test:$1 failed (see the output above)"
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
	# No log line at all would make the check below pass without checking.
	if ! journalctl --no-pager -o cat -u "$1" | grep -q 'level=INFO'; then
		err "$1: no log line found in the journal, so its errors cannot be checked"
	fi
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

# run_until_tip SHA: start runs until one has fetched SHA. A run that the
# timer started before the push is joined by `systemctl start` and does
# not see the new commit, so one start is not enough.
run_until_tip() {
	i=0
	while [ "$i" -lt 10 ]; do
		systemctl start "pullgate-run@$name.service" || err "pullgate-run@$name.service failed"
		if pullgate status "$name" | grep -q "^last tip: *$1\$"; then
			return 0
		fi
		i=$((i + 1))
		sleep 1
	done
	die "after 10 runs, pullgate status does not show the last tip $1"
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

tglog="$(mktemp)"
texts="$(mktemp)"
python3 -I "$here/faketg.py" "$port" "$tglog" --tap "$texts" &
tgpid=$!
cleanup() {
	kill "$tgpid" 2>/dev/null || true
	rm -rf "$work"
	if [ -f /usr/bin/curl.full-loop ]; then
		mv -f /usr/bin/curl.full-loop /usr/bin/curl
	fi
}
trap cleanup EXIT
i=0
until python3 -I -c 'import socket, sys; socket.create_connection(("127.0.0.1", int(sys.argv[1])), 1)' "$port" 2>/dev/null; do
	i=$((i + 1))
	[ "$i" -lt 20 ] || die "the fake Telegram did not start on 127.0.0.1:$port"
	sleep 0.5
done

step "3a. pullgate bootstrap: the token, the pairing on the fake Telegram"
# The token comes on stdin, never as an argument. The owner and chat IDs
# come only from the message with the code (DESIGN.md section 17).
boot_out="$(mktemp)"
printf '123:loop-test-token-made-by-the-script\n' |
	pullgate bootstrap --token-stdin --api-url "http://127.0.0.1:$port" \
		--instance "$name" --repo "$src/$name.git" >"$boot_out" 2>&1 &
boot_pid=$!
code=""
i=0
until [ -n "$code" ]; do
	code="$(sed -n 's/^    \([0-9]\{4\}\) \([0-9]\{4\}\)$/\1\2/p' "$boot_out")"
	[ -n "$code" ] && break
	i=$((i + 1))
	[ "$i" -lt 60 ] || {
		cat "$boot_out"
		die "pullgate bootstrap printed no pairing code"
	}
	sleep 0.5
done
python3 -I -c 'import json, sys, urllib.request
urllib.request.urlopen(urllib.request.Request("http://127.0.0.1:%s/test/say" % sys.argv[1], data=json.dumps({"text": sys.argv[2]}).encode(), method="POST"), timeout=5)' "$port" "$code"
wait "$boot_pid" || {
	cat "$boot_out"
	die "pullgate bootstrap failed"
}
cat "$boot_out"
grep -q '^owner_id = 4242$' /etc/pullgate/listener.toml || die "bootstrap did not take owner_id from the pairing"
grep -q '^chat_id = 4242$' /etc/pullgate/listener.toml || die "bootstrap did not take chat_id from the pairing"
want /etc/pullgate/listener.toml "root pullgate 640"
want /etc/pullgate/bot-token "root root 600"
if grep -q loop-test-token "$boot_out"; then
	die "bootstrap printed the token"
fi

if [ -n "$update_to" ]; then
	step "3b. A fake GitHub for /update"
	old_v="$(pullgate --version | sed 's/^pullgate //')"
	new_v="$("$update_to" --version | sed 's/^pullgate //')"
	case "$(uname -m)" in
	x86_64) arch=amd64 ;;
	aarch64) arch=arm64 ;;
	*) die "no release for $(uname -m)" ;;
	esac
	web="$out/web"
	# install.sh refuses a bin directory that group or others can write.
	# The runner image may have one; fix it, as an owner would.
	stat -c '%U %a' /usr/local/bin
	chown root:root /usr/local/bin
	chmod go-w /usr/local/bin
	# fake_release VERSION [BINARY]: the newest release on the fake GitHub.
	# With BINARY, also its files, made as the release workflow makes them.
	fake_release() {
		api="$web/api.github.com/repos/code6x/pullgate/releases"
		install -d -m 0755 "$api"
		install -d -m 0755 "$api/tags"
		printf '{"tag_name": "v%s", "body": "## v%s\\n\\n### Bug Fixes\\n\\n* a fix\\n"}\n' "$1" "$1" >"$api/latest"
		cp "$api/latest" "$api/tags/v$1"
		[ $# -eq 2 ] || return 0
		dl="$web/github.com/code6x/pullgate/releases/download/v$1"
		stage="$(mktemp -d)"
		install -d -m 0755 "$dl"
		install -m 0755 "$2" "$stage/pullgate"
		cp "$top/LICENSE" "$top/README.md" "$stage/"
		tar --owner=0 --group=0 --numeric-owner --mode=go-w -C "$stage" -cf - pullgate LICENSE README.md |
			gzip -9n >"$dl/pullgate_$1_linux_$arch.tar.gz"
		rm -rf "$stage"
		(cd "$dl" && sha256sum -- *.tar.gz >SHA256SUMS)
		echo '{}' >"$dl/pullgate_$1.sigstore.json"
	}
	fake_release "$old_v"
	# The fake curl serves https://HOST/PATH from $web/HOST/PATH. install.sh
	# runs as root with PATH=/usr/sbin:/usr/bin:/sbin:/bin, so it must be
	# /usr/bin/curl. The trap puts the real one back.
	mv /usr/bin/curl /usr/bin/curl.full-loop
	cat >/usr/bin/curl <<FAKE
#!/bin/sh
out=
url=
while [ "\$#" -gt 0 ]; do
	case "\$1" in
	--output) out="\$2"; shift ;;
	esac
	url="\$1"
	shift
done
case "\$url" in
https://*) ;;
*) echo "fake curl: not HTTPS: \$url" >&2; exit 1 ;;
esac
file="$web/\${url#https://}"
[ -f "\$file" ] || { echo "curl: (22) The requested URL returned error: 404" >&2; exit 22; }
cp "\$file" "\$out"
FAKE
	chmod 0755 /usr/bin/curl
	# The fake gh: the build under test has no attestation. It writes down
	# each call. [updates] gh names it, as install.sh --gh does.
	cat >"$lib/gh" <<FAKE
#!/bin/sh
echo "\$*" >>"$out/gh-calls"
if [ "\$1" = --version ]; then
	echo "gh version 2.102.0 (2026-09-30)"
fi
exit 0
FAKE
	chown root:root "$lib/gh"
	chmod 0755 "$lib/gh"
	printf '\n[updates]\ngh = "%s"\n' "$lib/gh" >>/etc/pullgate/listener.toml
fi

step "4. The README start: enable --now"
readme start

step "5. A good commit: ask, tap, apply"
# The timer may have started a run already; start waits for it.
run_until_tip "$sha1"
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
run_until_tip "$sha2"
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

if [ -n "$update_to" ]; then
	step "8. /update from $old_v to $new_v, with its units"
	# The timer has a random delay of up to an hour. One run now makes the
	# directories of the updates, so the listener can write /update.
	systemctl start pullgate-update.service || err "pullgate-update.service failed before the new release"
	fake_release "$new_v" "$update_to"
	sum="$(cut -d ' ' -f 1 "$web/github.com/code6x/pullgate/releases/download/v$new_v/SHA256SUMS")"
	# A unit that differs from the shipped one, and a drop-in.
	printf '# changed by full-loop.sh\n' >>/etc/systemd/system/pullgate-run@.timer
	install -d -m 0755 /etc/systemd/system/pullgate-listen.service.d
	printf '[Service]\nEnvironment=PULLGATE_LOOP=1\n' >/etc/systemd/system/pullgate-listen.service.d/loop.conf
	systemctl daemon-reload
	python3 -I -c 'import json, sys, urllib.request
urllib.request.urlopen(urllib.request.Request("http://127.0.0.1:%s/test/say" % sys.argv[1], data=json.dumps({"text": sys.argv[2]}).encode(), method="POST"), timeout=5)' "$port" /update
	if ! wait_text "pullgate is updated" 300; then
		journalctl --no-pager -o cat -u pullgate-update.service -u pullgate-listen.service || true
		die "no 'pullgate is updated' report reached the fake Telegram"
	fi
	grep -F "Update pullgate to" "$texts" | grep -q -F "$sum" || err "the update question does not show the SHA-256 $sum"
	got="$(/usr/local/bin/pullgate --version || true)"
	[ "$got" = "pullgate $new_v" ] || err "after /update, pullgate --version says '$got', want 'pullgate $new_v'"
	cmp -s "$top/contrib/systemd/pullgate-run@.timer" /etc/systemd/system/pullgate-run@.timer ||
		err "/update did not put back the shipped pullgate-run@.timer"
	grep -q -F 'pullgate-run@.timer' "$texts" || err "the report does not list the unit it installed"
	grep -q -F 'pullgate-listen.service.d/loop.conf' "$texts" || err "the report does not list the drop-in"
	[ -f /etc/systemd/system/pullgate-listen.service.d/loop.conf ] || err "/update removed a drop-in"
	[ "$(grep -c '^attestation verify' "$out/gh-calls" || true)" -ge 2 ] ||
		err "gh attestation verify must run before the question and again on Apply"
	[ ! -e /usr/local/bin/.pullgate.prev ] || err "the old binary is still kept after a good update"
	i=0
	while [ "$i" -lt 30 ] && [ -n "$(ls -A /var/spool/pullgate/_update/outbox)" ]; do
		i=$((i + 1))
		sleep 1
	done
	[ -z "$(ls -A /var/spool/pullgate/_update/outbox)" ] || err "the listener did not take every update message from the outbox"
	journalctl --sync || true
	if journalctl --no-pager -o cat -u pullgate-update.service | grep -E 'level=ERROR'; then
		err "pullgate-update.service logged an error (above)"
	fi
	journalctl --no-pager -o cat -u pullgate-update.service | grep -q 'msg=updated ' || err "pullgate-update.service did not log 'updated'"
	systemctl is-active --quiet pullgate-listen.service || err "the listener is not active after /update"
	listener_read_all "after /update"
fi

step "9. Every unit the README enables"
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

step "10. The README doctor: every check ok"
# The run unit's last run made every directory, so no "not made yet" note.
doctor_out="$(sh "$here/readme-block.sh" doctor "$top/README.md" | sh -eu 2>&1)" || {
	printf '%s\n' "$doctor_out"
	die "pullgate doctor found a problem (above)"
}
printf '%s\n' "$doctor_out"
printf '%s\n' "$doctor_out" | grep -q '^All checks passed\.$' || err "pullgate doctor did not say: All checks passed."
if printf '%s\n' "$doctor_out" | grep -q '^PROBLEM'; then
	err "pullgate doctor printed a PROBLEM line"
fi

if [ "$fail" -ne 0 ]; then
	exit 1
fi
echo
echo "ok: the README install and a full loop work under the units of contrib/systemd/"
