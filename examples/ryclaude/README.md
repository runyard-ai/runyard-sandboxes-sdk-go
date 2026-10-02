# ryclaude: Claude Code in your terminal, running in a sandbox

`ryclaude` looks and behaves exactly like `claude`: the same screen, the same
keys, Ctrl-C included. That's because it *is* `claude`, running in a PTY inside
a microVM, with your terminal carried there and back. Everything Claude does
happens in the sandbox: the files it reads and edits are the sandbox's, and so
are the commands it runs. Your machine is only the screen and the keyboard.

```sh
go run ./examples/ryclaude auth login -url https://sandboxes.example.com   # once
go run ./examples/ryclaude -allow github.com
go run ./examples/ryclaude -- --model opus   # whatever follows -- goes to claude
```

When it starts:

1. It makes a sandbox from its image ([`image/`](image)), which the daemon
   pulls from where it is published. The sandbox reaches
   Anthropic's domains and the ones you list in `-allow`, and nothing else.
   The first start from an image is the slow one: the daemon pulls and
   unpacks it once, and `ryclaude` says what it is doing meanwhile.
2. It copies your Claude credentials into the sandbox.
3. It runs `claude` in a terminal in the sandbox, connected to yours: your
   keys go in, the screen comes out, and window resizes follow.
4. When `claude` exits, it drops the sandbox, and exits with Claude's exit
   code.

Nothing of your machine is in the sandbox except those credentials: no files,
no shell configuration, no `~/.claude` settings. Claude starts in an empty
`/workspace`, and gets its code the way any fresh machine does, for example
by cloning it.

## Logging in

`ryclaude auth login -url https://sandboxes.example.com` gets this machine a
key of its own. It opens the daemon's console in your browser, where you sign
in, read what is asked and approve it. After that, `ryclaude` needs neither
`-addr` nor `-key`.

The key can do what `ryclaude` does and nothing more:

- **Four scopes**: `sandboxes.read`, `sandboxes.write`, `files.write` and
  `exec`.
- **Only the sandboxes it creates.** It cannot see or touch anybody else's,
  including your own from elsewhere.
- **For 7 days**, unless you choose another expiry on the page where you
  approve it. When it has expired, log in again.

On a machine with no browser, add `-no-browser`: it prints the address, you
open it anywhere, and paste the code the page shows.

The key is kept in `$XDG_CONFIG_HOME/ryclaude/credentials.json`
(`~/.config/ryclaude/credentials.json` when that is not set), readable only
by you. `ryclaude` refuses a file that others can read. One daemon at a time:
logging in again replaces the key, and revokes the one before it.

- `ryclaude auth status` says which daemon, whose key, when it expires, and
  whether the daemon still accepts it. It exits 0 when it does.
- `ryclaude auth logout` revokes the key and removes the file. When the
  daemon cannot be reached it keeps the file, so that you can try again;
  `-forget` removes it anyway and leaves the key to expire.

The key is only ever sent to the daemon that issued it. If `-addr` or
`RUNYARD_SANDBOXES_ADDR` names another daemon, `ryclaude` stops and says so
unless you also give that daemon's key.

When it has logged in, `ryclaude` says whose key it now holds. Check that it
is your account: the answer comes back through a port on your machine, and
the first one to arrive is the one it takes.

Login needs a daemon whose console signs people in. For one that does not,
mint a key on its host with `runyard-sandboxes keys mint`, and give it with
`-key` or `RUNYARD_SANDBOXES_KEY`.

`auth` is `ryclaude`'s own word. To run `claude auth …` in the sandbox, put it
after `--`.

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
- **A key, and the daemon's address**: `ryclaude auth login` gets both (see
  [Logging in](#logging-in)). `-key` or `RUNYARD_SANDBOXES_KEY`, with `-addr`
  or `RUNYARD_SANDBOXES_ADDR`, are used instead when you give them.

  Either way the address is the daemon's own, with no path:
  `https://sandboxes.example.com`, or `http://127.0.0.1:8099` for a daemon on
  this machine. Plain `http://` to another machine is refused, whichever key
  it is, because the key would cross the network in the clear.

## Limits of this proof of concept

- **Nothing outlives the session.** The sandbox and everything in it are
  dropped when `claude` exits. Work that should survive has to be pushed from
  inside it.
- **Settings stay behind.** Claude starts as a fresh install, without your
  settings, memory or MCP servers.
