# Claude in a sandbox

A microVM with the Claude CLI in it, that you use from the console's terminal —
and in which there is no Anthropic credential to find. Calls to the API leave
the machine through the host's shared `claude` broker, which is where the key
is.

Three pieces, each usable without the others:

| | |
|---|---|
| [`image/`](image) | an OCI image: Node, the Claude CLI, and a login script that points it at the broker |
| the `claude` broker | run on the daemon's host by its operator: it holds the key and forwards messages |
| [`main.go`](main.go) | the program that makes the sandbox |

## 1. The image

Built wherever there is Docker, and pushed wherever the daemon can pull from. A
registry on the daemon's own host is the simple case: the daemon speaks plain
HTTP to one on loopback, as Docker does.

```sh
docker run -d --restart unless-stopped --name runyard-registry -p 127.0.0.1:5001:5000 registry:2
docker build -t localhost:5001/runyard/claude-sandbox:latest examples/claude/image
docker push localhost:5001/runyard/claude-sandbox:latest
```

## 2. The broker

The daemon's operator runs the Claude broker on its host and declares it as the
shared broker `claude`. The credential it holds is an Anthropic API key, or a
subscription token from `claude setup-token`. `client.Info` lists the brokers a
daemon declares; this example needs one called `claude`.

## 3. The sandbox

```sh
go run ./examples/claude -key "$RUNYARD_SANDBOXES_KEY" -prompt "Say hello from inside a microVM."
```

```
making "claude" from localhost:5001/runyard/claude-sandbox:latest …
ready in 1.2s: 01a0dd62-2ab1-7ada-b93b-36282bda1eb0
inside it, Claude talks to http://127.0.0.1:61000 — a port on the machine's own loopback

> Say hello from inside a microVM.

…Claude's answer…

(how long it took)
dropped.
```

It makes the sandbox, runs `claude -p` in it with the prompt, prints the
answer, and drops the sandbox and its disk — also when Claude fails or Ctrl-C
interrupts it. `-timeout` bounds the answer (five minutes by default).

With `-keep` the sandbox stays up afterwards, and it prints where to find it:

```
open   http://127.0.0.1:8099/console#/sandboxes/01a0dd62-2ab1-7ada-b93b-36282bda1eb0
then   Terminal → Open a terminal, and type: claude
```

`-prompt ""` skips the question, for a sandbox made only to use from the
terminal.

The key it is given needs `sandboxes.read`, `sandboxes.write` and `exec`.
Opening the terminal needs `exec` too, on the key the console holds.

## What is where

- **In the sandbox:** the CLI, and a URL on its own loopback. No key, no token,
  no route to anything else — its network is closed and the broker is the only
  way out.
- **On the host:** the key, in a file only the broker reads, and a log line per
  request naming the sandbox that made it.
