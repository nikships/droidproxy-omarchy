package updater

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// extract unpacks the release tarball into paths.VersionsDir().
//
// Safety rules (a release tarball comes from the network after all):
//   - the archive must have exactly one top-level directory named
//     droidproxy-<version>-<platform>
//   - entry names may not escape via "..", absolute paths, or symlinks
//   - hardlinks and special files are refused
func ExtractTarGz(tarball, versionsDir, version string) error {
	return extractTarGz(tarball, versionsDir, version)
}

// pruneOldVersions keeps the given current version plus the most recent
// previous one and deletes the rest (Sparkle keeps no history; we keep one
// for Rollback).
func pruneOldVersions(current string) error {
	dir := versionsDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var old []string
	for _, e := range entries {
		if !e.IsDir() || e.Name() == current {
			continue
		}
		// Only prune things that look like version directories; never
		// delete "current" (a symlink) or strays.
		if _, err := os.ReadDir(filepath.Join(dir, e.Name())); err != nil {
			continue
		}
		if CompareVersions(e.Name(), current) > 0 {
			// Newer than what we just installed? Leave it alone.
			continue
		}
		old = append(old, e.Name())
	}
	// Newest first, then keep exactly one.
	sort.Slice(old, func(i, j int) bool { return CompareVersions(old[i], old[j]) > 0 })
	for i, name := range old {
		if i == 0 {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, name)); err != nil {
			return fmt.Errorf("remove %s: %w", name, err)
		}
	}
	return nil
}
