---
title: "Idioms"
permalink: /idioms.html
---
# Idioms

These are the rules chasm's code is held to. Each one comes from a real
failure: a number that meant one thing to chasm and another to the
guest, or an operator who had to go around chasm to get something done.
A change that breaks an idiom needs a reason in its commit message and a
test that pins the new behavior.

The idioms sit on top of the failure posture in
[architecture.md](architecture.md): refuse rather than guess, and prove
every change by reading it back.

## 1. One truth per resource

Every sandbox resource has exactly one number, and it is the number
Incus enforces.

| Resource | The truth | Where chasm records it |
| -------- | --------- | ---------------------- |
| Memory | the instance's `limits.memory` | the budget key must equal it |
| CPU | the instance's `limits.cpu` | the CPU budget key must equal it |
| Root disk | the root device `size` | read from Incus, not stored separately |
| Workspace | the workspace volume | read from Incus, not stored separately |

Where chasm keeps its own record (the budget keys that admission sums),
the record and the enforced value are written together, read back
together, and must match. chasm is the only writer: a sandbox changed
with plain `incus config set` is in **drift**, and chasm says so instead
of silently trusting either side.

**Check:** admission, `chasm show`, and every lifecycle path read the
enforced Incus value. When the record and the enforced value differ,
admission counts the larger of the two.

## 2. Show the truth, and flag drift

An operator should never need `incus`, `top`, `df`, or `/proc` to learn
what chasm's numbers mean. `chasm show NAME` reports each resource from
every angle that can disagree:

- **declared**: what chasm recorded;
- **enforced**: what Incus applies;
- **guest**: what the guest sees (for example `MemTotal`, `df`);
- **host cost**: what the host actually spends (for example the VM
  process's resident memory, or the pool space a disk occupies).

A mismatch between declared and enforced is printed as drift, with the
command that resolves it. A value chasm cannot observe is shown as
unknown, never guessed or left out.

**Check:** every resource `chasm show` prints names its source, and a
test covers the drift line.

## 3. Every operator need has a supported path

If operators routinely need a change, chasm provides a command for it,
so nobody has to edit Incus or the site descriptor by hand. Resizing a
sandbox's memory, CPU, or root disk goes through `chasm resize`; growing
the storage pool goes through the site descriptor and
`install-chasm.sh --provision`.

Each supported change:

- runs through the same admission as `create` and `start`;
- refuses what it cannot do safely (shrinking a disk, resizing a
  running sandbox where Incus cannot apply it live);
- writes the record and the enforced value together and proves both by
  reading them back;
- is appended to the executor's audit log like every privileged
  operation.

**Check:** no document tells operators to run `incus config set`,
`incus storage set`, or to edit a sandbox's `user.*` keys.

## 4. Commit only what the host can back

Limits come from the host chasm runs on, not from constants. A sandbox's
memory cap is the host's memory minus `host_reserve_mib`; its CPU cap is
the host's CPU count; the pool's size must fit in the host free space
that backs it, minus a margin. Admission sums the enforced commitments
of running sandboxes (idiom 1) against those host figures and refuses
up front. Overcommit should be refused by chasm, not discovered by the
kernel's out-of-memory killer.

`host_reserve_mib` is the memory the host keeps for itself: the desktop,
browsers, and agents running outside sandboxes. When the host is
observed using more than its reserve, chasm warns, because the budgets
it admitted no longer fit.

**Check:** no resource cap in the code is a fixed number of MiB or GiB
unless it is a floor, or a bound that keeps arithmetic safe; each cap's
source is named in the refusal message.
