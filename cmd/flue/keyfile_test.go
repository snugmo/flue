package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"golang.org/x/crypto/ssh"
)

// writeAllowlist writes content to a fresh file in the test's temp
// directory and returns its path.
func writeAllowlist(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "authorized_keys")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write allowlist: %v", err)
	}
	return path
}

// authLine returns the authorized_keys line for a stored test key blob.
func authLine(t *testing.T, blob []byte) string {
	t.Helper()
	pub, err := ssh.ParsePublicKey(blob)
	if err != nil {
		t.Fatalf("parse pub: %v", err)
	}
	return string(ssh.MarshalAuthorizedKey(pub))
}

// TestLoadAllowKeyFile verifies the authorized_keys parser: plain lines,
// comments and blank lines, CRLF endings, option prefixes, and
// certificates (fingerprinted as their underlying key) all come out as
// the same fingerprint values --allow-key takes.
func TestLoadAllowKeyFile(t *testing.T) {
	initTestKeys(t)
	line1 := authLine(t, testKeys.blob1)
	line2 := authLine(t, testKeys.blob2)
	line3 := authLine(t, testKeys.blob3)
	// strip the trailing newline the marshaller appends
	ln1, ln2 := strings.TrimSuffix(line1, "\n"), strings.TrimSuffix(line2, "\n")
	// newCertKey's blob is the wire encoding; a file holds the
	// authorized_keys line of the same certificate
	_, certBlob, certFP := newCertKey(t, "file-cert")
	parsedCert, err := ssh.ParsePublicKey(certBlob)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	certLine := string(ssh.MarshalAuthorizedKey(parsedCert))

	cases := []struct {
		name    string
		content string
		want    []string
	}{
		{"single key", line1, []string{testKeys.fp1}},
		{"comments and blank lines skipped",
			"# a comment\n\n" + line1 + "\n# another\n" + line2,
			[]string{testKeys.fp1, testKeys.fp2}},
		{"CRLF line endings", ln1 + "\r\n" + ln2 + "\r\n", []string{testKeys.fp1, testKeys.fp2}},
		{"option prefix", "command=/bin/true,no-pty " + ln1, []string{testKeys.fp1}},
		{"certificate fingerprints its underlying key", certLine, []string{certFP}},
		{"union of several keys", line1 + line2 + line3, []string{testKeys.fp1, testKeys.fp2, testKeys.fp3}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			set, err := loadAllowKeyFile(writeAllowlist(t, c.content))
			if err != nil {
				t.Fatalf("loadAllowKeyFile: %v", err)
			}
			want := make(map[string]struct{}, len(c.want))
			for _, fp := range c.want {
				want[fp] = struct{}{}
			}
			if diff := cmp.Diff(want, set); diff != "" {
				t.Fatalf("unexpected fingerprints (-want +got):\n%s", diff)
			}
		})
	}
}

// TestLoadAllowKeyFileErrors pins the fail-closed behavior: every
// malformed input is an error that names the file and the line, and a
// file with no keys is an error too, so a session never starts with a
// silently shrunken allowlist.
func TestLoadAllowKeyFileErrors(t *testing.T) {
	initTestKeys(t)
	line1 := authLine(t, testKeys.blob1)
	// the blob says ssh-ed25519; a line that declares ssh-rsa must be
	// rejected, not silently kept
	mismatched := "ssh-rsa" + line1[len("ssh-ed25519"):]

	cases := []struct {
		name     string
		content  string
		path     string
		wantSubs []string
	}{
		{"missing file", "", "/nonexistent/no-such-allowlist", []string{"open key allowlist"}},
		{"unparseable line", line1 + "foo bar\n", "", []string{"line 2"}},
		{"single-token line", line1 + "garbage\n", "", []string{"line 2", "no key"}},
		{"key type mismatch", mismatched, "", []string{"line 1"}},
		{"only comments and blank lines", "# nothing here\n\n# still nothing\n", "", []string{"no keys"}},
		{"empty file", "", "", []string{"no keys"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := c.path
			if c.path == "" {
				path = writeAllowlist(t, c.content)
			}
			_, err := loadAllowKeyFile(path)
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			for _, sub := range c.wantSubs {
				if !strings.Contains(err.Error(), sub) {
					t.Errorf("error %q does not contain %q", err, sub)
				}
			}
		})
	}
}

// TestFilterSignAllowlistFile verifies the file end to end through the
// filter: keys named in the file are listed and signable, a key in the
// keyring that the file does not name is denied.
func TestFilterSignAllowlistFile(t *testing.T) {
	initTestKeys(t)
	line1 := authLine(t, testKeys.blob1)
	path := writeAllowlist(t, "# the file admits only key1\n"+line1+"\n")

	set, err := loadAllowKeyFile(path)
	if err != nil {
		t.Fatalf("loadAllowKeyFile: %v", err)
	}
	f := newTestFilter(t, set)

	keys, err := f.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("List: got %d keys, want 1", len(keys))
	}
	if diff := cmp.Diff(testKeys.blob1, keys[0].Blob); diff != "" {
		t.Fatalf("unexpected key blob (-want +got):\n%s", diff)
	}

	pub1, err := ssh.ParsePublicKey(testKeys.blob1)
	if err != nil {
		t.Fatalf("parse pub1: %v", err)
	}
	sig, err := f.Sign(pub1, []byte("test message"))
	if err != nil {
		t.Fatalf("Sign (key in the file): %v", err)
	}
	if sig == nil {
		t.Fatal("expected non-nil signature")
	}

	pub2, err := ssh.ParsePublicKey(testKeys.blob2)
	if err != nil {
		t.Fatalf("parse pub2: %v", err)
	}
	if _, err := f.Sign(pub2, []byte("test message")); err == nil {
		t.Fatal("expected signing a key not in the file to be denied")
	}
}

// TestFilterConjunctionFile verifies that a file's keys still intersect
// --allow-key-type the way --allow-key values do: with both set, a key
// must be in the file and match the type list.
func TestFilterConjunctionFile(t *testing.T) {
	initTestKeys(t)
	addr := startTypeKeyring(t)
	line1 := authLine(t, testKeys.blob1) // ed25519
	line3 := authLine(t, testKeys.blob3) // ecdsa
	path := writeAllowlist(t, line1+line3)

	set, err := loadAllowKeyFile(path)
	if err != nil {
		t.Fatalf("loadAllowKeyFile: %v", err)
	}

	pub1, err := ssh.ParsePublicKey(testKeys.blob1)
	if err != nil {
		t.Fatalf("parse pub1: %v", err)
	}
	pub3, err := ssh.ParsePublicKey(testKeys.blob3)
	if err != nil {
		t.Fatalf("parse pub3: %v", err)
	}

	for _, tc := range []struct {
		name       string
		allowTypes map[string]struct{}
		want1      bool // key1 (ed25519) signs
		want3      bool // key3 (ecdsa) signs
	}{
		{"type admits only key1's type", oneSet("ssh-ed25519"), true, false},
		{"type admits only key3's type", oneSet("ecdsa-sha2-nistp256"), false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTestFilterFor(t, addr, set, tc.allowTypes)
			_, err1 := f.Sign(pub1, []byte("m"))
			_, err3 := f.Sign(pub3, []byte("m"))
			got1, got3 := err1 == nil, err3 == nil
			if got1 != tc.want1 || got3 != tc.want3 {
				t.Fatalf("Sign: key1 allowed=%v (want %v, err %v), key3 allowed=%v (want %v, err %v)",
					got1, tc.want1, err1, got3, tc.want3, err3)
			}
		})
	}
}
