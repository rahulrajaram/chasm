# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.1.0] - Unreleased

First public release. chasm is a Go + POSIX sh host control plane that
runs disposable, resource-bounded sandboxes as full Incus VMs on a
Debian host (x86_64; tested on Debian 13, Debian 12 untested), behind a
narrow root executor and an unprivileged supervisor.

### Added

- `chasm show` reports each sandbox resource from every view that can
  disagree (idiom 2): declared budget, enforced Incus limit, guest-agent
  figures, and host cost (the instance process's resident memory, the
  workspace volume's usage, and the storage pool's numbers, including a
  loop-backed pool's real allocation against its apparent size and the
  free space behind it). Every value names its source; an unobservable
  value is shown as unknown with a reason. Drift between declared and
  enforced is printed with the `chasm resize` command that resolves it.
- A host line on every `chasm show` (named or default): MemTotal,
  MemAvailable, `host_reserve_mib`, the sum of the larger of recorded and
  enforced memory of running sandboxes, and the host's observed own usage. chasm warns when that
  observed own usage exceeds `host_reserve_mib` (admitted budgets may not
  fit) and when a loop-backed pool's size exceeds what its loop file and
  backing free space could actually fill (idiom 4).
- Operator CLI `chasm`: create, start, stop, pause, resume, delete,
  show (details and observed filesystem layout), describe, repos, and
  interactive shell; compatibility aliases `list`, `status`,
  `inspect`, `layout`, and `locate`.
- Operator CLI `chasmctl` over the supervisor: full sandbox lifecycle,
  repository discovery, and site operations.
- Declared per-sandbox memory budgets (512 MiB floor, default 3072 MiB;
  the per-sandbox ceiling is host memory minus `host_reserve_mib`, named
  in refusals) and CPU budgets (`--cpu N`, default 2, capped by the host
  CPU count) enforced as VM `limits.memory`/`limits.cpu` limits, recorded
  in instance metadata beside the enforced value, read back at write
  time, and counted at the larger of recorded and enforced by the
  host-headroom admission gate (the 3072 MiB default applies only when a
  sandbox carries neither side); the template builder admission counts
  the larger of recorded and enforced `limits.memory` under the same
  gate, and fails closed on an unreadable or unsupported value in the
  instance inventory.
- nftables bridge firewall in a dedicated `inet chasm` table:
  IPv4 DHCP/DNS-only inbound from the sandbox bridge, with drops for
  routed IPv6 and forwarding to private/reserved IPv4. Sandboxes on
  the same bridge can communicate directly.
- Standalone desktop clipboard bridge (`chasm-clipboard`, xclip/wl-copy)
  that wraps `chasm shell` (and other interactive commands), including use
  over SSH from a separate desktop machine; the core `chasm` binary links
  no clipboard code.
- Minimal sealed guest template (`minimal` bootstrap profile) with a
  versioned template identity (v6) and a scrubbed, reproducible build.
- `chasm resize` re-applying the current memory, CPU, or root disk value
  is a proven no-op: memory and CPU rewrite their idempotent values and
  prove the read-back, and an equal root disk target performs no write at
  all (a profile-inherited device is never turned into an instance
  override) while the read-back still proves the value; shrinking the
  root disk remains refused. The supervisor forwards
  `budget_cpu` on create, resize reports each change with its unit, and
  `chasm show` degrades per view when an enforced value cannot be parsed:
  the section is never dropped, drift is reported as unverifiable, and a
  peer the host view cannot verify is counted and noted.
- Site installer `scripts/install-chasm.sh`: preflight, approved
  step-by-step provisioning (`--provision`, default-verify `--verify`
  drift report), opt-in SSH hardening (`--harden-ssh`, off by default),
  and opt-in Incus installation (`--install-incus`, off by default) from
  Debian's own repositories: the release's own packages on Debian 13
  (trixie) and newer, `bookworm-backports` on Debian 12 (bookworm).
- Site descriptor `/etc/chasm/site.conf` with a pinned site identity
  marker; both descriptor readers fail closed on unknown or malformed
  keys. Set optional `profile_root_disk_gib` before first provisioning to
  choose a root disk that fits a smaller pool.
- Read-only sandbox diagnostic
  (`scripts/diagnostics/sandbox-health.sh`).
- `chasm resize NAME [--memory SIZE] [--cpu N] [--root-disk SIZE]`
  (and `chasmctl resize-sandbox`): the supported path for resizing a
  **stopped** sandbox. Memory and CPU rewrites set the recorded budget
  key and the enforced `limits.memory`/`limits.cpu` together, refuse
  targets above the host-derived caps (host memory minus
  `host_reserve_mib`; the host CPU count) with the cap's source named,
  and prove every write by reading it back; the memory form also repairs
  drift between the recorded and enforced values. The root disk grows
  only, never past the storage pool's configured size, and a profile-
  inherited root device gains an instance-level override. Changes apply
  in a fixed order and a partial failure names exactly what was applied
  and what was not; the operation is audited and replay-protected like
  every privileged one.
- Pool growth through provision (idiom 3): editing `pool_size_gib` and
  rerunning `install-chasm.sh --provision` grows the existing storage
  pool after a host-backing verdict (added ceiling plus margin must fit
  beside the loop file, numbers named) and proves the new size by
  reading it back; a smaller value stops provision before any other
  change, naming both sizes.
- Uninstall and upgrade documentation; MIT license.

### Fixed during release testing

- First-run installer and template builder preserve exit status and clean
  up interrupted staging. Reprovisioning reloads the active firewall.
- The allocator excludes the configured gateway, and preflight detects
  overlapping main-table routes. Guest network configuration matches
  the Incus NIC by MAC address.

### Not included

This release deliberately installs **no AI agents**, ships **no model
relay**, and has **no workspace export/import pipeline**; the control
plane is the whole product. Agent templates, a relay, and the workspace
export/import pipeline are planned for a later release.

### Notes for existing private deployments

- The site descriptor keys `pi_runtime`, `pi_head_repository`,
  `legacy_pilot`, `relay`, `relay_listen_ipv4`, `relay_listen_port`, and
  `legacy_receipts_remote` are removed and are now rejected as fatal
  unknown keys by both readers; remove them from the descriptor before
  upgrading.
- The host-side `agent-sandbox` names are gone: the systemd units are
  `chasm-executor`/`chasm-supervisor`/`chasm-firewall`, the directories
  are `/etc/chasm`, `/usr/local/lib/chasm`, `/var/lib/chasm`, and
  `/run/chasm*`, the supervisor user is `chasm-supervisor`, the Incus
  profile is `chasm` with `chasm-template-*` template instances and
  `user.chasm.*` metadata, the nftables table is `inet chasm`, and the
  default bridge is `chasmbr0`.
- The operator CLI `sandboxctl` is now `chasmctl` (installed at
  `/usr/local/bin/chasmctl`); the session/receipt workflow commands are
  removed with the sealed-session pipeline.
- Installer sshd hardening is opt-in: `--skip-ssh-hardening` and
  `--force-ssh-hardening` are gone; hardening now runs only with
  `--harden-ssh` (off by default). Its staging and restore are protected
  against signals and shell aborts, not against SIGKILL or power loss.
- The template identity policy advanced to v6 during this release's
  development (v4 dropped the removed provisioning layers, v5 added the
  neutral guest zshrc, and v6 added the site guest uid); existing sealed
  templates are reported stale and must be rebuilt (`--provision` offers
  the rebuild;
  the builder is memory-admitted beside running sandboxes: keyless
  sandboxes count at the 3072 MiB default, and `host_reserve_mib` must
  be a canonical nonnegative integer — no sign, no leading zeros).
- Renamed operations and commands: the executor operation
  `inspect_pilot` is now `inspect_default_sandbox`, the sandboxctl
  command `pilot-status` is now `sandbox-status`, and
  `scripts/launch-pilot-vm.sh` is now `scripts/launch-sandbox-vm.sh`.
