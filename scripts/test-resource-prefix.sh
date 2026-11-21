#!/bin/sh
umask 022
set -eu

# Repo-local, unprivileged harness for site-configurable Incus naming
# (the resource_prefix site key).
#
# Covered, without root, Incus, or any host path:
#   1. the shared naming helpers (scripts/lib/incus-names.sh) derive every
#      Incus object name from the prefix: metadata keys, the shared volume,
#      workspace volumes, template instance names, and the drift grep
#      pattern — for the default prefix (chasm) and a custom one
#      (agent-sandbox);
#   2. the site descriptor readers expose resource_prefix: absent/empty
#      resolves to chasm, a custom value round-trips, and site_value
#      reports it;
#   3. the template identity composition separates prefixes: the same
#      inputs under two resource prefixes produce different template
#      identities (so two prefixes can never share one template instance),
#      and the same inputs under the same prefix are deterministic;
#   4. the launcher and the builder carry no hardcoded Incus object names:
#      every name they form comes from the shared helpers (grep gate), and
#      the installer's template probes use the derived pattern.
#
# The Incus calls of the identity helper run through a PATH shim, so no
# Incus daemon is involved.
#
# Usage: scripts/test-resource-prefix.sh   (repository root; no arguments)

[ "$#" -eq 0 ] || { echo "usage: $0" >&2; exit 2; }

ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
NAMES_LIB=$ROOT/scripts/lib/incus-names.sh
SITE_LIB=$ROOT/scripts/lib/require-site-host.sh
IDENTITY_LIB=$ROOT/scripts/lib/sandbox-template-identity.sh

die() {
    echo "test-resource-prefix: FAIL: $1" >&2
    exit 1
}

# shellcheck source=scripts/lib/incus-names.sh
. "$NAMES_LIB"

WORK=$(mktemp -d)
trap 'rm -rf -- "$WORK"' EXIT HUP INT TERM
mkdir -p "$WORK/bin"

assert_eq() { # got want label
    [ "$1" = "$2" ] || die "$3: got '$1', want '$2'"
}

# --- 1. name derivation ------------------------------------------------------

assert_eq "$(incus_metadata_key chasm managed)" "user.chasm.managed" "default metadata key"
assert_eq "$(incus_metadata_key agent-sandbox managed)" "user.agent-sandbox.managed" "custom metadata key"
assert_eq "$(incus_shared_volume_name chasm)" "chasm-shared" "default shared volume"
assert_eq "$(incus_shared_volume_name agent-sandbox)" "agent-sandbox-shared" "custom shared volume"
assert_eq "$(incus_workspace_volume_name chasm sandbox-demo)" "chasm-workspace-sandbox-demo" "default workspace volume"
assert_eq "$(incus_workspace_volume_name agent-sandbox sandbox-demo)" "agent-sandbox-workspace-sandbox-demo" "custom workspace volume"
assert_eq "$(incus_template_instance_name chasm minimal 0123)" "chasm-template-minimal-0123" "default template name"
assert_eq "$(incus_template_instance_name agent-sandbox minimal 0123)" "agent-sandbox-template-minimal-0123" "custom template name"
assert_eq "$(incus_template_instance_pattern chasm minimal)" "^chasm-template-minimal-" "default template pattern"
assert_eq "$(incus_template_instance_pattern agent-sandbox minimal)" "^agent-sandbox-template-minimal-" "custom template pattern"

# --- 2. descriptor readers ---------------------------------------------------

descriptor_fixture() { # prefix-value label -> writes $WORK/site-<label>.conf
    site_fixture_value=$1
    site_fixture_label=$2
    {
        cat "$ROOT/testdata/fixtures/descriptor-cases/valid-base.conf"
        if [ "$site_fixture_value" != absent ]; then
            printf 'resource_prefix=%s\n' "$site_fixture_value"
        fi
    } >"$WORK/site-$site_fixture_label.conf"
}

site_prefix_of() { # fixture-file -> prints the resolved SITE_RESOURCE_PREFIX
    site_case=$1
    site_case_output=$(
        CHASM_SITE_CONF="$site_case" sh -c \
            ". \"$SITE_LIB\"; site_require_descriptor && printf '%s' \"\$SITE_RESOURCE_PREFIX\"" 2>&1
    ) || { die "descriptor $site_case did not parse: $site_case_output"; }
    printf '%s' "$site_case_output"
}

descriptor_fixture absent default
descriptor_fixture "" empty
descriptor_fixture "agent-sandbox" custom
assert_eq "$(site_prefix_of "$WORK/site-default.conf")" "chasm" "absent resource_prefix defaults to chasm"
assert_eq "$(site_prefix_of "$WORK/site-empty.conf")" "chasm" "empty resource_prefix defaults to chasm"
assert_eq "$(site_prefix_of "$WORK/site-custom.conf")" "agent-sandbox" "custom resource_prefix round-trips"
site_value_output=$(CHASM_SITE_CONF="$WORK/site-custom.conf" sh -c ". \"$SITE_LIB\"; site_value resource_prefix")
assert_eq "$site_value_output" "agent-sandbox" "site_value resource_prefix"

# --- 3. template identity separation -----------------------------------------

# fake incus: image info and profile show with stable outputs
cat >"$WORK/bin/incus" <<'STUB'
#!/bin/sh
set -eu
case "$1" in
    image)
        printf 'Fingerprint: deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef\n'
        ;;
    profile)
        printf 'config: {}\ndescription: test\ndevices: {}\nname: chasm\nused_by: []\n'
        ;;
    *)
        echo "fake incus: unsupported invocation: $*" >&2
        exit 9
        ;;
esac
STUB
chmod 0755 "$WORK/bin/incus"
PATH="$WORK/bin:$PATH"
export PATH
mkdir "$WORK/profiles"
cp "$ROOT"/guest-config/profiles/*.profile "$WORK/profiles/"
chmod 0644 "$WORK/profiles"/*.profile
export CHASM_PROFILE_ROOT=$WORK/profiles

resolve_identity() { # resource-prefix -> prints TEMPLATE_IDENTITY
    resolve_prefix=$1
    (
        SCRIPT_DIR=$ROOT/scripts
        BASE_IMAGE_ALIAS=chasm-test-13
        PROFILE_NAME=chasm
        BOOTSTRAP_PROFILE=minimal
        RESOURCE_PREFIX=$resolve_prefix
        SITE_GUEST_UID=4242
        export SCRIPT_DIR BASE_IMAGE_ALIAS PROFILE_NAME BOOTSTRAP_PROFILE RESOURCE_PREFIX SITE_GUEST_UID
        # shellcheck source=scripts/lib/sandbox-template-identity.sh
        . "$IDENTITY_LIB"
        resolve_sandbox_template_identity || exit 1
        printf '%s' "$TEMPLATE_IDENTITY"
    )
}

identity_default=$(resolve_identity chasm) || die "default-prefix identity did not resolve"
identity_custom=$(resolve_identity agent-sandbox) || die "custom-prefix identity did not resolve"
identity_default_again=$(resolve_identity chasm) || die "default-prefix identity did not resolve again"
assert_eq "$identity_default" "$identity_default_again" "identity is deterministic per prefix"
[ "$identity_default" != "$identity_custom" ] ||
    die "two resource prefixes must not share one template identity"
case "$identity_default" in
    *[!0-9a-f]*|'') die "identity is not a sha256: $identity_default" ;;
esac

# --- 4. no hardcoded Incus object names in the launch path -------------------

for launch_script in "$ROOT/scripts/launch-sandbox-vm.sh" "$ROOT/scripts/build-sandbox-template.sh"; do
    if grep -nE 'user\.chasm\.|chasm-(shared|workspace|template)' "$launch_script"; then
        die "$launch_script hardcodes an Incus object name; names must derive from lib/incus-names.sh"
    fi
done
if grep -nE '\^chasm-template-|user\.chasm\.' "$ROOT/scripts/install-chasm.sh"; then
    die "install-chasm.sh hardcodes an Incus object name in a template probe"
fi

echo "test-resource-prefix: derivation, descriptor readers, identity separation, and no-hardcoding checks passed"
