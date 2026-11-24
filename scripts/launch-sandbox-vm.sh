#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=scripts/lib/require-site-host.sh
. "$SCRIPT_DIR/lib/require-site-host.sh"
# shellcheck source=scripts/lib/sandbox-template-identity.sh
. "$SCRIPT_DIR/lib/sandbox-template-identity.sh"
# shellcheck source=scripts/lib/incus-names.sh
. "$SCRIPT_DIR/lib/incus-names.sh"
# shellcheck source=scripts/lib/incus-ipv4-claims.sh
. "$SCRIPT_DIR/lib/incus-ipv4-claims.sh"

SHARED_DEVICE_NAME=shared
WORKSPACE_DEVICE_NAME=workspace
LEAVE_STOPPED=${LEAVE_STOPPED:-0}
LIFECYCLE_LOCK=/run/lock/chasm-lifecycle.lock

fail() {
    echo "launch: $*" >&2
    exit 1
}

# Only the executor supplies the private timing descriptor. Uptime is
# monotonic and is sampled here, at the actual launcher phase boundary.
timing_mark() {
    [ "${CHASM_LIFECYCLE_TIMING:-}" = 1 ] || return 0
    IFS=' ' read -r timing_uptime _ </proc/uptime || return 0
    printf '%s %s %s\n' "$1" "$2" "$timing_uptime" >&3 || :
}

# The claim inventory itself is shared with the template builder
# (lib/incus-ipv4-claims.sh): an address another instance already holds —
# through an eth0 ipv4.address override, the site's instance-ipv4 metadata,
# or a live address of a running instance — is refused with the claiming
# instance named.
ensure_unique_ipv4_claim() {
    candidate_ipv4=$1
    claim_refusal=$(ipv4_claim_refusal "$resource_prefix" "$candidate_ipv4" "$VM_NAME") ||
        fail "sandbox $claim_refusal"
}

ensure_custom_filesystem_volume() {
    volume_name=$1
    volume_scope=$2
    volume_owner=$3

    if ! incus storage volume show "$STORAGE_POOL" "$volume_name" >/dev/null 2>&1; then
        incus storage volume create "$STORAGE_POOL" "$volume_name" \
            "$(metadata protected)=true" \
            "$(metadata scope)=$volume_scope" \
            "$(metadata owner)=$volume_owner"
    fi

    volume_info=$(incus storage volume show "$STORAGE_POOL" "$volume_name")
    volume_type=$(printf '%s\n' "$volume_info" | awk '$1 == "type:" { print $2; exit }')
    content_type=$(printf '%s\n' "$volume_info" | awk '$1 == "content_type:" { print $2; exit }')
    [ "$volume_type" = custom ] || fail "storage volume $volume_name is not custom"
    [ "$content_type" = filesystem ] || fail "storage volume $volume_name is not a filesystem"
    [ "$(incus storage volume get "$STORAGE_POOL" "$volume_name" "$(metadata protected)")" = true ] ||
        fail "storage volume $volume_name protection metadata mismatch"
    [ "$(incus storage volume get "$STORAGE_POOL" "$volume_name" "$(metadata scope)")" = "$volume_scope" ] ||
        fail "storage volume $volume_name scope mismatch"
    [ "$(incus storage volume get "$STORAGE_POOL" "$volume_name" "$(metadata owner)")" = "$volume_owner" ] ||
        fail "storage volume $volume_name owner mismatch"
}

validate_custom_filesystem_volume() {
    volume_name=$1
    volume_scope=$2
    volume_owner=$3
    volume_info=$(incus storage volume show "$STORAGE_POOL" "$volume_name")
    volume_type=$(printf '%s\n' "$volume_info" | awk '$1 == "type:" { print $2; exit }')
    content_type=$(printf '%s\n' "$volume_info" | awk '$1 == "content_type:" { print $2; exit }')
    [ "$volume_type" = custom ] || fail "storage volume $volume_name is not custom"
    [ "$content_type" = filesystem ] || fail "storage volume $volume_name is not a filesystem"
    [ "$(incus storage volume get "$STORAGE_POOL" "$volume_name" "$(metadata protected)")" = true ] ||
        fail "storage volume $volume_name protection metadata mismatch"
    [ "$(incus storage volume get "$STORAGE_POOL" "$volume_name" "$(metadata scope)")" = "$volume_scope" ] ||
        fail "storage volume $volume_name scope mismatch"
    [ "$(incus storage volume get "$STORAGE_POOL" "$volume_name" "$(metadata owner)")" = "$volume_owner" ] ||
        fail "storage volume $volume_name owner mismatch"
}

ensure_volume_device() {
    device_name=$1
    volume_name=$2
    mount_path=$3

    device_type=$(incus config device get "$VM_NAME" "$device_name" type 2>/dev/null || true)
    if [ -z "$device_type" ]; then
        incus config device add "$VM_NAME" "$device_name" disk \
            pool="$STORAGE_POOL" source="$volume_name" path="$mount_path" readonly=false
    fi

    [ "$(incus config device get "$VM_NAME" "$device_name" type)" = disk ] ||
        fail "device $device_name is not a disk"
    [ "$(incus config device get "$VM_NAME" "$device_name" pool)" = "$STORAGE_POOL" ] ||
        fail "device $device_name storage pool mismatch"
    [ "$(incus config device get "$VM_NAME" "$device_name" source)" = "$volume_name" ] ||
        fail "device $device_name source mismatch"
    [ "$(incus config device get "$VM_NAME" "$device_name" path)" = "$mount_path" ] ||
        fail "device $device_name mount path mismatch"
    [ "$(incus config device get "$VM_NAME" "$device_name" readonly)" = false ] ||
        fail "device $device_name is not explicitly read-write"
}

ensure_instance_metadata() {
    metadata_key=$1
    expected_value=$2
    actual_value=$(incus config get "$VM_NAME" "$metadata_key")
    if [ -z "$actual_value" ]; then
        incus config set "$VM_NAME" "$metadata_key" "$expected_value"
        actual_value=$(incus config get "$VM_NAME" "$metadata_key")
    fi
    [ "$actual_value" = "$expected_value" ] || fail "instance metadata $metadata_key mismatch"
}

validate_metadata_if_present() {
    metadata_key=$1
    expected_value=$2
    actual_value=$(incus config get "$VM_NAME" "$metadata_key")
    [ -z "$actual_value" ] || [ "$actual_value" = "$expected_value" ] ||
        fail "instance metadata $metadata_key mismatch"
}

[ "$#" -eq 0 ] || fail "this command accepts no arguments"
[ "$(id -u)" -eq 0 ] || fail "run through sudo as root"
require_site_host || fail "site identity check failed"

# Every Incus object name and user.* metadata key below derives from the
# site's resource_prefix; host-side paths this script touches are the
# product's own and stay fixed.
resource_prefix=$SITE_RESOURCE_PREFIX
metadata() { incus_metadata_key "$resource_prefix" "$1"; }
SHARED_VOLUME_NAME=$(incus_shared_volume_name "$resource_prefix")

# The site descriptor is the only source for these values.
# VM_NAME is the Incus instance name; the site's default_sandbox is a logical
# name, so the derived instance is sandbox-<default>. A site that declares no
# default requires an explicit VM_NAME.
if [ -z "${VM_NAME:-}" ]; then
    [ -n "$SITE_DEFAULT_SANDBOX" ] ||
        fail "the site descriptor declares no default_sandbox; pass VM_NAME explicitly"
    VM_NAME=sandbox-$SITE_DEFAULT_SANDBOX
fi
VM_IPV4=${VM_IPV4:-${SITE_SANDBOX_IPV4_PREFIX}10}
BASE_IMAGE_ALIAS=$SITE_BASE_IMAGE_ALIAS
PROFILE_NAME=$SITE_PROFILE_NAME
BOOTSTRAP_PROFILE=$SITE_BOOTSTRAP_PROFILE
# Read by the sourced sandbox-template-identity helper.
# shellcheck disable=SC2034
RESOURCE_PREFIX=$resource_prefix
STORAGE_POOL=$SITE_STORAGE_POOL
NETWORK_NAME=$SITE_BRIDGE_NAME
GUEST_UID=$SITE_GUEST_UID
BRIDGE_ADDRESS=${SITE_BRIDGE_IPV4%/*}
case "$VM_NAME" in
    [a-z0-9]*)
        case "$VM_NAME" in *[!a-z0-9-]*|*-) fail "unsafe VM name" ;; esac
        ;;
    *) fail "unsafe VM name" ;;
esac
case "$VM_IPV4" in
    "$SITE_SANDBOX_IPV4_PREFIX"*) ;;
    *) fail "VM IPv4 is outside the site bridge network" ;;
esac
case "$LEAVE_STOPPED" in 0|1) ;; *) fail "LEAVE_STOPPED must be 0 or 1" ;; esac
if [ -n "${BUDGET_MEMORY_MIB:-}" ]; then
    case "$BUDGET_MEMORY_MIB" in *[!0-9]*|'') fail "BUDGET_MEMORY_MIB must be numeric" ;; esac
    # Floor plus arithmetic-safety bound only: the real per-sandbox cap is
    # this host's memory minus the site's host_reserve_mib (idiom 4),
    # derived below from /proc/meminfo.
    [ "$BUDGET_MEMORY_MIB" -ge 512 ] && [ "$BUDGET_MEMORY_MIB" -le 1048576 ] ||
        fail "BUDGET_MEMORY_MIB is outside the admitted range (floor 512, safety bound 1048576 MiB; the real cap is host memory minus host_reserve_mib)"
    host_total_kib=$(awk '/^MemTotal:/ {print $2; exit}' /proc/meminfo)
    case "$host_total_kib" in ''|*[!0-9]*) fail "host MemTotal is unreadable; the memory cap cannot be derived" ;; esac
    host_total_mib=$((host_total_kib / 1024))
    host_cap_mib=$((host_total_mib - SITE_HOST_RESERVE_MIB))
    [ "$host_cap_mib" -gt 0 ] ||
        fail "host_reserve_mib=$SITE_HOST_RESERVE_MIB covers all of MemTotal ${host_total_mib}MiB; no sandbox budget fits this host"
    [ "$BUDGET_MEMORY_MIB" -le "$host_cap_mib" ] ||
        fail "budget $BUDGET_MEMORY_MIB MiB exceeds this host's cap ${host_cap_mib} MiB (MemTotal ${host_total_mib} MiB - host_reserve_mib $SITE_HOST_RESERVE_MIB)"
fi
umask 077
exec 9>"$LIFECYCLE_LOCK"
/usr/bin/flock -x 9
incus admin waitready --timeout=60
if incus info "$VM_NAME" >/dev/null 2>&1; then
    # Existing instances own their address. A direct launcher caller may not
    # pass VM_IPV4; using the new-instance fallback here would rewrite guest
    # networkd to .10 while Incus still assigns the instance another IP.
    existing_ipv4=$(incus config device get "$VM_NAME" eth0 ipv4.address)
    case "$existing_ipv4" in
        "$SITE_SANDBOX_IPV4_PREFIX"*) VM_IPV4=$existing_ipv4 ;;
        *) fail "existing sandbox IPv4 claim is missing or outside the site bridge" ;;
    esac
fi
incus storage show "$STORAGE_POOL" >/dev/null
incus network show "$NETWORK_NAME" >/dev/null
incus profile show "$PROFILE_NAME" >/dev/null
incus image info "$BASE_IMAGE_ALIAS" >/dev/null
resolve_sandbox_template_identity || fail "could not resolve template identity"
template_identity=$TEMPLATE_IDENTITY
template_prefix=$(printf '%s' "$template_identity" | cut -c1-24)
template_name=${SANDBOX_TEMPLATE_NAME:-$(incus_template_instance_name "$resource_prefix" "$BOOTSTRAP_PROFILE" "$template_prefix")}
case "$template_name" in
    [a-z0-9]*)
        case "$template_name" in *[!a-z0-9-]*|*-) fail "unsafe template name" ;; esac
        ;;
    *) fail "unsafe template name" ;;
esac

# The final admission check runs below, under this lifecycle lock, after the
# target's requested budget and managed storage metadata are visible. The
# local read-only helper avoids a nested request into the serial executor.

run_id=$(date -u +%Y%m%dT%H%M%SZ)-$$
evidence=/var/lib/chasm/launches/$run_id
receipt=$evidence/receipt
install -d -o root -g root -m 0700 "$evidence"
printf '%s\n' \
    "run_id=$run_id" \
    "vm_name=$VM_NAME" \
    "vm_ipv4=$VM_IPV4" \
    "template_identity=$template_identity" \
    "template_name=$template_name" \
    "bootstrap_profile=$BOOTSTRAP_PROFILE" \
    "started_at=$(date --iso-8601=seconds)" \
    "result=pending" >"$receipt"
chmod 0600 "$receipt"

completed=0
created=0
target_was_present=1
on_exit() {
    status=$?
    trap - EXIT HUP INT TERM
    if [ "$status" -ne 0 ] && [ "$completed" -eq 0 ]; then
        if [ "$created" -eq 1 ] && incus info "$VM_NAME" >/dev/null 2>&1 &&
            [ "$(incus info "$VM_NAME" | awk '/^Status:/ {print $2}')" = RUNNING ]; then
            if incus stop "$VM_NAME" --timeout 60; then
                printf '%s\n' "failure_stop=graceful" >>"$receipt"
            else
                incus stop "$VM_NAME" --force || status=1
                printf '%s\n' "failure_stop=forced" >>"$receipt"
            fi
        fi
        printf '%s\n' "created=$created" "result=failed" "exit_status=$status" \
            "completed_at=$(date --iso-8601=seconds)" >>"$receipt"
        chmod 0600 "$receipt"
    fi
    exit "$status"
}
trap on_exit EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

if ! incus info "$VM_NAME" >/dev/null 2>&1; then
    target_was_present=0
    # The executor selected this address before acquiring our lock. Reject
    # a stale selection before copying a VM, leaving retries able to choose
    # the next free address.
    ensure_unique_ipv4_claim "$VM_IPV4"
    incus info "$template_name" >/dev/null 2>&1 ||
        fail "sealed template is missing; run build-sandbox-template.sh first"
    [ "$(incus info "$template_name" | awk '/^Status:/ {print $2}')" = STOPPED ] ||
        fail "sealed template is not stopped"
    [ "$(incus config get "$template_name" "$(metadata template-identity)")" = "$template_identity" ] ||
        fail "sealed template identity mismatch"
    [ "$(incus config get "$template_name" "$(metadata bootstrap-profile)")" = "$BOOTSTRAP_PROFILE" ] ||
        fail "sealed template bootstrap profile mismatch"
    [ "$(incus snapshot list "$template_name" --format csv | awk -F, '$1 == "sealed" { print $1; exit }')" = sealed ] ||
        fail "sealed template snapshot is missing"
    template_receipt=$(incus config get "$template_name" "$(metadata template-receipt)")
    [ -f "$template_receipt" ] && [ ! -L "$template_receipt" ] ||
        fail "sealed template receipt is missing or unsafe"
    grep -Fx result=passed "$template_receipt" >/dev/null ||
        fail "sealed template receipt did not pass"
    [ -z "$(incus config device get "$template_name" eth0 ipv4.address)" ] ||
        fail "sealed template retains a builder IPv4 address"

    echo "launch: phase=copy template=$template_name vm=$VM_NAME"
    timing_mark clone begin
    incus copy "$template_name" "$VM_NAME" --instance-only
    timing_mark clone end
    created=1
    incus config set "$VM_NAME" security.protection.delete=false
    incus config set "$VM_NAME" boot.autostart=false
    incus config set "$VM_NAME" "$(metadata cloned-from)=$template_name"
    incus config set "$VM_NAME" "$(metadata clone-template-identity)=$template_identity"
    incus config device set "$VM_NAME" eth0 ipv4.address "$VM_IPV4"
fi

clone_template=$(incus config get "$VM_NAME" "$(metadata cloned-from)")
clone_identity=$(incus config get "$VM_NAME" "$(metadata clone-template-identity)")
clone_instance=0
if [ -n "$clone_template" ] || [ -n "$clone_identity" ]; then
    [ "$clone_template" = "$template_name" ] || fail "clone provenance template mismatch"
    [ "$clone_identity" = "$template_identity" ] || fail "clone provenance identity mismatch"
    clone_instance=1
fi

case "$VM_NAME" in
    sandbox-*) logical_name=${VM_NAME#sandbox-} ;;
    *) logical_name=$VM_NAME ;;
esac
workspace_volume_name=$(incus_workspace_volume_name "$resource_prefix" "$VM_NAME")
printf '%s\n' \
    "logical_name=$logical_name" \
    "shared_volume=$SHARED_VOLUME_NAME" \
    "workspace_volume=$workspace_volume_name" >>"$receipt"
shared_volume_present=0
workspace_volume_present=0
shared_device_present=0
workspace_device_present=0
if incus storage volume show "$STORAGE_POOL" "$SHARED_VOLUME_NAME" >/dev/null 2>&1; then
    shared_volume_present=1
    validate_custom_filesystem_volume "$SHARED_VOLUME_NAME" global global
fi
if incus storage volume show "$STORAGE_POOL" "$workspace_volume_name" >/dev/null 2>&1; then
    workspace_volume_present=1
    validate_custom_filesystem_volume "$workspace_volume_name" workspace "$VM_NAME"
fi
if [ -n "$(incus config device get "$VM_NAME" "$SHARED_DEVICE_NAME" type 2>/dev/null || true)" ]; then
    shared_device_present=1
    ensure_volume_device "$SHARED_DEVICE_NAME" "$SHARED_VOLUME_NAME" /shared
fi
if [ -n "$(incus config device get "$VM_NAME" "$WORKSPACE_DEVICE_NAME" type 2>/dev/null || true)" ]; then
    workspace_device_present=1
    ensure_volume_device "$WORKSPACE_DEVICE_NAME" "$workspace_volume_name" /workspace
fi
validate_metadata_if_present "$(metadata managed)" true
validate_metadata_if_present "$(metadata logical-name)" "$logical_name"
validate_metadata_if_present "$(metadata workspace-volume)" "$workspace_volume_name"
validate_metadata_if_present "$(metadata shared-volume)" "$SHARED_VOLUME_NAME"

status=$(incus info "$VM_NAME" | awk '/^Status:/ {print $2}')
if [ "$target_was_present" -eq 1 ]; then
    instance_type=$(incus list "$VM_NAME" -c n,t --format csv | awk -F, -v target="$VM_NAME" '$1 == target { print $2; exit }')
    [ "$instance_type" = VIRTUAL-MACHINE ] || fail "existing sandbox is not a virtual machine"
    # Every instance is now a managed sandbox; the exception for a
    # pre-existing unmanaged legacy instance was deleted with that concept.
    [ "$clone_instance" -eq 1 ] || fail "refusing to adopt a provenance-free managed instance"
    [ "$shared_volume_present" -eq 1 ] && [ "$workspace_volume_present" -eq 1 ] &&
        [ "$shared_device_present" -eq 1 ] && [ "$workspace_device_present" -eq 1 ] ||
        fail "existing managed instance storage contract is incomplete"
    [ "$(incus config get "$VM_NAME" "$(metadata managed)")" = true ] ||
        fail "existing managed instance is not marked managed"
    [ "$(incus config get "$VM_NAME" "$(metadata logical-name)")" = "$logical_name" ] &&
        [ "$(incus config get "$VM_NAME" "$(metadata workspace-volume)")" = "$workspace_volume_name" ] &&
        [ "$(incus config get "$VM_NAME" "$(metadata shared-volume)")" = "$SHARED_VOLUME_NAME" ] ||
        fail "existing managed instance metadata contract is incomplete"
fi

ensure_custom_filesystem_volume "$SHARED_VOLUME_NAME" global global
ensure_custom_filesystem_volume "$workspace_volume_name" workspace "$VM_NAME"
ensure_volume_device "$SHARED_DEVICE_NAME" "$SHARED_VOLUME_NAME" /shared
ensure_volume_device "$WORKSPACE_DEVICE_NAME" "$workspace_volume_name" /workspace
ensure_instance_metadata "$(metadata managed)" true
ensure_instance_metadata "$(metadata logical-name)" "$logical_name"
ensure_instance_metadata "$(metadata workspace-volume)" "$workspace_volume_name"
ensure_instance_metadata "$(metadata shared-volume)" "$SHARED_VOLUME_NAME"

incus config set "$VM_NAME" boot.autostart=false
if [ "$target_was_present" -eq 0 ]; then
    launch_budget=${BUDGET_MEMORY_MIB:-3072}
    incus config set "$VM_NAME" "$(metadata budget-memory-mib)" "$launch_budget"
    incus config set "$VM_NAME" limits.memory "${launch_budget}MiB"
    [ "$(incus config get "$VM_NAME" "$(metadata budget-memory-mib)")" = "$launch_budget" ] &&
        [ "$(incus config get "$VM_NAME" limits.memory)" = "${launch_budget}MiB" ] ||
        fail "sandbox budget and memory limit could not be proven before boot"
elif [ -n "${BUDGET_MEMORY_MIB:-}" ]; then
    fail "BUDGET_MEMORY_MIB may only be set when creating a new sandbox"
fi
# Check the instance's actual claim before boot, including existing instances.
target_ipv4=$(incus config device get "$VM_NAME" eth0 ipv4.address)
ensure_unique_ipv4_claim "$target_ipv4"
launch_admission=$(/usr/local/bin/chasmctl --local-admit-launch "$VM_NAME" 2>&1) ||
    fail "launch admission refused before boot: $launch_admission"
status=$(incus info "$VM_NAME" | awk '/^Status:/ {print $2}')

if [ "$status" != RUNNING ]; then
    timing_mark boot begin
    incus start "$VM_NAME"
    timing_mark boot end
fi

timing_mark guest_agent_ready begin
attempt=0
while ! incus exec "$VM_NAME" -- true >/dev/null 2>&1; do
    attempt=$((attempt + 1))
    [ "$attempt" -lt 90 ] || fail "VM agent did not become ready within 180 seconds"
    sleep 2
done
timing_mark guest_agent_ready end
systemd_attempt=0
while :; do
    systemd_state=$(
        incus exec "$VM_NAME" -- systemctl show --property=SystemState --value 2>/dev/null || true
    )
    case "$systemd_state" in
        running|degraded) break ;;
    esac
    systemd_attempt=$((systemd_attempt + 1))
    [ "$systemd_attempt" -lt 90 ] || fail "systemd did not become ready within 180 seconds"
    sleep 2
done
if [ "$clone_instance" -eq 1 ]; then
    # Anchor guest networking to Incus's eth0 MAC. Linux interface names
    # depend on the VM's PCI layout and the guest's naming policy.
    vm_mac=$(incus config device get "$VM_NAME" eth0 hwaddr)
    if [ -z "$vm_mac" ]; then
        vm_mac=$(incus config get "$VM_NAME" volatile.eth0.hwaddr)
    fi
    vm_mac=$(printf '%s' "$vm_mac" | tr 'A-F' 'a-f')
    printf '%s\n' "$vm_mac" | grep -Eq '^([0-9a-f]{2}:){5}[0-9a-f]{2}$' ||
        fail "Incus did not report a usable eth0 MAC address for $VM_NAME"
    echo "launch: phase=instance-initialize vm=$VM_NAME ipv4=$VM_IPV4"
    incus exec "$VM_NAME" -- /bin/sh -s -- \
        "$VM_NAME" "$VM_IPV4" "$SITE_SANDBOX_IPV4_PREFIX" "$BRIDGE_ADDRESS" "$vm_mac" <<'GUEST_INITIALIZE'
set -eu

# Guard every non-builtin tool before first use; a managed instance must never
# be accepted half-initialized because a tool silently vanished.
for init_tool in hostname hostnamectl grep sed cat install rm ssh-keygen systemctl ip sleep awk; do
    command -v "$init_tool" >/dev/null 2>&1 || {
        echo "launch: guest missing tool: $init_tool" >&2
        exit 1
    }
done

instance_name=$1
instance_ipv4=$2
instance_prefix=$3
instance_gateway=$4
instance_mac=$5
case "$instance_name" in
    [a-z0-9]*) case "$instance_name" in *[!a-z0-9-]*|*-) exit 64 ;; esac ;;
    *) exit 64 ;;
esac
case "$instance_ipv4" in "$instance_prefix"*) ;; *) exit 64 ;; esac
[ "$(cat /etc/machine-id)" != uninitialized ]
[ -n "$(cat /etc/machine-id)" ]

old_hostname=$(hostname)
hostnamectl set-hostname "$instance_name"
if grep -q '^127[.]0[.]1[.]1[[:space:]]' /etc/hosts; then
    sed -i "s/^127[.]0[.]1[.]1[[:space:]].*/127.0.1.1 $instance_name/" /etc/hosts
else
    printf '127.0.1.1 %s\n' "$instance_name" >>/etc/hosts
fi

network_pending=$(mktemp /etc/systemd/network/.10-chasm.network.XXXXXXXX)
trap 'rm -f -- "$network_pending"' EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM
printf '%s\n' \
    '[Match]' \
    "MACAddress=$instance_mac" \
    '' \
    '[Network]' \
    "Address=$instance_ipv4/24" \
    "Gateway=$instance_gateway" \
    "DNS=$instance_gateway" >"$network_pending"
chown root:root "$network_pending"
chmod 0644 "$network_pending"
mv -fT -- "$network_pending" /etc/systemd/network/10-chasm.network
trap - EXIT HUP INT TERM

ssh-keygen -A
systemctl restart systemd-networkd.service
attempt=0
while :; do
    interface=$(ip -4 -o address show | awk -v address="$instance_ipv4/24" '$4 == address {print $2; exit}')
    if [ -n "$interface" ] && [ "$(cat "/sys/class/net/$interface/address")" = "$instance_mac" ]; then
        break
    fi
    attempt=$((attempt + 1))
    [ "$attempt" -lt 30 ]
    sleep 1
done
[ "$(hostname)" = "$instance_name" ]
[ "$old_hostname" != "$instance_name" ] || true
GUEST_INITIALIZE
    incus config set "$VM_NAME" "$(metadata instance-initializer)=controller-v1"
    incus config set "$VM_NAME" "$(metadata instance-ipv4)=$VM_IPV4"
else
    incus exec "$VM_NAME" -- cloud-init status --wait
fi

incus exec "$VM_NAME" -- install -d -o "$GUEST_UID" -g "$GUEST_UID" -m 0770 /workspace
# The shared collaboration volume is root-owned and only
# group-accessible by the agents' shared gid (the site guest_uid): agents can
# collaborate, world (and an agent re-chmodding the root) cannot. The
# private /workspace stays agent-owned.
incus exec "$VM_NAME" -- install -d -o 0 -g "$GUEST_UID" -m 0770 /shared

# Prove the sandbox mounts before the instance can be marked initialized:
# both paths must be real mountpoints (a share Incus silently failed to
# attach would otherwise land on the guest root filesystem), the
# ownership and mode must match what the launch just created, and the
# guest uid must actually be able to write through both shares. Any
# failure here aborts the launch before a passed receipt or the
# initialized marker can be written.
# Expansion is intentionally deferred to the shell inside the guest.
# shellcheck disable=SC2016
incus exec "$VM_NAME" -- /bin/sh -eu -c '
    guest_uid=$1
    mountpoint -q /shared
    mountpoint -q /workspace
    test "$(stat -c "%u:%g:%a" /shared)" = "0:$guest_uid:770"
    test "$(stat -c "%u:%g:%a" /workspace)" = "$guest_uid:$guest_uid:770"
' sh "$GUEST_UID"
for mount_path in /shared /workspace; do
    probe_name=.chasm-write-probe-$$
    incus exec "$VM_NAME" --user "$GUEST_UID" --group "$GUEST_UID" -- /bin/sh -eu -c '
        probe_path=$1/$2
        umask 077
        : >"$probe_path"
        test -f "$probe_path"
        rm -f -- "$probe_path"
    ' sh "$mount_path" "$probe_name"
done

if [ "$clone_instance" -eq 1 ]; then
    # Expansion is intentionally deferred to the shell inside the guest.
    # shellcheck disable=SC2016
    incus exec "$VM_NAME" -- /bin/sh -eu -c '
        test -s /etc/machine-id
        test "$(cat /etc/machine-id)" != uninitialized
        test ! -e /home/agent/.netrc
        test ! -e /home/agent/.git-credentials
        test "$(hostname)" = "$1"
        test -f /etc/systemd/network/10-chasm.network
        grep -Fx "Address=$2/24" /etc/systemd/network/10-chasm.network >/dev/null
        grep -Fx "MACAddress=$3" /etc/systemd/network/10-chasm.network >/dev/null
        interface=$(ip -4 -o address show | awk -v address="$2/24" '\''$4 == address {print $2; exit}'\'')
        test -n "$interface"
        test "$(cat "/sys/class/net/$interface/address")" = "$3"
        test -n "$(find /etc/ssh -maxdepth 1 -type f -name "ssh_host_*_key" -print -quit)"
    ' sh "$VM_NAME" "$VM_IPV4" "$vm_mac"
fi

machine_id=$(incus exec "$VM_NAME" -- cat /etc/machine-id)
final_status=RUNNING
if [ "$LEAVE_STOPPED" -eq 1 ]; then
    timing_mark shutdown_request begin
    incus stop "$VM_NAME" --timeout 60
    timing_mark shutdown_request end
    timing_mark stopped_confirmation begin
    [ "$(incus info "$VM_NAME" | awk '/^Status:/ {print $2}')" = STOPPED ] ||
        fail "sandbox did not reach the requested stopped state"
    timing_mark stopped_confirmation end
    final_status=STOPPED
fi
# The initialization marker is written only after every provisioning and
# verification step has passed, including the optional final stop, and
# immediately before the passed receipt line: a create that fails at its own
# terminal postcondition must not leave the sandbox adoptable. Create and
# ensure flows refuse to adopt or start a managed sandbox without it.
incus config set "$VM_NAME" "$(metadata initialized)" true
printf '%s\n' \
    "created=$created" \
    "clone_instance=$clone_instance" \
    "machine_id=$machine_id" \
    "instance_status=$final_status" \
    "completed_at=$(date --iso-8601=seconds)" \
    "result=passed" >>"$receipt"
chmod 0600 "$receipt"
completed=1
trap - EXIT HUP INT TERM
incus info "$VM_NAME"
echo "launch: passed vm=$VM_NAME created=$created receipt=$receipt"
