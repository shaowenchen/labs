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

- brings an environment **up on demand**: a repository runs one environment at a
  time, so when a lab is asked for it checks whether one is already running and
  uses it, dispatching a run only if none is;
- hands a caller a **link and a key** in one anonymous call;
- makes each delivered lab expire, by revoking the credential it minted.

It is deliberately **not** a proxy and not a multi-tenant platform. It does not
carry the caller's traffic after the hand-off, and the environments are shared
and disposable — a lab is a place to try something, not a place to keep
something.

## How a lab is made

```
caller ──POST /api/v1/labs──▶ labs
                               │  reserve a free slot in the environment (starting one if none is up)
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

There is no timer. A repository runs **one environment at a time** — its
workflow's `concurrency` group allows a single run — so "is it up" is a question
asked when a lab is wanted, not something to poll for. When a lab is created:

- **a run is already queued or in progress** — that *is* the environment, whoever
  started it (this service a minute ago, or a person by hand this morning), and
  it is used as it is. Nothing is dispatched, because a second dispatch would
  replace the first rather than add to it.
- **nothing is running** — a run is dispatched. It takes a few minutes to boot,
  so that request answers "one is being started" and the environment it started
  is there for the next one.
- **the run listing fails** — nothing is dispatched. A blind dispatch could
  cancel a run that is actually live; the caller can try again.

The check and the dispatch happen under one lock per environment, so two
requests arriving together cannot both see "nothing running" and both dispatch.

The run's own `session_hours` (default `4`) is how long it lives; when it ends,
the environment is gone until the next request starts another. That is the
trade: no runner is held between requests, and a quiet deployment costs nothing.

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

## What an environment costs

An environment is a GitHub-hosted runner held for as long as it is up — at a
four-hour run, roughly **130 runner-hours a month per environment that is up
continuously**. On a private repository or the free tier (2,000 minutes a
month), that is the number to watch.

labs holds nothing in between. It never dispatches on a timer: it checks, and
only starts an environment when a lab is asked for and none is running. A quiet
deployment therefore spends nothing, and pays a few minutes of cold boot on the
first request after a quiet spell.

There is no second environment to overlap. A repository's workflow runs one
environment at a time — its `concurrency` group allows a single run — so a
second dispatch would replace the first rather than add to it. If you ever want
blue/green, that is a change on the workflow side (key the `concurrency` group
on the domain as well) plus a second repository, not a labs setting.

## What you have to do in the other repositories

**One key, sent in every dispatch.** labs calls every environment with one key —
`ADMIN_KEY` when it is set, otherwise one it generated — and passes it as the
`api_key` input on every dispatch. That one key, on both sides, is all it takes
to drive any lab action: both workflows thread it through
(`api_key: ${{ inputs.api_key || secrets.<THEIR_SECRET> }}`), so the environment
comes up holding the key labs will call it with. Set it to the same value as each
repository's own secret — `APPLAB_API_KEY` for applab, and for sandboxlab the
secret of the same name as this variable — and one key covers both kinds.

**There is no built-in key.** labs used to fall back to a constant compiled into
this repository, which let a deployment run with nothing configured. It is gone:
a key checked into a repository is not a secret, so anyone who could read it
could call any deployment that had not set one. Instead, `ADMIN_KEY` is
optional — leave it out and the service generates a random one at startup, so a
deployment can come up with no key configured at all and still hand out labs it
started itself. What that costs is reach: a key that is random per process cannot
reach an environment an earlier process brought up, nor one started by hand, so
set the variable to the repositories' key when you want labs to reach an
environment already running. A per-environment `LABS_KEY_<ID>` still works for
the one environment that wants its own.

The other thing each environment needs is a named Cloudflare tunnel whose
hostname matches the domain you configured here, and — for applab —
`CLOUDFLARE_TOKEN`, the tunnel credential it already uses.

sandboxlab's `domain` input is a choice — only `sandboxlab-1.chenshaowen.com`
and `sandboxlab-2.chenshaowen.com` are accepted — so a named tunnel must serve
one of those hostnames, and labs is pointed at it with `LABS_DOMAIN_SANDBOXLAB`
(or a suffix that produces it); the default here is the first.

Because sandboxlab has one key for the whole deployment, a lab handed out from
it carries that key and the warning that says so. sandboxlab removed per-user
keys upstream, so this is what the deployment supports; a returned per-user key
would change only the sandboxlab driver.

**Either one — one line, for blue/green.** The `concurrency.group` change above,
which lets two environments of the same kind run at once.

**The key is a pair, and both halves have to match.** `ADMIN_KEY`
(or `LABS_KEY_<ID>` for one environment) is the value labs sends at dispatch;
the repository's own secret — `APPLAB_API_KEY` for applab, and `ADMIN_KEY` for
sandboxlab, where it is the same name as this variable — is what its workflow
falls back to when a dispatch carries nothing. They are not alternatives: the
environment is called with whatever the dispatch carried, so a value in only one
of the two places is a live environment reachable by only one of the two sides.

## Configuration

See [`.env.example`](.env.example) for every variable and what it is for.
Everything is an environment variable; there is no config file.

**There is no environment list to write.** One entry in a repository variable is
one environment, and everything a repository implies — the served path
(`/applab`, `/sandbox`) and the workflow that brings it up — is derived. One
fixed key covers every kind.

There is **one variable per kind**, and which variable a repository comes from
is what decides its kind:

```bash
GITHUB_TOKEN=...                                # the only variable required
LABS_APPLAB_REPOS=shaowenchen/applab                 # applab, defaults to this
LABS_SANDBOXLAB_REPOS=shaowenchen/sandboxlab         # sandboxlab, defaults to this
```

They are separate on purpose. A single list covering both kinds was the earlier
shape, and it had a bad failure: setting it to one repository silently dropped
the other kind, so a deployment that looked configured was serving half of what
it should. With one variable each, configuring applab can never hide sandboxlab.
Leave a variable unset to take its default; set it to empty to turn that kind
off on purpose.

That is the whole configuration, except the key. The **address** defaults to the
hostname each project's own debugger workflow starts on
(`applab-1.chenshaowen.com`, `sandboxlab-1.chenshaowen.com`); the **key** is the
one thing that is neither derived nor required — leave it out and a random one is
generated for the process, set it and that value is used.

`ADMIN_KEY` is the exception to everything above: the repository list and the
address can be left out and derived, the key cannot be — it is either computed
(randomly, per process) or configured. A generated key is what lets a deployment
run with nothing set; a configured one is what lets it reach environments it did
not start itself. It carries the same name as sandboxlab's own repository secret,
so that half needs no mapping. The overrides below are named for the repository
with its name uppercased (`APPLAB` for `shaowenchen/applab`).

- **`ADMIN_KEY`** — optional. Unset, the service generates a random key at
  startup and says so in its log; set, that value is what labs calls every
  environment with. There is no built-in key. See "What you have to do in the
  other repositories" above for how each project is given it.
- **`LABS_DOMAIN_APPLAB`** (or `LABS_DOMAIN_SUFFIX`, a shared suffix under which
  a repository named `applab` is served at `applab.<suffix>`) — set the address
  instead of discovering it, so an environment is reachable before its run has
  printed anything.
- **`LABS_KEY_APPLAB`** — use a different key for this one environment, instead
  of `ADMIN_KEY`.

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
configured, and nothing is started — no environment, and no dispatch.

Two things are worth knowing at the top:

- **The key is neither fixed nor discovered.** Neither control plane reports its
  key over its unauthenticated `/api/v1/config` (nor should it), so labs cannot
  read it — it calls every environment with `ADMIN_KEY` when that is
  set, and with a key it generated itself when it is not. A generated key is
  random per process, so it reaches only the environments this process started;
  setting the variable is what lets labs reach an environment it did not itself
  just dispatch.
- **The domain must be stable.** labs reaches an environment at the hostname you
  configure, so it must be the same across runs — which a *named* Cloudflare
  tunnel gives, and a quick tunnel does not (it is assigned a new hostname each
  run).

## Running it

It is a **long-lived process**, not a serverless function. The reaper loop runs
for the life of the process, expiring sessions every thirty seconds, and the
session store and rate limiter are in-process; there is no timer in a
request-scoped runtime for any of it. Put it on a host, a VM or a Kubernetes
cluster, not on a platform that starts it per request.

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

### The image, from CI

Pushing to `master` or `main`, or pushing a `v*` tag, builds the image for
`linux/amd64` and `linux/arm64` and pushes them under one manifest, so a pull on
either architecture gets a native image. It is `docker.io/<owner>/labs`, the
same name the Makefile's `IMAGE` default uses, tagged `latest` and with the
version `git describe` reports.

The version and commit are stamped in exactly as the Makefile stamps them, so an
image built by CI and one built locally report the same thing.

It needs two repository secrets, both from Docker Hub, under **Settings →
Secrets and variables → Actions**:

| Secret | What it is |
| --- | --- |
| `DOCKERHUB_USERNAME` | the Docker Hub account name |
| `DOCKERHUB_TOKEN` | an access token with Read & write — never the account password |

Each push also prunes the tags it made stale, keeping `latest` and the newest
three. Docker Hub has no tag retention on any plan — its docs describe deleting
tags by hand — so the publisher does it: `hack/prune-dockerhub-tags.sh`, which is
worth reading if you want a different number kept. Immutable tags are left alone
rather than failing the run, and `latest` is never a candidate.

### On Vercel

The service runs there as a server, not as a function. `cmd/server/main.go` is
one of the entry points Vercel's Go framework preset looks for; it binds the
`PORT` Vercel gives it and Vercel routes every request to it with the path
unchanged, so the service's own routing sees `/` and `/api/v1/labs` exactly as it
does behind Caddy. `vercel.json` pins the framework and switches Vercel's own git
integration off, so a push deploys once — from here — rather than twice.

The build and the deploy are GitHub Actions' job. The `vercel` job in
`.github/workflows/ci.yml` runs `vercel build` on the runner and uploads only the
result, so the source is never sent to Vercel to be built there. It needs three
repository secrets:

| Secret | Where it comes from |
| --- | --- |
| `VERCEL_TOKEN` | `vercel.com/account/tokens` |
| `VERCEL_ORG_ID` | `vercel link` — the values land in `.vercel/project.json` |
| `VERCEL_PROJECT_ID` | the same file |

The project is `shaowenchens-projects/labs`; `VERCEL_ORG_ID` is the id of the
`shaowenchens-projects` team, so both ids come from linking this repository to
that project.

The service's own settings — `GITHUB_TOKEN`, the repositories, the key — are
the Vercel project's environment variables, set in the dashboard. There is no
`.env` in the deployment. Set `LABS_TRUSTED_PROXY=true` there: every request
reaches the service through Vercel's proxy and its port is not reachable
directly, which is the same case the compose file makes for Caddy.

**Set `ADMIN_KEY` here; do not leave it to be generated.** A generated
key is random per process, and on Vercel every cold start is a new process — so
each instance would mint its own key, and an environment one instance started
would refuse every other. It also cannot reach an environment an earlier
instance brought up. Setting the variable, to the same value the repositories
are given, is what makes the key stable across instances and restarts.

**What Vercel costs this service.** Read this before deploying there, because it
is the shape the service was built against rather than a detail to find later.
It is one long-lived process, and two of its properties do not survive a platform
that runs several instances and starts them cold:

- **The session store is in memory.** Two instances hold two different halves of
  the sessions, so two callers arriving on different instances can be handed the
  same applab slot and the same key. The concurrent-session caps are enforced
  against that same store, so they are per-instance too.
- **The reaper does not run.** It is a goroutine the process owns, and a process
  started per request has no life to own one. Expiry is then left to the
  environments: sandboxlab reaps its own sandboxes, but applab's keys do not
  expire on their own and live until the environment is replaced.
- **`DELETE /api/v1/labs/{id}` can be a no-op.** A release looks the session up in
  the instance's own store; a session created on a different instance is not
  there to find, so the credential is not revoked.

None of this makes the deployment unsafe — the console serves a static document
and no key is ever logged — but it makes the limits and the expiry best-effort
rather than the guarantees they are on a single host. The rest of this section
describes that host, which is the shape the service was built for.

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

- a run already queued or in progress is used as it is, never dispatched over
  (a second dispatch would replace it, wedging the environment);
- a failed provision gives its slot back;
- a restart forgets every live session, so reconciliation rotates them all away
  rather than leaving a credential working;
- `GET` never returns a key, and `DELETE` is idempotent.

`hack/smoke.sh` is the real end-to-end path, for when you have a token and a
repository and want to watch it work.

## License

See [LICENSE](LICENSE).
