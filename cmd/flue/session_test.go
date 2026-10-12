package main

import (
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/sys/unix"
)

// Environment variables used when a test re-runs its own binary to
// play a role (the session-check test's broker child).
const (
	envSessionRole = "FLUE_SESSION_TEST_ROLE" // "broker" marks the child copy
	envSessionCtrl = "FLUE_SESSION_TEST_CTRL" // control socket path the child dials
	envSessionSock = "FLUE_SESSION_TEST_SOCK" // path of the child's agent listener
)

// TestReceiveListenerBrokerCredentials verifies that when the host end has
// SO_PASSCRED enabled (as runChildWithAgent does), the handover message
// carries the sender's SCM_CREDENTIALS and receiveListener records the
// sender's session id on the policy. Here the "broker" is the test process
// itself, so the expected value is its own session id.
func TestReceiveListenerBrokerCredentials(t *testing.T) {
	host, broker := makeControlPair(t)

	// Enable SO_PASSCRED on the host end, exactly as runChildWithAgent
	// does.
	rc, err := host.(*net.UnixConn).SyscallConn()
	if err != nil {
		t.Fatalf("syscall conn: %v", err)
	}
	var hfd int
	_ = rc.Control(func(fd uintptr) { hfd = int(fd) })
	if err := unix.SetsockoptInt(hfd, unix.SOL_SOCKET, unix.SO_PASSCRED, 1); err != nil {
		t.Fatalf("setsockopt SO_PASSCRED: %v", err)
	}

	// Hand over a real Unix listener, exactly as the broker does.
	l, err := net.Listen("unix", "")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { l.Close() })

	lf, err := l.(*net.UnixListener).File()
	if err != nil {
		t.Fatalf("listener file: %v", err)
	}
	oob := unix.UnixRights(int(lf.Fd()))
	if _, _, err := broker.(*net.UnixConn).WriteMsgUnix([]byte{0}, oob, nil); err != nil {
		lf.Close()
		t.Fatalf("write msg: %v", err)
	}
	lf.Close() // close only the dup; l keeps the path
	broker.Close()

	var pol policy
	var cs clientSet
	listener, acceptDone, rerr := receiveListener(host, &pol, &cs)
	if rerr != nil {
		t.Fatalf("receiveListener: %v", rerr)
	}
	if listener == nil {
		t.Fatal("receiveListener returned a nil listener")
	}
	listener.Close()
	<-acceptDone

	want, err := sessionIDOfPID(os.Getpid())
	if err != nil {
		t.Fatalf("sessionIDOfPID(self): %v", err)
	}
	if pol.brokerSession != want {
		t.Fatalf("brokerSession = %d, want %d (the writer's own session)", pol.brokerSession, want)
	}
}

// TestSessionIDFromStatLine verifies the /proc/<pid>/stat parser: the
// session id (field 6) must be found even when the comm field (2) contains
// spaces or parentheses — the parse starts after the last `)`.
func TestSessionIDFromStatLine(t *testing.T) {
	for _, tc := range []struct {
		name    string
		line    string
		want    int
		wantErr bool
	}{
		{"comm with spaces", "4242 (my comm) S 1 2 4242 0 -1 4194305 0 0 0 0 0 0 20 0 1 0 100 18446744073709551615 123 0 0", 4242, false},
		{"comm with parens", "7 (a (b (c)) d) S 1 7 7 7 0", 7, false},
		{"plain", "9 (comm) S 1 9 9 0", 9, false},
		{"no closing paren", "4242 (unclosed S 1 2 3", 0, true},
		{"too few fields", "42 (x) S 1", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := sessionIDFromStatLine(tc.line)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("session = %d, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("sessionIDFromStatLine: %v", err)
			}
			if got != tc.want {
				t.Fatalf("session = %d, want %d", got, tc.want)
			}
		})
	}

	// And against the real /proc/self/stat: the session must be positive
	// and stable across two reads.
	a, err := sessionIDOfPID(os.Getpid())
	if err != nil {
		t.Fatalf("sessionIDOfPID(self): %v", err)
	}
	b, err := sessionIDOfPID(os.Getpid())
	if err != nil {
		t.Fatalf("sessionIDOfPID(self): %v", err)
	}
	if a <= 0 || a != b {
		t.Fatalf("own session %d then %d, want the same positive value", a, b)
	}
}

// TestSessionCheckGate reports whether the client session check is in
// effect: the broker's session must be known and private (see
// sessionIsPrivate) — whether the broker leads it or not.
func TestSessionCheckGate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pid     int
		session int
		private bool
		known   bool
		want    bool
	}{
		{"no credentials", 0, 0, false, false, false},
		{"pid but unknown session", 4242, 0, false, false, false},
		{"private session, broker is the leader", 4242, 4242, true, true, true},
		{"private session, led by an exited helper", 4242, 7, true, true, true},
		{"shared session (e.g. the container's init)", 4242, 7, false, true, false},
		{"privacy scan failed", 4242, 7, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pol := &policy{
				brokerPID: tc.pid, brokerSession: tc.session,
				sessionPrivate: tc.private, sessionPrivateKnown: tc.known,
			}
			if got := sessionCheckActive(pol); got != tc.want {
				t.Fatalf("sessionCheckActive = %v, want %v (pid %d, session %d, private %v)",
					got, tc.want, tc.pid, tc.session, tc.private)
			}
		})
	}
}

// TestSessionIsPrivate verifies the private-session scan against a fake
// /proc tree: a session whose only live member is the broker is private;
// any other live process in it is not; an unreadable /proc is an error.
func TestSessionIsPrivate(t *testing.T) {
	tt := t.TempDir()
	// writeStat mirrors the real /proc layout: a per-pid directory
	// holding a "stat" file.
	writeStat := func(name, line string) {
		t.Helper()
		dir := filepath.Join(tt, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(line+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	oldProcRoot := procRoot
	procRoot = tt
	defer func() { procRoot = oldProcRoot }()

	// broker pid 100 in session 100; pid 101 in a different session.
	writeStat("100", "100 (broker) S 1 100 100 0")
	writeStat("101", "101 (other) S 1 101 200 0")
	writeStat("not-a-pid", "x (y) S 1 2 3 0")

	if private, err := sessionIsPrivate(100, 100); err != nil || !private {
		t.Fatalf("sessionIsPrivate(100, 100) = %v, %v; want true, nil", private, err)
	}

	// A second live member in the broker's session: not private.
	writeStat("102", "102 (member) S 1 101 100 0")
	if private, err := sessionIsPrivate(100, 100); err != nil || private {
		t.Fatalf("sessionIsPrivate(100, 100) = %v, %v; want false, nil", private, err)
	}

	// Unknown broker pid or session: not private, no error.
	if private, err := sessionIsPrivate(0, 100); private || err != nil {
		t.Fatalf("sessionIsPrivate(0, 100) = %v, %v; want false, nil", private, err)
	}
	if private, err := sessionIsPrivate(100, 0); private || err != nil {
		t.Fatalf("sessionIsPrivate(100, 0) = %v, %v; want false, nil", private, err)
	}

	// A broken /proc root: an error (the caller fails closed).
	procRoot = filepath.Join(tt, "no-such-dir")
	if private, err := sessionIsPrivate(100, 100); private || err == nil {
		t.Fatalf("sessionIsPrivate on a broken /proc = %v, %v; want false, error", private, err)
	}
}

// TestReceiveListenerCheckModes verifies the mode handling at handover:
// in required mode (injected broker with a tty), a handover that leaves
// the check disabled is a session failure (fail closed — the caller
// aborts); in off mode (no injection) the check is never enabled even
// when the handover carries broker credentials, because the broker is
// untrusted code; in advisory mode (the default) everything passes with
// the check simply off.
func TestReceiveListenerCheckModes(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mode       sessionCheckMode
		passCred   bool
		wantErr    bool
		wantActive bool
	}{
		{"required without credentials fails closed", sessionCheckRequired, false, true, false},
		{"off ignores the broker's credentials", sessionCheckOff, true, false, false},
		{"off without credentials", sessionCheckOff, false, false, false},
		{"advisory without credentials", sessionCheckAdvisory, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host, broker := makeControlPair(t)
			if tc.passCred {
				// Enable SO_PASSCRED on the host end, exactly as
				// runChildWithAgent does, so the message carries the
				// sender's credentials.
				rc, err := host.(*net.UnixConn).SyscallConn()
				if err != nil {
					t.Fatalf("syscall conn: %v", err)
				}
				var hfd int
				_ = rc.Control(func(fd uintptr) { hfd = int(fd) })
				if err := unix.SetsockoptInt(hfd, unix.SOL_SOCKET, unix.SO_PASSCRED, 1); err != nil {
					t.Fatalf("setsockopt SO_PASSCRED: %v", err)
				}
			}

			// Hand over a real Unix listener, exactly as the broker does.
			l, err := net.Listen("unix", "")
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			t.Cleanup(func() { l.Close() })

			lf, err := l.(*net.UnixListener).File()
			if err != nil {
				t.Fatalf("listener file: %v", err)
			}
			oob := unix.UnixRights(int(lf.Fd()))
			if _, _, err := broker.(*net.UnixConn).WriteMsgUnix([]byte{0}, oob, nil); err != nil {
				lf.Close()
				t.Fatalf("write msg: %v", err)
			}
			lf.Close() // close only the dup; l keeps the path
			broker.Close()

			var pol policy
			pol.sessionCheckMode = tc.mode
			var cs clientSet
			listener, acceptDone, err := receiveListener(host, &pol, &cs)
			t.Cleanup(func() {
				cs.closeAll()
				cs.Wait()
				if listener != nil {
					listener.Close()
				}
				<-acceptDone
			})
			if (err != nil) != tc.wantErr {
				t.Fatalf("receiveListener: err = %v, wantErr = %v", err, tc.wantErr)
			}
			if active := sessionCheckActive(&pol); active != tc.wantActive {
				t.Fatalf("sessionCheckActive = %v, want %v", active, tc.wantActive)
			}
		})
	}
}

// TestClientSessionAllowed verifies the session comparison: an unknown
// broker (check off) allows everything; a known broker allows only its
// own session and fails closed on an unknown client.
func TestClientSessionAllowed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		broker int
		client int
		want   bool
	}{
		{"check off (unknown broker)", 0, 0, true},
		{"check off allows any client", 0, 5, true},
		{"same session", 5, 5, true},
		{"different session", 5, 6, false},
		{"unknown client fails closed", 5, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := clientSessionAllowed(tc.broker, tc.client); got != tc.want {
				t.Fatalf("clientSessionAllowed(%d, %d) = %v, want %v",
					tc.broker, tc.client, got, tc.want)
			}
		})
	}
}

// TestPeerSessionID verifies that peerSessionID resolves the session of
// the peer of a real Unix connection (here: the test process itself) and
// fails closed for a connection that is not a Unix socket.
func TestPeerSessionID(t *testing.T) {
	l, err := net.Listen("unix", "")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { l.Close() })

	c, err := net.Dial("unix", l.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	want, err := sessionIDOfPID(os.Getpid())
	if err != nil {
		t.Fatalf("sessionIDOfPID(self): %v", err)
	}
	got, err := peerSessionID(c)
	if err != nil {
		t.Fatalf("peerSessionID: %v", err)
	}
	if got != want {
		t.Fatalf("peerSessionID = %d, want %d (own session)", got, want)
	}

	// A non-Unix connection has no peer credentials: fail closed.
	p1, p2 := net.Pipe()
	p2.Close()
	if _, err := peerSessionID(p1); err == nil {
		t.Fatal("peerSessionID(net.Pipe) = ok, want an error (fail closed)")
	}
	p1.Close()
}

// emptyAgent is a stub upstream agent for the session-check test: it has
// no keys and answers a list request with an empty identity list.
type emptyAgent struct{}

func (emptyAgent) List() ([]*agent.Key, error) { return nil, nil }
func (emptyAgent) Sign(ssh.PublicKey, []byte) (*ssh.Signature, error) {
	return nil, errors.New("emptyAgent: no keys")
}
func (emptyAgent) Add(agent.AddedKey) error       { return errors.New("emptyAgent: no adds") }
func (emptyAgent) Remove(ssh.PublicKey) error     { return errors.New("emptyAgent: no keys") }
func (emptyAgent) RemoveAll() error               { return nil }
func (emptyAgent) Lock([]byte) error              { return nil }
func (emptyAgent) Unlock([]byte) error            { return nil }
func (emptyAgent) Signers() ([]ssh.Signer, error) { return nil, nil }

// TestReceiveListenerSessionCheck is the end-to-end test of the session
// check. A child copy of this test binary plays the broker: it calls
// setsid (what a pty setup does to the process it runs — the process
// becomes the leader of a new session), creates and hands over a
// listener, and then dials its own socket as a client of that new
// session. The parent accepts the handover — learning the child's
// session, whose only live member is the child, so it is private and
// the check is active — and asserts that (a) a client from a different
// session (the parent's own dial) is refused before any agent traffic,
// and (b) a client from the broker's own session (the child's dial) is
// accepted and proxied through a stub upstream agent.
func TestReceiveListenerSessionCheck(t *testing.T) {
	if os.Getenv(envSessionRole) == "broker" {
		runSessionBroker() // does not return
	}
	if runtime.GOOS != "linux" {
		t.Skip("session ids come from /proc")
	}

	tmp := t.TempDir()
	brokerSock := filepath.Join(tmp, "broker.sock")

	// Control channel: the broker child dials this. The accepted
	// connection gets SO_PASSCRED (accepted sockets do not inherit
	// socket options from the listener): the kernel attaches the
	// sender's credentials to the handover message when either end has
	// the option, and delivers them only to a socket that has it — the
	// same arrangement as the host end of the socketpair in
	// runChildWithAgent.
	ctrlL, err := net.Listen("unix", filepath.Join(tmp, "ctrl"))
	if err != nil {
		t.Fatalf("listen ctrl: %v", err)
	}
	t.Cleanup(func() { ctrlL.Close() })

	// Stub upstream agent: answers the child's list request with an
	// empty identity list.
	agentL, err := net.Listen("unix", filepath.Join(tmp, "agent"))
	if err != nil {
		t.Fatalf("listen agent: %v", err)
	}
	t.Cleanup(func() { agentL.Close() })
	go func() {
		for {
			c, err := agentL.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_ = agent.ServeAgent(emptyAgent{}, c)
			}(c)
		}
	}()

	broker := exec.Command(os.Args[0], "-test.run="+t.Name())
	broker.Env = append(os.Environ(),
		envSessionRole+"=broker",
		envSessionCtrl+"="+ctrlL.Addr().String(),
		envSessionSock+"="+brokerSock)
	if err := broker.Start(); err != nil {
		t.Fatalf("start broker child: %v", err)
	}
	t.Cleanup(func() { _ = broker.Process.Kill() })

	// The child connects to the control channel. Enable SO_PASSCRED on
	// the accepted connection and signal the child to hand over its
	// listener (SCM_RIGHTS + SCM_CREDENTIALS), as flue-broker does.
	ctrlC := make(chan net.Conn, 1)
	go func() {
		c, err := ctrlL.Accept()
		if err != nil {
			return
		}
		ctrlC <- c
	}()
	var ctrl net.Conn
	select {
	case ctrl = <-ctrlC:
		// The child is connected.
	case <-time.After(15 * time.Second):
		_ = broker.Process.Kill()
		t.Fatalf("broker child did not connect within 15s")
	}
	defer ctrl.Close()
	{
		rc, err := ctrl.(*net.UnixConn).SyscallConn()
		if err != nil {
			t.Fatalf("syscall conn: %v", err)
		}
		var ctrlFD int
		_ = rc.Control(func(fd uintptr) { ctrlFD = int(fd) })
		if err := unix.SetsockoptInt(ctrlFD, unix.SOL_SOCKET, unix.SO_PASSCRED, 1); err != nil {
			t.Fatalf("SO_PASSCRED: %v", err)
		}
	}
	if _, err := ctrl.Write([]byte{'r'}); err != nil { // "ready": go ahead and hand over
		t.Fatalf("signal child: %v", err)
	}

	var pol policy
	pol.sock = agentL.Addr().String()
	var cs clientSet
	listener, acceptDone, rerr := receiveListener(ctrl, &pol, &cs)
	if rerr != nil {
		t.Fatalf("receiveListener: %v", rerr)
	}
	t.Cleanup(func() {
		cs.closeAll()
		cs.Wait()
		if listener != nil {
			listener.Close()
		}
		<-acceptDone
	})
	if listener == nil {
		t.Fatal("receiveListener returned a nil listener")
	}
	if !sessionCheckActive(&pol) {
		t.Fatalf("check is not active (pid %d, session %d, private %v); the broker leads its own session and is its only member, so it must be private",
			pol.brokerPID, pol.brokerSession, pol.sessionPrivate)
	}

	// (a) A client from a different session — this test process — must
	// be refused before any agent traffic: the connection is closed
	// without a response.
	c, err := net.Dial("unix", brokerSock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if n, err := c.Read(make([]byte, 4)); err != io.EOF {
		t.Fatalf("cross-session client was not refused (n=%d, err=%v)", n, err)
	}

	// (b) The broker child (in its own session) dials its own socket and
	// lists keys through the proxy: exit 0 means accepted and proxied.
	if err := broker.Wait(); err != nil {
		t.Fatalf("same-session client was not accepted: %v", err)
	}
}

// runSessionBroker runs in the child copy of the test binary (the fake
// broker). It calls setsid (the situation `podman exec -t` creates: the
// process becomes the leader of a new session), listens on the
// session-check socket and hands the listener over to the parent exactly
// as flue-broker does, then dials its own socket as a client of its new
// session and lists keys through the proxy. Exit 0 means the request was
// accepted and proxied; any other exit code means refused or broken.
func runSessionBroker() {
	if _, err := unix.Setsid(); err != nil {
		os.Exit(90)
	}
	sock := os.Getenv(envSessionSock)
	l, err := net.Listen("unix", sock)
	if err != nil {
		os.Exit(91)
	}
	ctrl, err := net.Dial("unix", os.Getenv(envSessionCtrl))
	if err != nil {
		l.Close()
		os.Exit(92)
	}
	lf, err := l.(*net.UnixListener).File()
	if err != nil {
		l.Close()
		ctrl.Close()
		os.Exit(93)
	}
	// Wait for the parent to have enabled SO_PASSCRED on its end, then
	// hand the listener over (SCM_RIGHTS + SCM_CREDENTIALS), exactly as
	// flue-broker does after the host is ready.
	if _, err := ctrl.Read(make([]byte, 1)); err != nil {
		l.Close()
		ctrl.Close()
		os.Exit(94)
	}
	oob := unix.UnixRights(int(lf.Fd()))
	if _, _, err := ctrl.(*net.UnixConn).WriteMsgUnix([]byte{0}, oob, nil); err != nil {
		lf.Close()
		ctrl.Close()
		os.Exit(94)
	}
	lf.Close() // close only the dup; l keeps the listener

	// The user's command, from the broker's own session: list keys
	// through the proxy.
	c, err := net.Dial("unix", sock)
	if err != nil {
		os.Exit(95)
	}
	defer c.Close()
	if _, err := agent.NewClient(c).List(); err != nil {
		os.Exit(1) // refused (closed before a response) or broken
	}
	os.Exit(0)
}
