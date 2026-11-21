# ssh-hardening plugin (v1)

Opt-in host sshd hardening step for a chasm site. See `docs/plugins.md`
for the plugin contract; core installs, verifies, and runs without this
directory, and no flag means no sshd effect.

## Interface

- Shape: standalone host provisioning script (v1):
  `ssh-hardening.sh` plus the drop-ins in `drop-ins/`
  (`00-chasm.conf`, `99-chasm.conf`).
- Invocation: only through the installer's explicit
  `install-chasm.sh --provision --harden-ssh` dispatch. There is no other
  supported entry point and no automatic run.

## Prerequisites and effects

- Prerequisites: root, an sshd whose main config includes
  `/etc/ssh/sshd_config.d/*.conf`, and a working SSH public key for the
  operator (or console access). There is no key guard.
- Effects: installs the two drop-ins (disable SSH password and root
  login), validates the full configuration with `sshd -t`, and reloads the
  first active unit among `ssh.service`/`sshd.service`. No default access
  policy beyond these two drop-ins is edited.

## Safety semantics (unchanged from when this was a core step)

- OFF by default; the approval must be typed interactively;
  `CHASM_ASSUME_YES` does not cover it.
- The step warns plainly that it disables SSH password and root login.
- Source trust: the drop-ins are read only from this plugin's own
  `drop-ins/` directory (the dispatcher passes `SSH_DROPINS_DIR`; the
  dispatcher refuses a symlinked plugin or drop-in directory). They are
  installed with explicit `root:root 0644` modes.
- Signal-safe staging: ALL existing drop-ins are copied and verified
  before anything is touched; a trap on EXIT HUP INT TERM restores the
  previous files until the step succeeds; `sshd -t` failure restores and
  reloads nothing. Not protected against SIGKILL/power loss.

## Check / remove

- Check: `install-chasm.sh --verify` reports drift on installed chasm
  fragments when this plugin is present in the tree; it only notes that
  fragments were not examined when it is absent.
- Remove: delete `/etc/ssh/sshd_config.d/00-chasm.conf` and
  `99-chasm.conf`, then reload the active ssh unit (and restore any
  backup you kept of the previous configuration).

## Failure behavior

Any backup, validation, or reload failure restores the previous drop-ins
and aborts the provision run with the destination unchanged. A request for
`--harden-ssh` without this plugin in the tree fails with a clear message
naming the missing plugin directory.
