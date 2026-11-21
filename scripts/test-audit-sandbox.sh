#!/bin/sh
# Run the collector with a simulated site and strict guest command responses.
set -eu
root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT
mkdir -p "$work/bin" "$work/scripts/diagnostics" "$work/scripts/lib"
cp "$root"/scripts/diagnostics/*.sh "$work/scripts/diagnostics/"
printf '%s\n' 'require_site_host() { SITE_RESOURCE_PREFIX=trial; }' > "$work/scripts/lib/require-site-host.sh"
cat > "$work/bin/sudo" <<'MOCK'
#!/bin/sh
set -eu
[ "$1 $2 $3 $4" = 'incus --force-local --project default' ] || exit 95
shift 4
case "$*" in
    'list sandbox-trial --format csv -c nst') echo 'sandbox-trial,RUNNING,VIRTUAL-MACHINE' ;;
    'config get sandbox-trial user.trial.managed') echo true ;;
    'config get sandbox-trial user.trial.logical-name') echo trial ;;
    'config get sandbox-trial volatile.uuid') echo '11111111-1111-4111-8111-111111111111' ;;
    'exec sandbox-trial -T -- cat /proc/sys/kernel/random/boot_id')
        if [ "$scenario" = reboot ] && [ -f scanned ]; then
            echo '33333333-3333-4333-8333-333333333333'
        else
            echo '22222222-2222-4222-8222-222222222222'
        fi ;;
    'exec sandbox-trial -T -- sh -ec mountpoint -q /workspace && mountpoint -q /shared') ;;
    'exec sandbox-trial -T -- dpkg-query -W lynis') printf 'lynis\t3.0\n' ;;
    'exec sandbox-trial -T -- sh -s')
        cmp - "$guest_script" || exit 96
        touch scanned
        echo 'lynis console' >&2
        echo 'hardening_index=70'
        [ "$scenario" != failed ] || exit 23 ;;
    *) echo "unexpected incus arguments: $*" >&2; exit 97 ;;
esac
MOCK
chmod +x "$work/bin/sudo"
PATH="$work/bin:$PATH"
guest_script=$root/scripts/diagnostics/guest-lynis.sh
GIT_DIR=$(git -C "$root" rev-parse --absolute-git-dir)
export PATH scenario guest_script GIT_DIR
for scenario in success failed reboot; do
    mkdir "$work/$scenario"
    cd "$work/$scenario"
    status=0
    "$work/scripts/diagnostics/audit-sandbox.sh" trial > stdout 2> stderr || status=$?
    set -- ./lynis-evidence.*
    [ "$#" -eq 1 ]
    [ -d "$1" ]
    [ -f scanned ]
    evidence=$1
    grep -Fxq 'lynis console' "$evidence/console.log"
    case "$scenario" in
        success)
            [ "$status" -eq 0 ]
            [ "$(cat stdout)" = "$evidence/report.dat" ]
            [ ! -e "$evidence/report.partial" ]
            grep -Fxq 'hardening_index=70' "$evidence/report.dat"
            cmp "$evidence/identity.before" "$evidence/identity.after" ;;
        failed|reboot)
            [ ! -e "$evidence/report.dat" ]
            grep -Fxq 'hardening_index=70' "$evidence/report.partial"
            if [ "$scenario" = failed ]; then
                [ "$status" -eq 23 ] && [ ! -e "$evidence/identity.after" ] || exit 1
            else
                [ "$status" -ne 0 ]
                grep -Fxq '33333333-3333-4333-8333-333333333333' "$evidence/identity.after"
            fi ;;
    esac
done
echo "test-audit-sandbox: success, scan failure, and guest reboot passed"
