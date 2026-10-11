# Deliberate duplication

The code below is **intentionally duplicated rather than shared**, to
keep each binary a self-contained static main (the broker is
"deliberately thin" — see its header). Do not "fix" any of it into a
shared `internal/` package:

- **Between the two binaries**: `parseLevel`, `fatalf`, `closeQuiet`, and
  the `log` variable plus its re-initialization from the log-level flag
  (the root `PersistentPreRun` in flue, `main` in flue-broker)
  are duplicated in both. They are small and stable; a shared package
  would need to parameterize the program-name log prefixes and fatalf
  semantics for ~25 saved lines. **If one changes, apply the paired edit
  to the other** (the slog switch and the fatalf format fix both did
  this).
- **Within `cmd/flue`**: `closeQuiet` (net.Conn) and `closeListener`
  (net.Listener) are near-identical but stay separate per-type helpers; a
  unified helper would need an interface and name plumbing that hurts
  readability.
- **The `agentFilter` deny methods** (`Add` / `Remove` / `RemoveAll` /
  `Lock` / `Unlock` / `Signers`) look similar but each implements a
  distinct protocol operation with a distinct error message, matching the
  per-operation policy table in the README. Keep them explicit — do not
  collapse them into a table-driven helper.
- **Test helpers**: `makeControlPair` (flue) and
  `newControlSocketpair` (broker) are per-package; a shared testutil
  package for a ~15-line helper is not worth it.
