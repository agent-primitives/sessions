module github.com/agent-primitives/sessions/examples/agentsuite-substrate

go 1.27.0

require (
	github.com/agent-primitives/sessions v0.0.0
	github.com/agent-substrate/substrate v0.4.1-0.20261013053953-96dffb287919
	github.com/kaimahi-agents/kaimahi v0.5.2-0.20261014180858-9c5c993e35ba
	google.golang.org/grpc v1.84.0
	google.golang.org/protobuf v1.36.12
)

require (
	github.com/distribution/reference v0.6.0 // indirect
	github.com/gowebpki/jcs v1.0.2 // indirect
	github.com/opencontainers/go-digest v1.0.0 // indirect
	golang.org/x/net v0.60.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/text v0.42.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260803160001-6ac0973c030d // indirect
)

// The example builds against this checkout. kaimahi still requires the v0.1.x module path,
// github.com/aramase/agentsessions, but the agentsuite package this links imports none of it, so
// only its go.mod is read and the two paths never register the same proto files in one binary.
replace github.com/agent-primitives/sessions => ../..
