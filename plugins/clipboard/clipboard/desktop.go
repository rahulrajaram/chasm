package clipboard

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"time"
)

// Desktop selects an already installed helper. Call before entering sudo, with
// the desktop user's environment; no display socket or credential enters a guest.
// An SSH server must not accidentally target its own desktop's clipboard.
func Desktop(getenv func(string) string, lookPath func(string) (string, error), environment []string) (CopyText, error) {
	if getenv("SSH_CONNECTION") != "" || getenv("SSH_TTY") != "" {
		return nil, errors.New("run chasm-clipboard on the desktop side of SSH")
	}
	name := ""
	switch {
	case getenv("WAYLAND_DISPLAY") != "":
		name = "wl-copy"
	case getenv("DISPLAY") != "":
		name = "xclip"
	default:
		return nil, errors.New("no desktop display is available")
	}
	path, err := lookPath(name)
	if err != nil {
		return nil, err
	}
	env := append([]string(nil), environment...)
	return func(selection string, text []byte) error {
		args := []string{"--type", "text/plain;charset=utf-8"}
		if name == "xclip" {
			target := "clipboard"
			if selection == "p" {
				target = "primary"
			}
			args = []string{"-selection", target, "-in"}
		} else if selection == "p" {
			args = append(args, "--primary")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, path, args...)
		cmd.WaitDelay = 100 * time.Millisecond
		cmd.Env = env
		cmd.Stdin = bytes.NewReader(text)
		// Do not capture stdout/stderr pipes: selection owners fork and may keep
		// them open for the entire lifetime of the clipboard contents.
		return cmd.Run()
	}, nil
}
