// Package notify sends desktop notifications over the freedesktop
// org.freedesktop.Notifications D-Bus interface (the NSUserNotification
// replacement), falling back to notify-send when the session bus is
// unavailable. It never panics without a bus; the worst case is a logged
// line.
package notify

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/nikships/droidproxy-omarchy/internal/buildinfo"
	"github.com/nikships/droidproxy-omarchy/internal/logx"
	"github.com/nikships/droidproxy-omarchy/internal/paths"
)

const (
	dbusName      = "org.freedesktop.Notifications"
	dbusPath      = "/org/freedesktop/Notifications"
	actionInvoked = "org.freedesktop.Notifications.ActionInvoked"
	// NotificationClosed is also received for every closed notification.
	notificationClosed = "org.freedesktop.Notifications.NotificationClosed"
	// CloseTimeout means the server closed the notification without an action.
	CloseTimeout uint32 = 1
	// CloseDismissed means the user dismissed the notification.
	CloseDismissed uint32 = 2
	// CloseClosedByCall means the notification was closed by a CloseNotification call.
	CloseClosedByCall uint32 = 3
)

// Action is one button on the notification.
type Action struct {
	Key   string // the key reported to OnAction
	Label string // the user-visible label
}

// Options tweak a single notification.
type Options struct {
	Icon      string   // icon name or absolute path; "" uses the app icon
	Actions   []Action // buttons; requires OnAction for any effect
	OnAction  func(key string)
	OnClosed  func(reason uint32)
	TimeoutMs int32 // -1 server default, 0 never expires
}

// AppIcon returns the icon for notifications: the hicolor icon name
// "droidproxy" when any hicolor copy is installed (system or user), otherwise
// the absolute path of the bundled icon (notification daemons resolve names
// against the icon theme, so the name is preferable once installed).
func AppIcon() string {
	for _, p := range []string{
		"/usr/share/icons/hicolor/scalable/apps/droidproxy.svg",
		filepath.Join(paths.IconsDir(), "hicolor", "512x512", "apps", "droidproxy.png"),
		filepath.Join(paths.IconsDir(), "hicolor", "256x256", "apps", "droidproxy.png"),
	} {
		if _, err := os.Stat(p); err == nil {
			return "droidproxy"
		}
	}
	// Bundled copy from the release layout, not (yet) installed.
	for _, size := range []string{"512x512", "256x256", "128x128"} {
		p := filepath.Join(paths.ShareDir(), "icons", "hicolor", size, "apps", "droidproxy.png")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "droidproxy"
}

// sessionBus opens the session bus, returning a helper that reports the
// failure as an error instead of panicking.
func sessionBus() (*dbus.Conn, error) {
	return dbus.SessionBus()
}

// Disabled is a kill switch used by tests (and anyone who wants silent
// operation): when true, Notify reports success without sending anything.
// atomic because startServer notifies from a goroutine that can outlive a test.
var Disabled atomic.Bool

// Notify posts a desktop notification and returns the server-assigned id.
// Without a session bus it falls back to notify-send (actions are then lost)
// and returns 0 with a nil error; if notify-send is missing too, it returns
// an error.
func Notify(title, body string, opts Options) (uint32, error) {
	if Disabled.Load() {
		return 0, nil
	}
	conn, err := sessionBus()
	if err != nil {
		return notifySend(title, body)
	}
	defer func() { _ = conn.Close() }()

	obj := conn.Object(dbusName, dbusPath)

	// Register a signal match before calling so a fast action is not missed.
	if opts.OnAction != nil || opts.OnClosed != nil {
		if err := conn.AddMatchSignal(
			dbus.WithMatchObjectPath(dbus.ObjectPath(dbusPath)),
			dbus.WithMatchInterface("org.freedesktop.Notifications"),
		); err != nil {
			logx.Logf("[Notify] AddMatchSignal: %v", err)
		}
	}

	icon := opts.Icon
	if icon == "" {
		icon = AppIcon()
	}

	var actions []string
	if len(opts.Actions) > 0 && opts.OnAction != nil {
		for _, a := range opts.Actions {
			actions = append(actions, a.Key, a.Label)
		}
	}

	timeout := opts.TimeoutMs
	if timeout == 0 && (len(opts.Actions) > 0 && opts.OnAction != nil) {
		// Actionable notifications must not expire while the user reads.
		timeout = 0
	}

	var id uint32
	callErr := obj.Call("Notify", 0,
		buildinfo.AppName, // app_name
		uint32(0),         // replaces_id
		icon,
		title,
		body,
		actions,
		map[string]dbus.Variant{}, // hints
		int32(timeout),
	).Store(&id)
	if callErr != nil {
		logx.Logf("[Notify] D-Bus Notify failed: %v", callErr)
		return notifySend(title, body)
	}

	if opts.OnAction != nil || opts.OnClosed != nil {
		go watchSignals(conn, id, opts)
	}
	return id, nil
}

// watchSignals routes ActionInvoked/NotificationClosed for one notification
// until it closes or the connection drops.
func watchSignals(conn *dbus.Conn, id uint32, opts Options) {
	ch := make(chan *dbus.Signal, 16)
	conn.Signal(ch)
	defer func() {
		conn.Signal(nil)
		_ = conn.RemoveMatchSignal(
			dbus.WithMatchObjectPath(dbus.ObjectPath(dbusPath)),
			dbus.WithMatchInterface("org.freedesktop.Notifications"),
		)
	}()

	timeout := time.After(24 * time.Hour)
	for {
		select {
		case sig, ok := <-ch:
			if !ok {
				return
			}
			switch sig.Name {
			case actionInvoked:
				if len(sig.Body) >= 2 {
					if sid, ok := sig.Body[0].(uint32); ok && sid == id {
						if key, ok := sig.Body[1].(string); ok {
							if opts.OnAction != nil {
								opts.OnAction(key)
							}
							return
						}
					}
				}
			case notificationClosed:
				if len(sig.Body) >= 2 {
					if sid, ok := sig.Body[0].(uint32); ok && sid == id {
						reason, _ := sig.Body[1].(uint32)
						if opts.OnClosed != nil {
							opts.OnClosed(reason)
						}
						return
					}
				}
			}
		case <-timeout:
			return
		}
	}
}

// notifySend is the no-session-bus fallback. Actions are unsupported there.
func notifySend(title, body string) (uint32, error) {
	path, err := lookPath("notify-send")
	if err != nil {
		logx.Logf("[Notify] No session bus and no notify-send; dropping notification: %s: %s", title, body)
		return 0, fmt.Errorf("notify: no session bus and no notify-send: %w", err)
	}
	return 0, startDetached(path, title, body, "-a", buildinfo.AppName)
}

// Injectables for tests: notify-send lookup and launch.
var (
	lookPath      = exec.LookPath
	startDetached = func(path string, args ...string) error {
		cmd := exec.Command(path, args...)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		return cmd.Start()
	}
)

// Close closes a notification by id (best effort).
func Close(id uint32) error {
	conn, err := sessionBus()
	if err != nil {
		return nil
	}
	defer func() { _ = conn.Close() }()
	return conn.Object(dbusName, dbus.ObjectPath(dbusPath)).
		Call("CloseNotification", 0, id).Err
}

// ErrNoBus is returned by helpers that require the bus.
var ErrNoBus = errors.New("notify: no session bus available")
