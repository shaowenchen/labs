package api

import (
	"net/http"
)

// console is the landing page: one button that asks for a lab, and the
// environments it can hand out. It is a static document carrying nothing —
// everything it shows is fetched from the API, which is where the limits are —
// so it is safe to serve to anyone.
//
// It is embedded as a string rather than a file so the binary is the whole
// product, and there is no build step and no static directory to keep in step
// with a deployment.
func (s *Server) console(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		// A path that is not a route and not the root is a real 404, not the
		// landing page, so a mistyped API path does not look like a success.
		writeJSON(w, http.StatusNotFound, errorBody{Error: "no such page: " + r.URL.Path})
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(consoleHTML))
}

const consoleHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>labs</title>
<style>
  :root { color-scheme: light dark; --fg: #111; --bg: #fff; --muted: #666; --line: #ddd; --accent: #2b6cb0; --ok: #2f855a; --warnc: #b7791f; --err: #c53030; }
  @media (prefers-color-scheme: dark) { :root { --fg: #e6e6e6; --bg: #111; --muted: #999; --line: #333; --accent: #63b3ed; --ok: #68d391; --warnc: #f6ad55; --err: #fc8181; } }
  body { margin: 0; font: 16px/1.5 ui-sans-serif, system-ui, -apple-system, sans-serif; color: var(--fg); background: var(--bg); }
  main { max-width: 640px; margin: 0 auto; padding: 8vh 20px; }
  h1 { font-size: 1.6rem; margin: 0 0 .25rem; }
  h2 { font-size: .8rem; text-transform: uppercase; letter-spacing: .06em; color: var(--muted); margin: 2rem 0 .5rem; }
  p.lead { color: var(--muted); margin: 0 0 2rem; }
  button { font: inherit; padding: .6rem 1.2rem; border: 1px solid var(--accent); background: var(--accent); color: #fff; border-radius: 6px; cursor: pointer; }
  button:disabled { opacity: .5; cursor: default; }
  button.small { padding: .3rem .7rem; font-size: .85rem; }
  #out { margin-top: 2rem; }
  .row { display: flex; gap: .5rem; align-items: baseline; padding: .5rem 0; border-top: 1px solid var(--line); }
  .row .k { width: 8rem; color: var(--muted); flex: none; }
  code { font: 13px/1.4 ui-monospace, SFMono-Regular, Menlo, monospace; word-break: break-all; }
  .ok { color: var(--ok); }
  .warn { color: var(--warnc); }
  .err { color: var(--err); }
  .banner { margin-top: 1.5rem; padding: .75rem 1rem; border: 1px solid var(--line); border-radius: 8px; background: color-mix(in srgb, var(--warnc) 8%, transparent); }
  .banner h2 { font-size: .95rem; text-transform: none; letter-spacing: 0; color: var(--fg); margin: 0 0 .5rem; }
  .banner ul { margin: 0; padding-left: 1.2rem; }
  .banner li { margin: .2rem 0; }
  .env { padding: .6rem 0; border-top: 1px solid var(--line); }
  .env .top { display: flex; gap: .5rem; align-items: baseline; }
  .env .name { font-weight: 600; }
  .env .meta { color: var(--muted); font-size: .85rem; }
  .env .msg { color: var(--muted); font-size: .85rem; margin-top: .15rem; }
  .dot { display: inline-block; width: .55rem; height: .55rem; border-radius: 50%; margin-right: .4rem; vertical-align: baseline; }
  .dot.ok { background: var(--ok); }
  .dot.warn { background: var(--warnc); }
  .dot.err { background: var(--err); }
  footer { margin-top: 3rem; padding-top: .75rem; border-top: 1px solid var(--line); color: var(--muted); font: 12px/1.5 ui-monospace, SFMono-Regular, Menlo, monospace; }
</style>
</head>
<body>
<main>
  <h1>labs</h1>
  <p class="lead">Get a working environment for a couple of hours.</p>
  <div id="actions"></div>
  <div id="out"></div>
  <h2 id="running-title" hidden>Running labs</h2>
  <div id="running"></div>
  <div id="status"></div>
  <h2 id="envs-title" hidden>Cluster status</h2>
  <div id="envs"></div>
  <footer id="build"></footer>
</main>
<script>
const out = document.getElementById('out');
const status = document.getElementById('status');
const envs = document.getElementById('envs');
const envsTitle = document.getElementById('envs-title');
const actions = document.getElementById('actions');
const running = document.getElementById('running');
const runningTitle = document.getElementById('running-title');
const build = document.getElementById('build');
function row(k, v, cls) {
  const d = document.createElement('div'); d.className = 'row' + (cls ? ' ' + cls : '');
  const kk = document.createElement('div'); kk.className = 'k'; kk.textContent = k;
  const vv = document.createElement('div'); vv.innerHTML = v;
  d.append(kk, vv); return d;
}
// problems renders a list of what is wrong with the deployment, so the message
// that says "not configured" is followed by the actual missing variables.
function problems(list) {
  const box = document.createElement('div'); box.className = 'banner';
  const h = document.createElement('h2'); h.textContent = 'This deployment is not configured yet'; box.append(h);
  const ul = document.createElement('ul');
  (list || []).forEach(p => { const li = document.createElement('li'); li.textContent = p; ul.append(li); });
  box.append(ul);
  const hint = document.createElement('p'); hint.className = 'lead'; hint.style.margin = '.75rem 0 0';
  hint.textContent = 'Set these and redeploy. ';
  const a = document.createElement('a'); a.href = 'api/v1/config'; a.textContent = 'GET /api/v1/config';
  hint.append(a, document.createTextNode(' shows the full state.'));
  box.append(hint);
  return box;
}
// renderEnvs shows each environment and whether it is up. It updates in place
// on every poll, so the page is where you watch an environment come up rather
// than somewhere you reload.
function renderEnvs(list) {
  envs.replaceChildren();
  envsTitle.hidden = !(list && list.length);
  (list || []).forEach(e => {
    const box = document.createElement('div'); box.className = 'env';
    const top = document.createElement('div'); top.className = 'top';
    // The dot is green when a lab can be made, amber while the cluster is
    // coming up, and red when it is up but refuses our key.
    const cls = e.ready ? 'ok' : (e.unauthorized ? 'err' : 'warn');
    const dot = document.createElement('span'); dot.className = 'dot ' + cls;
    const name = document.createElement('span'); name.className = 'name'; name.textContent = e.kind || e.id;
    const st = document.createElement('span'); st.className = 'meta ' + cls;
    st.textContent = e.ready ? 'ready' : (e.unauthorized ? 'key needed' : 'starting');
    top.append(dot, name, document.createTextNode(' '), st, Object.assign(document.createElement('span'), { className: 'meta', textContent: ' · ' + e.id }));
    box.append(top);
    // applab lends out named applications; sandboxlab hands out sandboxes, and a
    // lab there is not an "application" — so the line names what is being
    // counted rather than calling everything an app.
    if (e.capacity) {
      const what = e.kind === 'sandboxlab' ? 'sandboxes' : 'application slots';
      box.append(Object.assign(document.createElement('div'), { className: 'msg', textContent: e.occupied + ' of ' + e.capacity + ' ' + what + ' in use' }));
    }
    if (e.console_url) {
      const u = document.createElement('div'); u.className = 'msg';
      const a = document.createElement('a'); a.href = e.console_url; a.textContent = e.console_url; a.target = '_blank'; a.rel = 'noopener';
      u.append(a); box.append(u);
    }
    if (!e.ready && e.message) box.append(Object.assign(document.createElement('div'), { className: 'msg', textContent: e.message }));
    envs.append(box);
  });
}
// renderActions offers one button per kind the deployment serves, so either
// kind can be asked for. A kind that is not up gets a "Start" button rather
// than none: creating a lab is what dispatches an environment's run, so hiding
// the button while nothing runs would leave no way to ask for one at all — the
// page would show "starting" forever with nothing starting it.
//
// The buttons are keyed by the set of kinds and their state, and only redrawn
// when that changes, so the five-second poll does not replace a button under the
// pointer.
function renderActions(list) {
  const kinds = [];
  (list || []).forEach(e => {
    if (!e.kind) return;
    let k = kinds.find(x => x.kind === e.kind);
    if (!k) { k = { kind: e.kind, ready: false }; kinds.push(k); }
    k.ready = k.ready || !!e.ready; // ready if any environment of the kind is
  });
  const want = kinds.map(k => k.kind + (k.ready ? ':up' : ':down')).join(',');
  if (actions.dataset.kinds === want) return; // nothing changed; leave the buttons
  actions.dataset.kinds = want;
  actions.replaceChildren();
  kinds.forEach(k => {
    const b = document.createElement('button');
    b.textContent = k.ready ? 'Get a ' + k.kind + ' lab' : 'Start ' + k.kind;
    b.style.marginRight = '.5rem';
    b.onclick = () => requestLab(k.kind, b);
    actions.append(b);
  });
}
// setFooter renders the build identity, when the page last heard from the
// service, and which repositories are in play. The updated time sits right
// after the commit, so the footer says both which build this is and how fresh
// what is on screen is — a status page that stopped polling looks exactly like
// a quiet one otherwise.
//
// The repository count is here because serving fewer kinds than expected is a
// silent mistake: nothing errors, there is just less than there should be. When
// LABS_REPOS names the repositories, saying which is what turns "where is
// sandboxlab" into "LABS_REPOS is set to one repository".
function setFooter(b) {
  const parts = [];
  if (b.commit) parts.push('commit ' + b.commit);
  parts.push('updated ' + new Date().toLocaleTimeString());
  if (b.build_time && b.build_time !== 'unknown') parts.push('built ' + new Date(b.build_time).toLocaleString());
  if (b.repos) parts.push('repos ' + b.repos.length + (b.repos_from === 'LABS_REPOS' ? ' (LABS_REPOS)' : ''));
  build.textContent = parts.join(' · ');
}
// countdown is how long a lab has left, as "1h 57m" or "3m 12s". It is the
// product's promise, so it is shown counting down rather than as an end time
// the reader has to subtract from.
function countdown(expiresAt) {
  let s = Math.floor((new Date(expiresAt).getTime() - Date.now()) / 1000);
  if (s <= 0) return 'expired';
  const h = Math.floor(s / 3600), m = Math.floor((s % 3600) / 60);
  s = s % 60;
  if (h) return h + 'h ' + String(m).padStart(2, '0') + 'm';
  if (m) return m + 'm ' + String(s).padStart(2, '0') + 's';
  return s + 's';
}
// sinceFmt is how long ago a lab was created, as "12m ago".
function sinceFmt(createdAt) {
  let s = Math.floor((Date.now() - new Date(createdAt).getTime()) / 1000);
  if (s < 60) return 'just now';
  const m = Math.floor(s / 60), h = Math.floor(m / 60);
  if (h) return h + 'h ' + (m % 60) + 'm ago';
  return m + 'm ago';
}
let sessionTTL = 0; // seconds; from /config, for kinds that report no expiry
// renderRunning lists every lab the environments are running, by kind, with when
// each was created and how long it has left. It is read from the environments,
// so it is right even for a lab this service did not itself hand out — and it
// never carries a key: a key is shown once, at creation, and this list exists so
// it does not have to be shown again.
function renderRunning(lists) {
  running.replaceChildren();
  const any = (lists || []).some(l => (l.labs || []).length);
  runningTitle.hidden = !any;
  (lists || []).forEach(l => {
    (l.labs || []).forEach(it => {
      const box = document.createElement('div'); box.className = 'env';
      const top = document.createElement('div'); top.className = 'top';
      const name = document.createElement('span'); name.className = 'name'; name.textContent = l.kind;
      const st = document.createElement('span'); st.className = 'meta'; st.textContent = it.state || '';
      const left = document.createElement('span'); left.className = 'meta'; left.dataset.expires = expiresOf(it);
      const at = document.createElement('span'); at.className = 'meta'; at.dataset.created = it.created_at || '';
      top.append(name, document.createTextNode(' '), st, Object.assign(document.createElement('span'), { className: 'meta', textContent: ' · ' + it.id }), left, at);
      box.append(top);
      running.append(box);
    });
  });
  tick();
}
// expiresOf is when a running instance ends. A sandboxlab sandbox reports its
// own expiry; an applab app has none, so the lab's clock is the session's —
// created plus the deployment's TTL.
function expiresOf(it) {
  if (it.expires_at) return it.expires_at;
  if (it.created_at && sessionTTL) return new Date(new Date(it.created_at).getTime() + sessionTTL * 1000).toISOString();
  return '';
}
// tick refreshes every countdown on the page from the current time. It runs on
// its own one-second timer and reads the data-expires values rather than
// re-rendering, so the lists do not flicker under the pointer.
function tick() {
  document.querySelectorAll('#running [data-expires]').forEach(e => {
    e.textContent = e.dataset.expires ? ' · expires in ' + countdown(e.dataset.expires) : '';
  });
  document.querySelectorAll('#running [data-created]').forEach(e => {
    e.textContent = e.dataset.created ? ' · created ' + sinceFmt(e.dataset.created) : '';
  });
  document.querySelectorAll('#out [data-expires]').forEach(e => {
    e.textContent = new Date(e.dataset.expires).toLocaleString() + ' · ' + countdown(e.dataset.expires);
  });
}
setInterval(tick, 1000);
let configured = true;
// fetchJSON fetches with a deadline, so a slow or hung endpoint cannot leave
// the page blank — it fails, and the caller shows why.
async function fetchJSON(url, opts) {
  const ctrl = new AbortController();
  const timer = setTimeout(() => ctrl.abort(), 15000);
  try {
    const res = await fetch(url, Object.assign({ signal: ctrl.signal }, opts || {}));
    const body = await res.json();
    return { ok: res.ok, status: res.status, body };
  } finally {
    clearTimeout(timer);
  }
}
async function loadStatus() {
  try {
    // /config carries both the state and the cluster status; it is what the
    // page shows, refreshed on a timer so an environment coming up turns into a
    // button without a reload. Nothing is started here — a lab is created, and
    // that is what brings an environment up.
    const { body } = await fetchJSON('api/v1/config');
    const cfg = body.data || {};
    setFooter(cfg);
    sessionTTL = cfg.session_ttl_seconds || 0;
    renderRunning(cfg.labs);
    renderEnvs(cfg.environments);
    configured = cfg.configured !== false;
    // A button per ready kind, so either kind can be asked for; before one is
    // up there is nothing to offer and the status list says why.
    renderActions(configured ? cfg.environments : []);
    if (!configured && (cfg.problems || []).length) {
      status.replaceChildren(problems(cfg.problems));
    } else {
      status.replaceChildren();
    }
  } catch (e) {
    // Say so rather than showing nothing, which is what a silent failure looks
    // like: the cluster status simply never appears.
    status.replaceChildren(row('status', 'could not reach the service: ' + e.message, 'err'));
    renderActions([]);
  }
}
// requestLab asks for one lab of the named kind — the kind is what the button
// says, so a deployment serving both hands out whichever was pressed, rather
// than always the first.
async function requestLab(kind, btn) {
  btn.disabled = true; out.replaceChildren();
  try {
    const { ok, status: code, body } = await fetchJSON('api/v1/labs', {
      method: 'POST', headers: {'Content-Type':'application/json'}, body: JSON.stringify({ kind: kind }) });
    if (!ok) {
      const d = row('error', document.createTextNode(body.error || ('HTTP ' + code)).textContent, 'err');
      if (body.retryable) d.append(Object.assign(document.createElement('span'), { textContent: ' (retryable)' }));
      out.append(d);
      if ((body.problems || []).length) out.append(problems(body.problems));
      loadStatus(); // show the environment the request just started
      return;
    }
    const d = body.data;
    // The key is shown here and only here. It is the deliverable, shown once at
    // the hand-off; the list below deliberately never carries it.
    out.append(row('console', '<a href="' + d.console_url + '" target="_blank" rel="noopener">' + d.console_url + '</a>'));
    out.append(row('api key', '<code>' + (d.api_key || '(none)') + '</code>'));
    out.append(row('expires', '<span data-expires="' + d.expires_at + '"></span>'));
    if (d.app) out.append(row('app', '<code>' + d.app + '</code>'));
    if (d.warning) out.append(row('note', d.warning, 'warn'));
    out.append(row('how', kind === 'sandboxlab' ? 'Open the link; the key is already in it.' : 'Open the console and paste the key.'));
    loadStatus(); // the new lab appears in the running list on the next poll
  } catch (e) {
    out.append(row('error', String(e), 'err'));
  } finally {
    btn.disabled = false;
  }
}
loadStatus();
// Poll, so an environment that is booting becomes a lab without a reload. The
// interval is short enough to feel live and long enough not to hammer.
setInterval(() => { if (!document.hidden) loadStatus(); }, 5000);
</script>
</body>
</html>
`
