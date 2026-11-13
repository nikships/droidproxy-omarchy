package backend

import "os"

// terminate ends p. Windows cannot deliver SIGTERM to a child without a
// console, so TerminateProcess is the only option.
func terminate(p *os.Process) error { return p.Kill() }
