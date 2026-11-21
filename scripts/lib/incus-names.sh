#!/bin/sh

# Incus object naming, shared by the launcher, the template builder, the
# installer, and the unprivileged name-derivation harness
# (scripts/test-resource-prefix.sh).
#
# Every Incus object this site owns is named from the site's resource_prefix
# (a site descriptor key; default chasm): the instance-config metadata
# namespace user.<prefix>.*, the global shared custom volume, the per-sandbox
# workspace custom volumes, and the sealed template instances. Host-side
# names (systemd units, /etc/chasm, /usr/local/lib/chasm, /run/chasm*,
# /var/lib/chasm, the chasm-supervisor user, the nftables table, binaries,
# sudoers, and the ssh drop-ins) are product constants and NEVER derive from
# the prefix. The Incus profile and the bridge are their own site keys
# (profile_name, bridge_name) and are not derived either.
#
# The Go control plane mirrors these derivations in
# internal/siteconfig (Site.MetadataKey, Site.SharedVolumeName,
# Site.WorkspaceVolumeName, Site.TemplateInstanceName); the two must agree.

# incus_metadata_key PREFIX FIELD: the instance-config (or storage-volume)
# metadata key for one site-owned field.
incus_metadata_key() {
    printf 'user.%s.%s\n' "$1" "$2"
}

# incus_shared_volume_name PREFIX: the site's global shared custom volume.
incus_shared_volume_name() {
    printf '%s-shared\n' "$1"
}

# incus_workspace_volume_name PREFIX INSTANCE: the per-sandbox workspace
# custom volume of one instance.
incus_workspace_volume_name() {
    printf '%s-workspace-%s\n' "$1" "$2"
}

# incus_template_instance_name PREFIX PROFILE IDENTITY_PREFIX: the sealed
# template instance name for one bootstrap profile and the first 24 hex
# characters of a template identity.
incus_template_instance_name() {
    printf '%s-template-%s-%s\n' "$1" "$2" "$3"
}

# incus_template_instance_pattern PREFIX PROFILE: the anchored grep pattern
# matching every sealed template instance of one profile (identity prefixes
# are hex, so the pattern is ERE-safe for safe-name prefixes).
incus_template_instance_pattern() {
    printf '^%s-template-%s-' "$1" "$2"
}
