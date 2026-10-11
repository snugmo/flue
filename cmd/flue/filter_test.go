package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	"go.uber.org/goleak"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// testKeys holds the shared test key material (ed25519 private keys and
// their SSH public key blobs + fingerprints).
var testKeys struct {
	priv1 ed25519.PrivateKey
	priv2 ed25519.PrivateKey
	blob1 []byte
	blob2 []byte
	fp1   string
	fp2   string
	once  bool
}

// initTestKeys generates two ed25519 keys once and stores them globally.
// The fingerprint is computed with the same algorithm the filter uses
// (SHA256 of the raw key blob), so allowlists match.
func initTestKeys(t *testing.T) {
	t.Helper()
	if testKeys.once {
		return
	}

	var pub1, pub2 ssh.PublicKey
	var err error

	_, testKeys.priv1, err = ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("gen ed25519 1: %v", err)
	}
	pub1, err = ssh.NewPublicKey(ed25519.PublicKey(testKeys.priv1[32:]))
	if err != nil {
		t.Fatalf("ssh pub 1: %v", err)
	}
	testKeys.blob1 = pub1.Marshal()

	_, testKeys.priv2, err = ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("gen ed25519 2: %v", err)
	}
	pub2, err = ssh.NewPublicKey(ed25519.PublicKey(testKeys.priv2[32:]))
	if err != nil {
		t.Fatalf("ssh pub 2: %v", err)
	}
	testKeys.blob2 = pub2.Marshal()

	testKeys.fp1 = keyFingerprintFromBlob(testKeys.blob1)
	testKeys.fp2 = keyFingerprintFromBlob(testKeys.blob2)
	testKeys.once = true
}

// startKeyringAgent starts a Unix socket agent server serving both test keys.
// It tracks in-flight ServeAgent goroutines so goleak can verify they drain
// after the listener closes.
func startKeyringAgent(t *testing.T) string {
	t.Helper()
	initTestKeys(t)

	kr := agent.NewKeyring()
	if err := kr.Add(agent.AddedKey{
		PrivateKey: testKeys.priv1,
		Comment:    "test-key-1",
	}); err != nil {
		t.Fatalf("add key 1: %v", err)
	}
	if err := kr.Add(agent.AddedKey{
		PrivateKey: testKeys.priv2,
		Comment:    "test-key-2",
	}); err != nil {
		t.Fatalf("add key 2: %v", err)
	}

	l, err := net.Listen("unix", "")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	var wg sync.WaitGroup
	done := make(chan struct{})

	go func() {
		defer close(done)
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func(c net.Conn) {
				defer wg.Done()
				defer c.Close()
				_ = agent.ServeAgent(kr, c)
			}(conn)
		}
	}()

	t.Cleanup(func() {
		l.Close()
		<-done
		wg.Wait()
	})

	return l.Addr().String()
}

// newTestFilter builds an agentFilter wrapping a real agent over a Unix
// socket, exercising the actual protocol path.
func newTestFilter(t *testing.T, allow map[string]struct{}) *agentFilter {
	t.Helper()
	addr := startKeyringAgent(t)

	upstreamConn, err := net.Dial("unix", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	// Close the client side first so the server's ServeAgent returns before
	// the listener cleanup waits on in-flight goroutines.
	t.Cleanup(func() { upstreamConn.Close() })

	return &agentFilter{
		upstream: agent.NewClient(upstreamConn),
		allow:    allow,
	}
}

func TestFilterListAllKeys(t *testing.T) {
	f := newTestFilter(t, nil)
	keys, err := f.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("expected 2 keys, got %d", len(keys))
	}
}

func TestFilterListAllowlist(t *testing.T) {
	initTestKeys(t)
	allow := map[string]struct{}{testKeys.fp1: {}}
	f := newTestFilter(t, allow)
	keys, err := f.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("expected 1 key, got %d", len(keys))
	}
	if diff := cmp.Diff(testKeys.blob1, keys[0].Blob); diff != "" {
		t.Fatalf("unexpected key blob (-want +got):\n%s", diff)
	}
}

func TestFilterSignAllowed(t *testing.T) {
	initTestKeys(t)
	allow := map[string]struct{}{testKeys.fp1: {}}
	f := newTestFilter(t, allow)

	pub, err := ssh.ParsePublicKey(testKeys.blob1)
	if err != nil {
		t.Fatalf("parse pub: %v", err)
	}

	sig, err := f.Sign(pub, []byte("test message"))
	if err != nil {
		t.Fatalf("Sign allowed key: %v", err)
	}
	if sig == nil {
		t.Fatal("expected non-nil signature")
	}
}

func TestFilterSignDenied(t *testing.T) {
	initTestKeys(t)
	// Allow only fp1; key 2 has fp2 and should be denied.
	allow := map[string]struct{}{testKeys.fp1: {}}
	f := newTestFilter(t, allow)

	pub, err := ssh.ParsePublicKey(testKeys.blob2)
	if err != nil {
		t.Fatalf("parse pub: %v", err)
	}

	_, err = f.Sign(pub, []byte("test message"))
	if err == nil {
		t.Fatal("expected error signing disallowed key, got nil")
	}
}

func TestFilterSignWithFlagsAllowed(t *testing.T) {
	initTestKeys(t)
	allow := map[string]struct{}{testKeys.fp1: {}}
	f := newTestFilter(t, allow)

	pub, err := ssh.ParsePublicKey(testKeys.blob1)
	if err != nil {
		t.Fatalf("parse pub: %v", err)
	}

	sig, err := f.SignWithFlags(pub, []byte("test"), 0)
	if err != nil {
		t.Fatalf("SignWithFlags allowed key: %v", err)
	}
	if sig == nil {
		t.Fatal("expected non-nil signature")
	}
}

func TestFilterMutatingOpsDenied(t *testing.T) {
	f := newTestFilter(t, nil)

	if err := f.RemoveAll(); err == nil {
		t.Error("RemoveAll should be denied")
	}
	if err := f.Lock([]byte("pass")); err == nil {
		t.Error("Lock should be denied")
	}
	if err := f.Unlock([]byte("pass")); err == nil {
		t.Error("Unlock should be denied")
	}
	if _, err := f.Signers(); err == nil {
		t.Error("Signers should be denied")
	}

	initTestKeys(t)
	pub, err := ssh.ParsePublicKey(testKeys.blob1)
	if err != nil {
		t.Fatalf("parse pub: %v", err)
	}
	if err := f.Remove(pub); err == nil {
		t.Error("Remove should be denied")
	}

	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	ak := agent.AddedKey{
		PrivateKey: priv,
	}
	if err := f.Add(ak); err == nil {
		t.Error("Add should be denied")
	}
}

func TestFilterExtension(t *testing.T) {
	f := newTestFilter(t, nil)

	// session-bind should be forwarded to upstream (keyring returns its own
	// unsupported error, proving the filter let it through).
	_, err := f.Extension("session-bind@openssh.com", []byte{})
	if err == nil {
		t.Error("expected an error for session-bind on a keyring")
	}

	// Any other extension should be rejected by our filter with the
	// canonical unsupported error.
	_, err = f.Extension("some-unknown-extension", []byte{})
	if err != agent.ErrExtensionUnsupported {
		t.Errorf("expected ErrExtensionUnsupported, got %v", err)
	}
}
