package main

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// refusalWindow is how often a burst of refused client connections is
// summarized. It is a variable so tests can shorten it.
var refusalWindow = 30 * time.Second

// refusalLimiter rate-limits the per-client refusal warnings of the
// session check. flue's stderr is the user's tty, and any process in
// the container can connect in a loop to fill it with "refusing"
// lines; so the first refusal of a burst is logged in full, the rest
// are counted and summarized once per refusalWindow ("N more in the
// last 30 s"), and whatever is left over is flushed when the session
// ends. Each window therefore starts with one in-full refusal and
// ends with a summary of the rest; a quiet gap longer than
// refusalWindow just ends the window (no summary is emitted) and the
// next refusal starts a fresh one.
type refusalLimiter struct {
	mu         sync.Mutex
	timer      *time.Timer // nil when no burst is in progress
	suppressed int         // refusals not yet summarized
}

// reject logs one refusal: in full for the first of a burst, counted
// otherwise. A nil limiter (a policy built without one) logs every
// refusal in full — the pre-rate-limit behavior.
func (r *refusalLimiter) reject(level slog.Level, msg string, kv ...any) {
	if r == nil {
		log.Log(context.Background(), level, msg, kv...)
		return
	}
	r.mu.Lock()
	if r.timer == nil && r.suppressed == 0 {
		r.timer = time.AfterFunc(refusalWindow, r.onWindow)
		r.mu.Unlock()
		log.Log(context.Background(), level, msg, kv...)
		return
	}
	r.suppressed++
	r.mu.Unlock()
}

// onWindow fires at the end of a burst window: it summarizes the
// suppressed refusals and starts a fresh window if the burst continues.
func (r *refusalLimiter) onWindow() {
	r.mu.Lock()
	n := r.suppressed
	r.suppressed = 0
	r.timer = nil
	r.mu.Unlock()
	if n == 0 {
		return
	}
	log.Warn("flue: client refusals", "more", n, "in the last", refusalWindow)
}

// flush reports the suppressions left over when the session ends (and
// stops the pending window): the last word on how many clients were
// refused. A nil limiter is a no-op.
func (r *refusalLimiter) flush() {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.timer != nil {
		r.timer.Stop()
		r.timer = nil
	}
	n := r.suppressed
	r.suppressed = 0
	r.mu.Unlock()
	if n == 0 {
		return
	}
	log.Warn("flue: client refusals (session ending)", "more", n)
}
