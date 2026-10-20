# The embedded flue-broker

`flue` embeds the `flue-broker` built at the same time (`go:embed`, stored
verbatim — see [flue.md](flue.md#embedded-broker)) and its cosign-signed
sigstore bundle. The embedded broker moves in two directions, and this doc
covers both:

- **Into the container** — how `flue enter` runs the embedded broker in
  memory by default (injection). This is the main flow; you never touch a
  broker file for it.
- **Out of the binary** — the `flue broker` sub-subcommands, which exist
  for the person who wants to inspect the bundled binary or do their own
  thing (install it into a container, verify a copy). Not part of the
  usual flow.

## How `flue enter` injects the broker

By default (`--inject-broker`, on unless disabled) `flue enter` does not
run the image's `flue-broker` at all. It runs the *embedded* one, entirely
in memory:

1. `flue enter` builds and runs
   `podman exec [-ti] --preserve-fds 2 <container> /proc/self/fd/4
   [command…]`. Two fds are preserved: the control socketpair end on fd 3
   (the usual one) and the broker's memfd on fd 4.
2. Before starting that child, `injectBrokerMemfd`
   (`cmd/flue/inject.go`) prepares the memfd:
   - It first checks the embedded blob's ELF magic (`looksLikeELF`) — a
     misbuilt embed source fails here, at `flue` startup, not later inside
     a container.
   - It creates a memfd named `flue-broker` with
     `MFD_EXEC | MFD_CLOEXEC | MFD_ALLOW_SEALING`
     (`unix.MemfdCreate`).
   - It writes the embedded broker's bytes into it, then seals it with
     `F_SEAL_SEAL | F_SEAL_SHRINK | F_SEAL_GROW | F_SEAL_WRITE`: the
     container side receives the fd writable, so without seals any process
     in the container that could reach it (via `/proc/<pid>/fd`) would
     have been able to rewrite the running broker's bytes. Sealing makes
     that impossible and forbids further seal changes.
3. The memfd is handed to the child via `exec.Command.ExtraFiles` as fd 4;
   `MFD_CLOEXEC` (belt-and-braces) drops it again across the child's exec.
4. Inside the container, `execve("/proc/self/fd/4")` makes the kernel map
   the memfd's bytes as an executable file. The broker never touches the
   container's filesystem, so injection works in a container with a
   read-only root filesystem and no `flue-broker` in its image at all. In
   `/proc/self/fd` the broker's own fd shows as
   `memfd:flue-broker (deleted)`; `ps` and `/proc/<pid>/comm` show
   `flue-broker` because the broker sets its process name
   (`PR_SET_NAME`).

### Kernel and sysctl fallbacks

`MFD_EXEC` (Linux ≥ 6.3) opts the memfd into execution against the
`vm.memfd_noexec` sysctl, which otherwise refuses to exec memfd-backed
files. `memfdCreateWithFallback` distinguishes the failure directions:

- `EINVAL` — the kernel predates `MFD_EXEC` and does not know the flag:
  retry with a plain memfd (`MFD_CLOEXEC | MFD_ALLOW_SEALING`). Kernels
  before 6.3 have no `vm.memfd_noexec` at all, so a plain memfd is still
  executable there.
- Anything else — in particular `EACCES` from a system-level refusal such
  as `vm.memfd_noexec=2` — is a hard error, and the message points at the
  two remedies: run with `--no-inject-broker`, or change the sysctl.

### Disabling injection, and the session check

Injection is disabled with `--no-inject-broker`,
`--inject-broker=false`, or `FLUE_INJECT_BROKER=0` (a set flag wins over
the variable). Then `flue enter` runs the image's `flue-broker` instead
(`--preserve-fds 1`, no memfd), so the container must have one.

How the broker was started also decides the **session check** mode (see
[flue.md](flue.md#session-check) for the check itself):

- An *injected* broker is trusted host-side code, so the check stays on:
  **required** for an interactive `flue enter` (tty — if it cannot be
  enabled the session is aborted rather than served without it) and
  **advisory** for an injected broker without a tty.
- `--no-inject-broker` (or the `flue exec` passthrough) starts the broker
  from the image — untrusted code that could hand over a listener from any
  session it controls — so the check is **off** and a warning is logged.

## Getting the broker out: the `flue broker` subcommands

The `broker` group is declared in `cmd/flue/broker.go`. None of its
sub-subcommands needs an agent socket (they are not annotated with
`needsAgentAnnotation` in the root `PersistentPreRun`), and none takes
flags. The embedded bytes are stored verbatim, so everything below is
100% identical to what was built — including the broker that `flue enter`
injects.

### `flue broker executable <filename>`

Writes the embedded `flue-broker` to `<filename>`, mode 0755,
byte-identical to the built broker (`cmd/flue/extract.go`). It checks the
ELF magic before writing and writes atomically — a temp file in the
target directory, `chmod`, rename — so the target is never left
half-written; a directory as the target is refused. The canonical use is
installing a broker into a container when injection is disabled:

```sh
flue broker executable /tmp/flue-broker
podman cp /tmp/flue-broker <container>:/usr/local/bin/flue-broker
```

But it doubles as inspection: the file is a plain static ELF, so
`file`, `sha256sum`, `objdump`, … all work on it without pulling an image
or building the broker separately.

### `flue broker sha256`

Prints the SHA256 of the embedded broker as a bare hex digest — the value
`sha256sum` of the file `executable` writes — so it can be compared
against another source without writing the binary out:

```sh
$ flue broker sha256
8ea5d136dd59…
$ sha256sum /usr/local/bin/flue-broker   # from a container or a separate build
8ea5d136dd59…                            # a match means the same broker
```

### `flue broker sigstore-bundle [filename]`

Writes the embedded broker's sigstore bundle out: to `<filename>`,
mode 0644, when a filename is given (`cmd/flue/bundle.go`, the same
atomic write as `executable`), and to **stdout** when none is — or when
it is `-` — so the bundle can be piped or redirected (`flue broker
sigstore-bundle - > flue-broker.sigstore.json`). It is the other half of the
pair below: together, the two files let anyone verify a recovered broker
offline.

One caveat: a `flue` built without a signed broker (a bare local build)
carries the committed placeholder template instead of a real bundle.
`broker sigstore-bundle` detects it by its JSON `kind` field
(`FlueLocalPlaceholder`) and logs a warning that the emitted bundle
cannot verify the extracted broker — the output still happens (file or
stdout), so the command stays scriptable, and in the stdout form the
warning goes to stderr so the bundle itself stays pipe-clean. A
placeholder carries no signature, so cosign cannot verify it; it exists
so a bare build is labeled as unverifiable rather than mistaken for a
signed one.

## Verifying a recovered broker

The pair is the offline verification recipe:

```sh
flue broker executable /tmp/flue-broker
flue broker sigstore-bundle - > /tmp/flue-broker.sigstore.json   # or: flue broker sigstore-bundle /tmp/flue-broker.sigstore.json
cosign verify-blob /tmp/flue-broker --bundle /tmp/flue-broker.sigstore.json
```

The bundle is self-contained (signature, certificate, chain), so the
verify needs no registry or network lookup. For a release `flue`, the
release workflow signed the broker with `cosign sign-blob --bundle` over
that very file (Sigstore keyless, GitHub Actions OIDC), so pin the
certificate's identity with the same `--certificate-identity` /
`--certificate-oidc-issuer` pair as the release signature — see the
[Release image](../README.md#release-image) section for the exact values.
A broker recovered from any release `flue`, any architecture, can also be
verified against the GHCR attestations with `gh attestation verify`.

Two properties follow from the embed: the recovered broker matches the
`flue` binary's architecture (one source tree, one build — the
Containerfile builds the broker for the image's platform before that
platform's `flue`), and it is exactly the binary `flue enter` would
inject — same bytes, just written to disk instead of a memfd.
