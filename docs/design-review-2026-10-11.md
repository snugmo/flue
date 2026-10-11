# Review of design and implementation 2026-10-11

Review performed by Claude Opus 5.5

Overall it's in good shape. The core is sound, the lifecycle handling is careful, and you made one design change that's better than what I proposed. I couldn't run the test suite here because module downloads are blocked in my environment, so the points below come from reading the code.

## The listener handoff is a good change

Instead of passing each client's fd to the host, the broker hands its *listening* socket to the host once, and the host `accept()`s on it directly. That's simpler than both variants we discussed. The broker never touches client traffic and isn't a moving part per connection. It also helps the session check later: `SO_PEERCRED` on a connection the host accepted gives you the client's pid in the host's pid namespace, and the check runs in the trusted process.

Things done well:
- the umask around `Listen`, so the socket is 0600 from creation
- `markCloexecAll` before starting the child
- sealing the memfd
- the client cap, idle timeout and handover timeout
- exit codes mapped as 128+signal
- goleak in the tests

## Things I'd fix

**1. Validate the fd the container sends.** `receiveListener` wraps whatever arrives with `net.FileListener`, and that accepts any listening socket, including TCP. A container on host networking could hand over a TCP listener bound to `0.0.0.0`, and flue would serve the agent on it. This is hardening rather than a new hole, since a compromised container could relay its own socket to the network anyway. It becomes essential once you add peer-credential checks, because a TCP peer has no credentials. Before wrapping the fd, check:
- `SO_DOMAIN` is `AF_UNIX`
- `SO_TYPE` is `SOCK_STREAM`
- `SO_ACCEPTCONN` is 1

Then type-assert the result to `*net.UnixListener`.

**2. Certificate fingerprints won't match `ssh-add -l`.** `List` hashes `k.Blob` and `Sign` hashes the key it receives. For a certificate, that's the whole certificate blob. `ssh-add -l` prints the fingerprint of the underlying plain key, so an `--allow-key` value copied from it never matches a cert. Parse the blob, and if it's an `*ssh.Certificate`, fingerprint `cert.Key`.

**3. Some documented commands no longer work.** Since the broker started generating a random socket path, standalone mode needs `-sock` pinned. These examples still set `SSH_AUTH_SOCK=/tmp/flue.sock` without `-sock /tmp/flue.sock`, so the client looks for a socket that doesn't exist:
- the AGENTS.md "Forward agent" row
- the Containerfile header
- the `flue exec` help text (`cmd/flue/main.go:208`)

AGENTS.md also still describes "Socket path coupling" with `enterSocket` and `TestEnterSocketMatchesBrokerDefault`, neither of which exists anymore. Coding agents follow that file literally, so stale rules there do real harm. The broker's `-sock` help also shows `/tmp/flue.sock` as its default, which isn't true.

**4. The memfd comments and fallback are off.**
- `MFD_EXEC` arrived in Linux 6.3, not 5.17. It relates to `vm.memfd_noexec`, not MDWE; MDWE restricts writable+executable mappings and doesn't affect exec'ing a memfd.
- The fallback retries without `MFD_EXEC` on *any* error. With `vm.memfd_noexec=2`, the first call fails with `EACCES` and the retry creates a non-executable memfd. The exec then fails inside the container with a confusing error. Retry only on `EINVAL`, and report other errors clearly.
- The same mistakes appear in `docs/flue.md`.

**5. Set the broker's process name.** Without `prctl(PR_SET_NAME)` it shows up as `4` in `ps` inside the container.

**6. The Security section undersells the tool.** "Only use this in trusted containers" contradicts the reason the filter exists. State the actual guarantee instead: the container can list allowed keys and get signatures from them while a session is open, and can't add, remove, lock or unlock keys, or use keys outside the allowlist.

## Smaller points

- `agentFilter` serves both as the shared policy (with a nil `upstream`) and as the per-client filter. A separate `policy` struct holding `allow` and `sock` would make the per-client value clearly complete.
- `pickSocketDir` accepts `$XDG_RUNTIME_DIR` without checking that it's writable, unlike the other candidates.
- The code comments are thorough, but item 3 shows they're already drifting from the code. Fewer, shorter comments would be easier to keep accurate.

## Next step: the session check

The design is now ready for it:

1. Enable `SO_PASSCRED` on the host's end of the control socket before starting podman.
2. Have the broker attach `SCM_CREDENTIALS` to the listener handoff message. The host then learns the broker's host pid and reads its session id from `/proc/<pid>/stat`.
3. On each accepted connection, use `SO_PEERCRED` (or `SO_PEERPIDFD` on Linux 6.5+) and compare the client's session id with the broker's.

This works because the broker is the session leader that `podman exec -t` creates, so everything run from your terminal shares its session id.
