#!/bin/sh
# A fresh install with the units of contrib/systemd/, and the first run.
#
# It runs `pullgate run` under the real pullgate-run@.service and
# `pullgate listen` under the real pullgate-listen.service, with no
# drop-in. Then it checks the owners and modes of DESIGN.md section 4, and
# that the listener read the outbox and sent the question. So a unit
# setting that breaks a run fails here (issue #19).
#
# It changes the system: it installs /usr/local/bin/pullgate, adds users,
# and writes to /etc and /var. Run it only as root, on a machine you throw
# away, such as a CI runner.
#
# Usage: sudo sh test/units/first-run.sh PATH-TO-PULLGATE-BINARY
set -eu

if [ "$(id -u)" -ne 0 ]; then
	echo "first-run.sh: run it as root, on a machine you throw away" >&2
	exit 1
fi
if [ $# -ne 1 ] || [ ! -f "$1" ]; then
	echo "usage: first-run.sh PATH-TO-PULLGATE-BINARY" >&2
	exit 64
fi

here="$(cd "$(dirname "$0")" && pwd)"
top="$(cd "$here/../.." && pwd)"
name=config-repo
port=18443
fail=0

err() {
	echo "FAIL: $*" >&2
	fail=1
}

# The binary, the users and the directories, as in the README.
install -m 0755 -o root -g root "$1" /usr/local/bin/pullgate
id pullgate >/dev/null 2>&1 || useradd --system --no-create-home --user-group --shell /usr/sbin/nologin pullgate
id pullgate-check >/dev/null 2>&1 || useradd --system --no-create-home --user-group --shell /usr/sbin/nologin pullgate-check
install -d -m 0755 -o root -g root /etc/pullgate
install -d -m 0711 -o root -g root /var/lib/pullgate
install -m 0644 -o root -g root "$top"/contrib/systemd/* /etc/systemd/system/

# A local repo with one commit, owned by root.
src=/srv/pullgate-units-test
rm -rf "$src"
install -d -m 0755 -o root -g root "$src"
git init -q --bare "$src/$name.git"
git -C "$src/$name.git" symbolic-ref HEAD refs/heads/main
work="$(mktemp -d)"
printf 'a = 1\n' >"$work/policy.toml"
git -C "$work" init -q
git -C "$work" checkout -q -b main
git -C "$work" add policy.toml
git -C "$work" -c user.name=test -c user.email=test@example.com commit -q -m "first commit"
git -C "$work" push -q "$src/$name.git" main
rm -rf "$work"

cat >"/etc/pullgate/$name.toml" <<EOF
mode = "ask"
host = "example-host"

[repo]
url = "$src/$name.git"
branch = "main"

[commands]
apply = ["/usr/bin/true"]
check_user = "pullgate-check"

[watch]
paths = ["policy.toml"]

[listener]
user = "pullgate"
EOF
chown root:root "/etc/pullgate/$name.toml"
chmod 0600 "/etc/pullgate/$name.toml"

# The first run, under the real unit. A oneshot unit: start waits for it.
systemctl daemon-reload
if ! systemctl start "pullgate-run@$name.service"; then
	err "the first run failed under pullgate-run@.service"
fi
inv="$(systemctl show -p InvocationID --value "pullgate-run@$name.service")"
journalctl --no-pager -o cat "_SYSTEMD_INVOCATION_ID=$inv" || true
if journalctl --no-pager -o cat "_SYSTEMD_INVOCATION_ID=$inv" | grep -q 'level=ERROR'; then
	err "the first run logged an error"
fi

# The owners and modes of DESIGN.md section 4.
want() {
	got="$(stat -c '%U %G %a' "$1" 2>/dev/null || echo missing)"
	if [ "$got" != "$2" ]; then
		err "$1: got '$got', want '$2'"
	fi
}
spool="/var/spool/pullgate/$name"
want /var/lib/pullgate "root root 711"
want "/var/lib/pullgate/$name" "root root 711"
want "/var/lib/pullgate/$name/repo.git" "root root 700"
want "/var/lib/pullgate/$name/state.json" "root root 600"
want /var/spool/pullgate "root root 755"
want "$spool" "root root 755"
want "$spool/outbox" "root pullgate 770"
want "$spool/inbox" "pullgate pullgate 700"
want "$spool/check" "pullgate pullgate 700"
n=0
for f in "$spool"/outbox/*.json; do
	[ -e "$f" ] || continue
	n=$((n + 1))
	want "$f" "root pullgate 640"
done
if [ "$n" -ne 1 ]; then
	err "the outbox holds $n messages after the first run, want 1 (the question)"
fi

# The listener, under the real unit, against a fake Telegram on 127.0.0.1.
printf '123:units-test-token-made-by-the-script\n' >/etc/pullgate/bot-token
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
python3 -I "$here/faketg.py" "$port" "$tglog" &
tgpid=$!
trap 'kill "$tgpid" 2>/dev/null || true' EXIT
sleep 1
systemctl start pullgate-listen.service

# The listener sends the question, then removes the file from the outbox.
i=0
while [ "$i" -lt 30 ]; do
	if grep -qx sendMessage "$tglog" && [ -z "$(ls -A "$spool/outbox")" ]; then
		break
	fi
	i=$((i + 1))
	sleep 1
done
journalctl --no-pager -o cat -u pullgate-listen.service || true
if ! grep -qx sendMessage "$tglog"; then
	err "the listener sent no message"
fi
if [ -n "$(ls -A "$spool/outbox")" ]; then
	err "the listener did not remove the message from the outbox"
fi
if journalctl --no-pager -o cat -u pullgate-listen.service | grep -q 'level=ERROR'; then
	err "the listener logged an error"
fi
if ! systemctl is-active --quiet pullgate-listen.service; then
	err "the listener is not running"
fi
systemctl stop pullgate-listen.service

if [ "$fail" -ne 0 ]; then
	exit 1
fi
echo "ok: the first run and the listener work under the units of contrib/systemd/"
