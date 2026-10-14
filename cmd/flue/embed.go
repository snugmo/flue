package main

import (
	_ "embed"
)

// flueBrokerBin is the full flue-broker binary, embedded verbatim (by the
// embed directive below) from the build-time file cmd/flue/flue-broker.bin.
// It lets flue hand a container-side broker to a container whose image does
// not provide one (extract-broker, see extract.go).
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
// .gitignore and .containerignore). The Containerfile's build stage
// produces it (per architecture, before that architecture's flue), and
// the image ships the flue with it embedded: a flue from the image
// extracts the broker it was built with, for the image's architecture.
// The Containerfile's brokers stage keeps a copy of each per-
// architecture file so the release workflow can attest the brokers
// for every architecture without running non-native binaries.
//
// Architecture: the embedded broker is built for the same architecture as
// this flue binary (one source tree, one build).
//
//go:embed flue-broker.bin
var flueBrokerBin []byte
