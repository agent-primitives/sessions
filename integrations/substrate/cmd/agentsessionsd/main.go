// Command agentsessionsd is the Sessions API server with the agent-substrate backend linked in: on top
// of everything the neutral cmd/agentsessionsd serves, -substrate-harness name=atespace/template runs
// a harness as one substrate actor per session, created from that ActorTemplate, controlled over the
// ate-api (-ateapi-addr, -ateapi-token-file) and reached through the atenet-router (-atenet-router).
//
// It lives in this module because the core module must not link agent-substrate; the server itself
// is internal/agentsessionsd, shared with the neutral binary.
package main

import (
	"google.golang.org/grpc"

	ateadapter "github.com/agent-primitives/sessions/integrations/substrate"
	"github.com/agent-primitives/sessions/internal/agentsessionsd"
	"github.com/agent-primitives/sessions/runtime/substrate"
)

func main() { agentsessionsd.Main(agentsessionsd.WithSubstrate(newControl)) }

func newControl(conn grpc.ClientConnInterface) substrate.ControlClient { return ateadapter.New(conn) }
