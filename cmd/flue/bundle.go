package main

import (
	"encoding/json"

	"github.com/spf13/cobra"
)

// brokerBundleCmd is the broker sigstore-bundle subcommand
// (flue broker sigstore-bundle): it writes the sigstore bundle embedded
// in flue (see embed.go) out — to <filename>, mode 0644, when a
// filename is given, and to stdout when none is (or when it is "-") —
// byte-identical to the bundle that was embedded when flue was built.
// Together with broker executable it is how an extracted broker is
// verified offline with cosign: the bundle carries the cosign
// signature, signing certificate, and chain for the broker's bytes —
// no registry or network lookup needed at verify time.
var brokerBundleCmd = &cobra.Command{
	Use:   "sigstore-bundle [filename]",
	Short: "Write the embedded flue-broker sigstore bundle to a file or stdout",
	Long: `sigstore-bundle writes the sigstore bundle embedded in this flue
binary — the cosign signature, signing certificate, and chain for the
embedded flue-broker — byte-identical to the bundle that was embedded
when flue was built. With <filename> it writes to that file, mode
0644; with no filename, or with "-", it goes to stdout, so the bundle
can be piped or redirected.

Extracted together with the broker, the pair verifies offline:

	flue broker executable ./flue-broker
	flue broker sigstore-bundle ./flue-broker.sigstore.json
	cosign verify-blob ./flue-broker --bundle ./flue-broker.sigstore.json \
	  --certificate-identity "<identity>" \
	  --certificate-oidc-issuer "<issuer>"

In a release build the release workflow signs the broker it builds
(Sigstore keyless: the certificate's identity is the workflow file at
the pushed ref, under the GitHub Actions OIDC issuer); in a local
build it is either a bundle you signed the broker with (any cosign
key, or keyless where supported) or the placeholder (see below).

For a keyless release bundle the identity is the workflow URI (see the
README's verify section) and the issuer is
https://token.actions.githubusercontent.com; for a self-signed bundle
pass --key <your public key> instead of the identity flags.

When the embedded bundle is the placeholder — a flue built outside the
release workflow without signing the broker first — a warning is
printed: the bundle carries no signature and cannot verify the broker
that broker executable writes.

Unlike exec and enter, this subcommand needs no agent socket.`,
	Args: cobra.RangeArgs(0, 1),
	Run: func(cmd *cobra.Command, args []string) {
		path := "-"
		if len(args) == 1 {
			path = args[0]
		}
		if path != "-" {
			if err := writeEmbeddedBundle(path); err != nil {
				fatalf("flue: broker sigstore-bundle: %v", err)
			}
			return
		}
		warnPlaceholderBundle()
		if _, err := cmd.OutOrStdout().Write(flueBrokerBundle); err != nil {
			fatalf("flue: broker sigstore-bundle: %v", err)
		}
	},
}

// flueLocalPlaceholderKind is the value of the "kind" field in the
// committed placeholder bundle template
// (cmd/flue/flue-broker.sigstore.json.placeholder): the marker that
// distinguishes the unsigned local build's placeholder from a real
// sigstore bundle (which uses a different kind, e.g. "Bundle").
const flueLocalPlaceholderKind = "FlueLocalPlaceholder"

// isPlaceholderBundle reports whether b is the placeholder bundle rather
// than a real sigstore bundle: it parses as a JSON object whose "kind"
// field is flueLocalPlaceholderKind. Anything that does not parse, or
// parses with any other kind, is treated as a real bundle — cosign is
// the authority on what a real bundle is; this is only the warning
// heuristic for the placeholder the build produces (see embed.go).
func isPlaceholderBundle(b []byte) bool {
	var m struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return false
	}
	return m.Kind == flueLocalPlaceholderKind
}

// warnPlaceholderBundle logs the placeholder warning for the embedded
// sigstore bundle: when the embedded bundle is the placeholder (a flue
// built without a release or self-signed broker signature), the bundle
// the command writes or prints carries no signature and will not
// verify the broker broker executable writes. It is logged (stderr, or
// --log-file) rather than written to the bundle's destination, so the
// stdout form stays clean for piping.
func warnPlaceholderBundle() {
	if isPlaceholderBundle(flueBrokerBundle) {
		log.Warn("flue: the embedded broker bundle is the local placeholder (no release or self-signed broker signature): the bundle will not verify the extracted broker with cosign")
	}
}

// writeEmbeddedBundle writes the embedded sigstore bundle to path,
// atomically (a temp file in the target directory, then rename) with
// mode 0644. It refuses to overwrite a directory. The stdout form (no
// filename, or "-") is handled by the command writing to
// cmd.OutOrStdout() directly, with warnPlaceholderBundle doing the
// same warning first.
func writeEmbeddedBundle(path string) error {
	warnPlaceholderBundle()
	return writeFileAtomic(path, flueBrokerBundle, 0o644)
}
