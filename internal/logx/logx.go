// Package logx is the NSLog replacement. Everything goes to stderr, which the
// systemd user unit routes to the journal (journalctl --user -u droidproxy).
package logx

import (
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/nikships/droidproxy-omarchy/internal/paths"
)

var std = log.New(os.Stderr, "", 0)

// Logf logs one line. Callers include their own "[Component]" prefix, matching
// the macOS app's NSLog convention.
func Logf(format string, args ...any) {
	std.Printf(format, args...)
}

var (
	debugMu   sync.Mutex
	debugFile *os.File
	debugPath string
)

// Debugf appends a timestamped line to the ThinkingProxy debug log
// (~/.local/state/droidproxy/droidproxy-debug.log). The file is created 0600
// because request lines can include model names and reasoning settings.
func Debugf(format string, args ...any) {
	line := fmt.Sprintf("[%s] %s\n", time.Now().Format(time.RFC3339), fmt.Sprintf(format, args...))
	debugMu.Lock()
	defer debugMu.Unlock()
	path := paths.DebugLogPath()
	if debugFile == nil || debugPath != path {
		if debugFile != nil {
			debugFile.Close()
		}
		if err := paths.EnsureDir(paths.StateDir(), 0o700); err != nil {
			return
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return
		}
		debugFile, debugPath = f, path
	}
	_, _ = debugFile.WriteString(line)
}
