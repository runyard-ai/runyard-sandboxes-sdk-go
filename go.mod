// The Go SDK for runyard-sandboxes, and the code generated from the API's
// OpenAPI document (openapi/sandboxes.yaml) that it is built on.
//
// It is developed inside runyard-sandboxes, beside the daemon it talks to, and
// published to github.com/runyard-ai/runyard-sandboxes-sdk-go with each
// release: the same version, so SDK v0.9.0 is the client of daemon v0.9.0. It
// requires nothing of the daemon's.
module github.com/runyard-ai/runyard-sandboxes-sdk-go

go 1.26

require (
	github.com/google/uuid v1.6.0
	github.com/oapi-codegen/runtime v1.7.0
	go.uber.org/goleak v1.3.0
	gopkg.in/yaml.v2 v2.4.0
)

require (
	github.com/apapsch/go-jsonmerge/v2 v2.0.0 // indirect
	github.com/kr/text v0.2.0 // indirect
)
