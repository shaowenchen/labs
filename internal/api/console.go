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
  :root {
    color-scheme: light dark;
    --bg: #f7f6f3;
    --surface: #ffffff;
    --fg: #17171a;
    --muted: #6c6c74;
    --line: #e8e5df;
    --line-soft: #f0ede8;
    --accent: #4f46e5;
    --accent-soft: #eef0fe;
    --accent-fg: #ffffff;
    --ok: #15803d;
    --ok-soft: #e7f5ec;
    --warn: #b45309;
    --warn-soft: #fdf3e3;
    --err: #b91c1c;
    --err-soft: #fdeceb;
    --radius: 14px;
    --radius-sm: 9px;
    --shadow: 0 1px 2px rgba(20,20,30,.04), 0 6px 20px rgba(20,20,30,.05);
    --mono: ui-monospace, SFMono-Regular, "SF Mono", Menlo, Consolas, monospace;
  }
  @media (prefers-color-scheme: dark) {
    :root {
      --bg: #0f0f11;
      --surface: #17171a;
      --fg: #ececef;
      --muted: #9a9aa3;
      --line: #2a2a30;
      --line-soft: #212127;
      --accent: #8b86ff;
      --accent-soft: #1e1c33;
      --accent-fg: #12121a;
      --ok: #6ee7a0;
      --ok-soft: #12241a;
      --warn: #fbbf68;
      --warn-soft: #2a1f10;
      --err: #fca5a5;
      --err-soft: #2c1414;
      --shadow: 0 1px 2px rgba(0,0,0,.3), 0 6px 20px rgba(0,0,0,.25);
    }
  }
  * { box-sizing: border-box; }
  body {
    margin: 0;
    font: 16px/1.55 ui-sans-serif, system-ui, -apple-system, "Segoe UI", sans-serif;
    color: var(--fg);
    background: var(--bg);
    -webkit-font-smoothing: antialiased;
  }
  main { max-width: 720px; margin: 0 auto; padding: 64px 20px 80px; }

  header { margin-bottom: 36px; }
  h1 { font-size: 1.9rem; line-height: 1.1; letter-spacing: -.02em; margin: 0 0 8px; }
  h1 .mark {
    display: inline-block; width: 10px; height: 10px; border-radius: 3px;
    background: var(--accent); margin-right: 12px; vertical-align: middle;
    transform: translateY(-2px);
  }
  p.lead { color: var(--muted); margin: 0; font-size: 1.02rem; }

  h2 {
    font-size: .74rem; text-transform: uppercase; letter-spacing: .09em;
    color: var(--muted); font-weight: 600; margin: 34px 0 12px;
  }

  /* ── buttons ────────────────────────────────────────────────────────── */
  #actions { display: flex; flex-wrap: wrap; gap: 10px; }
  button {
    font: inherit; font-weight: 550;
    padding: 11px 20px;
    border: 1px solid transparent;
    background: var(--accent);
    color: var(--accent-fg);
    border-radius: var(--radius-sm);
    cursor: pointer;
    transition: transform .06s ease, filter .12s ease, background .12s ease;
  }
  button:hover:not(:disabled) { filter: brightness(1.08); }
  button:active:not(:disabled) { transform: translateY(1px); }
  button:disabled { opacity: .45; cursor: default; }
  button:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; }
  button.small {
    padding: 4px 11px; font-size: .8rem; font-weight: 500;
    background: transparent; color: var(--muted);
    border-color: var(--line);
  }
  button.small:hover:not(:disabled) { background: var(--line-soft); filter: none; }

  /* ── cards ──────────────────────────────────────────────────────────── */
  #out:not(:empty), #running:not(:empty), #envs:not(:empty) {
    background: var(--surface);
    border: 1px solid var(--line);
    border-radius: var(--radius);
    box-shadow: var(--shadow);
    overflow: hidden;
  }
  #out:not(:empty) { padding: 18px 20px; }

  .row {
    display: grid; grid-template-columns: 6.5rem 1fr; gap: 14px;
    align-items: baseline; padding: 11px 0; border-top: 1px solid var(--line-soft);
  }
  .row:first-child { border-top: 0; padding-top: 0; }
  .row:last-child { padding-bottom: 0; }
  .row .k { color: var(--muted); font-size: .86rem; }
  .row.err { color: var(--err); }

  .env { padding: 16px 20px; border-top: 1px solid var(--line-soft); }
  .env:first-child { border-top: 0; }
  .env .top { display: flex; align-items: center; gap: 9px; flex-wrap: wrap; }
  .env .name { font-weight: 600; }
  .env .meta { color: var(--muted); font-size: .85rem; }
  .env .msg {
    color: var(--muted); font-size: .86rem; margin-top: 7px;
    overflow-wrap: anywhere;
  }
  .env .msg a { color: var(--muted); }

  .dot { width: 8px; height: 8px; border-radius: 50%; flex: none; }
  .dot.ok { background: var(--ok); box-shadow: 0 0 0 3px var(--ok-soft); }
  .dot.warn { background: var(--warn); box-shadow: 0 0 0 3px var(--warn-soft); }
  .dot.err { background: var(--err); box-shadow: 0 0 0 3px var(--err-soft); }

  .ok { color: var(--ok); }
  .warn { color: var(--warn); }
  .err { color: var(--err); }

  /* A single kind of small label, used for states. */
  .tail {
    display: inline-flex; align-items: center; gap: 6px;
    font-size: .8rem; color: var(--muted);
    background: var(--line-soft); border-radius: 999px; padding: 2px 10px;
  }

  code {
    font: .84rem/1.45 var(--mono);
    background: var(--line-soft);
    padding: 2px 7px; border-radius: 6px;
    overflow-wrap: anywhere;
  }
  a { color: var(--accent); text-decoration: none; }
  a:hover { text-decoration: underline; }

  /* ── the hand-off, shown once, as a dialog ──────────────────────────── */
  dialog.key-modal {
    border: 1px solid var(--line);
    border-radius: var(--radius);
    background: var(--surface);
    color: var(--fg);
    padding: 0;
    width: min(560px, calc(100vw - 32px));
    box-shadow: 0 24px 60px rgba(10,10,20,.28);
  }
  dialog.key-modal::backdrop { background: rgba(12,12,18,.5); backdrop-filter: blur(2px); }
  .key-modal .head {
    padding: 22px 24px 18px;
    border-bottom: 1px solid var(--line-soft);
  }
  .key-modal .head .eyebrow {
    font-size: .72rem; font-weight: 600; letter-spacing: .09em;
    text-transform: uppercase; color: var(--ok);
  }
  .key-modal .head h3 { margin: 6px 0 0; font-size: 1.2rem; letter-spacing: -.01em; }
  .key-modal .body { padding: 8px 24px 20px; }
  .key-modal .row { grid-template-columns: 5.5rem 1fr; }
  .key-modal .warnbox {
    margin: 14px 24px 0; padding: 11px 13px; font-size: .85rem;
    border: 1px solid var(--warn); border-radius: var(--radius-sm);
    background: var(--warn-soft); color: var(--warn);
  }
  .key-modal .once {
    margin: 14px 24px 0; padding: 11px 13px; font-size: .85rem;
    border: 1px solid var(--line); border-radius: var(--radius-sm);
    background: var(--line-soft); color: var(--muted);
  }
  .key-modal .foot {
    display: flex; gap: 10px; justify-content: flex-end;
    padding: 16px 24px 20px;
  }
  .key-modal .keyline { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
  .key-modal .keyline code { font-size: .9rem; padding: 5px 10px; }
  body.modal-open { overflow: hidden; }

  .banner {
    padding: 16px 18px; border: 1px solid var(--warn);
    border-radius: var(--radius); background: var(--warn-soft);
    margin-top: 18px;
  }
  .banner h2 {
    font-size: .95rem; text-transform: none; letter-spacing: 0;
    color: var(--fg); margin: 0 0 8px;
  }
  .banner ul { margin: 0; padding-left: 1.15rem; }
  .banner li { margin: 3px 0; font-size: .9rem; }

  footer {
    margin-top: 48px; padding-top: 16px; border-top: 1px solid var(--line);
    color: var(--muted); font: .74rem/1.6 var(--mono);
  }
  @media (max-width: 480px) {
    main { padding: 40px 16px 64px; }
    h1 { font-size: 1.6rem; }
    .row { grid-template-columns: 1fr; gap: 2px; }
    .row .k { font-size: .78rem; }
  }
</style>
</head>
<body>
<main>
  <header>
    <h1><span class="mark"></span>labs</h1>
    <p class="lead">Get a working environment for a couple of hours.</p>
  </header>
  <div id="actions"></div>
  <div id="out"></div>
  <h2 id="running-title" hidden>Running labs</h2>
  <div id="running"></div>
  <div id="status"></div>
  <h2 id="envs-title" hidden>Cluster status</h2>
  <div id="envs"></div>
  <footer id="build"></footer>
</main>
<dialog id="key-modal" class="key-modal">
  <div class="head">
    <div class="eyebrow">Ready</div>
    <h3 id="km-title">Your lab is ready</h3>
  </div>
  <div class="body" id="km-body"></div>
  <div class="foot">
    <button id="km-close" class="small">Close</button>
    <button id="km-open">Open the lab</button>
  </div>
</dialog>
<script>
const out = document.getElementById('out');
const status = document.getElementById('status');
const envs = document.getElementById('envs');
const envsTitle = document.getElementById('envs-title');
const actions = document.getElementById('actions');
const running = document.getElementById('running');
const runningTitle = document.getElementById('running-title');
const build = document.getElementById('build');
const km = document.getElementById('key-modal');
const kmTitle = document.getElementById('km-title');
const kmBody = document.getElementById('km-body');
const kmOpen = document.getElementById('km-open');
const kmClose = document.getElementById('km-close');
function row(k, v, cls) {
  const d = document.createElement('div'); d.className = 'row' + (cls ? ' ' + cls : '');
  const kk = document.createElement('div'); kk.className = 'k'; kk.textContent = k;
  const vv = document.createElement('div'); vv.className = 'v'; vv.innerHTML = v;
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
    const st = document.createElement('span'); st.className = 'tail'; st.textContent = e.ready ? 'ready' : (e.unauthorized ? 'key needed' : 'starting');
    top.append(dot, name, st, Object.assign(document.createElement('span'), { className: 'meta', textContent: e.id }));
    box.append(top);
    // One line for every kind, so the two are read the same way. applab lends
    // out named application slots, and a lab holds one for as long as it lasts;
    // sandboxlab has no slots — a sandbox is created on demand — so its number
    // is what is running rather than what is left.
    if (e.capacity) {
      const what = e.kind === 'sandboxlab'
        ? ('sandboxes: ' + e.occupied + ' of ' + e.capacity + ' in use')
        : ('application slots: ' + e.occupied + ' of ' + e.capacity + ' in use');
      box.append(Object.assign(document.createElement('div'), { className: 'msg', textContent: what }));
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
// service, and which kinds are in play. The updated time sits right after the
// commit, so the footer says both which build this is and how fresh what is on
// screen is — a status page that stopped polling looks exactly like a quiet one
// otherwise.
//
// The kind count is here because serving fewer kinds than expected is a silent
// mistake: nothing errors, there is just less than there should be. Saying how
// many kinds are configured turns "where is sandboxlab" into an answer.
function setFooter(b) {
  const parts = [];
  if (b.commit) parts.push('commit ' + b.commit);
  parts.push('updated ' + new Date().toLocaleTimeString());
  if (b.build_time && b.build_time !== 'unknown') parts.push('built ' + new Date(b.build_time).toLocaleString());
  if (b.kinds) parts.push('kinds ' + b.kinds.length);
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
      const st = document.createElement('span'); st.className = 'tail'; st.textContent = it.state || 'running';
      const left = document.createElement('span'); left.className = 'meta'; left.dataset.expires = expiresOf(it);
      const at = document.createElement('span'); at.className = 'meta'; at.dataset.created = it.created_at || '';
      top.append(name, st, Object.assign(document.createElement('span'), { className: 'meta', textContent: it.id }), left, at);
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
    e.textContent = e.dataset.expires ? 'expires in ' + countdown(e.dataset.expires) : '';
  });
  document.querySelectorAll('#running [data-created]').forEach(e => {
    e.textContent = e.dataset.created ? 'created ' + sinceFmt(e.dataset.created) : '';
  });
  document.querySelectorAll('#out [data-expires]').forEach(e => {
    e.textContent = countdown(e.dataset.expires) + ' left · until ' + new Date(e.dataset.expires).toLocaleTimeString();
  });
  // The dialog's own countdown, so the hand-off keeps ticking while it is open.
  document.querySelectorAll('#km-body [data-expires]').forEach(e => {
    e.textContent = e.dataset.expires
      ? countdown(e.dataset.expires) + ' left · until ' + new Date(e.dataset.expires).toLocaleTimeString()
      : '';
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
    showLab(body.data, kind);
    loadStatus(); // the new lab appears in the running list on the next poll
  } catch (e) {
    out.append(row('error', String(e), 'err'));
  } finally {
    btn.disabled = false;
  }
}
// showLab is the hand-off: the one moment a key is shown, in a dialog that says
// so. The key is not kept anywhere on the page afterwards — the running list
// below never carries it — so the dialog is explicit that closing it is the last
// chance to take it, and offers a copy button rather than a string to select by
// hand.
function showLab(d, kind) {
  kmTitle.textContent = 'Your ' + kind + ' lab is ready';
  kmBody.replaceChildren();

  kmBody.append(row('console', '<a href="' + d.console_url + '" target="_blank" rel="noopener">' + d.console_url + '</a>'));
  kmBody.append(keyRow(d.api_key));
  kmBody.append(row('expires', '<span data-expires="' + d.expires_at + '"></span>'));
  if (d.app) kmBody.append(row('app', '<code>' + d.app + '</code>'));
  kmBody.append(row('how', kind === 'sandboxlab'
    ? 'Open the link — the key is already in it.'
    : 'Open the console and paste the key.'));

  if (d.warning) {
    const w = document.createElement('div'); w.className = 'warnbox'; w.textContent = d.warning;
    kmBody.append(w);
  }
  const once = document.createElement('div');
  once.className = 'once';
  once.textContent = 'This key is shown once, now. It is not shown again — copy it before closing.';
  kmBody.append(once);

  kmOpen.onclick = () => window.open(d.console_url, '_blank', 'noopener');
  document.body.classList.add('modal-open');
  if (typeof km.showModal === 'function') km.showModal(); else km.setAttribute('open', '');
  kmClose.onclick = () => {
    if (typeof km.close === 'function') km.close(); else km.removeAttribute('open');
    document.body.classList.remove('modal-open');
  };
  km.onclose = () => document.body.classList.remove('modal-open');
  tick();
}
// keyRow renders the one-time key with a copy button. It is the only place a
// key ever appears, and it is not stored anywhere on the page afterwards.
function keyRow(key) {
  const r = row('api key', '', 'key');
  const v = r.querySelector('.v');
  v.className = 'v keyline';
  const code = document.createElement('code'); code.textContent = key || '(none)';
  const copy = document.createElement('button'); copy.className = 'small'; copy.textContent = 'Copy';
  copy.onclick = async () => {
    try { await navigator.clipboard.writeText(key); copy.textContent = 'Copied'; }
    catch (e) { copy.textContent = 'Select it'; }
    setTimeout(() => { copy.textContent = 'Copy'; }, 1500);
  };
  v.append(code, copy);
  return r;
}
loadStatus();
// Poll, so an environment that is booting becomes a lab without a reload. The
// interval is short enough to feel live and long enough not to hammer.
setInterval(() => { if (!document.hidden) loadStatus(); }, 5000);
</script>
</body>
</html>
`
