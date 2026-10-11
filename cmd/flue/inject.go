package main

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// brokerMemfdName is the name given to the memfd that carries the injected
// broker. It shows up in /proc/self/fd as "memfd:flue-broker (deleted)" and
// exists only for diagnostics.
const brokerMemfdName = "flue-broker"

// brokerMemfdFlags are the flags for the injected-broker memfd. MFD_EXEC
// makes the memfd executable via /proc/self/fd/<n>; it is required once the
// target process enables MDWE (memory-deny-write-execute, a hardened-runtime
// prctl) and is harmless for ordinary processes. MFD_CLOEXEC is belt-and-
// braces: the fd is already dropped after the in-container exec by the
// dup2 in the child setup, but it documents the intent that the broker does
// not need to keep the memfd it was launched from. MFD_ALLOW_SEALING lets
// injectBrokerMemfd seal the contents once written (see below).
const brokerMemfdFlags = unix.MFD_EXEC | unix.MFD_CLOEXEC | unix.MFD_ALLOW_SEALING

// injectBrokerMemfd creates a memfd containing the embedded flue-broker
// bytes (see embed.go) and returns it as an *os.File ready to be passed to
// a child as a preserved fd (fd 4, alongside the control socketpair on fd
// 3). The container then execs the broker from /proc/self/fd/4, so the
// broker runs entirely in memory and never touches the container's
// filesystem — no writable filesystem is required.
//
// The memfd is created with MFD_EXEC so it can be executed via
// /proc/self/fd/<n> even in MDWE-hardened processes. If the kernel does not
// support MFD_EXEC (Linux < 5.17), it falls back to a plain memfd, which is
// still executable by ordinary (non-MDWE) processes.
func injectBrokerMemfd() (*os.File, error) {
	if !looksLikeELF(flueBrokerBin) {
		return nil, fmt.Errorf("embedded flue-broker is not a Linux ELF binary: build flue-broker (to cmd/flue/flue-broker.bin) before building flue, then rebuild")
	}

	fd, err := unix.MemfdCreate(brokerMemfdName, brokerMemfdFlags)
	if err != nil {
		// Older kernel without MFD_EXEC: a plain memfd is still executable
		// by non-MDWE processes, so retry without the flag before failing.
		fd, err = unix.MemfdCreate(brokerMemfdName, unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
		if err != nil {
			return nil, fmt.Errorf("memfd_create: %w", err)
		}
	}
	f := os.NewFile(uintptr(fd), "flue-broker-memfd")
	if _, err := f.Write(flueBrokerBin); err != nil {
		f.Close()
		return nil, fmt.Errorf("write broker to memfd: %w", err)
	}
	// The container side receives this fd writable. Without seals, any
	// process in the container able to reach it (via /proc/<pid>/fd) could
	// rewrite the bytes of the running broker. Seal it read-only and
	// fixed-size, and refuse further seal changes.
	const seals = unix.F_SEAL_SEAL | unix.F_SEAL_SHRINK | unix.F_SEAL_GROW | unix.F_SEAL_WRITE
	if _, err := unix.FcntlInt(f.Fd(), unix.F_ADD_SEALS, seals); err != nil {
		f.Close()
		return nil, fmt.Errorf("seal broker memfd: %w", err)
	}
	log.Debug("flue: created broker memfd", "bytes", len(flueBrokerBin))
	return f, nil
}
