// Command agentsessionsd is the Sessions API server with the agent-substrate backend linked in: on
// top of everything the neutral cmd/agentsessionsd serves, -substrate-harness
// name=atespace/template runs a harness as one substrate actor per session, created from that
// ActorTemplate, controlled over the ate-api (-ateapi-addr, -ateapi-token-file) and reached through
// the atenet-router (-atenet-router).
//
// The server is the core module's internal/agentsessionsd, shared with the neutral binary. This
// module adds the substrate harnesses through its neutral agentsessionsd.WithHarnessBackends hook:
// the substrate flags, their checks and the ate-api connection live here (substrate.go). The root
// module's daemon links no substrate package; this module adds runtime/substrate and the ate-api
// adapter.
package main

import (
	"google.golang.org/grpc"

	ateadapter "github.com/agent-primitives/sessions/integrations/substrate"
	"github.com/agent-primitives/sessions/internal/agentsessionsd"
	"github.com/agent-primitives/sessions/runtime/substrate"
)

func main() { agentsessionsd.Main(agentsessionsd.WithHarnessBackends(newSubstrateFlags(newControl))) }

func newControl(conn grpc.ClientConnInterface) substrate.ControlClient { return ateadapter.New(conn) }
