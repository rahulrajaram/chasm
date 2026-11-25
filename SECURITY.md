# Security Policy

## Supported versions

chasm is pre-1.0. Security fixes are applied to the latest `0.1.x`
release line only; earlier 0.1.x minors do not receive backports.

| Version | Supported |
| ------- | --------- |
| 0.1.x (latest) | Yes |
| anything older | No |

## Reporting a vulnerability

Report through GitHub's private vulnerability reporting on the project
repository: open **Report a vulnerability** under the repository's
Security tab, or go directly to:

https://github.com/rahulrajaram/chasm/security/advisories/new

- This project does not accept security reports by email; the private
  reporting channel above is the only supported route.
- Please do not open public issues or pull requests for suspected
  vulnerabilities, and do not include exploit details in them.

Helpful things to include: the commit or release you tested, the
component involved, steps to reproduce, and the impact you observed. A
proof of concept is welcome but not required.

### What to expect

- Every report is read and triaged through the private channel.
- The reporter is kept informed of triage decisions and fix progress in
  the same private thread.
- Fixes are coordinated with the reporter before publication where
  practical.
- Credit in the published advisory is offered unless the reporter
  declines it.

This is a small project; no specific response-time targets are promised
at this stage.

## Scope

chasm is a host control plane that runs untrusted workloads — code,
tooling, and fetched content you do not trust — inside Incus
virtual-machine sandboxes on a Debian Linux host (x86_64; tested on
Debian 13, Debian 12 untested). The first public release ships the
control plane only: it installs no AI agents, and agent templates
arrive separately.

## Threat model summary

The supported deployment has one trusted operator per site. The VM
boundary separates workloads from the host. Each sandbox alone mounts
its own root disk and `/workspace` volume, so either can hold files that
should stay private to that sandbox at the storage layer. Host
administrators retain access. All sandboxes on a site mount `/shared`
read-write and can communicate over the stock bridge. A guest can copy its
own files to `/shared` or send them over the network.

### Trusted

- **The host kernel, the KVM hypervisor, Incus, and systemd.** chasm
  does not attempt to survive the compromise of any of these; a kernel
  or hypervisor escape defeats the sandbox.
- **The host administrator.** Anyone with host root can read or modify
  everything chasm protects, including sandbox disks and audit logs.
- **chasm's own privileged code**: the root executor service and the
  small set of privileged helpers described below. A bug in this code
  can compromise the host, which is why it is kept minimal.
- **The operator account** that runs `chasm`, and the site
  configuration it reads. The operator decides what may enter a
  sandbox.

### Not trusted

- **Everything inside a guest sandbox**: any process run there, the
  code it writes, and the content it downloads or fetches.
- **Any data an operator moves into a guest**, whether typed or pasted.

### How the design contains privilege

1. **A narrow, audited lifecycle path.** Privileged lifecycle operations run
   in a root executor service behind a systemd-activated unix socket;
   the socket is owned by root, group-restricted to the supervisor
   service user, and mode 0660, and the executor rejects any connection
   whose peer uid is not the supervisor's.
2. **Peer-credential gating.** On every connection the executor reads
   the kernel-provided peer credentials (`SO_PEERCRED`) and refuses the
   connection unless the peer's uid is exactly the expected supervisor
   uid; this check happens before any request is parsed. The
   unprivileged supervisor applies the same mechanism with a uid
   allowlist before serving its own clients.
3. **Executor outcome auditing.** After dispatch, the executor attempts
   to append the outcome to a root-owned 0600 JSONL
   audit file (`/var/lib/chasm/executor/audit.jsonl`) inside a 0700
   directory, recording time, peer uid, request id, operation, and
   error code. Connections refused at the peer-credential gate and
   requests that cannot be read or parsed are refused without an audit
   record. If the audit record cannot be persisted, the peer receives a
   generic error. A dispatched operation may already have taken effect.
   Inspect the current state before retrying a state-changing request.
4. **The supervisor is unprivileged.** It runs as its own service user,
   separate from the root executor. The operator-facing
   supervisor socket is likewise root-owned, group-restricted, and mode
   0660. The executor service runs under systemd hardening
   (`ProtectSystem=strict`, `NoNewPrivileges`, `PrivateDevices`,
   `MemoryDenyWriteExecute`, a restricted syscall surface, and more).
5. **Narrow sudoers scope.** The shell entry receives one safe sandbox
   name. The foreground exec entry accepts the exact `chasm __exec`
   command and receives guest arguments over a bounded pipe protocol.
   Both verify the operator and site identity and run as the configured
   unprivileged guest UID/GID. Exec also validates target ownership and
   requires the local Incus socket peer to be UID 0. The sudo grant disables
   `NOPASSWD` and `NOSETENV` are supported by both sudo and sudo-rs. Classic
   sudo may still apply `log_output` or `use_pty`; in that case its changed
   stdio causes the exec helper to refuse the request before reading it
   unless stdin and stdout are both FIFOs (`cmd/chasm/exec.go`,
   `runPrivilegedExec`). Chasm's separate mode-0600
   `/var/lib/chasm/executor/exec-audit.jsonl` records argv0, the argument
   count and digest, cwd, environment key names, and execution status.
   It excludes argument tails, environment values, and stream payloads.
   Incus has its own operation metadata and logging behavior.
6. **Guests are virtual machines behind a scoped bridge filter.** Each
   sandbox is an Incus virtual machine. A dedicated nftables table for
   the sandbox bridge accepts only IPv4 DHCP and DNS inbound from guests,
   and drops other bridge input; forwarded guest traffic to private and
   reserved IPv4 ranges (RFC 1918, loopback, link-local, shared address
   space, benchmarking, documentation, and multicast ranges) is
   dropped. The host firewall drops IPv6 traffic from the sandbox bridge
   at both input and forward hooks. Public IPv4 egress remains permitted
   by the shipped rules.
7. **Firewall changes are identity-gated and fail-closed.** The
   firewall helper refuses to apply or remove rules unless the host's
   pinned site identity verifies, and on removal it verifies its rules
   are actually gone before deleting the vetting table, preferring a
   failed stop over an open bridge. When `iptables` is available, the
   helper inserts bridge-scoped accepts in `INPUT`/`FORWARD` only for
   packets the sandbox table has already vetted, which runs first. It
   does not rewrite Docker's rules.

### What chasm does not protect against

- **A malicious host administrator**, or anyone who already has host
  root.
- **Kernel or hypervisor escapes**, or compromise of Incus or systemd.
- **Data you bring into a sandbox.** Anything typed or pasted into a
  guest must be treated as disclosed to whatever runs there. Clipboard
  bridging moves data at your request; it does not inspect contents.
- **Cross-sandbox data flow through the global `/shared` volume.**
  Every sandbox on the site mounts `/shared` read-write, so anything
  one guest writes there is exposed to every other guest.
- **Same-bridge peer traffic.** The default Incus bridge permits IPv4
  traffic between sandboxes. Traffic switched within the bridge, including
  IPv6 link-local traffic, can remain outside the host IP routing hooks.
- **Public-internet egress.** The shipped firewall rules drop guest
  traffic to private and reserved ranges; they do not drop traffic to
  public destinations. Whether a guest can reach the public internet
  depends on the surrounding site configuration — assume it can unless
  you have verified otherwise.
- **Resource exhaustion.** Sandboxes are given bounded virtual-machine
  resources, but chasm does not guarantee host availability against a
  determined abusive guest.

When in doubt, treat any statement above as a design intent with a
concrete mechanism behind it, not a formal guarantee: if you find code
that does not match this document, that is a security issue and we want
to hear about it.
