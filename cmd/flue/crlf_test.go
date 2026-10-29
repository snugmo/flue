package main

import (
	"bytes"
	"log/slog"
	"testing"
)

// TestCrlfWriterNewlines covers the conversion itself: a bare \n gains a
// \r, an existing \r\n is left untouched, and data without newlines
// passes through verbatim.
func TestCrlfWriterNewlines(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"bare newline", "a\n", "a\r\n"},
		{"crlf stays", "a\r\n", "a\r\n"},
		{"no newline", "abc", "abc"},
		{"mixed", "x\ny\r\nz\n", "x\r\ny\r\nz\r\n"},
		{"multiple bare", "a\nb\nc", "a\r\nb\r\nc"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			w := &crlfWriter{w: &buf}
			if n, err := w.Write([]byte(tt.in)); err != nil {
				t.Fatalf("Write: %v", err)
			} else if n != len(tt.in) {
				t.Errorf("Write n = %d, want %d", n, len(tt.in))
			}
			if got := buf.String(); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// TestCrlfWriterSplitState covers a \r\n pair split across two Write
// calls: the second write must not insert a second \r, which is why the
// lastCR state is kept on the writer rather than per call.
func TestCrlfWriterSplitState(t *testing.T) {
	var buf bytes.Buffer
	w := &crlfWriter{w: &buf}
	if _, err := w.Write([]byte("a\r")); err != nil {
		t.Fatalf("Write 1: %v", err)
	}
	if _, err := w.Write([]byte("\nb")); err != nil {
		t.Fatalf("Write 2: %v", err)
	}
	if got := buf.String(); got != "a\r\nb" {
		t.Errorf("got %q, want %q", got, "a\r\nb")
	}
}

// TestCrlfWriterSlogLog exercises the writer through the same path the
// real logger uses: a slog TextHandler writes each record as one Write
// call ending in \n, so two consecutive records must come out as
// separate \r\n-terminated lines even when the terminal's ONLCR is off.
func TestCrlfWriterSlogLog(t *testing.T) {
	var buf bytes.Buffer
	l := slog.New(slog.NewTextHandler(&crlfWriter{w: &buf}, &slog.HandlerOptions{Level: slog.LevelInfo}))
	l.Info("flue: broker session", "pid", 1)
	l.Info("flue: session check on", "session", 2)
	out := buf.Bytes()
	// No record may start where the previous one ended: every line
	// break in the output must be \r\n, never a bare \n.
	for i, b := range out {
		if b == '\n' && (i == 0 || out[i-1] != '\r') {
			t.Fatalf("output contains a bare \\n: %q", out)
		}
	}
	if !bytes.HasSuffix(out, []byte("\r\n")) {
		t.Errorf("output does not end with \\r\\n: %q", out)
	}
}
