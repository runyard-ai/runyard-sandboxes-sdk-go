# Points the Claude CLI at the sandbox's Claude broker, when it has one.
#
# The broker's address is published by runyard-sandboxes in
# /run/runyard/brokers.env — the same file a runyard box has. The token set
# here is a placeholder, and deliberately so: whatever this sandbox sends, the
# broker takes off and replaces with the real credential, which only the host
# holds.
if [ -r /run/runyard/brokers.env ]; then
  . /run/runyard/brokers.env
fi
if [ -n "${RUNYARD_BROKER_CLAUDE_URL:-}" ]; then
  export ANTHROPIC_BASE_URL="$RUNYARD_BROKER_CLAUDE_URL"
  export ANTHROPIC_AUTH_TOKEN="brokered-by-the-host"
fi
# Everything the CLI would reach other than the API is unreachable from a
# sandbox anyway; saying so stops it trying, and waiting, on every start.
export CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1
export DISABLE_AUTOUPDATER=1
