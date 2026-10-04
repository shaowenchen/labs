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

## Starting an environment, on demand

A request for a lab that finds nothing running **starts an environment itself**.
That is what lets the service run on a host that starts it per request: the
background keeper may not be running there, but a request is, and a request is
enough to dispatch the workflow. The environment takes a few minutes to boot, so
that request answers "one is being started" and the environment it started is
there for the next one.

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
turns the writer off: an environment is then started only when a request needs
one, which costs a few minutes on the first request after a quiet spell but
holds no runner at all in between.

To remove the boot gap you need two environments of the same kind overlapping:
while one drains, the other is already up. The workflows' `concurrency` group
currently keys on the repository alone, so two runs of the same workflow cannot
coexist — one line changes that:

```yaml
concurrency:
  group: applab-debugger-${{ github.repository }}-${{ inputs.domain }}
```

With that, configure two repositories' worth of environments on different
domains and the keeper holds them in antiphase.

## What you have to do in the other repositories

**applab — nothing.** labs dispatches `debugger.yml` with the key it generated
as the `api_key` input. applab's workflow already threads that through
(`inputs.api_key || secrets.APPLAB_API_KEY`), so the environment comes up
configured with the key labs holds, and there is no secret to set. The only
other thing the environment needs is the tunnel credential it already uses —
`CLOUDFLARE_TOKEN` — and a named tunnel whose hostname matches the domain you
configured here.

**sandboxlab — one line, then nothing.** The action already accepts an `api_key`
input (`action/action.yml:14`) and installs it into the chart, but the debugger
workflow neither declares one nor passes it down — so as it stands a dispatched
key has no way in. Declare the input alongside the others, and pass it to the
action:

```yaml
on:
  workflow_dispatch:
    inputs:
      api_key:
        required: false
        default: ''
      # ... session_hours, tunnel, domain
```

```yaml
      - uses: ./action
        with:
          api_key: ${{ inputs.api_key }}
          # ... session_hours, tunnel, cloudflare_token, domain
```

There is no secret to create: the key is a non-sensitive value labs generates
for the deployment and passes at dispatch. sandboxlab's `domain` input is a
choice — only `sandboxlab-1.chenshaowen.com` and `sandboxlab-2.chenshaowen.com`
are accepted — so a named tunnel must serve one of those hostnames, and labs is
pointed at it with `LABS_DOMAIN_SANDBOXLAB` (or a suffix that produces it); the
default here is the second.

**Do this before adding sandboxlab to `LABS_REPOS`.** GitHub rejects a dispatch
that carries an input the workflow does not declare — the whole dispatch fails,
nobody starts a run, and labs sees `422`. Add the repository to `LABS_REPOS`
once the input is declared (or set `LABS_KEY_SANDBOXLAB` and skip the dispatch
key entirely, if you would rather not touch the workflow).

Because sandboxlab has one key for the whole deployment, a lab handed out from
it carries that key and the warning that says so. sandboxlab removed per-user
keys upstream, so this is what the deployment supports; a returned per-user key
would change only the sandboxlab driver.

**Either one — one line, for blue/green.** The `concurrency.group` change above,
which lets two environments of the same kind run at once.

**If you would rather not have labs generate a key** — for instance because you
rotate it yourself — set the repository secret (`APPLAB_API_KEY`, or
`SANDBOXLAB_API_KEY`) and put the same value in `LABS_KEY_APPLAB`
(`LABS_KEY_SANDBOXLAB`). Then labs uses that key and does not send an `api_key`
input.

## Configuration

See [`.env.example`](.env.example) for every variable and what it is for.
Everything is an environment variable; there is no config file.

**There is no environment list to write.** One entry in `LABS_REPOS` is one
environment, and everything a repository implies — the project (`applab` or
`sandboxlab`, from the name), the served path (`/applab`, `/sandbox`) and the
workflow that brings it up — is derived from it. The key is generated for you.

```bash
LABS_GITHUB_TOKEN=...                          # read and write on the repos below
LABS_REPOS=shaowenchen/applab,shaowenchen/sandboxlab  # one entry = one environment
```

That is the whole configuration. The environment's **address is not configured
either** — it is read back from the environment's own run log, where the
debugger workflow prints it (`Open the console: <url>`). That is the only way to
learn a hostname that belongs to whatever tunnel the deployment owns, and it
means there is nothing to type in and nothing to keep in sync.

Two optional overrides, named for the repository with its name uppercased
(`APPLAB` for `shaowenchen/applab`):

- **`LABS_DOMAIN_APPLAB`** (or `LABS_DOMAIN_SUFFIX`, a shared suffix under which
  a repository named `applab` is served at `applab.<suffix>`) — set the address
  instead of discovering it, so an environment is reachable before its run has
  printed anything.
- **`LABS_KEY_APPLAB`** — use a key configured out of band, for instance one you
  rotate yourself, instead of a generated one.

Whatever the address, it must be the hostname of a **named** Cloudflare tunnel,
which is stable across runs; a quick tunnel is assigned a new hostname each time.
Without a configured domain this only matters for discovery, which caches the
address for the life of a run.

**A missing variable does not stop the service.** It starts, answers `/healthz`,
and reports what is wrong through its log, `/readyz` and `GET /api/v1/config` —
so a container that is not configured yet is a container that says so, rather
than one that exits with a code indistinguishable from a crash. The only thing
still fatal is a variable set to an unreadable value (a `LABS_SESSION_TTL` of
`two hours`), because continuing would silently substitute the default for the
value that was meant.

It will not dispatch anything or hand out a lab until the configuration is
complete: `/readyz` stays `503`, `POST /api/v1/labs` says the deployment is not
configured, and nothing is started — neither the keeper nor an on-demand start.

Two things are worth knowing at the top:

- **labs chooses the environment's key.** Neither control plane reports its key
  over its unauthenticated `/api/v1/config` (nor should it), so a key cannot be
  discovered — labs generates one and hands it to the environment as the
  dispatch's `api_key` input. A key you would rather manage yourself overrides
  that with `LABS_KEY_<ID>`, in which case it is set on both sides and not sent.
- **The domain must be stable.** labs finds an environment by polling
  `GET <domain><path>/api/v1/config`, which only works if the hostname is the
  same across runs. That is what a *named* Cloudflare tunnel gives; a quick
  tunnel is assigned a new hostname each run and labs would never find it.

## Running it

It is a **long-lived process**, not a serverless function. Two loops run for the
life of the process — the keeper that dispatches successor runs, and the reaper
that expires sessions every thirty seconds — and there is no timer in a
request-scoped runtime for either to run in. Put it on a host, a VM or a
Kubernetes cluster, not on a platform that starts it per request.

```bash
cp .env.example .env      # fill it in
docker compose up -d --build
```

Caddy terminates TLS for the domain in the `Caddyfile` and is the only service
that publishes a port; labs is reachable only on the compose network, which is
what makes `LABS_TRUSTED_PROXY=true` in the compose file correct. Point the
domain's DNS at the host first. Nothing else is needed: there is no database and
no volume.

Session state is in memory, and deliberately so — a lab is disposable and lasts
two hours, so the service keeps only what it needs to expire and free what it
handed out. Two consequences are worth knowing. A restart forgets the live
sessions, which is the safe direction: reconciliation then rotates every
outstanding credential away, so a restart can cut a lab short but cannot leave
one working. And the state is per-process, so the service runs **one replica**;
two would each hold half the sessions and hand the same slot out twice.

Without docker:

```bash
make build
./bin/labs
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
- a restart forgets every live session, so reconciliation rotates them all away
  rather than leaving a credential working;
- `GET` never returns a key, and `DELETE` is idempotent.

`hack/smoke.sh` is the real end-to-end path, for when you have a token and a
repository and want to watch it work.

## License

See [LICENSE](LICENSE).
