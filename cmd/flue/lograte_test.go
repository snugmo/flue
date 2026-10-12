package main

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// captureHandler records log records for assertions.
type captureHandler struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (h *captureHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.recs = append(h.recs, r)
	return nil
}
func (h *captureHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(_ string) slog.Handler      { return h }

// snapshot returns a copy of the records so far.
func (h *captureHandler) snapshot() []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]slog.Record, len(h.recs))
	copy(out, h.recs)
	return out
}

// attrInt returns the value of a record's integer attribute.
func attrInt(r slog.Record, key string) (int64, bool) {
	var got int64
	found := false
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			got = a.Value.Int64()
			found = true
			return false
		}
		return true
	})
	return got, found
}

// waitRecords polls until the handler has at least n records.
func waitRecords(t *testing.T, h *captureHandler, n int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for len(h.snapshot()) < n {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d records (have %d)", n, len(h.snapshot()))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestRefusalLimiter verifies the refusal rate-limit: the first
// refusal of a burst is logged in full, the rest are summarized once
// per window, a quiet gap starts a fresh in-full first, and flush
// reports whatever is left when the session ends.
func TestRefusalLimiter(t *testing.T) {
	oldWindow := refusalWindow
	refusalWindow = 100 * time.Millisecond
	defer func() { refusalWindow = oldWindow }()

	oldLog := log
	h := &captureHandler{}
	log = slog.New(h)
	defer func() { log = oldLog }()

	r := &refusalLimiter{}

	// A burst: first in full, the rest counted (no records yet).
	r.reject(slog.LevelWarn, "first", "k", 1)
	r.reject(slog.LevelWarn, "second")
	r.reject(slog.LevelWarn, "third")
	recs := h.snapshot()
	if len(recs) != 1 {
		t.Fatalf("after a burst of 3: %d records, want 1 (the first, in full)", len(recs))
	}
	if recs[0].Message != "first" || recs[0].Level != slog.LevelWarn {
		t.Fatalf("first record = %q at %v, want %q at warn", recs[0].Message, recs[0].Level, "first")
	}
	if v, ok := attrInt(recs[0], "k"); !ok || v != 1 {
		t.Fatalf("first record attr k = %d, %v; want 1, true", v, ok)
	}

	// The window ends: a summary of the 2 suppressed refusals.
	waitRecords(t, h, 2, 2*time.Second)
	recs = h.snapshot()
	if recs[1].Message != "flue: client refusals" {
		t.Fatalf("summary message = %q, want %q", recs[1].Message, "flue: client refusals")
	}
	if v, ok := attrInt(recs[1], "more"); !ok || v != 2 {
		t.Fatalf("summary attr more = %d, %v; want 2, true", v, ok)
	}

	// A quiet gap, then a refusal: it is the first of a fresh burst
	// and is logged in full again (no summary for the quiet window).
	time.Sleep(refusalWindow + 20*time.Millisecond)
	r.reject(slog.LevelWarn, "fresh")
	waitRecords(t, h, 3, 2*time.Second)
	recs = h.snapshot()
	if recs[2].Message != "fresh" {
		t.Fatalf("post-gap record = %q, want %q", recs[2].Message, "fresh")
	}

	// Session ending: flush reports the leftover suppression.
	r.reject(slog.LevelWarn, "leftover")
	r.flush()
	waitRecords(t, h, 4, 2*time.Second)
	recs = h.snapshot()
	if recs[3].Message != "flue: client refusals (session ending)" {
		t.Fatalf("flush message = %q, want %q", recs[3].Message, "flue: client refusals (session ending)")
	}
	if v, ok := attrInt(recs[3], "more"); !ok || v != 1 {
		t.Fatalf("flush attr more = %d, %v; want 1, true", v, ok)
	}

	// Nothing left: a second flush is silent.
	before := len(h.snapshot())
	r.flush()
	if after := len(h.snapshot()); after != before {
		t.Fatalf("second flush logged %d records, want 0", after-before)
	}
}
