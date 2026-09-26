// Command droidproxy is the CLI for the DroidProxy daemon: it starts and
// controls the service, exposes the control API to scripts and the Omarchy
// shell plugin (ctl), and installs/updates/uninstalls the application.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/nikships/droidproxy-omarchy/internal/buildinfo"
	"github.com/nikships/droidproxy-omarchy/internal/control"
	"github.com/nikships/droidproxy-omarchy/internal/daemon"
	"github.com/nikships/droidproxy-omarchy/internal/desktop"
	"github.com/nikships/droidproxy-omarchy/internal/installer"
	"github.com/nikships/droidproxy-omarchy/internal/paths"
	"github.com/nikships/droidproxy-omarchy/internal/webui"
)

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		args = []string{"help"}
	}
	cmd, rest := args[0], args[1:]

	var err error
	switch cmd {
	case "serve":
		err = cmdServe(rest)
	case "ctl":
		err = cmdCtl(rest)
	case "status":
		err = cmdStatus(rest)
	case "start", "stop", "restart":
		err = cmdService(cmd, rest)
	case "open":
		err = cmdOpen(rest)
	case "login":
		err = cmdLogin(rest)
	case "update":
		err = cmdUpdate(rest)
	case "rollback":
		err = cmdRollback(rest)
	case "install":
		err = cmdInstall(rest)
	case "uninstall":
		err = cmdUninstall(rest)
	case "version", "--version", "-v":
		err = cmdVersion()
	case "logs":
		err = cmdLogs(rest)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "droidproxy: unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "droidproxy:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(stdout, `droidproxy — DroidProxy for Omarchy

usage: droidproxy <command> [args]

  serve                     run the daemon (normally started by systemd)
  ctl state                 print the current state snapshot as JSON
  ctl watch                 stream events as NDJSON, forever
  ctl call <method> [json]  run one action, e.g. droidproxy ctl call server.toggle
  status                    human-readable summary of the daemon state
  start | stop | restart    control the droidproxy systemd user service
  open                      open the DroidProxy settings page in the browser
  login <provider>          start a provider sign-in flow
  update [check|install]    check for updates, or install the available one
  rollback                  switch back to the previous version
  install [--from DIR]      install from a release tree (default: this build)
  uninstall [--purge] [--yes]
                            remove DroidProxy; --purge also deletes data
  version                   print version information
  logs                      follow the daemon logs (journalctl)
  help                      show this help
`)
}

// ---- serve ------------------------------------------------------------------

func cmdServe(args []string) error {
	fs := newFlagSet("serve")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	return daemon.Run(ctx)
}

// ---- ctl --------------------------------------------------------------------

func cmdCtl(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("ctl requires a subcommand: state, watch, or call")
	}
	client := control.DefaultClient()
	switch args[0] {
	case "state":
		st, err := client.State()
		if err != nil {
			return offlineOrErr(err)
		}
		return encodeJSON(st)
	case "watch":
		return cmdCtlWatch(client)
	case "call":
		return cmdCtlCall(client, args[1:])
	default:
		return fmt.Errorf("unknown ctl subcommand %q", args[0])
	}
}

// offlineOrErr prints the offline marker and exits 1 when the daemon is not
// reachable; other errors are returned.
func offlineOrErr(err error) error {
	if control.IsOffline(err) {
		fmt.Println(`{"type":"offline"}`)
		os.Exit(1)
	}
	return err
}

func cmdCtlWatch(client *control.Client) error {
	// Never exits: when the daemon is unreachable, print the offline marker
	// once and retry every 2s, resuming with a fresh state event.
	first := true
	for {
		err := client.Watch(context.Background(), func(ev control.Event) {
			line, jerr := json.Marshal(ev)
			if jerr != nil {
				return
			}
			fmt.Println(string(line))
		})
		if err == nil || control.IsOffline(err) || ctxCanceled(err) {
			if first {
				fmt.Println(`{"type":"offline"}`)
				first = false
			}
		} else {
			return err
		}
		time.Sleep(2 * time.Second)
	}
}

func ctxCanceled(err error) bool {
	return err == context.Canceled || err == context.DeadlineExceeded
}

func cmdCtlCall(client *control.Client, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("ctl call requires a method, e.g. droidproxy ctl call server.toggle")
	}
	method := args[0]
	var params any
	if len(args) > 1 {
		if err := json.Unmarshal([]byte(args[1]), &params); err != nil {
			return fmt.Errorf("params must be a JSON object: %w", err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	res, err := client.Call(ctx, method, params)
	if err != nil {
		return offlineOrErr(err)
	}
	if err := encodeJSON(res); err != nil {
		return err
	}
	if !res.OK {
		os.Exit(1)
	}
	return nil
}

// ---- status -----------------------------------------------------------------

func cmdStatus(args []string) error {
	fs := newFlagSet("status")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := control.DefaultClient().State()
	if err != nil {
		return offlineOrErr(err)
	}
	fmt.Printf("DroidProxy %s (%s)\n", st.App.Version, st.App.Commit)
	fmt.Printf("server:    %s (proxy %s, backend %s)\n", serverStateText(st), st.Server.URL, st.Server.DashboardURL)
	if st.Server.LastError != "" {
		fmt.Printf("last error: %s\n", st.Server.LastError)
	}
	for _, p := range st.Providers {
		state := "off"
		if p.Enabled {
			state = fmt.Sprintf("on, %d account(s)", len(p.Accounts))
			if p.Authenticating {
				state += ", signing in"
			}
		}
		fmt.Printf("%-14s %s\n", p.Name+":", state)
	}
	if st.Update.LatestVersion != "" && st.Update.State == control.UpdateAvailable {
		fmt.Printf("update:    %s available (current %s)\n", st.Update.LatestVersion, st.Update.CurrentVersion)
	} else {
		fmt.Printf("update:    %s\n", st.Update.State)
	}
	return nil
}

func serverStateText(st control.State) string {
	switch {
	case st.Server.Running:
		return "running"
	case st.Server.Starting:
		return "starting"
	default:
		return "stopped"
	}
}

// ---- service control --------------------------------------------------------

func cmdService(action string, args []string) error {
	fs := newFlagSet(action)
	if err := fs.Parse(args); err != nil {
		return err
	}
	switch action {
	case "start":
		// When invoked outside systemd (terminal), bring the unit up.
		if !desktop.IsRunning() {
			return desktop.Start()
		}
		fmt.Println("DroidProxy is already running.")
		return nil
	case "stop":
		return desktop.Stop()
	case "restart":
		return desktop.Restart()
	}
	return nil
}

func cmdOpen(args []string) error {
	fs := newFlagSet("open")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !desktop.IsRunning() {
		if err := desktop.Start(); err != nil {
			return fmt.Errorf("could not start the service: %w", err)
		}
	}
	// The settings live in the browser now: the bar icon and the launcher
	// entry both land here.
	return desktop.OpenURL(webui.URL())
}

func cmdLogin(args []string) error {
	fs := newFlagSet("login")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: droidproxy login <provider>  (claude, codex, grok, copilot, meta, …)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	res, err := control.DefaultClient().Call(ctx, "provider.connect", map[string]any{"provider": fs.Arg(0)})
	if err != nil {
		return offlineOrErr(err)
	}
	if res.Message != "" {
		fmt.Println(res.Message)
	}
	if !res.OK {
		if res.Error != "" {
			fmt.Fprintln(os.Stderr, res.Error)
		}
		os.Exit(1)
	}
	return nil
}

// ---- update -----------------------------------------------------------------

func cmdUpdate(args []string) error {
	fs := newFlagSet("update")
	if err := fs.Parse(args); err != nil {
		return err
	}
	action := "check"
	if fs.NArg() > 0 {
		action = fs.Arg(0)
	}
	switch action {
	case "check":
		res, err := control.DefaultClient().Call(context.Background(), "update.check", nil)
		if err != nil {
			return offlineOrErr(err)
		}
		st, err := control.DefaultClient().State()
		if err != nil {
			return offlineOrErr(err)
		}
		switch st.Update.State {
		case control.UpdateAvailable:
			fmt.Printf("Update available: %s (current %s)\n", st.Update.LatestVersion, st.Update.CurrentVersion)
			fmt.Println("Install it with: droidproxy update install")
		case control.UpdateUpToDate:
			fmt.Println("DroidProxy is up to date.")
		case control.UpdateError:
			fmt.Printf("Update check failed: %s\n", st.Update.Error)
		default:
			fmt.Printf("Update state: %s\n", st.Update.State)
		}
		if !res.OK && res.Error != "" {
			fmt.Fprintln(os.Stderr, res.Error)
			os.Exit(1)
		}
		return nil
	case "install":
		res, err := control.DefaultClient().Call(context.Background(), "update.install", nil)
		if err != nil {
			return offlineOrErr(err)
		}
		if res.Message != "" {
			fmt.Println(res.Message)
		}
		if !res.OK {
			if res.Error != "" {
				fmt.Fprintln(os.Stderr, res.Error)
			}
			os.Exit(1)
		}
		return nil
	default:
		return fmt.Errorf("unknown update action %q (want check or install)", action)
	}
}

// ---- install / uninstall / rollback -----------------------------------------

func cmdRollback(args []string) error {
	fs := newFlagSet("rollback")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return installer.Rollback()
}

func cmdInstall(args []string) error {
	fs := newFlagSet("install")
	from := fs.String("from", "", "release tree to install (default: this build's resource root)")
	update := fs.Bool("update", false, "mark as an update (skip service enable/start)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	opts := installer.Options{From: *from, Update: *update}
	if opts.From == "" {
		opts.From = paths.ResourceRoot()
	}
	fmt.Printf("Installing DroidProxy from %s…\n", opts.From)
	if err := installer.Install(opts); err != nil {
		return err
	}
	fmt.Println("Done.")
	fmt.Println("The menu bar widget is available in Omarchy shell settings.")
	fmt.Printf("Follow the daemon with: droidproxy logs\n")
	return nil
}

func cmdUninstall(args []string) error {
	fs := newFlagSet("uninstall")
	purge := fs.Bool("purge", false, "also delete preferences, state, and CLIProxyAPI credentials")
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !*yes {
		prompt := "Remove DroidProxy?"
		if *purge {
			prompt = "Remove DroidProxy AND delete all its data and credentials?"
		}
		ok, err := confirm(prompt)
		if err != nil {
			return err
		}
		if !ok {
			fmt.Println("Aborted.")
			return nil
		}
	}
	return installer.Uninstall(*purge)
}

func confirm(prompt string) (bool, error) {
	fmt.Printf("%s [y/N] ", prompt)
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		return false, nil // EOF: treat as "no"
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}

// ---- misc -------------------------------------------------------------------

func cmdVersion() error {
	fmt.Printf("droidproxy %s (%s, %s)\n", buildinfo.Version, buildinfo.Commit, buildinfo.BuildDate)
	return nil
}

func cmdLogs(args []string) error {
	fs := newFlagSet("logs")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// Replace this process so that Ctrl-C and journalctl -f behave exactly
	// like running journalctl directly.
	journalctl, err := exec.LookPath("journalctl")
	if err != nil {
		return fmt.Errorf("journalctl not found (is systemd running?)")
	}
	return syscall.Exec(journalctl,
		[]string{"journalctl", "--user", "-u", desktop.ServiceUnit, "-f"}, os.Environ())
}

// ---- helpers ----------------------------------------------------------------

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	fs.Usage = func() { usage() }
	return fs
}

// stdout is where command output goes; tests redirect it.
var stdout io.Writer = os.Stdout

func encodeJSON(v any) error {
	line, err := json.Marshal(v)
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, string(line))
	return nil
}
