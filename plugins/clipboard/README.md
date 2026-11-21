# clipboard plugin (v1)

Standalone desktop clipboard helper (`chasm-clipboard`) for `chasm shell`.
See `docs/plugins.md` for the plugin contract; core builds, installs, and
runs without this directory, and no clipboard code is linked into the core
`chasm` binary.

## Interface

- Shape: standalone user command (v1). Nested Go module
  (`github.com/rahulrajaram/chasm/plugins/clipboard`), Go standard library
  only — no third-party dependencies.
- Invocation (explicit, opt-in — the only supported form):
  `chasm-clipboard -- chasm shell NAME`
  This filters OSC 52 sequences out of the terminal stream and bridges
  application text copies to the local desktop through `xclip` (X11) or
  `wl-copy` (Wayland), when installed. Paste uses the terminal shortcut.
- For remote shells run `chasm-clipboard -- ssh -t HOST chasm shell NAME` on
  your desktop machine.

## Prerequisites and effects

- Prerequisite: a Go toolchain to build; `xclip` or `wl-copy` on the
  machine where the helper runs. Nothing is auto-installed: if neither
  helper is present the helper reports the gap on stderr and exits 1
  without running the wrapped command — run the bare `chasm shell NAME`
  directly for a plain, unbridged session.
- Effects: none on the site or the host control plane. The helper runs
  outside sudo and the sandbox as the invoking user.

## Build / install / check

- Build: `cd plugins/clipboard && go build ./cmd/chasm-clipboard`
- Test: `cd plugins/clipboard && go test ./... && go vet ./...`
- Install (optional): put the built binary on your PATH, e.g.
  `go build -o ~/.local/bin/chasm-clipboard ./cmd/chasm-clipboard`.

## Upgrade behavior

A `chasm-clipboard` binary installed by an earlier core release keeps
working: core no longer builds, installs, links, or verifies it, and an
upgrade leaves any existing helper in place. The behavior change is the
invocation: clipboard bridging is no longer automatic for `chasm shell`;
wrap it with chasm-clipboard explicitly as above.

## Failure behavior

The helper fails closed on unsafe terminal states and reports desktop
bridge unavailability on stderr without suppressing the wrapped command's
exit status. It never touches the supervisor, executor, or guest.
