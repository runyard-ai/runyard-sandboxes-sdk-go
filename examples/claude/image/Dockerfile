# A sandbox image with the Claude CLI in it.
#
# It knows nothing about runyard-sandboxes beyond one convention: a sandbox
# given a broker called `claude` finds its address in /run/runyard/brokers.env,
# and claude-broker.sh points the CLI there. There is no API key in this image
# and there cannot be one in a sandbox made from it — the key is on the host,
# held by the Claude broker, and presented on the sandbox's behalf.
#
#   docker build -t localhost:5001/runyard/claude-sandbox:latest .
#   docker push localhost:5001/runyard/claude-sandbox:latest
FROM node:22-bookworm-slim

# What a person at a shell inside a sandbox expects to find, and what Claude
# Code's own tools shell out to.
RUN apt-get update \
 && apt-get install -y --no-install-recommends bash ca-certificates curl git less procps ripgrep vim-tiny \
 && rm -rf /var/lib/apt/lists/*

ARG CLAUDE_CODE_VERSION=latest
RUN npm install -g @anthropic-ai/claude-code@${CLAUDE_CODE_VERSION} \
 && npm cache clean --force

# Sourced by every login shell — which is what the console's terminal opens.
COPY claude-broker.sh /etc/profile.d/claude-broker.sh

# Root, inside the machine. The VM is the boundary here, not a uid within it —
# and a daemon that builds root filesystems without privilege cannot yet keep
# an image's file owners, so a non-root user would find its own home owned by
# somebody else and Claude unable to save anything in it.
COPY claude.json /root/.claude.json
WORKDIR /root

# Nothing runs on its own. A sandbox of this image is somewhere to open a
# terminal and type `claude`.
CMD ["sleep", "infinity"]
