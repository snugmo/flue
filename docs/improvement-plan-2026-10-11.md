# Improvement plan — design review 2026-10-11

Plan for addressing the Claude Opus 5.5 review in
[design-review-2026-10-11.md](design-review-2026-10-11.md) (added in
81d9c05). **All twelve tasks are fixed.** The per-task detail (code
change, tests, docs) is in the git history of this file (81d9c05) and in
the fixing commits below; the outcomes are recorded as items 19–28 of
[docs/code-quality.md](code-quality.md).

| Task | Fixing commit |
| ---- | ------------- |
| 1 — Host: validate the listener fd the container sends | 797b945 |
| 2 — Host: certificate fingerprints must match `ssh-add -l` | 8995e22 |
| 3 — Docs: fix the stale standalone-mode socket examples | dfcfaa3 |
| 4 — Memfd: fix the fallback and the kernel/MDWE comments | a7d4912 |
| 5 — Broker: set the process name | 5696911 |
| 6 — README: state the actual security guarantee | 78d5e5c |
| 7 — Host: split the shared policy out of `agentFilter` | f14e8a6 |
| 8 — Broker: check that XDG_RUNTIME_DIR is writable | 35c3d6c |
| 9 — Session check, part 1: learn the broker's session | 8d62851 |
| 10 — Session check, part 2: verify each client's session | f8c6203 |
| 11 — Comment hygiene: shrink the drift surface | 3e7e064 |
| 12 — Close-out: record outcomes | 6aae5fa |
