// Command agentsessionsd serves the Sessions API over TCP. See internal/agentsessionsd for the
// flags and what they serve.
//
// This is the neutral build: it serves the built-in harnesses and harnesses registered by address.
// The build that also runs harnesses as agent-substrate actors is
// integrations/substrate/cmd/agentsessionsd, because only that module may link agent-substrate.
package main

import "github.com/aramase/agentsessions/internal/agentsessionsd"

func main() { agentsessionsd.Main() }
