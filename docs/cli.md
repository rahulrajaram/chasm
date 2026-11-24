---
title: "Command reference"
permalink: /cli.html
---
# Command reference

Two CLIs ship with chasm. `chasm` is the operator's daily interface;
`chasmctl` exposes the supervisor's full surface, mostly as JSON, for
scripting. `chasm help COMMAND` and `chasmctl help COMMAND` print each
CLI's per-command help; neither needs a readable site descriptor, so
help still works on a host whose descriptor is missing or invalid.

## `chasm`

```
chasm
chasm COMMAND [OPTIONS]
chasm help [COMMAND]
```

Running `chasm` with no arguments lists sandboxes. Every command accepts
`-h`/`--help`. A `--` marker ends option and help-flag scanning: everything
after it is taken as literal data, so `chasm describe NAME --set -- --text`
stores the text `--text`.

| Command | Form |
| ------- | ---- |
| `create` | `chasm create NAME [BUDGET_MEMORY_MIB]` |
| `show` | `chasm show [NAME] [--layout]` |
| `resize` | `chasm resize NAME [--memory SIZE] [--cpu N] [--root-disk SIZE]` |
| `describe` | `chasm describe NAME [--set TEXT \| --clear]` |
| `repos` | `chasm repos [REPOSITORY]` |
| `start` | `chasm start [NAME]` |
| `pause` | `chasm pause NAME` |
| `resume` | `chasm resume NAME` |
| `stop` | `chasm stop [NAME]` |
| `delete` | `chasm delete NAME` |
| `shell` | `chasm shell [NAME]` |

### `create NAME [BUDGET_MEMORY_MIB]`

Create a named sandbox from the sealed template. `NAME` is 1–32
lowercase letters, digits, or hyphens (leading/trailing hyphens
rejected). The optional budget is in MiB with a 512 MiB floor (default
3072; `--memory MIB` is the same setting), and `--cpu N` sets the CPU
budget (default 2). Memory budgets reserve host headroom: a later
`start` is refused when the running budgets plus this one would exceed
the host's cap (host memory minus `host_reserve_mib`), counting each
sandbox at the larger of its recorded and enforced value. CPU is capped
per sandbox at `create` and `resize` only — a budget above the host CPU
count is refused there; start admission does not sum CPU. A memory
budget above its host cap is refused at `create` time, naming the cap's
source. The new sandbox gets its
own persistent `/workspace` volume plus the global `/shared` volume and
is left stopped.

#### Sandbox storage

| Location | Who mounts it | What happens on delete |
| --- | --- | --- |
| VM root disk, including `/home/agent` | Only this sandbox | Removed with the VM |
| `/workspace` | Only this sandbox | Removed with the sandbox |
| `/shared` | Every sandbox on the site, read-write | Retained when an individual sandbox is deleted |

All three persist across `chasm stop` and `chasm start`. Use `/workspace`
for project files that should stay private to one sandbox. Use the root
disk for files that belong to the VM, such as tools and account settings.
Use `/shared` for deliberate collaboration. File permissions still apply
inside each mount.

The root disk and `/workspace` are separate storage objects on the site's
Incus pool. Host administrators can access both. A workload can still
copy its own files to `/shared` or send them over the network.

### `show [NAME] [--layout]`

Show details for one sandbox, or the site's `default_sandbox` when the
site descriptor declares one (NAME is required otherwise). `--layout`
shows a filesystem observation bounded to three levels. Running
sandboxes are observed live; stopped ones return the last saved
observation or report that they have not yet been observed.

After the detail fields, `show` prints a Resources section that reports
each resource from every angle that can disagree (idiom 2). Every value
names its source; a value chasm cannot observe is shown as `unknown`
with the reason, never guessed or left out:

- **declared** — what chasm recorded in the sandbox's budget keys;
- **enforced** — what Incus applies (`limits.memory`, `limits.cpu`, the
  root device `size`); when the two differ, `show` prints a drift line
  naming the command that resolves it, for example `drift: memory record
  20480 MiB, enforced 49152 MiB; fix: chasm resize NAME --memory 49152`;
- **guest** — what the guest agent reports (`memory.total`,
  `memory.usage`); a VM whose agent is not reporting shows these as
  unknown;
- **host cost** — what the host actually spends: the instance process's
  `VmRSS` while the sandbox runs (a paused one still has a process, so
  its resident memory is reported too), the workspace volume's usage,
  the root disk's `pool holds` figure (the storage pool's usage of the
  root volume, as the storage driver reports it — not what the guest's
  `df` shows), and the storage pool's own numbers.

The human view labels each value with the angle it comes from —
declared, enforced, guest, or host — and the root-disk row names
whether its size comes from the site profile or an instance override.
The JSON output (`chasmctl` without human mode) additionally carries
each value's exact source string. The workspace row carries the pool's size and used
space next to the volume's, because a quota-less `/workspace` shares the
pool: the guest's `df` on `/workspace` shows pool-wide numbers, and this
row is where those numbers come from. For a loop-backed pool, the pool
row also shows the loop file's allocated bytes against its apparent size
and the free space of the filesystem that holds it.

Every `show` — named or default — ends with a host line: `MemTotal`,
`MemAvailable`, `host_reserve_mib`, the sum of the larger of recorded
and enforced memory of running sandboxes, and the host's observed own
usage (total minus available minus the sandboxes' resident memory). When that observed own
usage exceeds `host_reserve_mib`, chasm warns that the admitted budgets
may not fit and names the fix (raise `host_reserve_mib` in site.conf and
rerun provision).

### `describe NAME [--set TEXT | --clear]`

Show, set, or clear a sandbox's human-readable description. `TEXT` is
one trimmed printable line, at most 120 characters. Literal text that
starts with `-` is given after the end-of-options marker:
`chasm describe NAME --set -- --text`.

### `repos [REPOSITORY]`

List every observed Git repository across sandboxes, or locate one by
basename. Discovery never starts a stopped sandbox; results say whether
each observation is live, last-observed, or unavailable. Extra worktrees
under `/workspace/.worktrees` stay visible in the sandbox layout but are
not listed as independent projects.

### `start [NAME]`

Start a sandbox and wait for its guest agent. Named sandboxes may run
together when their declared budgets fit host headroom.

### `pause NAME` / `resume NAME`

Pause freezes the VM while preserving its running state; `resume`
continues the paused VM in place. Both take an explicit NAME (neither
addresses a default sandbox). Instance and volumes are untouched.

### `stop [NAME]`

Stop a sandbox without deleting persistent state: the guest is asked to
shut down and the command waits for the stopped state.

### `resize NAME [--memory SIZE] [--cpu N] [--root-disk SIZE]`

Resize a **stopped** sandbox. At least one flag is required; `SIZE`
accepts plain MiB integers (`2048`, as `create` reads a memory budget)
and explicit suffixes (`2GiB`, `2048MiB`). The command is refused while
the sandbox runs — stop it first with `chasm stop NAME`.

- `--memory` rewrites the declared budget **and** the enforced
  `limits.memory` together; a target above the host's cap (host memory
  minus `host_reserve_mib`) is refused with the cap's source named.
- `--cpu` rewrites the CPU budget and `limits.cpu`; the cap is the host
  CPU count.
- `--root-disk` grows the root device only: a smaller size is refused
  with the current size named, re-applying the current size is a proven
  no-op, and the new size may not exceed the storage pool's configured
  size.

The changes apply in a fixed order (memory, CPU, root disk) and every
change is proven by reading the applied value back. A failure part-way
reports exactly which changes were applied and which were not. A memory
resize also repairs drift: when the recorded budget and the enforced
limit differ (for example after a change made outside chasm),
`chasm resize NAME --memory X` brings both to X. Re-applying the same
values is a no-op that still proves the current values.

### `delete NAME`

Delete a **stopped** sandbox and its workspace volume (deletion is
refused while it runs). Each teardown step — instance, per-sandbox
`/workspace` volume, catalog entry — is proven before the delete
succeeds. The global `/shared` volume is never touched. Passing a
`sandbox-`-prefixed instance name is refused. Idempotent: an
already-absent sandbox reports `already_deleted`.

### `shell [NAME]`

Open an interactive login shell as the unprivileged agent user in
`/home/agent`, using the site's guest uid/gid. Chasm does not grant
general Incus or root access. On a Linux desktop, application text
copies are bridged to the desktop clipboard with `xclip` (X11) or
`wl-copy` (Wayland) when installed — but only when the command runs
wrapped in the standalone `chasm-clipboard` helper (see
[clipboard.md](clipboard.md)); unwrapped, `chasm shell` is a plain
terminal session. For remote hosts, run
`chasm-clipboard -- ssh -t HOST chasm shell NAME` on the desktop.

### Compatibility commands

`list`, `status`, `inspect`, `layout`, and `locate` are retained for
migration and map onto `chasm`, `show`, `show`, `show --layout`, and
`repos` respectively. Each one prints its own usage line when given bad
arguments. They work, but new scripts and documentation should use the
preferred forms.

## `chasmctl`

```
chasmctl [--supervisor-socket PATH] COMMAND ...
```

`chasmctl` is the operator-level CLI over the supervisor. Output is
JSON by default; `SANDBOXING_OUTPUT=human` switches to human-readable
output (the `chasm` CLI sets this automatically when it delegates).
Every command below talks to the supervisor over its Unix socket,
`/run/chasm-supervisor.sock` by default; pass `--supervisor-socket PATH`
to override it. As with `chasm`, `chasmctl help` and `chasmctl help
COMMAND` work without a readable site descriptor, so a host whose
descriptor is missing or invalid still gets working help.

Supervisor commands:

```
create-sandbox INSTANCE [BUDGET_MEMORY_MIB]
describe-sandbox INSTANCE
set-sandbox-description INSTANCE DESCRIPTION
list-sandboxes
inspect-sandbox INSTANCE
start-sandbox INSTANCE
stop-sandbox INSTANCE
pause-sandbox INSTANCE
resume-sandbox INSTANCE
resize-sandbox INSTANCE [--memory SIZE] [--cpu N] [--root-disk SIZE]
                 (SIZE: MiB integer, or GiB/MiB suffix)
delete-sandbox INSTANCE
admit-launch INSTANCE
sandbox-layout INSTANCE DEPTH (1-5)
list-repositories
locate-repository REPOSITORY
sandbox-status
ensure-guest
stop-guest
collect-performance PHASE
```

The commands take Incus instance names (`sandbox-NAME`); `chasm`
translates logical names to them automatically. `chasmctl help` prints
the full synopsis table.

## Process exit status

Chasm control commands return 0 on success, 1 on configuration, authorization
or operation failure, and 2 for rejected usage. Help exits 0 when it
successfully writes the requested topic. A failure to write help or a
`chasmctl` operation result returns status 1. Most commands load the installed site configuration before
validating arguments, so a missing configuration can produce status 1 first.
The interactive shell delegates to the guest shell through sudo and Incus, and
its exit status reflects the delegated process.

For automation, inspect the resulting sandbox state before retrying a failed
state-changing request. An audit persistence error can follow a successful
side effect.
