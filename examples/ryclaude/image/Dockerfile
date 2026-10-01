# The image ryclaude makes its sandbox from: a machine with claude in it, and
# what claude's tools shell out to.
#
# Nothing here is anybody's: no credentials, no settings, no broker. ryclaude
# copies your credentials into the sandbox when it starts one, and they go
# with it. Each release of runyard-sandboxes publishes this image where anyone
# can pull it, with no key:
#
#   releases.runyard.ai/runyard-public/ryclaude
FROM node:22-bookworm-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends bash ca-certificates curl git less procps ripgrep vim-tiny \
 && rm -rf /var/lib/apt/lists/*

ARG CLAUDE_CODE_VERSION=latest
RUN npm install -g @anthropic-ai/claude-code@${CLAUDE_CODE_VERSION} \
 && npm cache clean --force

# Root, inside the machine. The VM is the boundary here, not a uid within it —
# and a daemon that builds root filesystems without privilege cannot yet keep
# an image's file owners, so a non-root user would find its own home owned by
# somebody else and claude unable to save anything in it.
WORKDIR /workspace

# Nothing runs on its own: ryclaude opens a terminal and runs claude in it.
CMD ["sleep", "infinity"]
