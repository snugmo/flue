package main

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"go.uber.org/goleak"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// testKeys holds the shared test key material (an ed25519 key, a second
// ed25519 key, and an ECDSA P-256 key, so tests can distinguish key
// types), with their SSH public key blobs + fingerprints.
var testKeys struct {
	priv1 ed25519.PrivateKey
	priv2 ed25519.PrivateKey
	priv3 *ecdsa.PrivateKey
	blob1 []byte
	blob2 []byte
	blob3 []byte
	fp1   string
	fp2   string
	fp3   string
	once  bool
}

// initTestKeys generates the shared test keys once and stores them
// globally. The fingerprints are computed with the same algorithm the
// filter uses (SHA256 of the raw key blob), so allowlists match.
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

	testKeys.priv3, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen ecdsa: %v", err)
	}
	pub3, err := ssh.NewPublicKey(testKeys.priv3.Public())
	if err != nil {
		t.Fatalf("ssh pub 3: %v", err)
	}
	testKeys.blob3 = pub3.Marshal()

	testKeys.fp1 = keyFingerprintFromBlob(testKeys.blob1)
	testKeys.fp2 = keyFingerprintFromBlob(testKeys.blob2)
	testKeys.fp3 = keyFingerprintFromBlob(testKeys.blob3)
	testKeys.once = true
}

// serveKeyring serves kr on a fresh Unix socket with an accept loop and
// returns the socket path. It tracks in-flight ServeAgent goroutines so
// goleak can verify they drain after the listener closes.
func serveKeyring(t *testing.T, kr agent.Agent) string {
	t.Helper()

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

// startKeyringAgent starts a Unix socket agent server serving both test
// keys.
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

	return serveKeyring(t, kr)
}

// startTypeKeyring starts a Unix socket agent serving one ed25519 key
// and one ECDSA P-256 key, so tests can filter by key type.
func startTypeKeyring(t *testing.T) string {
	t.Helper()
	initTestKeys(t)

	kr := agent.NewKeyring()
	if err := kr.Add(agent.AddedKey{
		PrivateKey: testKeys.priv1,
		Comment:    "ed25519-key",
	}); err != nil {
		t.Fatalf("add ed25519 key: %v", err)
	}
	if err := kr.Add(agent.AddedKey{
		PrivateKey: testKeys.priv3,
		Comment:    "ecdsa-key",
	}); err != nil {
		t.Fatalf("add ecdsa key: %v", err)
	}

	return serveKeyring(t, kr)
}

// newTestFilterFor dials the filter at an agent address with both
// allowlists set.
func newTestFilterFor(t *testing.T, addr string, allow, allowTypes map[string]struct{}) *agentFilter {
	t.Helper()
	conn, err := net.Dial("unix", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return &agentFilter{
		upstream: agent.NewClient(conn),
		pol:      &policy{allow: allow, allowTypes: allowTypes},
	}
}

// newCertKey generates an ed25519 user key certified by a fresh CA and
// returns the AddedKey to add it to a keyring, the certificate's wire
// blob, and the fingerprint of the underlying key — the value
// ssh-add -l prints for a certified key.
func newCertKey(t *testing.T, comment string) (agent.AddedKey, []byte, string) {
	t.Helper()
	_, caPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("gen ca: %v", err)
	}
	caPub, err := ssh.NewPublicKey(caPriv.Public())
	if err != nil {
		t.Fatalf("ca pub: %v", err)
	}
	caSigner, err := ssh.NewSignerFromKey(caPriv)
	if err != nil {
		t.Fatalf("ca signer: %v", err)
	}
	_, userPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("gen user: %v", err)
	}
	userPub, err := ssh.NewPublicKey(userPriv.Public())
	if err != nil {
		t.Fatalf("user pub: %v", err)
	}
	cert := &ssh.Certificate{
		Key:          userPub,
		CertType:     ssh.UserCert,
		ValidAfter:   uint64(time.Now().Add(-time.Hour).Unix()),
		ValidBefore:  uint64(time.Now().Add(time.Hour).Unix()),
		SignatureKey: caPub,
	}
	if err := cert.SignCert(rand.Reader, caSigner); err != nil {
		t.Fatalf("sign cert: %v", err)
	}
	return agent.AddedKey{
		PrivateKey:  userPriv,
		Certificate: cert,
		Comment:     comment,
	}, cert.Marshal(), ssh.FingerprintSHA256(userPub)
}

// TestFilterListCertificate verifies that List fingerprints a certified key
// the way ssh-add -l prints it: with the allowlist set to the underlying
// key's fingerprint, the certificate is returned, and a certificate whose
// base key is not in the allowlist is filtered out.
func TestFilterListCertificate(t *testing.T) {
	akA, blobA, fpA := newCertKey(t, "cert-a")
	akB, blobB, fpB := newCertKey(t, "cert-b")
	if fpA == fpB {
		t.Fatal("two freshly generated base keys have the same fingerprint")
	}

	run := func(t *testing.T, allow map[string]struct{}, wantBlob []byte) {
		t.Helper()
		kr := agent.NewKeyring()
		for _, ak := range []agent.AddedKey{akA, akB} {
			if err := kr.Add(ak); err != nil {
				t.Fatalf("add: %v", err)
			}
		}
		addr := serveKeyring(t, kr)

		conn, err := net.Dial("unix", addr)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		t.Cleanup(func() { conn.Close() })

		f := &agentFilter{upstream: agent.NewClient(conn), pol: &policy{allow: allow}}
		keys, err := f.List()
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(keys) != 1 {
			t.Fatalf("List: got %d keys, want 1", len(keys))
		}
		if diff := cmp.Diff(wantBlob, keys[0].Blob); diff != "" {
			t.Fatalf("unexpected key blob (-want +got):\n%s", diff)
		}
	}

	t.Run("allowlist matches underlying key", func(t *testing.T) {
		run(t, map[string]struct{}{fpA: {}}, blobA)
	})
	t.Run("allowlist matches the other cert's base key", func(t *testing.T) {
		run(t, map[string]struct{}{fpB: {}}, blobB)
	})
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
		pol:      &policy{allow: allow},
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

// TestAgentOpLogHasKeyType verifies the debug trace of an op about a
// specific key carries the key type alongside its fingerprint (allowed
// and denied alike), while summary records keep the detail field.
func TestAgentOpLogHasKeyType(t *testing.T) {
	initTestKeys(t)
	// Allow only fp1: key 2's ops are denied and key 1's allowed, so
	// both directions of the per-key record are exercised.
	f := newTestFilter(t, map[string]struct{}{testKeys.fp1: {}})

	pub1, err := ssh.ParsePublicKey(testKeys.blob1)
	if err != nil {
		t.Fatalf("parse pub1: %v", err)
	}
	pub2, err := ssh.ParsePublicKey(testKeys.blob2)
	if err != nil {
		t.Fatalf("parse pub2: %v", err)
	}

	oldLog := log
	h := &captureHandler{}
	log = slog.New(h)
	defer func() { log = oldLog }()

	f.List()
	f.Sign(pub1, []byte("ok"))
	f.Sign(pub2, []byte("no"))
	f.Remove(pub1)

	recs := h.snapshot()
	type want struct {
		op, keyType, fingerprint, detail string
		allowed                          bool
		hasKey                           bool // true: type+fingerprint fields
	}
	wants := []want{
		{"list", "ssh-ed25519", testKeys.fp2, "", false, true},
		{"list", "", "", "1 of 2 keys allowed", true, false},
		{"sign", "ssh-ed25519", testKeys.fp1, "", true, true},
		{"sign", "ssh-ed25519", testKeys.fp2, "", false, true},
		{"remove", "ssh-ed25519", testKeys.fp1, "", false, true},
	}
	if len(recs) != len(wants) {
		t.Fatalf("got %d log records, want %d", len(recs), len(wants))
	}
	for i, w := range wants {
		r := recs[i]
		if r.Message != "flue: agent op" {
			t.Fatalf("record %d: message %q, want %q", i, r.Message, "flue: agent op")
		}
		var op, ktype, fp, detail string
		allowed, hasAllowed := false, false
		r.Attrs(func(a slog.Attr) bool {
			switch a.Key {
			case "op":
				op = a.Value.String()
			case "type":
				ktype = a.Value.String()
			case "fingerprint":
				fp = a.Value.String()
			case "detail":
				detail = a.Value.String()
			case "allowed":
				allowed, hasAllowed = a.Value.Bool(), true
			}
			return true
		})
		if !hasAllowed || allowed != w.allowed {
			t.Fatalf("record %d: allowed=%v (present=%v), want %v", i, allowed, hasAllowed, w.allowed)
		}
		if op != w.op {
			t.Fatalf("record %d: op=%q, want %q", i, op, w.op)
		}
		if w.hasKey {
			if ktype != w.keyType || fp != w.fingerprint {
				t.Fatalf("record %d: type=%q fingerprint=%q, want %q %q", i, ktype, fp, w.keyType, w.fingerprint)
			}
		} else if ktype != "" || fp != "" || detail != w.detail {
			t.Fatalf("record %d: summary record = type %q fp %q detail %q, want detail %q", i, ktype, fp, detail, w.detail)
		}
	}
}

// TestKeyTypesPlain verifies that a plain key matches only its own wire
// type.
func TestKeyTypesPlain(t *testing.T) {
	initTestKeys(t)
	pub, err := ssh.ParsePublicKey(testKeys.blob1)
	if err != nil {
		t.Fatalf("parse pub: %v", err)
	}
	if got := keyTypes(pub); !cmp.Equal(got, []string{"ssh-ed25519"}) {
		t.Fatalf("keyTypes = %v, want [ssh-ed25519]", got)
	}
}

// TestKeyTypesCertificate verifies that a certificate matches both its
// certificate type and its underlying key's type.
func TestKeyTypesCertificate(t *testing.T) {
	ak, _, _ := newCertKey(t, "cert")
	want := []string{"ssh-ed25519-cert-v01@openssh.com", "ssh-ed25519"}
	if got := keyTypes(ak.Certificate); !cmp.Equal(got, want) {
		t.Fatalf("keyTypes = %v, want %v", got, want)
	}
}

// TestKeyTypesFromBlob verifies the blob variant: a certificate blob
// yields both names, an unparsable blob yields nil.
func TestKeyTypesFromBlob(t *testing.T) {
	_, certBlob, _ := newCertKey(t, "cert")
	want := []string{"ssh-ed25519-cert-v01@openssh.com", "ssh-ed25519"}
	if got := keyTypesFromBlob(certBlob); !cmp.Equal(got, want) {
		t.Fatalf("keyTypesFromBlob = %v, want %v", got, want)
	}
	if got := keyTypesFromBlob([]byte("not a key blob")); got != nil {
		t.Fatalf("unparsable blob: keyTypesFromBlob = %v, want nil", got)
	}
}

// TestTypeAllowed verifies the --allow-key-type predicate: an empty set
// allows everything; otherwise a key passes when any of its type names is
// listed — including, for a certificate, through either name.
func TestTypeAllowed(t *testing.T) {
	_, certBlob, _ := newCertKey(t, "cert")
	certTypes := keyTypesFromBlob(certBlob)
	plainTypes := []string{"ssh-ed25519"}
	one := func(s string) map[string]struct{} { return map[string]struct{}{s: {}} }
	cases := []struct {
		name  string
		allow map[string]struct{}
		types []string
		want  bool
	}{
		{"empty set allows all", nil, certTypes, true},
		{"underlying type matches cert", one("ssh-ed25519"), certTypes, true},
		{"cert type matches cert", one("ssh-ed25519-cert-v01@openssh.com"), certTypes, true},
		{"other type denies cert", one("ecdsa-sha2-nistp256"), certTypes, false},
		{"cert-only set denies plain key", one("ssh-ed25519-cert-v01@openssh.com"), plainTypes, false},
		{"set set denies unparsable key", one("ssh-ed25519"), nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &agentFilter{pol: &policy{allowTypes: c.allow}}
			if got := f.typeAllowed(c.types); got != c.want {
				t.Fatalf("typeAllowed(%v) = %v, want %v", c.types, got, c.want)
			}
		})
	}
}

// oneSet is a test helper building a single-entry allowlist set.
func oneSet(s string) map[string]struct{} { return map[string]struct{}{s: {}} }

// TestFilterListType verifies List filters by key type over a keyring
// holding keys of two different types.
func TestFilterListType(t *testing.T) {
	initTestKeys(t)
	addr := startTypeKeyring(t)

	cases := []struct {
		name       string
		allowTypes map[string]struct{}
		want       [][]byte
	}{
		{"ed25519 only", oneSet("ssh-ed25519"), [][]byte{testKeys.blob1}},
		{"ecdsa only", oneSet("ecdsa-sha2-nistp256"), [][]byte{testKeys.blob3}},
		{"both types", map[string]struct{}{"ssh-ed25519": {}, "ecdsa-sha2-nistp256": {}}, [][]byte{testKeys.blob1, testKeys.blob3}},
		{"unlisted type allows none", oneSet("rsa"), nil},
		{"no type filter", nil, [][]byte{testKeys.blob1, testKeys.blob3}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newTestFilterFor(t, addr, nil, c.allowTypes)
			keys, err := f.List()
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			var got [][]byte
			for _, k := range keys {
				got = append(got, k.Blob)
			}
			if diff := cmp.Diff(c.want, got); diff != "" {
				t.Fatalf("unexpected keys (-want +got):\n%s", diff)
			}
		})
	}
}

// TestFilterSignType verifies Sign and SignWithFlags enforce the type
// list, independently of the fingerprint list.
func TestFilterSignType(t *testing.T) {
	initTestKeys(t)
	addr := startTypeKeyring(t)
	f := newTestFilterFor(t, addr, nil, oneSet("ssh-ed25519"))

	pub1, err := ssh.ParsePublicKey(testKeys.blob1)
	if err != nil {
		t.Fatalf("parse pub1: %v", err)
	}
	pub3, err := ssh.ParsePublicKey(testKeys.blob3)
	if err != nil {
		t.Fatalf("parse pub3: %v", err)
	}

	if _, err := f.Sign(pub1, []byte("ok")); err != nil {
		t.Fatalf("Sign ed25519 key: %v", err)
	}
	if _, err := f.SignWithFlags(pub1, []byte("ok"), 0); err != nil {
		t.Fatalf("SignWithFlags ed25519 key: %v", err)
	}
	if _, err := f.Sign(pub3, []byte("no")); err == nil {
		t.Fatal("Sign ecdsa key: expected denial, got nil error")
	}
	if _, err := f.SignWithFlags(pub3, []byte("no"), 0); err == nil {
		t.Fatal("SignWithFlags ecdsa key: expected denial, got nil error")
	}
}

// TestFilterCertificateType verifies a certificate matches both its
// certificate type and its underlying key's type in List: a list naming
// the underlying type admits the cert and the static key alike, while
// one naming only the certificate type admits the cert but not the
// static key.
func TestFilterCertificateType(t *testing.T) {
	initTestKeys(t)
	ak, certBlob, _ := newCertKey(t, "cert")

	run := func(t *testing.T, allowTypes map[string]struct{}) []*agent.Key {
		t.Helper()
		kr := agent.NewKeyring()
		for _, k := range []agent.AddedKey{{PrivateKey: testKeys.priv1, Comment: "static"}, ak} {
			if err := kr.Add(k); err != nil {
				t.Fatalf("add: %v", err)
			}
		}
		addr := serveKeyring(t, kr)
		conn, err := net.Dial("unix", addr)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		t.Cleanup(func() { conn.Close() })
		f := &agentFilter{upstream: agent.NewClient(conn), pol: &policy{allowTypes: allowTypes}}
		keys, err := f.List()
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		return keys
	}

	t.Run("underlying type admits cert and static key", func(t *testing.T) {
		if keys := run(t, oneSet("ssh-ed25519")); len(keys) != 2 {
			t.Fatalf("List: got %d keys, want 2", len(keys))
		}
	})

	t.Run("certificate type admits the cert but not the static key", func(t *testing.T) {
		keys := run(t, oneSet("ssh-ed25519-cert-v01@openssh.com"))
		if len(keys) != 1 {
			t.Fatalf("List: got %d keys, want 1 (the certificate)", len(keys))
		}
		if diff := cmp.Diff(certBlob, keys[0].Blob); diff != "" {
			t.Fatalf("wrong key returned (-want +got):\n%s", diff)
		}
	})
}

// TestFilterConjunction verifies --allow-key and --allow-key-type
// compose: a key must pass both lists to be usable.
func TestFilterConjunction(t *testing.T) {
	initTestKeys(t)
	addr := startTypeKeyring(t)

	pub1, err := ssh.ParsePublicKey(testKeys.blob1) // ed25519, fp1
	if err != nil {
		t.Fatalf("parse pub1: %v", err)
	}
	pub3, err := ssh.ParsePublicKey(testKeys.blob3) // ecdsa, fp3
	if err != nil {
		t.Fatalf("parse pub3: %v", err)
	}

	cases := []struct {
		name       string
		allow      map[string]struct{}
		allowTypes map[string]struct{}
		want1      bool // key1 (ed25519/fp1) signs
		want3      bool // key3 (ecdsa/fp3) signs
	}{
		{"both pass key1", map[string]struct{}{testKeys.fp1: {}}, oneSet("ssh-ed25519"), true, false},
		{"type fails both", map[string]struct{}{testKeys.fp1: {}}, oneSet("ecdsa-sha2-nistp256"), false, false},
		{"fingerprint fails both", map[string]struct{}{testKeys.fp3: {}}, oneSet("ssh-ed25519"), false, false},
		{"both pass key3", map[string]struct{}{testKeys.fp3: {}}, oneSet("ecdsa-sha2-nistp256"), false, true},
		{"type list only", nil, oneSet("ssh-ed25519"), true, false},
		{"fingerprint list only", map[string]struct{}{testKeys.fp1: {}}, nil, true, false},
		{"no lists", nil, nil, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newTestFilterFor(t, addr, c.allow, c.allowTypes)
			_, err1 := f.Sign(pub1, []byte("m"))
			got1 := err1 == nil
			_, err3 := f.Sign(pub3, []byte("m"))
			got3 := err3 == nil
			if got1 != c.want1 || got3 != c.want3 {
				t.Fatalf("Sign: key1 allowed=%v (want %v, err %v), key3 allowed=%v (want %v, err %v)",
					got1, c.want1, err1, got3, c.want3, err3)
			}
		})
	}
}

// TestAgentOpLogDenialReason verifies that a key an allowlist denies is
// logged with a reason field naming the violated list(s) by their flag
// names, so the operator knows which flag to adjust.
func TestAgentOpLogDenialReason(t *testing.T) {
	initTestKeys(t)
	pub1, err := ssh.ParsePublicKey(testKeys.blob1)
	if err != nil {
		t.Fatalf("parse pub1: %v", err)
	}

	build := func(allow, allowTypes map[string]struct{}) *agentFilter {
		return newTestFilterFor(t, startKeyringAgent(t), allow, allowTypes)
	}

	oldLog := log
	h := &captureHandler{}
	log = slog.New(h)
	defer func() { log = oldLog }()

	// Denied by the fingerprint list only.
	_, _ = build(map[string]struct{}{testKeys.fp2: {}}, nil).Sign(pub1, []byte("m"))
	// Denied by the type list only.
	_, _ = build(nil, oneSet("ecdsa-sha2-nistp256")).Sign(pub1, []byte("m"))
	// Denied by both lists.
	_, _ = build(map[string]struct{}{testKeys.fp2: {}}, oneSet("ecdsa-sha2-nistp256")).Sign(pub1, []byte("m"))

	recs := h.snapshot()
	if len(recs) != 3 {
		t.Fatalf("got %d log records, want 3", len(recs))
	}
	wants := []string{"allow-key", "allow-key-type", "allow-key,allow-key-type"}
	for i, w := range wants {
		r := recs[i]
		var op, reason string
		allowed, hasAllowed := false, false
		r.Attrs(func(a slog.Attr) bool {
			switch a.Key {
			case "op":
				op = a.Value.String()
			case "reason":
				reason = a.Value.String()
			case "allowed":
				allowed, hasAllowed = a.Value.Bool(), true
			}
			return true
		})
		if !hasAllowed || allowed {
			t.Fatalf("record %d: allowed=%v (present=%v), want false", i, allowed, hasAllowed)
		}
		if op != "sign" {
			t.Fatalf("record %d: op=%q, want sign", i, op)
		}
		if reason != w {
			t.Fatalf("record %d: reason=%q, want %q", i, reason, w)
		}
	}
}
