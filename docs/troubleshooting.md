---
title: "Troubleshooting"
permalink: /troubleshooting.html
---
# Troubleshooting

Two read-only tools answer most questions: the installer's verify mode
and the sandbox diagnostic.

```sh
# Check the site descriptor, host identity, binaries and units.
# Verify Incus, firewall and SSH configuration.
sudo scripts/install-chasm.sh --verify

# Inspect one sandbox.
sudo scripts/diagnostics/sandbox-health.sh NAME
```

The diagnostic accepts a logical name (`development`) or an instance
name (`sandbox-development`) and reports, changing nothing: host memory
and disk headroom, the pool's total/used/free bytes against the site's
declared pool size, the instance and last logged QEMU run states,
whether the QEMU process still exists, persistent root/workspace volume
state, snapshot counts, and a bounded guest mount probe when the VM
runs. After installation the same script lives at
`/usr/local/lib/chasm/provision/scripts/diagnostics/sandbox-health.sh`.

Reading its output:

- A snapshot count of zero means there is no saved point-in-time copy;
  the persistent volumes are unaffected.
- `/workspace` and `/shared` sit on the same storage pool, so their free
  space is one shared budget; the diagnostic itself reports no per-mount
  figure — use `pool_free_bytes` for real headroom. (A hypothetical `df`
  inside the guest on either mount would show that same shared pool, but
  no such field is emitted by this tool.)
- `workspace_du_bytes` is a `du` estimate, not allocation.

**Supervisor fails to start: "could not be resolved through the system
account lookup (NSS)"**
The supervisor resolves the configured operator through host NSS, but its
systemd unit permits only Unix-domain sockets. Local account files work;
network-backed NSS needs a local Unix-socket proxy such as SSSD or nslcd,
whose socket must also be accessible under the unit's other restrictions.
Direct network-socket NSS backends are unsupported. Installer preflight
runs `getent` outside the unit, so a passing preflight does not prove the
supervisor can resolve the account. Check
`journalctl -u chasm-supervisor.service` for the lookup error.

## Common refusals and their fixes

**Provisioning refuses: "nested KVM is still enabled"**
The supported site shape disables nested virtualization. Install the
shipped modprobe drop-in and reload the module — exact commands in
[install.md](install.md#disabling-nested-virtualization).

**Provisioning refuses: "disk headroom ... is insufficient"**
The filesystem carrying `/var/lib/incus` needs the pool size (64 GiB by
default) plus a fixed 10 GiB margin free. Free space there or lower
`pool_size_gib` in the descriptor before provisioning.

**`chasm start` refuses, naming a memory resource**
Admission refused the start: the sum of running sandbox budgets plus
this sandbox's budget exceeds this host's cap — host memory minus the
site's `host_reserve_mib` (the refusal names both numbers). Stop another
sandbox, lower this one's budget at `create` time, or raise host memory.

**`chasm create` refuses: "budget ... exceeds this host's cap"**
The requested memory budget (or `--cpu` count) is above what this host
can back: memory is capped by host memory minus `host_reserve_mib`, CPU
by the host CPU count. The refusal names the cap and its source; request
a smaller budget or add host resources.

**A sandbox's resources need to change after creation**
Use `chasm resize NAME --memory SIZE --cpu N --root-disk SIZE` (at
least one flag) on a **stopped** sandbox; it rewrites the recorded
budget and the enforced Incus limit together and proves both by reading
them back. The same command repairs drift — a sandbox whose recorded
budget and enforced limit disagree (changed outside chasm) is brought
back in line for the resource you resize. Never edit `user.*` metadata
keys or run `incus config set` on a managed instance; that is exactly
what creates drift.

**`chasm resize` refuses: "stop the sandbox first"**
Resize is a stopped-instance operation; run `chasm stop NAME` first and
resize, then `chasm start NAME`.

**`chasm show` prints: "drift: memory record X, enforced Y"**
The sandbox's recorded budget key and its enforced Incus limit disagree
— the instance was changed outside chasm. The drift line names the fix:
`chasm resize NAME --memory <enforced value>` brings the record and the
limit back together (run it while the sandbox is stopped). Admission
counts the larger of the two sides until the drift is repaired, so a
drifted sandbox may be refused at start even when its record looks
small enough.

**`chasm show` prints: "the host is using X MiB for itself but
host_reserve_mib is Y"**
The host's observed own usage — MemTotal minus MemAvailable minus the
running sandboxes' resident memory — exceeds the reserve the site set
aside for the desktop and everything outside sandboxes. The budgets
chasm admitted may no longer fit. Raise `host_reserve_mib` in site.conf
and rerun provision; until then, starts may be refused or the host may
come under memory pressure.

**`chasm show` prints: "the pool could not be filled"**
For a loop-backed pool, the pool's configured size exceeds the loop
file's actually allocated bytes plus the free space of the filesystem
that holds it. The pool cannot grow to its declared size. The warning
names the loop file's path and the MiB still needed (size minus
allocated minus free): free that much space on the filesystem that
holds the loop file. Do not grow the pool — a larger configured size
widens the shortfall, and provision preflight refuses a pool resize
that the backing filesystem cannot cover.

**`chasm resize --root-disk` refuses: grow-only or pool ceiling**
A root disk can only grow: the refusal names the current size. A smaller
size is refused; re-applying the current size is a proven no-op (nothing
is written; the current value is proven by read-back). A new size above the storage pool's
configured size is refused too — grow the pool first (edit
`pool_size_gib` and rerun `install-chasm.sh --provision`; see
[install.md](install.md)).

**`chasm delete` refuses: the sandbox is running**
Stop it first (`chasm stop NAME`). Passing a `sandbox-`-prefixed
instance name is also refused; use the logical name.

**Diagnostic reports `diagnosis=pool_exhaustion_paused_vm`**
The pool has no free space and QEMU's last logged state is `io-error`:
the guest was paused after an I/O failure. Create free space on the pool
first (Incus documents online growth for its managed loop-backed pools),
then bring the VM back. `chasm resume NAME` is the path that keeps the
process and its memory, but it requires the paused (`Frozen`) state; the
diagnosis above reports `incus_status=ERROR`, which resume refuses
("sandbox instance must be paused to resume"). If the instance is merely
paused, resume it directly; if the state is `ERROR`, there is no
supported resume path in this build — after freeing pool space, the
operator must investigate and recover the ERROR instance using Incus/QEMU
procedures outside Chasm's supported start/resume paths (then re-run the
diagnostic); this build provides no start or resume path for an ERROR
instance and no guaranteed recovery recipe. The diagnostic never grows a pool, resumes a
VM, or takes a snapshot by itself.

**`install-chasm.sh --verify` reports a stale sealed template**
The tree changed since the template was sealed. Re-run `--provision`
and accept the template rebuild (it is admitted beside running
sandboxes when host memory fits); see
[upgrade-and-uninstall.md](upgrade-and-uninstall.md).

**The template build refuses: "IPv4 address ... is already claimed by NAME"**
The builder VM's address is fixed (the descriptor's `sandbox_ipv4_prefix`
plus `.11`) and is never reselected, and another instance already claims
it — through an eth0 `ipv4.address` override, the site's `instance-ipv4`
metadata, or a live address while it runs. Check the named instance and
its owner before changing it. A preserved builder can hold the address
after a build failure. The failure prints the receipt path, and the
receipt identifies any `preserved_builder`.

To resume a stopped builder with its matching receipt, run the
installed builder as an administrator with both values from that receipt:

```sh
sudo env SANDBOX_TEMPLATE_RESUME_BUILDER="$builder_name" \
    SANDBOX_TEMPLATE_RESUME_RECEIPT="$receipt_path" \
    /usr/local/lib/chasm/provision/scripts/build-sandbox-template.sh
```

If sealing completed before the final rename, use the same command with the passed receipt. The builder verifies the stopped instance and its sealed snapshot, then finishes the rename.

For an interrupted build with a failed receipt, the builder validates the base image, profile, guest configuration and
completed package layer before sealing. If the retained builder cannot
pass those checks, confirm its ownership and stopped state, then remove
that builder only if its data can be discarded. Rerun provisioning to
build from scratch and verify the site.

**A privileged step refuses: "site identity check failed"**
The pinned identity marker at `/etc/chasm/site-host-id` does not
match this machine (for example, the descriptor or marker was copied
from another host). Privileged actions fail closed. Re-pin deliberately
by re-running provisioning on the host you intend to keep.

**`chasm shell` reports the privileged shell entry was refused**
The sudo hop requires the real operator: the entry helper only accepts
root invoked through sudo by the site's `operator_user`, with exactly
one safe sandbox name. Run `chasm shell NAME` directly as the operator
user, not via an escalated shell.

**Copies inside `chasm shell` do not reach the desktop clipboard**
Bridging happens only under the standalone `chasm-clipboard` helper; an
unwrapped `chasm shell` is a plain terminal session with no clipboard
bridge. The helper needs `xclip` (X11) or `wl-copy` (Wayland) installed on
the desktop and application OSC 52 support; an intervening tmux must
forward it (`set -s set-clipboard on`). Details in [clipboard.md](clipboard.md).

**SQLite fails on `/workspace` or `/shared` (WAL, mmap, `SQLITE_IOERR`)**
`/workspace` and `/shared` are virtiofs mounts backed by the host storage
pool, and they do not support the shared-memory mapping (`xShmMap`) that
SQLite's WAL journal mode requires. Observed symptoms on these mounts:

- Database initialization aborts with `Error code 5386: I/O error within
  the xShmMap method (trying to map a shared-memory segment into process
  address space)` and leaves a broken partial store behind.
- Preallocated SQLite databases of roughly 344 KiB or more failed
  `PRAGMA integrity_check` or already failed to open with `SQLITE_IOERR`.
- Native SQLite runs that placed live scratch on the workspace virtiofs
  mount failed before their assertions ran.

The fix: keep SQLite databases on the guest's own disk (for example under
`/home/agent` or `/var/tmp`) or open them with
`PRAGMA journal_mode=DELETE`, which needs no shared-memory map. This
applies to every tool that embeds SQLite (indexers, caches, agent state),
not only to the `sqlite3` CLI.

**Verify reports "socket is not active"**
The socket-activated executor or supervisor socket is down. Check
`systemctl status chasm-executor.socket chasm-supervisor.socket` and the
service journals (`journalctl -u chasm-executor.service`); the executor
also refuses to start when the identity marker is missing.

If the firewall helper reports that it cannot verify old bridge accepts,
restore the matching iptables frontend and retry the operation. The helper
keeps filtering in place when existing legacy rules or tagged rescue rules
make cleanup ambiguous.
