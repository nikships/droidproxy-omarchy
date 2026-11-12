package desktop

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"time"
)

// OpenPath opens path (a file, folder, or URL) with the shell's default
// handler, detached so a slow launcher cannot stall the daemon.
func OpenPath(path string) error {
	if path == "" {
		return errors.New("desktop: nothing to open")
	}
	return Default.StartDetached("rundll32.exe", "url.dll,FileProtocolHandler", path)
}

// CopyToClipboard copies text via PowerShell's Set-Clipboard. The text
// travels through an environment variable so quoting never mangles it.
func CopyToClipboard(text string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive",
		"-Command", "Set-Clipboard -Value $env:DROIDPROXY_CLIP_TEXT")
	cmd.Env = append(os.Environ(), "DROIDPROXY_CLIP_TEXT="+text)
	return cmd.Run()
}
