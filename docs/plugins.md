# Plugins: the optional-component contract

Chasm core is self-contained. It builds, installs, verifies, and templates a
site with the entire `plugins/` directory absent. Everything under
`plugins/` is optional, explicitly vended source — core never discovers,
registers, probes, or auto-loads any of it.

## What core owns

Core owns the control plane (supervisor, executor, CLI), resource admission,
guest identity and the guest account, networking and isolation, the zsh shell
contract, timesync recovery, credential scrubbing and sealing, and template
identity. Core's supported platform is unchanged: Debian 12/13 amd64 hosts
with systemd, nftables, KVM (nested virtualization disabled), and Incus.
Incus is a core prerequisite — the plugin described below only helps acquire
it on Debian; nothing in the plugin contract widens the supported platform
list, and no unaudited architecture or provider is claimed as supported.

## What a plugin is

A plugin is one independent source subdirectory under `plugins/`, vended and
versioned by its own README. There is deliberately:

- no plugin discovery, registry, or `plugins.conf`,
- no general privileged callback or hook mechanism,
- no dependency graph between plugins,
- no CLI probing for installed helpers.

Plugins come in exactly three shapes:

1. **Guest package profiles** — data-only `<name>.profile` manifests in the
   existing validated format (see `scripts/lib/require-site-profile.sh`).
   They are never sourced or evaluated; a profile can never execute anything
   on the host, and its bytes are digest-bound into the sealed template
   identity like any core profile.
2. **Standalone user commands** — independently buildable programs (today a
   nested Go module using the standard library only) the operator builds and
   installs explicitly.
3. **Standalone host provisioning scripts** — opt-in, explicit-invocation
   scripts with their own typed confirmations, trust checks, and rollback;
   never run implicitly by the installer.

## Profile roots and upgrades

Production profile manifests live at `/etc/chasm/profiles/<name>.profile`;
an explicit `CHASM_PROFILE_ROOT` overrides the root for development and
tests, otherwise the installed `/etc/chasm/profiles` is used whenever it
exists, and only a host without a production tree falls back to the
checkout's `guest-config/profiles` (build-from-source). A separately
vended profile installed under `/etc/chasm/profiles` therefore wins over a
colocated checkout copy of the same name, and the sealed template identity
digests the selected manifest's bytes. The reader applies
the same fail-closed rules — safe name, regular non-symlink file, not group-
or world-writable, owned by root or the invoking user, plain-token values,
never sourced — to every manifest regardless of which root resolved it.

Upgrade semantics:

- The installer installs core's own reserved profile names (currently
  `minimal`) into `/etc/chasm/profiles` only when the file is absent. On an
  upgrade over an existing `minimal.profile` whose bytes differ from the
  shipped core manifest, the installed manifest is kept unchanged and a note
  is printed: an upgraded site keeps the package set its existing template
  was sealed from; only fresh installs get the new core manifest.
- Third-party and plugin manifests in `/etc/chasm/profiles` are never pruned
  or rewritten by a core upgrade. `core upgrade` touches only its reserved
  names; nothing else in that directory is core state.

Legacy vs fresh: a site provisioned before the developer tools were split
out of `minimal` keeps its installed `minimal.profile` (with the tools) on
upgrade, and its sealed templates keep working. A fresh site gets the lean
core manifest (`ca-certificates,zsh,systemd-timesyncd` — the certificates,
shell contract, and clock recovery core itself depends on) and installs
`plugins/developer-tools/developer-tools.profile` into the profile root when
it wants the tools. Core enforces its own prerequisites for custom profiles too: the package
layer seals the deterministic union of the mandatory core packages
(`ca-certificates`, `zsh`, `systemd-timesyncd`) with the selected
profile's addon packages, so a profile may declare only addons, and the
package layer verifies every effective package post-install against a
receipt bound to the effective list, the raw chosen profile packages, and
the chosen manifest digest. Profile-root precedence: an explicit
`CHASM_PROFILE_ROOT` wins, then the installed production tree
`/etc/chasm/profiles` (where separately vended site profiles land), and
only a host without a production tree falls back to a checkout's
`guest-config/profiles`.

## The plugins

| Plugin | Shape | Opt-in via |
| --- | --- | --- |
| `plugins/developer-tools` | guest package profile | profile root / `CHASM_PROFILE_ROOT` |
| `plugins/clipboard` | standalone user command (nested Go module) | explicit `chasm-clipboard -- chasm shell NAME` |
| `plugins/ssh-hardening` | host provisioning script | explicit `install-chasm.sh --provision --harden-ssh` |
| `plugins/install-incus` | host provisioning script | explicit `install-chasm.sh --provision --install-incus` |

Each plugin directory carries a README defining its interface and version
(v1), prerequisites and effects, selection/install/check/remove semantics,
and failure behavior. See `plugins/README.md` for the shared contract and
the per-plugin READMEs for specifics.

When a plugin-backed flag (`--harden-ssh`, `--install-incus`) is requested
but the plugin directory is absent from the source tree (or its installed
copy is absent from the provision root), the installer fails with a clear
message naming the missing plugin. With no flag, no plugin is consulted and
core produces no distro package-install, sshd, or clipboard side effect.
