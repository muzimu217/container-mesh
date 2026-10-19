// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package peer

import (
	"fmt"
	"os"
)

// publishNoClobber moves part to final without overwriting an existing file.
//
// The naive os.Rename silently replaces the destination, so a peer could make
// the receiver destroy an unrelated file that happens to share the name.
// Linking first is atomic on POSIX (link fails with EEXIST if the target
// exists); the fallback covers filesystems that do not support hard links.
func publishNoClobber(part, final string, force bool) error {
	if force {
		// The operator asked for replacement; rename is atomic and clobbers.
		if err := os.Rename(part, final); err != nil {
			return fmt.Errorf("peer: rename: %w", err)
		}
		return nil
	}

	if err := os.Link(part, final); err == nil {
		// The data is now reachable under both names; drop the temporary one.
		if rmErr := os.Remove(part); rmErr != nil && !os.IsNotExist(rmErr) {
			return fmt.Errorf("peer: remove part file after publish: %w", rmErr)
		}
		return nil
	} else if !os.IsExist(err) && !isLinkUnsupported(err) {
		return fmt.Errorf("peer: publish %s: %w", final, err)
	}

	// Either the destination exists (refuse) or linking is unsupported here.
	if _, statErr := os.Stat(final); statErr == nil {
		return fmt.Errorf("peer: refusing to overwrite existing file %s "+
			"(rerun with --force to replace it)", final)
	}
	if err := os.Rename(part, final); err != nil {
		return fmt.Errorf("peer: rename: %w", err)
	}
	return nil
}

// isLinkUnsupported reports whether an error from os.Link means the filesystem
// cannot create hard links (as opposed to the destination existing).
func isLinkUnsupported(err error) bool {
	// EPERM/ENOTSUP/EXDEV all mean "cannot link here"; the exact errno differs
	// across platforms and filesystems (FAT, some network mounts, Android).
	s := err.Error()
	for _, marker := range []string{
		"operation not permitted",
		"operation not supported",
		"not supported",
		"invalid cross-device link",
		"no such device or address",
	} {
		if containsFold(s, marker) {
			return true
		}
	}
	return false
}

func containsFold(s, sub string) bool {
	if len(sub) > len(s) {
		return false
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		match := true
		for j := 0; j < len(sub); j++ {
			a, b := s[i+j], sub[j]
			if a >= 'A' && a <= 'Z' {
				a += 'a' - 'A'
			}
			if b >= 'A' && b <= 'Z' {
				b += 'a' - 'A'
			}
			if a != b {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
