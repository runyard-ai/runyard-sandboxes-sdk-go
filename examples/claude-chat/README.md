# A conversation with Claude, one sandbox per turn

Each turn makes a microVM, lets Claude answer and do whatever it wants to the
filesystem, and drops the microVM. What it wrote is kept on a **volume** (the
sandbox's disk, kept by name), and the next turn's sandbox names the same
volume. So it boots with everything the previous turn left: the files Claude
made, whatever it installed, and Claude's own record of the conversation, which
is how `claude --continue` picks the thread up.

Nothing runs between turns. A conversation left for a week costs its disk and
no memory.

It needs the image and the broker from [`../claude`](../claude): build and push
the image, run the broker, and declare it as the shared broker `claude`, as that
README says.

## Running it

```sh
go run ./examples/claude-chat -key "$RUNYARD_SANDBOXES_KEY"
```

A real run on server02:

```
a new conversation, on volume demo-chat
type a message; /reset forgets everything, /exit leaves

you> Create a file notes.txt in the current directory containing the word banana.
claude> I created `/root/notes.txt`, and it contains the word "banana".

[sandbox 01a0de1a-… · ready in 170ms · answered in 3.471s · dropped in 91ms · volume holds 7.5 MB]

you> Without reading any file first: what did I ask you in my previous message?
claude> In your previous message, you asked me to create notes.txt …

[sandbox 01a0de1a-… · ready in 134ms · answered in 5.101s · dropped in 61ms · volume holds 7.7 MB]
```

- `/reset` deletes the volume, so the next turn starts from an empty one.
- `/exit` or end of input leaves.
- The volume is kept unless `-forget` is given. `-volume <name>` picks a
  conversation up again, from another run or another program.
- Lines are read from standard input, so a script can be piped in.
- `-timeout` bounds one turn (ten minutes by default).
- The key needs `sandboxes.read`, `sandboxes.write` and `exec`.

## How fast, and why

On server02, where the filesystem holding the disks cannot share blocks between
files:

| | ready |
|---|---|
| first turn: a new, empty volume is made | 170–199 ms |
| a later turn, volume holding a few MB | 132–187 ms |
| a later turn, volume holding 1 GiB | 147–184 ms |
| dropping a turn's sandbox | 61–98 ms |

The size of the volume makes no difference, because nothing is copied. The
volume is a file on the host, and the next sandbox is handed that file as its
disk, laid over the same read-only image as every other sandbox.

The price is that **one sandbox holds a volume at a time**. A second create
naming it while a turn is running gets `409 volume_in_use`. The drop returns
once the machine is down and the volume is free, so the next turn can follow it
immediately.

A volume also **belongs to the image it was first laid over**, by digest. It
holds changes to that image's filesystem, such as a package database or markers
for deleted files, and over another image those describe a machine nobody built.
If the tag is pushed over, the next turn fails with that reason. To continue,
pin the image by digest, or `/reset`.

## Seeing the volumes

The console's **Volumes** page lists every volume on the host: the sandbox
holding it, the image it belongs to, and what it holds. A volume no sandbox
holds can be deleted there. `GET /v1/volumes` and `DELETE /v1/volumes/{name}`
are the same thing through the API.
