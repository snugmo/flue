package main

import (
	_ "embed"
)

// flueBrokerBin is the full flue-broker binary, embedded verbatim with
// go:embed from the build-time file cmd/flue/flue-broker.bin. It lets flue
// hand a container-side broker to a container whose image does not provide
// one (extract-broker, see extract.go).
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
// .gitignore and .containerignore). build.sh and the Containerfile produce
// it, and the Containerfile copies the *same file* to the image's
// /usr/local/bin/flue-broker, so a flue from the image extracts a broker
// identical to the image's own flue-broker.
//
// Architecture: the embedded broker is built for the same architecture as
// this flue binary (one source tree, one build).
//
//go:embed flue-broker.bin
var flueBrokerBin []byte
