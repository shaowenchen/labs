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
</style>
</head>
<body>
<main>
  <h1>labs</h1>
  <p class="lead">Get a working environment for a couple of hours. No account.</p>
  <button id="go">Get a lab</button>
  <div id="out"></div>
</main>
<script>
const out = document.getElementById('out');
const go = document.getElementById('go');
function row(k, v, cls) {
  const d = document.createElement('div'); d.className = 'row' + (cls ? ' ' + cls : '');
  const kk = document.createElement('div'); kk.className = 'k'; kk.textContent = k;
  const vv = document.createElement('div'); vv.innerHTML = v;
  d.append(kk, vv); return d;
}
go.onclick = async () => {
  go.disabled = true; out.replaceChildren();
  try {
    const res = await fetch('api/v1/labs', { method: 'POST', headers: {'Content-Type':'application/json'}, body: '{}' });
    const body = await res.json();
    if (!res.ok) {
      const d = row('error', document.createTextNode(body.error || res.statusText).textContent, 'err');
      if (body.retryable) d.append(Object.assign(document.createElement('span'), { textContent: ' (retryable)' }));
      out.append(d); return;
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
</script>
</body>
</html>
`
