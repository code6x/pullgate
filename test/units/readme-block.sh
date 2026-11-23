#!/bin/sh
# The install commands of README.md, so CI runs them as written (issue #27).
#
# The README marks each install block with a comment line
# "<!-- install-test:NAME -->" just before its ```sh fence:
#   binary  install the binary (by hand)
#   doctor  pullgate doctor, the check of the setup (issue #28)
# pullgate bootstrap makes the users, the directories and the units, and
# enables them (issue #49): full-loop.sh runs it with a fake Telegram.
#
# Usage:
#   sh test/units/readme-block.sh NAME [README]
#       Print the commands of block NAME. Fails when the marker is missing,
#       is there twice, or is not followed by a non-empty ```sh block.
#   sh test/units/readme-block.sh --check [README]
#       Check that both blocks are there, and that no install command
#       (useradd, install -d, cp contrib/systemd, systemctl enable) of a
#       ```sh block is outside a marked block. Such a command would not
#       be run by CI.
set -eu

blocks="binary doctor"

usage() {
	echo "usage: readme-block.sh NAME [README] | readme-block.sh --check [README]" >&2
	exit 64
}

# block NAME FILE: print the lines of block NAME.
block() {
	awk -v name="$1" '
		BEGIN { marker = "<!-- install-test:" name " -->" }
		$0 == marker { n++; want = 1; next }
		want == 1 {
			if ($0 != "```sh") { bad = "the line after the marker is not ```sh"; exit }
			want = 2; next
		}
		want == 2 {
			if ($0 == "```") { want = 0; next }
			if (n == 1) { print; lines++ }
		}
		END {
			if (bad != "") { print "FAIL: " marker ": " bad > "/dev/stderr"; exit 1 }
			if (n != 1) { print "FAIL: found " marker " " n+0 " times, want 1" > "/dev/stderr"; exit 1 }
			if (want != 0) { print "FAIL: " marker ": the ```sh block has no end" > "/dev/stderr"; exit 1 }
			if (lines == 0) { print "FAIL: " marker ": the block is empty" > "/dev/stderr"; exit 1 }
		}
	' "$2"
}

[ $# -ge 1 ] || usage
if [ "$1" = --check ]; then
	readme="${2:-README.md}"
	for b in $blocks; do
		if ! block "$b" "$readme" >/dev/null; then
			echo "FAIL: $readme: the install-test:$b block is missing or broken. Fix: put \"<!-- install-test:$b -->\" on the line before its \`\`\`sh block" >&2
			exit 1
		fi
	done
	# Install commands in ```sh blocks that are not marked: CI would not
	# run them. The "Update from 0.1.0" steps use "reenable", which is
	# not matched here.
	# Every fenced block counts, indented or not, in any language.
	out="$(awk '
		/^<!-- install-test:[a-z]+ -->$/ { marked = 1; next }
		/^[[:space:]]*```/ {
			if (inblock) { inblock = 0; marked = 0 } else { inblock = 1 }
			next
		}
		inblock && !marked && /(^|[[:space:]])(useradd|groupadd|install -[a-zA-Z]|chown|chmod|cp contrib\/systemd|systemctl( --?[A-Za-z-]+)* enable)([[:space:]]|$)/ { print FILENAME ":" NR ": " $0 }
	' "$readme")"
	if [ -n "$out" ]; then
		echo "$out" >&2
		echo "FAIL: these install commands are in a \`\`\`sh block without an install-test marker, so CI does not run them. Fix: move them into a marked block (see test/units/readme-block.sh)" >&2
		exit 1
	fi
	echo "ok: $readme has the blocks $blocks, and no unmarked install command"
	exit 0
fi

case " $blocks " in
*" $1 "*) ;;
*) usage ;;
esac
block "$1" "${2:-README.md}"
