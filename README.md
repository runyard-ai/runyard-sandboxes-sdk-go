# runyard-sandboxes Go SDK

[![Go Reference](https://pkg.go.dev/badge/github.com/runyard-ai/runyard-sandboxes-sdk-go.svg)](https://pkg.go.dev/github.com/runyard-ai/runyard-sandboxes-sdk-go)

The Go client for [runyard-sandboxes](https://runyard.ai): create a microVM
from any OCI image, run commands and move files in it, decide what it may
reach on the network, and drop it.

```sh
go get github.com/runyard-ai/runyard-sandboxes-sdk-go
```

It needs Go 1.26 or later, and the address of a runyard-sandboxes daemon with
an API key.

## Example

```go
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/runyard-ai/runyard-sandboxes-sdk-go/sdk"
)

func main() {
	ctx := context.Background()
	client, err := sdk.New("https://sandboxes.example.com", sdk.WithKey(os.Getenv("RUNYARD_KEY")))
	if err != nil {
		log.Fatal(err)
	}

	// Create returns once the machine answers.
	sandbox, err := client.Create(ctx, "hello", sdk.Spec{Image: "debian:bookworm-slim"})
	if sandbox != nil {
		// A sandbox the daemon accepted exists even when Create fails.
		defer sandbox.Close(ctx)
	}
	if err != nil {
		log.Fatal(err)
	}

	result, err := sandbox.Run(ctx, "uname", "-a")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Print(result.Stdout)
}
```

`Run` takes the command already split: `Run(ctx, "sh", "-c", "…")` when you
want a shell. A non-zero exit code is in `result.ExitCode`, not an error.

The SDK covers what most programs need: sandboxes, commands, files, egress
rules, tunnels and volumes. Everything else in the API is on `client.API()`,
the client generated from the OpenAPI document, on the same address and key.

## Examples

Each is a program you can run against a daemon, with tests that run it against
the fake daemon:

| Example | What it shows |
|---|---|
| [`examples/hello`](examples/hello) | Two sandboxes made in parallel: commands, files, a brokered call, and dropping them. |
| [`examples/egress`](examples/egress) | A sandbox's egress rules at work: what they allow is reached, the rest is refused, while they change. |
| [`examples/claude`](examples/claude) | The Claude CLI in a sandbox, with the API key held outside it by a broker. |
| [`examples/claude-chat`](examples/claude-chat) | A conversation with Claude where each turn is a new sandbox on the last one's volume. |

```sh
go run github.com/runyard-ai/runyard-sandboxes-sdk-go/examples/hello@latest \
  -addr https://sandboxes.example.com -key "$RUNYARD_KEY"
```

## Packages

| Package | What it is |
|---|---|
| [`sdk`](https://pkg.go.dev/github.com/runyard-ai/runyard-sandboxes-sdk-go/sdk) | The client. |
| [`sandboxes/genv1`](https://pkg.go.dev/github.com/runyard-ai/runyard-sandboxes-sdk-go/sandboxes/genv1) | The API's types and the generated client, from [`openapi/sandboxes.yaml`](openapi/sandboxes.yaml). |
| [`test/doubles/fakedaemon`](https://pkg.go.dev/github.com/runyard-ai/runyard-sandboxes-sdk-go/test/doubles/fakedaemon) | A daemon in memory, for your tests. |
| [`test/doubles/spawn`](https://pkg.go.dev/github.com/runyard-ai/runyard-sandboxes-sdk-go/test/doubles/spawn) | Runs a function beside a test, which does not end before it has. |

## Testing your code

`fakedaemon` answers the same HTTP API as the real daemon, from memory and
with no virtual machines, so your tests exercise real requests and responses:

```go
func TestHello(t *testing.T) {
	daemon := fakedaemon.New(t, fakedaemon.WithKey("test-key"))
	client, err := sdk.New(daemon.URL, sdk.WithKey("test-key"))
	if err != nil {
		t.Fatal(err)
	}
	sandbox, err := client.Create(t.Context(), "hello", sdk.Spec{Image: "debian:bookworm-slim"})
	if err != nil {
		t.Fatal(err)
	}
	defer sandbox.Close(t.Context())
	// …
}
```

`fakedaemon.WithRun` decides what a command prints, and `Intercept` makes one
operation fail the way you want to test.

## The API itself

[`openapi/sandboxes.yaml`](openapi/sandboxes.yaml) is the OpenAPI document
for the whole API, for generating a client in another language. A daemon
also serves it at `GET /v1/openapi.yaml`, and renders it at `GET /v1/docs`.

## Versions

The SDK is released with the daemon, with the same version number: SDK
`v0.9.0` is the client of daemon `v0.9.0`. While the version is `v0.x`, a
minor version may change the API.

This repository is published from the runyard-sandboxes release process, and
every file in it is overwritten by the next release. Report issues here.

## License

[Apache License 2.0](LICENSE).
