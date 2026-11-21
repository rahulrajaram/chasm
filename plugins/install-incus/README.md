# install-incus plugin (v1)

Debian-specific Incus package acquisition for a chasm site. See
`docs/plugins.md` for the plugin contract; core installs, verifies, and
runs without this directory, and without `--install-incus` core performs
no distro package installation at all. Incus itself remains a core
prerequisite — this plugin is a Debian convenience and does not widen the
supported platform list.

## Interface

- Shape: standalone host provisioning script (v1): `incus-install.sh`,
  sourced by the installer's `--install-incus` dispatch.
- Invocation: only through
  `install-chasm.sh --provision --install-incus`, and only when preflight
  found incus absent. There is no other supported entry point.

## Prerequisites and effects

- Prerequisites: root, `apt-get` on PATH, and a supported Debian release:
  Debian 13 (trixie) and newer install `incus virtiofsd` from the release's
  own repositories; Debian 12 (bookworm) installs from
  `bookworm-backports` (refused up front when no enabled backports source
  is configured). The host must be Debian itself (ID=debian): derivatives
  and other distributions are refused before any approval.
- Effects: `apt-get update` plus one `apt-get install` for `incus` and
  `virtiofsd`, after an explicit approval prompt. Nothing else is touched.

## Trust checks (unchanged from when this was a core step)

- Derivatives are refused before approval (their VERSION_ID is not
  Debian's).
- The bookworm path refuses when no backports apt source is configured;
  the refusal explains how to add one.
- Only Debian's own repositories are used; no third-party source.

## Check / remove

- Check: preflight reports incus and virtiofsd presence as functional
  prerequisites regardless of how they were installed; `install-chasm.sh
  --verify` drift-probes the incus binary only.
- Remove: `apt-get remove incus virtiofsd` (the plugin performs no
  removal itself).

## Failure behavior

Every refusal (non-Debian host, unsupported release, missing backports
source) happens before the approval prompt. A request for
`--install-incus` without this plugin in the tree fails with a clear
message naming the missing plugin file. Without the flag, a missing incus
fails provision with the manual-install guidance instead of installing
anything.
