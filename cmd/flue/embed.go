package main

import (
	_ "embed"
)

// flueBrokerBin is the full flue-broker binary, embedded verbatim (by the
// embed directive below) from the build-time file cmd/flue/flue-broker.bin.
// It lets flue hand a container-side broker to a container whose image does
// not provide one (flue broker executable, see extract.go).
//
// Identity: go:embed stores the file's bytes untransformed, so the embedded
// copy is 100% identical to the flue-broker that was built as its source.
// That file must therefore be produced by building flue-broker *before*
// compiling flue:
//
//	CGO_ENABLED=0 go build -ldflags="-s -w" -o cmd/flue/flue-broker.bin \
//	  ./cmd/flue-broker
//
// If the file is missing, compiling this package fails with "no matching
// files found" — a deliberate hard failure, so a flue built without its
// broker is never produced silently. The file is a build artifact (see
// .gitignore and .containerignore). In the Containerfile the broker
// is built in its own stage and staged in a single flue-broker
// scratch stage (one per target platform); the flue build stage
// places that exact file on the embed path before compiling the
// platform's flue, and the release workflow attests the same files
// — so the attested broker is the one injected at build time, and
// a flue from the image extracts the broker it was built with, for
// the image's architecture.
//
// Architecture: the embedded broker is built for the same architecture as
// this flue binary (one source tree, one build).
//
//go:embed flue-broker.bin
var flueBrokerBin []byte

// flueBrokerBundle is the sigstore bundle for the embedded flue-broker
// (the flueBrokerBin above): the cosign signature, signing certificate,
// and certificate chain for the broker's bytes, embedded verbatim from
// the build-time file cmd/flue/flue-broker.sigstore. Together with the
// embedded binary it is how an extracted broker is verified *offline*
// with cosign (verify-blob --bundle): the broker and its bundle always
// travel together, inside every flue.
//
// Sources, in order of preference:
//
//   - A release build (the Containerfile, from the release workflow):
//     the bundle the workflow signed for this platform's broker is
//     placed in the build context at the embed path before the build
//     stage runs; the build stage's context copy wins over the
//     placeholder (see the Containerfile).
//   - A locally signed build: the user signs the built broker —
//
//	cosign sign-blob --key <key> \
//	  --bundle cmd/flue/flue-broker.sigstore cmd/flue/flue-broker.bin
//
//	(or --keyless where supported) — before compiling flue.
//   - A placeholder: when no signed bundle is present, the committed
//     template cmd/flue/flue-broker.sigstore.placeholder (its "kind"
//     field is flueLocalPlaceholderKind, see bundle.go) is copied to
//     the embed path — by the host Build/Test commands in AGENTS.md and
//     by the Containerfile's build stage (a cp -n fallback) — so a
//     plain local build never fails on the missing file. The
//     placeholder is clearly labeled as such: flue broker sigstore-bundle
//     warns when it writes one, and cosign cannot verify it (it carries
//     no signature).
//
// If the file is missing at compile time, the build fails with "no
// matching files found" — the same deliberate hard failure as the
// broker binary: a flue whose embed source is absent is never produced
// silently. The file is a build artifact (see .gitignore); unlike the
// broker binary it is *not* kept out of the image build context
// (.containerignore), because it is a build-context input the build
// stage consumes.
//
//go:embed flue-broker.sigstore
var flueBrokerBundle []byte
