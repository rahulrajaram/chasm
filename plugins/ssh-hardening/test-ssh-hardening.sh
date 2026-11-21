#!/bin/sh
umask 022
set -eu

# Repo-local, unprivileged regression harness for the opt-in sshd hardening
# step in plugins/ssh-hardening/ssh-hardening.sh (the --provision step of
# scripts/install-chasm.sh, applied only with --harden-ssh).
#
# The harness sources the real library and drives install_ssh_hardening
# against PATH shims (sshd, systemctl, install) inside a temp directory, so
# no root, no real sshd, and no host config mutation is involved:
#
#   1. without HARDEN_SSH the step prints the one skip line, prompts
#      nothing, installs nothing, and reloads nothing;
#   2. without HARDEN_SSH but with the drop-ins already installed, the step
#      says so, leaves them byte-identical in place, and re-validates them
#      with sshd -t;
#   3. HARDEN_SSH=1 warns about password/root login and the working-key
#      requirement BEFORE the approval, installs both drop-ins, validates,
#      reloads the active ssh unit, and removes the backup;
#   4. an sshd -t failure restores the previous files exactly (a foreign
#      drop-in byte for byte, a dangling symlink as a symlink) and never
#      reloads;
#   5. an install failure mid-staging restores the previous files through
#      the armed trap path and leaves no backup directory behind;
#   6. an interruption (SIGTERM delivered by the sshd shim after staging)
#      restores the previous files through the trap path and leaves no
#      backup directory behind;
#   7. with no active ssh unit the step says the drop-ins are installed and
#      take effect at the next sshd start, and reloads nothing;
#   8. a reload failure restores the previous files and fails;
#   9. --harden-ssh outside --provision is a usage error (exit 2), and
#      --help together with any flag prints help (exit 0);
#  10. when only ssh.socket is active, nothing is reloaded and the step
#      reports that the drop-ins take effect for new connections (socket
#      activation);
#  11. a signal arriving after a successful reload does not restore the
#      previous files (the daemon already loaded them) and leaves no backup;
#  12. a foreign drop-in sorting before 00-chasm.conf that sets a hardening
#      keyword is warned about on the skip path and the apply path;
#  13. the drift probe reports absent drop-ins as informational
#      ("not applied (optional)"), verifies present ones against the
#      shipped content, drifts on foreign winning drop-ins while a chasm
#      fragment is present, and downgrades them to an informational note
#      when the hardening is not applied;
#  14. the approval goes through approve_hardening (the explicit,
#      typed-confirmation entry point), never through plain approve;
#  15. the installer's approve_hardening requires an interactive typed
#      confirmation and has no CHASM_ASSUME_YES fast path;
#  16. the installer's trap hand-off sequence (payload staging cleaned and
#      its trap cleared before the hardening step runs) leaves nothing
#      behind;
#  17. the Include precondition is derived from INSTALL_SSH_CONFIG_DIR: a
#      main config whose Include line names another directory is refused;
#  18. a backup copy that fails (cp shim) leaves the ORIGINAL drop-ins
#      byte-identical, installs nothing, and fails the step without
#      leaving a backup directory behind;
#  19. a backup copy that succeeds but does not match its original
#      (cp shim writes different bytes) is caught by the cmp verification
#      and likewise changes nothing;
#  20. a rollback whose recorded backup list names a fragment whose backup
#      is missing reports the missing backup, leaves that live fragment in
#      place, restores the others, and still fails;
#  21. a SIGTERM delivered by the systemctl shim right after a successful
#      reload does not restore the installed drop-ins (the signal window
#      between the reload and the applied flag is closed);
#  22. a SIGTERM delivered by the systemctl shim on the socket-only path
#      (while the drop-ins are being reported as in force) does not
#      restore them either: applied is recorded through disarm under
#      deferred signals.
#
# TMPDIR is pointed inside the work directory so the step's backup
# directory is observable: a successful run must leave no backup behind.
#
# Usage: plugins/ssh-hardening/test-ssh-hardening.sh   (repository root; no arguments)

[ "$#" -eq 0 ] || { echo "usage: $0" >&2; exit 2; }

ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)
LIB=$ROOT/plugins/ssh-hardening/ssh-hardening.sh
INSTALLER=$ROOT/scripts/install-chasm.sh
FRAG00=$ROOT/plugins/ssh-hardening/drop-ins/00-chasm.conf
FRAG99=$ROOT/plugins/ssh-hardening/drop-ins/99-chasm.conf

die() {
    echo "test-ssh-hardening: FAIL: $1" >&2
    exit 1
}

# shellcheck source=plugins/ssh-hardening/ssh-hardening.sh
. "$LIB"

WORK=$(mktemp -d)
trap 'rm -rf -- "$WORK"' EXIT HUP INT TERM
mkdir -p "$WORK/bin" "$WORK/state" "$WORK/conf/sshd_config.d" "$WORK/etc" "$WORK/tmp"

export SYSTEMCTL_LOG=$WORK/state/systemctl.log
export SSHD_MODE=ok
export SYSTEMCTL_MODE=active
export FAIL_INSTALL_99=0
export FAIL_BACKUP_99=0
export CORRUPT_BACKUP_99=0

# The main config fixture carries the Include line the step expects for THIS
# harness config directory; the step derives the precondition from
# INSTALL_SSH_CONFIG_DIR, so a fixture naming the real /etc would be refused.
printf 'Include %s/*.conf\n' "$WORK/conf/sshd_config.d" >"$WORK/etc/sshd_config"

# --- fake sshd ------------------------------------------------------------
# -t (with -f CONFIG, which is required) obeys SSHD_MODE: ok exits 0, fail
# exits 1, kill signals the calling step (its parent) with SIGTERM to
# simulate an interruption after staging.
cat >"$WORK/bin/sshd" <<'STUB'
#!/bin/sh
set -eu
sshd_config=
while [ "$#" -gt 0 ]; do
    case "$1" in
        -t) ;;
        -f) [ "$#" -ge 2 ] || { echo "fake sshd: -f needs an argument" >&2; exit 9; }
            sshd_config=$2; shift ;;
        *)
            echo "fake sshd: unsupported argument: $1" >&2
            exit 9
            ;;
    esac
    shift
done
[ -n "$sshd_config" ] || { echo "fake sshd: sshd -t must be run with -f" >&2; exit 9; }
case "${SSHD_MODE:?SSHD_MODE unset}" in
    ok) exit 0 ;;
    fail) echo "fake sshd: configuration test failed" >&2; exit 1 ;;
    kill) kill -TERM "$PPID"; exit 0 ;;
    *)
        echo "fake sshd: unsupported mode: $SSHD_MODE" >&2
        exit 9
        ;;
esac
STUB

# --- fake systemctl -------------------------------------------------------
# Every invocation is logged (reload ordering is assertable). is-active
# honors SYSTEMCTL_MODE: active (default) and reloadfail admit every unit,
# socketonly admits only ssh.socket, none reports every unit inactive;
# reload succeeds unless SYSTEMCTL_MODE=reloadfail, and reloadsignal kills
# the calling step (its parent) after the reload is logged.
cat >"$WORK/bin/systemctl" <<'STUB'
#!/bin/sh
set -eu
printf '%s\n' "$*" >>"${SYSTEMCTL_LOG:?SYSTEMCTL_LOG unset}"
systemctl_unit=
for systemctl_argument in "$@"; do
    systemctl_unit=$systemctl_argument
done
case "$1" in
    is-active)
        case "${SYSTEMCTL_MODE:-active}" in
            active|reloadfail|reloadsignal) exit 0 ;;
            socketonly) [ "$systemctl_unit" = ssh.socket ] && exit 0; exit 3 ;;
            socketsignal)
                # Only ssh.socket is active, and probing it signals the
                # calling step (its parent) mid-step, inside the window
                # where the drop-ins are being reported as in force.
                [ "$systemctl_unit" = ssh.socket ] || exit 3
                kill -TERM "$PPID"
                exit 0
                ;;
            *) exit 3 ;;
        esac
        ;;
    reload)
        case "${SYSTEMCTL_MODE:-active}" in
            reloadfail)
                echo "fake systemctl: reload failed" >&2
                exit 1
                ;;
            reloadsignal)
                kill -TERM "$PPID"
                exit 0
                ;;
        esac
        exit 0
        ;;
esac
echo "fake systemctl: unsupported invocation: $*" >&2
exit 9
STUB

# --- fake install ---------------------------------------------------------
# Delegates to the real install, except when FAIL_INSTALL_99=1, where the
# second staging write (the 99 drop-in) fails to exercise the trap path.
cat >"$WORK/bin/install" <<'STUB'
#!/bin/sh
set -eu
if [ "${FAIL_INSTALL_99:-0}" = 1 ]; then
    for install_argument in "$@"; do
        case "$install_argument" in
            */99-chasm.conf)
                echo "fake install: simulated failure" >&2
                exit 1
                ;;
        esac
    done
fi
exec /usr/bin/install "$@"
STUB

# --- fake cp --------------------------------------------------------------
# Delegates to the real cp, except for copies INTO the backup directory
# (destination is a mktemp directory under TMPDIR): FAIL_BACKUP_99=1 makes
# the backup copy of the 99 drop-in fail; CORRUPT_BACKUP_99=1 makes it
# "succeed" with different bytes, so the cmp verification must catch it.
cat >"$WORK/bin/cp" <<'STUB'
#!/bin/sh
set -eu
if [ "${FAIL_BACKUP_99:-0}" = 1 ] || [ "${CORRUPT_BACKUP_99:-0}" = 1 ]; then
    cp_destination=
    for cp_argument in "$@"; do
        cp_destination=$cp_argument
    done
    case "$cp_destination" in
        */tmp*/99-chasm.conf)
            if [ "${FAIL_BACKUP_99:-0}" = 1 ]; then
                echo "fake cp: simulated backup failure" >&2
                exit 1
            fi
            printf '%s\n' corrupt-backup >"$cp_destination"
            exit 0
            ;;
    esac
fi
exec /usr/bin/cp "$@"
STUB
chmod 0755 "$WORK/bin/sshd" "$WORK/bin/systemctl" "$WORK/bin/install" "$WORK/bin/cp"
export PATH="$WORK/bin:$PATH"

# The harness context the installer normally provides for the library; the
# library's backup mktemp lands in $WORK/tmp through TMPDIR.
HARDEN_SSH=0
INSTALL_SSH_CONFIG_DIR=$WORK/conf/sshd_config.d
SSH_DROPINS_DIR=$ROOT/plugins/ssh-hardening/drop-ins
SSHD_MAIN_CONFIG=$WORK/etc/sshd_config
export TMPDIR=$WORK/tmp
approve() { echo "approved: $1"; }
approve_hardening() { echo "hardening-approved: $1"; }
fail() {
    echo "install: $*" >&2
    exit 1
}

reset_state() {
    rm -rf "${WORK:?}/conf" "${WORK:?}/tmp"
    mkdir -p "$WORK/conf/sshd_config.d" "$WORK/tmp"
    # Scenario include_precondition_follows_config_dir replaces the main
    # config fixture, so every reset restores the matching one.
    printf 'Include %s/*.conf\n' "$WORK/conf/sshd_config.d" >"$WORK/etc/sshd_config"
    : >"$SYSTEMCTL_LOG"
    SSHD_MODE="ok"
    SYSTEMCTL_MODE="active"
    FAIL_INSTALL_99=0
    FAIL_BACKUP_99=0
    CORRUPT_BACKUP_99=0
    HARDEN_SSH=0
    ssh_hardening_applied=0
    ssh_hardening_backed_up=
}

hardening_status=0
hardening_output=
run_hardening() {
    hardening_status=0
    hardening_output=$(install_ssh_hardening 2>&1) || hardening_status=$?
    # The hardening library clears EXIT HUP INT TERM when it ends (its
    # documented trap contract: it saves and restores nothing it did not
    # set), so the harness re-arms its own cleanup after every run.
    trap 'rm -rf -- "$WORK"' EXIT HUP INT TERM
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

reloads() {
    grep -c '^reload' "$SYSTEMCTL_LOG" || true
}

fragments_installed() {
    cmp -s "$FRAG00" "$INSTALL_SSH_CONFIG_DIR/00-chasm.conf" &&
        cmp -s "$FRAG99" "$INSTALL_SSH_CONFIG_DIR/99-chasm.conf"
}

backup_left_behind() {
    [ -n "$(ls -A "$WORK/tmp" 2>/dev/null)" ]
}

assert_backup_removed() {
    if backup_left_behind; then
        die "a backup directory was left behind in $WORK/tmp"
    fi
}

# 1. Default (no --harden-ssh): the one skip line, nothing else happens.
scenario_default_skip() {
    reset_state
    run_hardening
    [ "$hardening_status" -eq 0 ] || die "default skip failed: status=$hardening_status
$hardening_output"
    assert_contains "$hardening_output" "ssh hardening: skipped (pass --harden-ssh to apply)" "default skip message"
    assert_absent "$hardening_output" "approved:" "default skip must not prompt"
    assert_absent "$hardening_output" "hardening-approved:" "default skip must not prompt"
    [ -z "$(ls -A "$INSTALL_SSH_CONFIG_DIR" 2>/dev/null)" ] ||
        die "default skip installed files"
    [ "$(reloads)" -eq 0 ] || die "default skip reloaded a unit"
    assert_backup_removed
}

# 2. Default skip with the drop-ins already installed: say so, leave them,
#    and re-validate them with sshd -t; a broken installed set fails.
scenario_default_skip_leaves_installed() {
    reset_state
    mkdir -p "$INSTALL_SSH_CONFIG_DIR"
    cp "$FRAG00" "$FRAG99" "$INSTALL_SSH_CONFIG_DIR/"
    run_hardening
    [ "$hardening_status" -eq 0 ] || die "installed-skip failed: status=$hardening_status
$hardening_output"
    assert_contains "$hardening_output" "skipped (pass --harden-ssh to apply)" "installed-skip keeps the skip line"
    assert_contains "$hardening_output" "already installed" "installed-skip says the drop-ins are installed"
    fragments_installed || die "installed-skip altered the installed drop-ins"
    assert_absent "$hardening_output" "approved:" "installed-skip must not prompt"
    [ "$(reloads)" -eq 0 ] || die "installed-skip reloaded a unit"
}

# 2b. Default skip with installed drop-ins that fail sshd -t: report it.
scenario_default_skip_revalidates_installed() {
    reset_state
    mkdir -p "$INSTALL_SSH_CONFIG_DIR"
    cp "$FRAG00" "$FRAG99" "$INSTALL_SSH_CONFIG_DIR/"
    HARDEN_SSH=0
    SSHD_MODE="fail"
    run_hardening
    [ "$hardening_status" -eq 1 ] || die "skip-path validation failure was swallowed: status=$hardening_status
$hardening_output"
    assert_contains "$hardening_output" "do not pass sshd -t" "skip-path validation failure message"
    fragments_installed || die "skip-path validation must not alter the drop-ins"
}

# 3. --harden-ssh: warn before approval, install, validate, reload, clean up.
scenario_applies_and_reloads() {
    reset_state
    printf 'PasswordAuthentication yes\n' >"$INSTALL_SSH_CONFIG_DIR/00-chasm.conf"
    HARDEN_SSH=1
    run_hardening
    [ "$hardening_status" -eq 0 ] || die "apply failed: status=$hardening_status
$hardening_output"
    case "$hardening_output" in
        *"disables SSH password and root login"*"hardening-approved:"*) ;;
        *) die "apply run must warn before the approval:
$hardening_output" ;;
    esac
    assert_contains "$hardening_output" "working SSH public key" "apply warning names the key requirement"
    assert_contains "$hardening_output" "reloaded ssh.service" "apply run names the reloaded unit"
    assert_contains "$hardening_output" "ssh hardening: applied" "apply run reports success"
    fragments_installed || die "apply run did not install the drop-ins"
    [ "$(reloads)" -eq 1 ] || die "apply run reloaded more than once"
    assert_backup_removed
}

# 3b. A .socket unit is never reloaded: with only ssh.socket active the
#     drop-ins are reported as taking effect for new connections.
scenario_socket_only_activation() {
    reset_state
    HARDEN_SSH=1
    SYSTEMCTL_MODE="socketonly"
    run_hardening
    [ "$hardening_status" -eq 0 ] || die "socket-only run failed: status=$hardening_status
$hardening_output"
    assert_contains "$hardening_output" "takes effect for new connections (socket activation" "socket-only message"
    assert_absent "$hardening_output" "reloaded" "socket-only run must not claim a reload"
    [ "$(reloads)" -eq 0 ] || die "socket-only run reloaded a unit"
    fragments_installed || die "socket-only run did not install the drop-ins"
    assert_backup_removed
}

# 4. sshd -t failure: restore the previous files exactly (a foreign drop-in
#    byte for byte, a dangling symlink as a symlink) and never reload.
scenario_sshd_t_failure_restores() {
    reset_state
    printf 'PasswordAuthentication yes\n' >"$INSTALL_SSH_CONFIG_DIR/00-chasm.conf"
    ln -s nowhere "$INSTALL_SSH_CONFIG_DIR/99-chasm.conf"
    HARDEN_SSH=1
    SSHD_MODE="fail"
    run_hardening
    [ "$hardening_status" -eq 1 ] || die "sshd -t failure did not fail: status=$hardening_status
$hardening_output"
    [ "$(cat "$INSTALL_SSH_CONFIG_DIR/00-chasm.conf")" = "PasswordAuthentication yes" ] ||
        die "sshd -t failure did not restore the previous 00 drop-in:
$(cat "$INSTALL_SSH_CONFIG_DIR/00-chasm.conf")"
    [ -L "$INSTALL_SSH_CONFIG_DIR/99-chasm.conf" ] || \
        die "sshd -t failure did not restore the previous 99 drop-in as a symlink"
    [ "$(reloads)" -eq 0 ] || die "sshd -t failure reloaded a unit"
    assert_contains "$hardening_output" "previous files were restored and sshd was not reloaded" "sshd -t failure message"
    assert_backup_removed
}

# 5. install failure mid-staging: the armed trap restores the previous files
#    and leaves no backup directory behind.
scenario_install_failure_restores() {
    reset_state
    printf 'PermitRootLogin yes\n' >"$INSTALL_SSH_CONFIG_DIR/00-chasm.conf"
    HARDEN_SSH=1
    FAIL_INSTALL_99=1
    run_hardening
    [ "$hardening_status" -ne 0 ] || die "install failure was reported as success:
$hardening_output"
    [ "$(cat "$INSTALL_SSH_CONFIG_DIR/00-chasm.conf")" = "PermitRootLogin yes" ] ||
        die "install failure did not restore the previous 00 drop-in:
$(cat "$INSTALL_SSH_CONFIG_DIR/00-chasm.conf")"
    if [ -e "$INSTALL_SSH_CONFIG_DIR/99-chasm.conf" ]; then
        die "install failure left a 99 drop-in behind"
    fi
    [ "$(reloads)" -eq 0 ] || die "install failure reloaded a unit"
    assert_backup_removed
}

# 6. Interruption after staging (SIGTERM from the sshd shim): the trap
#    restores the previous files, removes the backup, and the step stops.
scenario_signal_after_staging_restores() {
    reset_state
    printf 'PasswordAuthentication yes\n' >"$INSTALL_SSH_CONFIG_DIR/00-chasm.conf"
    HARDEN_SSH=1
    SSHD_MODE="kill"
    run_hardening
    [ "$hardening_status" -ne 0 ] || die "interrupted run was reported as success:
$hardening_output"
    [ "$(cat "$INSTALL_SSH_CONFIG_DIR/00-chasm.conf")" = "PasswordAuthentication yes" ] ||
        die "interrupted run did not restore the previous 00 drop-in:
$(cat "$INSTALL_SSH_CONFIG_DIR/00-chasm.conf")"
    if [ -e "$INSTALL_SSH_CONFIG_DIR/99-chasm.conf" ]; then
        die "interrupted run left a 99 drop-in behind"
    fi
    [ "$(reloads)" -eq 0 ] || die "interrupted run reloaded a unit"
    assert_absent "$hardening_output" "ssh hardening: applied" "interrupted run must not report success"
    assert_backup_removed
}

# 6b. The trap's applied-flag semantics (m1): once the step has applied --
#     a successful reload -- a late signal must NOT restore the previous
#     files (the daemon already loaded them); it only removes the backup.
#     The flag is exercised through the trap directly because a signal
#     delivered while `systemctl reload` is still the foreground child
#     lands before the step could set the flag at all; the staged-signal
#     scenario above covers that pre-apply window.
scenario_signal_after_reload_keeps_applied_files() {
    reset_state
    printf '%s\n' new-validated >"$INSTALL_SSH_CONFIG_DIR/00-chasm.conf"
    printf '%s\n' old >"$INSTALL_SSH_CONFIG_DIR/99-chasm.conf"
    ssh_hardening_backup=$(mktemp -d)
    printf '%s\n' previous >"$ssh_hardening_backup/00-chasm.conf"
    ssh_hardening_backed_up=" 00-chasm.conf"
    ssh_hardening_applied=1
    trap_status=0
    ( ssh_hardening_trap >"$WORK/state/trap.out" 2>&1 ) || trap_status=$?
    # The trap clears EXIT HUP INT TERM (its documented contract), so the
    # harness re-arms its own cleanup.
    trap 'rm -rf -- "$WORK"' EXIT HUP INT TERM
    [ "$trap_status" -eq 70 ] || die "applied trap must exit 70, got $trap_status"
    [ "$(cat "$INSTALL_SSH_CONFIG_DIR/00-chasm.conf")" = "new-validated" ] ||
        die "applied trap must not restore the previous 00 drop-in"
    [ "$(cat "$INSTALL_SSH_CONFIG_DIR/99-chasm.conf")" = "old" ] ||
        die "applied trap must not touch other fragments"
    assert_backup_removed

    # The staging state (applied=0) still restores.
    reset_state
    printf '%s\n' new-staged >"$INSTALL_SSH_CONFIG_DIR/00-chasm.conf"
    ssh_hardening_backup=$(mktemp -d)
    printf '%s\n' previous >"$ssh_hardening_backup/00-chasm.conf"
    ssh_hardening_backed_up=" 00-chasm.conf"
    ssh_hardening_applied=0
    trap_status=0
    ( ssh_hardening_trap >"$WORK/state/trap.out" 2>&1 ) || trap_status=$?
    trap 'rm -rf -- "$WORK"' EXIT HUP INT TERM
    [ "$trap_status" -eq 70 ] || die "staging trap must exit 70, got $trap_status"
    [ "$(cat "$INSTALL_SSH_CONFIG_DIR/00-chasm.conf")" = "previous" ] ||
        die "staging trap must restore the previous 00 drop-in"
    assert_backup_removed
}

# 7. No active ssh unit: explicit next-start message, no reload.
scenario_no_active_unit() {
    reset_state
    HARDEN_SSH=1
    SYSTEMCTL_MODE="none"
    run_hardening
    [ "$hardening_status" -eq 0 ] || die "no-active-unit run failed: status=$hardening_status
$hardening_output"
    assert_contains "$hardening_output" "installed; takes effect at next sshd start" "no-active-unit message"
    fragments_installed || die "no-active-unit run did not install the drop-ins"
    [ "$(reloads)" -eq 0 ] || die "no-active-unit run reloaded a unit"
    assert_backup_removed
}

# 8. Reload failure: restore the previous files and fail the step.
scenario_reload_failure_restores() {
    reset_state
    printf 'PasswordAuthentication yes\n' >"$INSTALL_SSH_CONFIG_DIR/00-chasm.conf"
    HARDEN_SSH=1
    SYSTEMCTL_MODE="reloadfail"
    run_hardening
    [ "$hardening_status" -eq 1 ] || die "reload failure did not fail: status=$hardening_status
$hardening_output"
    [ "$(cat "$INSTALL_SSH_CONFIG_DIR/00-chasm.conf")" = "PasswordAuthentication yes" ] ||
        die "reload failure did not restore the previous 00 drop-in:
$(cat "$INSTALL_SSH_CONFIG_DIR/00-chasm.conf")"
    assert_contains "$hardening_output" "could not reload ssh.service" "reload failure message"
    assert_backup_removed
}

# 8b. A foreign drop-in that sorts before 00-chasm.conf and sets a hardening
#     keyword is warned about on both the skip and the apply path.
scenario_winning_foreign_fragments_warned() {
    reset_state
    printf 'PasswordAuthentication no\n' >"$INSTALL_SSH_CONFIG_DIR/00-a-foreign.conf"
    run_hardening
    [ "$hardening_status" -eq 0 ] || die "skip path with foreign fragments failed: status=$hardening_status
$hardening_output"
    assert_contains "$hardening_output" "skipped (pass --harden-ssh to apply)" "skip line stays"
    assert_contains "$hardening_output" "00-a-foreign.conf" "skip warning names the foreign fragment"
    assert_contains "$hardening_output" "first value" "skip warning names the first-value rule"

    reset_state
    printf 'AuthorizedKeysFile .ssh/other-keys\n' >"$INSTALL_SSH_CONFIG_DIR/00-a-foreign.conf"
    HARDEN_SSH=1
    run_hardening
    [ "$hardening_status" -eq 0 ] || die "apply path with foreign fragments failed: status=$hardening_status
$hardening_output"
    case "$hardening_output" in
        *"00-a-foreign.conf"*"first value"*"hardening-approved:"*) ;;
        *) die "apply run must warn about the winning fragment before the approval:
$hardening_output" ;;
    esac
    fragments_installed || die "apply path did not install the drop-ins"
}

# 8c. Fragments that sort after 00-chasm.conf, or that set no hardening
#     keyword, are not conflicts.
scenario_non_conflicting_foreign_fragments_silent() {
    reset_state
    printf 'X11Forwarding no\n' >"$INSTALL_SSH_CONFIG_DIR/00-other.conf"
    printf 'PasswordAuthentication no\n' >"$INSTALL_SSH_CONFIG_DIR/50-other.conf"
    HARDEN_SSH=1
    run_hardening
    [ "$hardening_status" -eq 0 ] || die "apply with benign foreign fragments failed: status=$hardening_status
$hardening_output"
    assert_absent "$hardening_output" "first value" "benign fragments must not raise the conflict warning"
}

# 9. Installer argument contract: --harden-ssh only with --provision (exit
#    2 elsewhere); --help wins over any flag (exit 0).
scenario_installer_flag_contract() {
    for arguments in "--harden-ssh" "--check --harden-ssh" "--verify --harden-ssh"; do
        # shellcheck disable=SC2086
        installer_status=0
        installer_output=$(sh "$INSTALLER" $arguments </dev/null 2>&1) || installer_status=$?
        [ "$installer_status" -eq 2 ] ||
            die "'$arguments' must be a usage error (exit 2), got $installer_status:
$installer_output"
    done
    for arguments in "--help --harden-ssh" "--harden-ssh --help"; do
        # shellcheck disable=SC2086
        installer_status=0
        installer_output=$(sh "$INSTALLER" $arguments </dev/null 2>&1) || installer_status=$?
        [ "$installer_status" -eq 0 ] ||
            die "'$arguments' must print help (exit 0), got $installer_status:
$installer_output"
        case "$installer_output" in
            *"install-chasm.sh — provision"*) ;;
            *) die "'$arguments' did not print the help text:
$installer_output" ;;
        esac
    done
}

# 10. Drift probe: absent drop-ins are informational, present ones must
#     match, foreign winners are drift while a chasm fragment is present
#     and informational when the hardening is not applied.
scenario_drift_probe() {
    report_ok_count=0
    report_drift_count=0
    report_ok_lines=
    report_drift_lines=
    report_ok() {
        report_ok_count=$((report_ok_count + 1))
        report_ok_lines="$report_ok_lines
$1"
    }
    report_drift() {
        report_drift_count=$((report_drift_count + 1))
        report_drift_lines="$report_drift_lines
$1"
    }

    reset_state
    ssh_hardening_report report_ok report_drift
    [ "$report_drift_count" -eq 0 ] || die "absent optional drop-ins must not drift:$report_drift_lines"
    case "$report_ok_lines" in
        *"ssh hardening: not applied (optional)"*) ;;
        *) die "absent drop-ins must be reported as not applied (optional):$report_ok_lines" ;;
    esac

    reset_state
    mkdir -p "$INSTALL_SSH_CONFIG_DIR"
    cp "$FRAG00" "$FRAG99" "$INSTALL_SSH_CONFIG_DIR/"
    report_ok_count=0
    report_drift_count=0
    report_ok_lines=
    report_drift_lines=
    ssh_hardening_report report_ok report_drift
    [ "$report_drift_count" -eq 0 ] || die "matching drop-ins must not drift:$report_drift_lines"
    [ "$report_ok_count" -eq 2 ] || die "matching drop-ins report one ok line each: $report_ok_count"

    reset_state
    mkdir -p "$INSTALL_SSH_CONFIG_DIR"
    printf 'PasswordAuthentication yes\n' >"$INSTALL_SSH_CONFIG_DIR/00-chasm.conf"
    printf 'PermitRootLogin yes\n' >"$INSTALL_SSH_CONFIG_DIR/99-chasm.conf"
    report_ok_count=0
    report_drift_count=0
    report_ok_lines=
    report_drift_lines=
    ssh_hardening_report report_ok report_drift
    [ "$report_drift_count" -eq 2 ] || die "drifted drop-ins must drift:$report_drift_count"
    case "$report_drift_lines" in
        *"fragment drifted: 00-chasm.conf"*) ;;
        *) die "drifted fragment must be named:$report_drift_lines" ;;
    esac

    reset_state
    printf 'PasswordAuthentication no\n' >"$INSTALL_SSH_CONFIG_DIR/00-a-foreign.conf"
    report_ok_count=0
    report_drift_count=0
    report_ok_lines=
    report_drift_lines=
    ssh_hardening_report report_ok report_drift
    [ "$report_drift_count" -eq 0 ] ||
        die "absent drop-ins with a foreign winner must not drift:$report_drift_lines"
    case "$report_ok_lines" in
        *"ssh hardening: not applied (optional)"*) ;;
        *) die "absent drop-ins with a foreign winner stay informational:$report_ok_lines" ;;
    esac
    case "$report_ok_lines" in
        *"00-a-foreign.conf"*"first-value rule"*"informational because the chasm hardening is not applied"*) ;;
        *) die "absent drop-ins with a foreign winner need an informational note naming the fragment:$report_ok_lines" ;;
    esac

    reset_state
    mkdir -p "$INSTALL_SSH_CONFIG_DIR"
    cp "$FRAG00" "$FRAG99" "$INSTALL_SSH_CONFIG_DIR/"
    printf 'PasswordAuthentication no\n' >"$INSTALL_SSH_CONFIG_DIR/00-a-foreign.conf"
    report_ok_count=0
    report_drift_count=0
    report_ok_lines=
    report_drift_lines=
    ssh_hardening_report report_ok report_drift
    [ "$report_drift_count" -eq 1 ] ||
        die "installed drop-ins with a foreign winner must drift once:$report_drift_count"
    case "$report_drift_lines" in
        *"00-a-foreign.conf"*"first-value rule"*) ;;
        *) die "winning foreign fragment against installed drop-ins must be drift:$report_drift_lines" ;;
    esac
}

# 11. The library asks through approve_hardening (the explicit typed-yes
#     entry point), never through plain approve.
scenario_explicit_approval_path() {
    reset_state
    HARDEN_SSH=1
    approve() {
        echo "plain approve must not be used for the hardening step" >&2
        exit 99
    }
    run_hardening
    unset -f approve
    approve() { echo "approved: $1"; }
    [ "$hardening_status" -eq 0 ] || die "apply through approve_hardening failed: status=$hardening_status
$hardening_output"
    assert_contains "$hardening_output" "hardening-approved:" "apply run used the explicit approval"
}

# 12. The installer's approve_hardening has no CHASM_ASSUME_YES fast path
#     and requires a typed confirmation on the terminal. (A functional
#     invocation would need a real tty, so the contract is asserted on the
#     extracted function body: no environment shortcut, a /dev/tty prompt,
#     and an exact-yes acceptance.)
scenario_installer_hardening_approval_is_always_typed() {
    approve_body=$(sed -n '/^approve_hardening()/,/^}/p' "$INSTALLER")
    [ -n "$approve_body" ] || die "could not extract approve_hardening from $INSTALLER"
    case "$approve_body" in
        *'${CHASM_ASSUME_YES'*|*'CHASM_ASSUME_YES='*|*'\$CHASM_ASSUME_YES'*)
            die "approve_hardening must not read CHASM_ASSUME_YES (only name it in the refusal message)"
            ;;
    esac
    case "$approve_body" in
        *'/dev/tty'*) ;;
        *) die "approve_hardening must prompt on /dev/tty" ;;
    esac
    case "$approve_body" in
        *'yes)'*) ;;
        *) die "approve_hardening must accept exactly a typed yes" ;;
    esac
}

# 13. The installer's trap hand-off: install_payload finishes its staging
#     cleanup and clears its own trap BEFORE the hardening step runs, so a
#     --harden-ssh provision leaves neither a staging directory nor a
#     backup. The sequence below mirrors install_payload's cleanup lines.
scenario_payload_staging_cleanup_survives_hardening() {
    reset_state
    HARDEN_SSH=1
    (
        staging_dir=$(mktemp -d)
        cleanup_staging() { rm -rf "$staging_dir"; }
        trap cleanup_staging EXIT HUP INT TERM
        # ... the payload's own work would run here ...
        rm -rf "$staging_dir"
        trap - EXIT HUP INT TERM
        install_ssh_hardening >/dev/null 2>&1
    )
    [ "$(reloads)" -eq 1 ] || die "staging hand-off scenario did not reach the reload"
    if backup_left_behind; then
        die "the staging hand-off sequence left files behind in $WORK/tmp:
$(ls -A "$WORK/tmp")"
    fi
}

# 14. The Include precondition follows INSTALL_SSH_CONFIG_DIR: a main
#     config whose Include names a different directory is refused before
#     anything is staged.
scenario_include_precondition_follows_config_dir() {
    reset_state
    printf 'Include /etc/ssh/sshd_config.d/*.conf\n' >"$WORK/etc/sshd_config"
    HARDEN_SSH=1
    run_hardening
    [ "$hardening_status" -eq 1 ] || die "mismatched Include line was accepted: status=$hardening_status
$hardening_output"
    assert_contains "$hardening_output" "has no 'Include" "mismatched Include message"
    assert_contains "$hardening_output" "$INSTALL_SSH_CONFIG_DIR/*.conf" "mismatched Include message names the expected directory"
    [ -z "$(ls -A "$INSTALL_SSH_CONFIG_DIR" 2>/dev/null)" ] ||
        die "refused Include must not stage anything"
    assert_backup_removed
}

# 15. A backup copy that fails (cp shim) stops the step BEFORE anything is
#     changed: the original drop-ins stay byte-identical, no hardening
#     fragment is installed, nothing is reloaded, and no backup directory
#     is left behind.
scenario_backup_failure_changes_nothing() {
    reset_state
    printf 'PasswordAuthentication yes\n' >"$INSTALL_SSH_CONFIG_DIR/00-chasm.conf"
    printf 'PermitRootLogin yes\n' >"$INSTALL_SSH_CONFIG_DIR/99-chasm.conf"
    HARDEN_SSH=1
    FAIL_BACKUP_99=1
    run_hardening
    [ "$hardening_status" -eq 1 ] || die "backup failure was reported as success: status=$hardening_status
$hardening_output"
    [ "$(cat "$INSTALL_SSH_CONFIG_DIR/00-chasm.conf")" = "PasswordAuthentication yes" ] ||
        die "backup failure altered the previous 00 drop-in:
$(cat "$INSTALL_SSH_CONFIG_DIR/00-chasm.conf")"
    [ "$(cat "$INSTALL_SSH_CONFIG_DIR/99-chasm.conf")" = "PermitRootLogin yes" ] ||
        die "backup failure altered the previous 99 drop-in:
$(cat "$INSTALL_SSH_CONFIG_DIR/99-chasm.conf")"
    [ "$(reloads)" -eq 0 ] || die "backup failure reloaded a unit"
    assert_contains "$hardening_output" "could not complete and verify the backup of" "backup failure message"
    assert_contains "$hardening_output" "left unchanged" "backup failure says nothing was changed"
    assert_backup_removed
}

# 16. A backup copy that "succeeds" with different bytes is caught by the
#     cmp verification; the originals stay byte-identical and nothing else
#     happens.
scenario_backup_verification_failure_changes_nothing() {
    reset_state
    printf 'PasswordAuthentication yes\n' >"$INSTALL_SSH_CONFIG_DIR/00-chasm.conf"
    printf 'PermitRootLogin yes\n' >"$INSTALL_SSH_CONFIG_DIR/99-chasm.conf"
    HARDEN_SSH=1
    CORRUPT_BACKUP_99=1
    run_hardening
    [ "$hardening_status" -eq 1 ] || die "corrupt backup was reported as success: status=$hardening_status
$hardening_output"
    [ "$(cat "$INSTALL_SSH_CONFIG_DIR/00-chasm.conf")" = "PasswordAuthentication yes" ] ||
        die "corrupt backup altered the previous 00 drop-in:
$(cat "$INSTALL_SSH_CONFIG_DIR/00-chasm.conf")"
    [ "$(cat "$INSTALL_SSH_CONFIG_DIR/99-chasm.conf")" = "PermitRootLogin yes" ] ||
        die "corrupt backup altered the previous 99 drop-in:
$(cat "$INSTALL_SSH_CONFIG_DIR/99-chasm.conf")"
    [ "$(reloads)" -eq 0 ] || die "corrupt backup reloaded a unit"
    assert_contains "$hardening_output" "could not complete and verify the backup of" "verification failure message"
    assert_backup_removed
}

# 17. A rollback whose recorded list names a fragment whose backup is
#     missing must report it (instead of treating absence as "no original"
#     and deleting the live fragment), restore the recorded fragments that
#     DO have backups, remove the backup directory, and fail.
scenario_rollback_missing_recorded_backup_fails() {
    reset_state
    printf '%s\n' staged-00 >"$INSTALL_SSH_CONFIG_DIR/00-chasm.conf"
    printf '%s\n' staged-99 >"$INSTALL_SSH_CONFIG_DIR/99-chasm.conf"
    ssh_hardening_backup=$(mktemp -d)
    printf '%s\n' previous-00 >"$ssh_hardening_backup/00-chasm.conf"
    # 99-chasm.conf is RECORDED as backed up, but its backup is missing.
    ssh_hardening_backed_up=" 00-chasm.conf 99-chasm.conf"
    ssh_hardening_applied=0
    trap_status=0
    ( ssh_hardening_trap >"$WORK/state/trap.out" 2>&1 ) || trap_status=$?
    trap 'rm -rf -- "$WORK"' EXIT HUP INT TERM
    [ "$trap_status" -eq 70 ] || die "missing-backup rollback must exit 70, got $trap_status"
    assert_contains "$(cat "$WORK/state/trap.out")" "recorded backup of 99-chasm.conf is missing" "missing recorded backup is reported"
    [ "$(cat "$INSTALL_SSH_CONFIG_DIR/00-chasm.conf")" = "previous-00" ] ||
        die "rollback must restore the fragment whose backup exists"
    [ "$(cat "$INSTALL_SSH_CONFIG_DIR/99-chasm.conf")" = "staged-99" ] ||
        die "rollback must NOT delete a live fragment whose recorded backup is missing"
    assert_backup_removed
}

# 18. The reload-to-applied window is closed: a SIGTERM delivered right
#     after a successful reload (the systemctl shim signals its parent
#     during reload) is deferred/ignored until the applied flag is
#     recorded, so the installed drop-ins stay in place and the step
#     succeeds.
scenario_reload_signal_window_closed() {
    reset_state
    printf 'PasswordAuthentication yes\n' >"$INSTALL_SSH_CONFIG_DIR/00-chasm.conf"
    HARDEN_SSH=1
    SYSTEMCTL_MODE="reloadsignal"
    run_hardening
    [ "$hardening_status" -eq 0 ] || die "signal right after the reload must not abort the step: status=$hardening_status
$hardening_output"
    fragments_installed || die "post-reload signal must not restore the installed drop-ins"
    [ "$(reloads)" -eq 1 ] || die "the reload must have happened exactly once"
    assert_backup_removed
}

# 19. The socket-only window is closed too: a SIGTERM delivered while the
#     drop-ins are being reported as in force (the systemctl shim signals
#     its parent from the ssh.socket probe) is deferred/ignored until
#     applied is recorded and the trap disarmed, so the drop-ins stay in
#     place and the step succeeds.
scenario_socket_only_signal_window_closed() {
    reset_state
    HARDEN_SSH=1
    SYSTEMCTL_MODE="socketsignal"
    run_hardening
    [ "$hardening_status" -eq 0 ] || die "signal on the socket-only path must not abort the step: status=$hardening_status
$hardening_output"
    assert_contains "$hardening_output" "takes effect for new connections (socket activation" "socket-only message"
    [ "$(reloads)" -eq 0 ] || die "socket-only run reloaded a unit"
    fragments_installed || die "socket-only signal must not restore the installed drop-ins"
    assert_backup_removed
}

SCENARIOS=0
for scenario in \
    scenario_default_skip \
    scenario_default_skip_leaves_installed \
    scenario_default_skip_revalidates_installed \
    scenario_applies_and_reloads \
    scenario_socket_only_activation \
    scenario_sshd_t_failure_restores \
    scenario_install_failure_restores \
    scenario_signal_after_staging_restores \
    scenario_signal_after_reload_keeps_applied_files \
    scenario_no_active_unit \
    scenario_reload_failure_restores \
    scenario_winning_foreign_fragments_warned \
    scenario_non_conflicting_foreign_fragments_silent \
    scenario_installer_flag_contract \
    scenario_drift_probe \
    scenario_explicit_approval_path \
    scenario_installer_hardening_approval_is_always_typed \
    scenario_payload_staging_cleanup_survives_hardening \
    scenario_include_precondition_follows_config_dir \
    scenario_backup_failure_changes_nothing \
    scenario_backup_verification_failure_changes_nothing \
    scenario_rollback_missing_recorded_backup_fails \
    scenario_reload_signal_window_closed \
    scenario_socket_only_signal_window_closed; do
    "$scenario"
    SCENARIOS=$((SCENARIOS + 1))
    echo "ok    $scenario"
done
echo "test-ssh-hardening: $SCENARIOS scenarios passed"
