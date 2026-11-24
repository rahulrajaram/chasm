#!/bin/sh
umask 022
set -eu

# Repo-local, unprivileged regression harness for the fixed builder IPv4
# address claim check (scripts/lib/incus-ipv4-claims.sh).
#
# The template builder gives its VM a fixed address (sandbox_ipv4_prefix +
# 11) and never reselects it, so an instance created by an older build may
# already hold that address. The shared claim inventory refuses the build,
# naming the claiming instance, when any other instance claims the address
# through an eth0 ipv4.address override, the site's
# user.<prefix>.instance-ipv4 metadata, or a live address of a RUNNING or
# FROZEN instance.
#
# The harness sources the real claim library and drives its verdicts
# against a PATH-shimmed incus (inventory, device overrides, and metadata
# come from exported CLAIM_* variables), so no root, no Incus, and no host
# state is involved:
#
#   1. a free address proceeds (including a stopped instance whose stale
#      live column still shows the candidate: live claims only count while
#      an instance actually runs);
#   2. an eth0 ipv4.address override on another instance refuses, naming
#      the instance and the override;
#   3. user.<prefix>.instance-ipv4 metadata on another instance refuses,
#      naming the instance and the metadata key (for the chasm prefix and,
#      separately, for a site whose resource_prefix is agent-sandbox);
#   4. a live address of a RUNNING instance refuses, naming the instance
#      and the live claim (a FROZEN instance claims the same way); the
#      live-address inventory uses the shape Incus really renders, where
#      several IPv4 addresses arrive newline-joined inside ONE quoted CSV
#      field, and a claim on a continuation line (not the first address)
#      is caught too;
#   5. the instance being checked itself (a resumed builder) is skipped,
#      so a preserved builder holding its own address still proceeds;
#   6. an unreadable address inventory, and a missing candidate, both fail
#      closed;
#   7. wiring gate: the builder runs the refusal with its fixed
#      BUILDER_IPV4 before `incus init`, and both privileged callers
#      (builder and launcher) source the shared library.
#
# The fake incus rejects any invocation other than the exact production
# calls (`list --format csv -c ns4`, `config device get NAME eth0
# ipv4.address`, and `config get NAME user.<prefix>.instance-ipv4`), so a
# fail-open mutation of the read paths cannot pass silently.
#
# Usage: scripts/test-builder-address-claim.sh   (repository root; no arguments)

[ "$#" -eq 0 ] || { echo "usage: $0" >&2; exit 2; }

ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
BUILDER=$ROOT/scripts/build-sandbox-template.sh
LAUNCHER=$ROOT/scripts/launch-sandbox-vm.sh

die() {
    echo "test-builder-address-claim: FAIL: $1" >&2
    exit 1
}

# shellcheck source=scripts/lib/incus-names.sh
. "$ROOT/scripts/lib/incus-names.sh"
# shellcheck source=scripts/lib/incus-ipv4-claims.sh
. "$ROOT/scripts/lib/incus-ipv4-claims.sh"

WORK=$(mktemp -d)
trap 'rm -rf -- "$WORK"' EXIT HUP INT TERM
mkdir -p "$WORK/bin"

# --- fake incus --------------------------------------------------------------
# State comes from exported CLAIM_* variables: CLAIM_INSTANCES carries the
# `incus list --format csv -c ns4` inventory (NAME,STATUS,addresses records,
# with quoted multi-line address fields exactly as Incus renders them),
# CLAIM_OVERRIDES the eth0 ipv4.address overrides (NAME=addr per line),
# CLAIM_METADATA the user.<prefix>.instance-ipv4 values, and
# CLAIM_LIST_FAIL makes the inventory unreadable. Any invocation other than
# the exact production calls is rejected with exit 9.
cat >"$WORK/bin/incus" <<'STUB'
#!/bin/sh
set -eu
METADATA_KEY=${CLAIM_METADATA_KEY:-user.chasm.instance-ipv4}
if [ "$#" -eq 5 ] && [ "$1" = list ] && [ "$2" = --format ] && \
    [ "$3" = csv ] && [ "$4" = -c ] && [ "$5" = ns4 ]; then
    if [ "${CLAIM_LIST_FAIL:-0}" = 1 ]; then
        echo "Error: inventory unavailable" >&2
        exit 1
    fi
    [ -n "${CLAIM_INSTANCES:-}" ] && printf '%s\n' "$CLAIM_INSTANCES"
    exit 0
fi
if [ "$#" -eq 6 ] && [ "$1" = config ] && [ "$2" = device ] && [ "$3" = get ] && \
    [ "$5" = eth0 ] && [ "$6" = ipv4.address ]; then
    printf '%s\n' "${CLAIM_OVERRIDES:-}" | awk -F= -v n="$4" '$1 == n { print $2 }'
    exit 0
fi
if [ "$#" -eq 4 ] && [ "$1" = config ] && [ "$2" = get ] && [ "$4" = "$METADATA_KEY" ]; then
    printf '%s\n' "${CLAIM_METADATA:-}" | awk -F= -v n="$3" '$1 == n { print $2 }'
    exit 0
fi
echo "fake incus: unsupported invocation: $*" >&2
exit 9
STUB
chmod 0755 "$WORK/bin/incus"
export PATH="$WORK/bin:$PATH"

PREFIX=chasm
CANDIDATE=10.99.0.11
SELF=
export CLAIM_METADATA_KEY=user.$PREFIX.instance-ipv4
export CLAIM_INSTANCES CLAIM_OVERRIDES CLAIM_METADATA CLAIM_LIST_FAIL

reset_state() {
    CLAIM_INSTANCES=
    CLAIM_OVERRIDES=
    CLAIM_METADATA=
    CLAIM_LIST_FAIL=0
}

expect_free() { # label
    claim_status=0
    claim_refusal=
    claim_refusal=$(ipv4_claim_refusal "$PREFIX" "$CANDIDATE" "$SELF") ||
        claim_status=$?
    [ "$claim_status" -eq 0 ] && [ -z "$claim_refusal" ] ||
        die "$1: expected the address to be free, got status=$claim_status refusal='$claim_refusal'"
}

expect_refusal() { # label want-substring
    claim_label=$1
    claim_want=$2
    claim_status=0
    claim_refusal=$(ipv4_claim_refusal "$PREFIX" "$CANDIDATE" "$SELF") ||
        claim_status=$?
    [ "$claim_status" -eq 1 ] ||
        die "$claim_label: expected a refusal, got status=$claim_status refusal='$claim_refusal'"
    case "$claim_refusal" in
        *"$claim_want"*) ;;
        *) die "$claim_label: refusal '$claim_refusal' does not mention '$claim_want'" ;;
    esac
}

# --- 1. free address proceeds ------------------------------------------------

reset_state
CLAIM_INSTANCES='sandbox-a,RUNNING,"10.99.0.30 (eth0)
10.99.0.31 (eth1)"
sandbox-stopped,STOPPED,
sandbox-stale,STOPPED,10.99.0.11 (eth0)'
CLAIM_OVERRIDES='sandbox-a=10.99.0.30'
CLAIM_METADATA='sandbox-a=10.99.0.30'
expect_free "free address proceeds (quoted multi-line live field, stopped stale live column)"

# --- 2. eth0 ipv4.address override refuses -----------------------------------

reset_state
CLAIM_INSTANCES='sandbox-old,STOPPED,'
CLAIM_OVERRIDES='sandbox-old=10.99.0.11'
expect_refusal "override claim refuses" "eth0 ipv4.address override"
expect_refusal "override claim names the instance" "sandbox-old"

# --- 3. instance-ipv4 metadata refuses ---------------------------------------

reset_state
CLAIM_INSTANCES='sandbox-old,RUNNING,10.99.0.20 (eth0)'
CLAIM_OVERRIDES='sandbox-old=10.99.0.20'
CLAIM_METADATA='sandbox-old=10.99.0.11'
expect_refusal "metadata claim refuses" "user.chasm.instance-ipv4 metadata"
expect_refusal "metadata claim names the instance" "sandbox-old"

# The metadata key follows the site's resource_prefix: a site whose prefix is
# agent-sandbox must be checked against user.agent-sandbox.instance-ipv4.
reset_state
PREFIX=agent-sandbox
CLAIM_METADATA_KEY=user.agent-sandbox.instance-ipv4
CLAIM_INSTANCES='sandbox-old,RUNNING,10.99.0.20 (eth0)'
CLAIM_METADATA='sandbox-old=10.99.0.11'
expect_refusal "agent-sandbox metadata refuses" "user.agent-sandbox.instance-ipv4 metadata"
expect_refusal "agent-sandbox metadata names the instance" "sandbox-old"
PREFIX=chasm
CLAIM_METADATA_KEY=user.chasm.instance-ipv4

# --- 4. live address of a running instance refuses ---------------------------

reset_state
CLAIM_INSTANCES='sandbox-live,RUNNING,10.99.0.11 (eth0)'
expect_refusal "live claim refuses" "live address"
expect_refusal "live claim names the instance" "sandbox-live"

reset_state
CLAIM_INSTANCES='sandbox-frozen,FROZEN,10.99.0.11 (eth0)'
expect_refusal "frozen live claim refuses" "sandbox-frozen"

# The real Incus shape (with the production `-c ns4` columns): several IPv4
# addresses arrive newline-joined inside ONE quoted CSV field, so the
# claimed address is on a continuation line of the record, not the first
# address of the instance.
reset_state
CLAIM_INSTANCES='sandbox-development,Running,"172.17.0.1 (docker0)
10.99.0.11 (enp5s0)"'
expect_refusal "multi-address live claim (second position) refuses" "live address"
expect_refusal "multi-address live claim names the instance" "sandbox-development"

# --- 5. the checked instance itself is skipped -------------------------------

reset_state
CLAIM_INSTANCES='as-build-demo123,STOPPED,
sandbox-a,RUNNING,10.99.0.20 (eth0)'
CLAIM_OVERRIDES='as-build-demo123=10.99.0.11'
SELF=as-build-demo123
expect_free "a resumed builder holding its own address is skipped"
SELF=

# --- 6. unreadable inventory and missing candidate fail closed ---------------

reset_state
CLAIM_LIST_FAIL=1
expect_refusal "unreadable inventory refuses" "inventory is unavailable"

reset_state
missing_status=0
missing_refusal=$(ipv4_claim_refusal "$PREFIX" "" "") || missing_status=$?
[ "$missing_status" -eq 1 ] && [ "$missing_refusal" = "eth0 IPv4 claim is missing" ] ||
    die "missing candidate: got status=$missing_status refusal='$missing_refusal'"

# --- 7. wiring: the builder refuses before creating, both callers share -------

for wired_script in "$BUILDER" "$LAUNCHER"; do
    grep -q 'lib/incus-ipv4-claims.sh' "$wired_script" ||
        die "$(basename "$wired_script") does not source the shared claim library"
done
builder_check_line=$(grep -n 'ipv4_claim_refusal "$resource_prefix" "$BUILDER_IPV4"' "$BUILDER" | cut -d: -f1 | head -n 1)
builder_init_line=$(grep -n 'incus init ' "$BUILDER" | cut -d: -f1 | head -n 1)
[ -n "$builder_check_line" ] && [ -n "$builder_init_line" ] &&
    [ "$builder_check_line" -lt "$builder_init_line" ] ||
    die "build-sandbox-template.sh must refuse the builder address before incus init"
grep -q 'ipv4_claim_refusal "$resource_prefix"' "$LAUNCHER" ||
    die "launch-sandbox-vm.sh must route its claim check through the shared library"

# The builder must resolve and copy a fingerprint from the same configured
# remote, including when that remote is not the usual "images" remote.
sed -n '/^if ! incus image info "$BASE_IMAGE_ALIAS"/,/^fi$/p' "$BUILDER" >"$WORK/image-import.sh"
grep -q '^if ! incus image info ' "$WORK/image-import.sh" ||
    die "could not extract the builder image import"
image_fingerprint=0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
(
    # Used by the extracted builder block.
    # shellcheck disable=SC2034
    BASE_IMAGE_ALIAS=chasm-test-13
    # Used by the extracted builder block.
    # shellcheck disable=SC2034
    SITE_BASE_IMAGE_REMOTE=other:debian/13/cloud
    # Used by the extracted builder block.
    # shellcheck disable=SC2034
    check_only=0
    incus() {
        printf '%s\n' "$*" >>"$WORK/image-calls"
        case "$*" in
            'image info chasm-test-13') return 1 ;;
            'image info --vm other:debian/13/cloud')
                if [ ! -f "$WORK/first-lookup" ]; then
                    touch "$WORK/first-lookup"
                    return 1
                fi
                printf 'Fingerprint: %s\n' "$image_fingerprint"
                ;;
            "image copy other:$image_fingerprint local: --alias chasm-test-13 --auto-update=false") return 0 ;;
            *) return 9 ;;
        esac
    }
    sleep() { :; }
    fail() { die "$*"; }
    # Extracted builder block under mocked Incus.
    # shellcheck disable=SC1090
    . "$WORK/image-import.sh"
) || die "the builder image import failed with a configured remote"
[ "$(grep -Fxc 'image info --vm other:debian/13/cloud' "$WORK/image-calls")" -eq 2 ] ||
    die "the builder did not retry the configured remote"
grep -Fx "image copy other:$image_fingerprint local: --alias chasm-test-13 --auto-update=false" "$WORK/image-calls" >/dev/null ||
    die "the builder copied the fingerprint from a different remote"

# Execute builder startup, including its real failure trap. Each injected
# failure must leave the matching instance state and receipt behind.
awk '/^builder_created=0$/{copy=1} copy{print}
    copy && /^incus exec .* -- cloud-init status --wait$/{exit}' "$BUILDER" > "$WORK/builder-start.sh"
cat > "$WORK/start-driver.sh" <<'DRIVER'
set -eu
receipt=$WORK/start-receipt
builder_name=as-build-test
template_name=trial-template
template_identity=trial-identity
resume_builder=
PROFILE_NAME=trial-profile
BASE_IMAGE_ALIAS=trial-image
BUILDER_IPV4=example-address
resumed_from_identity=
builder_release_lifecycle_lock() { :; }
metadata() { echo "user.trial.$1"; }
fail() { exit 71; }
incus() {
    case "$*" in
        'init trial-image as-build-test --vm --profile trial-profile') echo STOPPED > "$WORK/start-state" ;;
        'config device override as-build-test eth0 ipv4.address=example-address'|\
        'config set as-build-test boot.autostart=false'|\
        'config set as-build-test user.trial.template-build-identity=trial-identity') ;;
        'start as-build-test')
            [ "$stage" != before ] || return 71
            echo RUNNING > "$WORK/start-state" ;;
        'info as-build-test') printf 'Status: %s\n' "$(cat "$WORK/start-state")" ;;
        'stop as-build-test --timeout 60') echo STOPPED > "$WORK/start-state" ;;
        'delete as-build-test') rm "$WORK/start-state" ;;
        'exec as-build-test -- true') ;;
        'exec as-build-test -- cloud-init status --wait') return 72 ;;
        *) echo "unexpected builder call: $*" >&2; exit 98 ;;
    esac
}
. "$WORK/builder-start.sh"
DRIVER
export WORK
for stage in before after; do
    : > "$WORK/start-receipt"
    status=0
    stage=$stage sh "$WORK/start-driver.sh" || status=$?
    case "$stage" in
        before)
            [ "$status" -eq 71 ] && [ ! -e "$WORK/start-state" ] &&
                grep -Fxq 'discarded_unbooted_builder=as-build-test' "$WORK/start-receipt" ||
                die "failed first boot did not discard the builder" ;;
        after)
            [ "$status" -eq 72 ] && [ "$(cat "$WORK/start-state")" = STOPPED ] &&
                grep -Fxq 'preserved_builder=as-build-test' "$WORK/start-receipt" ||
                die "failure after first boot did not preserve the stopped builder" ;;
    esac
done

# Simulate an acknowledged rename, a lost reply, a hard stop immediately
# after the server rename, and a collision with another build. The mock
# observes the source instance and receipt at publication time.
sed -n '/^publish_sealed_builder() {/,/^}/p' "$BUILDER" > "$WORK/publish.sh"
sed -n '/^echo "template build: phase=seal"/,$p' "$BUILDER" > "$WORK/seal.sh"
cat > "$WORK/seal-driver.sh" <<'DRIVER'
set -eu
builder_name=as-build-test
template_name=trial-template
template_identity=trial-identity
BOOTSTRAP_PROFILE=trial-profile
TEMPLATE_PROFILE_MANIFEST_SHA256=trial-digest
resume_builder=
receipt="$PWD/receipt"
printf '%s\n' "template_identity=$template_identity" "template_name=$template_name" result=pending > "$receipt"
mkdir "$builder_name"
metadata() { echo "user.trial.$1"; }
fail() { echo "$*" >&2; exit 74; }
incus() {
    case "$*" in
        'stop as-build-test --timeout 60') touch "$builder_name/stopped" ;;
        'config device unset as-build-test eth0 ipv4.address') touch "$builder_name/neutral" ;;
        'config device get as-build-test eth0 ipv4.address') test -f "$builder_name/neutral" ;;
        'config set as-build-test user.trial.template-identity=trial-identity'|\
        'config set as-build-test user.trial.bootstrap-profile=trial-profile'|\
        'config set as-build-test user.trial.profile-manifest-sha256=trial-digest') ;;
        "config set as-build-test user.trial.template-receipt=$receipt") echo "$receipt" > "$builder_name/receipt" ;;
        'snapshot create as-build-test sealed') touch "$builder_name/sealed" ;;
        'config set as-build-test security.protection.delete=true') touch "$builder_name/protected" ;;
        'info as-build-test') test -f "$builder_name/stopped" && echo 'Status: STOPPED' ;;
        'move as-build-test trial-template')
            for key in stopped neutral sealed protected receipt; do
                [ -f "$builder_name/$key" ] || exit 98
            done
            grep -Fxq result=passed "$receipt" || exit 98
            if [ "$scenario" = collision ]; then
                mkdir "$template_name"
                echo another-receipt > "$template_name/receipt"
                return 1
            fi
            mv "$builder_name" "$template_name"
            case "$scenario" in
                lost) return 1 ;;
                power) kill -KILL "$$" ;;
            esac ;;
        'config get trial-template user.trial.template-receipt') cat "$template_name/receipt" ;;
        *) echo "unexpected sealing call: $*" >&2; exit 98 ;;
    esac
}
. "$WORK/publish.sh"
. "$WORK/seal.sh"
DRIVER
for scenario in acknowledged lost power collision; do
    mkdir "$WORK/$scenario"
    status=0
    (cd "$WORK/$scenario" && scenario=$scenario exec sh "$WORK/seal-driver.sh") || status=$?
    if [ "$scenario" = collision ]; then
        [ "$status" -eq 74 ] && [ "$(cat "$WORK/$scenario/trial-template/receipt")" = another-receipt ] ||
            die "claimed a competing template after an unsuccessful move"
    else
        expected=0
        [ "$scenario" != power ] || expected=137
        [ "$status" -eq "$expected" ] || die "$scenario seal returned $status"
        published_receipt=$(cat "$WORK/$scenario/trial-template/receipt")
        [ "$published_receipt" = "$WORK/$scenario/receipt" ] && grep -Fxq result=passed "$published_receipt" ||
            die "$scenario left the published template without its own passed receipt"
    fi
done

# Run explicit resume from receipt binding through publication. Remap only
# the evidence root so a non-root test can exercise canonical path checks.
awk '/^resumed_from_identity=$/{copy=1} /^builder_admission=/{exit} copy' "$BUILDER" |
    sed "s|/var/lib/chasm/template-builds|$WORK/recovery|g" > "$WORK/recover.sh"
cat > "$WORK/recover-driver.sh" <<'DRIVER'
set -eu
resume_builder=as-build-test
template_name=trial-template
template_identity=$(printf '%064d' 1)
resume_receipt=$WORK/recovery/$template_identity/20260201T120000Z-123/receipt
PROFILE_NAME=trial-profile
BOOTSTRAP_PROFILE=minimal
SITE_GUEST_UID=1001
TEMPLATE_BASE_IMAGE_FINGERPRINT=base
TEMPLATE_GUEST_ZSHRC_SHA256=zsh
TEMPLATE_PROFILE_MANIFEST_SHA256=manifest
TEMPLATE_PACKAGES_LAYER_SHA256=packages
TEMPLATE_PROFILE_READER_SHA256=reader
mkdir -p "$(dirname "$resume_receipt")"
receipt_identity=$template_identity
if [ "$scenario" = stale ]; then receipt_identity=$(printf '%064d' 2); fi
printf '%s\n' "template_identity=$receipt_identity" base_image_fingerprint=base profile=trial-profile \
    bootstrap_profile=minimal guest_uid=1001 guest_zshrc_sha256=zsh profile_manifest_sha256=manifest \
    packages_layer_sha256=packages profile_reader_sha256=reader builder_name=as-build-test \
    template_name=trial-template snapshot=sealed template_ipv4=dynamic template_status=STOPPED \
    protection_delete=true result=pending result=passed > "$resume_receipt"
stat() { [ "$*" = "-c %U:%G:%a $resume_receipt" ] && echo root:root:600; }
metadata() { echo "user.trial.$1"; }
fail() { echo "$*" >&2; exit 74; }
incus() {
    case "$*" in
        'info as-build-test') echo 'Status: STOPPED' ;;
        'list as-build-test -c nP --format csv')
            case "$scenario" in
                wrong-name) echo as-build-other,trial-profile ;;
                inventory-failure) return 1 ;;
                extra-profile) printf 'as-build-test,"trial-profile\nother-profile"\n' ;;
                *) echo as-build-test,trial-profile ;;
            esac ;;
        'config show as-build-test/sealed')
            case "$scenario" in
                incus7) printf 'devices:\n  eth0:\n    name: eth0\n    network: trialbr0\n    type: nic\nephemeral: false\n' ;;
                *)
                    printf 'devices:\n    eth0:\n        type: nic\n'
                    if [ "$scenario" = snapshot-address ]; then echo '        ipv4.address: example'; fi ;;
            esac ;;
        'config get as-build-test user.trial.template-build-identity') echo "$receipt_identity" ;;
        'config get as-build-test volatile.base_image') echo base ;;
        'config get as-build-test security.protection.delete')
            if [ "$scenario" = unprotected ]; then echo false; else echo true; fi ;;
        'config get as-build-test/sealed user.trial.template-receipt')
            case "$scenario" in
                no-snapshot) return 1 ;;
                foreign-snapshot) echo other-receipt ;;
                *) echo "$resume_receipt" ;;
            esac ;;
        'config get as-build-test user.trial.template-receipt') echo "$resume_receipt" ;;
        'config get trial-template user.trial.template-receipt') echo competing-receipt ;;
        'config get as-build-test'*' user.trial.template-identity') echo "$template_identity" ;;
        'config get as-build-test'*' user.trial.bootstrap-profile') echo minimal ;;
        'config get as-build-test'*' user.trial.profile-manifest-sha256') echo manifest ;;
        'config get as-build-test'*' user.trial.instance-ipv4') ;;
        'config device get as-build-test eth0 ipv4.address')
            if [ "$scenario" = address ]; then echo example; fi ;;
        'move as-build-test trial-template')
            if [ "$scenario" = collision ]; then return 1; fi
            echo publish >> "$WORK/recovery-operations" ;;
        *) echo "unexpected recovery command: $*" >&2; exit 99 ;;
    esac
}
. "$WORK/publish.sh"
. "$WORK/recover.sh"
exit 98
DRIVER
for scenario in recovered incus7 extra-profile wrong-name inventory-failure stale no-snapshot foreign-snapshot unprotected address snapshot-address collision; do
    : > "$WORK/recovery-operations"
    status=0
    scenario=$scenario sh "$WORK/recover-driver.sh" > "$WORK/recover-log" 2>&1 || status=$?
    if [ "$scenario" = recovered ] || [ "$scenario" = incus7 ]; then
        [ "$status" -eq 0 ] && [ "$(cat "$WORK/recovery-operations")" = publish ] ||
            die "sealed recovery failed: $(cat "$WORK/recover-log")"
    else
        [ "$status" -eq 74 ] && [ ! -s "$WORK/recovery-operations" ] ||
            die "$scenario recovery returned $status: $(cat "$WORK/recover-log")"
    fi
done

# Check both Incus MAC sources, the canonical output, and guest readiness on
# an interface whose kernel name is independent of its Incus device name.
sed -n '/^    vm_mac=$(incus config device get/,/^    echo "launch: phase=instance-initialize/{/phase=instance-initialize/!p;}' "$LAUNCHER" > "$WORK/mac-lookup.sh"
cat > "$WORK/mac-driver.sh" <<'DRIVER'
set -eu
VM_NAME=trial
incus() {
    case "$*" in
        'config device get trial eth0 hwaddr') echo "$explicit" ;;
        'config get trial volatile.eth0.hwaddr') echo fallback >> "$WORK/mac-calls"; echo "$volatile" ;;
        *) exit 98 ;;
    esac
}
fail() { exit 65; }
. "$WORK/mac-lookup.sh"
printf '%s\n' "$vm_mac"
DRIVER
mac=aa:bb:cc:dd:ee:ff
out=$(explicit=AA:BB:CC:DD:EE:FF volatile='' sh "$WORK/mac-driver.sh")
[ "$out" = "$mac" ] && [ ! -e "$WORK/mac-calls" ] || die "explicit MAC lookup failed"
out=$(explicit='' volatile=AA:BB:CC:DD:EE:FF sh "$WORK/mac-driver.sh")
[ "$out" = "$mac" ] && grep -Fxq fallback "$WORK/mac-calls" || die "volatile MAC fallback failed"
status=0
explicit='' volatile=invalid sh "$WORK/mac-driver.sh" || status=$?
[ "$status" -eq 65 ] || die "invalid MAC was accepted"

awk '/^systemctl restart systemd-networkd.service$/{copy=1; next}
    copy && /^done$/{print; exit} copy{print}' "$LAUNCHER" > "$WORK/nic-ready.sh"
cat > "$WORK/nic-driver.sh" <<'DRIVER'
set -eu
instance_ipv4=192.0.2.31
instance_mac=aa:bb:cc:dd:ee:ff
ip() {
    [ "$*" = '-4 -o address show' ] || exit 98
    echo '3: ens19 inet 192.0.2.31/24 scope global ens19'
}
cat() {
    [ "$1" = /sys/class/net/ens19/address ] || exit 97
    echo "$actual_mac"
}
sleep() { echo retry >> "$WORK/nic-retries"; }
. "$WORK/nic-ready.sh"
DRIVER
actual_mac=$mac sh "$WORK/nic-driver.sh" || die "matching address on renamed NIC was refused"
if actual_mac=aa:bb:cc:dd:ee:11 sh "$WORK/nic-driver.sh"; then
    die "correct IPv4 on the wrong MAC was accepted"
fi
[ "$(wc -l < "$WORK/nic-retries")" -eq 29 ] || die "unexpected guest NIC retry count"

echo "test-builder-address-claim: address claims, image import, builder startup, template publication, and guest MAC checks passed"
