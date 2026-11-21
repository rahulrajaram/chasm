#!/bin/sh
# ssh-hardening.sh: the opt-in sshd hardening step of scripts/install-chasm.sh.
#
# Hardening is OFF by default and is applied only when the operator passes
# --harden-ssh (accepted only with --provision). The drop-ins shipped in
# host-config/ssh disable SSH password and root login, so before the step
# asks for approval it warns plainly what it changes and that the operator
# must already hold a working SSH key (or console access): a host with
# neither is locked out. There is no key guard; the approval itself must be
# typed (see the approve_hardening contract below), even when the caller
# pre-approves every other step via CHASM_ASSUME_YES.
#
# Signal-safe staging, in order:
#   1. ALL existing drop-ins (including dangling symlinks) are COPIED,
#      never moved, into a backup directory, and every copy is verified
#      against its original (cmp, or readlink for a symlink), BEFORE the
#      rollback trap is armed and BEFORE any destination file is touched:
#      a backup copy that fails or cannot be verified stops the step with
#      the originals in place and nothing changed. The fragments that had
#      an original are recorded in ssh_hardening_backed_up;
#   2. a trap on EXIT HUP INT TERM restores the previous files (and
#      removes fragments that had no previous version) from the moment
#      staging starts until the step succeeds. The restore uses the
#      recorded list: a recorded fragment whose backup is missing is
#      REPORTED and left in place, never treated as "no original existed"
#      (which would delete a live drop-in). Staging is protected against
#      signals and shell aborts, not against SIGKILL or power loss; once
#      the step has applied (a successful reload, or an install with no
#      reloadable unit) the trap only removes the backup, so a late signal
#      cannot revert files the running sshd already loaded;
#   3. sshd -t -f "$SSHD_MAIN_CONFIG" validates the full configuration
#      BEFORE any reload; a validation failure restores the previous files
#      and reloads nothing;
#   4. on success the first active unit among ssh.service and sshd.service
#      is reloaded. A .socket unit is never reloaded: socket activation
#      execs a fresh sshd per connection, so when only ssh.socket is
#      active the step reports that the drop-ins take effect for new
#      connections; when no ssh unit is active the step says so explicitly
#      and the drop-ins take effect at the next sshd start;
#   5. only then are the trap cleared and the backup removed.
#
# Trap contract: this library arms EXIT HUP INT TERM for the duration of
# the staging step and CLEARS ALL FOUR when the step ends (success or
# failure). It saves and restores nothing it did not set: a caller that
# keeps its own traps armed across install_ssh_hardening loses them. The
# installer therefore finishes (and disarms) its own cleanup traps before
# calling this library.
#
# The installer sources this library and calls install_ssh_hardening; the
# unprivileged regression harness (plugins/ssh-hardening/test-ssh-hardening.sh)
# sources it
# with PATH shims for sshd/systemctl/install instead. Callers supply:
# HARDEN_SSH (1 to apply), INSTALL_SSH_CONFIG_DIR, SSH_DROPINS_DIR (the
# plugin's own drop-ins directory), the fail function, and approve_hardening (an approval function that requires
# an explicit interactive confirmation and is NOT satisfied by
# CHASM_ASSUME_YES). SSHD_MAIN_CONFIG may override the main config path
# (the harness points it at a fixture).

# The installer runs as root under --provision; the variable exists so the
# unprivileged regression harness can point the check at a fixture config.
: "${SSHD_MAIN_CONFIG:=/etc/ssh/sshd_config}"

SSH_HARDENING_FRAGMENTS="00-chasm.conf 99-chasm.conf"

# The first shipped fragment is the earliest drop-in this step can own.
ssh_hardening_first_fragment=${SSH_HARDENING_FRAGMENTS%% *}

# ssh_hardening_installed: both drop-ins are present as regular files,
# byte-identical to the provision tree.
ssh_hardening_installed() {
    for ssh_fragment in $SSH_HARDENING_FRAGMENTS; do
        if [ ! -f "$INSTALL_SSH_CONFIG_DIR/$ssh_fragment" ] || \
            [ -L "$INSTALL_SSH_CONFIG_DIR/$ssh_fragment" ] || \
            ! cmp -s "$SSH_DROPINS_DIR/$ssh_fragment" \
                "$INSTALL_SSH_CONFIG_DIR/$ssh_fragment"; then
            return 1
        fi
    done
    return 0
}

# ssh_hardening_any_fragment: at least one shipped drop-in is present as a
# regular file (byte-identical or not). A partially-present set is a drift
# condition, not "not applied".
ssh_hardening_any_fragment() {
    for ssh_fragment in $SSH_HARDENING_FRAGMENTS; do
        if [ -f "$INSTALL_SSH_CONFIG_DIR/$ssh_fragment" ]; then
            return 0
        fi
    done
    return 1
}

# ssh_hardening_conflicting_fragments: any *.conf in the drop-in directory
# whose name sorts strictly before the first shipped fragment AND that sets
# one of the keywords the shipped drop-ins set. OpenSSH keeps the FIRST
# value obtained for each keyword, so such a fragment wins over
# 00-chasm.conf even after a successful hardening apply. The check is
# generic on purpose: it names whatever the directory contains, not any
# particular predecessor's file name.
ssh_hardening_conflicting_fragments() {
    ssh_conflict_seen=0
    for ssh_candidate in "$INSTALL_SSH_CONFIG_DIR"/*.conf; do
        [ -f "$ssh_candidate" ] || continue
        ssh_candidate_name=${ssh_candidate##*/}
        [ "$ssh_candidate_name" != "$ssh_hardening_first_fragment" ] || continue
        [ "$(printf '%s\n%s\n' "$ssh_candidate_name" "$ssh_hardening_first_fragment" |
            sort | head -n1)" = "$ssh_candidate_name" ] || continue
        if grep -Eq \
            '^[[:space:]]*(PasswordAuthentication|PermitRootLogin|AuthorizedKeysFile)[[:space:]]' \
            "$ssh_candidate"; then
            if [ "$ssh_conflict_seen" -eq 1 ]; then
                printf ' '
            fi
            printf '%s' "$ssh_candidate_name"
            ssh_conflict_seen=1
        fi
    done
}

# ssh_hardening_conflict_warning: the shared first-value-rule warning text
# for the fragment names given as arguments.
ssh_hardening_conflict_warning() {
    echo "install: WARNING: ssh drop-in(s) $* in $INSTALL_SSH_CONFIG_DIR sort before $ssh_hardening_first_fragment and set PasswordAuthentication, PermitRootLogin, or AuthorizedKeysFile; OpenSSH keeps the first value it reads, so they win over the shipped fragments." >&2
}

# ssh_hardening_backup_matches ORIGINAL BACKUP: a backup copy is good
# only if it is byte-identical to the original it must be able to restore:
# cmp for regular files, and a readlink comparison for symlinks (cp -a
# copies the link itself, and cmp cannot read through a dangling one).
ssh_hardening_backup_matches() {
    if [ -L "$1" ]; then
        [ "$(readlink "$1")" = "$(readlink "$2")" ]
    else
        cmp -s "$1" "$2"
    fi
}

# ssh_hardening_was_backed_up FRAGMENT: the fragment was recorded in
# ssh_hardening_backed_up when the backups were completed and verified,
# i.e. it had an original that was successfully copied into the backup
# directory.
ssh_hardening_was_backed_up() {
    for ssh_recorded in ${ssh_hardening_backed_up:-}; do
        [ "$ssh_recorded" = "$1" ] && return 0
    done
    return 1
}

# ssh_hardening_restore: best-effort restore of the previous drop-ins from
# the backup directory, driven by ssh_hardening_backed_up, the list of
# fragments recorded when the backups were completed and verified. A
# recorded fragment must still have its backup (regular file or dangling
# symlink; cp -a copies the link itself); a missing one is REPORTED and its
# live file is left in place instead of being removed as if no original
# existed. A fragment with no recorded original is removed, so the
# directory ends in its pre-step state. Returns non-zero if any restore
# step failed (the report went to stderr either way).
ssh_hardening_restore() {
    ssh_restore_failed=0
    for ssh_fragment in $SSH_HARDENING_FRAGMENTS; do
        if ssh_hardening_was_backed_up "$ssh_fragment"; then
            if [ -e "$ssh_hardening_backup/$ssh_fragment" ] || \
                [ -L "$ssh_hardening_backup/$ssh_fragment" ]; then
                rm -f "$INSTALL_SSH_CONFIG_DIR/$ssh_fragment"
                cp -a "$ssh_hardening_backup/$ssh_fragment" \
                    "$INSTALL_SSH_CONFIG_DIR/$ssh_fragment" || {
                    echo "install: could not restore $ssh_fragment from $ssh_hardening_backup" >&2
                    ssh_restore_failed=1
                }
            else
                echo "install: the recorded backup of $ssh_fragment is missing from $ssh_hardening_backup; leaving $INSTALL_SSH_CONFIG_DIR/$ssh_fragment in place" >&2
                ssh_restore_failed=1
            fi
        else
            rm -f "$INSTALL_SSH_CONFIG_DIR/$ssh_fragment"
        fi
    done
    return "$ssh_restore_failed"
}

# ssh_hardening_applied is set once the new drop-ins are the intended end
# state (validated and reloaded, or validated with no reloadable unit);
# after that the trap must not restore the previous files.
ssh_hardening_applied=0

# ssh_hardening_trap: the trap armed while staging is in progress. Any
# abnormal end there (signal, or a set -e abort from a failed staging
# command) stops the step. The traps are cleared first so the EXIT trap
# cannot run a second time, the backup directory is always removed, and
# the previous files are restored only while the step is still staging.
ssh_hardening_trap() {
    trap - EXIT HUP INT TERM
    if [ "${ssh_hardening_applied:-0}" -ne 1 ]; then
        ssh_hardening_restore || :
    fi
    rm -rf "$ssh_hardening_backup"
    exit 70
}

# ssh_hardening_disarm: mark applied, clear the trap, and drop the backup;
# only the success paths and the two explicit failure paths (which restored
# first) may call it.
ssh_hardening_disarm() {
    ssh_hardening_applied=1
    trap - EXIT HUP INT TERM
    rm -rf "$ssh_hardening_backup"
}

# ssh_hardening_report OK_CALLBACK DRIFT_CALLBACK: the drift-report probe
# for the hardening drop-ins. The step is optional, so ABSENT drop-ins are
# informational ("not applied"), while present drop-ins must match the
# shipped content. Foreign fragments that win the first-value rule are
# drift ONLY while a chasm drop-in is present to defeat; with the hardening
# not applied there is nothing to override, so they are reported through
# the ok callback as a note (the caller's "ok" line) instead. The caller
# supplies its ok and drift reporting functions so their counters stay the
# caller's.
ssh_hardening_report() {
    ssh_report_ok=$1
    ssh_report_drift=$2
    ssh_report_present=0
    if ssh_hardening_installed; then
        ssh_report_present=1
        for ssh_fragment in $SSH_HARDENING_FRAGMENTS; do
            "$ssh_report_ok" "sshd hardening fragment present: $ssh_fragment"
        done
    elif ssh_hardening_any_fragment; then
        ssh_report_present=1
        for ssh_fragment in $SSH_HARDENING_FRAGMENTS; do
            if [ ! -f "$INSTALL_SSH_CONFIG_DIR/$ssh_fragment" ]; then
                "$ssh_report_drift" "sshd hardening fragment missing: $ssh_fragment"
            else
                "$ssh_report_drift" "sshd hardening fragment drifted: $ssh_fragment"
            fi
        done
    else
        "$ssh_report_ok" "ssh hardening: not applied (optional)"
    fi
    ssh_conflicts=$(ssh_hardening_conflicting_fragments)
    if [ -n "$ssh_conflicts" ]; then
        if [ "$ssh_report_present" -eq 1 ]; then
            "$ssh_report_drift" "ssh drop-in(s) $ssh_conflicts in $INSTALL_SSH_CONFIG_DIR sort before $ssh_hardening_first_fragment and win OpenSSH's first-value rule for the hardening keywords"
        else
            "$ssh_report_ok" "note: ssh drop-in(s) $ssh_conflicts in $INSTALL_SSH_CONFIG_DIR sort before $ssh_hardening_first_fragment and win OpenSSH's first-value rule for the hardening keywords; informational because the chasm hardening is not applied"
        fi
    fi
}

# ssh_install_directory / ssh_install_drop_in SRC DEST: install with the
# explicit deployment modes (0755 directories, 0644 drop-ins) regardless of
# the source tree's modes and the invoking umask. The owner is root:root
# exactly when the step runs as root (--provision); the unprivileged test
# harness drives the same paths with PATH shims and keeps its invoking
# owner, which the mode checks never consult.
ssh_install_directory() {
    if [ "$(id -u)" -eq 0 ]; then
        install -d -o root -g root -m 0755 "$INSTALL_SSH_CONFIG_DIR"
    else
        install -d -m 0755 "$INSTALL_SSH_CONFIG_DIR"
    fi
}

ssh_install_drop_in() {
    if [ "$(id -u)" -eq 0 ]; then
        install -o root -g root -m 0644 "$1" "$2"
    else
        install -m 0644 "$1" "$2"
    fi
}

# install_ssh_hardening: the --provision step. Without HARDEN_SSH=1 it
# prints one skip line (notes already-installed drop-ins it leaves alone,
# and re-validates them with sshd -t); with it, warn, require the explicit
# hardening approval, stage signal-safely, validate with sshd -t, reload
# the active ssh service, and only then disarm.
install_ssh_hardening() {
    if [ "${HARDEN_SSH:-0}" -ne 1 ]; then
        if ssh_hardening_installed; then
            echo "install: ssh hardening: skipped (pass --harden-ssh to apply); the hardening drop-ins are already installed and were left in place"
            # Re-validate what is installed: drift only compares content, so
            # a corrupted fragment that breaks the host config must be
            # caught here, where the operator is already acting on the host.
            if ! sshd -t -f "$SSHD_MAIN_CONFIG"; then
                fail "the installed ssh hardening drop-ins do not pass sshd -t; the host sshd configuration is broken"
            fi
        else
            echo "install: ssh hardening: skipped (pass --harden-ssh to apply)"
        fi
        ssh_skip_conflicts=$(ssh_hardening_conflicting_fragments)
        if [ -n "$ssh_skip_conflicts" ]; then
            ssh_hardening_conflict_warning "$ssh_skip_conflicts"
        fi
        return 0
    fi

    echo "install: WARNING: --harden-ssh disables SSH password and root login on this host." >&2
    echo "install: WARNING: you must already hold a working SSH public key for the operator (or console access); without either, this step locks you out." >&2
    ssh_apply_conflicts=$(ssh_hardening_conflicting_fragments)
    if [ -n "$ssh_apply_conflicts" ]; then
        ssh_hardening_conflict_warning "$ssh_apply_conflicts"
    fi

    approve_hardening "harden the host ssh daemon (install the chasm sshd_config drop-ins: disable password and root login; reload the active ssh unit; existing sessions keep running)"
    [ -f "$SSHD_MAIN_CONFIG" ] || fail "$SSHD_MAIN_CONFIG is missing"
    ssh_include_pattern=$(printf '%s' "$INSTALL_SSH_CONFIG_DIR" | sed 's/[.[\*^$()+?{|\\]/\\&/g')
    grep -Eq "^[[:space:]]*Include[[:space:]]+${ssh_include_pattern}/\*\.conf([[:space:]]|$)" "$SSHD_MAIN_CONFIG" ||
        fail "$SSHD_MAIN_CONFIG has no 'Include $INSTALL_SSH_CONFIG_DIR/*.conf' line; the drop-ins would be ignored"
    command -v sshd >/dev/null 2>&1 || fail "sshd is not installed; refusing to stage the hardening drop-ins"
    # The drop-ins are root-owned deployment content installed with an
    # explicit mode regardless of the source tree's own modes and the
    # invoking umask: ssh_install_drop_in sets root:root when the step runs
    # as root (--provision) and keeps the invoking owner for the
    # unprivileged test harness that drives this library with PATH shims.
    ssh_install_directory

    ssh_hardening_backup=$(mktemp -d)
    # Complete and verify EVERY backup BEFORE arming the rollback trap and
    # BEFORE touching any destination file: a copy that fails (full
    # filesystem, unreadable original) or does not match its original
    # stops the step with the originals in place and nothing changed, so
    # the rollback can never delete a drop-in it never captured. Only
    # fragments that had an original are recorded in ssh_hardening_backed_up.
    ssh_hardening_backed_up=
    ssh_backup_failed=
    for ssh_fragment in $SSH_HARDENING_FRAGMENTS; do
        if [ -e "$INSTALL_SSH_CONFIG_DIR/$ssh_fragment" ] || \
            [ -L "$INSTALL_SSH_CONFIG_DIR/$ssh_fragment" ]; then
            if ! cp -a "$INSTALL_SSH_CONFIG_DIR/$ssh_fragment" \
                "$ssh_hardening_backup/$ssh_fragment"; then
                ssh_backup_failed=$ssh_fragment
                break
            fi
            if ! ssh_hardening_backup_matches \
                "$INSTALL_SSH_CONFIG_DIR/$ssh_fragment" \
                "$ssh_hardening_backup/$ssh_fragment"; then
                ssh_backup_failed=$ssh_fragment
                break
            fi
            ssh_hardening_backed_up="$ssh_hardening_backed_up $ssh_fragment"
        fi
    done
    if [ -n "$ssh_backup_failed" ]; then
        rm -rf "$ssh_hardening_backup"
        fail "could not complete and verify the backup of $INSTALL_SSH_CONFIG_DIR/$ssh_backup_failed; the ssh drop-ins were left unchanged"
    fi
    trap ssh_hardening_trap EXIT HUP INT TERM
    for ssh_fragment in $SSH_HARDENING_FRAGMENTS; do
        ssh_install_drop_in \
            "$SSH_DROPINS_DIR/$ssh_fragment" \
            "$INSTALL_SSH_CONFIG_DIR/$ssh_fragment"
    done

    # Fail closed on an invalid configuration BEFORE reloading: an sshd that
    # refuses to start would lock every future session out of the host.
    if ! sshd -t -f "$SSHD_MAIN_CONFIG"; then
        ssh_hardening_restore || :
        ssh_hardening_disarm
        fail "sshd -t rejected the configuration after installing the drop-ins; the previous files were restored and sshd was not reloaded"
    fi

    ssh_hardening_reloaded=0
    for ssh_hardening_unit in ssh.service sshd.service; do
        if systemctl is-active --quiet "$ssh_hardening_unit" 2>/dev/null; then
            # Close the post-reload signal window: from just before the
            # reload until the applied flag is recorded, HUP INT TERM are
            # ignored (the EXIT trap stays armed for real failures), so a
            # signal cannot make the rollback restore files the daemon
            # already loaded.
            trap '' HUP INT TERM
            if ! systemctl reload "$ssh_hardening_unit"; then
                # The reload failed, so the rollback applies again.
                trap ssh_hardening_trap EXIT HUP INT TERM
                ssh_hardening_restore || :
                ssh_hardening_disarm
                fail "could not reload $ssh_hardening_unit to apply the hardening drop-ins; the previous files were restored"
            fi
            ssh_hardening_applied=1
            echo "install: ssh hardening: reloaded $ssh_hardening_unit"
            ssh_hardening_reloaded=1
            # Normal handling again: from here the applied flag, not the
            # blocked signals, protects the installed files.
            trap ssh_hardening_trap EXIT HUP INT TERM
            break
        fi
    done
    if [ "$ssh_hardening_reloaded" -ne 1 ]; then
        # The same window until disarm: with no reloadable unit the
        # drop-ins are reported as already in force (socket activation) or
        # as taking effect at the next sshd start, so a signal here must
        # not restore the previous files either.
        trap '' HUP INT TERM
        if systemctl is-active --quiet ssh.socket 2>/dev/null; then
            # A .socket unit has no reload job, and reloading it would not
            # re-read sshd_config anyway: socket activation execs a fresh
            # sshd:ssh@ per connection, so the drop-ins are already in
            # force for every new connection.
            echo "install: ssh hardening: installed; takes effect for new connections (socket activation; only ssh.socket is active)"
        else
            echo "install: ssh hardening: installed; takes effect at next sshd start (no ssh.service, sshd.service, or ssh.socket is active)"
        fi
    fi

    ssh_hardening_disarm
    echo "install: ssh hardening: applied (password and root login disabled; public-key login only)"
}
