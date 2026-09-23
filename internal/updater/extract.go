package updater

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/nikships/droidproxy-omarchy/internal/paths"
)

// extractTarGz unpacks tarball into versionsDir. See ExtractTarGz for the
// safety rules.
func extractTarGz(tarball, versionsDir, version string) error {
	f, err := os.Open(tarball)
	if err != nil {
		return err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("not a gzip file: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	topDir := "droidproxy-" + version + "-" + AssetPlatform()
	destRoot := filepath.Join(versionsDir, version)
	createdTop := false

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name := hdr.Name

		switch hdr.Typeflag {
		case tar.TypeDir:
			// Top-level directory check happens below; nested dirs are made
			// implicitly by regular file creation, but allow explicit ones.
		case tar.TypeReg:
		case tar.TypeSymlink, tar.TypeLink:
			// Symlinks could point a later-executed file anywhere; refuse.
			return fmt.Errorf("refusing link entry in release tarball: %s", name)
		default:
			return fmt.Errorf("refusing special file in release tarball: %s (type %d)", name, hdr.Typeflag)
		}

		// Normalize and validate the destination path.
		clean := filepath.Clean("/" + name) // makes "/" the root, drops ..
		rel := strings.TrimPrefix(clean, "/")
		if rel == "" || rel == "." {
			continue
		}
		parts := strings.Split(rel, string(filepath.Separator))
		if parts[0] != topDir {
			return fmt.Errorf("unexpected top-level entry %q, want %q", parts[0], topDir)
		}
		if filepath.Clean(filepath.Join(destRoot, strings.Join(parts[1:], string(filepath.Separator)))) == destRoot {
			continue // the top dir itself
		}
		if strings.Contains(rel, "..") {
			return fmt.Errorf("entry escapes extraction root: %s", name)
		}

		if hdr.Typeflag == tar.TypeDir {
			if err := paths.EnsureDir(filepath.Join(destRoot, filepath.Join(parts[1:]...)), 0o755); err != nil {
				return err
			}
			createdTop = true
			continue
		}

		// Regular file. Ensure the parent exists, then write with the mode
		// from the archive (but never setuid/setgid).
		mode := os.FileMode(hdr.Mode & 0o777)
		if mode == 0 {
			mode = 0o644
		}
		dst := filepath.Join(destRoot, filepath.Join(parts[1:]...))
		if err := paths.EnsureDir(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, tr); err != nil {
			out.Close()
			return err
		}
		if err := out.Close(); err != nil {
			return err
		}
		createdTop = true
	}

	if !createdTop {
		return errors.New("release tarball is empty")
	}
	return nil
}
