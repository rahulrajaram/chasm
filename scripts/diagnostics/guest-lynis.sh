#!/bin/sh
# Run inside a disposable VM. Only the completed Lynis report goes to stdout.
set -eu
umask 077
[ "$#" -eq 0 ] || { echo 'usage: guest-lynis.sh' >&2; exit 64; }
[ "$(id -u)" = 0 ] || { echo 'guest root is required for this audit' >&2; exit 1; }
case $(systemd-detect-virt --vm) in
    kvm|qemu) ;;
    *) echo 'expected a KVM or QEMU guest' >&2; exit 1 ;;
esac
command -v lynis >/dev/null || { echo 'install the Debian lynis package in the audit guest first' >&2; exit 1; }
work=$(mktemp -d /var/tmp/guest-lynis.XXXXXXXX)
trap 'rm -rf -- "$work"' EXIT
trap 'exit 1' HUP INT TERM
lynis audit system --quick --no-colors --logfile "$work/lynis.log" \
    --report-file "$work/report.dat" >&2
grep -Eq '^hardening_index=[0-9]+$' "$work/report.dat" || {
    echo 'Lynis did not produce a scored report' >&2
    exit 1
}
cat "$work/report.dat"
