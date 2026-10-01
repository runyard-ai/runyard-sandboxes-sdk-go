# ryclaude: Claude Code in your terminal, running in a sandbox

`ryclaude` looks and behaves exactly like `claude`: the same screen, the same
keys, Ctrl-C included. That's because it *is* `claude`, running in a PTY inside
a microVM, with your terminal carried there and back. Everything Claude does
happens in the sandbox: the files it reads and edits are the sandbox's, and so
are the commands it runs. Your machine is only the screen and the keyboard.

```sh
go run ./examples/ryclaude -addr https://sandboxes.example.com -allow github.com
go run ./examples/ryclaude -- --model opus   # whatever follows -- goes to claude
```

When it starts:

1. It makes a sandbox from its image ([`image/`](image)), which the daemon
   pulls from where it is published. The sandbox reaches
   Anthropic's domains and the ones you list in `-allow`, and nothing else.
2. It copies your Claude credentials into the sandbox.
3. It runs `claude` in a terminal in the sandbox, connected to yours: your
   keys go in, the screen comes out, and window resizes follow.
4. When `claude` exits, it drops the sandbox, and exits with Claude's exit
   code.

Nothing of your machine is in the sandbox except those credentials: no files,
no shell configuration, no `~/.claude` settings. Claude starts in an empty
`/workspace`, and gets its code the way any fresh machine does, for example
by cloning it.

## Your credentials are in the sandbox

Anything running in the sandbox can read the credentials `ryclaude` copies
in. That is temporary: a broker on the host is meant to carry Claude's calls
and keep the credential out of the sandbox. Until then it is no worse than
running `claude` on your own laptop, where the same file is within reach of
everything Claude runs.

## What it needs

- **A daemon that can reach `releases.runyard.ai`**, where the image is
  published for anyone to pull, with no key:
  `releases.runyard.ai/runyard-public/ryclaude:latest`. It is
  [`image/`](image): `claude` and what its tools shell out to, and nothing of
  yours. Each release publishes it under its version, and `latest` is the
  highest one.

  `-image`, or `RYCLAUDE_IMAGE`, names another one: a version of it, or one of
  your own, built from `image/` and pushed to a registry the daemon pulls from.
- **Your credentials**: `~/.claude/.credentials.json`, which `claude` writes
  when you log in on Linux. `-credentials` names another file.
- **A key** with `RUNYARD_SANDBOXES_KEY` or `-key`, and the daemon's address
  with `RUNYARD_SANDBOXES_ADDR` or `-addr`.

## Limits of this proof of concept

- **Nothing outlives the session.** The sandbox and everything in it are
  dropped when `claude` exits. Work that should survive has to be pushed from
  inside it.
- **Settings stay behind.** Claude starts as a fresh install, without your
  settings, memory or MCP servers.
