// chasm-clipboard runs on the desktop side of a terminal session, including SSH.
package main

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/rahulrajaram/chasm/plugins/clipboard/clipboard"
)

func main() {
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 || args[0] == "--help" {
		fmt.Fprintln(os.Stderr, "Usage: chasm-clipboard -- COMMAND [ARG...]\nRun on the desktop to bridge application text copies without OSC 52 terminal support.\nExample: chasm-clipboard -- ssh -t server chasm shell development\nPaste uses the terminal's normal paste shortcut. Requires xclip (X11) or wl-copy (Wayland).")
		if len(args) == 0 {
			os.Exit(2)
		}
		return
	}
	copyText, err := clipboard.Desktop(os.Getenv, exec.LookPath, os.Environ())
	if err != nil {
		fmt.Fprintln(os.Stderr, "chasm-clipboard:", err)
		os.Exit(1)
	}
	// Prevent a second bridge when the wrapped command is a local chasm shell.
	childEnvironment := append(os.Environ(), "CHASM_CLIPBOARD=off")
	code, err := clipboard.Session(args[0], args[1:], childEnvironment, os.Stdin, os.Stdout, os.Stderr, copyText)
	if err != nil {
		fmt.Fprintln(os.Stderr, "chasm-clipboard:", err)
	}
	os.Exit(code)
}
