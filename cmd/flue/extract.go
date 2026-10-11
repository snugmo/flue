package main

import (
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
binary, so a flue from the flue image extracts a broker identical to the
image's own /usr/local/bin/flue-broker.

Unlike exec and enter, this subcommand needs no agent socket.`,
	Args: cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		if err := writeEmbeddedBroker(args[0]); err != nil {
			fatalf("flue: extract-broker: %v", err)
		}
	},
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

	dir := filepath.Dir(path)
	if dir == "" {
		dir = "."
	}
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

	// Write to a temp file in the target directory and rename, so the
	// target is never left half-written, and the executable bit is set on
	// the temp file before it becomes the target.
	tmp, err := os.CreateTemp(dir, ".flue-broker-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %v", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename

	if _, err := tmp.Write(flueBrokerBin); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %v", path, err)
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod %s: %v", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %v", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename to %s: %v", path, err)
	}

	log.Info("flue: extracted broker", "path", path, "bytes", len(flueBrokerBin))
	return nil
}

// looksLikeELF reports whether b starts with the ELF magic bytes. It is a
// cheap sanity check on the embedded blob: it turns a flue built with a
// non-broker embed source into an error at extraction time, rather than
// at container runtime.
func looksLikeELF(b []byte) bool {
	return len(b) >= 4 && b[0] == 0x7f && b[1] == 'E' && b[2] == 'L' && b[3] == 'F'
}
