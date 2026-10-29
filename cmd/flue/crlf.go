package main

import "io"

// crlfWriter terminates bare newlines as \r\n. It wraps the log output
// when that output goes straight to the user's terminal and the
// subcommand spawns a child that may take the terminal (enter, exec).
// Such a child (a podman exec that requests a tty) puts the shared
// terminal into raw mode, which disables OPOST and with it ONLCR: the
// terminal then no longer translates a bare \n into CR+NL on output.
// A log record flue writes itself while the child owns the terminal
// (the handover lines, a session-check refusal, the exit report) ends
// with a bare \n in that state: the cursor drops to the next row
// without returning to column zero, so the record that follows starts
// where the previous one ended (and overwrites it when shorter).
// Ending every line with \r\n instead renders correctly in raw mode,
// and is harmless in cooked mode, where ONLCR turns the \n into \r\n
// and the extra \r is a no-op at column zero. A \n already preceded by
// a \r is passed through unchanged, so data that carries its own
// \r\n line endings is never doubled up.
type crlfWriter struct {
	w      io.Writer
	lastCR bool // the last byte written to w was a '\r'
}

func (c *crlfWriter) Write(p []byte) (int, error) {
	var out []byte
	for _, b := range p {
		if b == '\n' && !c.lastCR {
			out = append(out, '\r')
		}
		out = append(out, b)
		c.lastCR = b == '\r'
	}
	if len(out) == 0 {
		return 0, nil
	}
	if _, err := c.w.Write(out); err != nil {
		return 0, err
	}
	// The consumer (slog) reads only the error; report the input
	// consumed, not the \r-augmented bytes written.
	return len(p), nil
}
