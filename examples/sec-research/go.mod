module github.com/zeroroot-ai/setec/examples/sec-research

go 1.26.4

require (
	github.com/zeroroot-ai/setec v0.0.0-00010101000000-000000000000
	google.golang.org/grpc v1.82.0
)

require (
	golang.org/x/net v0.55.0 // indirect
	golang.org/x/sys v0.45.0 // indirect
	golang.org/x/text v0.37.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260414002931-afd174a4e478 // indirect
	google.golang.org/protobuf v1.36.12-0.20260120151049-f2248ac996af // indirect
)

// Local development: point the example at the parent repo. Drop this replace
// line before consuming the example outside the Setec source tree.
replace github.com/zeroroot-ai/setec => ../..
