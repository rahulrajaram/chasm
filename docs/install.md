# Installing chasm

These instructions provision a fresh Debian (x86_64) host — tested on
Debian 13; Debian 12 is untested. Fedora 45, Ubuntu 24.04/26.04, and Arch
also completed trial-only runs, but remain unqualified for this source until
the current portability fixes are rerun through install, upgrade, and verify.
The installer is
`scripts/install-chasm.sh` in this repository; run it from a checkout.

```sh
git clone https://github.com/rahulrajaram/chasm.git
cd chasm
sudo scripts/install-chasm.sh --provision
```

## Use a trusted checkout

Provisioning runs as root and uses code and configuration from this
checkout, so only run it from a checkout you trust. Before loading code,
the installer checks checkout ownership and permissions. With root
provisioning, every path in the checkout (except `.git`) must be owned by
root or by the invoking user, and no path may be world-writable. A
world-writable path is refused. Group-writable paths are allowed but
produce a warning because root will use those files. A direct root
invocation therefore needs a root-owned checkout; when invoked through
`sudo`, the checkout may be owned by the user who invoked `sudo`.

## Requirements recap

- Debian on x86_64 (tested on Debian 13; Debian 12 is untested).
- `/dev/kvm` present, and nested virtualization **disabled** (below).
- systemd, nftables and iproute2. A fresh managed Btrfs pool needs
  `btrfs-progs`, and the managed Incus bridge needs `dnsmasq-base`.
- Incus, its host `incus-agent` executable/package, and `virtiofsd` — install them yourself, or opt in to the
  acquisition step with `--install-incus` during provisioning. That step
  uses Debian's own repositories: the release's repositories on Debian
  13 (trixie) and newer, or `bookworm-backports` on Debian 12 (bookworm;
  refused when no `bookworm-backports` source is configured).
- A working QEMU VM backend for Incus. On Debian amd64, keep the matching
  QEMU packages, including `qemu-utils`, `ovmf`, and
  `qemu-system-modules-spice`, when installing with recommendations disabled.
  Install the QEMU modules from the same Debian suite as `qemu-system-x86`.
  If QEMU came from `trixie-backports`, select that suite for `qemu-utils`
  and `qemu-system-modules-spice` too.
  The default Chasm profile enables UEFI Secure Boot. A container-capable
  Incus daemon can still lack a working VM driver.
- A Go toolchain (1.23 or newer; the version pinned in `go.mod`) visible
  to root (the control plane is built from this checkout during
  provisioning). A private build cache is used when the environment supplies
  neither a home directory nor a cache path.
- Internet access at provision time (the Debian package install and the
  base-image download).
- Roughly 74 GiB free disk on the `/var/lib/incus` filesystem (64 GiB
  storage pool plus a fixed 10 GiB margin checked by preflight), and
  memory for each sandbox budget you plan to run plus the host. Budgets
  have a 512 MiB floor and no fixed product cap: a budget is refused when
  it exceeds the host's memory minus `host_reserve_mib`.

## Disabling nested virtualization

Preflight refuses to continue when the loaded KVM module reports nested
virtualization enabled. The supported site shape keeps guests from
launching their own hardware-accelerated VMs; the check makes that shape
explicit rather than advisory.

The fix is shipped in the tree. Install the matching drop-in and reload
the module (or reboot):

```sh
sudo install -o root -g root -m 0644 \
    host-config/modprobe.d/kvm-intel-no-nesting.conf \
    /etc/modprobe.d/kvm-intel-no-nesting.conf   # or kvm-amd-no-nesting.conf
sudo modprobe -r kvm_intel && sudo modprobe kvm_intel   # or kvm_amd
```

The drop-in sets `nested=0` for the module. Host-created VMs — chasm's
own sandboxes included — are unaffected.

## Installer modes

```text
install-chasm.sh [--help|--check|--provision|--verify] [--harden-ssh]
                 [--install-incus] [--allow-untested-host]
```

- `--provision` — first-run installation. Every root step prompts for
  approval on the terminal before it runs; `CHASM_ASSUME_YES=1` in the
  environment pre-approves every step. Re-runs resume: finished steps
  are detected and skipped.
- `--verify` — read-only identity, drift, and service-health report. This
  is the default when the script is run with no argument. It changes
  nothing; probes that need root are skipped with an explanation when run
  unprivileged.
- `--check` — the same probes as `--verify` under a check-summary label.
- `--harden-ssh` — opt in to the sshd hardening step of `--provision`
  (below). It is accepted only with `--provision` and is a usage error
  with any other mode. Requires the optional `plugins/ssh-hardening`
  add-on (see [plugins.md](plugins.md)); without the plugin in the tree
  the flag fails with a clear message.
- `--install-incus` — opt in to the Incus acquisition step of
  `--provision` (below). It is accepted only with `--provision` and is a
  usage error with any other mode. Requires the optional
  `plugins/install-incus` add-on; without the flag the installer installs
  no packages and requires incus and virtiofsd to be present already.
- `--allow-untested-host` — explicitly bypass the non-Debian qualification
  gate for provisioning. Capability checks still run, and this does not make
  the distro supported. Equivalent environment opt-in: set
  `CHASM_ALLOW_UNTESTED_HOST=1`.
- `--help` — the full installer surface.

`CHASM_SITE_CONF` selects an alternate descriptor path for read-only
runs; it is rejected under `--provision`, which always writes and pins
the installed descriptor.

See [site configuration](site-config.md) for descriptor values and their
change considerations.

Installer exit status is 0 on completed provisioning or a clean verification,
1 on installer-reported failure or verification drift, and 2 on invalid command
usage. A subordinate tool can propagate a different failure status.
A failed provisioning step can leave earlier steps applied. Verify mode may
report skipped root-only checks while returning 0. Use root when qualifying
all checks and read the skipped count in the summary.

## What provisioning does

In order, one approval per step:

1. **Preflight** (read-only): Debian-family qualification gate (non-Debian
   hosts require `--allow-untested-host` or `CHASM_ALLOW_UNTESTED_HOST=1`),
   systemd, nftables, `/dev/kvm`
   with nesting off, disk headroom, storage pool growth plan, Docker
   coexistence detection (informational), sandbox bridge prefix collision,
   operator account, resolved through host NSS, Go toolchain, Incus and
   virtiofsd and host `incus-agent` presence, and a Go toolchain at the
   version required by `go.mod`. The installed supervisor unit permits only
   Unix-domain sockets. Network-backed NSS configurations therefore
   require a local Unix-socket resolver proxy (for example, SSSD or
   nslcd), with its socket accessible under the unit's other
   restrictions; direct network-socket NSS backends are unsupported.
   Preflight runs `getent` outside the unit and does not enforce this
   limit, so it may pass even when the supervisor later fails to resolve
   the operator. Capability checks on an untested distro do not make it
   supported. Automatic Incus package installation remains Debian-only.
   Incus socket discovery uses `INCUS_SOCKET`, then the root-owned
   `/etc/chasm/incus-socket` setting, then `/var/lib/incus/unix.socket`,
   then `/run/incus/unix.socket`. The socket and all parent directories must
   be root-owned; parents may not be group- or world-writable. An explicit
   override must name an existing local Unix socket. Set `INCUS_SOCKET`
   during provisioning to persist that path in root-owned
   `/etc/chasm/incus-socket` (mode 0600); Chasm services and the narrow shell
   helper use this fixed setting without inheriting arbitrary caller
   environment. A later `--provision` without an explicit override removes
   the prior setting and selects a standard socket. With sudo `env_reset`, pass
   overrides as `sudo INCUS_SOCKET=/path/to/socket install-chasm.sh --provision`
   or `sudo CHASM_ALLOW_UNTESTED_HOST=1 install-chasm.sh --provision`;
   variables set only in the invoking shell are dropped.
2. **Incus install** (only with `--install-incus` and only if absent):
   `apt-get install incus virtiofsd` from the release's own repositories
   on Debian 13 (trixie) and newer; on Debian 12 (bookworm),
   `apt-get install -t bookworm-backports incus virtiofsd`. Debian's own
   repositories either way, no third-party source, and the bookworm step
   is refused up front when no `bookworm-backports` apt source is
   configured. Without `--install-incus` the installer installs no
   packages: install incus and virtiofsd yourself and re-run. The
   acquisition logic lives in the optional `plugins/install-incus`
   add-on ([plugins.md](plugins.md)); Incus itself remains a prerequisite
   validated by preflight either way.
3. **Descriptor write (first provision only)**: copies
   `host-config/site.conf.example` to `/etc/chasm/site.conf` (root:root
   0644) and fills in the resolved `site_name` and `operator_user`; on a
   re-run the existing descriptor is kept unchanged.
4. **Identity pin**: writes this machine's fingerprint to
   `/etc/chasm/site-host-id` (root 0444, create-once). Every later
   privileged entry verifies the host against this pin.
5. **Payload install**: builds the Go binaries and installs them under
   `/usr/local/libexec/chasm/` (SELinux enforcing hosts run `restorecon`
   and verify `bin_t` labels), symlinks `chasm` and
   `chasmctl` into `/usr/local/bin/`, copies the provision tree (scripts,
   host and guest configuration) under
   `/usr/local/lib/chasm/provision/`, installs the core profile manifest
   into `/etc/chasm/profiles/` (only when absent; see
   [plugins.md](plugins.md) for upgrade semantics), and installs the firewall helper
   (`/usr/local/sbin/chasm-firewall`), its ruleset template
   (`/etc/chasm/firewall.nft.template`), and the control-plane units,
   sockets, sudoers, sysusers, and tmpfiles fragments.
6. **SSH hardening** (opt-in, off by default): with `--harden-ssh`, the
   step (the `plugins/ssh-hardening` add-on; [plugins.md](plugins.md)) installs two sshd_config drop-ins that disable SSH password and
   root login (public-key only) and reloads sshd. The drop-ins also
   disable keyboard-interactive authentication and add
   `.ssh/authorized_keys-chasm` as an accepted authorized-keys file.
   Before asking for approval it warns plainly that it makes these changes and that the
   operator must already hold a working SSH key for the operator account
   or console access — a host with neither is locked out; there is no key
   guard. The confirmation itself must be typed (`yes`) on the terminal:
   `CHASM_ASSUME_YES` does not pre-approve this one step. The step stages
   signal-safely: existing drop-ins are copied (never
   moved) to a backup directory, a trap restores them on any failure or
   interruption, the full configuration is validated with `sshd -t`
   before anything is reloaded, and only after the reload succeeds is the
   backup removed. When only `ssh.socket` is active the drop-ins take
   effect for new connections (socket activation starts a fresh sshd per
   connection; a `.socket` unit is never reloaded); when no ssh unit is
   active the step says so and the drop-ins take effect at the next sshd
   start. Without the flag the step
   prints one skip line and leaves already-installed drop-ins alone.
7. **Incus initialization**: `incus admin init` from the rendered
   preseed — a btrfs loop-backed storage pool (64 GiB by default, from
   `pool_size_gib`), a managed IPv4 NAT bridge, and the sandbox profile.
   When the pool already exists (the re-run path), the provision applies
   the pool growth plan instead: see
   [Growing the storage pool](#growing-the-storage-pool).
8. **Sealed template build**: builds the sealed sandbox template from the
   site's bootstrap profile (`minimal` by default) in a disposable builder
   VM. The build installs the profile's package list and creates the
   neutral guest account every core path assumes: user `agent` with uid
   and gid pinned to the descriptor's `guest_uid`, home `/home/agent`
   (mode 0750), login shell `/usr/bin/zsh`, and no password, groups, or
   sudo rights. The account carries a minimal `/home/agent/.zshrc`
   (from `guest-config/guest/zshrc`, agent:agent 0644) so `chasm shell`
   never lands in zsh's first-run newuser menu. The first build downloads the base image named by
   `base_image_remote`; expect a multi-hundred-megabyte download and a
   slow first run. With the shipped defaults, the builder needs 3 GiB,
   the host reserve is 1024 MiB, and the build margin is 256 MiB, for a
   minimum `MemTotal` of 4352 MiB before adding configured budgets of
   running sandboxes. `MemAvailable` must also cover the need when the
   build starts. A rebuild is admitted while sandbox VMs run — the
   builder never stops or touches them — as long as `MemTotal` covers the
   profile's `limits.memory` plus `host_reserve_mib` plus a margin plus the
   configured budgets of every running sandbox (the same headroom rule
   sandbox starts use), and `MemAvailable` still covers the need at that
   moment; a short-memory host refuses, naming the numbers.
9. **Post-install verify** and the quickstart.

## Growing the storage pool

The supported path for growing the pool is the site descriptor plus
provision — never `incus storage set` by hand:

1. Edit `pool_size_gib` in `/etc/chasm/site.conf` to the new size in GiB.
2. Rerun `sudo install-chasm.sh --provision`.

Provision compares the descriptor with the pool's configured size:

- **Equal** — nothing to do.
- **Larger** — provision first checks that the host can back the growth:
  for the loop-backed pool, the added ceiling (the new size minus the
  loop file's currently allocated bytes) plus a 10 GiB margin must fit
  in the free space of the filesystem holding the loop file, with every
  number named in the verdict. With the operator's approval the pool is
  grown and the new size is proven by reading it back.
- **Smaller** — refused before any other change: `shrinking a storage
  pool is not supported; pool is XGiB, site asks for YGiB`. Choose a
  size at or above the current pool size.

Note that `pool_size_gib` is a ceiling on the loop file, not a
reservation: the file only grows as sandboxes actually use space.

## What the installer changes on the host

| Area | Change |
| ---- | ------ |
| Users | One system user for the supervisor (`chasm-supervisor` by default, via the sysusers fragment), home under `/var/lib/chasm/supervisor`, no login shell |
| Configuration | `/etc/chasm/site.conf` (the site descriptor) and the identity marker `/etc/chasm/site-host-id` |
| Binaries | `/usr/local/lib/chasm/` (binaries, provision tree) and symlinks in `/usr/local/bin/` |
| systemd units | `chasm-executor.service` + `.socket` (root), `chasm-supervisor.service` + `.socket` (unprivileged), `chasm-firewall.service` — all enabled |
| sudo | `/etc/sudoers.d/chasm` — grants the operator exactly the internal `chasm __shell` entry helper used by `chasm shell` |
| Firewall | `/usr/local/sbin/chasm-firewall` and `/etc/chasm/firewall.nft.template`; manages the `inet chasm` nftables table and bridge-scoped `iptables` accepts in `INPUT`/`FORWARD` when available; Docker rules are not rewritten |
| Incus | Storage pool (`chasm`), NAT bridge (`chasmbr0`), sandbox profile (`chasm`), and the sealed template instance; every site-owned Incus object is named from the descriptor's `resource_prefix` (default `chasm`): instance metadata keys `user.<prefix>.*`, the `<prefix>-shared` and `<prefix>-workspace-<instance>` volumes, and the `<prefix>-template-*` instances |
| sshd | with `--harden-ssh` only: `/etc/ssh/sshd_config.d/00-chasm.conf` and `99-chasm.conf` (off by default) |
| Temporary/runtime | sysusers and tmpfiles fragments under `/usr/lib/sysusers.d/` and `/usr/lib/tmpfiles.d/`; state under `/var/lib/chasm/`; sockets under `/run/` |

Beyond installing the Incus and virtiofsd packages (step 2), nothing
else on the host is modified. Removing everything is covered in
[upgrade-and-uninstall.md](upgrade-and-uninstall.md).

## Verifying an installed site

```sh
sudo scripts/install-chasm.sh --verify
```

The drift report checks the descriptor, the identity pin, every binary,
unit, socket, and fragment, the Incus pool, bridge, profile, and sealed
template (including template staleness against the current tree), the
nftables table, and — when run as root — the sshd hardening fragments:
absent fragments are reported as `ssh hardening: not applied (optional)`
(hardening is opt-in, so their absence is not drift), present fragments
are verified against the shipped content, and any foreign drop-in that
sorts before `00-chasm.conf` and sets the hardening keywords is drift
while a chasm fragment is installed (OpenSSH's first-value rule means
such a fragment wins); with the hardening not applied there is nothing
to override, so the foreign winner is only an informational note. An
unprivileged run skips that probe. The installed copy under
`/usr/local/lib/chasm/provision/scripts/` works the same way after the
checkout is gone.

Next: [first-sandbox.md](first-sandbox.md).
