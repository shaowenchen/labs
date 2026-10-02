# labs

Hand out a working [applab](https://github.com/shaowenchen/applab) or
[sandboxlab](https://github.com/shaowenchen/sandboxlab) environment to anyone
who asks, for a couple of hours, with no account and no login.

```
$ curl -sX POST https://labs.example.com/api/v1/labs -d '{}'
{
  "data": {
    "session_id": "9f2c1a...e1",
    "kind": "applab",
    "console_url": "https://applab-1.example.com/applab",
    "api_key": "3d7b...",
    "expires_at": "2026-10-02T21:36:00Z",
    "links": {"console": "https://applab-1.example.com/applab"}
  }
}
```

Open the console URL, paste the key, and you have an environment. Two hours
later the key stops working and the environment is freed for the next caller.

## Why it exists, and what it is not

applab and sandboxlab each ship a **debugger environment**: a GitHub Action that
builds a throwaway Kubernetes cluster, installs the control plane, publishes it
through a tunnel, and prints a console link and an API key to the run's summary.
It is started by hand, it lives for a few hours, and everything is deleted when
the run ends.

That is a good way to try the software and a bad way to hand it to someone: the
link is behind a GitHub login, the key has to be found and passed along, and
nobody is keeping the environment up.

`labs` closes that gap. It is a small service that:

- keeps an environment **warm** by dispatching the debugger workflow again before
  the current run ends, so there is always one up;
- hands a caller a **link and a key** in one anonymous call;
- makes each delivered lab expire, by revoking the credential it minted.

It is deliberately **not** a proxy and not a multi-tenant platform. It does not
carry the caller's traffic after the hand-off, and the environments are shared
and disposable — a lab is a place to try something, not a place to keep
something.

## How a lab is made

```
caller ──POST /api/v1/labs──▶ labs
                               │  reserve a free slot in a warm environment
                               │  rotate that slot's app key  ──▶ applab  (admin key)
                               ▼
                          { console_url, api_key, expires_at }  ──▶ the caller
                               │
            2h later, reaper ──┘  rotate the key away ──▶ applab
                                  stop the app, free the slot
```

The credential is the interesting part. applab issues a **per-app key** that
reaches exactly one app, and rotating it invalidates the previous value
immediately. So a session's key is a freshly rotated key for the slot it holds,
and ending a session is another rotation. No two callers ever hold the same key,
and the rotation happens *before* the app is stopped — so even if the teardown
half fails, no one is left holding a working credential.

The slot is a named app id from the environment's configuration. Reserving one
happens under the same lock that records the session, so two requests arriving
together take two different slots, and a request that fails to mint a key gives
its slot back rather than leaking it.

## Keeping the environment warm

A GitHub-hosted job cannot run for more than six hours, and an environment is
expected to be reachable for much longer than that, so the keeper dispatches a
**successor run** shortly before the current one ends. The successor waits in
the queue and starts the moment the current run stops.

There is a rule the keeper exists around. GitHub's default concurrency queue
holds a single pending run, and **a new dispatch cancels the one already
waiting**. A keeper that dispatched on every tick would therefore cancel its own
successor, forever, and the environment would never come back. So the keeper
never dispatches while a run is queued, and the check and the dispatch happen
under one lock per environment. It is the first thing the tests cover.

There is still a **gap** when one run ends and the next boots: the runner has to
create the cluster and install the control plane, which takes about ten minutes.
During it, `POST /api/v1/labs` answers `503` with a `Retry-After`, and `/readyz`
says which environment is missing. Closing the gap entirely needs two
environments of the same kind running in antiphase — see
[Warmth, and what it costs](#warmth-and-what-it-costs).

## The API

Everything is anonymous. There is no key to hold and nothing to log in to.

| Method | Path | What it does |
|---|---|---|
| `POST` | `/api/v1/labs` | create a lab; body `{"kind":"applab"}` is optional |
| `GET` | `/api/v1/labs/{id}` | read a lab back — never the key |
| `DELETE` | `/api/v1/labs/{id}` | end a lab early |
| `GET` | `/api/v1/config` | the deployment's shape and its environments |
| `GET` | `/api/v1/describe` | the endpoint list and the contract |
| `GET` | `/healthz`, `/readyz` | liveness; whether any environment is up |
| `GET` | `/` | a one-page console with a "Get a lab" button |

Success is the payload under `data`; failure is `{"error": "...", "retryable":
bool}`. The statuses a caller branches on:

- `201` — the lab, with its key. The only response that carries a key: a
  `GET` returns the session without it, so an id that leaks does not leak a
  credential.
- `429` — this address has had its share (either too many requests, or it
  already holds a lab). `Retry-After` says when to come back.
- `503` — no environment is up, or they are all in use. Retryable; a
  `Retry-After` says roughly when.

The session id is 16 bytes from `crypto/rand`, and it is what a `GET`/`DELETE`
presents — so it is compared in constant time, and it is the whole of a caller's
authority over their own lab.

### Limits

Anonymous access needs a ceiling. Three, all configurable:

- a **rate limit** on `POST /api/v1/labs` per address, a fixed window
  (`LABS_RATE_LIMIT_COUNT` per `LABS_RATE_LIMIT_WINDOW`, five an hour by
  default);
- a **concurrent-session cap per address** (`LABS_MAX_SESSIONS_PER_IP`, one by
  default), counted from live sessions so it heals when one expires;
- a **global cap** (`LABS_MAX_SESSIONS`, defaulting to the sum of the
  environments' capacities).

The client address comes from the connection unless `LABS_TRUSTED_PROXY` is set,
in which case `X-Forwarded-For` is believed. That flag is off by default for a
reason: trusting the header from the open internet makes every limit bypassable
by sending one, so it should only be on when the service sits behind a proxy
that sets it *and* the service's own port is not reachable directly.

## Warmth, and what it costs

Keeping an environment warm means holding a GitHub-hosted runner for as long as
the environment is up. At a four-hour run replaced continuously, that is roughly
**130 runner-hours a month, per warm environment** — which matters on a private
repository and on the free tier (2,000 minutes a month). `LABS_KEEPWARM=false`
turns the keeper off.

To remove the boot gap you need two environments of the same kind overlapping:
while one drains, the other is already up. The workflows' `concurrency` group
currently keys on the repository alone, so two runs of the same workflow cannot
coexist — one line changes that:

```yaml
concurrency:
  group: applab-debugger-${{ github.repository }}-${{ inputs.domain }}
```

With that, configure `applab-1` and `applab-2` on different domains and the
keeper holds them in antiphase.

## What you have to do in the other repositories

labs changes nothing in applab or sandboxlab. What it needs from them is
configuration, and one small edit:

**applab — a secret, no file change.** Set the repository secret
`APPLAB_API_KEY` to a fixed value and put the same value in `LABS_KEY_APPLAB_1`.
The existing `debugger.yml` already reads it (`inputs.api_key ||
secrets.APPLAB_API_KEY`), so an environment started without an `api_key` input
uses it — and labs never sends one, so the value does not appear in a dispatch
payload. Without this, every run generates a random key that labs cannot know.

**applab — one line, for blue/green.** The `concurrency.group` change above.

**sandboxlab — not yet.** Its driver is not implemented; a configuration naming
a sandboxlab environment is refused at startup. When it is added, sandboxlab
currently has a single deployment-wide key with no per-user keys (they were
removed upstream), so its sessions would share one credential — a real
limitation, not a detail, and the reason applab came first.

## Configuration

See [`.env.example`](.env.example) for every variable and what it is for.
Everything is an environment variable; there is no config file. A
configuration that cannot work is refused at startup with the variable named.

Two things are worth knowing at the top:

- **The environment's key is static and configured on both sides.** Neither
  control plane reports its key over its unauthenticated `/api/v1/config` (nor
  should it), so labs cannot discover it — it is the same value in the
  repository secret and in `LABS_KEY_<ID>`.
- **The domain must be stable.** labs finds an environment by polling
  `GET <domain><base_path>/api/v1/config`, which works because a *named*
  Cloudflare tunnel keeps its hostname across runs. A quick tunnel would give a
  new address each run and labs would never find it.

## Running it

```bash
cp .env.example .env      # fill it in
docker compose up -d --build
```

Caddy terminates TLS for the domain in the `Caddyfile` and is the only service
that publishes a port; labs is reachable only on the compose network, which is
what makes `LABS_TRUSTED_PROXY=true` in the compose file correct. Point the
domain's DNS at the host first.

Without docker:

```bash
make build
LABS_STATE_FILE=./state.json ./bin/labs
```

It starts even with no reachable environment: `/healthz` answers, and `/readyz`
and `GET /api/v1/config` say what is not ready and why.

## Development

```bash
make check        # gofmt, vet, tests — the gate CI runs
make test-race    # the store and the limiter under the race detector
make build
```

The tests need no cluster, no GitHub and no tunnel. GitHub is a fake
`httptest.Server` that records dispatches and serves a scripted run list; the
environments are fake servers that speak just enough of each control plane's
API. What they pin down is the behaviour that is expensive to get wrong:

- the keeper never dispatches over a queued run (the bug that would wedge an
  environment permanently);
- a failed provision gives its slot back;
- the state file carries no credential;
- `GET` never returns a key, and `DELETE` is idempotent.

`hack/smoke.sh` is the real end-to-end path, for when you have a token and a
repository and want to watch it work.

## License

See [LICENSE](LICENSE).
