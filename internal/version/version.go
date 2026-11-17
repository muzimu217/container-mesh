// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

// Package version holds the build-time version of the cmesh binary.
//
// It exists as its own package because two packages need the version and
// they cannot both have it: internal/cli stamps it in with -ldflags and
// internal/mesh reports it in the @info verb, but internal/cli already
// imports internal/mesh, so the mesh package cannot import the cli one.
//
// The stamped value lives in internal/cli.Version (that is the path the
// build scripts use) and is handed over here at startup. A build that
// links internal/mesh without internal/cli keeps the default below, which
// is honest: "dev" says the version was not stamped.
package version

// Version is the cmesh build version. It is "dev" in an unstamped build.
var Version = "dev"
