// Command fluehost is the host-side half of the flue SSH agent bridge.
//
// It opens the host's SSH agent socket (normally $SSH_AUTH_SOCK) with the
// canonical socket()+connect(2) sequence, dup2's the resulting connected
// descriptor onto a chosen FD number (default 3), and then exec's a command
// with that FD left open. Because a dup'd FD stays open across exec(2), the
// child receives a ready-to-use, connected agent socket on that FD.
//
// This is the reliable replacement for the shell redirection trick
// (exec 3< "$SSH_AUTH_SOCK" 3> "$SSH_AUTH_SOCK"), which can fail with
// ENXIO on some Unix socket states because the shell uses open(2)'s
// implicit-connect path rather than an explicit connect(2).
//
// Typical use:
//
//	fluehost -- podman exec \
//	  --preserve-fds 1 \
//	  -e SSH_AUTH_SOCK=/tmp/flue.sock \
//	  <container> flue
//
// fluehost opens the agent socket as FD 3, then exec's the podman command,
// which preserves FD 3 into the container where flue relays it on its own
// Unix socket.
package main

import (
	"flag"
	"log"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

func main() {
	sockArg := flag.String("sock", "",
		"Agent socket path to open (default $SSH_AUTH_SOCK)")
	fdArg := flag.String("fd", "3",
		"File descriptor number to place the connected agent socket on (default 3)")
	flag.Parse()

	sockPath := *sockArg
	if sockPath == "" {
		sockPath = os.Getenv("SSH_AUTH_SOCK")
		if sockPath == "" {
			log.Fatalf("no agent socket: set -sock or SSH_AUTH_SOCK")
		}
	}

	fdNum, err := strconv.Atoi(*fdArg)
	if err != nil || fdNum < 3 {
		log.Fatalf("invalid -fd %q: must be an integer >= 3", *fdArg)
	}

	args := flag.Args()
	if len(args) == 0 {
		log.Fatalf("usage: fluehost [-sock path] [-fd N] -- command [args...]")
	}

	// Open and connect to the agent socket using the explicit
	// socket()+connect(2) path (net.Dial does exactly this), which is more
	// reliable than the shell's open(2) implicit connect.
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		log.Fatalf("connect to agent socket %s: %v", sockPath, err)
	}
	uc := conn.(*net.UnixConn)

	// Hand the fd to a long-lived *os.File. This closes the conn's reference
	// but keeps the descriptor open in the returned file, so the socket stays
	// connected until that file is closed (which we never do: the exec below
	// replaces the process before any close can run).
	connFile, err := uc.File()
	if err != nil {
		log.Fatalf("resolve fd for %s: %v", sockPath, err)
	}
	_ = connFile // keep the *os.File (and thus the fd) alive
	connFD := int(connFile.Fd())

	// Close any pre-existing fdNum so we don't leak it, then move the
	// connected socket onto fdNum. The descriptor remains open across the
	// exec below, so the child inherits it.
	if err := unix.Close(fdNum); err != nil && err != unix.EBADF {
		log.Printf("close fd %d: %v", fdNum, err)
	}
	if err := unix.Dup2(connFD, fdNum); err != nil {
		log.Fatalf("dup2 %d -> %d: %v", connFD, fdNum, err)
	}

	log.Printf("fluehost: agent socket %s connected on fd %d; exec: %v",
		sockPath, fdNum, args)

	// exec the command; the child inherits fdNum pointing at the agent.
	// Resolve the command to an absolute path so exec works even when the
	// caller passes a bare name like "podman".
	cmdPath := args[0]
	if !strings.Contains(cmdPath, "/") {
		if resolved, rerr := exec.LookPath(cmdPath); rerr == nil {
			cmdPath = resolved
			args[0] = resolved
		}
	}
	execErr := syscall.Exec(cmdPath, args, os.Environ())
	log.Fatalf("exec %s: %v", cmdPath, execErr)
}
