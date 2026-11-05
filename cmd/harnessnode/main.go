// Command harnessnode is the harness server packaged to run inside a compute sandbox. It serves one
// of the built-in harnesses over harnesswire; see internal/harnessnode for HARNESS_KIND and the
// other settings it reads.
//
// This is the neutral build. A build that serves a harness this module does not link passes
// harnessnode.WithHarness from its own module.
package main

import "github.com/agent-primitives/sessions/internal/harnessnode"

func main() { harnessnode.Main() }
