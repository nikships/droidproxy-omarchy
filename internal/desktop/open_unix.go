//go:build !windows

package desktop

import (
	"context"
	"errors"
	"time"
)

// OpenPath opens path (a file, folder, or URL) with xdg-open, detached so a
// slow launcher cannot stall the daemon.
func OpenPath(path string) error {
	if path == "" {
		return errors.New("desktop: nothing to open")
	}
	return Default.StartDetached("xdg-open", path)
}

// CopyToClipboard copies text using wl-copy, falling back to xclip when
// wl-copy is missing (X11 sessions).
func CopyToClipboard(text string) error {
	if _, err := Default.LookPath("wl-copy"); err == nil {
		return copyWith("wl-copy", text)
	}
	if _, err := Default.LookPath("xclip"); err == nil {
		return copyWith("xclip", "-selection", "clipboard", text)
	}
	return errors.New("desktop: no clipboard tool found (install wl-copy or xclip)")
}

func copyWith(name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := Default.Run(ctx, name, args...)
	return err
}
