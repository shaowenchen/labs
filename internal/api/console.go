package api

import (
	"net/http"
)

// console is the landing page: one button that asks for a lab and shows what
// came back. It is a static document carrying nothing — everything it shows is
// fetched from POST /api/v1/labs, which is where the limits are — so it is safe
// to serve to anyone.
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
  :root { color-scheme: light dark; --fg: #111; --bg: #fff; --muted: #666; --line: #ddd; --accent: #2b6cb0; }
  @media (prefers-color-scheme: dark) { :root { --fg: #e6e6e6; --bg: #111; --muted: #999; --line: #333; --accent: #63b3ed; } }
  body { margin: 0; font: 16px/1.5 ui-sans-serif, system-ui, -apple-system, sans-serif; color: var(--fg); background: var(--bg); }
  main { max-width: 640px; margin: 0 auto; padding: 8vh 20px; }
  h1 { font-size: 1.6rem; margin: 0 0 .25rem; }
  p.lead { color: var(--muted); margin: 0 0 2rem; }
  button { font: inherit; padding: .6rem 1.2rem; border: 1px solid var(--accent); background: var(--accent); color: #fff; border-radius: 6px; cursor: pointer; }
  button:disabled { opacity: .5; cursor: default; }
  #out { margin-top: 2rem; }
  .row { display: flex; gap: .5rem; align-items: baseline; padding: .5rem 0; border-top: 1px solid var(--line); }
  .row .k { width: 8rem; color: var(--muted); flex: none; }
  code { font: 13px/1.4 ui-monospace, SFMono-Regular, Menlo, monospace; word-break: break-all; }
  .warn { color: #b7791f; }
  .err { color: #c53030; }
  .banner { margin-top: 1.5rem; padding: .75rem 1rem; border: 1px solid var(--line); border-radius: 8px; background: color-mix(in srgb, var(--warn) 8%, transparent); }
  .banner h2 { font-size: .95rem; margin: 0 0 .5rem; }
  .banner ul { margin: 0; padding-left: 1.2rem; }
  .banner li { margin: .2rem 0; }
  footer { margin-top: 3rem; padding-top: .75rem; border-top: 1px solid var(--line); color: var(--muted); font: 12px/1.5 ui-monospace, SFMono-Regular, Menlo, monospace; }
</style>
</head>
<body>
<main>
  <h1>labs</h1>
  <p class="lead">Get a working environment for a couple of hours. No account.</p>
  <button id="go">Get a lab</button>
  <div id="out"></div>
  <div id="status"></div>
  <footer id="build"></footer>
</main>
<script>
const out = document.getElementById('out');
const status = document.getElementById('status');
const go = document.getElementById('go');
const build = document.getElementById('build');
function row(k, v, cls) {
  const d = document.createElement('div'); d.className = 'row' + (cls ? ' ' + cls : '');
  const kk = document.createElement('div'); kk.className = 'k'; kk.textContent = k;
  const vv = document.createElement('div'); vv.innerHTML = v;
  d.append(kk, vv); return d;
}
// problems renders a list of what is wrong with the deployment. It is shown on
// load and on a failed request, so the message that says "not configured" is
// followed by the actual missing variables rather than pointing elsewhere.
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
// The build footer is filled from much earlier, when the page's own template is
// rendered, so the commit and build time are in the served document rather than
// fetched after it.
function setFooter(b) {
  const parts = [];
  if (b.commit) parts.push('commit ' + b.commit);
  if (b.build_time && b.build_time !== 'unknown') parts.push(new Date(b.build_time).toLocaleString());
  build.textContent = parts.join(' · ');
}
async function loadStatus() {
  try {
    const res = await fetch('api/v1/config');
    const body = await res.json();
    const cfg = body.data || {};
    setFooter(cfg);
    if (!cfg.configured && (cfg.problems || []).length) {
      status.replaceChildren(problems(cfg.problems));
      go.disabled = true;
    }
  } catch (e) { /* the button still works and will report a real error */ }
}
go.onclick = async () => {
  go.disabled = true; out.replaceChildren();
  try {
    const res = await fetch('api/v1/labs', { method: 'POST', headers: {'Content-Type':'application/json'}, body: '{}' });
    const body = await res.json();
    if (!res.ok) {
      const d = row('error', document.createTextNode(body.error || res.statusText).textContent, 'err');
      if (body.retryable) d.append(Object.assign(document.createElement('span'), { textContent: ' (retryable)' }));
      out.append(d);
      if ((body.problems || []).length) out.append(problems(body.problems));
      return;
    }
    const d = body.data;
    out.append(row('console', '<a href="' + d.console_url + '" target="_blank" rel="noopener">' + d.console_url + '</a>'));
    out.append(row('api key', '<code>' + (d.api_key || '(none)') + '</code>'));
    out.append(row('expires', new Date(d.expires_at).toLocaleString()));
    if (d.app) out.append(row('app', '<code>' + d.app + '</code>'));
    if (d.warning) out.append(row('note', d.warning, 'warn'));
    out.append(row('how', 'Open the console and paste the key.'));
  } catch (e) {
    out.append(row('error', String(e), 'err'));
  } finally {
    go.disabled = false;
  }
};
loadStatus();
</script>
</body>
</html>
`
