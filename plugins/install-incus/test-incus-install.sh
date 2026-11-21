#!/bin/sh
umask 022
set -eu

# Repo-local, unprivileged regression harness for the Incus install step's
# release logic in plugins/install-incus/incus-install.sh (the --provision step of
# scripts/install-chasm.sh).
#
# The harness sources the real library and drives install_incus against a
# PATH-shimmed apt-get and os-release fixtures, so no root, no apt, and no
# host state is involved:
#
#   1. with INCUS_INSTALL_NEEDED=0 the step is a no-op: no approval, no
#      apt-get call;
#   2. trixie (Debian 13) installs incus virtiofsd from the release's own
#      repositories: no -t bookworm-backports anywhere;
#   3. a release newer than trixie (Debian 14) behaves the same way;
#   4. bookworm (Debian 12) with a bookworm-backports source installs with
#      -t bookworm-backports;
#   5. bookworm without a bookworm-backports source refuses BEFORE the
#      approval and before any apt-get call, and the refusal explains how
#      to add the source;
#   6. an older release (bullseye, Debian 11) refuses with a clear
#      message, again before any approval or apt-get call;
#   7. an os-release without VERSION_ID falls back to the codename
#      (trixie -> native install);
#   8. an os-release without VERSION_ID and an unknown codename (sid)
#      refuses;
#   9. a commented-out bookworm-backports .list line is NOT a configured
#      source (nor is a deb-src-only line): bookworm refuses before any
#      approval or apt-get call;
#  10. a Deb822 .sources stanza with Suites: bookworm-backports but
#      Enabled: no is NOT a configured source: bookworm refuses;
#  11. an enabled Deb822 stanza (Suites naming bookworm-backports, no
#      Enabled: no) IS a configured source: bookworm approves and installs
#      with -t bookworm-backports;
#  12. apt-cache policy output naming the suite confirms the source even
#      when the file scan finds no enabled entry;
#  13. a Debian derivative (ID=ubuntu, ID_LIKE=debian, VERSION_ID=22) is
#      refused before any approval or apt-get call: 0.1.0 supports Debian
#      12 and 13 only, and a derivative's version number is not Debian's.
#
# Usage: plugins/install-incus/test-incus-install.sh   (repository root; no arguments)

[ "$#" -eq 0 ] || { echo "usage: $0" >&2; exit 2; }

ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)
LIB=$ROOT/plugins/install-incus/incus-install.sh

die() {
    echo "test-incus-install: FAIL: $1" >&2
    exit 1
}

# --- harness-provided step dependencies ------------------------------------
# The installer provides fail and approve; the harness mirrors their
# contracts: fail exits 1, approve records the step text it was asked to
# approve (the tests assert the approval prompt stays accurate).
fail() {
    echo "install: $*" >&2
    exit 1
}

approve() {
    printf 'approve: %s\n' "$1" >>"$APPROVE_LOG"
    echo "approved via harness: $1"
}

WORK=$(mktemp -d)
trap 'rm -rf -- "$WORK"' EXIT HUP INT TERM
APPROVE_LOG=$WORK/state/approve.log
APT_LOG=$WORK/state/apt.log
export APT_LOG
export OS_RELEASE_FILE=$WORK/os-release
export APT_SOURCES_ROOT=$WORK/apt

# shellcheck source=plugins/install-incus/incus-install.sh
. "$LIB"

mkdir -p "$WORK/bin" "$WORK/state" "$WORK/apt/sources.list.d"
export PATH="$WORK/bin:$PATH"

# --- fake apt-get -----------------------------------------------------------
# Every invocation is logged (arguments between angle brackets) so the
# tests can assert the exact apt command line per release.
cat >"$WORK/bin/apt-get" <<'STUB'
#!/bin/sh
set -eu
: "${APT_LOG:?APT_LOG unset}"
{
    printf 'apt-get'
    for argument in "$@"; do printf ' <%s>' "$argument"; done
    printf '\n'
} >>"$APT_LOG"
exit 0
STUB
chmod +x "$WORK/bin/apt-get"

# --- fake apt-cache ----------------------------------------------------------
# The backports detector prefers apt-cache policy as its live confirmation,
# so the harness pins it (the host's real apt-cache must never leak in):
# policy output is empty unless APT_CACHE_SHOWS_BACKPORTS=1.
cat >"$WORK/bin/apt-cache" <<'STUB'
#!/bin/sh
if [ "${1:-}" = policy ]; then
    if [ "${APT_CACHE_SHOWS_BACKPORTS:-0}" = 1 ]; then
        printf '500 http://deb.debian.org/debian bookworm-backports/main amd64 Packages\n'
    fi
    exit 0
fi
echo "fake apt-cache: unsupported invocation: $*" >&2
exit 1
STUB
chmod +x "$WORK/bin/apt-cache"

# --- fixtures ---------------------------------------------------------------
write_os_release() { # version_id codename
    if [ -n "$1" ]; then
        printf 'ID=debian\nVERSION_ID=%s\nVERSION_CODENAME=%s\n' "$1" "$2" \
            >"$OS_RELEASE_FILE"
    else
        printf 'ID=debian\nVERSION_CODENAME="%s"\n' "$2" >"$OS_RELEASE_FILE"
    fi
}

write_os_release_id() { # id version_id codename id_like
    {
        printf 'ID=%s\n' "$1"
        [ -n "$2" ] && printf 'VERSION_ID=%s\n' "$2"
        printf 'VERSION_CODENAME=%s\n' "$3"
        [ -n "$4" ] && printf 'ID_LIKE=%s\n' "$4"
    } >"$OS_RELEASE_FILE"
}

add_backports_source() {
    printf 'deb http://deb.debian.org/debian bookworm-backports main\n' \
        >"$APT_SOURCES_ROOT/sources.list.d/backports.list"
}

reset_apt_sources() {
    rm -f "$APT_SOURCES_ROOT/sources.list.d/"*.list "$APT_SOURCES_ROOT/sources.list.d/"*.sources
    APT_CACHE_SHOWS_BACKPORTS=0
    export APT_CACHE_SHOWS_BACKPORTS
}

# run_install: run install_incus in a subshell, capturing status, combined
# output, the approval log, and the apt-get log.
run_install() {
    INCUS_INSTALL_NEEDED=${INCUS_INSTALL_NEEDED:-1}
    : >"$APPROVE_LOG"
    : >"$APT_LOG"
    status=0
    output=$(install_incus 2>&1) || status=$?
    approvals=$(cat "$APPROVE_LOG" 2>/dev/null || true)
    apt_calls=$(cat "$APT_LOG" 2>/dev/null || true)
}

assert_contains() { # haystack needle label
    case "$1" in
        *"$2"*) ;;
        *) die "$3: missing '$2' in:
$1" ;;
    esac
}

assert_absent() { # haystack needle label
    case "$1" in
        *"$2"*) die "$3: unexpected '$2' in:
$1" ;;
    esac
}

assert_no_apt_or_approval() { # label
    [ -z "$apt_calls" ] || die "$1: unexpected apt-get call(s):
$apt_calls"
    [ -z "$approvals" ] || die "$1: unexpected approval(s):
$approvals"
}

# 1. Incus already present: the step is a no-op.
scenario_not_needed() {
    INCUS_INSTALL_NEEDED=0
    write_os_release 12 bookworm
    run_install
    [ "$status" -eq 0 ] || die "not-needed failed: status=$status
$output"
    assert_no_apt_or_approval "not-needed"
    unset INCUS_INSTALL_NEEDED
}

# 2. trixie: the release's own repositories, no backports pin.
scenario_trixie() {
    write_os_release 13 trixie
    run_install
    [ "$status" -eq 0 ] || die "trixie failed: status=$status
$output"
    assert_contains "$approvals" \
        "install Incus from this Debian release's own repositories (apt-get update && apt-get install incus virtiofsd)" \
        "trixie approval"
    assert_contains "$apt_calls" "apt-get <update>" "trixie apt-get update"
    assert_contains "$apt_calls" \
        "apt-get <install> <-y> <incus> <virtiofsd>" "trixie apt-get install"
    assert_absent "$apt_calls" "bookworm-backports" "trixie apt-get call"
}

# 3. A release newer than trixie behaves like trixie.
scenario_newer_release() {
    write_os_release 14 forky
    run_install
    [ "$status" -eq 0 ] || die "newer release failed: status=$status
$output"
    assert_contains "$apt_calls" \
        "apt-get <install> <-y> <incus> <virtiofsd>" "newer release apt-get install"
    assert_absent "$apt_calls" "bookworm-backports" "newer release apt-get call"
}

# 4. bookworm with a backports source: the pinned suite.
scenario_bookworm_with_backports() {
    write_os_release 12 bookworm
    add_backports_source
    run_install
    [ "$status" -eq 0 ] || die "bookworm+backports failed: status=$status
$output"
    assert_contains "$approvals" \
        "install Incus from Debian bookworm-backports (apt-get update && apt-get install -t bookworm-backports incus virtiofsd)" \
        "bookworm approval"
    assert_contains "$apt_calls" "apt-get <update>" "bookworm apt-get update"
    assert_contains "$apt_calls" \
        "apt-get <install> <-y> <-t> <bookworm-backports> <incus> <virtiofsd>" \
        "bookworm apt-get install"
}

# 5. bookworm without a backports source: refused before any side effect,
#    with instructions for adding the source.
scenario_bookworm_without_backports() {
    write_os_release 12 bookworm
    reset_apt_sources
    run_install
    [ "$status" -eq 1 ] || die "bookworm without backports must refuse: status=$status
$output"
    assert_contains "$output" \
        "installing Incus on Debian 12 (bookworm) needs the bookworm-backports apt source" \
        "bookworm refusal"
    assert_contains "$output" \
        "bookworm-backports.list" \
        "bookworm refusal how-to"
    assert_no_apt_or_approval "bookworm refusal"
}

# 6. An older release: no supported source at all.
scenario_older_release() {
    write_os_release '"11"' bullseye
    run_install
    [ "$status" -eq 1 ] || die "older release must refuse: status=$status
$output"
    assert_contains "$output" \
        "no supported Incus source for this Debian release" \
        "older release refusal"
    assert_no_apt_or_approval "older release refusal"
}

# 7. No VERSION_ID: the codename decides (trixie -> native).
scenario_codename_fallback_trixie() {
    write_os_release '' trixie
    run_install
    [ "$status" -eq 0 ] || die "codename fallback trixie failed: status=$status
$output"
    assert_contains "$apt_calls" \
        "apt-get <install> <-y> <incus> <virtiofsd>" "codename fallback install"
    assert_absent "$apt_calls" "bookworm-backports" "codename fallback apt-get call"
}

# 8. No VERSION_ID and an unknown codename (sid): refused.
scenario_codename_fallback_sid() {
    write_os_release '' sid
    run_install
    [ "$status" -eq 1 ] || die "codename fallback sid must refuse: status=$status
$output"
    assert_contains "$output" \
        "no supported Incus source for this Debian release" \
        "sid refusal"
    assert_no_apt_or_approval "sid refusal"
}

# 9. bookworm with only a commented-out (or deb-src-only) backports line:
#    not a configured source, so the step refuses before any side effect.
scenario_bookworm_commented_list() {
    write_os_release 12 bookworm
    reset_apt_sources
    printf '# deb http://deb.debian.org/debian bookworm-backports main\n' \
        >"$APT_SOURCES_ROOT/sources.list.d/backports.list"
    run_install
    [ "$status" -eq 1 ] || die "bookworm with only a commented backports line must refuse: status=$status
$output"
    assert_contains "$output" \
        "installing Incus on Debian 12 (bookworm) needs the bookworm-backports apt source" \
        "commented-list refusal"
    assert_no_apt_or_approval "commented-list refusal"

    printf 'deb-src http://deb.debian.org/debian bookworm-backports main\n' \
        >"$APT_SOURCES_ROOT/sources.list.d/backports.list"
    run_install
    [ "$status" -eq 1 ] || die "bookworm with only a deb-src backports line must refuse: status=$status
$output"
    assert_contains "$output" "needs the bookworm-backports apt source" "deb-src refusal"
    assert_no_apt_or_approval "deb-src refusal"
    reset_apt_sources
}

# 10. bookworm with a disabled Deb822 stanza: not a configured source.
scenario_bookworm_disabled_deb822() {
    write_os_release 12 bookworm
    reset_apt_sources
    cat >"$APT_SOURCES_ROOT/sources.list.d/backports.sources" <<'EOF'
Types: deb
URIs: http://deb.debian.org/debian
Suites: bookworm-backports
Components: main
Enabled: no
EOF
    run_install
    [ "$status" -eq 1 ] || die "bookworm with a disabled deb822 stanza must refuse: status=$status
$output"
    assert_contains "$output" "needs the bookworm-backports apt source" "disabled-deb822 refusal"
    assert_no_apt_or_approval "disabled-deb822 refusal"
    reset_apt_sources
}

# 11. bookworm with an enabled Deb822 stanza: the pinned suite installs.
#     (The stanza omits Types, so it relies on the deb822 default of
#     deb deb-src — binary packages are provided.)
scenario_bookworm_deb822_enabled() {
    write_os_release 12 bookworm
    reset_apt_sources
    cat >"$APT_SOURCES_ROOT/sources.list.d/backports.sources" <<'EOF'
URIs: http://deb.debian.org/debian
Suites: bookworm bookworm-backports
Components: main
Signed-By: /usr/share/keyrings/debian-archive-keyring.gpg
EOF
    run_install
    [ "$status" -eq 0 ] || die "bookworm with an enabled deb822 stanza failed: status=$status
$output"
    assert_contains "$apt_calls" \
        "apt-get <install> <-y> <-t> <bookworm-backports> <incus> <virtiofsd>" \
        "deb822-enabled apt-get install"

    # A Types: deb-src-only stanza ships no binaries, so it is not a
    # configured source either.
    cat >"$APT_SOURCES_ROOT/sources.list.d/backports.sources" <<'EOF'
Types: deb-src
URIs: http://deb.debian.org/debian
Suites: bookworm-backports
Components: main
EOF
    run_install
    [ "$status" -eq 1 ] || die "bookworm with a deb-src-only deb822 stanza must refuse: status=$status
$output"
    assert_contains "$output" "needs the bookworm-backports apt source" "deb822 deb-src refusal"
    assert_no_apt_or_approval "deb822 deb-src refusal"
    reset_apt_sources
}

# 12. apt-cache policy naming the suite confirms the source even when no
#     enabled file entry exists.
scenario_bookworm_apt_cache_confirms() {
    write_os_release 12 bookworm
    reset_apt_sources
    APT_CACHE_SHOWS_BACKPORTS=1
    export APT_CACHE_SHOWS_BACKPORTS
    run_install
    [ "$status" -eq 0 ] || die "bookworm with apt-cache confirmation failed: status=$status
$output"
    assert_contains "$apt_calls" \
        "apt-get <install> <-y> <-t> <bookworm-backports> <incus> <virtiofsd>" \
        "apt-cache-confirmed apt-get install"
    reset_apt_sources
}

# 13. A Debian derivative is not Debian: refused before any side effect,
#     whatever its VERSION_ID says.
scenario_derivative_refused() {
    write_os_release_id ubuntu 22 jammy debian
    reset_apt_sources
    add_backports_source
    run_install
    [ "$status" -eq 1 ] || die "a Debian derivative must refuse: status=$status
$output"
    assert_contains "$output" "chasm 0.1.0 supports Debian 12 (bookworm) and 13 (trixie) only" \
        "derivative refusal"
    assert_contains "$output" "ID=ubuntu" "derivative refusal names the ID"
    assert_no_apt_or_approval "derivative refusal"
    reset_apt_sources
}

scenario_not_needed
scenario_trixie
scenario_newer_release
scenario_bookworm_with_backports
scenario_bookworm_without_backports
scenario_older_release
scenario_codename_fallback_trixie
scenario_codename_fallback_sid
scenario_bookworm_commented_list
scenario_bookworm_disabled_deb822
scenario_bookworm_deb822_enabled
scenario_bookworm_apt_cache_confirms
scenario_derivative_refused

echo "test-incus-install: ok (13 scenarios)"
