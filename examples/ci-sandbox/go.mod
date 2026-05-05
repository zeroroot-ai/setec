module github.com/zero-day-ai/setec/examples/ci-sandbox

go 1.25.3

require (
	github.com/zero-day-ai/setec v0.0.0-00010101000000-000000000000
	google.golang.org/grpc v1.81.0
)

require (
	golang.org/x/net v0.52.0 // indirect
	golang.org/x/sys v0.42.0 // indirect
	golang.org/x/text v0.35.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260401024825-9d38bb4040a9 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)

// Local development: point the example at the parent repo. Drop this replace
// line before consuming the example outside the Setec source tree.
replace github.com/zero-day-ai/setec => ../..
