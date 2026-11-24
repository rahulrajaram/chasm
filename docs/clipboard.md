---
title: "Terminal clipboard"
permalink: /clipboard.html
---
# Terminal clipboard

`chasm shell` by itself is a plain terminal session: the core `chasm`
binary links no clipboard code. Application text copies are bridged to a
Linux desktop by the standalone `chasm-clipboard` helper, which wraps the
command explicitly and filters OSC 52 out of the terminal stream,
independent of the terminal emulator's OSC 52 support. It uses an already
installed `xclip` on X11 or `wl-copy` on Wayland. No packages are installed
automatically.

The desktop user owns the bridge, outside sudo and the sandbox. The helper
handles
OSC 52 text writes from terminal applications, passing other terminal output
through. The guest receives neither desktop display sockets nor credentials.
The bridge only exists for the shell session; the copied text can remain on the
desktop clipboard after the shell exits.

## Copy and paste

- An application's copy action and application-owned mouse selection can write
  to the desktop clipboard even in XFCE Terminal, which does not support OSC 52
  itself. In a fullscreen application UI, releasing a left-button drag selection
  typically triggers copy.
- Ctrl+Shift+C still copies the terminal's own selection. When an application
  captures mouse events, hold Shift while selecting to use terminal selection.
  For terminal-owned selections to copy on release too, enable the terminal's
  own copy-on-select preference (XFCE: `misc-copy-on-select`). Chasm cannot set
  a universal selection preference for all terminal emulators.
- Ctrl+Shift+V pastes through the terminal's normal input path. Bracketed paste
  and multiline text pass through without rewriting.
- X11's PRIMARY selection (normally pasted with the middle mouse button) is
  distinct from CLIPBOARD (normally pasted with Ctrl+Shift+V). The bridge honors
  explicit OSC 52 requests for either; applications normally write CLIPBOARD.

This is text transfer through the interactive terminal, not a synchronized
clipboard service inside the guest. Guest `xclip`, application-native
clipboard-read shortcuts, image paste, and graphical drag-and-drop do not gain
desktop access. Clipboard read queries are not serviced by the bridge. Copies
whose OSC 52 sequence exceeds 1 MiB (about 768 KiB of decoded text), invalid
UTF-8, unsupported selection targets, and failed desktop writes fall back to
the terminal's own handling. A failed desktop write emits a warning once per
session, without printing clipboard contents.

## Remote sessions

The clipboard owner is the computer running the desktop, not necessarily the
Incus host. Run the standalone helper there:

```sh
chasm-clipboard -- ssh -t server chasm shell development
```

The helper needs no Chasm site configuration. It runs on Linux and wraps any
interactive command; the server needs no display access. Ordinary SSH alone
still depends on the client terminal's clipboard support. A helper running on
the server cannot make an unsupported client terminal support clipboard writes.

An intervening tmux must be configured to forward application OSC 52 requests
(for example, `set -s set-clipboard on` with an `Ms` entry in the
terminal's terminfo description).
A multiplexer that discards requests before they reach the bridge prevents
application copy. Native terminal selection/paste remains independent of this.

## Testing and installation

The clipboard bridge is an optional plugin (`plugins/clipboard`; see
[plugins.md](plugins.md)), a nested Go module with no dependencies outside
the standard library. `chasm shell` alone is a plain terminal session; to
bridge copies, run the helper explicitly:

```sh
chasm-clipboard -- chasm shell NAME
```

Build and test the plugin from its own module:

```sh
cd plugins/clipboard
go vet ./... && go test ./...
go build -o ~/.local/bin/chasm-clipboard ./cmd/chasm-clipboard
```

Tests use private PTYs and fake clipboard helpers; they do not read or replace
your desktop clipboard. They cover terminal restoration, shell exit status,
initial terminal dimensions, bracketed paste, arbitrary output chunking,
malformed requests, and native helper selection. Live XFCE, Wayland, and SSH
testing remains a separate check after deployment.

The site installer no longer builds or installs `chasm-clipboard`; a helper
installed by an earlier release keeps working. For a client-only Linux
machine, build the plugin module and place the binary on its PATH; it does
not require installing the site services.
