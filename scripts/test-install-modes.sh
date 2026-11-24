#!/bin/sh
umask 022
set -eu

# Repo-local, unprivileged regression harness for the provision-tree
# installation modes in scripts/lib/provision-tree.sh (the --provision
# payload step of scripts/install-chasm.sh).
#
# The defect this harness pins: a release candidate packaged with
# `git archive` (as the GitHub release tarballs are) carries 0664 files
# and 0775 directories for every member — tar applies the packaging umask
# (tar.umask=0002) to all of them, git's executable bit included. The old
# payload step copied the trees with cp -a and only fixed ownership, so
# the installed profile manifest stayed group-writable and the profile
# reader refused it ("profile manifest is group- or world-writable"),
# which aborted the template build.
#
# The harness:
#
#   1. builds a git-archive tarball of HEAD exactly as the release
#      pipeline does (git -c tar.umask=0002 archive) and extracts it with
#      the archive's own permissions under umask 0002, then asserts the
#      candidate really reproduces the defect precondition (0664 files,
#      0775 directories);
#   2. checks PROVISION_TREE_EXECUTABLES against `git ls-files -s`: the
#      list must name exactly the 100755 entries of the four provision
#      trees, so the embedded list and git cannot drift apart;
#   3. runs the installer's own copy/normalize function,
#      provision_tree_install, against a temporary destination (the
#      destination is the function's second argument — the tests-only
#      override; production passes /usr/local/lib/chasm/provision) under
#      umask 0002;
#   4. asserts the installed tree: no file or directory is group- or
#      world-writable, every directory is 0755, every regular file is
#      0644 or 0755, and the 0755 set is exactly the executable list;
#   5. proves the defect closed end to end: the profile reader accepts
#      the installed bootstrap profile;
#   6. proves the refusal message names the offending path and its mode:
#      a group-writable manifest is refused with a message that carries
#      both;
#   7. reinstalls over a pre-populated DEST (the four trees plus a sibling
#      sentinel file and a stale leftover inside a tree) and asserts that
#      exactly the four trees are replaced and the sentinel survives;
#   8. a source missing one listed executable is refused BEFORE anything
#      under DEST is touched: the previous installation stays byte-identical;
#   9.  a source that resolves to DEST is refused: DEST stays intact;
#   10. a symlink in the source is refused: a listed executable that is a
#       symlink to a sentinel file OUTSIDE the tree must be refused with the
#       sentinel's mode untouched (the old `[ -f ]` gate accepted the link
#       and the normalization chmod followed it), and an unlisted symlink
#       anywhere in a tree must be refused too;
#   11. an interruption during the tree swap rolls back: an injected mv
#       failure after the first tree has swapped restores DEST exactly, a
#       SIGTERM mid-swap does the same through the swap trap, and leftover
#       staging/old-tree names of an earlier killed run are swept at start;
#   12. wiring gate: install_payload in scripts/install-chasm.sh installs
#      the provision tree through provision_tree_install with `|| fail`
#      and no raw `cp -a` of the provision root remains.
#
# Usage: scripts/test-install-modes.sh   (repository root; no arguments)

[ "$#" -eq 0 ] || { echo "usage: $0" >&2; exit 2; }

ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
LIB=$ROOT/scripts/lib/provision-tree.sh
READER=$ROOT/scripts/lib/require-site-profile.sh

die() {
    echo "test-install-modes: FAIL: $1" >&2
    exit 1
}

command -v git >/dev/null 2>&1 || die "git is required"
command -v tar >/dev/null 2>&1 || die "tar is required"
command -v stat >/dev/null 2>&1 || die "stat is required"
[ -f "$LIB" ] || die "library is missing: $LIB"
git -C "$ROOT" rev-parse --verify HEAD >/dev/null 2>&1 ||
    die "HEAD is not resolvable: $ROOT"

# shellcheck source=scripts/lib/provision-tree.sh
. "$LIB"

WORK=$(mktemp -d)
trap 'rm -rf -- "$WORK"' EXIT HUP INT TERM

# --- 1. the release candidate, exactly as the tarball pipeline builds it -----+
# The candidate is built from the tracked WORKING TREE content, with the
# modes the git-archive release tarball applies: every tracked file is
# 0664, every directory 0775, except exactly the 100755 entries of the
# index (the tar pipeline applies the packaging umask to every member and
# keeps only git's executable intent). Building from the tracked tree
# (instead of archiving HEAD) lets the harness gate the uncommitted
# candidate directly; the mode contract is identical.

cd "$ROOT" || die "could not enter the repository root"
git ls-files -s >"$WORK/index-entries" || die "could not list the index"
cd - >/dev/null || exit 1
mkdir "$WORK/candidate.modes"
while read -r mode _candidate_sha _candidate_stage path; do
    case "$path" in plugins/*) continue ;; esac  # plugins are not payload trees
    case "$mode" in
        100755) candidate_mode=0755 ;;
        100644) candidate_mode=0664 ;;
        120000) die "symlink tracked in the payload trees: $path" ;;
        *) continue ;;  # submodules, gitlinks
    esac
    mkdir -p "$WORK/candidate.modes/$(dirname -- "$path")"
    cp "$ROOT/$path" "$WORK/candidate.modes/$path" || die "could not copy tracked file: $path"
    chmod "$candidate_mode" "$WORK/candidate.modes/$path" || die "could not flatten mode: $path"
done <"$WORK/index-entries"
find "$WORK/candidate.modes" -type d -exec chmod 0775 {} + || die "could not flatten directory modes"

CAND=$WORK/candidate.modes
DEST=$WORK/dest
mkdir "$DEST"

[ "$(stat -c '%a' "$CAND/guest-config/profiles/minimal.profile")" = 664 ] ||
    die "candidate does not reproduce the tarball modes: minimal.profile is $(stat -c '%a' "$CAND/guest-config/profiles/minimal.profile"), want 664"
[ "$(stat -c '%a' "$CAND/scripts")" = 775 ] ||
    die "candidate does not reproduce the tarball modes: scripts/ is $(stat -c '%a' "$CAND/scripts"), want 775"
[ -n "$(find "$CAND/scripts" "$CAND/guest-config" "$CAND/host-config" "$CAND/host-control" -type f -perm /022 -print)" ] ||
    die "candidate does not reproduce the tarball modes: no group-writable file found"

# --- 2. the executable list matches git's executable bit -------------------

expected_exec=$(
    git -C "$ROOT" ls-files -s -- scripts guest-config host-config host-control |
        awk '$1 == "100755" { print $4 }' | LC_ALL=C sort
)
listed_exec=$(printf '%s\n' $PROVISION_TREE_EXECUTABLES | LC_ALL=C sort)
[ -n "$expected_exec" ] || die "git reports no executable files in the provision trees"
[ "$listed_exec" = "$expected_exec" ] || {
    diff_out=$(printf '%s\n' "$listed_exec" "$expected_exec" | LC_ALL=C sort -u | sed 's/^/  /')
    die "PROVISION_TREE_EXECUTABLES does not match git's 100755 entries:
$diff_out"
}

# --- 3. the installer's own normalization, unprivileged, under umask 0002 ---

(umask 0002 && provision_tree_install "$CAND" "$DEST") ||
    die "provision_tree_install failed on the release candidate"

# --- 4. the installed tree carries the deployment modes --------------------

unsafe=$(find "$DEST/scripts" "$DEST/guest-config" "$DEST/host-config" \
    "$DEST/host-control" \( -type f -o -type d \) -perm /022 -print)
[ -z "$unsafe" ] || die "group- or world-writable entries survived the install:
$unsafe"

bad_dir=$(find "$DEST/scripts" "$DEST/guest-config" "$DEST/host-config" \
    "$DEST/host-control" -type d ! -perm 0755 -print)
[ -z "$bad_dir" ] || die "directories are not 0755:
$bad_dir"

bad_mode=$(find "$DEST/scripts" "$DEST/guest-config" "$DEST/host-config" \
    "$DEST/host-control" -type f ! -perm 0644 ! -perm 0755 -print)
[ -z "$bad_mode" ] || die "regular files outside 0644/0755:
$bad_mode"

installed_exec=$(find "$DEST/scripts" "$DEST/guest-config" \
    "$DEST/host-config" "$DEST/host-control" -type f -perm 0755 -print |
    sed "s|^$DEST/||" | LC_ALL=C sort)
[ "$installed_exec" = "$expected_exec" ] || {
    diff_out=$(printf '%s\n' "$installed_exec" "$expected_exec" | LC_ALL=C sort -u | sed 's/^/  /')
    die "the 0755 set is not exactly the intended executables:
$diff_out"
}
[ "$(stat -c '%a' "$DEST/guest-config/profiles/minimal.profile")" = 644 ] ||
    die "minimal.profile is $(stat -c '%a' "$DEST/guest-config/profiles/minimal.profile"), want 644"

# --- 5. the profile reader accepts the installed bootstrap profile ---------

reader_verdict() {
    reader_root=$1
    reader_name=$2
    if CHASM_PROFILE_ROOT="$reader_root" sh -c '
        set -eu
        # shellcheck source=scripts/lib/require-site-profile.sh
        . "$1"
        require_site_profile "$2"
    ' sh "$READER" "$reader_name" 2>"$WORK/reader.err"; then
        echo "accept"
    else
        echo "reject"
    fi
}

[ "$(reader_verdict "$DEST/guest-config/profiles" minimal)" = accept ] ||
    die "the profile reader refused the installed manifest: $(cat "$WORK/reader.err")"

# --- 6. the refusal names the offending path and its mode ------------------

mkdir -p "$WORK/writable-profiles"
cp "$DEST/guest-config/profiles/minimal.profile" "$WORK/writable-profiles/minimal.profile"
chmod 0664 "$WORK/writable-profiles/minimal.profile"
if CHASM_PROFILE_ROOT="$WORK/writable-profiles" sh -c '
    set -eu
    # shellcheck source=scripts/lib/require-site-profile.sh
    . "$1"
    require_site_profile "$2"
' sh "$READER" minimal >"$WORK/writable.out" 2>"$WORK/writable.err"; then
    die "the profile reader accepted a group-writable manifest"
fi
case "$(cat "$WORK/writable.err")" in
    *"$WORK/writable-profiles/minimal.profile"*) ;;
    *) die "the refusal does not name the offending path: $(cat "$WORK/writable.err")" ;;
esac
case "$(cat "$WORK/writable.err")" in
    *664*) ;;
    *) die "the refusal does not name the offending mode: $(cat "$WORK/writable.err")" ;;
esac

# --- 7. reinstalling replaces exactly the four trees -------------------------

# Pre-populate DEST as a previous install would have left it, plus a stale
# leftover inside a tree and a sibling sentinel that no install may touch.
printf 'sentinel\n' >"$DEST/SENTINEL-MARKER"
printf 'stale\n' >"$DEST/scripts/stale-leftover"
sentinel_sum=$(sha256sum "$DEST/SENTINEL-MARKER")
(umask 0002 && provision_tree_install "$CAND" "$DEST") ||
    die "reinstall over a pre-populated destination failed"
[ -f "$DEST/SENTINEL-MARKER" ] || die "reinstall removed the sibling sentinel"
[ "$(sha256sum "$DEST/SENTINEL-MARKER")" = "$sentinel_sum" ] ||
    die "reinstall modified the sibling sentinel"
[ ! -e "$DEST/scripts/stale-leftover" ] ||
    die "reinstall did not replace the scripts tree (stale leftover survived)"
[ -x "$DEST/scripts/launch-sandbox-vm.sh" ] ||
    die "reinstall left the installed tree unusable"

leftovers=$(find "$(dirname -- "$DEST")" -maxdepth 1 \
    \( -name '*.staging.*' -o -name '*.provision-tree-old.*' \) -print)
[ -z "$leftovers" ] || die "reinstall left staging or old-tree leftovers:
$leftovers"

# --- 8. a source missing a listed executable leaves DEST intact --------------

dest_before=$(cd "$DEST" && find . -type f -print | LC_ALL=C sort | xargs sha256sum)
BROKEN=$WORK/broken
mkdir "$BROKEN"
for provision_tree_name in $PROVISION_TREE_NAMES; do
    cp -a "$CAND/$provision_tree_name" "$BROKEN/$provision_tree_name"
done
rm "$BROKEN/scripts/lib/require-site-host.sh"
if (umask 0002 && provision_tree_install "$BROKEN" "$DEST") 2>"$WORK/broken.err"; then
    die "a source missing a listed executable must be refused"
fi
case "$(cat "$WORK/broken.err")" in
    *require-site-host.sh*) ;;
    *) die "the missing-executable refusal does not name the file: $(cat "$WORK/broken.err")" ;;
esac
dest_after=$(cd "$DEST" && find . -type f -print | LC_ALL=C sort | xargs sha256sum)
[ "$dest_before" = "$dest_after" ] ||
    die "a refused install must leave the previous installation byte-identical"

# --- 9. a source that resolves to DEST is refused ----------------------------

if (umask 0002 && provision_tree_install "$DEST" "$DEST") 2>"$WORK/same.err"; then
    die "a source that resolves to the destination must be refused"
fi
case "$(cat "$WORK/same.err")" in
    *same*director*) ;;
    *) die "the same-directory refusal is not named: $(cat "$WORK/same.err")" ;;
esac
dest_after=$(cd "$DEST" && find . -type f -print | LC_ALL=C sort | xargs sha256sum)
[ "$dest_before" = "$dest_after" ] ||
    die "the same-directory refusal must leave the destination intact"

# --- 10. a symlink in the source is refused, sentinels stay untouched --------

# dest_snapshot DIR: a text fingerprint of an installed tree — every file
# name with its content hash, every directory name, and the 0755 set — so
# "restored exactly" is assertable.
dest_snapshot() {
    (cd "$1" && find . -type f -print | LC_ALL=C sort | xargs sha256sum && \
        find . -type d -print | LC_ALL=C sort && \
        find . -type f -perm 0755 -print | LC_ALL=C sort)
}

# 10a: a LISTED executable that is a symlink to a sentinel outside the tree.
# The old `[ -f ]` source gate accepted the link (it dereferences), `cp -a`
# preserved it, and the staged `chmod 0755` followed it: a root install
# changed the mode of an arbitrary host file (e.g. /etc/shadow). The install
# must refuse, and the sentinel's mode must come out unchanged.
SYMLINK_SRC=$WORK/symlink-src
mkdir "$SYMLINK_SRC"
for provision_tree_name in $PROVISION_TREE_NAMES; do
    cp -a "$CAND/$provision_tree_name" "$SYMLINK_SRC/$provision_tree_name"
done
printf 'sentinel\n' >"$WORK/exec-sentinel"
chmod 0640 "$WORK/exec-sentinel"
sentinel_mode_before=$(stat -c '%a' "$WORK/exec-sentinel")
rm "$SYMLINK_SRC/scripts/lib/incus-names.sh"
ln -s "$WORK/exec-sentinel" "$SYMLINK_SRC/scripts/lib/incus-names.sh"
dest_before=$(dest_snapshot "$DEST")
if (umask 0002 && provision_tree_install "$SYMLINK_SRC" "$DEST") 2>"$WORK/symlink-exec.err"; then
    die "a listed executable that is a symlink must be refused"
fi
case "$(cat "$WORK/symlink-exec.err")" in
    *symlink*) ;;
    *) die "the symlinked-executable refusal does not name the defect: $(cat "$WORK/symlink-exec.err")" ;;
esac
[ "$(stat -c '%a' "$WORK/exec-sentinel")" = "$sentinel_mode_before" ] ||
    die "the install followed the symlinked executable and changed the sentinel's mode ($sentinel_mode_before -> $(stat -c '%a' "$WORK/exec-sentinel"))"
[ "$(dest_snapshot "$DEST")" = "$dest_before" ] ||
    die "the symlinked-executable refusal must leave the destination intact"

# 10b: an UNLISTED symlink anywhere in a source tree is refused too; the
# sentinel it points at must stay untouched.
SYMLINK_SRC2=$WORK/symlink-src2
mkdir "$SYMLINK_SRC2"
for provision_tree_name in $PROVISION_TREE_NAMES; do
    cp -a "$CAND/$provision_tree_name" "$SYMLINK_SRC2/$provision_tree_name"
done
printf 'sentinel\n' >"$WORK/tree-sentinel"
chmod 0640 "$WORK/tree-sentinel"
sentinel_mode_before=$(stat -c '%a' "$WORK/tree-sentinel")
ln -s "$WORK/tree-sentinel" "$SYMLINK_SRC2/guest-config/profiles/sentinel-link"
if (umask 0002 && provision_tree_install "$SYMLINK_SRC2" "$DEST") 2>"$WORK/symlink-tree.err"; then
    die "an unlisted symlink anywhere in a source tree must be refused"
fi
case "$(cat "$WORK/symlink-tree.err")" in
    *symlink*sentinel-link*) ;;
    *) die "the tree-symlink refusal does not name the link: $(cat "$WORK/symlink-tree.err")" ;;
esac
[ "$(stat -c '%a' "$WORK/tree-sentinel")" = "$sentinel_mode_before" ] ||
    die "the install changed the mode of the tree symlink's sentinel"
[ "$(dest_snapshot "$DEST")" = "$dest_before" ] ||
    die "the tree-symlink refusal must leave the destination intact"

# --- 11. an interruption during the swap rolls back to the previous trees -----

# A PATH shim for mv records every invocation and can inject a failure (or a
# SIGTERM to the installing shell) on a chosen invocation number, counted
# from 1 across the whole install. With a fully populated DEST the swap
# phase performs: #1 elder scripts aside, #2 staged scripts in, #3 elder
# guest-config aside, #4 staged guest-config in, ... so invocation #4 is
# the first moment a failure leaves tree #1 already swapped.
REAL_MV=$(command -v mv)
mkdir "$WORK/shim"
cat >"$WORK/shim/mv" <<'EOF'
#!/bin/sh
shim_count_file=${SHIM_COUNT_FILE:?}
shim_n=$(cat "$shim_count_file" 2>/dev/null || echo 0)
shim_n=$((shim_n + 1))
printf '%s\n' "$shim_n" >"$shim_count_file"
if [ -n "${SHIM_FAIL_NTH:-}" ] && [ "$shim_n" -eq "$SHIM_FAIL_NTH" ]; then
    echo "shim mv: injected failure on invocation $shim_n" >&2
    exit 1
fi
if [ -n "${SHIM_SIGNAL_NTH:-}" ] && [ "$shim_n" -eq "$SHIM_SIGNAL_NTH" ]; then
    # $PPID here is the shell running provision_tree_install.
    kill -TERM "$PPID" 2>/dev/null || :
fi
exec "${SHIM_REAL_MV:?}" "$@"
EOF
chmod 0755 "$WORK/shim/mv"

# A PATH shim for cp records every invocation and can inject a failure on
# a chosen invocation number, counted from 1. provision_tree_install calls
# cp -a once per source tree, so invocation #2 fails staging after the
# first tree copied: DEST untouched, staging discarded, elders retained.
REAL_CP=$(command -v cp)
mkdir "$WORK/shim-cp"
cat >"$WORK/shim-cp/cp" <<'EOF'
#!/bin/sh
cp_count_file=${CP_COUNT_FILE:?}
cp_n=$(cat "$cp_count_file" 2>/dev/null || echo 0)
cp_n=$((cp_n + 1))
printf '%s\n' "$cp_n" >"$cp_count_file"
if [ -n "${CP_FAIL_NTH:-}" ] && [ "$cp_n" -eq "$CP_FAIL_NTH" ]; then
    echo "shim cp: injected failure on invocation $cp_n" >&2
    exit 1
fi
exec "${CP_REAL:?}" "$@"
EOF
chmod 0755 "$WORK/shim-cp/cp"

# 11a: an ordinary mv failure after the first tree has swapped must restore
# DEST exactly (the swap-scoped failure path, not just the pre-swap checks).
SHIM_COUNT_FILE=$WORK/shim-count-1
: >"$SHIM_COUNT_FILE"
dest_before=$(dest_snapshot "$DEST")
if (umask 0002 && PATH="$WORK/shim:$PATH" SHIM_FAIL_NTH=4 SHIM_REAL_MV="$REAL_MV" \
    SHIM_COUNT_FILE="$SHIM_COUNT_FILE" provision_tree_install "$CAND" "$DEST") \
    2>"$WORK/mvfail.err"; then
    die "an injected mv failure during the swap must fail the install"
fi
case "$(cat "$WORK/mvfail.err")" in
    *"staged tree into place"*) ;;
    *) die "the injected-failure refusal does not name the failed swap: $(cat "$WORK/mvfail.err")" ;;
esac
[ "$(dest_snapshot "$DEST")" = "$dest_before" ] ||
    die "the injected mv failure did not restore DEST exactly (mutation: swap rollback)"
leftovers=$(find "$(dirname -- "$DEST")" -maxdepth 1 \
    \( -name '*.staging.*' -o -name '*.provision-tree-old.*' \) -print
    find "$DEST" -name '*.provision-tree-old.*' -print)
[ -z "$leftovers" ] || die "the failed swap left staging or old-tree leftovers:
$leftovers"

# 11b: a SIGTERM delivered mid-swap (between the two mv calls of the second
# tree) must fire the swap trap, restore DEST, and leave no leftovers.
SHIM_COUNT_FILE=$WORK/shim-count-2
: >"$SHIM_COUNT_FILE"
if (umask 0002 && PATH="$WORK/shim:$PATH" SHIM_SIGNAL_NTH=4 \
    SHIM_REAL_MV="$REAL_MV" SHIM_COUNT_FILE="$SHIM_COUNT_FILE" \
    provision_tree_install "$CAND" "$DEST") 2>"$WORK/signal.err"; then
    die "a SIGTERM mid-swap must abort the install"
fi
case "$(cat "$WORK/signal.err")" in
    *interrupted*) ;;
    *) die "the signal path does not report the interruption: $(cat "$WORK/signal.err")" ;;
esac
[ "$(dest_snapshot "$DEST")" = "$dest_before" ] ||
    die "the SIGTERM mid-swap did not restore DEST exactly (mutation: swap trap)"
leftovers=$(find "$(dirname -- "$DEST")" -maxdepth 1 \
    \( -name '*.staging.*' -o -name '*.provision-tree-old.*' \) -print
    find "$DEST" -name '*.provision-tree-old.*' -print)
[ -z "$leftovers" ] || die "the interrupted swap left staging or old-tree leftovers:
$leftovers"

# 11c: the exact leftover names of an EARLIER run killed before its trap
# could fire are cleaned only by a COMMITTED replacement: the leftover
# staging directory is swept at the start of the install, the stale
# previous-tree elders are retained through staging and the swap, and the
# post-commit cleanup removes them. The safety semantics: a stale elder is
# the only rollback copy of the last coherent deployment, so a failed
# retry must never discard it (see 11d); a successful install may.
mkdir "$DEST.staging.abc123" "$DEST/scripts.provision-tree-old.999"
printf junk >"$DEST.staging.abc123/junk"
printf junk >"$DEST/scripts.provision-tree-old.999/junk"
(umask 0002 && provision_tree_install "$CAND" "$DEST") ||
    die "the install failed while cleaning an earlier run's leftovers"
[ ! -e "$DEST.staging.abc123" ] ||
    die "the leftover staging directory of an earlier run was not swept (mutation: leftover sweep)"
[ ! -e "$DEST/scripts.provision-tree-old.999" ] ||
    die "the stale previous tree of an earlier run was not removed by the committed replacement (mutation: post-commit elder cleanup)"
[ -x "$DEST/scripts/launch-sandbox-vm.sh" ] ||
    die "the cleanup run left the installed tree unusable"

# 11d: the compound interruption/retry-failure regression. A prior run
# died mid-swap (stale elders beside the trees); a later retry FAILS in
# staging; the stale elders — the only rollback copies of the last
# coherent deployment — must survive the failed retry exactly, and a
# subsequent successful run must heal the deployment and clean them.
mkdir "$DEST/scripts.provision-tree-old.424" "$DEST/guest-config.provision-tree-old.424"
printf elder >"$DEST/scripts.provision-tree-old.424/marker"
printf elder >"$DEST/guest-config.provision-tree-old.424/marker"
dest_before_stale=$(dest_snapshot "$DEST")
if (umask 0002 && PATH="$WORK/shim-cp:$PATH" \
    CP_FAIL_NTH=2 CP_REAL="$REAL_CP" CP_COUNT_FILE="$WORK/shim-cp-count" \
    provision_tree_install "$CAND" "$DEST") 2>"$WORK/cpfail.err"; then
    die "an injected staging failure must fail the install"
fi
case "$(cat "$WORK/cpfail.err")" in
    *"could not copy"*) ;;
    *) die "the injected staging failure does not name the failed copy: $(cat "$WORK/cpfail.err")" ;;
esac
[ -e "$DEST/scripts.provision-tree-old.424" ] &&
    [ -e "$DEST/guest-config.provision-tree-old.424" ] ||
    die "the failed retry discarded the stale rollback elders (regression: interrupted-swap sweep)"
[ -e "$DEST/scripts.provision-tree-old.424/marker" ] ||
    die "the stale elder content did not survive the failed retry"
[ "$(dest_snapshot "$DEST")" = "$dest_before_stale" ] ||
    die "the failed staging attempt changed DEST"
leftovers=$(find "$(dirname -- "$DEST")" -maxdepth 1 -name '*.staging.*' -print)
[ -z "$leftovers" ] || die "the failed staging attempt left a staging directory:\n$leftovers"
(umask 0002 && provision_tree_install "$CAND" "$DEST") ||
    die "the recovery run after the failed retry did not succeed"
[ ! -e "$DEST/scripts.provision-tree-old.424" ] &&
    [ ! -e "$DEST/guest-config.provision-tree-old.424" ] ||
    die "the committed replacement did not clean the stale elders"
[ -x "$DEST/scripts/launch-sandbox-vm.sh" ] ||
    die "the recovery run left the installed tree unusable"
leftovers=$(find "$(dirname -- "$DEST")" -maxdepth 1 \
    \( -name '*.staging.*' -o -name '*.provision-tree-old.*' \) -print
    find "$DEST" -name '*.provision-tree-old.*' -print)
[ -z "$leftovers" ] || die "the recovery run left leftovers:\n$leftovers"

# --- 12. the installer payload is wired through provision_tree_install -------

INSTALLER=$ROOT/scripts/install-chasm.sh
[ -f "$INSTALLER" ] || die "the installer is missing: $INSTALLER"
wiring_call='provision_tree_install "$PROVISION_ROOT" "$INSTALL_PROVISION" ||'
wiring_line=$(grep -nF "$wiring_call" "$INSTALLER" | cut -d: -f1 | head -n 1)
[ -n "$wiring_line" ] ||
    die "install_payload must call provision_tree_install \"\$PROVISION_ROOT\" \"\$INSTALL_PROVISION\" ||"
next_line=$(sed -n "$((wiring_line + 1))p" "$INSTALLER")
case "$next_line" in
    *'fail '*) ;;
    *) die "the provision_tree_install call must be followed by || fail (got: $next_line)" ;;
esac
if grep -qF 'cp -a "$PROVISION_ROOT/' "$INSTALLER"; then
    die "install_payload must not copy the provision trees with cp -a"
fi

# --- 13. root checks every checkout path before sourcing or execution --------
# Use a minimal fixture with no plugin directory: optional plugins must stay
# optional. The fake id command exercises the root-only recursive scan while
# this harness itself remains unprivileged.
GUARD_FIXTURE=$WORK/checkout-guard
mkdir -p "$GUARD_FIXTURE/scripts/lib"
cp "$INSTALLER" "$GUARD_FIXTURE/scripts/install-chasm.sh"
cp "$ROOT/scripts/lib/require-site-host.sh" "$ROOT/scripts/lib/incus-names.sh" \
    "$ROOT/scripts/lib/host-resources.sh" "$ROOT/scripts/lib/provision-tree.sh" \
    "$GUARD_FIXTURE/scripts/lib/"
run_guard_installer() {
    sh "$GUARD_FIXTURE/scripts/install-chasm.sh" --help >"$WORK/guard.out" 2>"$WORK/guard.err"
}
run_guard_installer || die "safe caller-owned checkout without optional plugins should reach --help: $(cat "$WORK/guard.err")"

GUARD_FAKEBIN=$WORK/guard-fakebin
GUARD_CALLER_UID=$(id -u)
mkdir -p "$GUARD_FAKEBIN" "$GUARD_FIXTURE/host-config"
cat >"$GUARD_FAKEBIN/id" <<'EOF_ID'
#!/bin/sh
if [ "${1-}" = -u ]; then
    echo 0
else
    exec /usr/bin/id "$@"
fi
EOF_ID
chmod 0755 "$GUARD_FAKEBIN/id"

chmod 0666 "$GUARD_FIXTURE/scripts/lib/require-site-host.sh"
if PATH="$GUARD_FAKEBIN:$PATH" SUDO_UID=$GUARD_CALLER_UID run_guard_installer; then
    die "root-mode guard should refuse a world-writable sourced file"
fi
grep -q 'world-writable' "$WORK/guard.err" || die "world-writable refusal was not explained"
chmod 0644 "$GUARD_FIXTURE/scripts/lib/require-site-host.sh"

chmod 0775 "$GUARD_FIXTURE/scripts/lib"
PATH="$GUARD_FAKEBIN:$PATH" SUDO_UID=$GUARD_CALLER_UID run_guard_installer ||
    die "group-writable checkout should remain usable"
[ "$(grep -c 'warning: checkout has group-writable paths' "$WORK/guard.err")" -eq 1 ] ||
    die "group-writable paths should produce one actionable warning"
chmod 0755 "$GUARD_FIXTURE/scripts/lib"

printf 'unsafe template\n' >"$GUARD_FIXTURE/host-config/unsafe.template"
chmod 0666 "$GUARD_FIXTURE/host-config/unsafe.template"
if PATH="$GUARD_FAKEBIN:$PATH" SUDO_UID=$GUARD_CALLER_UID run_guard_installer; then
    die "root-mode recursive scan should reject a world-writable template"
fi
grep -q 'world-writable' "$WORK/guard.err" ||
    die "recursive checkout refusal did not identify the world-writable path"
chmod 0644 "$GUARD_FIXTURE/host-config/unsafe.template"
ln -s "$ROOT/README.md" "$GUARD_FIXTURE/host-config/escaping-link"
if PATH="$GUARD_FAKEBIN:$PATH" SUDO_UID=$GUARD_CALLER_UID run_guard_installer; then
    die "root-mode recursive scan should reject a symlink escaping the checkout"
fi
grep -q 'escaping symlink' "$WORK/guard.err" ||
    die "escaping symlink refusal was not explained"
rm "$GUARD_FIXTURE/host-config/escaping-link"
mkdir -p "$GUARD_FIXTURE/.git"
printf 'metadata\n' >"$GUARD_FIXTURE/.git/config"
ln -s "$GUARD_FIXTURE/.git/config" "$GUARD_FIXTURE/host-config/git-link"
if PATH="$GUARD_FAKEBIN:$PATH" SUDO_UID=$GUARD_CALLER_UID run_guard_installer; then
    die "root-mode recursive scan should reject a symlink into excluded .git metadata"
fi
grep -q 'escaping symlink' "$WORK/guard.err" ||
    die "symlink into .git refusal was not explained"

rm "$GUARD_FIXTURE/host-config/git-link"
if [ "$GUARD_CALLER_UID" -ne 0 ]; then
    if PATH="$GUARD_FAKEBIN:$PATH" SUDO_UID=65534 run_guard_installer; then
        die "checkout owned by neither root nor sudo caller should be refused"
    fi
    grep -q 'untrusted owner' "$WORK/guard.err" || die "ownership refusal was not explained"
fi

# --- 14. capability opt-in and release binary migration ---------------------
# Execute the production helpers with a scratch payload and fixture tools.
# No package manager, sudo, Incus, or systemd command participates.
FUNCTIONS=$WORK/portability-functions.sh
{
    sed -n '/^check_host_distribution() {$/,/^}$/p' "$INSTALLER"
    sed -n '/^selinux_enforcing() {$/,/^}$/p' "$INSTALLER"
    sed -n '/^selinux_bin_label() {$/,/^}$/p' "$INSTALLER"
    sed -n '/^check_service_executable() {$/,/^}$/p' "$INSTALLER"
    sed -n '/^install_go_payload() {$/,/^}$/p' "$INSTALLER"
    sed -n '/^site_kernel_hostname() {$/,/^}$/p' "$ROOT/scripts/lib/require-site-host.sh"
} >"$FUNCTIONS"
# shellcheck disable=SC1090
. "$FUNCTIONS"
p_ok() { printf 'ok %s\n' "$*"; }
p_note() { printf 'note %s\n' "$*"; }
p_fail() { printf 'fail %s\n' "$*"; }
fail() { printf 'fail %s\n' "$*" >&2; exit 1; }

OS_RELEASE=$WORK/os-release
printf 'ID=fedora\nID_LIKE="rhel centos"\n' >"$OS_RELEASE"
ALLOW_UNTESTED_HOST=0 CHASM_ALLOW_UNTESTED_HOST=0 check_host_distribution "$OS_RELEASE" |
    grep -q '^fail .*--allow-untested-host' || die "unknown distro should require opt-in"
ALLOW_UNTESTED_HOST=1 CHASM_ALLOW_UNTESTED_HOST=0 check_host_distribution "$OS_RELEASE" |
    grep -q '^note WARNING:' || die "explicit flag must admit an untested distro with a warning"
ALLOW_UNTESTED_HOST=0 CHASM_ALLOW_UNTESTED_HOST=1 check_host_distribution "$OS_RELEASE" |
    grep -q '^note WARNING:' || die "explicit environment opt-in must warn"
printf 'ID=ubuntu\nID_LIKE="debian linux"\n' >"$OS_RELEASE"
ALLOW_UNTESTED_HOST=0 CHASM_ALLOW_UNTESTED_HOST=0 check_host_distribution "$OS_RELEASE" |
    grep -q '^ok Debian-family host' || die "Debian derivative should pass the default gate"

CAPABILITY_BIN=$WORK/capability-bin
mkdir -p "$CAPABILITY_BIN"
cat >"$CAPABILITY_BIN/uname" <<'EOF_UNAME'
#!/bin/sh
[ "$#" -eq 1 ] && [ "$1" = -n ] || exit 1
echo unqualified-host
EOF_UNAME
chmod 0755 "$CAPABILITY_BIN/uname"
[ "$(PATH="$CAPABILITY_BIN" site_kernel_hostname)" = unqualified-host ] ||
    die "kernel hostname resolution must not need hostname executable"

INSTALL_GO=$WORK/libexec/chasm
INSTALL_OLD_GO=$WORK/lib/chasm/go
INSTALL_BIN=$WORK/bin
BUILD_GO=$WORK/staged-go
GO_BINARIES="chasm chasmctl executor_server supervisor_server"
mkdir -p "$INSTALL_GO" "$INSTALL_OLD_GO" "$INSTALL_BIN" "$BUILD_GO"
for binary in $GO_BINARIES; do
    printf 'old %s\n' "$binary" >"$INSTALL_OLD_GO/$binary"
    printf 'new %s\n' "$binary" >"$BUILD_GO/$binary"
done
# Stub privileged ownership flags while retaining ordinary copy/mode behavior.
install() {
    [ "$#" -eq 8 ] && [ "$1" = -o ] && [ "$3" = -g ] && [ "$5" = -m ] ||
        die "unexpected install arguments: $*"
    /usr/bin/install "$5" "$6" "$7" "$8"
}
verify_payload_links() {
    for binary in $GO_BINARIES; do
        [ "$(readlink "$INSTALL_OLD_GO/$binary")" = "$INSTALL_GO/$binary" ] ||
            die "legacy $binary path did not migrate"
        [ -x "$INSTALL_GO/$binary" ] || die "$binary did not install executable"
    done
    [ "$(readlink "$INSTALL_BIN/chasm")" = "$INSTALL_GO/chasm" ] || die "CLI link did not migrate"
    [ "$(readlink "$INSTALL_BIN/chasmctl")" = "$INSTALL_GO/chasmctl" ] || die "control CLI link did not migrate"
}

# Restrict command discovery to known tools to make SELinux absence deterministic.
for tool in ln awk grep; do ln -s "$(command -v "$tool")" "$CAPABILITY_BIN/$tool"; done
( PATH="$CAPABILITY_BIN"; ! selinux_enforcing && install_go_payload "$BUILD_GO" ) ||
    die "migration failed with SELinux absent"
verify_payload_links

getenforce() { printf '%s\n' "$SELINUX_MODE"; }
restorecon() {
    [ "$1" = -F ] || die "unexpected restorecon arguments"
    printf '%s\n' "$2" >>"$WORK/labels"
}
ls() {
    if [ "$1" = -Z ]; then
        printf 'system_u:object_r:%s:s0 %s\n' "$SELINUX_TYPE" "$2"
    else
        /usr/bin/ls "$@"
    fi
}
SELINUX_MODE=Permissive
SELINUX_TYPE=usr_t
install_go_payload "$BUILD_GO"
[ ! -e "$WORK/labels" ] || die "permissive SELinux unexpectedly required relabeling"
verify_payload_links

SELINUX_MODE=Enforcing
rm "$INSTALL_OLD_GO/chasm" "$INSTALL_BIN/chasm"
printf 'old chasm\n' >"$INSTALL_OLD_GO/chasm"
if (install_go_payload "$BUILD_GO") >"$WORK/selinux.out" 2>&1; then
    die "enforcing SELinux accepted a drifted executable label"
fi
grep -q 'label is not bin_t' "$WORK/selinux.out" || die "label failure lacks a diagnostic"
[ ! -L "$INSTALL_OLD_GO/chasm" ] && [ ! -e "$INSTALL_BIN/chasm" ] ||
    die "migration published links before all executable labels were verified"
SELINUX_TYPE=bin_t
install_go_payload "$BUILD_GO"
verify_payload_links
selinux_bin_label "$INSTALL_GO/chasm" || die "known bin_t label did not verify"
SELINUX_TYPE=usr_t
if selinux_bin_label "$INSTALL_GO/chasm"; then die "drifted label passed verification"; fi

for server in executor supervisor; do
    UNIT=$ROOT/host-control/chasm-$server.service.template
    command_path=/usr/local/libexec/chasm/${server}_server
    check_service_executable "$UNIT" "$command_path" || die "unit does not use migrated executable: $UNIT"
    printf '# ExecStart=%s\nExecStart=/usr/local/lib/chasm/go/%s_server\n' "$command_path" "$server" >"$WORK/stale-unit"
    if check_service_executable "$WORK/stale-unit" "$command_path"; then
        die "stale unit passed executable path verification"
    fi
done

echo "test-install-modes: ok (candidate modes reproduced; normalized tree clean; executables exact; reader accepts the installed profile; safe reinstall validated; symlinks refused; interrupted swaps roll back; stale elders retained across failed retries and cleaned by committed replacements; payload wiring pinned; root checkout guard verified)"
