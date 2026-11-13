package journal

import "testing"

func mkfifo(t *testing.T, _ string) {
	t.Helper()
	t.Skip("Windows has no FIFOs")
}
