// Command agentsessionsd serves the Sessions API over TCP. See internal/agentsessionsd for the
// flags and what they serve.
package main

import "github.com/agent-primitives/sessions/internal/agentsessionsd"

func main() { agentsessionsd.Main() }
