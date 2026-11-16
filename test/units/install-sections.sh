#!/bin/sh
# Each unit that the README tells you to enable must have an [Install]
# section with WantedBy=. Without it, `systemctl enable` does nothing, and
# the unit does not start after a reboot (issue #20).
#
# Usage: sh test/units/install-sections.sh   (from the top of the repo)
set -eu

# Lines joined at a trailing backslash; "sudo" and options before
# "enable" are allowed.
units="$(sed -e ':a' -e '/\\$/N; s/\\\n//; ta' README.md \
	| grep -E '^[[:space:]]*(sudo[[:space:]]+)?systemctl([[:space:]]+--?[A-Za-z-]+)*[[:space:]]+(re)?enable[[:space:]]' \
	| sed -e 's/#.*//' \
	| tr -s ' \t' '\n' \
	| grep -E '^pullgate-[A-Za-z0-9@._-]+\.(service|path|timer|socket)$' \
	| sort -u || true)"
# The README enables 4 units today. Fewer means the parsing broke.
n="$(echo "$units" | grep -c . || true)"
if [ "$n" -lt 4 ]; then
	echo "FAIL: found $n 'systemctl enable' units in README.md, want at least 4. Fix: check the README, or this script" >&2
	exit 1
fi

fail=0
for u in $units; do
	# pullgate-run@NAME.timer is an instance of pullgate-run@.timer.
	f="contrib/systemd/$(echo "$u" | sed -E 's/@[^.]+\./@./')"
	if [ ! -f "$f" ]; then
		echo "FAIL: README.md enables $u, but $f does not exist" >&2
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
