// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package cli

// `cmesh verify` — check a downloaded file against a published checksum.
//
// The product's whole pitch is "no untrusted middleman", so the one step a
// user MUST do after downloading a binary — confirm it is the one the
// project published — deserves a first-class command instead of a line of
// sha256sum the README asks people to type (and most people skip).
//
// This command only READS local files. It never downloads and never
// executes what it verifies.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func runVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	file := fs.String("file", "", "path of the file to verify")
	checksums := fs.String("checksums", "", "path of a SHA256SUMS file (the format sha256sum prints)")
	expect := fs.String("expect", "", "name to look up in the checksums file (default: the file's own name)")
	want := fs.String("sha256", "", "expected digest, instead of a checksums file")
	if err := fs.Parse(args); err != nil {
		return ExitError{Code: 2, Err: err}
	}
	if *file == "" {
		return ExitError{Code: 2, Err: errors.New(
			"usage: cmesh verify --file PATH [--checksums SHA256SUMS] [--expect NAME] | --sha256 HEX")}
	}

	sum, err := sha256File(*file)
	if err != nil {
		return ExitError{Code: 1, Err: fmt.Errorf("cannot read %s: %w", *file, err)}
	}

	switch {
	case *want != "":
		expected := strings.ToLower(strings.TrimSpace(*want))
		if expected != sum {
			return ExitError{Code: 1, Err: fmt.Errorf(
				"checksum mismatch for %s\n  expected %s\n  actual   %s\nThe file does NOT match the published digest — do not run it",
				*file, expected, sum)}
		}
		fmt.Printf("ok  %s\n  sha256 %s\n", *file, sum)
		return nil

	case *checksums != "":
		name := *expect
		if name == "" {
			name = filepath.Base(*file)
		}
		expected, err := lookupChecksum(*checksums, name)
		if err != nil {
			return ExitError{Code: 1, Err: err}
		}
		if expected != sum {
			return ExitError{Code: 1, Err: fmt.Errorf(
				"checksum mismatch for %s\n  expected %s (%s)\n  actual   %s\nThe file does NOT match the published digest — do not run it",
				*file, expected, name, sum)}
		}
		fmt.Printf("ok  %s\n  sha256 %s\n  matches %s\n", *file, sum, name)
		return nil

	default:
		return ExitError{Code: 2, Err: errors.New(
			"nothing to verify against: pass --checksums SHA256SUMS or --sha256 HEX")}
	}
}

// lookupChecksum finds one file's digest in a sha256sum-format manifest.
// The format is "<hex>  <name>", and the name may carry a "*" marker for
// binary mode; both are accepted.
func lookupChecksum(path, name string) (string, error) {
	blob, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("cannot read the checksums file %s: %w", path, err)
	}
	for _, line := range strings.Split(string(blob), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		if strings.TrimPrefix(fields[1], "*") == name {
			return strings.ToLower(fields[0]), nil
		}
	}
	return "", fmt.Errorf("%s does not list %s", path, name)
}

// sha256File hashes a local file.
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
