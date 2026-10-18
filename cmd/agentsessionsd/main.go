// Command agentsessionsd serves the Sessions API over TCP. See internal/agentsessionsd for the
// flags and what they serve.
//
// This is the neutral build: it serves the built-in harnesses and harnesses registered by address.
// A build that serves harnesses on another backend passes agentsessionsd.WithHarnessBackends from
// its own module; integrations/substrate/cmd/agentsessionsd is the one that runs harnesses as
// agent-substrate actors.
package main

import "github.com/agent-primitives/sessions/internal/agentsessionsd"

func main() { agentsessionsd.Main() }
