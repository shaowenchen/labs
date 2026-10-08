#!/usr/bin/env bash
#
# Delete a Docker Hub repository's older tags, keeping the newest few.
#
# Why this exists: Docker Hub has no tag retention on any plan. There is no
# setting to turn on and no rule to write — the Hub's own docs describe deleting
# tags by hand and nothing else — so pruning is something the publisher does.
# This is that, run after the image is pushed.
#
# It never touches `latest`: that is the tag a bare `docker pull` takes, and
# deleting it breaks the pull for everyone until the next push. What it keeps is
# the newest KEEP tags besides it.
#
# Credentials come from the environment:
#
#   DOCKERHUB_USERNAME   the account
#   DOCKERHUB_TOKEN      a personal access token with Read & write
#
# Usage:
#
#   prune-dockerhub-tags.sh <owner>/<repository> [keep]
#
# A tag that will not delete is reported and skipped, not fatal: the usual
# reason is that it is immutable, which the Hub refuses to delete by design, and
# that is no reason to fail a deployment that already succeeded. What is fatal
# is not being able to read the repository at all — a login or a listing that
# failed means the decision of what to keep was never made, and silently
# deleting nothing would hide that.
set -euo pipefail

REPO_SLUG=${1:?usage: prune-dockerhub-tags.sh <owner>/<repository> [keep]}
KEEP=${2:-3}

: "${DOCKERHUB_USERNAME:?set DOCKERHUB_USERNAME}"
: "${DOCKERHUB_TOKEN:?set DOCKERHUB_TOKEN}"

case $REPO_SLUG in
  */*) ;;
  *) echo "expected <owner>/<repository>, got '$REPO_SLUG'" >&2; exit 2 ;;
esac

tags_url="https://hub.docker.com/v2/repositories/${REPO_SLUG}/tags"
hdrs=$(mktemp)
trap 'rm -f "$hdrs"' EXIT

# ── log in ──────────────────────────────────────────────────────────────────
# The PAT goes in the password field; that is the documented way to use a token
# here, and it is what Docker's own hub-tool does. The reply's `token` is a JWT
# and is sent back as a Bearer token — not the `JWT` scheme older docs described.
token=$(curl -fsS -X POST https://hub.docker.com/v2/users/login/ \
    -H 'Content-Type: application/json' \
    -d "$(jq -nc --arg u "$DOCKERHUB_USERNAME" --arg p "$DOCKERHUB_TOKEN" \
        '{username: $u, password: $p}')" \
  | jq -r '.token // empty')
if [ -z "$token" ]; then
  echo "could not log in to Docker Hub as ${DOCKERHUB_USERNAME}" >&2
  exit 1
fi

# ── list every tag, newest first ────────────────────────────────────────────
# Every page is read before anything is deleted. Deleting while paging would
# shift the pages underneath the cursor and skip tags.
#
# The ordering is done here rather than trusted from the API. `ordering=
# last_updated` does return newest-first today, but it is a query parameter on a
# route whose documented behaviour is thinner than its actual one, and the
# direction is easy to get backwards — sorting the timestamps ourselves costs
# nothing and cannot be silently wrong.
listing=$(mktemp)
trap 'rm -f "$hdrs" "$listing"' EXIT

page="${tags_url}/?page_size=100&ordering=last_updated"
while [ -n "$page" ] && [ "$page" != null ]; do
  body=$(curl -fsS -H "Authorization: Bearer ${token}" "$page")
  jq -r '.results[] | "\(.last_updated)\t\(.name)"' <<<"$body" >>"$listing"
  page=$(jq -r '.next // empty' <<<"$body")
done

# Newest first. The timestamps are RFC3339 UTC, so sorting them as strings sorts
# them as times.
names=()
while IFS= read -r name; do
  names+=("$name")
done < <(sort -r "$listing" | cut -f2)

if [ "${#names[@]}" -eq 0 ]; then
  echo "no tags in ${REPO_SLUG}" >&2
  exit 0
fi

# ── delete all but the newest KEEP ──────────────────────────────────────────
echo "${REPO_SLUG}: ${#names[@]} tags, keeping the newest ${KEEP} besides latest"

delete_tag() {
  local name=$1 attempt=0 code wait
  while :; do
    # -o /dev/null with -w so the status is read without consuming the (empty)
    # body, and without -f so a 4xx is a value to judge rather than an exit.
    code=$(curl -sS -X DELETE -D "$hdrs" -o /dev/null -w '%{http_code}' \
      -H "Authorization: Bearer ${token}" "${tags_url}/${name}/")
    case $code in
      2*)
        echo "  deleted ${name}"
        return 0
        ;;
      429)
        # The rate limiter says how long to wait; honour it rather than guess.
        wait=$(awk -F': ' 'tolower($1) == "retry-after" {print $2}' "$hdrs" | tr -d '\r')
        wait=${wait:-5}
        attempt=$((attempt + 1))
        if [ "$attempt" -gt 5 ]; then
          echo "  ${name}: still rate limited after ${attempt} tries; leaving it" >&2
          return 0
        fi
        echo "  rate limited; waiting ${wait}s" >&2
        sleep "$wait"
        ;;
      *)
        echo "  ${name}: not deleted (HTTP ${code}) — immutable tags cannot be; leaving it" >&2
        return 0
        ;;
    esac
  done
}

kept=0
for name in "${names[@]}"; do
  [ "$name" = latest ] && continue
  kept=$((kept + 1))
  [ "$kept" -le "$KEEP" ] && continue
  delete_tag "$name"
done
