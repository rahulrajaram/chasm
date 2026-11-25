#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=scripts/lib/require-site-host.sh
. "$SCRIPT_DIR/lib/require-site-host.sh"
# shellcheck source=scripts/lib/sandbox-template-identity.sh
. "$SCRIPT_DIR/lib/sandbox-template-identity.sh"
. "$SCRIPT_DIR/lib/incus-names.sh"
# shellcheck source=scripts/lib/incus-ipv4-claims.sh
. "$SCRIPT_DIR/lib/incus-ipv4-claims.sh"
# shellcheck source=scripts/lib/host-resources.sh
. "$SCRIPT_DIR/lib/host-resources.sh"

# The builder VM is admitted beside RUNNING sandboxes when the host can
# carry it: MemTotal covers the builder's limits.memory plus every running
# sandbox's configured budget plus the site's host_reserve_mib plus this
# fixed margin, and MemAvailable still covers the need at admission time
# (the old rule refused whenever any instance ran).
TEMPLATE_BUILD_MEMORY_MARGIN_MIB=256

fail() {
    echo "template build: $*" >&2
    exit 1
}

# The sandbox lifecycle lock is the same exclusive flock the executor
# (internal/execserver/handlers.go withLifecycleLockAt) and the launcher
# (scripts/launch-sandbox-vm.sh) take on /run/lock/chasm-lifecycle.lock. The
# build holds it from before builder_memory_admission until the builder VM is
# RUNNING: inside that window a concurrent `chasm start` must wait instead of
# being admitted against headroom this build is about to consume. Once the
# builder is RUNNING the executor refuses every start by itself (an
# as-build-* instance is an unmanaged running instance), so the lock is no
# longer needed and is released for the long install/scrub/seal phases.
# The file is created 0600 root-owned (umask 077, like the launcher) because
# the executor refuses a lifecycle lock whose mode differs from 0600; a lock
# that cannot be acquired fails the build closed. The lock is released on
# every exit path: after the exit trap is armed, on_exit releases it
# explicitly, and any earlier exit closes fd 9, which the kernel uses to
# drop the lock. The builder only ever talks to Incus while holding the
# lock, so it cannot deadlock with the launcher or the executor (both only
# ever wait for the lock). The CHASM_* overrides exist only for the
# unprivileged regression harness and are never set on a provisioned host.
LIFECYCLE_LOCK=${CHASM_LIFECYCLE_LOCK:-/run/lock/chasm-lifecycle.lock}
FLOCK_BIN=${CHASM_FLOCK_BIN:-/usr/bin/flock}
builder_lifecycle_lock_fd=
builder_acquire_lifecycle_lock() {
    umask 077
    exec 9>"$LIFECYCLE_LOCK"
    "$FLOCK_BIN" -x 9 || fail "could not acquire the sandbox lifecycle lock at $LIFECYCLE_LOCK"
    builder_lifecycle_lock_fd=9
}
builder_release_lifecycle_lock() {
    [ -n "$builder_lifecycle_lock_fd" ] || return 0
    "$FLOCK_BIN" -u 9 || :
    exec 9>&-
    builder_lifecycle_lock_fd=
}

[ "$#" -le 1 ] || fail "this command accepts at most one argument: --check or the bootstrap profile"
[ "$(id -u)" -eq 0 ] || fail "run through sudo as root"
require_site_host || fail "site identity check failed"

# --check resolves the current identity and runs the fast-path completion
# checks read-only: exit 0 when the current-identity sealed template exists,
# exit 1 (with the reason) when a real build would create or rebuild it. No
# image import, VM, or instance state is touched.
check_only=0
check_bootstrap_profile=
case "${1:-}" in
    --check) check_only=1 ;;
    '') ;;
    *) check_bootstrap_profile=$1 ;;
esac

# The optional positional argument names the bootstrap profile; the site
# descriptor (default minimal) supplies it when absent. The identity helper
# re-validates the name against the profile grammar and fails closed on an
# unknown or unsafe profile.
BOOTSTRAP_PROFILE=${check_bootstrap_profile:-$SITE_BOOTSTRAP_PROFILE}
export BOOTSTRAP_PROFILE

# The site descriptor is the only source for these values.
BASE_IMAGE_ALIAS=$SITE_BASE_IMAGE_ALIAS
PROFILE_NAME=$SITE_PROFILE_NAME
BUILDER_IPV4=${BUILDER_IPV4:-${SITE_SANDBOX_IPV4_PREFIX}11}
NETWORK_NAME=$SITE_BRIDGE_NAME
STORAGE_POOL=$SITE_STORAGE_POOL
resource_prefix=$SITE_RESOURCE_PREFIX
metadata() { incus_metadata_key "$resource_prefix" "$1"; }
# Read by the sourced sandbox-template-identity helper.
# shellcheck disable=SC2034
RESOURCE_PREFIX=$resource_prefix
case "$BUILDER_IPV4" in
    "$SITE_SANDBOX_IPV4_PREFIX"*) ;;
    *) fail "builder IPv4 is outside the site bridge network" ;;
esac

incus admin waitready --timeout=60
incus storage show "$STORAGE_POOL" >/dev/null
incus network show "$NETWORK_NAME" >/dev/null
incus profile show "$PROFILE_NAME" >/dev/null
if ! incus image info "$BASE_IMAGE_ALIAS" >/dev/null 2>&1; then
    if [ "$check_only" -eq 1 ]; then
        fail "base image $BASE_IMAGE_ALIAS is not imported; --check cannot resolve the template identity (a real build would import it)"
    fi
    attempt=1
    until image_info=$(incus image info --vm "$SITE_BASE_IMAGE_REMOTE"); do
        [ "$attempt" -lt 3 ] ||
            fail "could not look up VM image $SITE_BASE_IMAGE_REMOTE after $attempt attempts"
        echo "template build: retrying VM image lookup for $SITE_BASE_IMAGE_REMOTE" >&2
        sleep "$attempt"
        attempt=$((attempt + 1))
    done
    fingerprint=$(printf '%s\n' "$image_info" | awk '/^Fingerprint:/ {print $2}')
    case "$fingerprint" in
        ''|*[!0-9a-f]*) fail "invalid VM image fingerprint from $SITE_BASE_IMAGE_REMOTE" ;;
    esac
    [ "${#fingerprint}" -eq 64 ] || fail "invalid VM image fingerprint from $SITE_BASE_IMAGE_REMOTE"
    base_image_remote_name=${SITE_BASE_IMAGE_REMOTE%%:*}
    echo "template build: importing $base_image_remote_name:$fingerprint"
    incus image copy "$base_image_remote_name:$fingerprint" local: \
        --alias "$BASE_IMAGE_ALIAS" --auto-update=false ||
        fail "could not import VM image $base_image_remote_name:$fingerprint as $BASE_IMAGE_ALIAS"
fi
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

publish_sealed_builder() {
    if ! incus move "$builder_name" "$template_name"; then
        published_receipt=$(incus config get "$template_name" "$(metadata template-receipt)" 2>/dev/null) || published_receipt=
        [ "$published_receipt" = "$receipt" ] ||
            fail "could not confirm publication of sealed builder $builder_name as $template_name (receipt $receipt)"
    fi
}

resume_builder=${SANDBOX_TEMPLATE_RESUME_BUILDER:-}
resume_receipt=${SANDBOX_TEMPLATE_RESUME_RECEIPT:-}
if incus info "$template_name" >/dev/null 2>&1; then
    [ "$(incus info "$template_name" | awk '/^Status:/ {print $2}')" = STOPPED ] ||
        fail "existing template is not stopped"
    [ "$(incus config get "$template_name" "$(metadata template-identity)")" = "$template_identity" ] ||
        fail "existing template identity mismatch"
    [ "$(incus config get "$template_name" "$(metadata bootstrap-profile)")" = "$BOOTSTRAP_PROFILE" ] ||
        fail "existing template bootstrap profile mismatch"
    [ "$(incus config get "$template_name" "$(metadata profile-manifest-sha256)")" = "$TEMPLATE_PROFILE_MANIFEST_SHA256" ] ||
        fail "existing template profile manifest mismatch"
    receipt=$(incus config get "$template_name" "$(metadata template-receipt)")
    [ -f "$receipt" ] && [ ! -L "$receipt" ] || fail "template receipt is missing or unsafe"
    if [ -n "$resume_builder$resume_receipt" ] && [ "$check_only" -eq 0 ]; then
        [ -n "$resume_builder" ] && [ "$receipt" = "$resume_receipt" ] &&
            grep -Fxq "builder_name=$resume_builder" "$receipt" ||
            fail "existing template belongs to another build"
    fi
    grep -Fx "template_identity=$template_identity" "$receipt" >/dev/null ||
        fail "template receipt identity mismatch"
    grep -Fx "template_name=$template_name" "$receipt" >/dev/null ||
        fail "template receipt instance mismatch"
    grep -Fx result=passed "$receipt" >/dev/null || fail "template receipt did not pass"
    [ "$(incus snapshot list "$template_name" --format csv | awk -F, '$1 == "sealed" { print $1; exit }')" = sealed ] ||
        fail "template sealed snapshot is missing"
    [ -z "$(incus config device get "$template_name" eth0 ipv4.address)" ] ||
        fail "template retains a builder IPv4 address"
    echo "template build: current template=$template_name identity=$template_identity"
    exit 0
fi

if [ "$check_only" -eq 1 ]; then
    fail "no current sealed template for profile $BOOTSTRAP_PROFILE at identity $template_identity; a real build would create it"
fi

# Resource admission under the lifecycle lock (see
# builder_acquire_lifecycle_lock above): the bounded build may run while
# sandbox VMs run when the host can carry the builder VM on top of them. The
# check is read-only — no other instance is ever stopped, frozen, or
# reconfigured — and the refusal names every number it used. The capacity
# rule counts the configured budgets of every running sandbox, like the
# executor's start admission; MemAvailable stays a second, instantaneous
# guard. Admission and `incus start` are one critical section with respect
# to sandbox starts, so the admission snapshot cannot go stale before the
# builder exists.
builder_acquire_lifecycle_lock
resumed_from_identity=
if [ -n "$resume_builder" ]; then
    case "$resume_builder" in
        as-build-[a-z0-9-]*)
            case "$resume_builder" in *[!a-z0-9-]*|*-) fail "unsafe resume builder name" ;; esac
            ;;
        *) fail "unsafe resume builder name" ;;
    esac
    case "$resume_receipt" in
        /var/lib/chasm/template-builds/[0-9a-f]*/[0-9A-Z-]*/receipt) ;;
        *) fail "unsafe or missing resume receipt path" ;;
    esac
    incus info "$resume_builder" >/dev/null 2>&1 || fail "resume builder is missing"
    [ "$(incus info "$resume_builder" | awk '/^Status:/ {print $2}')" = STOPPED ] ||
        fail "resume builder is not stopped"
    [ -f "$resume_receipt" ] && [ ! -L "$resume_receipt" ] ||
        fail "resume receipt is missing or unsafe"
    [ "$(stat -c %U:%G:%a "$resume_receipt")" = root:root:600 ] ||
        fail "resume receipt metadata is unsafe"
    resumed_from_identity=$(incus config get "$resume_builder" "$(metadata template-build-identity)")
    case "$resumed_from_identity" in *[!0-9a-f]*|'') fail "resume builder identity is invalid" ;; esac
    [ "${#resumed_from_identity}" -eq 64 ] || fail "resume builder identity length is invalid"
    [ "$(incus config get "$resume_builder" volatile.base_image)" = "$TEMPLATE_BASE_IMAGE_FINGERPRINT" ] ||
        fail "resume builder base image does not match the current definition"
    resume_profiles=$(incus list "$resume_builder" -c nP --format csv) ||
        fail "could not read resume builder profiles"
    [ "$resume_profiles" = "$resume_builder,$PROFILE_NAME" ] ||
        fail "resume builder profile does not match the current definition"
    for requirement in \
        "template_identity=$resumed_from_identity" \
        "base_image_fingerprint=$TEMPLATE_BASE_IMAGE_FINGERPRINT" \
        "profile=$PROFILE_NAME" \
        "bootstrap_profile=$BOOTSTRAP_PROFILE" \
        "guest_uid=$SITE_GUEST_UID" \
        "guest_zshrc_sha256=$TEMPLATE_GUEST_ZSHRC_SHA256" \
        "profile_manifest_sha256=$TEMPLATE_PROFILE_MANIFEST_SHA256" \
        "packages_layer_sha256=$TEMPLATE_PACKAGES_LAYER_SHA256" \
        "profile_reader_sha256=$TEMPLATE_PROFILE_READER_SHA256" \
        "builder_name=$resume_builder"
    do
        grep -Fx "$requirement" "$resume_receipt" >/dev/null ||
            fail "resume receipt does not bind the preserved builder: $requirement"
    done
    builder_name=$resume_builder
    resume_result=$(awk -F= '$1 == "result" {result=$2} END {print result}' "$resume_receipt")
    case "$resume_result" in
        passed)
            # Only a fully sealed builder of the current definition may be
            # published directly. All reads must succeed, including empty
            # values for the neutralized IPv4 address.
            [ "$resumed_from_identity" = "$template_identity" ] ||
                fail "sealed builder identity differs from the current definition"
            printf '%s\n' "$resume_receipt" |
                grep -Eq "^/var/lib/chasm/template-builds/$template_identity/[0-9]{8}T[0-9]{6}Z-[0-9]+/receipt$" ||
                fail "sealed receipt directory does not match the current identity"
            [ "$(realpath -e "$resume_receipt")" = "$resume_receipt" ] ||
                fail "sealed receipt path is not canonical"
            for marker in "template_name=$template_name" snapshot=sealed template_ipv4=dynamic template_status=STOPPED protection_delete=true; do
                grep -Fxq "$marker" "$resume_receipt" || fail "missing sealed receipt marker: $marker"
            done
            for instance in "$builder_name" "$builder_name/sealed"; do
                [ "$(incus config get "$instance" "$(metadata template-receipt)")" = "$resume_receipt" ] &&
                    [ "$(incus config get "$instance" "$(metadata template-identity)")" = "$template_identity" ] &&
                    [ "$(incus config get "$instance" "$(metadata bootstrap-profile)")" = "$BOOTSTRAP_PROFILE" ] &&
                    [ "$(incus config get "$instance" "$(metadata profile-manifest-sha256)")" = "$TEMPLATE_PROFILE_MANIFEST_SHA256" ] ||
                    fail "sealed builder or snapshot ownership does not match: $instance"
                claim=$(incus config get "$instance" "$(metadata instance-ipv4)") &&
                    [ -z "$claim" ] || fail "sealed instance address metadata is unavailable or claimed: $instance"
            done
            address=$(incus config device get "$builder_name" eth0 ipv4.address) &&
                [ -z "$address" ] || fail "sealed builder address is unavailable or claimed"
            snapshot_config=$(incus config show "$builder_name/sealed") ||
                fail "sealed snapshot configuration is unavailable"
            printf '%s\n' "$snapshot_config" | awk '
                /^[^ ]/ {devices = $0 == "devices:"; eth = 0; child = 0; next}
                devices && /^ +[^ ]/ {
                    indent = match($0, /[^ ]/) - 1
                    key = substr($0, indent + 1)
                    if (!child) child = indent
                    if (indent == child) eth = key == "eth0:"
                    else if (eth && indent > child) {
                        if (key == "type: nic") found = 1
                        if (key ~ /^ipv4[.]address:/) claimed = 1
                    }
                }
                END {exit !(found && !claimed)}' ||
                fail "sealed snapshot lacks a neutral eth0 device"
            [ "$(incus config get "$builder_name" security.protection.delete)" = true ] ||
                fail "sealed builder is not protected"
            receipt=$resume_receipt
            publish_sealed_builder
            echo "template build: published sealed builder=$builder_name template=$template_name receipt=$receipt"
            exit 0
            ;;
        failed) ;;
        *) fail "resume receipt has no completed build result" ;;
    esac
else
    [ -z "$resume_receipt" ] || fail "resume receipt requires a resume builder"
    builder_name=as-build-$template_prefix-$$
fi

builder_admission=$(builder_memory_admission \
    "$PROFILE_NAME" "$SITE_HOST_RESERVE_MIB" "$TEMPLATE_BUILD_MEMORY_MARGIN_MIB" "$resource_prefix") ||
    fail "$builder_admission"
echo "template build: $builder_admission"
root_free_percent=$(df -Pk /var/lib/incus | awk 'NR == 2 {gsub(/%/, "", $5); print 100-$5}')
[ "$root_free_percent" -ge 10 ] || fail "host storage headroom is below 10 percent"

# The builder address is fixed (sandbox_ipv4_prefix + 11) and never
# reselected, so an instance created by an older build can still hold it:
# through an eth0 ipv4.address override, the site's instance-ipv4 metadata,
# or a live address while it runs. Refuse here — after the builder name is
# known (a resumed builder legitimately holds the address itself and is
# skipped) and before anything is created, resumed, or written to disk —
# naming the claiming instance and the claim's source.
builder_claim=$(ipv4_claim_refusal "$resource_prefix" "$BUILDER_IPV4" "$builder_name") ||
    fail "$builder_claim"

run_id=$(date -u +%Y%m%dT%H%M%SZ)-$$
evidence=/var/lib/chasm/template-builds/$template_identity/$run_id
receipt=$evidence/receipt
install -d -o root -g root -m 0700 "$evidence"
printf '%s\n' \
    "run_id=$run_id" \
    "template_identity=$template_identity" \
    "base_image_fingerprint=$TEMPLATE_BASE_IMAGE_FINGERPRINT" \
    "profile_sha256=$TEMPLATE_PROFILE_SHA256" \
    "bootstrap_profile=$BOOTSTRAP_PROFILE" \
    "guest_uid=$SITE_GUEST_UID" \
    "guest_zshrc_sha256=$TEMPLATE_GUEST_ZSHRC_SHA256" \
    "profile_manifest_sha256=$TEMPLATE_PROFILE_MANIFEST_SHA256" \
    "packages=$PROFILE_PACKAGES" \
    "packages_layer_sha256=$TEMPLATE_PACKAGES_LAYER_SHA256" \
    "profile_reader_sha256=$TEMPLATE_PROFILE_READER_SHA256" \
    "builder_sha256=$TEMPLATE_BUILDER_SHA256" \
    "identity_helper_sha256=$TEMPLATE_IDENTITY_HELPER_SHA256" \
    "builder_executable_sha256=$TEMPLATE_BUILDER_EXECUTABLE_SHA256" \
    "identity_helper_executable_sha256=$TEMPLATE_IDENTITY_HELPER_EXECUTABLE_SHA256" \
    "template_name=$template_name" \
    "builder_name=$builder_name" \
    "base_image=$BASE_IMAGE_ALIAS" \
    "profile=$PROFILE_NAME" \
    "builder_ipv4=$BUILDER_IPV4" \
    "resumed_from_identity=$resumed_from_identity" \
    "resumed_from_receipt=$resume_receipt" \
    "started_at=$(date --iso-8601=seconds)" \
    "result=pending" >"$receipt"
chmod 0600 "$receipt"

builder_created=0
builder_booted=0
committed=0
on_exit() {
    status=$?
    trap - EXIT HUP INT TERM
    if [ "$status" -ne 0 ] && [ "$committed" -eq 0 ]; then
        if [ "$builder_created" -eq 1 ] && incus info "$builder_name" >/dev/null 2>&1; then
            state=$(incus info "$builder_name" | awk '/^Status:/ {print $2}')
            if [ "$state" = RUNNING ]; then
                if incus stop "$builder_name" --timeout 60; then
                    printf '%s\n' "failure_stop=graceful" >>"$receipt"
                else
                    incus stop "$builder_name" --force || status=1
                    printf '%s\n' "failure_stop=forced" >>"$receipt"
                fi
            fi
            # A new builder that never booted cannot qualify for resume.
            # Its device override otherwise blocks every retry on the
            # reserved builder address. Preserve an explicit resume target.
            if [ -z "$resume_builder" ] && [ "$builder_booted" -eq 0 ] &&
                [ "$(incus info "$builder_name" | awk '/^Status:/ {print $2}')" = STOPPED ] &&
                incus delete "$builder_name"; then
                printf '%s\n' "discarded_unbooted_builder=$builder_name" >>"$receipt"
            else
                printf '%s\n' "preserved_builder=$builder_name" >>"$receipt"
            fi
        fi
        printf '%s\n' "result=failed" "exit_status=$status" \
            "completed_at=$(date --iso-8601=seconds)" >>"$receipt"
        chmod 0600 "$receipt"
        printf 'template build: failed (exit %s); see receipt %s\n' "$status" "$receipt" >&2
    fi
    builder_release_lifecycle_lock
    exit "$status"
}
trap on_exit EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

if [ -n "$resume_builder" ]; then
    echo "template build: phase=resume builder=$builder_name prior_identity=$resumed_from_identity"
    builder_created=1
else
    echo "template build: phase=init builder=$builder_name"
    incus init "$BASE_IMAGE_ALIAS" "$builder_name" --vm --profile "$PROFILE_NAME"
    builder_created=1
    incus config device override "$builder_name" eth0 ipv4.address="$BUILDER_IPV4"
    incus config set "$builder_name" boot.autostart=false
fi
incus config set "$builder_name" "$(metadata template-build-identity)"="$template_identity"
incus start "$builder_name"
[ "$(incus info "$builder_name" | awk '/^Status:/ {print $2}')" = RUNNING ] ||
    fail "builder VM $builder_name did not reach the RUNNING state"
builder_booted=1
# The builder is a running unmanaged instance from the executor's point of
# view, so it now refuses every sandbox start by itself: release the
# lifecycle lock for the long install/scrub/seal phases (also on every
# failure path, via the exit trap).
builder_release_lifecycle_lock

attempt=0
while ! incus exec "$builder_name" -- true >/dev/null 2>&1; do
    attempt=$((attempt + 1))
    [ "$attempt" -lt 90 ] || fail "builder Incus agent readiness timed out; verify the host incus-agent package or binary is installed"
    sleep 2
done
incus exec "$builder_name" -- cloud-init status --wait

if [ -n "$resume_builder" ]; then
    echo "template build: phase=resume-qualification"
    # Expansion is intentionally deferred to the shell inside the guest.
    # shellcheck disable=SC2016
    incus exec "$builder_name" -- /bin/sh -eu -c '
        for resume_tool in git jq zsh curl id getent cut stat; do
            command -v "$resume_tool" >/dev/null 2>&1 || {
                echo "template build: guest missing tool: $resume_tool" >&2
                exit 1
            }
        done
        guest_uid=$1
        test "$(id -u agent 2>/dev/null)" = "$guest_uid"
        test "$(id -g agent 2>/dev/null)" = "$guest_uid"
        test "$(getent passwd agent | cut -d: -f6)" = /home/agent
        test "$(getent passwd agent | cut -d: -f7)" = /usr/bin/zsh
        test -d /home/agent
        test "$(stat -c %U:%G /home/agent)" = agent:agent
        test -f /home/agent/.zshrc
        test "$(stat -c %U:%G /home/agent/.zshrc)" = agent:agent
    ' sh "$SITE_GUEST_UID"
    # The identity the resumed build will seal under digests the CURRENT
    # guest zshrc and pins the CURRENT site guest uid, but the preserved
    # builder contains whatever the interrupted build installed. Run the
    # packages layer's read-only check against the builder: it verifies that
    # the guest's sealed receipt records exactly this bootstrap profile,
    # package list, site guest uid, and guest zshrc digest, and that the
    # account and the bytes, owner, and mode of /home/agent/.zshrc still
    # match. A resumed builder that predates a zshrc or guest_uid change
    # therefore refuses here instead of being sealed under an identity it
    # does not contain.
    echo "template build: phase=resume-guest-layer-check"
    VM_NAME=$builder_name "$SCRIPT_DIR/install-guest-packages.sh" --check "$BOOTSTRAP_PROFILE"
else
    echo "template build: phase=bootstrap"
    VM_NAME=$builder_name "$SCRIPT_DIR/install-guest-packages.sh" --apply "$BOOTSTRAP_PROFILE"
fi

echo "template build: phase=scrub"
incus exec "$builder_name" -- /bin/sh -s <<'GUEST_SCRUB'
set -eu

# Guard every non-builtin tool before first use; a sealed template must never
# carry a silently degraded scrub.
for scrub_tool in stat find grep rm runuser env apt-get cloud-init cat sync; do
    command -v "$scrub_tool" >/dev/null 2>&1 || {
        echo "template build: guest missing tool: $scrub_tool" >&2
        exit 1
    }
done

home=/home/agent
[ -d "$home" ] && [ ! -L "$home" ]
[ "$(stat -c %U:%G "$home")" = agent:agent ]
for sensitive in \
    "$home/.netrc" \
    "$home/.git-credentials"
do
    [ ! -e "$sensitive" ] && [ ! -L "$sensitive" ]
done
if [ -d "$home/workspaces" ]; then
    [ -z "$(find "$home/workspaces" -mindepth 1 -print -quit)" ]
fi
[ ! -d "$home/.ssh" ] || [ -z "$(find "$home/.ssh" -mindepth 1 -print -quit)" ]
rm -f -- "$home/.bash_history" "$home/.zsh_history" /root/.bash_history /root/.zsh_history
apt-get clean </dev/null
find /etc/ssh -maxdepth 1 -type f -name 'ssh_host_*' -delete
rm -f -- /var/lib/systemd/random-seed
cloud-init clean --logs --machine-id --seed --configs all </dev/null
[ "$(cat /etc/machine-id)" = uninitialized ]
sync
GUEST_SCRUB

# Expansion is intentionally deferred to the shell inside the guest.
# shellcheck disable=SC2016
incus exec "$builder_name" -- /bin/sh -eu -c '
    test ! -e /home/agent/.netrc
    test ! -e /home/agent/.git-credentials
    test ! -e /home/agent/.bash_history
    test ! -e /home/agent/.zsh_history
    test "$(cat /etc/machine-id)" = uninitialized
    test -z "$(find /etc/ssh -maxdepth 1 -type f -name "ssh_host_*" -print -quit)"
'

echo "template build: phase=seal"
incus stop "$builder_name" --timeout 60
incus config device unset "$builder_name" eth0 ipv4.address
[ -z "$(incus config device get "$builder_name" eth0 ipv4.address)" ] ||
    fail "could not neutralize template builder IPv4 address"
incus config set "$builder_name" "$(metadata template-identity)"="$template_identity"
incus config set "$builder_name" "$(metadata bootstrap-profile)"="$BOOTSTRAP_PROFILE"
incus config set "$builder_name" "$(metadata profile-manifest-sha256)"="$TEMPLATE_PROFILE_MANIFEST_SHA256"
incus config set "$builder_name" "$(metadata template-receipt)"="$receipt"
# Publish only after the stopped builder, its sealed snapshot, and its
# receipt are ready. Readers can then qualify the destination even if the
# mover loses its reply or the host restarts immediately after the rename.
if [ -n "$resume_builder" ]; then
    incus snapshot create "$builder_name" sealed --reuse
else
    incus snapshot create "$builder_name" sealed
fi
incus config set "$builder_name" security.protection.delete=true
[ "$(incus info "$builder_name" | awk '/^Status:/ {print $2}')" = STOPPED ]

printf '%s\n' \
    "snapshot=sealed" \
    "template_ipv4=dynamic" \
    "template_status=STOPPED" \
    "protection_delete=true" \
    "completed_at=$(date --iso-8601=seconds)" \
    "result=passed" >>"$receipt"
chmod 0600 "$receipt"
sync -f "$receipt"
# A passed receipt certifies the sealed instance. After this point a failed
# client request cannot prove that the server failed to publish it.
committed=1
publish_sealed_builder
trap - EXIT HUP INT TERM
echo "template build: passed template=$template_name receipt=$receipt"
