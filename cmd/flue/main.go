// Command flue is the host-side half of the flue SSH agent bridge.
//
// It creates a Unix socketpair (SOCK_SEQPACKET) and passes one end into the
// container as fd 3 via `podman exec --preserve-fds 1`. The container-side
// broker (flue-broker) listens on a Unix socket, then hands that listening
// fd back to the host over the control channel. The host accepts client
// connections directly on the received listener and proxies each one to the
// real host agent ($SSH_AUTH_SOCK) using golang.org/x/crypto/ssh/agent with
// a filtering wrapper. Each client gets a fresh upstream connection, so
// repeated ssh / ssh-add / git invocations all work.
//
// Lifetime is driven by the process tree, not the control channel. The
// control channel carries exactly one message (the listener fd); its
// connection stays open until the broker is gone (so the broker never sees a
// premature "host gone"), and flue blocks on its child (the `podman exec`
// that runs the broker) exiting. In standalone mode the broker runs until
// flue is signalled; in wrapper mode the broker exits when the wrapped
// command does, so flue returns that command's exit status.
//
// The filter implements agent.ExtendedAgent:
//   - List: forwarded, but only keys matching --allow-key are returned
//     (all keys if no --allow-key flag is given).
//   - Sign / SignWithFlags: forwarded only for allowed keys.
//   - Add, Remove, RemoveAll, Lock, Unlock, Signers: always denied.
//   - Extension: only session-bind@openssh.com is forwarded.
//
// Typical use:
//
//	flue -- podman exec \
//	  --preserve-fds 1 \
//	  -e SSH_AUTH_SOCK=/tmp/flue.sock \
//	  <container> flue-broker
//
// Wrapper mode — hand the broker a command and get a shell (or any command)
// inside the container with the agent available. The broker sets
// SSH_AUTH_SOCK for the child, so -e is not required, and flue exits when
// the child does, returning its exit code:
//
//	flue -- podman exec \
//	  --preserve-fds 1 \
//	  <container> flue-broker bash
package main

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/sys/unix"
)

var (
	sockArg   = flag.String("sock", "", "Agent socket path to use (default $SSH_AUTH_SOCK)")
	allowKeys = newAllowKeyFlag()
	verbose   = flag.Bool("verbose", false, "log every agent request to stderr")
)

func init() {
	flag.Var(allowKeys, "allow-key", "SHA256 fingerprint to allow (repeatable)")
}

func main() {
	flag.Parse()

	sockPath := *sockArg
	if sockPath == "" {
		sockPath = os.Getenv("SSH_AUTH_SOCK")
		if sockPath == "" {
			log.Fatalf("no agent socket: set -sock or SSH_AUTH_SOCK")
		}
	}

	args := flag.Args()
	if len(args) == 0 {
		log.Fatalf("usage: flue [-sock path] [-allow-key fp]... -- command [args...]")
	}

	// Resolve the command to an absolute path.
	cmdPath := args[0]
	if !strings.Contains(cmdPath, "/") {
		if resolved, err := exec.LookPath(cmdPath); err == nil {
			cmdPath = resolved
			args[0] = resolved
		}
	}

	// Create a socketpair for the control channel.
	// The container receives end B (fd 3); the host keeps end A.
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		log.Fatalf("socketpair: %v", err)
	}
	a, b := pair[0], pair[1]
	// Pass end B as an extra file in the child; it will be fd 3
	// (first extra file after stdin/stdout/stderr).
	childFd := os.NewFile(uintptr(b), "flue-control-child")
	defer closeFile(childFd)

	cmd := exec.Command(cmdPath, args[1:]...)
	cmd.ExtraFiles = []*os.File{childFd}
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()

	if err := cmd.Start(); err != nil {
		log.Fatalf("start %s: %v", cmdPath, err)
	}

	// Close the host copy of end B; the container now owns it.
	if err := unix.Close(b); err != nil {
		log.Printf("flue: close control end B: %v", err)
	}

	// The host's control socket is end A.
	ctrl, err := net.FileConn(os.NewFile(uintptr(a), "flue-control-host"))
	if err != nil {
		log.Fatalf("control fd: %v", err)
	}

	allow := allowKeys.toSet()
	filter := &agentFilter{
		upstream: nil, // set per-client in handleClient
		allow:    allow,
		verbose:  *verbose,
	}

	log.Printf("flue: agent %s, filter: %s, exec: %v",
		sockPath, allowKeys.describe(), args)

	var wg sync.WaitGroup
	listener, acceptDone := receiveListener(ctrl, filter, &wg)

	// Wait for the command (and thus the broker) to exit. This is the
	// primary "broker gone" signal, and unlike control-channel EOF it does
	// not depend on the netpoller reporting peer closure. In standalone mode
	// the broker outlives the child, so this blocks until flue itself is
	// signalled; in wrapper mode the broker exits when the wrapped command
	// does, so this returns promptly carrying that command's status.
	err = cmd.Wait()

	// The broker is gone: close the one-shot control connection (kept open
	// for the child's lifetime so the broker never sees a premature "host
	// gone"), stop the accept loop, and let in-flight handlers drain.
	closeQuiet(ctrl)
	if listener != nil {
		listener.Close()
	}
	<-acceptDone
	wg.Wait()

	if err != nil {
		code := 1
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
		log.Printf("flue: command exited: %v", err)
		os.Exit(code)
	}
}

// --- allow-key repeatable flag ---

type allowKeyFlag struct {
	vals []string
}

func newAllowKeyFlag() *allowKeyFlag { return &allowKeyFlag{} }

func (a *allowKeyFlag) String() string { return strings.Join(a.vals, ",") }

func (a *allowKeyFlag) Set(s string) error {
	a.vals = append(a.vals, s)
	return nil
}

// toSet returns a map of fingerprint → struct{} for O(1) lookups.
func (a *allowKeyFlag) toSet() map[string]struct{} {
	m := make(map[string]struct{}, len(a.vals))
	for _, v := range a.vals {
		m[v] = struct{}{}
	}
	return m
}

// describe returns a human-readable summary for logging.
func (a *allowKeyFlag) describe() string {
	if len(a.vals) == 0 {
		return "all keys"
	}
	return strings.Join(a.vals, ", ")
}

// --- agent filter ---

// agentFilter wraps an upstream ssh agent client and implements
// agent.ExtendedAgent with a key allowlist policy.
type agentFilter struct {
	upstream agent.ExtendedAgent
	allow    map[string]struct{} // empty = allow all
	verbose  bool
}

// isAllowed checks whether a key's fingerprint is in the allowlist.
func (f *agentFilter) isAllowed(fp string) bool {
	if len(f.allow) == 0 {
		return true
	}
	_, ok := f.allow[fp]
	return ok
}

func (f *agentFilter) logf(op, fp string, allowed bool) {
	if !f.verbose {
		return
	}
	status := "allowed"
	if !allowed {
		status = "DENIED"
	}
	log.Printf("flue: %s %s [%s]", op, fp, status)
}

// keyFingerprint computes the SHA256: fingerprint of an ssh.PublicKey.
func keyFingerprint(key ssh.PublicKey) string {
	return ssh.FingerprintSHA256(key)
}

// keyFingerprintFromBlob computes the SHA256: fingerprint from a raw key blob.
// Uses the same algorithm as ssh.FingerprintSHA256: SHA256 of the blob,
// encoded with base64.RawStdEncoding (no padding), prefixed with "SHA256:".
func keyFingerprintFromBlob(blob []byte) string {
	h := sha256.Sum256(blob)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(h[:])
}

// List returns keys, filtered by allowlist if one is set.
func (f *agentFilter) List() ([]*agent.Key, error) {
	keys, err := f.upstream.List()
	if err != nil {
		return nil, err
	}
	if len(f.allow) == 0 {
		f.logf("list", fmt.Sprintf("%d keys", len(keys)), true)
		return keys, nil
	}
	var out []*agent.Key
	for _, k := range keys {
		fp := keyFingerprintFromBlob(k.Blob)
		if f.isAllowed(fp) {
			out = append(out, k)
		} else {
			f.logf("list", fp, false)
		}
	}
	f.logf("list", fmt.Sprintf("%d of %d keys allowed", len(out), len(keys)), true)
	return out, nil
}

// Sign forwards the signing request only for allowed keys.
func (f *agentFilter) Sign(key ssh.PublicKey, data []byte) (*ssh.Signature, error) {
	fp := keyFingerprint(key)
	if !f.isAllowed(fp) {
		f.logf("sign", fp, false)
		return nil, fmt.Errorf("signing denied for key %s", fp)
	}
	f.logf("sign", fp, true)
	return f.upstream.Sign(key, data)
}

// SignWithFlags is like Sign but with flags (needed for RSA rsa-sha2-256/512).
func (f *agentFilter) SignWithFlags(key ssh.PublicKey, data []byte, flags agent.SignatureFlags) (*ssh.Signature, error) {
	fp := keyFingerprint(key)
	if !f.isAllowed(fp) {
		f.logf("sign(flags)", fp, false)
		return nil, fmt.Errorf("signing denied for key %s", fp)
	}
	f.logf("sign(flags)", fp, true)
	return f.upstream.SignWithFlags(key, data, flags)
}

// Add is always denied.
func (f *agentFilter) Add(k agent.AddedKey) error {
	f.logf("add", "", false)
	return fmt.Errorf("adding keys is not allowed through flue")
}

// Remove is always denied.
func (f *agentFilter) Remove(key ssh.PublicKey) error {
	f.logf("remove", keyFingerprint(key), false)
	return fmt.Errorf("removing keys is not allowed through flue")
}

// RemoveAll is always denied.
func (f *agentFilter) RemoveAll() error {
	f.logf("remove-all", "", false)
	return fmt.Errorf("removing all keys is not allowed through flue")
}

// Lock is always denied.
func (f *agentFilter) Lock(passphrase []byte) error {
	f.logf("lock", "", false)
	return fmt.Errorf("locking the agent is not allowed through flue")
}

// Unlock is always denied.
func (f *agentFilter) Unlock(passphrase []byte) error {
	f.logf("unlock", "", false)
	return fmt.Errorf("unlocking the agent is not allowed through flue")
}

// Signers returns signers for the given key blob.
func (f *agentFilter) Signers() ([]ssh.Signer, error) {
	f.logf("signers", "", false)
	return nil, fmt.Errorf("signers request is not allowed through flue")
}

// Extension forwards only session-bind@openssh.com; everything else is
// unsupported.
func (f *agentFilter) Extension(extensionType string, req []byte) (resp []byte, err error) {
	if extensionType == "session-bind@openssh.com" {
		return f.upstream.Extension(extensionType, req)
	}
	return nil, agent.ErrExtensionUnsupported
}

// --- control channel ---

// closeQuiet closes a connection, logging the error unless it is a
// benign "use of closed network connection".
func closeQuiet(c net.Conn) {
	if err := c.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		log.Printf("flue: close: %v", err)
	}
}

// closeFD closes a raw file descriptor, logging the error unless it is
// already closed.
func closeFD(fd int) {
	if err := unix.Close(fd); err != nil && !errors.Is(err, unix.EBADF) {
		log.Printf("flue: close fd %d: %v", fd, err)
	}
}

// closeFile closes an os.File, logging the error unless it is already
// closed.
func closeFile(f *os.File) {
	if err := f.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		log.Printf("flue: close file: %v", err)
	}
}

// receiveListener reads the single control message (the broker's listening
// fd) from the control channel, wraps it in a *net.UnixListener, and starts
// a background accept loop that proxies each client connection to the agent.
// It returns the listener (so the caller can close it to stop the loop) and
// a channel that is closed when the accept loop exits. The control message is
// one-shot, but the control connection is NOT closed here: the caller keeps
// it open until the broker is gone, so the broker (which reads its own end)
// does not observe a premature "host gone". The caller closes ctrl.
func receiveListener(ctrl net.Conn, filter *agentFilter, wg *sync.WaitGroup) (net.Listener, <-chan struct{}) {
	buf := make([]byte, 4096)
	oob := make([]byte, 4096)

	n, oobn, flags, _, err := ctrl.(*net.UnixConn).ReadMsgUnix(buf, oob)
	_ = n
	if err != nil {
		if err == io.EOF || err == unix.ECONNRESET {
			log.Printf("flue: control channel closed before listener received")
		} else {
			log.Printf("flue: control read: %v", err)
		}
		return nil, closedChannel()
	}
	if flags&unix.MSG_CTRUNC != 0 {
		log.Printf("flue: control message truncated; discarding")
		return nil, closedChannel()
	}
	cmsgs, err := unix.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		log.Printf("flue: parse cmsg: %v", err)
		return nil, closedChannel()
	}
	// Expect exactly one listener fd; use the first, close any extras.
	var listenFd int = -1
	for _, cmsg := range cmsgs {
		rights, rerr := unix.ParseUnixRights(&cmsg)
		if rerr != nil {
			log.Printf("flue: parse rights: %v", rerr)
			continue
		}
		for _, r := range rights {
			if listenFd < 0 {
				listenFd = r
			} else {
				closeFD(r)
			}
		}
	}
	if listenFd < 0 {
		log.Printf("flue: no listener fd received")
		return nil, closedChannel()
	}

	// Wrap the received listener fd in a *net.UnixListener so Accept works.
	listener, err := net.FileListener(os.NewFile(uintptr(listenFd), "flue-listener"))
	if err != nil {
		log.Printf("flue: listener fd: %v", err)
		closeFD(listenFd)
		return nil, closedChannel()
	}
	log.Printf("flue: accepting on handed-over listener")

	// Accept client connections in a background loop; it ends when the
	// listener is closed by the caller.
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		for {
			conn, err := listener.Accept()
			if err != nil {
				log.Printf("flue: accept: %v", err)
				return
			}
			wg.Add(1)
			go func(c net.Conn) {
				defer wg.Done()
				handleClient(c, filter)
			}(conn)
		}
	}()

	return listener, acceptDone
}

// closedChannel returns an already-closed channel, used as a sentinel
// acceptDone when no listener was received.
func closedChannel() <-chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}

// handleClient proxies a single client connection (fd) to the agent.
// It dials a fresh upstream agent connection for this client, wraps it
// with the filter, and runs agent.ServeAgent.
// handleClient proxies a single client connection to the agent. It dials a
// fresh upstream agent connection for this client, wraps it with the filter,
// and runs agent.ServeAgent, which blocks until the client disconnects.
func handleClient(client net.Conn, filter *agentFilter) {
	defer closeQuiet(client)

	// Dial a fresh upstream agent connection for this client.
	upstream, err := dialAgent()
	if err != nil {
		log.Printf("flue: dial upstream: %v", err)
		return
	}
	defer closeQuiet(upstream)

	// Build a per-client filter wrapping this client's upstream.
	clientFilter := &agentFilter{
		upstream: agent.NewClient(upstream),
		allow:    filter.allow,
		verbose:  filter.verbose,
	}

	// ServeAgent returns when the client connection ends; I/O errors there
	// are normal (peer closed), so the result is not checked.
	_ = agent.ServeAgent(clientFilter, client)
}

// dialAgent connects to the host's real SSH agent.
// Prefers the -sock flag over $SSH_AUTH_SOCK.
func dialAgent() (net.Conn, error) {
	sockPath := *sockArg
	if sockPath == "" {
		sockPath = os.Getenv("SSH_AUTH_SOCK")
	}
	return net.Dial("unix", sockPath)
}
