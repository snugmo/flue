package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

// extractCmd is the extract-broker subcommand: it writes the flue-broker
// binary embedded in flue (see embed.go) to a file, byte-identical to the
// flue-broker that was built when flue was built.
var extractCmd = &cobra.Command{
	Use:   "extract-broker <filename>",
	Short: "Write the embedded flue-broker binary to a file",
	Long: `extract-broker writes the flue-broker binary embedded in this flue
binary to <filename>, mode 0755, byte-identical to the flue-broker that
was built when flue was built. It is how flue can place a broker in a
container whose image does not provide one: extract (on the host, or
inside the container for a flue from the flue image) and put the result
where it is needed.

The embedded broker was built for the same architecture as this flue
binary, so a flue from the flue image extracts a broker built for that
same architecture.

Unlike exec and enter, this subcommand needs no agent socket.`,
	Args: cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		if err := writeEmbeddedBroker(args[0]); err != nil {
			fatalf("flue: extract-broker: %v", err)
		}
	},
}

// brokerSha256Cmd is the broker-sha256 subcommand: it prints the SHA256
// of the embedded flue-broker (the same bytes extract-broker writes) as a
// bare hex digest — the value `sha256sum` of the extracted binary — so the
// embedded broker can be compared against any other source (a separately
// built flue-broker, an image's /usr/local/bin/flue-broker, another
// flue's embed) without writing the binary out. Like extract-broker it
// needs no agent socket.
var brokerSha256Cmd = &cobra.Command{
	Use:   "broker-sha256",
	Short: "Print the SHA256 of the embedded flue-broker",
	Args:  cobra.NoArgs,
	Run: func(cmd *cobra.Command, args []string) {
		sum, err := embeddedBrokerSha256()
		if err != nil {
			fatalf("flue: broker-sha256: %v", err)
		}
		if _, err := fmt.Fprintln(cmd.OutOrStdout(), sum); err != nil {
			fatalf("flue: broker-sha256: %v", err)
		}
	},
}

// embeddedBrokerSha256 returns the hex SHA256 of the embedded flue-broker
// bytes, or the same error writeEmbeddedBroker returns when the embedded
// blob is not a Linux ELF binary (the symptom of building flue without
// building flue-broker first, see embed.go).
func embeddedBrokerSha256() (string, error) {
	if !looksLikeELF(flueBrokerBin) {
		return "", errors.New("embedded flue-broker is not a Linux ELF binary: build flue-broker (to cmd/flue/flue-broker.bin) before building flue, then rebuild")
	}
	sum := sha256.Sum256(flueBrokerBin)
	return hex.EncodeToString(sum[:]), nil
}

// writeEmbeddedBroker writes the embedded flue-broker binary to path,
// atomically (a temp file in the target directory, then rename) with mode
// 0755. It refuses to overwrite a directory, and it fails if the embedded
// blob is not a Linux ELF binary — the symptom of building flue without
// building flue-broker first (see embed.go).
func writeEmbeddedBroker(path string) error {
	if !looksLikeELF(flueBrokerBin) {
		return errors.New("embedded flue-broker is not a Linux ELF binary: build flue-broker (to cmd/flue/flue-broker.bin) before building flue, then rebuild")
	}
	if err := writeFileAtomic(path, flueBrokerBin, 0o755); err != nil {
		return err
	}
	log.Info("flue: extracted broker", "path", path, "bytes", len(flueBrokerBin))
	return nil
}

// writeFileAtomic writes data to path atomically: a temp file in the
// target directory is written, chmod'd to mode, flushed to disk, and
// renamed onto path, so the target is never left half-written and the
// final mode is set on the temp file before it becomes the target
// (for the broker that is the executable bit, set before the rename).
// It refuses a target that is a directory and fails if the target
// directory does not exist. Shared by the extract-broker (0755) and
// broker-bundle (0644) writes.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path) // "." for a bare filename, never ""
	st, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("directory %s: %v", dir, err)
	}
	if !st.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	if st, err := os.Stat(path); err == nil && st.IsDir() {
		return fmt.Errorf("%s is a directory", path)
	}

	tmp, err := os.CreateTemp(dir, ".flue-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %v", dir, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op after a successful rename

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write %s: %v", path, err)
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod %s: %v", path, err)
	}
	// Flush to disk before the rename, so a crash cannot leave the target
	// name pointing at an empty or partial file.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync %s: %v", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %v", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename to %s: %v", path, err)
	}
	return nil
}

// looksLikeELF reports whether b starts with the ELF magic bytes. It is a
// cheap sanity check on the embedded blob: it turns a flue built with a
// non-broker embed source into an error at extraction time, rather than
// at container runtime.
func looksLikeELF(b []byte) bool {
	return len(b) >= 4 && b[0] == 0x7f && b[1] == 'E' && b[2] == 'L' && b[3] == 'F'
}
