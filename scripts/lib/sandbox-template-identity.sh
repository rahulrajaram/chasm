#!/bin/sh

# These policy identities name the scrub/seal outcome used by the admitted
# template. Identity v6 digests the RESOLVED bootstrap profile manifest, every
# remaining layer script, the neutral guest zshrc, and the site guest uid, so
# a change to any of them produces a new identity and therefore a coexisting
# template instead of a rebuild collision; only a deliberate scrub-policy or
# build-order change should advance these policy constants. The v4 identity
# dropped the removed provisioning layers (vendored runtimes, agent CLIs, and
# personal configuration) from the digest input set, the v5 identity added
# the guest zshrc (guest-config/guest/zshrc, installed as the guest account's
# /home/agent/.zshrc), and the v6 identity added the site guest uid (the
# packages layer bakes it into the sealed account: useradd/groupadd pin the
# agent uid and gid to it, the launcher provisions /workspace and /shared
# for it, and `chasm shell` opens as it); all three are template-content
# input-set changes, so the identity policy constant was deliberately
# advanced with each of them. The chasm rename
# (unit/instance/key names and paths, first public release) is a naming
# change only: it flows through the digested executable inputs (the builder,
# this helper, the profile reader, and the packages layer all renamed names
# and paths they reference) and through the format string below, so every
# composed identity changes deliberately WITHOUT advancing these policy
# constants — the scrub/seal policy and the build order are unchanged, and
# the digest input set keeps the v5 shape.
TEMPLATE_BUILDER_POLICY_SHA256=04ebadec7017587ad1d00102d41acb045a697d36b1a446dbeb273d720cd4defa
TEMPLATE_IDENTITY_POLICY_SHA256=e586be66e38608b3180d615720d0bd8ed4562372ea849f0754a93c1d690d07be

template_identity_fail() {
    echo "template identity: $*" >&2
    return 1
}

template_identity_file_sha256() {
    template_identity_path=$1
    [ -f "$template_identity_path" ] && [ ! -L "$template_identity_path" ] || {
        template_identity_fail "input is missing or unsafe: $template_identity_path"
        return 1
    }
    # Capture before trimming: in a bare `sha256sum | cut` pipeline the status
    # is cut's, which would hide a failed read.
    template_identity_sum=$(sha256sum "$template_identity_path") || return 1
    printf '%s\n' "${template_identity_sum%% *}"
}

# The resolved uppercase values are an intentional sourced-file interface.
# shellcheck disable=SC2034
resolve_sandbox_template_identity() {
    : "${SCRIPT_DIR:?SCRIPT_DIR is required}"
    : "${BASE_IMAGE_ALIAS:?BASE_IMAGE_ALIAS is required}"
    : "${PROFILE_NAME:?PROFILE_NAME is required}"
    : "${BOOTSTRAP_PROFILE:?BOOTSTRAP_PROFILE is required}"

    # The profile reader resolves the bootstrap profile manifest. This helper
    # is the one place that resolves CHASM_PROFILE_ROOT, through the pure
    # selector in scripts/lib/profile-root.sh, and it exports the choice so
    # every layer script the builder or launcher spawns sees the same profile
    # root. The precedence is strict: an explicit CHASM_PROFILE_ROOT wins
    # (the developer/test override), then the installed production tree
    # /etc/chasm/profiles whenever it exists (an installed chasm always has
    # it, and separately vended site profiles are vendored there, so they
    # are the ones that must be identity-bound), and only when no production
    # tree is installed does the checkout's guest-config/profiles serve as
    # the fallback. The resolved profile vars (PROFILE_ID, PROFILE_PACKAGES,
    # PROFILE_MANIFEST_SHA256) live in the calling shell after this function
    # returns, for callers that need the pins. The reader never sets
    # PROFILE_NAME: that name keeps the Incus-profile meaning the builder and
    # launcher assigned it before this helper ran, and
    # `incus profile show "$PROFILE_NAME"` below uses that meaning.

    # The site guest uid is template identity input (the packages layer bakes
    # it into the sealed account), so it must be present and a canonical
    # positive decimal spelling (digits only, no sign, no leading zeros —
    # the same rule the site readers enforce) before it enters the identity
    # preimage. The site reader validates the value and its range; this
    # guard only keeps a missing, malformed, or non-canonical value out of
    # the digest and fails with a clear message. Every caller sources
    # require-site-host and runs require_site_host before resolving, so the
    # validated SITE_GUEST_UID is always set here.
    case "${SITE_GUEST_UID:-}" in
        [1-9]|[1-9][0-9]*) ;;
        *)
            template_identity_fail "site guest uid is missing or invalid"
            return 1
            ;;
    esac
    provision_root=$(CDPATH='' cd -- "$SCRIPT_DIR/.." && pwd)
    # shellcheck source=scripts/lib/profile-root.sh
    . "$SCRIPT_DIR/lib/profile-root.sh"
    CHASM_PROFILE_ROOT=$(select_profile_root "${CHASM_PROFILE_ROOT:-}" \
        "$provision_root" /etc/chasm/profiles) || {
        template_identity_fail "no bootstrap profile root could be selected"
        return 1
    }
    export CHASM_PROFILE_ROOT
    # shellcheck source=scripts/lib/require-site-profile.sh
    . "$SCRIPT_DIR/lib/require-site-profile.sh"
    if ! require_site_profile "$BOOTSTRAP_PROFILE"; then
        template_identity_fail "bootstrap profile $BOOTSTRAP_PROFILE could not be read"
        return 1
    fi
    TEMPLATE_PROFILE_MANIFEST_SHA256=$PROFILE_MANIFEST_SHA256

    # The neutral guest zshrc is template identity input exactly like a layer
    # script: it ships in the provision tree beside the profiles and becomes
    # the guest account's /home/agent/.zshrc, so editing it must deliberately
    # change the sealed template identity. It resolves from the same provision
    # root selected above (explicit override, installed production tree,
    # or checkout fallback, in that order).
    TEMPLATE_GUEST_ZSHRC_SHA256=$(template_identity_file_sha256 "$provision_root/guest-config/guest/zshrc") || return 1

    # Every layer script participates in the identity: a change to any layer
    # produces a new identity and therefore a coexisting template instance.
    TEMPLATE_PACKAGES_LAYER_SHA256=$(template_identity_file_sha256 "$SCRIPT_DIR/install-guest-packages.sh") || return 1
    TEMPLATE_PROFILE_READER_SHA256=$(template_identity_file_sha256 "$SCRIPT_DIR/lib/require-site-profile.sh") || return 1
    TEMPLATE_BUILDER_EXECUTABLE_SHA256=$(template_identity_file_sha256 "$SCRIPT_DIR/build-sandbox-template.sh") || return 1
    TEMPLATE_IDENTITY_HELPER_EXECUTABLE_SHA256=$(template_identity_file_sha256 "$SCRIPT_DIR/lib/sandbox-template-identity.sh") || return 1
    TEMPLATE_BUILDER_SHA256=$TEMPLATE_BUILDER_POLICY_SHA256
    TEMPLATE_IDENTITY_HELPER_SHA256=$TEMPLATE_IDENTITY_POLICY_SHA256
    TEMPLATE_BASE_IMAGE_FINGERPRINT=$(
        incus image info "$BASE_IMAGE_ALIAS" |
            awk '/^Fingerprint:/ {print $2; exit}'
    ) || return 1
    # Incus appends dynamic name/used_by/project metadata to profile show.
    # Only config, description, and devices affect the template definition.
    TEMPLATE_PROFILE_SHA256=$(
        incus profile show "$PROFILE_NAME" | sed '/^name:/,$d' |
            sha256sum | cut -d' ' -f1
    ) || return 1

    for template_identity_digest in \
        "$TEMPLATE_PACKAGES_LAYER_SHA256" \
        "$TEMPLATE_PROFILE_READER_SHA256" \
        "$TEMPLATE_GUEST_ZSHRC_SHA256" \
        "$TEMPLATE_BUILDER_SHA256" \
        "$TEMPLATE_IDENTITY_HELPER_SHA256" \
        "$TEMPLATE_BUILDER_EXECUTABLE_SHA256" \
        "$TEMPLATE_IDENTITY_HELPER_EXECUTABLE_SHA256" \
        "$TEMPLATE_BASE_IMAGE_FINGERPRINT" \
        "$TEMPLATE_PROFILE_SHA256" \
        "$TEMPLATE_PROFILE_MANIFEST_SHA256"
    do
        case "$template_identity_digest" in
            *[!0-9a-f]*|'') template_identity_fail "input digest is invalid"; return 1 ;;
        esac
        [ "${#template_identity_digest}" -eq 64 ] ||
            template_identity_fail "input digest length is invalid" || return 1
    done

    TEMPLATE_IDENTITY=$(
        printf '%s\n' \
            'format=chasm-template-identity-v6' \
            "base_image_alias=$BASE_IMAGE_ALIAS" \
            "base_image_fingerprint=$TEMPLATE_BASE_IMAGE_FINGERPRINT" \
            "profile_name=$PROFILE_NAME" \
            "profile_sha256=$TEMPLATE_PROFILE_SHA256" \
            "bootstrap_profile=$BOOTSTRAP_PROFILE" \
            "resource_prefix=$RESOURCE_PREFIX" \
            "guest_uid=$SITE_GUEST_UID" \
            "profile_manifest_sha256=$TEMPLATE_PROFILE_MANIFEST_SHA256" \
            "packages_layer_sha256=$TEMPLATE_PACKAGES_LAYER_SHA256" \
            "guest_zshrc_sha256=$TEMPLATE_GUEST_ZSHRC_SHA256" \
            "profile_reader_sha256=$TEMPLATE_PROFILE_READER_SHA256" \
            "builder_policy_sha256=$TEMPLATE_BUILDER_POLICY_SHA256" \
            "identity_policy_sha256=$TEMPLATE_IDENTITY_POLICY_SHA256" \
            "builder_executable_sha256=$TEMPLATE_BUILDER_EXECUTABLE_SHA256" \
            "identity_helper_executable_sha256=$TEMPLATE_IDENTITY_HELPER_EXECUTABLE_SHA256" |
            sha256sum | cut -d' ' -f1
    ) || return 1
}
