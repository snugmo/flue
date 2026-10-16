package main

import (
	"github.com/spf13/cobra"
)

// brokerCmd is the parent of the subcommands that expose the
// flue-broker embedded in flue (see embed.go): `flue broker executable`
// (extract.go) writes the embedded broker out, `flue broker sha256`
// (extract.go) prints its SHA256, and `flue broker sigstore-bundle`
// (bundle.go) writes the embedded broker's sigstore bundle out. None
// needs an agent socket (none carries the needs-agent annotation): the
// embedded broker and its bundle are inspected and recovered without an
// SSH agent.
var brokerCmd = &cobra.Command{
	Use:   "broker",
	Short: "Expose the embedded flue-broker (executable, sha256, sigstore-bundle)",
	Long: `broker exposes the flue-broker embedded in this flue binary:

	flue broker executable <filename>
		Writes the embedded broker to <filename>, mode 0755,
		identical to the broker built when flue was built.

	flue broker sha256
		Prints the SHA256 of the embedded broker as a bare hex
		digest (the value sha256sum of the file executable
		writes), for comparing against another source.

	flue broker sigstore-bundle [filename]
		Writes the embedded broker's sigstore bundle (cosign
		signature, signing certificate, and chain) to
		<filename>, mode 0644 — or to stdout when no filename
		is given (or it is "-"). Together with executable it
		is how a recovered broker is verified offline (cosign
		verify-blob --bundle).

None of these subcommands needs an agent socket.`,
}
