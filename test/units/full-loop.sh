#!/bin/sh
# The README install as written, and one full loop under the real units
# (issue #27, DESIGN.md section 14).
#
# 1. It installs the binary with the README "install-test:binary" block,
#    or checks the binary that install.sh installed (--installed).
# 2. Nothing else: no user, no unit, no config, no folder (issues #49,
#    #55).
# 3. It publishes the instance as a recipe on a fake GitHub release
#    server on 127.0.0.1, with a fake gh, and makes a local bare repo as
#    the remote and a fake Telegram on 127.0.0.1 that taps Apply on every
#    question. Then `pullgate bootstrap --recipe` checks and unpacks the
#    recipe, makes the users, the directories, the units and the config
#    from the template, pairs, runs the recipe's setup and doctor, and
#    enables the units.
# 4. It checks what bootstrap made.
# 5. A good commit: the run asks (as root, check and summary as the check
#    user), the fake taps Apply, the listener (user pullgate) writes the
#    approval, pullgate-apply@ applies it, and `pullgate status` shows it.
#    5b. A commit that moves the recipe to a new version: the recipe's
#    apply runs `pullgate recipe fetch` and `pullgate recipe use`.
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

step "2. No setup by hand: bootstrap makes the users and the units"
check_user="pullgate-check-$name"
if id pullgate >/dev/null 2>&1 || id "$check_user" >/dev/null 2>&1; then
	die "the users exist before bootstrap; run this on a fresh machine"
fi
want() {
	got="$(stat -c '%U %G %a' "$1" 2>/dev/null || echo missing)"
	if [ "$got" != "$2" ]; then
		err "$1: got '$got', want '$2'"
	fi
}
want /usr/local/bin/pullgate "root root 755"

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

# The instance comes as a recipe (DESIGN.md section 20), from a fake
# GitHub release on 127.0.0.1 with a fake gh, as a project that ships a
# recipe tests it in its own CI. check and summary run as the check user;
# apply and setup as root.
install -d -m 0755 -o root -g root "$lib"
install -d -m 0755 -o root -g root "$out"
rport=18445
rweb="$out/releases"
rgh="$lib/recipe-gh"
cat >"$rgh" <<'EOF'
#!/bin/sh
# A fake gh for the recipe: it checks the flags as the real one would,
# and prints the signed statement with the name and SHA-256 of the file.
set -eu
if [ "$1" = --version ]; then echo "gh version 2.100.0 (2026-01-01)"; exit 0; fi
[ "$1 $2" = "attestation verify" ] || exit 9
f=$3
shift 3
self=0
while [ $# -gt 0 ]; do
	case $1 in
	--repo) [ "$2" = example-org/example-recipe ] || exit 1; shift 2 ;;
	--cert-identity) [ "$2" = https://github.com/example-org/example-recipe/.github/workflows/release.yml@refs/heads/main ] || exit 1; shift 2 ;;
	--deny-self-hosted-runners) self=1; shift ;;
	*) shift 2 ;;
	esac
done
[ "$self" = 1 ] || exit 1
printf '[{"verificationResult":{"statement":{"subject":[{"name":"%s","digest":{"sha256":"%s"}}]}}}]' "$(basename "$f")" "$(sha256sum "$f" | cut -d ' ' -f 1)"
EOF
chown root:root "$rgh"
chmod 0755 "$rgh"
# make_recipe VERSION: publish version VERSION of the recipe.
make_recipe() {
	rdir="$(mktemp -d)"
	cat >"$rdir/pullgate-recipe.toml" <<EOF
format = 1
version = "$1"

[[instance]]
name = "$name"
config = "$name.toml.in"
requires = ["install", "python3"]
setup = ["setup.sh"]
EOF
	cat >"$rdir/$name.toml.in" <<'EOF'
on_failure = "rollback"
host = "example-host"

[repo]
url = "@REPO_URL@"
branch = "@BRANCH@"

[commands]
check = ["@RECIPE_DIR@/check.sh"]
summary = ["@RECIPE_DIR@/summary.sh"]
apply = ["@RECIPE_DIR@/apply.sh"]

[watch]
paths = ["policy.toml", "recipe-version"]
EOF
	cat >"$rdir/check.sh" <<'EOF'
#!/bin/sh
# Runs as the check user: it must be able to read the tree.
set -eu
test -f "$PULLGATE_TREE/policy.toml"
test "$(id -un)" != root
EOF
	cat >"$rdir/summary.sh" <<'EOF'
#!/bin/sh
set -eu
echo "loop-summary by $(id -un)"
EOF
	cat >"$rdir/setup.sh" <<'EOF'
#!/bin/sh
# Runs once as root, at the setup, with four variables only.
set -eu
env | sort >/var/lib/pullgate-loop/setup-env
EOF
	cat >"$rdir/apply.sh" <<EOF
#!/bin/sh
# The apply fails for a policy that starts with "fail".
set -eu
if grep -q '^fail' "\$PULLGATE_TREE/policy.toml"; then
	echo "apply.sh: this policy fails on purpose" >&2
	exit 1
fi
install -m 0644 "\$PULLGATE_TREE/policy.toml" /var/lib/pullgate-loop/policy.toml
printf '%s\n' "\$PULLGATE_SHA" >/var/lib/pullgate-loop/applied-sha
# The split form of a recipe update (DESIGN.md section 21): fetch the new
# version, then use it. It counts from the next run.
if [ -f "\$PULLGATE_TREE/recipe-version" ]; then
	v="\$(cat "\$PULLGATE_TREE/recipe-version")"
	case "\$v" in *[!0-9.]* | "") exit 1 ;; esac
	/usr/local/bin/pullgate recipe fetch "http://127.0.0.1:$rport/example-org/example-recipe/releases/download/v\$v/loop_\${v}_recipe.tar.gz" --gh "$rgh" | tail -n 1 >/var/lib/pullgate-loop/recipe-dir
	/usr/local/bin/pullgate recipe use "\$PULLGATE_INSTANCE" "\$v"
fi
EOF
	chmod 0755 "$rdir"/*.sh
	rel="$rweb/example-org/example-recipe/releases/download/v$1"
	install -d -m 0755 "$rel"
	tar -C "$rdir" --owner=0 --group=0 --numeric-owner -czf "$rel/loop_$1_recipe.tar.gz" .
	(cd "$rel" && sha256sum "loop_$1_recipe.tar.gz" >SHA256SUMS)
	printf '{}\n' >"$rel/loop_$1_recipe.sigstore.json"
	rm -rf "$rdir"
}
make_recipe 1.0.0
recipe_url="http://127.0.0.1:$rport/example-org/example-recipe/releases/download/v1.0.0/loop_1.0.0_recipe.tar.gz"

tglog="$(mktemp)"
texts="$(mktemp)"
python3 -I "$here/faketg.py" "$port" "$tglog" --tap "$texts" &
tgpid=$!
python3 -I -m http.server --bind 127.0.0.1 --directory "$rweb" "$rport" >/dev/null 2>&1 &
relpid=$!
cleanup() {
	kill "$tgpid" 2>/dev/null || true
	kill "$relpid" 2>/dev/null || true
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
i=0
until python3 -I -c 'import socket, sys; socket.create_connection(("127.0.0.1", int(sys.argv[1])), 1)' "$rport" 2>/dev/null; do
	i=$((i + 1))
	[ "$i" -lt 20 ] || die "the fake release server did not start on 127.0.0.1:$rport"
	sleep 0.5
done

step "3a. pullgate bootstrap --recipe: the recipe, the token, the pairing on the fake Telegram"
# The token comes on stdin, never as an argument. The owner and chat IDs
# come only from the message with the code (DESIGN.md section 17).
boot_out="$(mktemp)"
printf '123:loop-test-token-made-by-the-script\n' |
	pullgate bootstrap --token-stdin --api-url "http://127.0.0.1:$port" \
		--recipe "$recipe_url" --gh "$rgh" \
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
rdir0=/usr/local/lib/pullgate/recipes/example-org/example-recipe/1.0.0
want "/etc/pullgate/$name.toml" "root root 600"
want "$rdir0" "root root 755"
want "$rdir0/apply.sh" "root root 755"
grep -q '^mode = "ask"$' "/etc/pullgate/$name.toml" || err "the config from the recipe is not in ask mode"
grep -q -F "$rdir0/apply.sh" "/etc/pullgate/$name.toml" || err "the config from the recipe does not name its apply.sh"
grep -q '^version = "1.0.0"$' "/etc/pullgate/$name.recipe" || err "bootstrap wrote no recipe record"
[ "$(cut -d = -f 1 /var/lib/pullgate-loop/setup-env 2>/dev/null | tr '\n' ' ')" = "LC_ALL PATH PULLGATE_INSTANCE PULLGATE_RECIPE_DIR PWD " ] ||
	err "the setup of the recipe did not run with only its four variables: $(cat /var/lib/pullgate-loop/setup-env 2>/dev/null)"
pullgate status "$name" | grep -q '^recipe: *example-org/example-recipe v1.0.0 ' || err "pullgate status does not show the recipe"

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

step "4. What bootstrap made: users, directories, units"
want /etc/pullgate "root root 755"
want /var/spool/pullgate "root root 755"
want /var/lib/pullgate "root root 711"
id pullgate >/dev/null || die "bootstrap made no user pullgate"
id "$check_user" >/dev/null || die "bootstrap made no user $check_user"
grep -q '^Enabled and started: ' "$boot_out" || die "bootstrap did not enable the units"
for u in $(pullgate units | python3 -I -c 'import json, sys; print(" ".join(json.load(sys.stdin)["units"]))'); do
	cmp -s "/etc/systemd/system/$u" "$top/contrib/systemd/$u" || err "/etc/systemd/system/$u is not the unit of contrib/systemd/"
done

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

step "5b. The recipe's apply runs recipe fetch and recipe use"
make_recipe 1.0.1
printf '1.0.1\n' >"$work/recipe-version"
git -C "$work" add recipe-version
sha3="$(commit 'a = 1' 'move the recipe to 1.0.1')"
run_until_tip "$sha3"
i=0
# pullgate status shows it applied only after the whole apply returned:
# after recipe fetch and recipe use.
until [ "$(applied_sha)" = "$sha3" ]; do
	i=$((i + 1))
	[ "$i" -lt 90 ] || {
		journalctl --no-pager -o cat -u "pullgate-apply@$name.service" || true
		die "apply.sh did not apply $sha3"
	}
	sleep 1
done
rdir1=/usr/local/lib/pullgate/recipes/example-org/example-recipe/1.0.1
[ "$(cat "$out/recipe-dir")" = "$rdir1" ] || err "recipe fetch did not print the folder $rdir1"
pullgate status "$name" | grep -q '^recipe: *example-org/example-recipe v1.0.1 ' || err "after recipe use, pullgate status does not show 1.0.1"
grep -q -F "$rdir1/apply.sh" "/etc/pullgate/$name.toml" || err "recipe use did not switch the config to 1.0.1"
[ -d "$rdir0" ] || err "recipe use removed the folder of the version before"
pullgate status "$name"
# The last good commit is now this one.
sha1="$sha3"
listener_read_all "after the recipe update"

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
# The menu: set at the first start, not again after a restart (the same).
[ "$(grep -c -x setMyCommands "$tglog" || true)" = 1 ] || err "the listener must set the command menu once, at its first start"
[ "$(grep -c -x getMyCommands "$tglog" || true)" -ge 2 ] || err "the listener must read the command menu at each start"

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

step "9. Every unit that bootstrap enables"
units="$(pullgate units | python3 -I -c 'import json, sys; print("\n".join(sorted(json.load(sys.stdin)["enable"])))' | sed "s/NAME/$name/g")"
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
doctor_cmds="$(sh "$here/readme-block.sh" doctor "$top/README.md")" || die "README.md has no install-test:doctor block"
doctor_out="$(printf '%s\n' "$doctor_cmds" | sh -eu 2>&1)" || {
	printf '%s\n' "$doctor_out"
	die "pullgate doctor found a problem (above)"
}
printf '%s\n' "$doctor_out"
printf '%s\n' "$doctor_out" | grep -q '^All checks passed\.$' || err "pullgate doctor did not say: All checks passed."
if printf '%s\n' "$doctor_out" | grep -q '^PROBLEM'; then
	err "pullgate doctor printed a PROBLEM line"
fi
# A note is no problem, but here every check must be ok: only the note
# that Telegram was not checked (no --telegram) is expected.
if printf '%s\n' "$doctor_out" | grep '^note' | grep -v '^note *telegram: not checked'; then
	err "pullgate doctor printed a note (above); on this host every check must be ok"
fi

if [ "$fail" -ne 0 ]; then
	exit 1
fi
echo
echo "ok: the README install and a full loop work under the units of contrib/systemd/"
