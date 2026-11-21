# plugins/

Optional, explicitly vended components. Core does not require this
directory: it builds, installs, verifies, and templates with `plugins/`
absent, and no core code path discovers, registers, or auto-loads anything
from here. The contract is documented in `docs/plugins.md`.

Every plugin is one subdirectory with:

- a README declaring the interface version (v1), prerequisites and effects,
  selection/install/check/remove semantics, and failure behavior;
- its own source, independently buildable and independently vendable;
- no mandatory place in any core payload tree, gate, or installer step.

Current plugins:

- `developer-tools/` — data-only guest package profile (v1) adding the
  optional developer tool set (curl, git, jq, python3/pip/venv, sqlite3,
  tmux, universal-ctags, vim) to a guest template.
- `clipboard/` — standalone `chasm-clipboard` helper (v1, nested Go module,
  standard library only). Opt-in invocation:
  `chasm-clipboard -- chasm shell NAME`.
- `ssh-hardening/` — opt-in host sshd hardening step (v1) with typed
  confirmation, source trust checks, and signal-safe rollback; dispatched
  only by the explicit `--harden-ssh` flag.
- `install-incus/` — Debian-specific Incus package acquisition (v1);
  dispatched only by the explicit `--install-incus` flag. Incus itself
  remains a core prerequisite: the plugin is a Debian convenience, not a
  supported-platform widening.

Rules for plugin authors:

- Guest profiles use the validated manifest grammar of
  `scripts/lib/require-site-profile.sh` and are data only — never sourced,
  never evaluated, digest-bound into template identity.
- Provisioning scripts own their approval, trust, and rollback semantics;
  they must fail closed and must not weaken core's confirmations or trust
  checks.
- Removing a plugin directory never breaks core; requesting a plugin-backed
  flag without the plugin fails clearly.
