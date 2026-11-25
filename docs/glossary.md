---
title: "Glossary"
permalink: /glossary.html
---
# Glossary

- **Site descriptor** — `/etc/chasm/site.conf`: the flat
  `key=value` file that holds every host fact (bridge, storage pool,
  users, admission reserve). Copied from
  `host-config/site.conf.example` at provisioning; both descriptor
  readers fail closed on unknown keys, missing required keys, malformed
  values, or contradictory settings. The example file documents the full
  schema and marks which keys are optional.
- **Site identity pin** — the fingerprint written once to
  `/etc/chasm/site-host-id` during provisioning. The executor verifies the
  host against it when it starts; the privileged shell entry, the firewall
  helper, the sandbox launcher, and the template builder verify it each time
  they are invoked. Bootstrap and rendering steps that run before the pin
  exists validate the descriptor instead.
- **Sealed template** — the finished sandbox image built by
  `scripts/build-sandbox-template.sh` from a bootstrap profile: packages
  installed, the guest account created, credential files checked absent and history cleared,
  snapshot sealed. Every `chasm create` clones it; the template itself
  never runs as a sandbox.
- **Template identity** — the versioned digest (currently v6) computed over
  the base image alias and fingerprint, the Incus profile name and contents,
  the bootstrap profile name and resolved manifest, the resource prefix,
  the site guest uid, the guest zshrc, the packages layer and profile reader
  scripts, and the builder and identity-helper scripts and policies. Stored on the
  template instance; a change to any of these inputs produces a different identity, so
  verification reports the old template as stale until it is rebuilt.
- **Bootstrap profile** — the manifest (`guest-config/profiles/*.profile`,
  default `minimal`) naming the Debian packages a sealed template
  installs.
- **Guest account** — the unprivileged account `agent` (uid and gid
  pinned to the descriptor's `guest_uid`, home `/home/agent` mode 0750,
  login shell `/usr/bin/zsh`) the packages layer creates inside every
  sealed template. No password, no supplementary groups, no sudo
  rights: the launcher provisions `/workspace` and `/shared` for this
  uid, and `chasm shell` opens as this account. The account carries a
  minimal `.zshrc` (template input `guest-config/guest/zshrc`) so an
  interactive zsh never runs its first-run newuser menu.
- **Logical name vs. instance name** — the operator-facing sandbox name
  (`research`, 1–32 lowercase letters/digits/hyphens) versus the Incus
  instance it maps to (`sandbox-research`). CLIs take logical names;
  `incus` sees instance names.
- **Memory budget** — the per-sandbox memory declaration in MiB
  (512 MiB floor, default 3072) set at `create` time. It becomes the VM's
  `limits.memory` ceiling and the unit of admission: starts are refused
  when running budgets plus the new budget exceed the host's cap (host
  memory minus `host_reserve_mib`). A budget above that cap is refused at
  `create` time, naming the cap's source.
- **CPU budget** — the per-sandbox CPU declaration set at `create` time
  (`--cpu N`, default 2). It becomes the VM's `limits.cpu` count; a
  budget above the host CPU count is refused at `create` time.
- **Host reserve** — `host_reserve_mib` in the site descriptor (required
  key; supported range 0–1048576 MiB; 1024 in the shipped descriptor):
  host memory the admission gate never allocates to sandboxes or the
  template builder.
- **Default sandbox** — `default_sandbox` in the site descriptor: the
  sandbox `chasm shell`, `start`, `stop`, and `show` without `--layout` (and
  the `status` alias) address when NAME is omitted. `pause`, `resume`,
  `resize`, `describe`, and `delete` always require an explicit NAME.
- **Resource prefix** — `resource_prefix` in the site descriptor
  (default `chasm`): the prefix for instance metadata keys
  (`user.<prefix>.*`), the shared and workspace volumes, and the sealed
  template instances. Sandbox instances always use the fixed `sandbox-`
  prefix, and the bridge, storage pool, profile, and base-image alias have
  their own site keys. Host-side names
  (units, paths, users, the nftables table) never derive from it. Leave it
  at the default unless you are adopting an existing installation's Incus
  objects (for example ones named `agent-sandbox-*`).
- **Workspace and shared volumes** — each sandbox's persistent
  `/workspace` volume (its own) and the global `/shared` volume (all
  sandboxes). Both are virtiofs disk shares inside the VM, never host
  mounts.
- **Clipboard bridge** — the desktop-side helper that forwards
  application text copies from inside `chasm shell` to the desktop
  clipboard; see [clipboard.md](clipboard.md).
- **Declared vs. enforced** — for one resource, what chasm recorded in
  the instance's budget keys (declared) versus what Incus applies
  (`limits.memory`, `limits.cpu`, the root device `size`;
  enforced). chasm writes both together and reads both back; `chasm
  show` reports each side with its source (idiom 1 and idiom 2).
- **Drift** — a sandbox whose declared and enforced values differ,
  typically because the instance was changed with plain `incus config
  set` behind chasm's back. `chasm show` prints the drift with the
  command that resolves it; `chasm resize` brings both sides to the
  target value. Admission counts the larger of the two.
- **Host cost** — what a sandbox actually spends on the host, as opposed
  to what it is limited to: the instance process's resident memory
  (`VmRSS`), the pool space its disks occupy. `chasm show` reports it
  per resource so an operator never needs `top` or `/proc` (idiom 2).
