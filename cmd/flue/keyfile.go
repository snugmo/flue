package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"golang.org/x/crypto/ssh"
)

// loadAllowKeyFile reads a key allowlist from path — one public key per
// line, in the authorized_keys format ssh-keygen and ssh-add print (an
// optional trailing comment; blank lines and # comment lines are
// skipped; an OpenSSH option prefix on a line, as in a real
// authorized_keys, is allowed) — and returns the fingerprints of the
// keys it contains. They are the same values --allow-key takes,
// computed by keyFingerprint: for a certificate that is the underlying
// key's fingerprint, so a file of authorized_keys lines admits exactly
// the keys the corresponding --allow-key flags would. The fingerprints
// union into the --allow-key set (runChildWithAgent), so the file
// intersects with --allow-key-type exactly like the flags do. It fails
// closed: an unreadable file, a non-blank, non-comment line that does
// not parse as a key, or a file with no keys at all, is an error (naming
// the line), so a session never starts with a silently shrunken
// allowlist. ssh.ParseAuthorizedKey is called per line on purpose: on a
// multi-line input it skips lines it cannot parse, which would drop a
// mistyped key without a trace.
func loadAllowKeyFile(path string) (map[string]struct{}, error) {
	// #nosec G304 — --allow-key-file is the user's explicit choice of
	// allowlist; the file is read, never written.
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open key allowlist %s: %w", path, err)
	}
	defer func() {
		// The file is fully read before it closes, so a close error
		// cannot change the parsed set; note it anyway.
		if cerr := f.Close(); cerr != nil {
			log.Warn("flue: close key allowlist", "file", path, "err", cerr)
		}
	}()

	set := make(map[string]struct{})
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for lineNo := 1; sc.Scan(); lineNo++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line + "\n"))
		if err == nil && key == nil {
			err = fmt.Errorf("no key on the line")
		}
		if err != nil {
			return nil, fmt.Errorf("key allowlist %s, line %d: %v", path, lineNo, err)
		}
		set[keyFingerprint(key)] = struct{}{}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read key allowlist %s: %w", path, err)
	}
	if len(set) == 0 {
		return nil, fmt.Errorf("key allowlist %s: no keys (only blank lines and comments)", path)
	}
	return set, nil
}
