//go:build tools

// Package internal pins third-party modules shared by several packages so
// go.mod stays stable while packages are developed in parallel.
package internal

import (
	_ "github.com/fsnotify/fsnotify"
	_ "github.com/godbus/dbus/v5"
)
