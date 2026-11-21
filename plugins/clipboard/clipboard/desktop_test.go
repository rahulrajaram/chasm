package clipboard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDesktopSelectionAndEnvironment(t *testing.T) {
	for _, test := range []struct {
		name         string
		env          map[string]string
		helper, args string
	}{
		{"x11", map[string]string{"DISPLAY": ":test"}, "xclip", "-selection clipboard -in"},
		{"wayland", map[string]string{"DISPLAY": ":test", "WAYLAND_DISPLAY": "wayland-test"}, "wl-copy", "--type text/plain;charset=utf-8"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "helper")
			body := "#!/bin/sh\nprintf '%s' \"$*\" > \"$CAPTURE_ARGS\"\ncat > \"$CAPTURE_TEXT\"\n"
			if err := os.WriteFile(path, []byte(body), 0700); err != nil {
				t.Fatal(err)
			}
			argsPath, textPath := filepath.Join(dir, "args"), filepath.Join(dir, "text")
			copyText, err := Desktop(func(key string) string { return test.env[key] }, func(name string) (string, error) {
				if name != test.helper {
					t.Fatalf("helper=%s", name)
				}
				return path, nil
			}, []string{"PATH=/usr/bin:/bin", "CAPTURE_ARGS=" + argsPath, "CAPTURE_TEXT=" + textPath})
			if err != nil {
				t.Fatal(err)
			}
			if err := copyText("c", []byte("text\nwith newline\n")); err != nil {
				t.Fatal(err)
			}
			args, _ := os.ReadFile(argsPath)
			text, _ := os.ReadFile(textPath)
			if string(args) != test.args || string(text) != "text\nwith newline\n" {
				t.Fatalf("args=%q text=%q", args, text)
			}
			if err := copyText("p", []byte("primary")); err != nil {
				t.Fatal(err)
			}
			args, _ = os.ReadFile(argsPath)
			if !strings.Contains(string(args), "primary") {
				t.Fatalf("primary args=%q", args)
			}
		})
	}
}

func TestDesktopRejectsServerAndHeadless(t *testing.T) {
	for _, env := range []map[string]string{{}, {"DISPLAY": ":0", "SSH_CONNECTION": "remote"}, {"DISPLAY": ":0", "SSH_TTY": "/dev/pts/1"}} {
		_, err := Desktop(func(key string) string { return env[key] }, func(string) (string, error) { t.Fatal("must not discover helper"); return "", nil }, nil)
		if err == nil {
			t.Fatal("expected unavailable clipboard")
		}
	}
}
