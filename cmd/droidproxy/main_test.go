package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/nikships/droidproxy-omarchy/internal/control"
)

// The CLI's ctl subcommands are thin wrappers over internal/control; the
// important behaviors to lock down are the output shapes and exit-code
// conventions, which the shell plugin depends on.

func TestEncodeJSONOneLine(t *testing.T) {
	var buf bytes.Buffer
	old := stdout
	stdout = &buf
	defer func() { stdout = old }()

	if err := encodeJSON(control.OK("hi")); err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(buf.String())
	if strings.Contains(got, "\n") {
		t.Errorf("expected one line, got %q", got)
	}
	var res control.CallResult
	if err := json.Unmarshal([]byte(got), &res); err != nil {
		t.Fatalf("output is not JSON: %q", got)
	}
	if !res.OK || res.Message != "hi" {
		t.Errorf("unexpected result: %+v", res)
	}
}

func TestUsageListsCommands(t *testing.T) {
	// usage() writes to stdout; just ensure it does not panic and mentions
	// the documented subcommands.
	var buf bytes.Buffer
	old := stdout
	stdout = &buf
	defer func() { stdout = old }()
	usage()
	out := buf.String()
	for _, want := range []string{"serve", "ctl watch", "ctl call", "status", "start", "stop", "restart", "open", "login", "update", "rollback", "install", "uninstall", "version", "logs", "help"} {
		if !strings.Contains(out, want) {
			t.Errorf("usage is missing %q", want)
		}
	}
}
