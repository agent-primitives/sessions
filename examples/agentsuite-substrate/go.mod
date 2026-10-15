module github.com/aramase/agentsessions/examples/agentsuite-substrate

go 1.27.0

require (
	github.com/agent-substrate/substrate v0.4.1-0.20261013053953-96dffb287919
	github.com/aramase/agentsessions v0.1.3-0.20261013201528-ca85f47a2ec1
	github.com/aramase/agentsessions/integrations/substrate v0.0.0-00010101000000-000000000000
	github.com/distribution/reference v0.6.0
	github.com/kaimahi-agents/kaimahi v0.6.1-0.20261015030249-75f27d67f40b
	google.golang.org/grpc v1.84.0
	google.golang.org/protobuf v1.36.12
	k8s.io/apimachinery v0.37.1
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/fxamacker/cbor/v2 v2.9.1 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/gowebpki/jcs v1.0.2 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/opencontainers/go-digest v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/x448/float16 v0.8.4 // indirect
	golang.org/x/net v0.60.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/text v0.42.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260803160001-6ac0973c030d // indirect
	gopkg.in/inf.v0 v0.9.1 // indirect
	k8s.io/klog/v2 v2.140.0 // indirect
	k8s.io/utils v0.0.0-20260626114624-be93311217bd // indirect
	modernc.org/libc v1.77.1 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
	modernc.org/sqlite v1.60.1 // indirect
	sigs.k8s.io/json v0.0.0-20250730193827-2d320260d730 // indirect
)

// The example builds against this checkout's agentsessions, whatever version kaimahi requires.
replace github.com/aramase/agentsessions => ../..

replace github.com/aramase/agentsessions/integrations/substrate => ../../integrations/substrate
