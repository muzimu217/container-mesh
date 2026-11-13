// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package peer

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Directory support is implemented as a tar stream rather than as new protocol
// frames: a directory is packed on the fly, sent as one ordinary file, and
// unpacked by the receiver. That keeps the wire format unchanged (no version
// bump, no new failure modes on the relay) and gives resume for free at the
// archive level.
//
// The archive name is "<base>.cmeshtar" so the receiver knows to unpack it.

// archiveSuffix marks a transferred file as a packed directory.
const archiveSuffix = ".cmeshtar"

// IsArchiveName reports whether a received file should be unpacked.
func IsArchiveName(name string) bool { return strings.HasSuffix(name, archiveSuffix) }

// ArchiveName builds the wire name for a directory.
func ArchiveName(dir string) string {
	return filepath.Base(filepath.Clean(dir)) + archiveSuffix
}

// PackDir streams dir into w as a tar archive.
//
// Only regular files and directories are archived; symlinks, devices and
// sockets are skipped, because extracting them on the receiver would either
// escape the target directory (symlinks) or require privileges (devices).
// The number of bytes written is returned so the caller can report progress
// against the same total it advertised.
func PackDir(dir string, w io.Writer) (int64, error) {
	base := filepath.Base(filepath.Clean(dir))
	tw := tar.NewWriter(w)

	var written int64
	err := filepath.Walk(dir, func(p string, fi os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil // the root itself is implicit
		}
		// Archive paths always use forward slashes and are rooted at the
		// directory's base name, so extracting "photos/x.jpg" recreates the
		// directory the user sent.
		name := path.Join(base, filepath.ToSlash(rel))

		switch {
		case fi.IsDir():
			hdr := &tar.Header{
				Typeflag: tar.TypeDir,
				Name:     name + "/",
				// The sender's own bits travel with the entry: a private
				// directory must arrive private, not widened to 0755 by a
				// hardcoded default (doubao round 53 — the pack side was
				// discarding the mode the receiver then had to guess).
				Mode:    int64(fi.Mode().Perm()),
				ModTime: fi.ModTime(),
			}
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
		case fi.Mode().IsRegular():
			hdr := &tar.Header{
				Typeflag: tar.TypeReg,
				Name:     name,
				Mode:     int64(fi.Mode().Perm()),
				Size:     fi.Size(),
				ModTime:  fi.ModTime(),
			}
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			n, cpErr := io.CopyN(tw, f, fi.Size())
			closeErr := f.Close()
			written += n
			if cpErr != nil {
				return cpErr
			}
			if closeErr != nil {
				return closeErr
			}
		default:
			// Symlinks, devices, sockets: skipped deliberately.
			return nil
		}
		return nil
	})
	if err != nil {
		return written, err
	}
	if err := tw.Close(); err != nil {
		return written, err
	}
	return written, nil
}

// EstimateDirSize walks dir and returns the total size of the regular files
// that will be archived. It is used to advertise a size before packing.
func EstimateDirSize(dir string) (int64, error) {
	var total int64
	err := filepath.Walk(dir, func(_ string, fi os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if fi.Mode().IsRegular() {
			total += fi.Size()
		}
		return nil
	})
	return total, err
}

// UnpackArchive extracts a tar archive into dir.
//
// Every entry is validated before anything is written: absolute paths, ".."
// components and symlinks are refused, so a malicious sender cannot write
// outside the destination. Files that already exist are only replaced when
// overwrite is true.
//
// Permissions follow the sender's own bits, tightened only (see
// archiveFileMode): a private file or directory must not arrive wider than
// it was sent (oh-no-agent round 53, doubao round 53).
func UnpackArchive(archive string, dir string, overwrite bool) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()

	dirModes, err := extractEntries(tar.NewReader(f), dir, overwrite)
	if err != nil {
		return err
	}
	// Directory modes are applied only after every entry landed: a
	// directory sent as 0500/0400 would otherwise refuse the writes that
	// still had to happen inside it. Deepest first, so a parent that ends
	// up non-traversable cannot block the chmod of its children.
	for i := len(dirModes) - 1; i >= 0; i-- {
		if err := os.Chmod(dirModes[i].path, dirModes[i].mode); err != nil {
			return fmt.Errorf("peer: set mode %s: %w", dirModes[i].path, err)
		}
	}
	return nil
}

// dirMode is a directory whose final permissions are applied after the
// extraction pass.
type dirMode struct {
	path string
	mode os.FileMode
}

// extractEntries writes every archive entry below dir and returns the
// directories whose final mode was deferred.
func extractEntries(tr *tar.Reader, dir string, overwrite bool) ([]dirMode, error) {
	var dirModes []dirMode
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return dirModes, nil
		}
		if err != nil {
			return nil, fmt.Errorf("peer: read archive: %w", err)
		}

		clean, err := safeArchivePath(hdr.Name)
		if err != nil {
			return nil, err
		}
		target := filepath.Join(dir, clean)

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return nil, fmt.Errorf("peer: mkdir %s: %w", target, err)
			}
			// Owner rwx during the extraction (the final mode may forbid
			// writes), then the exact mode afterwards.
			create := archiveFileMode(os.FileMode(hdr.Mode)) | 0o700
			if err := os.Chmod(target, create); err != nil {
				return nil, fmt.Errorf("peer: set mode %s: %w", target, err)
			}
			dirModes = append(dirModes, dirMode{path: target, mode: archiveFileMode(os.FileMode(hdr.Mode))})
		case tar.TypeReg:
			if !overwrite {
				if _, statErr := os.Stat(target); statErr == nil {
					return nil, fmt.Errorf("peer: refusing to overwrite existing file %s", target)
				}
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return nil, err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, archiveFileMode(os.FileMode(hdr.Mode)))
			if err != nil {
				return nil, fmt.Errorf("peer: create %s: %w", target, err)
			}
			// The umask can only REMOVE bits, so a private header mode
			// (0700/0600) written through archiveFileMode's exec-bit answer
			// lands WIDER than sent (oh-no-agent round 53: 0700 → 0755).
			// Re-state the exact permitted mode now that the file exists —
			// chmod is not umask-filtered. This is a tightening move only:
			// archiveFileMode already refused setuid/group-write, so the
			// chmod never adds what OpenFile did not grant.
			if err := out.Chmod(archiveFileMode(os.FileMode(hdr.Mode))); err != nil {
				_ = out.Close()
				return nil, fmt.Errorf("peer: set mode %s: %w", target, err)
			}
			if _, err := io.Copy(out, tr); err != nil {
				_ = out.Close()
				return nil, fmt.Errorf("peer: write %s: %w", target, err)
			}
			if err := out.Close(); err != nil {
				return nil, err
			}
		case tar.TypeSymlink, tar.TypeLink:
			// Refused: a link can point outside the destination.
			return nil, fmt.Errorf("peer: archive contains a link (%s); refused", hdr.Name)
		default:
			// Other types (devices, fifos) are skipped rather than extracted.
			continue
		}
	}
}

// safeArchivePath validates an archive entry name and returns it in OS form.
func safeArchivePath(name string) (string, error) {
	if name == "" {
		return "", errors.New("peer: empty archive entry")
	}
	// Reject Windows-style separators and absolute paths outright.
	if strings.Contains(name, `\`) {
		return "", fmt.Errorf("peer: archive entry uses backslashes: %q", name)
	}
	if path.IsAbs(name) || strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("peer: archive entry is absolute: %q", name)
	}
	clean := path.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("peer: archive entry escapes the destination: %q", name)
	}
	return filepath.FromSlash(clean), nil
}

// packDirToTemp writes dir into a temporary tar file and returns its path.
// The caller removes it.
func packDirToTemp(dir string) (string, error) {
	f, err := os.CreateTemp("", "cmesh-pack-*.cmeshtar")
	if err != nil {
		return "", fmt.Errorf("peer: create temp archive: %w", err)
	}
	name := f.Name()
	if _, err := PackDir(dir, f); err != nil {
		_ = f.Close()
		_ = os.Remove(name)
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	return name, nil
}

// archiveFileMode picks the on-disk mode for an unpacked entry. Regular
// files keep their executable bit — scripts and binaries sent as a
// directory must stay runnable (netns-probe round 37, R37-1: the old
// blanket &0o644 silently stripped +x).
//
// The mode is the sender's permission bits masked down, never remapped:
// setuid/setgid/sticky never survive, and group/world WRITE never survives
// (the header is attacker controllable and the receiver runs as an
// ordinary user) — but private READ/EXEC scope does: a 0700 script must
// land as 0700, not be widened to 0755 (oh-no-agent round 53; the old
// exec-bit→0755 remap also made 0600 land as 0644, world-readable).
// Masking only removes bits, so the result is never wider than sent.
func archiveFileMode(m os.FileMode) os.FileMode {
	return m.Perm() & 0o755 &^ 0o022
}
