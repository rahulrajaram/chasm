#!/bin/sh

# provision-tree.sh: explicit-mode installation of the provision trees.
#
# Sourced by scripts/install-chasm.sh (the --provision payload step) and by
# the unprivileged regression harness scripts/test-install-modes.sh. It
# defines PROVISION_TREE_NAMES, PROVISION_TREE_EXECUTABLES, and
# provision_tree_install SRC DEST.
#
# The installer must never depend on the source tree's file modes or on the
# invoking umask. A release candidate packaged with git archive (as the
# GitHub release tarballs are) carries 0664 files and 0775 directories for
# EVERY member regardless of git's executable bit, because tar applies the
# packaging umask (0002) to all of them. A tree copied with cp -a preserves
# those modes verbatim, and the mode-sensitive readers downstream then
# refuse the deployment surface: the profile reader rejects a
# group-writable manifest, the template identity fails to resolve, and the
# sealed-template build aborts. provision_tree_install therefore copies the
# trees and then normalizes them to the deployment contract: root:root
# ownership (when the installer runs as root; the unprivileged harness
# keeps its invoking owner), 0755 directories, 0644 regular files, and 0755
# for exactly the files git carries as executable. Every mode is set with
# chmod after the copy, so neither the source modes nor the umask can leak
# through.
#
# The destination is replaced safely: the source (every listed executable
# present as a regular file; no symlink anywhere in the four trees; SRC and
# DEST not the same directory after resolution) is fully validated BEFORE
# anything under DEST is touched, the new trees are staged and normalized
# in a staging directory beside DEST, and only then is each tree swapped
# into place (previous tree moved aside, staged tree moved in, previous
# tree removed). Symlinks are refused outright — in the source AND again
# in the staged copy — because `cp -a` preserves a symlink and the later
# normalization chmod/chown follows it: an absolute symlink among the
# staged files would let a root install change the mode or ownership of an
# arbitrary host file outside the staging directory (for example
# /etc/shadow). With no symlink present, the normalization can never
# escape the staging directory. Every rm and mv status is checked — the
# caller wraps this function in `|| fail`, which suspends set -e for the
# whole call — and any failure after a swap rolls the already-swapped
# trees back, so a failed install leaves the previously installed
# deployment intact.
#
# Trap contract (same shape as the ssh-hardening plugin's staging step): the swap
# phase arms EXIT HUP INT TERM for its duration and CLEARS ALL FOUR when
# it ends (success, ordinary failure, or signal). It saves and restores
# nothing it did not set: a caller that keeps its own traps armed across
# provision_tree_install loses them. The installer therefore finishes (and
# disarms) its own cleanup traps before calling this library, which
# install_payload in scripts/install-chasm.sh already does. While the
# traps are armed, a signal or abnormal exit restores every previous tree
# that is mid-swap (the filesystem state alone decides what to restore:
# a previous tree found beside its name moves back, replacing any half
# swapped-in new tree) and removes the staging directory.
#
# Stale elders of an earlier run that died before its trap could fire
# (kill -9, power loss) are deliberately NOT swept at the start of the
# next run: a sweep before staging would discard the only rollback copies
# of the last coherent deployment, so a subsequent staging or swap failure
# would leave the mixed destination with no path back. Stale elders are
# retained until a replacement has COMMITTED: the post-commit cleanup —
# run only after the whole swap phase succeeded and only with this
# library's traps already cleared, so cleanup can never race a restore —
# removes this run's elders and any stale <tree>.provision-tree-old.<pid>
# copies beside them. A failed run (staging or swap) leaves every elder in
# place; only the exact staging-directory names this library creates
# (<dest>.staging.XXXXXX beside DEST) are swept at the start of a run,
# because a staging directory is never a rollback copy.

# The repository trees the payload step installs under the provision root.
PROVISION_TREE_NAMES="scripts guest-config host-config host-control"

# The files inside those trees that must be executable (0755) after the
# install. The list captures git's executable intent (the 100755 entries of
# `git ls-files -s`); it is deliberately NOT derived from the source tree's
# modes at install time, because a git-archive tarball flattens them all to
# 0664. scripts/test-install-modes.sh re-checks the list against
# `git ls-files -s` on every run, so the list and git cannot drift apart.
PROVISION_TREE_EXECUTABLES="host-control/chasm-firewall \
scripts/audit-workspace-symlinks.sh \
scripts/build-sandbox-template.sh \
scripts/check-ci-coverage.sh \
scripts/check-commit-identity.sh \
scripts/check-core-only.sh \
scripts/check-descriptor-parity.sh \
scripts/check-portability.sh \
scripts/check-site-profiles.sh \
scripts/diagnostics/audit-sandbox.sh \
scripts/diagnostics/guest-lynis.sh \
scripts/diagnostics/sandbox-health.sh \
scripts/initialize-incus.sh \
scripts/install-chasm.sh \
scripts/install-control-plane.sh \
scripts/install-guest-packages.sh \
scripts/launch-sandbox-vm.sh \
scripts/lib/incus-names.sh \
scripts/lib/require-site-host.sh \
scripts/lib/require-site-profile.sh \
scripts/render-firewall.sh \
scripts/render-host-config.sh \
scripts/render-host-units.sh \
scripts/render-incus-preseed.sh \
scripts/test-audit-sandbox.sh \
scripts/test-builder-address-claim.sh \
scripts/test-core-package-union.sh \
scripts/test-firewall-helper.sh \
scripts/test-guest-user-layer.sh \
scripts/test-install-modes.sh \
scripts/test-profile-root-selection.sh \
scripts/test-provision-rerun.sh \
scripts/test-resource-prefix.sh"
# Plugin test scripts (plugins/ssh-hardening, plugins/install-incus) are NOT
# core payload executables: core installs and verifies with plugins/ absent.

provision_tree_fail() {
    echo "provision tree: $*" >&2
    return 1
}

# provision_tree_rollback SWAPPED: restore DEST to the state the previous
# installation had, for every tree this run already swapped in, and remove
# the staging directory. The previous tree of each swapped tree still sits
# beside its name under the recorded old suffix. Every rm and mv status is
# checked; the function returns non-zero when any restore step failed (the
# destination then needs a manual re-run of the installer, which is safe to
# repeat beside running sandboxes).
provision_tree_rollback() {
    # This function runs on the ordinary failure path (traps still armed)
    # and from the swap trap handler (which cleared them first): disarm here
    # too, so a signal arriving DURING the rollback cannot re-enter the
    # handler while the previous trees are half restored.
    trap - EXIT HUP INT TERM
    provision_tree_rollback_swapped=$1
    provision_tree_rollback_status=0
    for provision_tree_rollback_name in $provision_tree_rollback_swapped; do
        provision_tree_rollback_old="$provision_tree_dest/$provision_tree_rollback_name$provision_tree_old_suffix"
        if ! rm -rf -- "${provision_tree_dest:?}/$provision_tree_rollback_name"; then
            provision_tree_fail "rollback could not remove the failed tree: $provision_tree_dest/$provision_tree_rollback_name"
            provision_tree_rollback_status=1
            continue
        fi
        if ! mv -- "${provision_tree_rollback_old:?}" "$provision_tree_dest/$provision_tree_rollback_name"; then
            provision_tree_fail "rollback could not restore the previous tree: $provision_tree_rollback_old"
            provision_tree_rollback_status=1
        fi
    done
    if ! rm -rf -- "${provision_tree_staging:?}"; then
        provision_tree_fail "rollback could not remove the staging directory: $provision_tree_staging"
        provision_tree_rollback_status=1
    fi
    [ "$provision_tree_rollback_status" -eq 0 ] ||
        provision_tree_fail "the destination may be incomplete after the failed swap; re-run the installer to repair it"
    return "$provision_tree_rollback_status"
}

# provision_tree_swap_trap: the handler armed for the duration of the
# swap phase. It restores purely from filesystem state — for each of the
# four trees, any previous tree found beside its name moves back over
# whatever half-swapped state sits at the tree's place — so it is correct
# in every interruption window (before, between, or after the two mv calls
# of any tree) without relying on bookkeeping variables the signal could
# have caught mid-update. It then removes the staging directory and exits.
provision_tree_swap_trap() {
    trap - EXIT HUP INT TERM
    provision_tree_swap_recover || :
    provision_tree_fail "the provision tree swap was interrupted; the previous installation was restored; re-run the installer to finish" || :
    exit 70
}

# provision_tree_swap_recover: restore DEST from the mid-swap state. For
# every tree, a previous tree beside its name (the recorded old suffix)
# means the swap of that tree did not commit: remove whatever occupies the
# tree's place (a half moved-in staged tree, or the freshly swapped-in new
# tree) and move the previous tree back. A tree with no previous tree
# beside its name needs nothing: either it was never swapped, or its swap
# committed and its previous tree was already removed (or never existed).
provision_tree_swap_recover() {
    provision_tree_recover_status=0
    for provision_tree_recover_name in $PROVISION_TREE_NAMES; do
        provision_tree_recover_old="$provision_tree_dest/$provision_tree_recover_name$provision_tree_old_suffix"
        [ -e "$provision_tree_recover_old" ] || [ -L "$provision_tree_recover_old" ] || continue
        if [ -e "$provision_tree_dest/$provision_tree_recover_name" ] ||
            [ -L "$provision_tree_dest/$provision_tree_recover_name" ]; then
            rm -rf -- "${provision_tree_dest:?}/$provision_tree_recover_name" ||
                provision_tree_recover_status=1
        fi
        mv -- "$provision_tree_recover_old" "$provision_tree_dest/$provision_tree_recover_name" ||
            provision_tree_recover_status=1
    done
    [ -z "${provision_tree_staging:-}" ] ||
        rm -rf -- "${provision_tree_staging:?}" || provision_tree_recover_status=1
    return "$provision_tree_recover_status"
}

# provision_tree_stage_failed REASON: discard the staging directory after a
# staging failure and report REASON. Nothing under DEST has been touched at
# this point, so no rollback is needed.
provision_tree_stage_failed() {
    provision_tree_stage_reason=$1
    if ! rm -rf -- "${provision_tree_staging:?}"; then
        provision_tree_fail "could not remove the failed staging directory: $provision_tree_staging"
    fi
    provision_tree_fail "$provision_tree_stage_reason" || return 1
}

# provision_tree_install SRC DEST: copy the four provision trees from SRC
# into DEST and normalize them to the deployment modes. DEST must already
# exist (the payload step creates it with install -d; the test harness
# passes a temporary directory, which is the destination override the
# installer needs for unprivileged testing). The copy itself is cp -a into
# a staging directory beside DEST, but only as a content copy: every mode
# and the ownership are then set explicitly, so the source tree's modes and
# the invoking umask are irrelevant. The staged trees are swapped into
# place only after the whole source has validated and staged cleanly.
# Returns non-zero after reporting the first failure.
provision_tree_install() {
    provision_tree_src=$1
    provision_tree_dest=$2

    # Validate the source and the destination relationship before anything
    # under DEST is touched: the previously installed trees must survive an
    # incomplete source, and a source that resolves to the destination
    # would otherwise be destroyed by the swap below.
    [ -d "$provision_tree_dest" ] ||
        provision_tree_fail "destination does not exist: $provision_tree_dest" || return 1
    for provision_tree_name in $PROVISION_TREE_NAMES; do
        [ -d "$provision_tree_src/$provision_tree_name" ] &&
            [ ! -L "$provision_tree_src/$provision_tree_name" ] ||
            provision_tree_fail "source tree is missing or is a symlink: $provision_tree_src/$provision_tree_name" || return 1
    done
    provision_tree_resolved_src=$(CDPATH='' cd -- "$provision_tree_src" && pwd -P) ||
        provision_tree_fail "source is not resolvable: $provision_tree_src" || return 1
    provision_tree_resolved_dest=$(CDPATH='' cd -- "$provision_tree_dest" && pwd -P) ||
        provision_tree_fail "destination is not resolvable: $provision_tree_dest" || return 1
    [ "$provision_tree_resolved_src" != "$provision_tree_resolved_dest" ] ||
        provision_tree_fail "source and destination are the same directory: $provision_tree_resolved_src" || return 1
    # Refuse ANY symlink anywhere in the four source trees, listed
    # executable or not: the normalization chmod/chown must never be able
    # to follow a link out of the staging directory, and `cp -a` would
    # faithfully carry the link into it. -type l matches a symlink to a
    # directory just as well (find does not follow by default).
    for provision_tree_name in $PROVISION_TREE_NAMES; do
        provision_tree_links=$(find "$provision_tree_src/$provision_tree_name" -type l -print 2>/dev/null) ||
            provision_tree_fail "could not walk the source tree: $provision_tree_src/$provision_tree_name" || return 1
        [ -z "$provision_tree_links" ] ||
            provision_tree_fail "the source tree carries symlinks, which the install refuses:\n$provision_tree_links" || return 1
    done
    for provision_tree_exec in $PROVISION_TREE_EXECUTABLES; do
        [ -f "$provision_tree_src/$provision_tree_exec" ] &&
            [ ! -L "$provision_tree_src/$provision_tree_exec" ] ||
            provision_tree_fail "executable is missing from the source tree or is a symlink: $provision_tree_src/$provision_tree_exec" || return 1
    done

    # Sweep only the leftover STAGING directories of an earlier run that
    # was killed before its swap trap could fire (kill -9, power loss):
    # only the exact name this library creates — a staging directory
    # <dest>.staging.XXXXXX beside DEST — never anything else that happens
    # to sit there. The <tree>.provision-tree-old.<pid> elders of an
    # interrupted run are deliberately kept here: they are the only
    # rollback copies of the last coherent deployment, and removing them
    # before this run's replacement has committed would turn a later
    # staging/swap failure into an unrecoverable mixed destination. They
    # are removed only by the post-commit cleanup below. An unmatched glob
    # stays literal and is skipped by the existence test.
    for provision_tree_leftover in "$provision_tree_resolved_dest".staging.??????; do
        [ -d "$provision_tree_leftover" ] || continue
        rm -rf -- "$provision_tree_leftover" ||
            provision_tree_fail "could not sweep the leftover staging directory of an earlier interrupted run: $provision_tree_leftover" || return 1
    done

    # Stage and normalize beside DEST (inside DEST's parent, so the later
    # renames never leave the same filesystem): copy, normalize directories
    # to 0755, regular files to 0644, then 0755 for exactly the intended
    # executables. The umask is never consulted, and a file missing from
    # the staged tree is a failure, not a silent 0644 leftover of the copy.
    provision_tree_staging=$(mktemp -d "$provision_tree_resolved_dest.staging.XXXXXX") ||
        provision_tree_fail "could not create a staging directory beside $provision_tree_resolved_dest" || return 1
    provision_tree_old_suffix=".provision-tree-old.$$"
    for provision_tree_name in $PROVISION_TREE_NAMES; do
        cp -a "$provision_tree_src/$provision_tree_name" "$provision_tree_staging/$provision_tree_name" ||
            provision_tree_stage_failed "could not copy $provision_tree_src/$provision_tree_name into staging" || return 1
        # Second symlink gate: the copy must carry no link either, so a
        # source that changed between the sweep above and the copy (or any
        # other path to the same effect) still cannot steer the chmod and
        # chown below at a file outside the staging directory.
        provision_tree_links=$(find "$provision_tree_staging/$provision_tree_name" -type l -print 2>/dev/null) ||
            provision_tree_stage_failed "could not walk the staged tree: staging/$provision_tree_name" || return 1
        [ -z "$provision_tree_links" ] ||
            provision_tree_stage_failed "the staged tree carries symlinks, which the install refuses:\n$provision_tree_links" || return 1
        find "$provision_tree_staging/$provision_tree_name" -type d -exec chmod 0755 {} + ||
            provision_tree_stage_failed "could not normalize directory modes under staging/$provision_tree_name" || return 1
        find "$provision_tree_staging/$provision_tree_name" -type f -exec chmod 0644 {} + ||
            provision_tree_stage_failed "could not normalize file modes under staging/$provision_tree_name" || return 1
    done
    for provision_tree_exec in $PROVISION_TREE_EXECUTABLES; do
        [ -f "$provision_tree_staging/$provision_tree_exec" ] &&
            [ ! -L "$provision_tree_staging/$provision_tree_exec" ] ||
            provision_tree_stage_failed "executable is missing from the staged tree or is a symlink: $provision_tree_exec" || return 1
        chmod 0755 "$provision_tree_staging/$provision_tree_exec" ||
            provision_tree_stage_failed "could not make $provision_tree_exec executable" || return 1
    done

    # Fail closed on the contract itself: before the swap, nothing in the
    # staged trees may be group- or world-writable.
    [ -z "$(find "$provision_tree_staging" \( -type f -o -type d \) -perm /022 -print 2>/dev/null)" ] ||
        provision_tree_stage_failed "the staged trees still carry group- or world-writable entries" || return 1

    # The provision tree is a root-owned deployment surface: the descriptor
    # and profile readers refuse manifests owned by neither root nor the
    # invoking user, and the invoking user of --provision is root. The
    # unprivileged regression harness runs the same normalization without
    # the ownership step (it cannot chown to root, and its files already
    # carry its own uid, which the readers accept).
    if [ "$(id -u)" -eq 0 ]; then
        provision_tree_stage_paths=
        for provision_tree_name in $PROVISION_TREE_NAMES; do
            provision_tree_stage_paths="$provision_tree_stage_paths $provision_tree_staging/$provision_tree_name"
        done
        # shellcheck disable=SC2086  # the staged paths are a word list by construction
        chown -R root:root $provision_tree_stage_paths ||
            provision_tree_stage_failed "could not set root:root ownership on the staged trees" || return 1
    fi

    # Swap each staged tree into place: the previous tree moves aside, the
    # staged tree moves in, and the previous tree is removed only after the
    # whole swap phase has succeeded (the rollback path still needs it).
    # The swap-scoped traps cover the window the ordinary rollback cannot:
    # a signal (or an abnormal exit) between the two mv calls of one tree,
    # or after several trees have swapped, would otherwise bypass every
    # checked-failure branch and leave the deployment half swapped. The
    # trap contract is ssh-hardening's: all four traps are set here and
    # cleared here, on every exit path; the caller has already cleared its
    # own traps before calling (install_payload does).
    trap provision_tree_swap_trap EXIT HUP INT TERM
    provision_tree_swapped=
    provision_tree_elders=
    for provision_tree_name in $PROVISION_TREE_NAMES; do
        provision_tree_old=
        if [ -e "$provision_tree_dest/$provision_tree_name" ] ||
            [ -L "$provision_tree_dest/$provision_tree_name" ]; then
            provision_tree_old="$provision_tree_dest/$provision_tree_name$provision_tree_old_suffix"
            if ! mv -- "${provision_tree_dest:?}/$provision_tree_name" "$provision_tree_old"; then
                provision_tree_rollback "$provision_tree_swapped" || return 1
                provision_tree_fail "could not move the previous tree aside: $provision_tree_dest/$provision_tree_name" || return 1
            fi
        fi
        if mv -- "${provision_tree_staging:?}/$provision_tree_name" "$provision_tree_dest/$provision_tree_name"; then
            provision_tree_swapped="$provision_tree_swapped $provision_tree_name"
            [ -n "$provision_tree_old" ] &&
                provision_tree_elders="$provision_tree_elders $provision_tree_old"
        else
            if [ -n "$provision_tree_old" ]; then
                mv -- "${provision_tree_old:?}" "$provision_tree_dest/$provision_tree_name" ||
                    provision_tree_fail "could not restore the previous tree: $provision_tree_old"
            fi
            provision_tree_rollback "$provision_tree_swapped" || return 1
            provision_tree_fail "could not move the staged tree into place: $provision_tree_dest/$provision_tree_name" || return 1
        fi
    done

    # The swap succeeded: the replacement has committed. Disarm this
    # library's traps FIRST — the committed state must not be "restored"
    # over itself while cleanup runs, and the trap contract is that the
    # library clears what it set on every exit path — then drop this
    # run's replaced trees, the stale elders of any earlier interrupted
    # run (safe now: the coherent deployment is in place), and the
    # staging shell.
    trap - EXIT HUP INT TERM
    for provision_tree_elder in $provision_tree_elders; do
        if ! rm -rf -- "${provision_tree_elder:?}"; then
            provision_tree_fail "could not remove the replaced tree: $provision_tree_elder" || return 1
        fi
    done
    for provision_tree_name in $PROVISION_TREE_NAMES; do
        for provision_tree_stale_elder in \
            "$provision_tree_dest/$provision_tree_name".provision-tree-old.*; do
            [ -e "$provision_tree_stale_elder" ] || [ -L "$provision_tree_stale_elder" ] || continue
            rm -rf -- "$provision_tree_stale_elder" ||
                provision_tree_fail "could not remove the stale previous tree of an earlier interrupted run: $provision_tree_stale_elder" || return 1
        done
    done
    if ! rm -rf -- "${provision_tree_staging:?}"; then
        provision_tree_fail "could not remove the staging directory: $provision_tree_staging" || return 1
    fi
    # The traps were already cleared above; the whole install committed.
    return 0
}
