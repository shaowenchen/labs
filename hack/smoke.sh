#!/usr/bin/env bash
#
# The real end-to-end path: build the service, point it at a real repository,
# watch it keep an environment warm, and take a lab from it.
#
# It needs three things from you and does nothing without them:
#
#   LABS_GITHUB_TOKEN   a token with Actions: write on the repository
#   LABS_REPOS          owner/repo, comma-separated
#   LABS_ENVIRONMENTS   the environment JSON (see .env.example)
#   LABS_KEY_<ID>       the environment's admin key, matching the repo secret
#
# Everything else takes the default. It runs the service on a port of its own and
# a state file under ./bin, so it does not touch a real deployment.
#
# The first run is slow: the environment has to be started, which is about ten
# minutes of cluster and control-plane boot. The script polls for it and says so
# as it waits.
set -euo pipefail

: "${LABS_GITHUB_TOKEN:?set LABS_GITHUB_TOKEN to a token with Actions: write}"
: "${LABS_REPOS:?set LABS_REPOS, e.g. shaowenchen/applab}"
: "${LABS_ENVIRONMENTS:?set LABS_ENVIRONMENTS; see .env.example}"

PORT="${LABS_SMOKE_PORT:-18080}"
BASE="http://127.0.0.1:${PORT}"
STATE="$(pwd)/bin/smoke-state.json"
mkdir -p "$(pwd)/bin"

echo "==> building"
make build >/dev/null

echo "==> starting labs on ${BASE}"
LABS_LISTEN=":${PORT}" \
LABS_STATE_FILE="${STATE}" \
LABS_LOG_LEVEL=debug \
  ./bin/labs &
PID=$!
trap 'kill "$PID" 2>/dev/null || true' EXIT

# Wait for the process to answer at all, then for an environment to be up.
for _ in $(seq 1 30); do
  curl -fsS "${BASE}/healthz" >/dev/null 2>&1 && break
  sleep 1
done

echo "==> waiting for an environment to come up (the keeper dispatches it; this can take ~10 minutes)"
ready=0
for _ in $(seq 1 90); do
  if curl -fsS "${BASE}/readyz" >/dev/null 2>&1; then ready=1; break; fi
  sleep 10
done
if [ "$ready" != 1 ]; then
  echo "no environment became ready; here is what the service thinks:"
  curl -s "${BASE}/api/v1/config" || true
  echo
  echo "check the keeper's log lines above for a dispatch that failed."
  exit 1
fi

echo "==> asking for a lab"
response="$(curl -fsS -X POST "${BASE}/api/v1/labs" -H 'Content-Type: application/json' -d '{}')"
echo "$response"

console="$(printf '%s' "$response" | sed -n 's/.*"console_url":"\([^"]*\)".*/\1/p')"
key="$(printf '%s' "$response" | sed -n 's/.*"api_key":"\([^"]*\)".*/\1/p')"
id="$(printf '%s' "$response" | sed -n 's/.*"session_id":"\([^"]*\)".*/\1/p')"

cat <<EOF

==> got a lab
    console : ${console}
    api key : ${key}
    session : ${id}

Open the console and paste the key. To end it now:

    curl -sX DELETE ${BASE}/api/v1/labs/${id}

Press Ctrl-C to stop the service (the lab's key is rotated away when it expires,
or when you delete it; stopping the service does not end the lab).
EOF

wait "$PID"
