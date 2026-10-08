package api

import (
	"net/http"
)

// console is the landing page: one panel, with a tab per kind of lab to ask for
// and the count and list of what the chosen kind is running under it. It is a
// static document carrying nothing — everything it shows is fetched from the
// API, which is where the limits are — so it is safe to serve to anyone.
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

  header { position: relative; margin-bottom: 36px; }
  /* The language switch sits in the corner of the header, out of the reading
     order: the page is about one thing, and which language it is written in is
     a setting rather than a step. It carries no label of its own — each option
     is written in the language it selects, which is the only label that is
     readable to the person who needs it. */
  .lang { position: absolute; top: 4px; right: 0; display: inline-flex; }
  .lang button {
    font: inherit; font-size: .78rem; font-weight: 500; line-height: 1;
    padding: 5px 9px; border: 0; border-radius: 6px;
    background: transparent; color: var(--muted); cursor: pointer;
  }
  .lang button:hover { color: var(--fg); background: var(--line-soft); }
  .lang button:focus-visible { outline: 2px solid var(--accent); outline-offset: 1px; }
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
  /* The one action a panel offers. It is a real button rather than a link so it
     keeps the pressed and disabled states that a request needs — a create that
     is in flight says so on the button itself. */
  button.small {
    padding: 4px 11px; font-size: .8rem; font-weight: 500;
    background: transparent; color: var(--muted);
    border-color: var(--line);
  }
  button.small:hover:not(:disabled) { background: var(--line-soft); filter: none; }

  /* ── cards ──────────────────────────────────────────────────────────── */
  #status:not(:empty) {
    background: var(--surface);
    border: 1px solid var(--line);
    border-radius: var(--radius);
    box-shadow: var(--shadow);
    overflow: hidden;
  }
  #status:not(:empty) { padding: 18px 20px; }
  /* A request's error sits in the panel above the controls that made it: it is
     about this kind, and it belongs under the thing it is about. */

  .row {
    display: grid; grid-template-columns: 6.5rem 1fr; gap: 14px;
    align-items: baseline; padding: 11px 0; border-top: 1px solid var(--line-soft);
  }
  .row:first-child { border-top: 0; padding-top: 0; }
  .row:last-child { padding-bottom: 0; }
  .row .k { color: var(--muted); font-size: .86rem; }
  .row.err { color: var(--err); }

  /* ── the panel ──────────────────────────────────────────────────────── */
  /* One panel, not one per kind. The kind is a switch inside it, so changing
     what you are asking for does not change where you are looking — the page
     stays put and one word in it changes. The card is drawn only once a kind
     exists, because a card with nothing in it is a bare outline. */
  #panel { display: none; }
  #panel.has {
    display: block;
    margin-top: 22px;
    padding: 16px 20px 18px;
    background: var(--surface);
    border: 1px solid var(--line);
    border-radius: var(--radius);
    box-shadow: var(--shadow);
  }
  /* ── the kind tabs ──────────────────────────────────────────────────── */
  /* One tab per kind the deployment serves. A tab strip rather than a
     dropdown because there are only ever a handful of kinds and each one has a
     state worth showing: a select hides every kind but the chosen one behind a
     click, and the kind you are not reading is exactly the one whose state you
     want. The active tab is the same surface as the panel below it and shares
     its top edge, so the two read as one card rather than two. */
  .tabs {
    display: flex; align-items: flex-end; gap: 2px; flex-wrap: wrap;
    margin: -16px -20px 16px; padding: 12px 20px 0;
    border-bottom: 1px solid var(--line);
  }
  .tabs[hidden] { display: none; }
  .tabs button {
    font: inherit; font-size: .87rem; font-weight: 550; line-height: 1.3;
    display: inline-flex; align-items: center; gap: 8px;
    padding: 8px 14px; border: 1px solid transparent; border-bottom: 0;
    border-radius: var(--radius-sm) var(--radius-sm) 0 0;
    background: transparent; color: var(--muted); cursor: pointer;
    white-space: nowrap; margin-bottom: -1px;
  }
  .tabs button:hover { color: var(--fg); }
  /* The active tab is drawn as the panel continuing upward: same background,
     same border, its own bottom edge removed into the panel's top border. That
     alone was too quiet — on a strip of two the difference between the card's
     white and the page's off-white is a shade, and the tab you are reading has
     to be findable at a glance. So the one that is on also carries the accent:
     a bar along its top edge and its own label, which is a difference in kind
     rather than in shade. The bar is an inset shadow, not a border, so nothing
     shifts by the pixel a second border would add. */
  .tabs button[aria-selected="true"] {
    background: var(--surface); color: var(--accent);
    border-color: var(--line); border-bottom: 1px solid var(--surface);
    box-shadow: inset 0 2px 0 var(--accent);
  }
  .tabs button[aria-selected="true"]:hover { color: var(--accent); }
  .tabs button:focus-visible { outline: 2px solid var(--accent); outline-offset: -2px; }
  .tabs .tick { width: 7px; height: 7px; border-radius: 50%; flex: none; background: var(--muted); }
  .tabs .tick.ok { background: var(--ok); }
  .tabs .tick.warn { background: var(--warn); }
  .tabs .tick.err { background: var(--err); }

  /* What the kind is for. Prose, so it is set as prose and given the width to
     be read in one pass rather than a column of broken lines. */
  .blurb {
    margin-top: 4px; max-width: 46rem;
    font-size: .92rem; line-height: 1.5; color: var(--muted);
  }
  .blurb:empty { display: none; }

  /* ── the kind's two states ──────────────────────────────────────────── */
  /* The cluster and the application answer different questions and are read at
     different times: is anything serving this kind at all, and is what it hands
     out usable. They are two dots in a fixed order, above what the kind is
     running, so "the environment is up but its key was refused" cannot be read
     as one state or the other. A dot is a dot rather than a pill: the pill drew
     a box around a word, which on a page this quiet read as a button.

     They share one line rather than stacking. Stacked, each pair was a grid row
     owning a fixed label column, and two short labels left most of that column
     empty — the value sat far from the word it belonged to, and the two rows
     read as a table with a missing column. Inline, the dot delimits the pair,
     so nothing has to align for the reading to be right, and the capacity
     follows the states on the same line instead of sitting under them as a
     third, unrelated-looking thought. */
  .states {
    margin-top: 14px;
    display: flex; flex-wrap: wrap; align-items: center;
    gap: 6px 22px;
    font-size: .87rem; color: var(--muted);
  }
  .state { display: inline-flex; align-items: center; gap: 7px; }
  .state::before {
    content: ''; width: 7px; height: 7px; border-radius: 50%;
    background: currentColor; flex: none;
  }
  .state .what { color: var(--muted); }
  .state.ok { color: var(--ok); }
  .state.warn { color: var(--warn); }
  .state.err { color: var(--err); }
  /* How much the kind holds is a count, not a third state, so it carries no dot
     and no status colour — it is said in the same muted voice as the labels and
     told apart by shape alone. */
  .states .usage { color: var(--muted); }

  /* The template is a choice, and the row it sits on is where the choice is
     made: its label and the picker share a baseline so the picker starts in the
     same column whatever language its label is in — the labels are not the same
     width, and a picker that starts wherever the word ends reads as a mistake. */
  .choice {
    display: grid; grid-template-columns: 7rem 1fr; align-items: center;
    gap: 12px; margin-top: 12px;
  }
  .choice[hidden] { display: none; }
  .choice .label { font-size: .87rem; color: var(--muted); }
  .choice select {
    font: inherit; font-size: .9rem; line-height: 1.35; padding: 8px 12px;
    max-width: 22rem; width: 100%;
    color: var(--fg); background: var(--surface);
    border: 1px solid var(--line); border-radius: var(--radius-sm);
    cursor: pointer;
  }
  .choice select:hover { border-color: var(--muted); }
  .choice select:focus-visible { outline: 2px solid var(--accent); outline-offset: 1px; }

  /* ── what the kind is running ───────────────────────────────────────── */
  /* A table, because these are records with the same fields in each. The
     headings say what a column is once; the rows only carry the values. The id
     and the state are set in the fixed-width face the rest of the page uses for
     things that are data rather than prose — and because both are passed through
     from the environment unchanged, including when the page is in Chinese.
     It follows the states, which are what the rows are underneath: the kind's
     own state first, then the instances it is running. */
  .labs { margin-top: 16px; }
  .labs-table { width: 100%; border-collapse: collapse; font-size: .86rem; }
  .labs-table th {
    text-align: left; font-weight: 600; font-size: .74rem;
    letter-spacing: .05em; text-transform: uppercase; color: var(--muted);
    padding: 0 12px 7px 0; border-bottom: 1px solid var(--line);
    white-space: nowrap;
  }
  .labs-table td {
    padding: 9px 12px 9px 0; border-bottom: 1px solid var(--line-soft);
    vertical-align: baseline;
  }
  .labs-table tr:last-child td { border-bottom: 0; }
  .labs-table th:last-child, .labs-table td:last-child { padding-right: 0; }
  .labs-table .id { font: 600 .86rem/1.5 var(--mono); overflow-wrap: anywhere; }
  /* The instance's own state, undecorated: it is the environment's word, not a
     second opinion on whether the lab is any good, and the accent here would
     have been the third thing on the line competing for the eye. */
  .labs-table .st { font: .82rem/1.5 var(--mono); color: var(--muted); }
  .labs-table .meta { color: var(--muted); white-space: nowrap; }
  .empty { color: var(--muted); font-size: .87rem; margin: 4px 0 0; }
  /* The action closes the tab's panel: what the kind is, what it is running,
     and then the one thing you can do about it. The rule above it separates the
     action from what it acts on, and is drawn even when the list above is empty
     — the separation is between the two, not between rows. */
  .actions {
    display: flex; justify-content: flex-end;
    margin-top: 18px; padding-top: 14px; border-top: 1px solid var(--line-soft);
  }
  .actions button { padding: 9px 18px; border-radius: var(--radius-sm); white-space: nowrap; }
  /* Why an environment is not up, in the environment's own words. It is the one
     thing the state word cannot say, so it is the only prose in the panel. */
  .reason {
    margin-top: 13px; font-size: .86rem; color: var(--muted);
    overflow-wrap: anywhere;
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
  .key-modal .body:empty { display: none; }
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
  /* The hand-off is a dialog and nothing else: the ask is on the page now, so
     the dialog opens only when there is something to hand over, and closes
     when it has been. */
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
  /* The table is four columns of mostly fixed-width text, which at a phone's
     width does not fit without being allowed to break. The times are the two
     that can wrap and still be read; the id already breaks anywhere. */
  @media (max-width: 560px) {
    .labs-table { font-size: .79rem; }
    .labs-table th, .labs-table td { padding-right: 8px; }
    .labs-table .meta { white-space: normal; }
  }
</style>
</head>
<body>
<main>
  <header>
    <div class="lang" id="lang" role="group" aria-label="language"></div>
    <h1><span class="mark"></span>labs</h1>
    <p class="lead" id="lead">Get a working environment for a couple of hours.</p>
  </header>
  <div id="status"></div>
  <div id="panel">
    <div class="tabs" id="kinds" role="tablist" aria-label="kind of lab" hidden></div>
    <div class="blurb" id="blurb"></div>
    <div id="error"></div>
    <div class="choice" id="choice" hidden>
      <label class="label" id="tmpl-label" for="template">Template</label>
      <select id="template"></select>
    </div>
    <div id="usage"></div>
    <div class="reason" id="reason"></div>
    <div class="labs" id="labs"></div>
    <div class="actions">
      <button id="create">Create a lab</button>
    </div>
  </div>
  <footer id="build"></footer>
</main>
<dialog id="key-modal" class="key-modal">
  <div class="head">
    <div class="eyebrow" id="km-eyebrow">Ready</div>
    <h3 id="km-title">Your lab is ready</h3>
  </div>
  <div class="body" id="km-body"></div>
  <div id="km-error"></div>
  <div class="foot">
    <button id="km-close" class="small">Close</button>
    <button id="km-confirm">Open the lab</button>
  </div>
</dialog>
<script>
const status = document.getElementById('status');
const panel = document.getElementById('panel');
const build = document.getElementById('build');
const blurbBox = document.getElementById('blurb');
const usageBox = document.getElementById('usage');
const tmplLabel = document.getElementById('tmpl-label');
const choiceBox = document.getElementById('choice');
const tabsBox = document.getElementById('kinds');
const tmplSel = document.getElementById('template');
const createBtn = document.getElementById('create');
const errBox = document.getElementById('error');
const reasonBox = document.getElementById('reason');
const labsBox = document.getElementById('labs');
const km = document.getElementById('key-modal');
const kmTitle = document.getElementById('km-title');
const kmBody = document.getElementById('km-body');
const kmConfirm = document.getElementById('km-confirm');
const kmClose = document.getElementById('km-close');
const kmEyebrow = document.getElementById('km-eyebrow');
const kmError = document.getElementById('km-error');
const leadEl = document.getElementById('lead');
const langBox = document.getElementById('lang');

// ── language ────────────────────────────────────────────────────────────────
// The page is read by people who arrive from a link with no context and no
// account, so it is written in the two languages its audience is. There is no
// build step and no translation file to load: both languages live here as one
// table, and every string the page draws goes through t().
//
// What is translated is the page's own prose. What an environment calls its own
// state ("running", "created") and an environment's own error message are passed
// through as they came — they are the environment's words, and a translation
// here would be this page's guess at what they mean. The clocks are formatted in
// the chosen language's conventions rather than left to the browser's, so the
// page does not read half in one language and half in another.
const LANGS = [
  { code: 'en', label: 'English', locale: 'en-US' },
  { code: 'zh', label: '中文', locale: 'zh-CN' },
];
const STRINGS = {
  en: {
    'lead': 'Get a working environment for a couple of hours.',
    'blurb.applab': 'Upload an application and get back the URL it is running at.',
    'blurb.sandboxlab': 'Create a sandbox from a template — a shell, a filesystem, a browser — and throw it away when its time is up.',
    'kind.aria': 'kind of lab',
    'state.ready': 'ready',
    'state.key': 'key needed',
    'state.starting': 'starting',
    'state.gone': 'not answering',
    'what.cluster': 'cluster',
    'what.app': 'app',
    'tmpl.label': 'Template',
    'usage.sandboxes': 'sandboxes: {n} of {cap} in use',
    'usage.slots': 'application slots: {n} of {cap} in use',
    'env.start': 'Create a lab',
    'creating': 'Creating…',
    'col.id': 'id', 'col.state': 'state', 'col.created': 'created', 'col.expires': 'expires',
    'empty': 'Nothing running yet.',
    'lang.aria': 'language',
    'expires.in': 'expires in {d}',
    'created.at': 'created {d}',
    'ago.just': 'just now',
    'ago.hm': '{h}h {m}m ago',
    'ago.m': '{m}m ago',
    'left': '{d} left · until {at}',
    'dur.expired': 'expired',
    'dur.hm': '{h}h {m}m',
    'dur.ms': '{m}m {s}s',
    'dur.s': '{s}s',
    'footer.commit': 'commit {c}',
    'footer.built': 'built {d}',
    'err.retryable': '(retryable)',
    'err.reach': 'could not reach the service: {m}',
    'row.error': 'error',
    'row.status': 'status',
    'row.console': 'console',
    'row.key': 'api key',
    'row.expires': 'expires',
    'row.app': 'app',
    'row.template': 'template',
    'row.how': 'how',
    'how.sandbox': 'Open the link — the key is already in it.',
    'how.other': 'Open the console and paste the key.',
    'once': 'This key is shown once, now. It is not shown again — copy it before closing.',
    'ready': 'Ready',
    'modaltitle': 'Your {kind} lab is ready',
    'close': 'Close',
    'open': 'Open the lab',
    'copy': 'Copy',
    'copied': 'Copied',
    'select': 'Select it',
    'none': '(none)',
    'unconfigured': 'This deployment is not configured yet',
    'redeploy': 'Set these and redeploy. ',
    'fullstate': ' shows the full state.',
    'tabtitle.ready': '{kind}: ready',
    'tabtitle.key': '{kind}: key needed',
    'tabtitle.starting': '{kind}: starting',
  },
  zh: {
    'lead': '获取一个可用的环境，有效期两小时。',
    'blurb.applab': '上传应用源码，拿回它正在运行的地址。',
    'blurb.sandboxlab': '从模板创建一个沙箱 —— shell、文件系统、浏览器 —— 到期即销毁。',
    'kind.aria': '实验室类型',
    'state.ready': '就绪',
    'state.key': '需要密钥',
    'state.starting': '启动中',
    'state.gone': '无法访问',
    'what.cluster': '集群',
    'what.app': '应用',
    'tmpl.label': '模板',
    'usage.sandboxes': '沙箱：{cap} 个中占用 {n} 个',
    'usage.slots': '应用槽位：{cap} 个中占用 {n} 个',
    'env.start': '创建实验',
    'creating': '创建中…',
    'col.id': 'ID', 'col.state': '状态', 'col.created': '创建于', 'col.expires': '到期',
    'empty': '暂无运行中的实例。',
    'lang.aria': '语言',
    'expires.in': '{d}后到期',
    'created.at': '{d}创建',
    'ago.just': '刚刚',
    'ago.hm': '{h}小时{m}分钟前',
    'ago.m': '{m}分钟前',
    'left': '还剩 {d} · 到期时间 {at}',
    'dur.expired': '已过期',
    'dur.hm': '{h}小时{m}分',
    'dur.ms': '{m}分{s}秒',
    'dur.s': '{s}秒',
    'footer.commit': '提交 {c}',
    'footer.built': '构建于 {d}',
    'err.retryable': '（可重试）',
    'err.reach': '无法连接到服务：{m}',
    'row.error': '错误',
    'row.status': '状态',
    'row.console': '控制台',
    'row.key': 'API 密钥',
    'row.expires': '到期',
    'row.app': '应用',
    'row.template': '模板',
    'row.how': '使用方式',
    'how.sandbox': '打开链接即可 —— 密钥已包含在链接里。',
    'how.other': '打开控制台并粘贴密钥。',
    'once': '密钥只在此处显示一次，关闭后不再显示 —— 请先复制。',
    'ready': '已就绪',
    'modaltitle': '你的 {kind} 实验室已就绪',
    'close': '关闭',
    'open': '打开实验室',
    'copy': '复制',
    'copied': '已复制',
    'select': '手动选择',
    'none': '（无）',
    'unconfigured': '此部署尚未配置完成',
    'redeploy': '设置以下变量后重新部署。',
    'fullstate': ' 可查看完整状态。',
    'tabtitle.ready': '{kind}：就绪',
    'tabtitle.key': '{kind}：需要密钥',
    'tabtitle.starting': '{kind}：启动中',
  },
};
function bestLocale() {
  const list = navigator.languages && navigator.languages.length ? navigator.languages : [navigator.language];
  const tags = (list || []).map(l => String(l).toLowerCase());
  if (tags.some(t => t.indexOf('zh') === 0)) return 'zh';
  if (tags.some(t => t.indexOf('en') === 0)) return 'en';
  return 'en';
}
// storedLang is what the reader chose last time. It is a read of browser
// storage, which may be unavailable — a private window, or cleared data — so it
// may simply find nothing, and the browser's own preference stands.
let lang = (function () {
  try { return localStorage.getItem('labs.lang') || ''; } catch (e) { return ''; }
})();
if (!STRINGS[lang]) lang = bestLocale();
function locale() { return (LANGS.find(l => l.code === lang) || LANGS[0]).locale; }
// t looks a string up and fills its {placeholders}. A missing string returns its
// key: the page then shows a name it has for the thing rather than nothing, which
// is the difference between a bug you can read and a gap you cannot.
function t(key, vars) {
  const table = STRINGS[lang] || STRINGS.en;
  let s = (key in table) ? table[key] : (key in STRINGS.en ? STRINGS.en[key] : key);
  if (vars) for (const k in vars) s = s.split('{' + k + '}').join(String(vars[k]));
  return s;
}
function setLang(code) {
  if (!STRINGS[code] || code === lang) return;
  lang = code;
  try { localStorage.setItem('labs.lang', code); } catch (e) {}
  renderStatic();
  renderLang();
  setFooter(cfg);   // the build line is prose too, and the date is formatted to the language
  render();
  if (km.open) renderModalText();
}
// renderStatic writes the words that belong to the page rather than to any data:
// the lead, and the accessible names of the three controls that carry one. They
// are set rather than baked into the markup so switching the language reaches
// them as well — an aria-label is read aloud, so leaving it in the other
// language is the same mistake as leaving the prose.
function renderStatic() {
  leadEl.textContent = t('lead');
  tabsBox.setAttribute('aria-label', t('kind.aria'));
  tmplLabel.textContent = t('tmpl.label');
  langBox.setAttribute('aria-label', t('lang.aria'));
}
// renderLang keeps the switch's one button honest, and the document's own
// language with it, so the browser hyphenates and reads the page as what it is.
//
// One button that says what it would switch to, not two that say what is on.
// On a control the size of a masthead the pair was a segmented control with a
// filled half, which reads as a setting that is set rather than as something to
// press; a single button offers the other language and does that. The label is
// the language's own word for itself, so the button is readable to whoever
// needs it — "中文" to a Chinese reader, not "Chinese".
function renderLang() {
  document.documentElement.lang = lang === 'zh' ? 'zh-CN' : 'en';
  langBox.replaceChildren();
  const other = LANGS.find(l => l.code !== lang) || LANGS[0];
  const b = document.createElement('button');
  b.type = 'button';
  b.textContent = other.label;
  b.title = t('lang.aria');
  b.setAttribute('aria-label', t('lang.aria') + ': ' + other.label);
  b.onclick = () => setLang(other.code);
  langBox.append(b);
}
// The page keeps one live copy of what it last read, and redraws from it.
// Provision changes what should be on screen before the next poll answers — a
// create that was refused can start an environment — so the redraws are driven
// by this rather than by the poll alone.
let cfg = {};
// kind is the kind being read. It is state rather than a read of the tabs,
// because the tabs are redrawn on every poll: the rows are updated in place
// rather than rebuilt — rebuilding would replace a button under the pointer —
// so there is no element whose value is the answer.
let kind = '';
// tabRows holds one live tab per kind, so the strip is updated rather than
// rebuilt on every poll. Keyed by kind, in the order the kinds are first seen.
const tabRows = new Map();
// rememberedKind is the kind the last visit was left on, so a reload comes back
// to it. It is a read of browser storage, which may be unavailable — a private
// window, or cleared data — so it may simply find nothing.
function rememberedKind() {
  try { return localStorage.getItem('labs.kind') || ''; } catch (e) { return ''; }
}
// rememberKind keeps that choice for the next visit.
function rememberKind(k) {
  try { localStorage.setItem('labs.kind', k); } catch (e) {}
}
function currentKind() { return kind; }
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
  const h = document.createElement('h2'); h.textContent = t('unconfigured'); box.append(h);
  const ul = document.createElement('ul');
  (list || []).forEach(p => { const li = document.createElement('li'); li.textContent = p; ul.append(li); });
  box.append(ul);
  const hint = document.createElement('p'); hint.className = 'lead'; hint.style.margin = '.75rem 0 0';
  hint.textContent = t('redeploy');
  const a = document.createElement('a'); a.href = 'api/v1/config'; a.textContent = 'GET /api/v1/config';
  hint.append(a, document.createTextNode(t('fullstate')));
  box.append(hint);
  return box;
}
// renderTabs draws the kind tabs: one per kind the deployment serves, the kind
// being read marked as selected.
//
// The tabs are grown once and then updated, never rebuilt. The poll runs every
// five seconds and this is a row of buttons: replacing one between a pointer
// going down and coming up swallows the click, and the row a person navigates
// with is the last place that should happen. So a tab is created when its kind
// first appears and removed when it goes away — which a deployment reconfigured
// under us can do — and otherwise only its label and state are rewritten.
//
// Each tab carries the kind's own state as a dot, so the strip answers "is the
// other one up" without being clicked. That is the thing a dropdown cannot show.
function renderTabs(served) {
  const want = new Set(served.map(e => e.kind));
  for (const [k, row] of tabRows) {
    if (!want.has(k)) { row.btn.remove(); tabRows.delete(k); }
  }
  served.forEach(e => {
    let row = tabRows.get(e.kind);
    if (!row) {
      const btn = document.createElement('button');
      btn.type = 'button';
      btn.setAttribute('role', 'tab');
      btn.dataset.kind = e.kind;
      const tick = document.createElement('span'); tick.className = 'tick';
      const name = document.createElement('span'); name.textContent = e.kind;
      btn.append(tick, name);
      btn.onclick = () => selectKind(e.kind);
      tabsBox.append(btn);
      row = { btn, tick };
      tabRows.set(e.kind, row);
    }
    row.btn.setAttribute('aria-selected', e.kind === kind ? 'true' : 'false');
    const state = e.ready ? 'ok' : (e.unauthorized ? 'err' : 'warn');
    row.tick.className = 'tick ' + state;
    row.btn.title = e.ready ? t('tabtitle.ready', { kind: e.kind })
      : (e.unauthorized ? t('tabtitle.key', { kind: e.kind }) : t('tabtitle.starting', { kind: e.kind }));
  });
  tabsBox.hidden = served.length === 0;
}
// selectKind is the tab: it changes what the panel is about, remembers the
// choice so a reload comes back to it, and drops any refusal that was on screen
// — an error is an answer to the kind that was showing, not to the one just
// chosen.
function selectKind(k) {
  if (k === kind) return;
  kind = k;
  rememberKind(k);
  error = ''; retryable = false; problems_ = null;
  render();
}
// render draws the whole page from one reading of /config.
//
// There is one panel and one kind on screen at a time, so this is a redraw of
// that panel rather than a reconciliation of several: the tabs decide what the
// rest of it is about, and everything below them belongs to the kind they name.
// The tabs themselves are updated in place rather than rebuilt — the poll runs
// every five seconds, and a button replaced under a click cannot be clicked.
function render() {
  const list = cfg.configured === false ? [] : (cfg.environments || []);
  const labs = cfg.labs || [];
  const templates = cfg.templates || {};

  // A kind that has gone away — a deployment reconfigured under us — must not
  // be the one being read, or the panel below describes an environment that no
  // longer exists. One served is kept if it still is; otherwise the remembered
  // one, and failing that the first.
  const served = list.filter(e => e.kind);
  if (!served.some(e => e.kind === kind)) {
    const remembered = rememberedKind();
    const pick = served.find(e => e.kind === remembered) || served[0];
    kind = pick ? pick.kind : '';
  }
  renderTabs(served);

  // The template picker is shown for exactly the kinds that offer a choice, and
  // is filled from that kind's list each time — so it can never be stale, and
  // there is no second copy of the templates to keep in step with the poll.
  const choices = templates[kind] || [];
  const keepT = tmplSel.value;
  tmplSel.replaceChildren();
  choices.forEach(o => {
    const opt = document.createElement('option');
    opt.value = o.id;
    opt.textContent = o.title && o.title !== o.id ? o.title + ' — ' + o.id : o.id;
    if (o.description) opt.title = o.description;
    tmplSel.append(opt);
  });
  if (choices.some(o => o.id === keepT)) tmplSel.value = keepT;
  choiceBox.hidden = choices.length === 0;

  // What this kind is for, in one line. It is the first thing under the tabs
  // because it is what the tab is a name for: the two kinds do different things,
  // and the state below says whether one is up without saying what it does.
  blurbBox.textContent = kind === 'sandboxlab' ? t('blurb.sandboxlab')
    : kind === 'applab' ? t('blurb.applab') : '';

  // The kind's two states, which are two different facts. The cluster is the
  // environment this service drives: is a run going, is anything serving the
  // address. The app is what that environment hands out: is the run it made
  // reachable from here. The second is only asked when the first is up — there
  // is nothing at the other end of a request to a cluster that is not running —
  // so "no run" is the first one's answer, not the second's.
  //
  // They are drawn separately because the failure they rule out is a real one:
  // a cluster that is serving and refusing the key this page holds used to be a
  // single word on the page ("key needed"), which reads as the environment
  // being down when it is in fact up and answering.
  const env = list.find(e => e.kind === kind);
  const up = !!(env && env.ready);
  const cluster = !env ? null
    : env.ready ? { cls: 'ok', word: t('state.ready') }
      : env.unauthorized ? { cls: 'err', word: t('state.gone') }
        : { cls: 'warn', word: t('state.starting') };
  const app = !up ? null
    : env.unauthorized ? { cls: 'err', word: t('state.key') }
      : { cls: 'ok', word: t('state.ready') };
  usageBox.replaceChildren();
  if (cluster || app) {
    const box = document.createElement('div'); box.className = 'states';
    [[t('what.cluster'), cluster], [t('what.app'), app]].forEach(([what, st]) => {
      // The app's line is only there once the cluster is up and there is an app
      // to describe; below that it is a second dot saying nothing.
      if (!st) return;
      const line = document.createElement('span'); line.className = 'state ' + st.cls;
      const w = document.createElement('span'); w.className = 'what'; w.textContent = what;
      const v = document.createElement('span'); v.textContent = st.word;
      line.append(w, v);
      box.append(line);
    });
    // How much the kind holds is the cluster's number and only exists when there
    // is a cluster to ask: an environment that is not up is never asked, so its
    // count is zero by construction, and "0 of 8 in use" under "starting" is a
    // number nobody produced being read by someone deciding whether to press the
    // button. The reason line is the honest answer there. It rides on the states'
    // line rather than below it because it is a property of what is on that
    // line, not a separate thought.
    const capacity = env && env.capacity;
    if (up && capacity) {
      const n = document.createElement('span'); n.className = 'usage';
      n.textContent = kind === 'sandboxlab'
        ? t('usage.sandboxes', { n: env.occupied, cap: capacity })
        : t('usage.slots', { n: env.occupied, cap: capacity });
      box.append(n);
    }
    usageBox.append(box);
  }
  reasonBox.textContent = (env && !env.ready && env.message) ? env.message : '';
  // One word for both states. The button says what pressing it asks for — an
  // environment — not what the environment happens to be doing right now; that
  // is what the lines above it are for. A kind that is up still hands out a lab
  // and one that is not is what the press starts, and the two differ in what
  // comes back, not in what is being asked for. It is never hidden: a kind with
  // nothing running still needs a way to ask, since asking is what starts it.
  createBtn.textContent = t('env.start');
  createBtn.disabled = !kind;

  renderLabs(labs.filter(l => l.kind === kind));

  // A panel is drawn only once there is a kind to put in it; otherwise an
  // unconfigured deployment shows an empty outline.
  panel.classList.toggle('has', !!kind);
  renderError();
  maybeAutoStart(kind, env, up);
}
// maybeAutoStart brings a kind up by itself, so that a visitor arriving at a
// kind nobody has started yet is not the one who has to work out that they can.
//
// The condition is the one the page already draws as "starting": the kind is
// served and the page is reading it, its environment is neither up nor
// unauthorized, and nothing of the visitor's is already being made. The request
// is the same one the button sends, and the server answers it the same way —
// with the environment a few minutes out — so what this changes is who asks
// first, not what is asked for.
//
// It is guarded in time, not in state. A trigger that only ever fired once
// would leave a kind that came up and then went down sitting idle, so the page
// keeps asking on the poll's slow beat; the gap is well over the couple of
// minutes an environment takes, which is what keeps one down environment from
// being asked about twice a second. The request goes through the visitor's own
// rate limit, and that limit therefore counts auto-starts as well as presses.
let autoStartAt = 0;
const autoStartGap = 120000; // 2 min, about one environment's boot
function maybeAutoStart(kind, env, up) {
  if (!kind || !env || env.ready || env.unauthorized) return;
  if (busy || error || Date.now() - autoStartAt < autoStartGap) return;
  autoStartAt = Date.now();
  create({ silent: true });
}
// renderLabs draws what the kind is running as a table, one row per instance.
//
// A table rather than the rows this used to draw, because these are records with
// the same fields in each: the column headings then say what each value is once
// rather than on every row. The state is passed through in the environment's own
// word — applab's "created" is an app record with nothing deployed behind it, and
// a friendlier word would be this page's guess at what that implies — so the
// state column is not translated and is set in a fixed-width face to say so.
//
// The times are the two columns that differ per row: when the instance was made,
// and when it ends. Both are drawn from the row's own data attributes by tick(),
// which is what makes them count down without the table being rebuilt.
function renderLabs(mine) {
  labsBox.replaceChildren();
  const items = [];
  (mine || []).forEach(l => (l.labs || []).forEach(it => items.push(it)));
  if (!items.length) {
    const p = document.createElement('p'); p.className = 'empty'; p.textContent = t('empty');
    labsBox.append(p);
    return;
  }
  const headings = [
    [t('col.id'), 'id'],
    [t('col.state'), 'st'],
    [t('col.created'), 'meta'],
    [t('col.expires'), 'meta'],
  ];
  const table = document.createElement('table'); table.className = 'labs-table';
  const head = document.createElement('thead');
  const hrow = document.createElement('tr');
  headings.forEach(([label, cls]) => {
    const th = document.createElement('th');
    th.className = cls; th.textContent = label;
    hrow.append(th);
  });
  head.append(hrow);
  const body = document.createElement('tbody');
  items.forEach(it => {
    const tr = document.createElement('tr');
    tr.append(Object.assign(document.createElement('td'), { className: 'id', textContent: it.id }));
    // A state the environment did not name is a blank cell rather than an empty
    // label: there is nothing to say, and the column is already headed.
    tr.append(Object.assign(document.createElement('td'), { className: 'st', textContent: it.state ? String(it.state).toLowerCase() : '' }));
    const created = document.createElement('td'); created.className = 'meta'; created.dataset.created = it.created_at || '';
    const expires = document.createElement('td'); expires.className = 'meta'; expires.dataset.expires = expiresOf(it);
    tr.append(created, expires);
    body.append(tr);
  });
  table.append(head, body);
  labsBox.append(table);
  tick();
}
// setFooter renders the build identity.
//
// The time is the build's, not the moment of the last poll. An "updated" line
// was a freshness signal — this page polls every few seconds, so a stale one
// means it has stopped — but it read as a second, unexplained clock next to the
// commit and said nothing about the thing the footer is actually about: which
// build is running. The build time answers that, and it is what someone
// checking whether a deploy landed is looking for.
function setFooter(b) {
  const parts = [];
  if (b.commit) parts.push(t('footer.commit', { c: b.commit }));
  // Shown only when the build carried one. A build that was never told when it
  // happened reports "unknown", and a date formatted from that is not a date.
  if (b.build_time && b.build_time !== 'unknown') parts.push(t('footer.built', { d: new Date(b.build_time).toLocaleString(locale()) }));
  build.textContent = parts.join(' · ');
}
// countdown is how long a lab has left, as "1h 57m" or "3m 12s". It is the
// product's promise, so it is shown counting down rather than as an end time
// the reader has to subtract from.
function countdown(expiresAt) {
  let s = Math.floor((new Date(expiresAt).getTime() - Date.now()) / 1000);
  if (s <= 0) return t('dur.expired');
  const h = Math.floor(s / 3600), m = Math.floor((s % 3600) / 60);
  s = s % 60;
  if (h) return t('dur.hm', { h: h, m: String(m).padStart(2, '0') });
  if (m) return t('dur.ms', { m: m, s: String(s).padStart(2, '0') });
  return t('dur.s', { s: s });
}
// sinceFmt is how long ago a lab was created, as "12m ago".
function sinceFmt(createdAt) {
  let s = Math.floor((Date.now() - new Date(createdAt).getTime()) / 1000);
  if (s < 60) return t('ago.just');
  const m = Math.floor(s / 60), h = Math.floor(m / 60);
  if (h) return t('ago.hm', { h: h, m: m % 60 });
  return t('ago.m', { m: m });
}
let sessionTTL = 0; // seconds; from /config, for kinds that report no expiry
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
  document.querySelectorAll('#panel [data-expires]').forEach(e => {
    e.textContent = e.dataset.expires ? t('expires.in', { d: countdown(e.dataset.expires) }) : '';
  });
  document.querySelectorAll('#panel [data-created]').forEach(e => {
    e.textContent = e.dataset.created ? t('created.at', { d: sinceFmt(e.dataset.created) }) : '';
  });
  // The dialog's own countdown, so the hand-off keeps ticking while it is open.
  document.querySelectorAll('#km-body [data-expires]').forEach(e => {
    e.textContent = e.dataset.expires
      ? t('left', { d: countdown(e.dataset.expires), at: new Date(e.dataset.expires).toLocaleTimeString(locale()) })
      : '';
  });
}
setInterval(tick, 1000);
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
    cfg = body.data || {};
    sessionTTL = cfg.session_ttl_seconds || 0;
    setFooter(cfg);
    render();
    if (cfg.configured === false && (cfg.problems || []).length) {
      status.replaceChildren(problems(cfg.problems));
    } else {
      status.replaceChildren();
    }
  } catch (e) {
    // Say so rather than showing nothing, which is what a silent failure looks
    // like: the cluster status simply never appears. What was last read stays
    // on the page, since it is still the most recent thing known.
    status.replaceChildren(row(t('row.status'), t('err.reach', { m: e.message }), 'err'));
    render();
  }
}
// create asks for one lab. The kind and template are read from the controls at
// the moment of the press, so what is sent is what is on screen.
//
// A failure goes into the panel above these controls, under the thing it is
// about, and the poll is asked for again immediately: a refused request is one
// that may have started an environment, and the page should show that rather
// than sit on a state it now knows is old.
//
// opts.silent is how the page's own trigger asks (see maybeAutoStart). It does
// not touch the button, because nobody pressed it, and it does not draw a
// retryable refusal: "one is being started" is the answer that trigger is
// after, not a fault the visitor should be shown. A failure that is not
// retryable is still drawn — that one is about the deployment, not about
// waiting.
async function create(opts) {
  const silent = !!(opts && opts.silent);
  const kind = currentKind();
  if (!kind || busy) return;
  busy = true;
  const template = choiceBox.hidden ? '' : tmplSel.value;
  if (!silent) {
    error = '';
    createBtn.disabled = true;
    createBtn.textContent = t('creating');
    renderError();
  }
  const req = { kind: kind };
  if (template) req.template = template;
  try {
    const { ok, status: code, body } = await fetchJSON('api/v1/labs', {
      method: 'POST', headers: {'Content-Type':'application/json'}, body: JSON.stringify(req) });
    if (!ok) {
      if (!silent || !body.retryable) showError(body, code);
      loadStatus(); // show the environment the request just started
      return;
    }
    showLab(body.data, kind);
    loadStatus(); // the new lab appears in the running list on the next poll
  } catch (e) {
    if (!silent) {
      error = t('err.reach', { m: e.message });
      renderError();
    }
  } finally {
    // Back to whatever the environment now warrants. render() is what decides
    // the button's word, so the progress text written above is always undone.
    busy = false;
    render();
  }
}
// busy is true while a request of this page's own is in flight, whether the
// visitor pressed the button or the page did. A second one while the first is
// out would spend another unit of the rate limit to ask for the same thing.
let busy = false;
// error is a failed request's message, held rather than drawn on the spot so
// the poll does not wipe it while it is on screen. Only a new request or a
// successful one clears it.
let error = '';
let retryable = false;
let problems_ = null;
// showError records a failed request's message, with the retryable marker when
// the answer said so.
function showError(body, code) {
  error = body.error || ('HTTP ' + code);
  retryable = !!body.retryable;
  problems_ = (body.problems || []).length ? body.problems : null;
  renderError();
}
// renderError draws the panel's current error, or clears it when there is none.
function renderError() {
  errBox.replaceChildren();
  if (!error) return;
  const d = row(t('row.error'), error, 'err');
  if (retryable) d.append(Object.assign(document.createElement('span'), { textContent: ' ' + t('err.retryable') }));
  errBox.append(d);
  if (problems_) errBox.append(problems(problems_));
}
// openModal shows the dialog, and closeModal hides it. Both go through the
// dialog element so the backdrop and Escape keep working, with the attribute as
// the fallback for a browser that has no showModal.
function openModal() {
  document.body.classList.add('modal-open');
  if (typeof km.showModal === 'function') { if (!km.open) km.showModal(); }
  else km.setAttribute('open', '');
  kmClose.onclick = () => closeModal();
  km.onclose = () => document.body.classList.remove('modal-open');
}
function closeModal() {
  if (typeof km.close === 'function') km.close(); else km.removeAttribute('open');
  document.body.classList.remove('modal-open');
}
// showLab is the hand-off: the one moment a key is shown, in a dialog that says
// so. The key is not kept anywhere on the page afterwards — the running list
// below never carries it — so the dialog is explicit that closing it is the last
// chance to take it, and offers a copy button rather than a string to select by
// hand.
function showLab(d, kind) {
  shownLab = Object.assign({ kind: kind }, d);
  renderModalText(shownLab);
  openModal();
  tick();
}
// renderModalText draws the dialog's words from the lab it is handing over, and
// is called again when the language changes while it is open — the modal is a
// dialog over the page, and switching the page under it should not leave it in
// the other language.
function renderModalText(d) {
  d = d || shownLab;
  if (!d) return;
  kmEyebrow.textContent = t('ready');
  kmTitle.textContent = t('modaltitle', { kind: d.kind || '' });
  kmBody.replaceChildren();
  kmError.replaceChildren();

  kmBody.append(row(t('row.console'), '<a href="' + d.console_url + '" target="_blank" rel="noopener">' + d.console_url + '</a>'));
  kmBody.append(keyRow(d.api_key));
  kmBody.append(row(t('row.expires'), '<span data-expires="' + d.expires_at + '"></span>'));
  if (d.app) kmBody.append(row(t('row.app'), '<code>' + d.app + '</code>'));
  if (d.template) kmBody.append(row(t('row.template'), '<code>' + d.template + '</code>'));
  kmBody.append(row(t('row.how'), d.kind === 'sandboxlab' ? t('how.sandbox') : t('how.other')));

  if (d.warning) {
    const w = document.createElement('div'); w.className = 'warnbox'; w.textContent = d.warning;
    kmBody.append(w);
  }
  const once = document.createElement('div');
  once.className = 'once';
  once.textContent = t('once');
  kmBody.append(once);

  kmClose.textContent = t('close');
  kmConfirm.disabled = false;
  kmConfirm.textContent = t('open');
  kmConfirm.onclick = () => window.open(d.console_url, '_blank', 'noopener');
}
// shownLab is the lab the dialog is currently handing over, kept so the dialog
// can be redrawn in another language without the caller having to hold it.
let shownLab = null;
// keyRow renders the one-time key with a copy button. It is the only place a
// key ever appears, and it is not stored anywhere on the page afterwards.
function keyRow(key) {
  const r = row(t('row.key'), '', 'key');
  const v = r.querySelector('.v');
  v.className = 'v keyline';
  const code = document.createElement('code'); code.textContent = key || t('none');
  const copy = document.createElement('button'); copy.className = 'small'; copy.textContent = t('copy');
  copy.onclick = async () => {
    try { await navigator.clipboard.writeText(key); copy.textContent = t('copied'); }
    catch (e) { copy.textContent = t('select'); }
    setTimeout(() => { copy.textContent = t('copy'); }, 1500);
  };
  v.append(code, copy);
  return r;
}
// The button is the one action on the page; the tabs that change what the page
// is about are wired per tab in renderTabs.
createBtn.onclick = create;
renderStatic();
renderLang();
loadStatus();
// Poll, so an environment that is booting becomes a lab without a reload. The
// interval is short enough to feel live and long enough not to hammer.
setInterval(() => { if (!document.hidden) loadStatus(); }, 5000);
</script>
</body>
</html>
`
