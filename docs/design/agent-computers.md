# Agent computers: portability and synchronous execution

**Status:** design baseline for the next implementation tranche.

## Objective and evidence

Make Chasm practical as a persistent agent computer on hosts operators control: install on the non-Debian distributions already exercised, run one foreground command in a named guest as the existing unprivileged guest identity, and diagnose the host/site through a read-only JSON command. Preserve Chasm's single-operator model, host identity pin, fixed privileged operations, audit boundary, and lack of agent-specific services.

The cross-distro trial on 2026-11-24 reports full lifecycle success on Ubuntu 26.04 and 24.04, Fedora 45 with SELinux enforcing, and Arch. Ubuntu 24.04 required Go 1.23+ and the separately installed Incus agent; Fedora required a non-Debian gate, `/run/incus/unix.socket`, and executable SELinux labels; Arch required the same gate, hostname fallback, and `/usr/lib/virtiofsd`. The exact remaining qualifications in that report still apply: no reboot or upgrade trial, no Fedora firewalld trial, and no AMD or non-x86_64 validation. See `chasm-distros-20261124/REPORT.md` in the evidence archive.

The 2026-11-24 Fly/Sprites phase-one assessment recommends Go for the control plane, shell changes only at existing provisioning seams, synchronous execution before detached jobs, `doctor --json`, and stopped-only checkpoint/restore as a later tranche. It also flags synchronous work inside the current serial supervisor as a lifecycle starvation risk. See `chasm-flyalt-20261124/phase1-report.md`.

## Scope and sequence

### This tranche

1. Make installer preflight capability-based while preserving an explicit gate for untested distributions: require `--allow-untested-host` or `CHASM_ALLOW_UNTESTED_HOST=1` to proceed on an unqualified distro. Never silently treat an unknown distro as supported. Keep automatic package installation an explicit supported-distro choice; passing preflight does not infer package-manager compatibility.
2. Resolve Incus endpoint deterministically: non-empty `INCUS_SOCKET`, then `/var/lib/incus/unix.socket`, then `/run/incus/unix.socket`, selecting an existing usable socket and reporting the selected path. The systemd service must receive a deliberate, restricted environment: no inherited arbitrary `INCUS_*`, `PATH`, or user environment. If supporting `INCUS_SOCKET` for the service, provision it through an explicit service configuration value and validate it; otherwise environment override applies only to direct CLI/client contexts. Never silently fall back after a configured endpoint fails.
3. Install service executables under `/usr/local/sbin/` and operator CLIs under `/usr/local/bin/`. For SELinux systems, apply `restorecon` when available after installation and verify the resulting executable label in preflight/doctor. Package upgrades and verification must detect label drift and report the exact repair command. Do not add a custom SELinux policy in this tranche.
4. Determine the kernel hostname with a portable kernel interface (`uname -n`, with `/proc/sys/kernel/hostname` fallback as needed), not a required `hostname` utility.
5. Search virtiofsd at `PATH`, `/usr/libexec/virtiofsd`, `/usr/lib/virtiofsd`, and `/usr/lib/qemu/virtiofsd`; report the resolved executable.
6. Check for the host Incus agent artifact/package needed by template boot, and fail with a specific prerequisite and remedy instead of a generic guest-agent timeout. Keep host prerequisite and guest readiness checks distinct.
7. Enforce Go `go.mod`'s Go 1.23 minimum during preflight, before build work begins; document versioned package/link guidance for distributions whose default package is older.
8. Add `chasm exec NAME [--cwd DIR] [--env K=V ...] -- COMMAND ARGS...` as a synchronous foreground operation.
9. Add read-only `chasm doctor --json` with consolidated checks, observed state, severity, and supported remedies.

### Explicitly subsequent

- Checkpoints and restore: first stopped-only; cover root plus per-sandbox workspace with a crash-recoverable manifest and exclude global `/shared`.
- Opt-in `/shared` attachment and peer networking, with a migration decision for current guests before changing defaults.
- Local port forwarding, initially loopback-bound and explicit opt-in; public binds and TLS remain separate decisions.
- Optional stdio MCP adapter after CLI/result contracts stabilize. It must reuse Chasm authorization and must not expand privileges.
- Detached jobs, reattach, transfer, public/remote API, multi-user tenancy, distro-specific package managers/installers, and non-x86_64 qualification.

These are roadmap notes only; none is authorization for implementation in this tranche.

## Host portability contract

The installer separates **capability detection** from **supported automation**. Required capabilities include Linux, x86_64/KVM as currently supported, working systemd, Incus with an accessible local Unix socket, a suitable virtiofsd, required host utilities, enough storage, and a Go toolchain at the module minimum when building from source. Unknown distributions require the explicit `--allow-untested-host` flag or `CHASM_ALLOW_UNTESTED_HOST=1`; the installer must print that this bypasses the distro qualification gate. It must not silently continue on an untested distro or offer distro-specific package installation. The optional Incus installation plugin provides automatic package installation on Debian. Debian derivatives such as Ubuntu can pass the core distribution gate with their host prerequisites already installed.

The Incus resolver follows the order above and returns both the endpoint and its source. An explicitly set but absent/unusable `INCUS_SOCKET` is an error, not a reason to use a different daemon. Fallback candidates are considered only when no override is configured. The executor and service use the same resolution helper and the same local-only Incus invocation (`--force-local` where applicable); tests cover disagreement and precedence. The service environment is intentionally small and stable. It must not source a login shell, arbitrary environment file, or caller-controlled Incus remote configuration.

Service binaries move as one installation unit to `/usr/local/sbin/` (`chasm-executor-server` and `chasm-supervisor-server`); the operator CLIs and sudo re-exec helper stay in `/usr/local/bin/`. Upgrade replaces payloads at both `/usr/local/lib/chasm/go/` and `/usr/local/libexec/chasm/` with compatibility symlinks only after installing and validating the new payload; verification detects stale paths, missing files, and SELinux label drift. SELinux handling is conditional: when enforcing, require `restorecon` and verify `bin_t` on the installed files; when SELinux is absent or permissive, report not applicable. Provisioning/upgrade must not disable SELinux or install policy modules.

`doctor --json` is observational. It uses an exact, zero-argument sudo helper to read the root-only Incus configuration and observations. The helper verifies the operator and site identity before accessing Incus, queries only a running daemon, and leaves services, guest state, labels, and the firewall untouched. Each check has a stable id, status (`ok`, `warn`, `fail`, or `skipped`), summary, evidence/source, and remedy where one is supported. Output includes distro/kernel/architecture, resolved Incus socket, virtiofsd path, Go version when relevant, Incus-agent prerequisite, service/socket health, host identity pin, site descriptor validity, template freshness, resource drift/headroom, and guest readiness when that can be observed from the running VM. Unavailable or inapplicable probes are `skipped`. JSON reports version 1 and evolves additively within that contract.

## Synchronous execution interface

### CLI and guest identity

Form:

```text
chasm exec NAME [--cwd DIR] [--env K=V ...] -- COMMAND ARGS...
```

`NAME` follows the existing sandbox-name grammar. `--` is mandatory and ends Chasm option parsing. `COMMAND` must be non-empty; argv is transmitted as an argument vector and is never joined, quoted, or evaluated by a host shell. Default cwd is the guest account home (`/home/agent`). An explicit cwd is a guest absolute path; no host path is mounted or resolved by this option. Repeated `--env K=V` entries are passed as guest environment entries; reject empty/invalid keys, NUL, and duplicate keys rather than relying on order. Values may contain `=` and empty values. Do not inherit host environment variables.

Execution is always as the site's configured `guest_uid`/`guest_gid` (the same unprivileged identity as `chasm shell`). No guest-root mode is provided. Reject unknown, absent, deleted, or non-started targets with a clear control-plane result; this command does not start a guest. Validate the site host identity pin and sandbox ownership before dispatch. The privileged helper sends a fixed local Incus REST request with the guest UID/GID, working directory, environment, and argv. It must not accept an arbitrary Incus instance name from the caller; derive the instance name only after validating `NAME` against the site catalog and prefix rules.

### Privilege boundary

`chasm exec` uses a new exact sudoers entry to an internal `chasm __exec` entry point, adjacent to `__shell`; it does not put foreground execution on the serial supervisor request loop. The grant names only the installed Chasm executable and internal command form. The helper requires effective root, validates the invoking operator via sudo metadata, checks the site identity pin, validates the one name and all framing/size limits, confirms the target exists and is running, and executes a fixed Incus client path/endpoint. It may not run `sh -c`, evaluate a command string, accept arbitrary Incus flags, or expose host paths/environment. Existing shell grant and semantics remain unchanged.

The child is launched with stdin connected separately from stdout and stderr, and these streams are copied byte-for-byte without combining stdout/stderr or writing control metadata into either. Stderr from the guest remains guest stderr. The helper must bound argv count, individual argument and aggregate encoded request sizes, environment count/bytes, and simultaneous in-flight execs per operator/site; limits are constants documented in help. Resource exhaustion must fail closed before dispatch. This command is synchronous: no daemon-owned job, durable session, or supervisor worker is created.

### Output, status, cancellation, and disconnect

The privileged execution protocol returns a structured result containing transport/disposition (`completed`, `rejected`, `interrupted`, or `outcome_unknown`), guest exit status when completed, signal termination when applicable, and phase timings. A completed guest status is preserved exactly in this structured result and audit record. Control/transport errors are separately typed and never fabricated into guest status. Use a small stdlib-only Incus local REST/WebSocket client in the root helper for exec and control; do not infer remote command status from `incus exec` CLI's own exit status, which conflates guest and transport failures. Bound and validate frames, and pin operations to the local Incus socket.

The CLI process itself has an 8-bit POSIX exit code, so all 256 guest values cannot be represented disjointly alongside control failures. Preserve confirmed guest exit codes exactly. Incus 6.0 can synthesize return code 129 on VM disconnect, so that return is reported as an unknown outcome with `incus_return: 129` and no `guest_status`. Reserve conventional helper outcomes 124 (completion unknown), 125 (transport/admission failure), and 126 (execution setup failure) for the CLI process; these can collide with the same guest statuses, so the caller must use a separate structured status channel when it needs certainty. Offer a non-privileged-client-owned `--status-fd FD` channel carrying one bounded JSON result frame; guest output streams remain byte-exact and untouched. Never silently remap guest status. Document collisions and make the status frame authoritative. Numeric codes may be finalized during implementation, but guest statuses are never remapped.

On interrupt/cancel, send Incus exec control JSON (`command=signal`, signal number) for TERM, wait a bounded grace period, then send KILL; report the signal and whether completion was observed. On client disconnect, apply the same cleanup. If the guest completion status cannot be proven after cancellation/disconnect, return `outcome_unknown`, never claim the command failed or succeeded. No automatic retry is allowed. The audit record carries an operation/request id so a later diagnostic can correlate events, but no detached job or queryable long-running session is promised. Incus 6.0 forwards these signals to the primary guest process and does not return independently verified exit-by-signal metadata. The in-memory transport tests exercise escalation. Supported-host qualification still needs a live guest. If reliable termination/reap is unavailable, reject the operation rather than leave an unbounded orphan.

Do not hold the supervisor's global serialization/admission lock while a guest command runs. Execute on the narrow sudo path, outside the serial supervisor loop, so lifecycle commands and other guests remain serviceable. Resource limits cap concurrency. Lifecycle races are handled by Incus/target state checks and surfaced as a typed control result; the command does not acquire a site-wide lock for the duration of arbitrary guest work.

### Audit and privacy

Every attempt that crosses the privileged dispatch boundary is audited. Record request id, operator uid, sandbox name, disposition, guest UID/GID, cwd, environment key names, argv0, argument count, and SHA-256 over the full length-prefixed argv. Record any confirmed guest status, signal sent, cancellation disposition, and phase timings. The Chasm audit excludes argument tails, environment values, and all stream payloads. Incus retains its own operation metadata. The argv digest is for correlation, not secrecy: low-entropy commands can be guessed. Do not expose it as a substitute for redaction. Audit persistence failure fails closed from the caller's perspective and must clearly acknowledge that dispatch may already have happened, matching the existing executor audit caveat.

### Timing observations

Capture monotonic phase durations for validation, Incus endpoint/target resolution, process spawn, time to first output (separately for stdout/stderr if practical), execution, termination/reap, and total wall time. Include these in audit and structured observation output; do not infer guest runtime by subtracting unrelated wall clocks. Also capture lifecycle CREATE/START/STOP phases, including clone, boot, guest-agent readiness, shutdown request, stopped-state confirmation, and total duration; retain them with existing performance observations and expose them in `show` human and JSON output. Timing must not include or store output content. `doctor` is read-only and is not a timing mechanism.

## Failure classes

Keep failure origin explicit at each layer: CLI syntax/validation; denied sudo or wrong operator; host identity mismatch; invalid site config; absent/invalid sandbox; guest not running; unavailable Incus daemon/socket; Incus rejected operation; spawn/pipe failure; stream I/O failure; cancellation with known result; cancellation/disconnect with unknown result; guest exit/signal; audit persistence failure. Human errors name the layer and remedy where safe. Machine results carry stable codes and a `guest_status` only when completion was observed. Never turn an Incus/transport error into an apparent guest exit code or retry a possibly dispatched command.

## Security properties and non-goals

- Keep the existing one trusted host operator and site identity pin.
- Guest execution stays unprivileged and fixed to configured UID/GID.
- No shell interpretation; no arbitrary Incus CLI, host environment, host filesystem, or root grant.
- No auto-start, auto-repair, package install, public network/API, remote host, guest-root, detached command, or automatic retry.
- Bound all untrusted request data, concurrent commands, buffers, and teardown wait.
- Preserve separate streams and do not leak guest output to audit.
- Treat disconnect as cancellation and uncertain completion as unknown.
- Keep host/distro preflight honest: capability pass does not assert distro qualification.

## Verification plan for implementation

The implementation must add deterministic tests for: distro capability checks versus Debian-only package install; Incus socket precedence, explicit override failure, fixed service environment and local endpoint; Go version comparison; every virtiofsd candidate; hostname without the `hostname` binary; `incus-agent` missing/present diagnostics; migration and repeat-upgrade from old executable paths; SELinux enforcing/permissive/absent behavior and label drift; doctor JSON schema, read-only behavior, and unknown states; exec argv metacharacters/empty arguments/UTF-8/NUL rejection; cwd and env validation; guest identity and non-root enforcement; separate stdin/stdout/stderr; all guest statuses including 255 and signal termination; transport failures and their 255 collision; termination escalation, disconnect, unknown outcome, audit redaction/hash/timing, audit failure after dispatch, output backpressure, bounded concurrency, and lifecycle activity during a long command.

Qualification must include repository-native Go and shell checks, installer verification, and real disposable Incus trials on Debian 13, Ubuntu 24.04, Fedora 45 with SELinux enforcing, and Arch. Explicitly test upgrade from both older payload layouts (`/usr/local/lib/chasm/go/` and `/usr/local/libexec/chasm/`) and rerun provision/verify. This design does not claim these checks have been run.

## Open questions for implementation review

1. Which supported Incus versions provide dependable termination after a disconnected local client and unambiguous guest completion evidence?
2. Which enforcing SELinux policies supply the expected executable labels at the installed path?
3. Which distro and release versions should enter the formal qualification matrix after the installer, upgrade, and runtime trials?

