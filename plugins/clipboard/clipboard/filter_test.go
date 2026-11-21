package clipboard

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"strings"
	"testing"
)

type byteReader struct{ io.Reader }

func (r byteReader) Read(p []byte) (int, error) { return r.Reader.Read(p[:1]) }

func TestFilterClipboardAndTerminalBytes(t *testing.T) {
	for _, terminator := range []string{"\a", "\x1b\\"} {
		for _, singleByte := range []bool{false, true} {
			text := "hello\n世界"
			sequence := "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(text)) + terminator
			before, after := "\x1b[31mred\x1b[0m\x1b]0;title\a", "\x1b[?2004h\r\nend"
			var source io.Reader = strings.NewReader(before + sequence + after)
			if singleByte {
				source = byteReader{source}
			}
			var output bytes.Buffer
			calls := 0
			err := Filter(&output, source, func(selection string, data []byte) error {
				calls++
				if selection != "c" || string(data) != text {
					t.Fatalf("copy = %q %q", selection, data)
				}
				return nil
			})
			if err != nil || calls != 1 || output.String() != before+after {
				t.Fatalf("calls=%d output=%q err=%v", calls, output.String(), err)
			}
		}
	}
}

func TestFilterPreservesUnsupportedMalformedAndPartial(t *testing.T) {
	cases := []string{
		"ordinary\x1b[200~paste\x1b[201~",
		"\x1b]52;c;?\a", "\x1b]52;c;!!!\a", "\x1b]52;cp;aGk=\a",
		"\x1b]52;c;/w==\a", "\x1b]52;c;AA==\a", "\x1b]52;c;unfinished",
		"\x1b", "\x1b]52;c;" + strings.Repeat("A", maxSequence) + "\a",
	}
	for _, input := range cases {
		var out bytes.Buffer
		err := Filter(&out, strings.NewReader(input), func(string, []byte) error { t.Fatal("unexpected clipboard call"); return nil })
		if err != nil || out.String() != input {
			t.Fatalf("output changed (input length %d), err=%v", len(input), err)
		}
	}
}

func TestFilterFailedWriteFallsBack(t *testing.T) {
	input := "before\x1b]52;c;aGk=\aafter"
	var out bytes.Buffer
	err := Filter(&out, byteReader{strings.NewReader(input)}, func(string, []byte) error { return errors.New("no desktop") })
	if err != nil || out.String() != input {
		t.Fatalf("output=%q err=%v", out.String(), err)
	}
}

func TestFilterEmptyPrimaryAndAdjacentEscape(t *testing.T) {
	var out bytes.Buffer
	err := Filter(&out, byteReader{strings.NewReader("\x1b\x1b]52;p;\a")}, func(selection string, data []byte) error {
		if selection != "p" || len(data) != 0 {
			t.Fatalf("copy=%q %q", selection, data)
		}
		return nil
	})
	if err != nil || out.String() != "\x1b" {
		t.Fatalf("output=%q err=%v", out.String(), err)
	}
}

func FuzzFilterPreservesOutputWithoutClipboardWrites(f *testing.F) {
	f.Add([]byte("hello\x1b[32mworld\x1b[0m"))
	f.Add([]byte("\x1b]52;c;aGk=\a"))
	f.Fuzz(func(t *testing.T, input []byte) {
		var out bytes.Buffer
		if err := Filter(&out, bytes.NewReader(input), func(string, []byte) error { return errors.New("disabled") }); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(input, out.Bytes()) {
			t.Fatal("output changed on fallback")
		}
	})
}
