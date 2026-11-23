#!/bin/sh
# Each unit that pullgate bootstrap enables must have an [Install]
# section with WantedBy=. Without it, `systemctl enable` does nothing, and
# the unit does not start after a reboot (issue #20).
#
# Usage: sh test/units/install-sections.sh   (from the top of the repo)
set -eu

# pullgate bootstrap enables the units of Enable in embed.go (issue #49),
# and pullgate update enables them again after an update.
units="$(sed -n '/^var Enable = \[\]string{$/,/^}$/p' embed.go \
	| grep -oE '"pullgate-[A-Za-z0-9@._-]+\.(service|path|timer|socket)"' \
	| tr -d '"' | sort -u || true)"
# Enable has 6 units today. Fewer than 4 means the parsing broke.
n="$(echo "$units" | grep -c . || true)"
if [ "$n" -lt 4 ]; then
	echo "FAIL: found $n units in Enable of embed.go, want at least 4. Fix: check embed.go, or this script" >&2
	exit 1
fi
fail=0
for u in $units; do
	# pullgate-run@NAME.timer is an instance of pullgate-run@.timer.
	f="contrib/systemd/$(echo "$u" | sed -E 's/@[^.]+\./@./')"
	if [ ! -f "$f" ]; then
		echo "FAIL: embed.go enables $u, but $f does not exist" >&2
		fail=1
		continue
	fi
	# The [Install] section and a WantedBy= line in it.
	if ! awk '
		/^\[/ { sec = $0; next }
		sec == "[Install]" && /^WantedBy=[^[:space:]]/ { ok = 1 }
		END { exit ok ? 0 : 1 }
	' "$f"; then
		echo "FAIL: $f has no [Install] section with WantedBy=. Fix: add one, so that systemctl enable starts $u at boot" >&2
		fail=1
		continue
	fi
	echo "ok: $u ($f) has [Install] with WantedBy="
done
exit "$fail"
