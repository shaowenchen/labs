#!/usr/bin/env bash
#
# The real end-to-end path: build the service, start an environment on demand,
# and take a lab from it.
#
# It needs one thing from you and does nothing without it:
#
#   LABS_GITHUB_TOKEN   a token with Actions: write on the repositories
#
# Everything else takes the default — the repositories (both projects), the key
# both sides use, and the domains. Set LABS_REPOS only to narrow it to one
# repository. It runs the service on a port of its own, so it does not touch a
# real deployment.
#
# The first run is slow: creating the lab is what dispatches the run, and the
# environment is about ten minutes of cluster and control-plane boot. The script
# polls for it and says so as it waits.
set -euo pipefail

: "${LABS_GITHUB_TOKEN:?set LABS_GITHUB_TOKEN to a token with Actions: write}"

PORT="${LABS_SMOKE_PORT:-18080}"
BASE="http://127.0.0.1:${PORT}"

echo "==> building"
make build >/dev/null

echo "==> starting labs on ${BASE}"
LABS_LISTEN=":${PORT}" \
LABS_LOG_LEVEL=debug \
  ./bin/labs &
PID=$!
trap 'kill "$PID" 2>/dev/null || true' EXIT

# Wait for the process to answer at all.
for _ in $(seq 1 30); do
  curl -fsS "${BASE}/healthz" >/dev/null 2>&1 && break
  sleep 1
done

# Ask for a lab. This is what checks whether an environment is already running
# and, if not, dispatches one — so it comes first, and the boot is waited for
# after it. Nothing is dispatched on a timer.
echo "==> asking for a lab (this dispatches a run if none is up)"
attempt=0
while :; do
  attempt=$((attempt + 1))
  code="$(curl -s -o /tmp/labs-smoke.json -w '%{http_code}' -X POST "${BASE}/api/v1/labs" \
    -H 'Content-Type: application/json' -d '{}')"
  if [ "$code" = 201 ]; then
    break
  fi
  # 503 retryable: an environment is starting, which is the ordinary answer for
  # the first several minutes. Anything else is a real failure.
  if [ "$code" != 503 ]; then
    echo "POST /api/v1/labs answered ${code}; here is the body:"
    cat /tmp/labs-smoke.json; echo; exit 1
  fi
  if [ "$attempt" = 1 ]; then
    echo "==> no environment is up; one was dispatched and is booting (about ten minutes)"
    echo "    watch the run, and the dispatch lines in this service's log, as it comes up"
  fi
  if [ "$attempt" -gt 120 ]; then
    echo "no environment became ready; here is what the service thinks:"
    curl -s "${BASE}/api/v1/config" || true
    echo; exit 1
  fi
  sleep 10
done
response="$(cat /tmp/labs-smoke.json)"
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
