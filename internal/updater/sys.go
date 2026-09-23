package updater

import (
	"github.com/nikships/droidproxy-omarchy/internal/desktop"
	"github.com/nikships/droidproxy-omarchy/internal/paths"
)

// Hook points for the restart path, overridable in tests.
var (
	underSystemd       = desktop.UnderSystemd
	restartService     = desktop.Restart
	reExec             = syscallExec
	realUnderSystemd   = desktop.UnderSystemd
	realRestartService = desktop.Restart
)

// versionsDir is the directory holding one subdirectory per version.
func versionsDir() string { return paths.VersionsDir() }
