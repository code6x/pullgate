#!/bin/sh
# install.sh installs pullgate from a release on GitHub, or updates it.
#
# Run it as root on the host itself. README.md (section "Install") shows
# how to get and run it, DESIGN.md (section 16) says what it does, and
# THREAT-MODEL.md (T17) says what it trusts.
#
#   sh install.sh [--version X.Y.Z] [--bin-dir DIR] [--gh PATH] [--reinstall]
#
# It installs nothing unless every check passes:
#   - only root can change the bin directory, the old file, the temporary
#     directory and the gh of --gh (when the script does not run as root,
#     the user who runs it may too);
#   - the release has the tag vX.Y.Z of the version;
#   - the SHA-256 of the tarball is the one in SHA256SUMS of the release;
#   - gh attestation verify, with the bundle file of the release, says that
#     the release workflow of code6x/pullgate on main built the tarball, on
#     a runner that GitHub hosts;
#   - the new pullgate says that it is the version asked for. The
#     attestation proves which workflow built a file, not its version, so
#     this check stops an older build that was published under a newer
#     name.
#
# All the work is in functions, and only the last line starts it. So if a
# download stops in the middle, nothing runs.
#
# Exit codes: 0 done or nothing to do, 1 failure, 64 wrong usage.

set -eu

# The gh that checks the attestation when --gh is not given, and the
# SHA-256 of its tarball for each CPU. The SHA-256s are the lines of
# gh_2.102.0_checksums.txt of that release, and both tarballs passed
# "gh attestation verify --repo cli/cli" on 2026-10-17. Change the version
# and both SHA-256s together, and check them in the same way.
gh_version=2.102.0
gh_sha256_amd64=bb766f710eef8ede859c18578c72c327597cd4c8a85b06001b1f3843c6019386
gh_sha256_arm64=7862c86c72f43df3a2d93ddde6f473285b4e2af61b494849846827e513ef6484

# The oldest gh that --gh may name. Older versions have known bugs in
# gh attestation verify (GHSA-fgw4-v983-mgp8, GHSA-8xvp-7hj6-mcj9). Newer
# advisories (GHSA-mm27-mwq9-fr5g, GHSA-wjmr-j3rp-mh2g, GHSA-4mq3-hpgx-9cx8)
# are about --signer-repo, --signer-workflow and --source-ref only, which
# this script does not use.
gh_min_version=2.93.0

repo=code6x/pullgate

# Only the release workflow on main may have built the tarball. This is
# the identity of docs/releasing.md, "Verify a download".
cert_identity=https://github.com/code6x/pullgate/.github/workflows/release.yml@refs/heads/main

# As root, the script runs only the programs of these directories.
root_path=/usr/sbin:/usr/bin:/sbin:/bin

# Time limits, in seconds, of the programs that the script runs, other
# than curl (it has its own). timeout sends TERM, and KILL 10 seconds
# later.
max_run=60     # pullgate version, gh --version
max_verify=300 # gh attestation verify: it gets the trusted root of Sigstore online

# Size limits of the downloads, in bytes. An older curl checks only the
# size that the server announces.
max_small=1048576 # 1 MiB: the answer of the API, SHA256SUMS, the bundle
max_big=67108864  # 64 MiB: the tarballs of pullgate and of gh

usage() {
	cat <<EOF
Usage: sh install.sh [--version X.Y.Z] [--bin-dir DIR] [--gh PATH] [--reinstall]

Install pullgate from a release on GitHub, or update it. Run it as root on
the host itself.

  --version X.Y.Z  install this version, also one that is older than the
                   installed one (default: the newest release)
  --bin-dir DIR    install pullgate as DIR/pullgate (default /usr/local/bin)
  --gh PATH        check the attestation with this gh, $gh_min_version or newer,
                   which only root can change (default: download gh
                   $gh_version and check its SHA-256)
  --reinstall      install also when this version is installed already,
                   or when the installed pullgate has an unknown version
  --help           print this help

Before it installs anything, it checks the SHA-256 and the build
attestation of the release, and the version of the new pullgate. It
restarts nothing.

Exit codes: 0 done or nothing to do, 1 failure, 64 wrong usage.
EOF
}

# say prints a line for the person.
say() {
	printf '%s\n' "$*"
}

# die prints the last line of a failed run and exits with 1. The old file
# is replaced only in the very last step, so nothing is installed then.
die() {
	printf 'pullgate: %s; nothing was installed (exit 1)\n' "$*" >&2
	exit 1
}

# usage_error prints the usage, and the last line says why the usage is
# wrong. It exits with 64.
usage_error() {
	usage >&2
	printf 'pullgate: %s (exit 64)\n' "$*" >&2
	exit 64
}

# cleanup removes the temporary directory and a new file that was not
# renamed yet. It runs at every exit, also after a failure. It must return
# 0: under set -e, a failed trap would change the exit code.
cleanup() {
	if [ -n "$tmp_target" ]; then
		rm -f "$tmp_target" || :
	fi
	if [ -n "$tmp" ]; then
		rm -rf "$tmp" || :
	fi
	return 0
}

# on_signal ends the script after a signal. Its last line says whether the
# new file was already in place.
on_signal() {
	if [ -n "$renamed" ]; then
		printf 'pullgate: stopped by a signal after the new file was installed as %s; run %s version to check it (exit 1)\n' "$target" "$target" >&2
		exit 1
	fi
	if [ -n "$renaming" ]; then
		printf 'pullgate: stopped by a signal during the rename; the new file may be installed as %s; run %s version to check it (exit 1)\n' "$target" "$target" >&2
		exit 1
	fi
	die "stopped by a signal; run the script again"
}

# check_tools stops early when timeout is not the one of GNU coreutils
# (busybox timeout has no --foreground). Later errors would else point the
# wrong way, for example to an unknown installed version.
check_tools() {
	timeout --foreground 10 true >/dev/null 2>&1 ||
		die "timeout --foreground does not work; install GNU coreutils and run the script again"
}

# clean_env removes the variables that change what the programs of the
# script do in a way that is not safe here, sets LC_ALL=C, and sets uid.
# As root, PATH becomes root_path, so that only the programs of the system
# run. The variables:
#   - TAR_OPTIONS: options for tar, such as --to-command or
#     --checkpoint-action=exec, which run commands as root;
#   - GZIP: options for gzip;
#   - POSIXLY_CORRECT: changes how the GNU programs read their arguments;
#   - CURL_HOME: where curl finds .curlrc (download gives curl -q too);
#   - CURL_CA_BUNDLE, SSL_CERT_FILE, SSL_CERT_DIR: the TLS certificates
#     that curl and gh trust;
#   - SSLKEYLOGFILE: a file that curl writes the TLS keys to;
#   - OPENSSL_CONF, OPENSSL_MODULES, OPENSSL_ENGINES: the config of
#     OpenSSL, which curl reads at its start, and the places of the
#     modules it loads. A config can name any library to load as a
#     provider;
#   - LD_PRELOAD, LD_LIBRARY_PATH, LD_AUDIT, GCONV_PATH: code that each
#     program loads;
#   - LD_DEBUG, LD_DEBUG_OUTPUT, LD_PROFILE, LD_PROFILE_OUTPUT,
#     GLIBC_TUNABLES, MALLOC_TRACE, MALLOC_CHECK_, MALLOC_PERTURB_: files
#     that the loader and libc write as root, and how they work;
#   - HOSTALIASES, RES_OPTIONS, LOCALDOMAIN: how names are looked up;
#   - GODEBUG: how gh, a Go program, works (for example its TLS);
#   - GH_DEBUG, GH_PAGER, PAGER, GH_BROWSER, BROWSER: what gh prints and
#     runs;
#   - CDPATH and IFS: how cd finds a directory and how the shell splits
#     words.
# The proxy variables (https_proxy and the others) stay: some hosts need a
# proxy, and TLS and the attestation protect what comes through it.
# Variables that act before the first line runs (for example SHELLOPTS
# when sh is bash) cannot be removed here. To start with a clean
# environment, run: env -i PATH=$root_path sh install.sh
clean_env() {
	unset TAR_OPTIONS GZIP POSIXLY_CORRECT CURL_HOME CURL_CA_BUNDLE SSL_CERT_FILE SSL_CERT_DIR \
		SSLKEYLOGFILE OPENSSL_CONF OPENSSL_MODULES OPENSSL_ENGINES \
		LD_PRELOAD LD_LIBRARY_PATH LD_AUDIT GCONV_PATH \
		LD_DEBUG LD_DEBUG_OUTPUT LD_PROFILE LD_PROFILE_OUTPUT GLIBC_TUNABLES MALLOC_TRACE MALLOC_CHECK_ MALLOC_PERTURB_ \
		HOSTALIASES RES_OPTIONS LOCALDOMAIN GODEBUG GH_DEBUG GH_PAGER PAGER GH_BROWSER BROWSER CDPATH IFS
	LC_ALL=C
	export LC_ALL
	uid=$(
		PATH=$root_path
		export PATH
		id -u
	) || die "cannot run id -u; check that $root_path has the program id"
	case $uid in
	'' | *[!0-9]*) die "id -u does not print a user ID; check the program id in $root_path" ;;
	esac
	owners=root
	if [ "$uid" = 0 ]; then
		PATH=$root_path
		export PATH
	else
		owners="root or user $uid"
	fi
}

# is_number reports whether $1 is a number of at most 9 digits, without a
# leading zero.
is_number() {
	case $1 in
	0) return 0 ;;
	'' | 0* | *[!0-9]* | ??????????*) return 1 ;;
	esac
	return 0
}

# is_version reports whether $1 is a version X.Y.Z: three numbers, and
# nothing else.
is_version() {
	case $1 in
	*.*.*.* | *[!0-9.]*) return 1 ;;
	*.*.*) ;;
	*) return 1 ;;
	esac
	v_rest=${1#*.}
	is_number "${1%%.*}" && is_number "${v_rest%%.*}" && is_number "${v_rest#*.}"
}

# compare_versions sets cmp to -1, 0 or 1 when the version $1 is older
# than, the same as, or newer than the version $2. Both must pass
# is_version, so each part is a small number.
compare_versions() {
	cmp_a=$1
	cmp_b=$2
	cmp=0
	for _ in 1 2 3; do
		if [ "${cmp_a%%.*}" -lt "${cmp_b%%.*}" ]; then
			cmp=-1
			return 0
		fi
		if [ "${cmp_a%%.*}" -gt "${cmp_b%%.*}" ]; then
			cmp=1
			return 0
		fi
		cmp_a=${cmp_a#*.}
		cmp_b=${cmp_b#*.}
	done
}

# real_dir sets real to the path of the directory $1 without links, . or
# .. in it. The script then uses this path, so a link that someone changes
# later does not matter.
real_dir() {
	real=$(cd -P -- "$1" && pwd -P) || die "cannot find the directory $1; check the path"
}

# check_entry checks that only $owners can change the path $1: its owner
# must be one of them, and group and others must not be able to write to
# it. With $2 "shared", a directory may let them write when it has the
# sticky bit, as /tmp does: then they cannot rename or remove what is not
# theirs in it.
check_entry() {
	ce_stat=$(stat -c '%u %a' "$1") || die "cannot read the owner and the mode of $1; check that it exists and that stat works"
	ce_owner=${ce_stat%% *}
	ce_mode=${ce_stat#* }
	case $ce_owner$ce_mode in
	'' | *[!0-9]*) die "cannot read the owner and the mode of $1; check that stat is GNU stat" ;;
	esac
	if [ "$ce_owner" != 0 ] && [ "$ce_owner" != "$uid" ]; then
		die "$1 is owned by user $ce_owner; it must be owned by $owners (fix it with chown, or use another path)"
	fi
	if [ "$((0$ce_mode & 022))" != 0 ]; then
		if [ "$2" != shared ] || [ ! -d "$1" ] || [ "$((0$ce_mode & 01000))" = 0 ]; then
			die "group or others can write to $1; only $owners may (fix it with chmod go-w, or use another path)"
		fi
	fi
}

# check_tree checks the absolute path $1 as check_entry does, with $2, and
# each directory above it, up to /, as shared. $1 must have no link, . or
# .. in it (real_dir).
check_tree() {
	ct_path=$1
	check_entry "$ct_path" "$2"
	while [ "$ct_path" != / ]; do
		ct_path=${ct_path%/*}
		if [ -z "$ct_path" ]; then
			ct_path=/
		fi
		check_entry "$ct_path" shared
	done
}

# parse_args reads the arguments into want_version, bin_dir, gh and
# reinstall.
parse_args() {
	while [ "$#" -gt 0 ]; do
		case $1 in
		--version)
			if [ "$#" -lt 2 ]; then
				usage_error "--version needs a version, such as 0.2.0"
			fi
			if ! is_version "$2"; then
				usage_error "--version must be X.Y.Z, such as 0.2.0"
			fi
			want_version=$2
			shift 2
			;;
		--bin-dir)
			if [ "$#" -lt 2 ]; then
				usage_error "--bin-dir needs a directory"
			fi
			bin_dir=$2
			shift 2
			;;
		--gh)
			if [ "$#" -lt 2 ]; then
				usage_error "--gh needs the path of gh"
			fi
			gh=$2
			shift 2
			;;
		--reinstall)
			reinstall=1
			shift
			;;
		--help | -h)
			usage
			exit 0
			;;
		*)
			usage_error "unknown argument: $1"
			;;
		esac
	done
	case $bin_dir in
	/*) ;;
	*) usage_error "--bin-dir must be an absolute path" ;;
	esac
	case $gh in
	'' | /*) ;;
	*) usage_error "--gh must be an absolute path" ;;
	esac
}

# check_platform checks that this is Linux on amd64 or arm64.
check_platform() {
	os=$(uname -s) || die "cannot run uname -s; check that $root_path has the program uname"
	if [ "$os" != Linux ]; then
		die "pullgate runs on Linux only, and this is $os; run this on a Linux host"
	fi
	machine=$(uname -m) || die "cannot run uname -m; check that $root_path has the program uname"
	case $machine in
	x86_64 | amd64)
		arch=amd64
		gh_sha256=$gh_sha256_amd64
		;;
	aarch64 | arm64)
		arch=arm64
		gh_sha256=$gh_sha256_arm64
		;;
	*)
		die "pullgate has releases for amd64 and arm64 only, and this machine is $machine; build it from the source (README.md, section Install)"
		;;
	esac
}

# check_paths checks, before anything is downloaded, the places that the
# script runs programs from and writes to: the bin directory, the old
# file, and the gh of --gh. The script runs the old file and the gh, and
# as root it must not run a file that another user can change. A link as
# the old file or as the gh is refused: name the file itself.
check_paths() {
	if [ ! -d "$bin_dir" ]; then
		die "$bin_dir is not a directory; make it, or give another --bin-dir"
	fi
	real_dir "$bin_dir"
	bin_dir=$real
	check_tree "$bin_dir" own
	if [ ! -w "$bin_dir" ]; then
		die "cannot write to $bin_dir; run this as root"
	fi
	target=$bin_dir/pullgate
	if [ -L "$target" ]; then
		die "$target is a link; the script replaces only a file (remove the link, or give another --bin-dir)"
	fi
	if [ -e "$target" ]; then
		if [ ! -f "$target" ]; then
			die "$target is not a file; remove it, or give another --bin-dir"
		fi
		check_entry "$target" own
	fi
	if [ -n "$gh" ]; then
		if [ -L "$gh" ] || [ ! -f "$gh" ] || [ ! -x "$gh" ]; then
			die "--gh $gh is not a program file; name the file itself, not a link"
		fi
		gh_dir=${gh%/*}
		real_dir "${gh_dir:-/}"
		gh=${real%/}/${gh##*/}
		check_tree "$gh" own
	fi
}

# make_tmp makes the private temporary directory in TMPDIR, or /tmp.
# TMPDIR, and each directory above it, must be safe as check_tree says:
# else another user could swap a checked file in it for another one. The
# new directory itself must have the owner and mode 0700 that mktemp
# gives. The trap of main removes it.
make_tmp() {
	real_dir "${TMPDIR:-/tmp}"
	tmp_parent=$real
	check_tree "$tmp_parent" shared
	tmp=$(mktemp -d "$tmp_parent/pullgate-install.XXXXXXXX") ||
		die "cannot make a temporary directory in $tmp_parent; set TMPDIR to a directory that only root can write, such as /root"
	check_entry "$tmp" own
	tmp_fix="set TMPDIR to a directory that only root can write and where programs can run, such as /root"
	mkdir "$tmp/home" "$tmp/gh" "$tmp/new" || die "cannot make directories in $tmp; check the free space in $tmp_parent"
}

# download gets the URL $1 into the file $2, at most $3 bytes: HTTPS only,
# also after a redirect, with TLS 1.2 or newer, with retries and a time
# limit. An HTTP error is a failure. -q comes first, so curl reads no
# .curlrc.
download() {
	curl -q --proto '=https' --proto-redir '=https' --tlsv1.2 --fail --silent --show-error --location \
		--retry 3 --connect-timeout 30 --max-time 600 --max-filesize "$3" --output "$2" "$1" </dev/null
}

# limited runs the program $2 with its arguments, at most $1 seconds.
# --foreground keeps it in the process group of the script, so Ctrl-C and
# a signal to the group reach it.
limited() {
	l_max=$1
	shift
	timeout --foreground -k 10 "$l_max" "$@"
}

# sha256_of sets file_sha256 to the SHA-256 of the file $1, in hex.
sha256_of() {
	file_sha256=$(sha256sum <"$1") || die "cannot compute the SHA-256 of $1; check that sha256sum works"
	file_sha256=${file_sha256%% *}
}

# run_gh runs gh with no login and an empty config: no token and no host
# from the environment, and the home, the config and the XDG directories
# in the temporary directory. It turns off the update notices, the prompts
# and the telemetry of gh.
run_gh() {
	(
		unset GH_TOKEN GITHUB_TOKEN GH_HOST GH_ENTERPRISE_TOKEN GITHUB_ENTERPRISE_TOKEN
		HOME=$tmp/home
		GH_CONFIG_DIR=$tmp/home/.config/gh
		XDG_CONFIG_HOME=$tmp/home/.config
		XDG_CACHE_HOME=$tmp/home/.cache
		XDG_DATA_HOME=$tmp/home/.local/share
		XDG_STATE_HOME=$tmp/home/.local/state
		GH_NO_UPDATE_NOTIFIER=1
		GH_NO_EXTENSION_UPDATE_NOTIFIER=1
		GH_PROMPT_DISABLED=1
		GH_TELEMETRY=false
		export HOME GH_CONFIG_DIR XDG_CONFIG_HOME XDG_CACHE_HOME XDG_DATA_HOME XDG_STATE_HOME \
			GH_NO_UPDATE_NOTIFIER GH_NO_EXTENSION_UPDATE_NOTIFIER GH_PROMPT_DISABLED GH_TELEMETRY
		exec timeout --foreground -k 10 "$gh_max" "$gh" "$@" </dev/null
	)
}

# choose_version sets version: the one of --version, or the newest
# release. It asks the GitHub API for the release, and its tag must be
# vX.Y.Z of that version: the files of the release are found by the tag.
choose_version() {
	if [ -n "$want_version" ]; then
		say "Asking GitHub for the release v$want_version of pullgate."
		download "https://api.github.com/repos/$repo/releases/tags/v$want_version" "$tmp/release.json" "$max_small" ||
			die "cannot get the release v$want_version from GitHub; check the version at https://github.com/$repo/releases"
	else
		say "Asking GitHub for the newest release of pullgate."
		download "https://api.github.com/repos/$repo/releases/latest" "$tmp/release.json" "$max_small" ||
			die "cannot ask GitHub for the newest release; check the network, or give --version"
	fi
	tag=$(sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$tmp/release.json") ||
		die "cannot read the answer of GitHub; check that sed works"
	version=${tag#v}
	if [ "v$version" != "$tag" ] || ! is_version "$version"; then
		die "GitHub does not name the release in the form vX.Y.Z; try again later, or give --version X.Y.Z"
	fi
	if [ -n "$want_version" ] && [ "$version" != "$want_version" ]; then
		die "GitHub names the release v$want_version as $tag; check the version at https://github.com/$repo/releases"
	fi
}

# check_installed reads the version of the installed pullgate, and stops
# when there is nothing to do. A build from the source says "pullgate
# dev": its version is unknown, and only --reinstall replaces it. Only the
# text that the installed file prints counts here: the installed file is
# not checked.
check_installed() {
	installed=
	if [ ! -e "$target" ]; then
		say "Installing pullgate $version as $target."
		return 0
	fi
	if [ -x "$target" ]; then
		installed_out=$(limited "$max_run" "$target" version </dev/null 2>/dev/null) || installed_out=
		installed=${installed_out#pullgate }
		if [ "pullgate $installed" != "$installed_out" ] || ! is_version "$installed"; then
			installed=
		fi
	fi
	if [ -z "$installed" ]; then
		if [ -z "$reinstall" ]; then
			die "$target has an unknown version (a build from the source, or it does not run); to replace it with pullgate $version, run this again with --reinstall"
		fi
		say "$target has an unknown version. pullgate $version will replace it, as --reinstall asks."
		return 0
	fi
	compare_versions "$version" "$installed"
	case $cmp in
	0)
		if [ -z "$reinstall" ]; then
			say "$target says that it is pullgate $version already. Nothing to do."
			say "This did not check the installed file. To replace it with a checked copy, run this again with --reinstall."
			exit 0
		fi
		say "Installing pullgate $version again as $target, as --reinstall asks."
		;;
	1)
		say "Updating $target from pullgate $installed to pullgate $version."
		;;
	*)
		if [ -z "$want_version" ]; then
			say "$target says that it is pullgate $installed, which is newer than $version, the newest release. Nothing to do."
			say "This did not check the installed file. To install $version anyway, run this again with --version $version."
			exit 0
		fi
		say "This is a downgrade: replacing pullgate $installed with the older pullgate $version, as --version asks."
		;;
	esac
}

# download_release gets the tarball for this machine, SHA256SUMS and the
# bundle of the build attestation.
download_release() {
	tarball=pullgate_${version}_linux_${arch}.tar.gz
	bundle=pullgate_${version}.sigstore.json
	base=https://github.com/$repo/releases/download/v$version
	say "Downloading $tarball, SHA256SUMS and $bundle."
	download "$base/$tarball" "$tmp/$tarball" "$max_big" || die "cannot download $base/$tarball; check the network and try again"
	download "$base/SHA256SUMS" "$tmp/SHA256SUMS" "$max_small" || die "cannot download $base/SHA256SUMS; check the network and try again"
	download "$base/$bundle" "$tmp/$bundle" "$max_small" || die "cannot download $base/$bundle; check the network and try again"
}

# check_sha256 checks the SHA-256 of the tarball against its own line in
# SHA256SUMS. There must be exactly one such line. It finds a broken
# download early. It proves nothing about who built the file: whoever can
# publish a release can publish its SHA256SUMS too. The attestation proves
# that.
check_sha256() {
	want=
	while read -r line_sum line_name || [ -n "$line_sum" ]; do
		if [ "$line_name" = "$tarball" ] || [ "$line_name" = "*$tarball" ]; then
			if [ -n "$want" ]; then
				die "SHA256SUMS of the release has more than one line for $tarball; do not use this release, and report it at https://github.com/$repo/issues"
			fi
			want=$line_sum
		fi
	done <"$tmp/SHA256SUMS"
	if [ -z "$want" ]; then
		die "SHA256SUMS of the release has no line for $tarball; do not use this release, and report it at https://github.com/$repo/issues"
	fi
	sha256_of "$tmp/$tarball"
	if [ "$file_sha256" != "$want" ]; then
		die "the SHA-256 of $tarball is not the one in SHA256SUMS; the download is broken or changed, try again later"
	fi
}

# get_gh sets gh to the gh that checks the attestation: the one of --gh,
# which check_paths checked, or gh $gh_version, downloaded and checked
# against the SHA-256 in this script. It must be $gh_min_version or newer:
# the first line of gh --version must be "gh version X.Y.Z", then a space
# or nothing, so a version such as 2.93.0-rc.1 does not count.
get_gh() {
	gh_hint=" (check that $gh runs here and is gh $gh_min_version or newer)"
	if [ -z "$gh" ]; then
		gh_hint=" (if programs cannot run in $tmp_parent, use --gh, or $tmp_fix)"
		gh_tarball=gh_${gh_version}_linux_${arch}.tar.gz
		say "Downloading gh $gh_version to check the attestation."
		download "https://github.com/cli/cli/releases/download/v$gh_version/$gh_tarball" "$tmp/$gh_tarball" "$max_big" ||
			die "cannot download $gh_tarball; check the network, or give a gh with --gh"
		sha256_of "$tmp/$gh_tarball"
		if [ "$file_sha256" != "$gh_sha256" ]; then
			die "the SHA-256 of $gh_tarball is not the one in this script; give a gh with --gh"
		fi
		tar --no-same-owner -xzf "$tmp/$gh_tarball" -C "$tmp/gh" "gh_${gh_version}_linux_${arch}/bin/gh" </dev/null ||
			die "cannot take gh out of $gh_tarball; check the free space in $tmp_parent, or give a gh with --gh"
		gh=$tmp/gh/gh_${gh_version}_linux_${arch}/bin/gh
		if [ ! -f "$gh" ] || [ ! -x "$gh" ]; then
			die "$gh_tarball holds no program gh; give a gh with --gh"
		fi
	fi
	gh_max=$max_run
	if ! gh_out=$(run_gh --version); then
		die "cannot run $gh --version$gh_hint"
	fi
	case $gh_out in
	"gh version "*) gh_have=${gh_out#gh version } ;;
	*) gh_have= ;;
	esac
	gh_have=${gh_have%%[[:space:]]*}
	if ! is_version "$gh_have"; then
		die "cannot tell the version of $gh; it must be gh $gh_min_version or newer (give another gh with --gh, or leave out --gh)"
	fi
	compare_versions "$gh_have" "$gh_min_version"
	if [ "$cmp" = -1 ]; then
		die "$gh is gh $gh_have; it must be gh $gh_min_version or newer (update it, or leave out --gh)"
	fi
}

# verify_attestation checks the build attestation of the tarball with the
# bundle of the release. It proves that the release workflow of
# code6x/pullgate on main built this exact file, on a runner that GitHub
# hosts. With --bundle, gh needs no login. It gets the trusted root of
# Sigstore online.
verify_attestation() {
	say "Checking the build attestation of $tarball with gh $gh_have."
	gh_max=$max_verify
	if ! run_gh attestation verify "$tmp/$tarball" --bundle "$tmp/$bundle" \
		--repo "$repo" --cert-identity "$cert_identity" --deny-self-hosted-runners; then
		die "gh attestation verify failed or ran longer than $max_verify seconds, so it is not proven that the release workflow built $tarball; read the output of gh above: after a network error, try again; else do not use this release, and report it at https://github.com/$repo/issues"
	fi
	say "The attestation is valid: the release workflow of $repo on main built this file."
}

# check_new_version runs the file $1 with "version". It must print
# "pullgate $version", and nothing else. $2 is added to the error when the
# file does not run.
check_new_version() {
	new_out=$(limited "$max_run" "$1" version </dev/null) || die "the new pullgate does not run, or runs longer than $max_run seconds$2"
	if [ "$new_out" != "pullgate $version" ]; then
		die "the new pullgate does not say that it is pullgate $version; do not use this release, and report it at https://github.com/$repo/issues"
	fi
}

# unpack takes only the file pullgate out of the tarball, and checks its
# version, in the temporary directory. The attestation passed already, so
# only a file that the release workflow built runs here.
unpack() {
	tar --no-same-owner -xzf "$tmp/$tarball" -C "$tmp/new" pullgate </dev/null ||
		die "cannot take pullgate out of $tarball; check the free space in $tmp_parent, or report it at https://github.com/$repo/issues"
	if [ ! -f "$tmp/new/pullgate" ] || [ -L "$tmp/new/pullgate" ]; then
		die "$tarball holds no file pullgate; do not use this release, and report it at https://github.com/$repo/issues"
	fi
	chmod 0700 "$tmp/new/pullgate" || die "cannot make the new pullgate a program; check that chmod works in $tmp_parent"
	check_new_version "$tmp/new/pullgate" " (if programs cannot run in $tmp_parent, $tmp_fix)"
}

# install_new copies the new pullgate into the directory of the old one,
# under a new name, and checks again that it says it is pullgate
# $version. Then it renames it over the old file. A rename in one
# directory replaces the file in one step: whoever starts pullgate gets
# the old file or the new one, never a part of one.
install_new() {
	tmp_target=$(mktemp "$bin_dir/.pullgate.XXXXXXXX") || die "cannot make a new file in $bin_dir; check the free space and that $bin_dir is not read-only"
	install -m 0755 "$tmp/new/pullgate" "$tmp_target" || die "cannot copy pullgate into $bin_dir; check the free space in $bin_dir"
	if [ "$uid" = 0 ]; then
		chown 0:0 "$tmp_target" || die "cannot make $tmp_target owned by root; check that chown works in $bin_dir"
	fi
	check_new_version "$tmp_target" " (check that programs can run in $bin_dir)"
	# Write the new file to the disk before the rename, so that a power
	# loss cannot leave an empty file under the old name. sync with a file
	# needs coreutils 8.24 or newer; else sync writes all files.
	sync -- "$tmp_target" 2>/dev/null || sync || die "cannot write $tmp_target to the disk; check the disk"
	renaming=1
	mv -f "$tmp_target" "$target" || die "cannot rename the new file to $target; check that $bin_dir is not read-only"
	tmp_target=
	renamed=1
	sync -- "$bin_dir" 2>/dev/null || sync || :
}

# print_restart prints what the new file says, and what to restart. It
# restarts nothing.
print_restart() {
	if done_out=$(limited "$max_run" "$target" version </dev/null); then
		say "Done. $target version says: $done_out"
	fi
	say "This script restarts nothing. The listener still runs the old version. Restart it:"
	say "  systemctl restart pullgate-listen.service"
	say "The timers (pullgate-run@NAME.timer) and the path units (pullgate-apply@NAME.path, pullgate-check@NAME.path)"
	say "start a new process each time, so they use the new file from their next start."
	say "A run or an apply that is running now ends with the old version. See them with:"
	say "  systemctl list-timers 'pullgate-run@*'"
	say "Compare the units in contrib/systemd of the new version with yours, and"
	say "run systemctl daemon-reload after you change a unit."
}

main() {
	bin_dir=/usr/local/bin
	want_version=
	gh=
	reinstall=
	tmp=
	tmp_target=
	renaming=
	renamed=
	trap cleanup EXIT
	# A signal ends the script through exit, so the EXIT trap runs too.
	trap on_signal HUP INT PIPE TERM

	clean_env
	parse_args "$@"
	check_platform
	check_tools
	check_paths
	make_tmp
	choose_version
	check_installed
	download_release
	check_sha256
	get_gh
	verify_attestation
	unpack
	install_new
	print_restart
}

main "$@"
