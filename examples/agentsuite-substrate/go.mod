module github.com/aramase/agentsessions/examples/agentsuite-substrate

go 1.27.0

require (
	github.com/agent-substrate/substrate v0.4.1-0.20261013053953-96dffb287919
	github.com/aramase/agentsessions v0.1.3-0.20261008190619-e680ea10d3b4
	github.com/kaimahi-agents/kaimahi v0.5.2-0.20261013052909-ccde30794a6b
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

// The example builds against this checkout's agentsessions, whatever version kaimahi requires.
replace github.com/aramase/agentsessions => ../..
