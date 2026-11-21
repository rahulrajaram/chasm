# Contributing to chasm

Thanks for helping improve chasm. This file is short on purpose: match
the existing code, keep the checks green, and do not ship secrets.

## Prerequisites

- Go 1.23 or newer (the pinned version lives in `go.mod`)
- [shellcheck](https://www.shellcheck.net/) for POSIX shell linting
- git, and a Linux environment if you want to exercise the full
  sandbox stack; the checks below run unprivileged anywhere Go does

## Running the checks

CI runs exactly these commands. Run them locally before opening a pull
request:

```sh
# Must print nothing:
gofmt -l cmd internal

go vet ./...
go build ./cmd/...
go test ./...

git ls-files '*.sh' 'host-control/chasm-firewall' \
  | xargs -r shellcheck --severity=warning

scripts/check-site-profiles.sh
scripts/check-descriptor-parity.sh
scripts/check-portability.sh
scripts/check-core-only.sh
scripts/check-ci-coverage.sh
scripts/test-install-modes.sh
scripts/test-firewall-helper.sh
scripts/test-audit-sandbox.sh
scripts/test-provision-rerun.sh
scripts/test-builder-address-claim.sh
scripts/test-guest-user-layer.sh
scripts/test-profile-root-selection.sh
scripts/test-core-package-union.sh
scripts/test-resource-prefix.sh

# Plugins (optional add-ons; see docs/plugins.md) are gated separately:
(cd plugins/clipboard && go vet ./... && go test ./...)
plugins/ssh-hardening/test-ssh-hardening.sh
plugins/install-incus/test-incus-install.sh
```

## Style

- Go code must be gofmt-clean and follow the conventions of the
  surrounding package.
- Shell must be POSIX `sh` (`#!/bin/sh`, no bashisms) in new or touched
  scripts and pass shellcheck at severity `warning`.
- Write code, comments, commit messages, and documentation in English.

## Idioms

Read [docs/idioms.md](docs/idioms.md) before changing how sandboxes are
sized, admitted, displayed, or changed. It lists the rules the code is
held to (one truth per resource, show the truth and flag drift, a
supported path for every operator need, commit only what the host can
back) and the check for each.

## Tests

Behavior changes come with tests. Go tests live next to the code they
cover; shell behavior that is product behavior gets a gate script under
`scripts/` so CI can run it unprivileged.

## Privileged code needs extra care

Anything that runs as root - the executor, privileged helpers, the
units in `host-control/`, and commands reachable through sudoers -
receives especially careful review. Keep the surface minimal, validate
every input at the boundary, and prefer refusing over guessing. Expect
this review to take longer; that is on purpose.

## Documentation accuracy

Documentation must describe what the tree actually does. When you
change behavior, update the affected page under `docs/` in the same
change, and never document a command, flag, path, or number you have
not verified against the source. The portability gate also scans the
tree: it first skips every tracked path under `docs/` and `testdata/`,
and the checker script itself (`scripts/check-portability.sh`); the
remaining scanned files are checked for machine-local absolute paths
under `/home` (a built-in rule) and for host names, private project
names, and similar site-specific facts, the latter only when you
supply them via the optional `CHASM_PORTABILITY_PATTERNS_FILE` pattern
file (see `scripts/check-portability.sh`). Excluded paths are not
scanned for secrets or any other pattern.

## Commits

- Use conventional commit subjects: `feat:`, `fix:`, `docs:`,
  `refactor:`, `test:`, `chore:`.
- Never commit secrets, credentials, or host-specific data (hostnames,
  IP addresses, personal paths). The gate's built-in check catches
  `/home` paths; other site-specific facts are caught only if you add
  patterns for them, so review your diffs yourself. History is hard to
  unwind.

## Security issues

Do not report security issues in public issues or pull requests; see
[SECURITY.md](SECURITY.md) for the private reporting channel.
