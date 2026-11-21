#!/bin/sh
umask 022
set -eu

# Repo-local, unprivileged regression harness for the neutral guest account
# the minimal template must carry (the packages layer creates it).
#
# The thin public cut removed the agent-provisioning layers that used to
# create the guest user as a side effect, so the sealed-template scrub failed
# at `stat /home/agent`. The packages layer now creates the account directly:
# name `agent`, uid and gid pinned to the site guest_uid, home /home/agent
# (agent:agent, mode 0750), login shell /usr/bin/zsh, no password, no
# supplementary groups, no sudo rights. This harness holds that contract:
#
#   1. the REAL guest apply fragment (extracted from
#      scripts/install-guest-packages.sh between the GUEST_SCRIPT heredoc
#      markers) executed against a fake guest (PATH shims for apt-get,
#      dpkg-query, install, stat, id, getent, groupadd, useradd; the three
#      guest absolute roots /var/lib/chasm, /home/agent, /usr/bin/zsh are
#      mechanically rewritten into the fake guest root before execution, and
#      the pristine literals are asserted separately) creates the account
#      with the pinned attributes (the useradd shim mirrors the real tool:
#      it refuses a missing --gid group and creates the home only with
#      --create-home), installs the neutral /home/agent/.zshrc
#      from guest-config/guest/zshrc (agent:agent, 0644, byte-identical, so
#      an interactive zsh never runs zsh-newuser-install), enables the
#      systemd-timesyncd unit the profile installs (so a resumed guest can
#      resynchronize its clock after a long pause) and fails closed when
#      that unit is not enabled, records guest_uid
#      and zshrc_sha256 in the receipt, is idempotent on re-run, fails closed
#      on a uid/gid drift, on a missing zsh, on an invalid uid token, and on
#      a pre-existing .zshrc that differs from the template input (never
#      overwritten);
#   2. the REAL guest_is_current fragment accepts the completed state and
#      rejects a state whose account is gone, whose .zshrc drifted, or whose
#      systemd-timesyncd unit is disabled;
#   3. the pending zshrc write never follows an agent-planted name: symlinks
#      planted at the old predictable /home/agent/.zshrc.pending.$$ path and
#      at the literal staging name of the current layer leave their sentinel
#      target untouched while the layer succeeds safely, and the install
#      step never sources the staged file from inside the agent-owned home;
#   4. wiring: the builder invokes the packages layer before phase=scrub,
#      passes the site guest uid into both guest invocations of the layer,
#      the scrub block requires agent:agent on /home/agent, the resumed
#      builder qualification probes the account and its .zshrc, the pending
#      build receipt records the site guest uid and the guest zshrc digest,
#      and `chasm shell` opens the same shell path and home the layer pins;
#   5. the resume requirement list, evaluated as the REAL extracted
#      production lines, accepts a receipt that binds the current inputs and
#      refuses a receipt whose guest zshrc digest or site guest uid differs,
#      or that predates the binding;
#   6. identity: the sealed-template identity resolved through the real
#      helper changes when the packages layer (which embeds the guest
#      account step) changes, when the guest zshrc input changes, and when
#      the site guest uid changes, while the format tag stays at the pinned
#      v6 value this input-set change established.
#
# Usage: scripts/test-guest-user-layer.sh   (repository root; no arguments)

[ "$#" -eq 0 ] || { echo "usage: $0" >&2; exit 2; }

ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
LAYER=$ROOT/scripts/install-guest-packages.sh
BUILDER=$ROOT/scripts/build-sandbox-template.sh
IDENTITY_LIB=$ROOT/scripts/lib/sandbox-template-identity.sh
GUEST_UID=4242
PROFILE=minimal
PKGS='ca-certificates,curl,git,jq,python3,python3-pip,python3-venv,sqlite3,systemd-timesyncd,tmux,universal-ctags,vim,zsh'
# The layer seals the mandatory core union of the chosen profile's packages
# (scripts/lib/core-package-union.sh); the fragments are therefore driven
# with the effective list, the raw profile list, and the chosen manifest
# digest, exactly as the layer passes them.
PROFILE_SHA=$(sha256sum "$ROOT/guest-config/profiles/$PROFILE.profile" | cut -d' ' -f1)
# shellcheck source=scripts/lib/core-package-union.sh
. "$ROOT/scripts/lib/core-package-union.sh"
EFFECTIVE_PKGS=$(union_core_packages "$PKGS")
GUEST_ZSHRC=$ROOT/guest-config/guest/zshrc
[ -f "$GUEST_ZSHRC" ] || { echo "test-guest-user-layer: FAIL: guest zshrc input is missing" >&2; exit 1; }
GUEST_ZSHRC_SUM=$(sha256sum "$GUEST_ZSHRC") || { echo "test-guest-user-layer: FAIL: guest zshrc input could not be digested" >&2; exit 1; }
ZSHRC_SHA=${GUEST_ZSHRC_SUM%% *}
ZSHRC_CONTENT=$(cat "$GUEST_ZSHRC")

die() {
    echo "test-guest-user-layer: FAIL: $1" >&2
    exit 1
}

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT HUP INT TERM
GUEST_BIN=$WORK/guest-bin
ORIG_PATH=$PATH
mkdir -p "$GUEST_BIN"

# --- fake guest tools ---------------------------------------------------------
# Every shim is deliberately minimal: it supports exactly the invocation
# shapes the layer fragments use and exits 9 on anything else, so a change
# in the production fragment cannot silently pass through an unimplemented
# code path.

cat >"$GUEST_BIN/apt-get" <<'STUB'
#!/bin/sh
set -eu
case "$1" in
    update|clean) exit 0 ;;
    install)
        shift
        [ "${1:-}" = "-y" ] && shift
        for pkg in "$@"; do printf '%s\n' "$pkg" >>"$GUEST_ROOT/installed.packages"; done
        exit 0
        ;;
esac
echo "apt-get shim: unsupported invocation: $*" >&2
exit 9
STUB

cat >"$GUEST_BIN/dpkg-query" <<'STUB'
#!/bin/sh
set -eu
while [ "$#" -gt 1 ]; do shift; done
if grep -Fx "$1" "$GUEST_ROOT/installed.packages" >/dev/null 2>&1; then
    echo installed
else
    echo not-installed
fi
STUB

cat >"$GUEST_BIN/install" <<'STUB'
#!/bin/sh
set -eu
owner=root group=root mode=0755 directory=0
if [ "${1:-}" = "-d" ]; then
    directory=1
    shift
fi
while [ "$#" -gt 1 ]; do
    case "$1" in
        -o) owner=$2; shift 2 ;;
        -g) group=$2; shift 2 ;;
        -m) mode=$2; shift 2 ;;
        --) shift; break ;;
        *) echo "install shim: unsupported invocation: $*" >&2; exit 9 ;;
    esac
done
if [ "$directory" = 1 ]; then
    [ "$#" -eq 1 ] || { echo "install shim: unsupported invocation: $*" >&2; exit 9; }
    mkdir -p -- "$1"
    chmod "$mode" -- "$1"
    printf '%s:%s\n' "$owner" "$group" >"$1.chasm-owner"
else
    [ "$#" -eq 2 ] || { echo "install shim: unsupported invocation: $*" >&2; exit 9; }
    # The layer must stage the pending zshrc outside the agent-owned home;
    # refuse any file-mode source under it so a regression back to staging
    # inside /home/agent fails the harness even when the planted pid range
    # below misses the fragment's pid.
    case "$1" in
        "$GUEST_ROOT"/home/agent|"$GUEST_ROOT"/home/agent/*)
            echo "install shim: refusing a staged source inside the agent home: $1" >&2
            exit 9
            ;;
    esac
    cp -- "$1" "$2"
    chmod "$mode" -- "$2"
    printf '%s:%s\n' "$owner" "$group" >"$2.chasm-owner"
fi
STUB

cat >"$GUEST_BIN/stat" <<'STUB'
#!/bin/sh
set -eu
[ "${1:-}" = "-c" ] || { echo "stat shim: unsupported invocation: $*" >&2; exit 9; }
fmt=$2
shift 2
for path in "$@"; do
    [ -e "$path" ] || { echo "stat: cannot statx '$path'" >&2; exit 1; }
    owner=root group=root
    if [ -f "$path.chasm-owner" ]; then
        owner=$(cut -d: -f1 "$path.chasm-owner")
        group=$(cut -d: -f2 "$path.chasm-owner")
    fi
    mode=$(find "$path" -maxdepth 0 -printf '%m')
    case "$fmt" in
        '%U:%G:%a') printf '%s:%s:%s\n' "$owner" "$group" "$mode" ;;
        '%U:%G') printf '%s:%s\n' "$owner" "$group" ;;
        '%a') printf '%s\n' "$mode" ;;
        *) echo "stat shim: unsupported format: $fmt" >&2; exit 9 ;;
    esac
done
STUB

cat >"$GUEST_BIN/id" <<'STUB'
#!/bin/sh
set -eu
case "${1:-}" in
    -u) field=3 ;;
    -g) field=4 ;;
    *) echo "id shim: unsupported invocation: $*" >&2; exit 9 ;;
esac
line=$(awk -F: -v n="$2" '$1 == n { print; exit }' "$GUEST_ROOT/etc-passwd")
[ -n "$line" ] || { echo "id: '$2': no such user" >&2; exit 1; }
printf '%s\n' "$line" | cut -d: -f"$field"
STUB

cat >"$GUEST_BIN/getent" <<'STUB'
#!/bin/sh
set -eu
case "${1:-}" in
    passwd) db=$GUEST_ROOT/etc-passwd ;;
    group) db=$GUEST_ROOT/etc-group ;;
    *) echo "getent shim: unsupported database: $1" >&2; exit 9 ;;
esac
line=$(awk -F: -v n="$2" '$1 == n { print; exit }' "$db")
[ -n "$line" ] || exit 2
printf '%s\n' "$line"
STUB

cat >"$GUEST_BIN/groupadd" <<'STUB'
#!/bin/sh
set -eu
[ "$1" = "--gid" ] || { echo "groupadd shim: unsupported invocation: $*" >&2; exit 9; }
if awk -F: -v n="$3" '$1 == n { f = 1 } END { exit !f }' "$GUEST_ROOT/etc-group"; then
    echo "groupadd shim: group '$3' already exists" >&2
    exit 9
fi
echo "groupadd $3" >>"$GUEST_ROOT/calls.log"
printf '%s:x:%s:\n' "$3" "$2" >>"$GUEST_ROOT/etc-group"
STUB

cat >"$GUEST_BIN/useradd" <<'STUB'
#!/bin/sh
set -eu
uid= gid= home=/home/defunct shell=/bin/sh create=0
while [ "$#" -gt 1 ]; do
    case "$1" in
        --uid) uid=$2; shift 2 ;;
        --gid) gid=$2; shift 2 ;;
        --home-dir) home=$2; shift 2 ;;
        --shell) shell=$2; shift 2 ;;
        --create-home) create=1; shift ;;
        *) echo "useradd shim: unsupported flag: $1" >&2; exit 9 ;;
    esac
done
name=$1
if awk -F: -v n="$name" '$1 == n { f = 1 } END { exit !f }' "$GUEST_ROOT/etc-passwd"; then
    echo "useradd shim: user '$name' already exists" >&2
    exit 9
fi
# Like the real useradd: --gid must name an existing group, and the home
# directory is created only with --create-home.
if [ -z "$gid" ] || ! awk -F: -v g="$gid" '$3 == g { f = 1 } END { exit !f }' "$GUEST_ROOT/etc-group"; then
    echo "useradd shim: group '$gid' does not exist" >&2
    exit 6
fi
echo "useradd $name uid=$uid gid=$gid create_home=$create shell=$shell" >>"$GUEST_ROOT/calls.log"
printf '%s:x:%s:%s::%s:%s\n' "$name" "$uid" "$gid" "$home" "$shell" >>"$GUEST_ROOT/etc-passwd"
[ "$create" = 1 ] || exit 0
if [ -e "$home" ]; then
    echo "useradd shim: warning: the home directory '$home' already exists; not creating it" >&2
    exit 0
fi
mkdir -- "$home"
chmod 0755 -- "$home"
printf '%s:%s\n' "$name" "$name" >"$home.chasm-owner"
STUB

chmod 0755 "$GUEST_BIN"/*

cat >"$GUEST_BIN/systemctl" <<'STUB'
#!/bin/sh
set -eu
case "${1:-}" in
    enable)
        [ "${2:-}" = "systemd-timesyncd" ] || { echo "systemctl shim: unsupported unit: ${2:-}" >&2; exit 9; }
        # SYSTEMCTL_ENABLE_FAIL=1 simulates the enable step failing (as it
        # would if the unit file were missing or masked) after all the
        # account and zshrc work has already succeeded.
        if [ "${SYSTEMCTL_ENABLE_FAIL:-0}" = 1 ]; then
            echo "systemctl shim: enable failed" >&2
            exit 1
        fi
        grep -Fxq "$2" "$GUEST_ROOT/enabled.units" 2>/dev/null ||
            printf '%s\n' "$2" >>"$GUEST_ROOT/enabled.units"
        exit 0
        ;;
    is-enabled)
        [ "${2:-}" = "systemd-timesyncd" ] || { echo "systemctl shim: unsupported unit: ${2:-}" >&2; exit 9; }
        # SYSTEMCTL_IS_ENABLED_DISABLED=1 simulates the postflight reading a
        # unit that is not enabled even though the enable step reported
        # success (the silent-partial case the postflight exists to catch).
        if [ "${SYSTEMCTL_IS_ENABLED_DISABLED:-0}" = 1 ]; then
            echo disabled
            exit 1
        fi
        if grep -Fxq "$2" "$GUEST_ROOT/enabled.units" 2>/dev/null; then
            echo enabled
        else
            echo disabled
            exit 1
        fi
        exit 0
        ;;
esac
echo "systemctl shim: unsupported invocation: $*" >&2
exit 9
STUB
chmod 0755 "$GUEST_BIN/systemctl"

# --- extract the production fragments ----------------------------------------

# The apply fragment: everything between the GUEST_SCRIPT heredoc markers.
awk -v q="'" '
    !on && index($0, "<<" q "GUEST_SCRIPT" q) > 0 { on = 1; next }
    on && $0 == "GUEST_SCRIPT" { on = 0; exit }
    on { print }
' "$LAYER" >"$WORK/apply-orig.sh" || die "could not scan the layer script"
[ -s "$WORK/apply-orig.sh" ] || die "could not extract the guest apply fragment"

# The currency fragment: the single-quoted sh -eu -c body of guest_is_current.
awk -v q="'" '
    !on && index($0, "-- sh -eu -c " q) > 0 { on = 1; next }
    on && index($0, q " sh ") > 0 && index($0, "BOOTSTRAP_PROFILE") > 0 { on = 0; exit }
    on { print }
' "$LAYER" >"$WORK/check-orig.sh" || die "could not scan the layer script"
[ -s "$WORK/check-orig.sh" ] || die "could not extract the guest currency fragment"

# The pristine fragments must pin the guest contract literals this harness
# rewires; the behavioral runs below therefore exercise the real logic.
for literal in /var/lib/chasm /home/agent /usr/bin/zsh; do
    grep -qF "$literal" "$WORK/apply-orig.sh" ||
        die "the apply fragment no longer references $literal"
done

# Rewrite the three guest absolute roots into the per-scenario fake root.
# The paths come from mktemp -d, so they carry no sed metacharacters.
rewrite_fragment() { # in out
    sed \
        -e "s|/var/lib/chasm|$GUEST_ROOT/var/lib/chasm|g" \
        -e "s|/home/agent|$GUEST_ROOT/home/agent|g" \
        -e "s|/usr/bin/zsh|$GUEST_ROOT/usr/bin/zsh|g" \
        "$1" >"$2"
}

new_guest() { # name
    GUEST_ROOT=$WORK/guest-$1
    export GUEST_ROOT
    rm -rf -- "$GUEST_ROOT"
    mkdir -p "$GUEST_ROOT/usr/bin" "$GUEST_ROOT/var/lib/chasm" "$GUEST_ROOT/home"
    : >"$GUEST_ROOT/etc-passwd"
    : >"$GUEST_ROOT/etc-group"
    : >"$GUEST_ROOT/calls.log"
    printf '#!/bin/sh\nexit 0\n' >"$GUEST_ROOT/usr/bin/zsh"
    chmod 0755 "$GUEST_ROOT/usr/bin/zsh"
}

run_apply() { # profile packages uid  (stdin: rewritten fragment)
    PATH="$GUEST_BIN:$ORIG_PATH" sh -s -- "$@"
}

run_check() { # profile packages uid
    PATH="$GUEST_BIN:$ORIG_PATH" sh -eu -c "$(cat "$WORK/check-run.sh")" sh "$@"
}

expect_fail() { # status label want-substring errfile
    [ "$1" -ne 0 ] || die "$2: expected a failure, got exit 0"
    case "$(cat "$4")" in
        *"$3"*) ;;
        *) die "$2: failure output does not mention '$3': $(cat "$4")" ;;
    esac
}

# --- 1. apply creates the pinned account, idempotently ------------------------

new_guest a
rewrite_fragment "$WORK/apply-orig.sh" "$WORK/apply-run.sh"
run_apply "$PROFILE" "$EFFECTIVE_PKGS" "$PKGS" "$PROFILE_SHA" "$GUEST_UID" "$ZSHRC_SHA" "$ZSHRC_CONTENT" <"$WORK/apply-run.sh" \
    >"$WORK/out" 2>"$WORK/err" ||
    die "fresh apply failed: $(cat "$WORK/err")"

agent_line=$(awk -F: -v n=agent '$1 == n { print; exit }' "$GUEST_ROOT/etc-passwd")
[ "$(printf '%s\n' "$agent_line" | cut -d: -f3)" = "$GUEST_UID" ] ||
    die "agent uid is not pinned to the site guest uid"
[ "$(printf '%s\n' "$agent_line" | cut -d: -f4)" = "$GUEST_UID" ] ||
    die "agent gid is not pinned to the site guest uid"
[ "$(printf '%s\n' "$agent_line" | cut -d: -f6)" = "$GUEST_ROOT/home/agent" ] ||
    die "agent home is not /home/agent"
[ "$(printf '%s\n' "$agent_line" | cut -d: -f7)" = "$GUEST_ROOT/usr/bin/zsh" ] ||
    die "agent login shell is not /usr/bin/zsh"
[ "$(awk -F: -v n=agent '$1 == n { c++ } END { print c + 0 }' "$GUEST_ROOT/etc-passwd")" = 1 ] ||
    die "the passwd database carries more than one agent entry"
[ "$(awk -F: -v n=agent '$1 == n { print; exit }' "$GUEST_ROOT/etc-group" | cut -d: -f3)" = "$GUEST_UID" ] ||
    die "agent group gid is not pinned to the site guest uid"

# The account creation must have gone through the real command sequence:
# groupadd for the pinned group and useradd with --create-home (the shim
# mirrors the real tool, so a layer that drops either step fails here).
grep -Fx "groupadd agent" "$GUEST_ROOT/calls.log" >/dev/null ||
    die "the layer did not create the agent group with groupadd"
grep -F "useradd agent uid=$GUEST_UID gid=$GUEST_UID create_home=1 shell=" "$GUEST_ROOT/calls.log" >/dev/null ||
    die "the layer did not create the agent account with --create-home and the pinned uid/gid"

guest_home=$GUEST_ROOT/home/agent
[ -d "$guest_home" ] && [ ! -L "$guest_home" ] ||
    die "agent home is missing or unsafe"
[ "$(PATH="$GUEST_BIN:$ORIG_PATH" stat -c %U:%G:%a "$guest_home")" = "agent:agent:750" ] ||
    die "agent home ownership or mode is wrong: expected agent:agent:750"

receipt=$GUEST_ROOT/var/lib/chasm/packages/$PROFILE/receipt
[ -f "$receipt" ] || die "receipt is missing"
grep -Fx "guest_uid=$GUEST_UID" "$receipt" >/dev/null ||
    die "receipt does not record the guest uid"
grep -Fx "zshrc_sha256=$ZSHRC_SHA" "$receipt" >/dev/null ||
    die "receipt does not record the guest zshrc digest"
grep -Fx "packages=$EFFECTIVE_PKGS" "$receipt" >/dev/null ||
    die "receipt does not record the effective package list"
grep -Fx "profile_packages=$PKGS" "$receipt" >/dev/null ||
    die "receipt does not record the raw chosen profile packages"
grep -Fx "manifest_sha256=$PROFILE_SHA" "$receipt" >/dev/null ||
    die "receipt does not record the chosen profile manifest digest"
grep -Fx result=passed "$receipt" >/dev/null ||
    die "receipt does not record a passed result"
[ "$(PATH="$GUEST_BIN:$ORIG_PATH" stat -c %U:%G:%a "$receipt")" = "root:root:600" ] ||
    die "receipt ownership or mode is wrong"
grep -Fx zsh "$GUEST_ROOT/installed.packages" >/dev/null ||
    die "zsh was not installed from the profile package list"
grep -Fx systemd-timesyncd "$GUEST_ROOT/installed.packages" >/dev/null ||
    die "systemd-timesyncd was not installed from the profile package list"
grep -Fx systemd-timesyncd "$GUEST_ROOT/enabled.units" >/dev/null ||
    die "the layer did not enable systemd-timesyncd"

# The neutral zshrc: present, safe, non-empty (so an interactive zsh skips
# zsh-newuser-install), byte-identical to the template input, and pinned to
# the account.
zshrc_file=$GUEST_ROOT/home/agent/.zshrc
[ -f "$zshrc_file" ] && [ ! -L "$zshrc_file" ] ||
    die "guest .zshrc is missing or unsafe"
[ -s "$zshrc_file" ] ||
    die "guest .zshrc is empty; zsh -i would run zsh-newuser-install"
cmp -s -- "$GUEST_ZSHRC" "$zshrc_file" ||
    die "guest .zshrc does not match the template input"
[ "$(PATH="$GUEST_BIN:$ORIG_PATH" stat -c %U:%G:%a "$zshrc_file")" = "agent:agent:644" ] ||
    die "guest .zshrc ownership or mode is wrong: expected agent:agent:644"

# Idempotent re-run: nothing recreated, receipt rewritten, account intact.
run_apply "$PROFILE" "$EFFECTIVE_PKGS" "$PKGS" "$PROFILE_SHA" "$GUEST_UID" "$ZSHRC_SHA" "$ZSHRC_CONTENT" <"$WORK/apply-run.sh" \
    >"$WORK/out" 2>"$WORK/err" ||
    die "idempotent re-apply failed: $(cat "$WORK/err")"
[ "$(grep -c useradd "$GUEST_ROOT/calls.log")" = 1 ] ||
    die "the re-run recreated the account"
[ "$(awk -F: -v n=agent '$1 == n { c++ } END { print c + 0 }' "$GUEST_ROOT/etc-passwd")" = 1 ] ||
    die "the re-run duplicated the passwd entry"
cmp -s -- "$GUEST_ZSHRC" "$zshrc_file" ||
    die "the re-run rewrote the guest .zshrc"

# The completed state satisfies the real currency fragment.
rewrite_fragment "$WORK/check-orig.sh" "$WORK/check-run.sh"
run_check "$PROFILE" "$EFFECTIVE_PKGS" "$PKGS" "$PROFILE_SHA" "$GUEST_UID" "$ZSHRC_SHA" ||
    die "guest_is_current rejected a completed guest state"

# --- 2. the layer fails closed on drift and bad inputs ------------------------

new_guest b
printf 'agent:x:1500:1500::%s/home/agent:%s/usr/bin/zsh\n' "$GUEST_ROOT" "$GUEST_ROOT" \
    >"$GUEST_ROOT/etc-passwd"
printf 'agent:x:1500:\n' >"$GUEST_ROOT/etc-group"
rewrite_fragment "$WORK/apply-orig.sh" "$WORK/apply-run.sh"
status=0
run_apply "$PROFILE" "$EFFECTIVE_PKGS" "$PKGS" "$PROFILE_SHA" "$GUEST_UID" "$ZSHRC_SHA" "$ZSHRC_CONTENT" <"$WORK/apply-run.sh" \
    >"$WORK/out" 2>"$WORK/err" || status=$?
expect_fail "$status" "uid drift" "different uid or gid" "$WORK/err"

new_guest c
rm -f "$GUEST_ROOT/usr/bin/zsh"
rewrite_fragment "$WORK/apply-orig.sh" "$WORK/apply-run.sh"
status=0
run_apply "$PROFILE" "$EFFECTIVE_PKGS" "$PKGS" "$PROFILE_SHA" "$GUEST_UID" "$ZSHRC_SHA" "$ZSHRC_CONTENT" <"$WORK/apply-run.sh" \
    >"$WORK/out" 2>"$WORK/err" || status=$?
expect_fail "$status" "missing zsh" "zsh is missing" "$WORK/err"

new_guest d
rewrite_fragment "$WORK/apply-orig.sh" "$WORK/apply-run.sh"
status=0
run_apply "$PROFILE" "$EFFECTIVE_PKGS" "$PKGS" "$PROFILE_SHA" 4x2 "$ZSHRC_SHA" "$ZSHRC_CONTENT" <"$WORK/apply-run.sh" \
    >"$WORK/out" 2>"$WORK/err" || status=$?
expect_fail "$status" "invalid uid token" "guest uid is invalid" "$WORK/err"

# A pre-existing .zshrc that differs from the template input is never
# overwritten: the layer fails closed so the drift is a deliberate decision.
new_guest f
mkdir -p "$GUEST_ROOT/home/agent"
printf 'PROMPT=%%#%%n custom\n' >"$GUEST_ROOT/home/agent/.zshrc"
rewrite_fragment "$WORK/apply-orig.sh" "$WORK/apply-run.sh"
status=0
run_apply "$PROFILE" "$EFFECTIVE_PKGS" "$PKGS" "$PROFILE_SHA" "$GUEST_UID" "$ZSHRC_SHA" "$ZSHRC_CONTENT" <"$WORK/apply-run.sh" \
    >"$WORK/out" 2>"$WORK/err" || status=$?
expect_fail "$status" "pre-existing drifted zshrc" "different content or attributes" "$WORK/err"
[ "$(cat "$GUEST_ROOT/home/agent/.zshrc")" = 'PROMPT=%#%n custom' ] ||
    die "the drifted pre-existing .zshrc was overwritten"

# The currency fragment rejects a guest whose account is gone: rebuild the
# completed state, then drop the passwd entry.
new_guest e
rewrite_fragment "$WORK/apply-orig.sh" "$WORK/apply-run.sh"
run_apply "$PROFILE" "$EFFECTIVE_PKGS" "$PKGS" "$PROFILE_SHA" "$GUEST_UID" "$ZSHRC_SHA" "$ZSHRC_CONTENT" <"$WORK/apply-run.sh" \
    >"$WORK/out" 2>"$WORK/err" ||
    die "apply for the currency-drift scenario failed: $(cat "$WORK/err")"
: >"$GUEST_ROOT/etc-passwd"
rewrite_fragment "$WORK/check-orig.sh" "$WORK/check-run.sh"
status=0
run_check "$PROFILE" "$EFFECTIVE_PKGS" "$PKGS" "$PROFILE_SHA" "$GUEST_UID" "$ZSHRC_SHA" >"$WORK/out" 2>"$WORK/err" || status=$?
[ "$status" -ne 0 ] ||
    die "guest_is_current accepted a guest whose account is gone"

# The currency fragment also rejects a guest whose .zshrc drifted after the
# layer completed.
new_guest g
rewrite_fragment "$WORK/apply-orig.sh" "$WORK/apply-run.sh"
run_apply "$PROFILE" "$EFFECTIVE_PKGS" "$PKGS" "$PROFILE_SHA" "$GUEST_UID" "$ZSHRC_SHA" "$ZSHRC_CONTENT" <"$WORK/apply-run.sh" \
    >"$WORK/out" 2>"$WORK/err" ||
    die "apply for the zshrc-drift scenario failed: $(cat "$WORK/err")"
printf 'HISTSIZE=1\n' >"$GUEST_ROOT/home/agent/.zshrc"
status=0
run_check "$PROFILE" "$EFFECTIVE_PKGS" "$PKGS" "$PROFILE_SHA" "$GUEST_UID" "$ZSHRC_SHA" >"$WORK/out" 2>"$WORK/err" || status=$?
[ "$status" -ne 0 ] ||
    die "guest_is_current accepted a guest whose .zshrc drifted"

# The currency fragment also rejects a guest whose time-sync unit was
# disabled after the layer completed: a sealed template must never freeze a
# guest whose clock cannot recover after a long pause.
new_guest t
rewrite_fragment "$WORK/apply-orig.sh" "$WORK/apply-run.sh"
run_apply "$PROFILE" "$EFFECTIVE_PKGS" "$PKGS" "$PROFILE_SHA" "$GUEST_UID" "$ZSHRC_SHA" "$ZSHRC_CONTENT" <"$WORK/apply-run.sh" \
    >"$WORK/out" 2>"$WORK/err" ||
    die "apply for the timesync-drift scenario failed: $(cat "$WORK/err")"
: >"$GUEST_ROOT/enabled.units"
rewrite_fragment "$WORK/check-orig.sh" "$WORK/check-run.sh"
status=0
run_check "$PROFILE" "$EFFECTIVE_PKGS" "$PKGS" "$PROFILE_SHA" "$GUEST_UID" "$ZSHRC_SHA" >"$WORK/out" 2>"$WORK/err" || status=$?
[ "$status" -ne 0 ] ||
    die "guest_is_current accepted a guest whose systemd-timesyncd is disabled"

# The apply fragment fails closed when the time-sync unit cannot be enabled:
# a sealed template must never ship with a clock that cannot recover after a
# long pause. First negative case: `systemctl enable` exits non-zero after
# the account and zshrc work succeeded — the layer must fail with the
# enable-failure message, not report success.
new_guest ts1
rewrite_fragment "$WORK/apply-orig.sh" "$WORK/apply-run.sh"
status=0
SYSTEMCTL_ENABLE_FAIL=1 run_apply "$PROFILE" "$EFFECTIVE_PKGS" "$PKGS" "$PROFILE_SHA" "$GUEST_UID" "$ZSHRC_SHA" "$ZSHRC_CONTENT" \
    <"$WORK/apply-run.sh" >"$WORK/out" 2>"$WORK/err" || status=$?
expect_fail "$status" "timesync enable failure" "systemd-timesyncd could not be enabled" "$WORK/err"

# Second negative case: `systemctl enable` succeeds but `is-enabled` reports
# the unit disabled — the silent-partial case — and the postflight check must
# fail the layer with the postflight message.
new_guest ts2
rewrite_fragment "$WORK/apply-orig.sh" "$WORK/apply-run.sh"
status=0
SYSTEMCTL_IS_ENABLED_DISABLED=1 run_apply "$PROFILE" "$EFFECTIVE_PKGS" "$PKGS" "$PROFILE_SHA" "$GUEST_UID" "$ZSHRC_SHA" "$ZSHRC_CONTENT" \
    <"$WORK/apply-run.sh" >"$WORK/out" 2>"$WORK/err" || status=$?
expect_fail "$status" "timesync postflight failure" "guest packages postflight failed: systemd-timesyncd is not enabled" "$WORK/err"

# --- 3. a planted symlink never redirects a root write -----------------------

# The old layer staged the pending zshrc as /home/agent/.zshrc.pending.$$,
# a predictable path inside the agent-owned home: an agent could plant a
# symlink there so the root redirection overwrites an arbitrary target.
# Plant symlinks at the whole plausible pid range of that name and at the
# literal staging name of the current layer, point them at a sentinel, and
# run the real apply fragment on a fresh guest: the layer must succeed
# safely, the sentinel must be untouched, and the planted names must be
# exactly as planted. The install shim also refuses any staged source under
# /home/agent, so a regression back to staging inside the home fails this
# scenario even when the planted pid range misses the fragment's pid.
new_guest h
mkdir -p "$GUEST_ROOT/home/agent" "$GUEST_ROOT/var/lib/chasm/packages/$PROFILE"
printf 'DO-NOT-OVERWRITE\n' >"$GUEST_ROOT/sentinel"
sentinel_sum=$(sha256sum "$GUEST_ROOT/sentinel" | cut -d' ' -f1)
h_pid=0
while [ "$h_pid" -le 400 ]; do
    ln -s "$GUEST_ROOT/sentinel" "$GUEST_ROOT/home/agent/.zshrc.pending.$h_pid"
    h_pid=$((h_pid + 1))
done
ln -s "$GUEST_ROOT/sentinel" \
    "$GUEST_ROOT/var/lib/chasm/packages/$PROFILE/.zshrc.pending.XXXXXX"
# The pre-created home must look agent-owned, or the postflight would (rightly)
# refuse for a different reason than the one under test.
printf 'agent:agent\n' >"$GUEST_ROOT/home/agent.chasm-owner"
rewrite_fragment "$WORK/apply-orig.sh" "$WORK/apply-run.sh"
run_apply "$PROFILE" "$EFFECTIVE_PKGS" "$PKGS" "$PROFILE_SHA" "$GUEST_UID" "$ZSHRC_SHA" "$ZSHRC_CONTENT" <"$WORK/apply-run.sh" \
    >"$WORK/out" 2>"$WORK/err" ||
    die "apply with planted pending symlinks failed: $(cat "$WORK/err")"
[ "$(sha256sum "$GUEST_ROOT/sentinel" | cut -d' ' -f1)" = "$sentinel_sum" ] ||
    die "a planted pending symlink redirected the root write; the sentinel was overwritten"
[ "$(find "$GUEST_ROOT/home/agent" -name '.zshrc.pending.*' | wc -l)" = 401 ] ||
    die "the layer removed or rewrote planted pending names in the agent home"
[ -L "$GUEST_ROOT/var/lib/chasm/packages/$PROFILE/.zshrc.pending.XXXXXX" ] ||
    die "the planted staging-name symlink was replaced instead of bypassed"
cmp -s -- "$GUEST_ZSHRC" "$GUEST_ROOT/home/agent/.zshrc" ||
    die "the guest .zshrc was not installed from the template input"
[ "$(PATH="$GUEST_BIN:$ORIG_PATH" stat -c %U:%G:%a "$GUEST_ROOT/home/agent/.zshrc")" = "agent:agent:644" ] ||
    die "guest .zshrc ownership or mode is wrong after the symlink scenario"

# --- 4. wiring: the build orders the user step before scrub -------------------

layer_line=$(grep -n 'install-guest-packages.sh" --apply' "$BUILDER" | cut -d: -f1 | head -n 1)
scrub_line=$(grep -n 'phase=scrub' "$BUILDER" | cut -d: -f1 | head -n 1)
[ -n "$layer_line" ] && [ -n "$scrub_line" ] ||
    die "could not locate the packages-layer invocation or the scrub phase in the builder"
[ "$layer_line" -lt "$scrub_line" ] ||
    die "the builder must run the packages layer (which creates the guest account) before phase=scrub"

awk -v q="'" '
    !on && index($0, "<<" q "GUEST_SCRUB" q) > 0 { on = 1; next }
    on && $0 == "GUEST_SCRUB" { on = 0; exit }
    on { print }
' "$BUILDER" >"$WORK/scrub.sh"
[ -s "$WORK/scrub.sh" ] || die "could not extract the scrub fragment"
grep -qF 'home=/home/agent' "$WORK/scrub.sh" ||
    die "the scrub fragment no longer checks /home/agent"
grep -qF 'agent:agent' "$WORK/scrub.sh" ||
    die "the scrub fragment no longer requires agent:agent ownership of /home/agent"

[ "$(grep -cF '"$SITE_GUEST_UID"' "$LAYER")" -ge 2 ] ||
    die "the layer must forward the site guest uid to both guest invocations"
grep -qF 'id -u agent' "$BUILDER" ||
    die "the resumed-builder qualification no longer probes the guest account"
grep -qF 'test -f /home/agent/.zshrc' "$BUILDER" ||
    die "the resumed-builder qualification no longer probes the guest zshrc"
grep -qF 'resume-guest-layer-check' "$BUILDER" ||
    die "the resume path no longer runs the packages layer's read-only check"
grep -qF 'install-guest-packages.sh" --check "$BOOTSTRAP_PROFILE"' "$BUILDER" ||
    die "the resume path no longer runs the packages layer's read-only check"

# The pending build receipt must record the guest uid and the guest zshrc
# digest, and the resume requirement list must demand both of the preserved
# builder's receipt; each line therefore appears at least twice in the
# builder (once in the receipt printf, once in the requirement loop).
[ "$(grep -cF '"guest_uid=$SITE_GUEST_UID"' "$BUILDER")" -ge 2 ] ||
    die "the builder must record guest_uid in the pending receipt and require it on resume"
[ "$(grep -cF '"guest_zshrc_sha256=$TEMPLATE_GUEST_ZSHRC_SHA256"' "$BUILDER")" -ge 2 ] ||
    die "the builder must record the guest zshrc digest in the pending receipt and require it on resume"
grep -qF '"--cwd", "/home/agent"' "$ROOT/cmd/chasm/main.go" ||
    die "chasm shell no longer opens in /home/agent"
grep -qF '"/usr/bin/zsh", "-l"' "$ROOT/cmd/chasm/main.go" ||
    die "chasm shell no longer opens /usr/bin/zsh"

# --- 5. resume binding: the REAL requirement loop refuses drift ---------------

# Extract the resume requirement loop from the builder (from `for requirement
# in` through its `done`) and evaluate it against synthetic preserved-builder
# receipts, with a fail() that reports refusal, so a resumed builder whose
# receipt does not bind the current guest zshrc digest or site guest uid
# refuses exactly as the production loop refuses.
awk '
    !on && /for requirement in/ { on = 1 }
    on { print }
    on && /^    done$/ { exit }
' "$BUILDER" >"$WORK/resume-req.sh"
[ -s "$WORK/resume-req.sh" ] || die "could not extract the resume requirement loop"

digest64=0000000000000000000000000000000000000000000000000000000000000000
make_resume_receipt() { # path guest_uid zshrc_sha256
    printf '%s\n' \
        "run_id=test" \
        "template_identity=$digest64" \
        "base_image_fingerprint=$digest64" \
        "profile_sha256=$digest64" \
        "profile=minimal-profile" \
        "bootstrap_profile=minimal" \
        "guest_uid=$2" \
        "guest_zshrc_sha256=$3" \
        "profile_manifest_sha256=$digest64" \
        "packages_layer_sha256=$digest64" \
        "profile_reader_sha256=$digest64" \
        "builder_name=as-build-resumetest" \
        "result=failed" >"$1"
}

cat >"$WORK/resume-req-run.sh" <<RUNNER
resume_receipt=\$1
resumed_from_identity=$digest64
TEMPLATE_BASE_IMAGE_FINGERPRINT=$digest64
PROFILE_NAME=minimal-profile
BOOTSTRAP_PROFILE=minimal
TEMPLATE_PROFILE_MANIFEST_SHA256=$digest64
TEMPLATE_PACKAGES_LAYER_SHA256=$digest64
TEMPLATE_PROFILE_READER_SHA256=$digest64
builder_name=as-build-resumetest
resume_builder=as-build-resumetest
SITE_GUEST_UID=$GUEST_UID
TEMPLATE_GUEST_ZSHRC_SHA256=$ZSHRC_SHA
fail() {
    echo "RESUME REFUSED: \$*" >&2
    exit 42
}
RUNNER
cat "$WORK/resume-req.sh" >>"$WORK/resume-req-run.sh"

# A receipt that binds the current inputs satisfies the production loop.
make_resume_receipt "$WORK/resume-ok" "$GUEST_UID" "$ZSHRC_SHA"
sh -eu "$WORK/resume-req-run.sh" "$WORK/resume-ok" 2>"$WORK/err" ||
    die "the resume requirements rejected a receipt that binds them: $(cat "$WORK/err")"

# A receipt whose guest zshrc digest differs (the zshrc input changed after
# the build failed) must refuse.
make_resume_receipt "$WORK/resume-zshrc" "$GUEST_UID" "$digest64"
status=0
sh -eu "$WORK/resume-req-run.sh" "$WORK/resume-zshrc" 2>"$WORK/err" || status=$?
expect_fail "$status" "resume zshrc drift" "guest_zshrc_sha256" "$WORK/err"

# A receipt whose guest uid differs must refuse.
make_resume_receipt "$WORK/resume-uid" 4243 "$ZSHRC_SHA"
status=0
sh -eu "$WORK/resume-req-run.sh" "$WORK/resume-uid" 2>"$WORK/err" || status=$?
expect_fail "$status" "resume guest uid drift" "guest_uid" "$WORK/err"

# A pre-binding receipt (no guest_uid / guest_zshrc_sha256 lines at all,
# i.e. recorded by an older builder) must refuse instead of resuming blind.
grep -v -e '^guest_uid=' -e '^guest_zshrc_sha256=' "$WORK/resume-ok" >"$WORK/resume-old"
status=0
sh -eu "$WORK/resume-req-run.sh" "$WORK/resume-old" 2>"$WORK/err" || status=$?
expect_fail "$status" "pre-binding receipt" "guest_uid" "$WORK/err"

# --- 6. the identity moves with the user layer --------------------------------

ID_BIN=$WORK/id-bin
mkdir -p "$ID_BIN"
cat >"$ID_BIN/incus" <<'STUB'
#!/bin/sh
set -eu
if [ "$1" = image ] && [ "$2" = info ]; then
    echo "Fingerprint: ffff00001111222233334444555566667777888899990000aaaabbbbccccdddd"
    exit 0
fi
if [ "$1" = profile ] && [ "$2" = show ]; then
    printf '%s\n' 'config: {}' 'description: test profile' 'devices: {}'
    exit 0
fi
echo "identity incus shim: unsupported invocation: $*" >&2
exit 9
STUB
chmod 0755 "$ID_BIN/incus"

cp -R "$ROOT/scripts" "$WORK/scripts-copy"
cp -R "$ROOT/guest-config" "$WORK/guest-config"
chmod -R g-w "$WORK/guest-config"
cat >"$WORK/resolve-identity.sh" <<DRIVER
#!/bin/sh
set -eu
: "\${SITE_GUEST_UID:?SITE_GUEST_UID is required}"
SCRIPT_DIR='$WORK/scripts-copy'
BASE_IMAGE_ALIAS=test-image
PROFILE_NAME=test-profile
BOOTSTRAP_PROFILE=minimal
RESOURCE_PREFIX=chasm
CHASM_PROFILE_ROOT='$WORK/guest-config/profiles'
export CHASM_PROFILE_ROOT
# shellcheck source=scripts/lib/sandbox-template-identity.sh
. "\$SCRIPT_DIR/lib/sandbox-template-identity.sh"
resolve_sandbox_template_identity || exit 1
printf '%s\n' "\$TEMPLATE_IDENTITY"
DRIVER
chmod 0755 "$WORK/resolve-identity.sh"

identity_before=$(SITE_GUEST_UID=$GUEST_UID PATH="$ID_BIN:$ORIG_PATH" "$WORK/resolve-identity.sh") ||
    die "could not resolve the template identity before the layer change"
case "$identity_before" in
    *[!0-9a-f]*|'') die "resolved identity is not a sha256 digest: $identity_before" ;;
esac

printf '%s\n' '# test mutation: the guest-user layer changed' \
    >>"$WORK/scripts-copy/install-guest-packages.sh"
identity_after=$(SITE_GUEST_UID=$GUEST_UID PATH="$ID_BIN:$ORIG_PATH" "$WORK/resolve-identity.sh") ||
    die "could not resolve the template identity after the layer change"
[ "$identity_after" != "$identity_before" ] ||
    die "the template identity did not change with the packages layer (guest-user step)"
case "$identity_after" in
    *[!0-9a-f]*|'') die "mutated identity is not a sha256 digest: $identity_after" ;;
esac

# The zshrc input moves the identity on its own, without touching the layer.
printf '%s\n' '# test mutation: the guest zshrc changed' \
    >>"$WORK/guest-config/guest/zshrc"
identity_zshrc=$(SITE_GUEST_UID=$GUEST_UID PATH="$ID_BIN:$ORIG_PATH" "$WORK/resolve-identity.sh") ||
    die "could not resolve the template identity after the zshrc change"
[ "$identity_zshrc" != "$identity_before" ] && [ "$identity_zshrc" != "$identity_after" ] ||
    die "the template identity did not change with the guest zshrc input"
case "$identity_zshrc" in
    *[!0-9a-f]*|'') die "zshrc-mutated identity is not a sha256 digest: $identity_zshrc" ;;
esac

# The site guest uid moves the identity on its own: the packages layer bakes
# it into the sealed account, so a uid change must produce a new identity
# (and therefore a coexisting template) instead of silently reusing one
# whose agent account has a different uid.
identity_uid=$(SITE_GUEST_UID=$((GUEST_UID + 1)) PATH="$ID_BIN:$ORIG_PATH" "$WORK/resolve-identity.sh") ||
    die "could not resolve the template identity after the guest uid change"
[ "$identity_uid" != "$identity_before" ] && [ "$identity_uid" != "$identity_after" ] &&
    [ "$identity_uid" != "$identity_zshrc" ] ||
    die "the template identity did not change with the site guest uid"
case "$identity_uid" in
    *[!0-9a-f]*|'') die "uid-mutated identity is not a sha256 digest: $identity_uid" ;;
esac

# The change must flow through the layer digest, not a policy advance: the
# format string and the policy constants stay at the pinned v6 values.
grep -qF 'format=chasm-template-identity-v6' "$IDENTITY_LIB" ||
    die "the identity format tag is not the pinned v6 value"
grep -qF 'TEMPLATE_IDENTITY_POLICY_SHA256=e586be66e38608b3180d615720d0bd8ed4562372ea849f0754a93c1d690d07be' "$IDENTITY_LIB" ||
    die "the identity policy constant is not the pinned v6 value"

echo "test-guest-user-layer: apply, idempotence, drift, symlink, wiring, resume, and identity checks passed"
