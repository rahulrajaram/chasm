// Package clipboard bridges application clipboard writes to the desktop that
// owns a terminal. Ordinary paste stays on the terminal's existing input path.
package clipboard

import (
	"bytes"
	"encoding/base64"
	"io"
	"strings"
	"unicode/utf8"
)

const maxSequence = 1 << 20

// CopyText receives an OSC 52 selection (c or p) and UTF-8 text. It must return
// an error if it cannot update the desktop; failed requests pass to the terminal.
type CopyText func(selection string, text []byte) error

// Filter preserves terminal output except successfully handled clipboard writes.
// It buffers only a possible OSC 52 sequence, across arbitrary read boundaries.
// Reads/queries are never serviced: host text enters through a user paste action.
func Filter(dst io.Writer, src io.Reader, copyText CopyText) error {
	const prefix = "\x1b]52;"
	pending := make([]byte, 0, 128)
	buffer := make([]byte, 32*1024)
	for {
		n, readErr := src.Read(buffer)
		output := make([]byte, 0, n)
		for _, b := range buffer[:n] {
			if len(pending) == 0 && b != '\x1b' {
				output = append(output, b)
				continue
			}
			pending = append(pending, b)
			if len(pending) <= len(prefix) && !bytes.HasPrefix([]byte(prefix), pending) {
				// Retain a trailing ESC: it may begin the next sequence.
				keep := 0
				if b == '\x1b' {
					keep = 1
				}
				output = append(output, pending[:len(pending)-keep]...)
				pending = append(pending[:0], pending[len(pending)-keep:]...)
				continue
			}
			if len(pending) <= len(prefix) {
				continue
			}
			end := 0
			if b == '\a' {
				end = 1
			}
			if bytes.HasSuffix(pending, []byte("\x1b\\")) {
				end = 2
			}
			if end != 0 {
				selection, text, ok := decodeWrite(pending[len(prefix) : len(pending)-end])
				if !ok || copyText(selection, text) != nil {
					output = append(output, pending...)
				}
				pending = pending[:0]
			} else if len(pending) >= maxSequence {
				output = append(output, pending...)
				pending = pending[:0]
			}
		}
		if _, err := dst.Write(output); err != nil {
			return err
		}
		if readErr != nil {
			if _, err := dst.Write(pending); err != nil {
				return err
			}
			if readErr == io.EOF {
				return nil
			}
			return readErr
		}
	}
}

func decodeWrite(payload []byte) (string, []byte, bool) {
	selection, encoded, found := strings.Cut(string(payload), ";")
	if !found {
		return "", nil, false
	}
	if selection == "" {
		selection = "c"
	}
	if selection != "c" && selection != "p" {
		return "", nil, false
	}
	text, err := base64.StdEncoding.Strict().DecodeString(encoded)
	return selection, text, err == nil && utf8.Valid(text) && !bytes.ContainsRune(text, 0)
}
