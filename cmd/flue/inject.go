package main

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// brokerMemfdName is the name given to the memfd that carries the injected
// broker. It shows up in /proc/self/fd as "memfd:flue-broker (deleted)" and
// exists only for diagnostics.
const brokerMemfdName = "flue-broker"

// brokerMemfdFlags are the flags for the injected-broker memfd. MFD_EXEC
// (Linux ≥ 6.3) opts the memfd into execution against the vm.memfd_noexec
// sysctl, which otherwise refuses to exec memfd-backed files; it is a
// no-op on kernels that predate the sysctl. MFD_CLOEXEC is belt-and-braces:
// the child drops the fd across its exec, but it documents that the broker
// needs no persistent handle to its own backing file. MFD_ALLOW_SEALING
// lets injectBrokerMemfd seal the bytes once written (see below).
const brokerMemfdFlags = unix.MFD_EXEC | unix.MFD_CLOEXEC | unix.MFD_ALLOW_SEALING

// memfdCreateWithFallback creates a memfd with the preferred flags,
// retrying without MFD_EXEC only when the kernel rejects the flag itself
// (EINVAL: older than Linux 6.3, where a plain memfd is still executable).
// Any other error — in particular EACCES from vm.memfd_noexec=2, which
// refuses executable memfds outright — is reported as a hard failure.
func memfdCreateWithFallback(create func(flags int) (int, error)) (int, error) {
	fd, err := create(brokerMemfdFlags)
	if err == nil {
		return fd, nil
	}
	if !errors.Is(err, unix.EINVAL) {
		return 0, fmt.Errorf("memfd_create: %w — this system refuses executable memfds (e.g. vm.memfd_noexec=2): run with --no-inject-broker or change the sysctl", err)
	}
	// Kernel too old to know MFD_EXEC: a plain memfd is still executable
	// there, since vm.memfd_noexec does not exist before Linux 6.3.
	fd, err = create(unix.MFD_CLOEXEC | unix.MFD_ALLOW_SEALING)
	if err != nil {
		return 0, fmt.Errorf("memfd_create (without MFD_EXEC): %w", err)
	}
	return fd, nil
}

// injectBrokerMemfd creates a memfd containing the embedded flue-broker
// bytes (see embed.go) and returns it as an *os.File ready to be passed to
// a child as a preserved fd (fd 4, alongside the control socketpair on fd
// 3). The container then execs the broker from /proc/self/fd/4, so the
// broker runs entirely in memory and never touches the container's
// filesystem — no writable filesystem is required. The MFD_EXEC fallback
// and the hard failure on vm.memfd_noexec=2 are in
// memfdCreateWithFallback.
func injectBrokerMemfd() (*os.File, error) {
	if !looksLikeELF(flueBrokerBin) {
		return nil, fmt.Errorf("embedded flue-broker is not a Linux ELF binary: build flue-broker (to cmd/flue/flue-broker.bin) before building flue, then rebuild")
	}

	fd, err := memfdCreateWithFallback(func(flags int) (int, error) {
		return unix.MemfdCreate(brokerMemfdName, flags)
	})
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "flue-broker-memfd")
	if _, err := f.Write(flueBrokerBin); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("write broker to memfd: %w", err)
	}
	// The container side receives this fd writable. Without seals, any
	// process in the container able to reach it (via /proc/<pid>/fd) could
	// rewrite the bytes of the running broker. Seal it read-only and
	// fixed-size, and refuse further seal changes.
	const seals = unix.F_SEAL_SEAL | unix.F_SEAL_SHRINK | unix.F_SEAL_GROW | unix.F_SEAL_WRITE
	if _, err := unix.FcntlInt(f.Fd(), unix.F_ADD_SEALS, seals); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("seal broker memfd: %w", err)
	}
	log.Debug("flue: created broker memfd", "bytes", len(flueBrokerBin))
	return f, nil
}
