/* Copilot panel — docs/ai_copilot_design.md, server side in
   cmd/web/copilot.go, replies drawn by static/copilot-render.js.

   Layout: html[data-copilot] = dock | overlay | sheet (absent = closed),
   computed from the viewport, never chosen. dock pushes the page so the
   chart reflows instead of being covered; overlay floats when the page would
   get too narrow; sheet is full screen on a phone. A docked panel stays open
   across pages (localStorage; partials.html applies it before paint).

   The conversation lives in this tab (sessionStorage); each question sends
   the recent turns and the page the user is on. Model text is never parsed
   as HTML: copilot-render.js builds nodes with textContent, and if it throws
   the reply is shown as plain text. */
(function () {
  'use strict';
  var I = window.I18N || {};
  var root = document.documentElement;
  var box = document.getElementById('copilot');
  var openBtn = document.getElementById('copilot-open');
  if (!box || !openBtn) return;
  var log = document.getElementById('copilot-log');
  var empty = document.getElementById('copilot-empty');
  var chips = document.getElementById('copilot-chips');
  var form = document.getElementById('copilot-form');
  var input = document.getElementById('copilot-input');
  var send = document.getElementById('copilot-send');
  var quotaEl = document.getElementById('copilot-quota');
  var modelSel = document.getElementById('copilot-model');
  var kbd = document.getElementById('copilot-kbd');
  var lang = box.dataset.lang === 'en' ? 'en' : 'zh';
  var symbols = (box.dataset.symbols || '').split(/\s+/).filter(Boolean);

  var MIN_W = 320, MAX_W = 720, DEF_W = 400, MIN_CONTENT = 560;
  var KEY = 'copilot.msgs';
  var msgs = load();
  var busy = null; // the AbortController of the request in flight
  var modelsLoaded = false;
  var mode = root.getAttribute('data-copilot') || '';

  function lsGet(k) { try { return localStorage.getItem(k); } catch (e) { return null; } }
  function lsSet(k, v) { try { localStorage.setItem(k, v); } catch (e) {} }
  function load() {
    try { var v = JSON.parse(sessionStorage.getItem(KEY) || '[]'); return Array.isArray(v) ? v : []; } catch (e) { return []; }
  }
  function save() {
    try { sessionStorage.setItem(KEY, JSON.stringify(msgs.slice(-30))); } catch (e) {}
  }
  function fmt(s) {
    var args = Array.prototype.slice.call(arguments, 1), i = 0;
    return String(s || '').replace(/%[sd]/g, function () { return i < args.length ? String(args[i++]) : ''; });
  }
  function el(tag, cls, text) {
    var e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text != null) e.textContent = text;
    return e;
  }
  function icon(name) {
    var t = document.getElementById('ico-' + name);
    return t ? t.content.firstElementChild.cloneNode(true) : document.createTextNode('');
  }
  function shortModel(m) { return String(m || '').replace(/^.*:/, '').replace(/^gemini-/, ''); }
  function clock(ms) {
    var d = new Date(ms + 8 * 3600e3), p = function (n) { return (n < 10 ? '0' : '') + n; };
    return p(d.getUTCHours()) + ':' + p(d.getUTCMinutes());
  }
  function params() { try { return new URLSearchParams(location.search); } catch (e) { return { get: function () { return null; } }; } }

  // ---- layout -------------------------------------------------------------

  function railW() { return innerWidth >= 1024 ? (root.getAttribute('data-rail') === 'narrow' ? 64 : 200) : 0; }
  function savedW() { return parseInt(lsGet('copilot.w'), 10) || DEF_W; }
  // Keep in step with the pre-paint copy in partials.html.
  function layout() {
    if (innerWidth <= 560) return { mode: 'sheet', w: innerWidth };
    var fit = innerWidth - railW() - MIN_CONTENT;
    if (innerWidth >= 1024 && fit >= MIN_W) return { mode: 'dock', w: Math.max(MIN_W, Math.min(savedW(), MAX_W, fit)) };
    return { mode: 'overlay', w: Math.min(savedW(), innerWidth - 48) };
  }
  function apply(open, animate) {
    var L = layout();
    if (animate) {
      root.classList.add('copilot-anim');
      clearTimeout(apply.t);
      apply.t = setTimeout(function () { root.classList.remove('copilot-anim'); }, 260);
    }
    box.classList.toggle('is-sheet', L.mode === 'sheet');
    if (open) {
      root.style.setProperty('--copilot-w', L.w + 'px');
      root.setAttribute('data-copilot', L.mode);
      mode = L.mode;
    } else {
      root.removeAttribute('data-copilot');
      mode = '';
    }
    box.inert = !open;
    openBtn.setAttribute('aria-expanded', open ? 'true' : 'false');
    document.dispatchEvent(new CustomEvent('copilot:layout', { detail: { mode: mode, w: open ? L.w : 0 } }));
  }
  function isOpen() { return !!root.getAttribute('data-copilot'); }
  function setOpen(open, focus) {
    if (open === isOpen()) { if (open && focus) input.focus(); return; }
    var dockable = layout().mode === 'dock';
    apply(open, true);
    if (dockable) lsSet('copilot.open', open ? '1' : '0');
    if (open) {
      refreshStatus(); loadModels();
      if (focus !== false) setTimeout(function () { input.focus(); }, 0);
    }
  }
  var rafPending = false;
  function relayout() {
    if (rafPending) return;
    rafPending = true;
    requestAnimationFrame(function () { rafPending = false; apply(isOpen(), false); });
  }
  window.addEventListener('resize', relayout);
  new MutationObserver(relayout).observe(root, { attributes: true, attributeFilter: ['data-rail'] });

  // ---- quota + model --------------------------------------------------------

  function showQuota(q) {
    if (!quotaEl || !Array.isArray(q) || !q.length) return;
    var cur = modelSel && modelSel.dataset.current;
    var live = q.filter(function (m) { return !m.blocked && (!cur || m.model === cur); })[0] ||
               q.filter(function (m) { return !m.blocked; })[0];
    if (!live) {
      quotaEl.textContent = fmt(I['copilot.left'], 0);
      quotaEl.title = fmt(I['copilot.quotaOut'], q[0].model);
      quotaEl.className = 'copilot-quota out';
      return;
    }
    if (live.remaining) {
      quotaEl.textContent = fmt(I['copilot.left'], live.remaining);
      quotaEl.title = fmt(I['copilot.quotaLeft'], live.model, live.remaining);
      quotaEl.className = 'copilot-quota' + (live.remaining <= 5 ? ' low' : '');
    } else {
      quotaEl.textContent = fmt(I['copilot.quotaToday'], live.used || 0);
      quotaEl.title = fmt(I['copilot.quota'], live.model, live.used || 0);
      quotaEl.className = 'copilot-quota';
    }
  }
  function refreshStatus() {
    fetch('/api/copilot/status', { credentials: 'same-origin' }).then(function (r) { return r.json(); }).then(function (j) {
      if (j && j.enabled) showQuota(j.quota);
      else if (quotaEl) { quotaEl.textContent = '—'; quotaEl.title = I['copilot.err.off'] || ''; quotaEl.className = 'copilot-quota out'; }
    }).catch(function () {});
  }
  // The selector is the same setting the server persists for every AI call
  // (/api/ai/model); the chosen model is tried first, fallbacks after it.
  function loadModels() {
    if (!modelSel || modelsLoaded) return;
    fetch('/api/ai/model', { credentials: 'same-origin' }).then(function (r) { return r.json(); }).then(function (j) {
      var list = (j && j.available) || [];
      if (j && j.current && list.indexOf(j.current) < 0) list.unshift(j.current);
      if (!list.length) return;
      modelSel.textContent = '';
      list.forEach(function (m) {
        var o = el('option', null, shortModel(m));
        o.value = m;
        if (m === j.current) o.selected = true;
        modelSel.appendChild(o);
      });
      modelSel.dataset.current = j.current || '';
      modelsLoaded = true;
    }).catch(function () {});
  }
  if (modelSel) modelSel.addEventListener('change', function () {
    var v = modelSel.value, prev = modelSel.dataset.current || '';
    modelSel.disabled = true;
    fetch('/api/ai/model', {
      method: 'POST', credentials: 'same-origin',
      headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
      body: new URLSearchParams({ model: v }).toString()
    }).then(function (r) {
      if (!r.ok) throw new Error('switch failed');
      modelSel.dataset.current = v;
      refreshStatus();
    }).catch(function () {
      modelSel.value = prev;
      errorMsg(I['copilot.modelErr'] || 'model switch failed');
    }).then(function () { modelSel.disabled = false; });
  });

  // ---- messages ---------------------------------------------------------------

  function nearBottom() { return log.scrollHeight - log.scrollTop - log.clientHeight < 80; }
  function add(node, stick) {
    var wasNear = nearBottom();
    log.appendChild(node);
    if (stick || wasNear) log.scrollTop = log.scrollHeight;
    return node;
  }
  function toolLabel(t) {
    var a = t.args || {}, name = I['copilot.tool.' + t.name] || t.name, arg = '';
    if (a.symbol) arg = String(a.symbol).replace(/-USDT$/i, '').toUpperCase() + (a.tf ? ' ' + a.tf : '');
    else if (a.id != null) arg = '#' + a.id;
    return arg ? name + ' ' + arg : name;
  }
  // trace is one quiet line above a reply: what the copilot looked up
  // (details on click), or a yellow "no data checked" when it answered
  // from memory — the user should know the reply is ungrounded.
  function trace(m) {
    if (Array.isArray(m.tools) && m.tools.length) {
      var d = el('details', 'cp-trace'), s = el('summary');
      s.appendChild(icon('chevron'));
      var ms = m.tools.reduce(function (a, t) { return a + (t.ms || 0); }, 0);
      var failed = m.tools.filter(function (t) { return t.error; }).length;
      var sum = fmt(I['copilot.checked'], m.tools.length) + ' · ' + m.tools.map(toolLabel).join('、') + (ms ? ' · ' + (ms / 1000).toFixed(1) + 's' : '');
      s.appendChild(el('span', 'cp-trace-sum', sum));
      if (failed) s.appendChild(el('span', 'cp-trace-fail', '· ' + fmt(I['copilot.failed'], failed)));
      d.appendChild(s);
      var body = el('div', 'cp-trace-body');
      m.tools.forEach(function (t) {
        var row = el('div', 'cp-trace-row');
        row.appendChild(el('span', 'cp-tool' + (t.error ? ' err' : ''), toolLabel(t)));
        var args = t.args ? Object.keys(t.args).map(function (k) { return k + '=' + t.args[k]; }).join(', ') : '';
        row.appendChild(el('span', null, t.name + '(' + args + ') · ' + (t.ms || 0) + 'ms' + (t.bytes ? ' · ' + (t.bytes / 1024).toFixed(1) + 'KB' : '')));
        body.appendChild(row);
        if (t.error) body.appendChild(el('div', 'err', t.error));
      });
      d.appendChild(body);
      return d;
    }
    if (Array.isArray(m.tools)) return el('div', 'cp-trace none', I['copilot.noTools'] || '');
    if (m.meta) return el('div', 'cp-trace', m.meta); // entries saved before the trace existed
    return null;
  }
  function body(text) {
    var b = el('div', 'cp-body');
    try {
      if (!window.CopilotRender) throw new Error('renderer not loaded');
      var tf = location.pathname === '/chart' ? params().get('tf') : '';
      b.appendChild(window.CopilotRender.render(text, { lang: lang, symbols: symbols.length ? symbols : undefined, tf: tf || '', doc: document }));
    } catch (e) {
      b.textContent = '';
      b.appendChild(el('div', 'cp-plain', text));
    }
    return b;
  }
  function assistant(m) {
    var d = el('div', 'copilot-msg assistant');
    var tr = trace(m);
    if (tr) d.appendChild(tr);
    d.appendChild(body(m.text || ''));
    if (m.note) d.appendChild(el('div', 'cp-note', m.note));
    var act = el('div', 'cp-actions');
    var copy = el('button', 'copilot-iconbtn');
    copy.type = 'button';
    copy.title = I['copilot.copy'] || 'Copy';
    copy.setAttribute('aria-label', copy.title);
    copy.appendChild(icon('copy'));
    copy.addEventListener('click', function () {
      var done = function () {
        copy.textContent = ''; copy.appendChild(icon('check'));
        setTimeout(function () { copy.textContent = ''; copy.appendChild(icon('copy')); }, 1200);
      };
      if (navigator.clipboard) navigator.clipboard.writeText(m.text || '').then(done, function () {});
    });
    act.appendChild(copy);
    var meta = [];
    if (m.at) meta.push(clock(m.at));
    if (m.model) meta.push(shortModel(m.model));
    if (meta.length) act.appendChild(el('span', null, meta.join(' · ')));
    d.appendChild(act);
    return d;
  }
  function user(m) { return el('div', 'copilot-msg user', m.text); }
  function errorMsg(text, opts) {
    opts = opts || {};
    var d = el('div', 'copilot-msg error', text);
    if (opts.retry) {
      var r = el('button', 'copilot-retry', I['copilot.retry'] || 'Retry');
      r.type = 'button';
      r.addEventListener('click', function () { d.remove(); request(); });
      d.appendChild(r);
    }
    if (opts.quota && modelSel) {
      var s = el('button', 'copilot-retry', I['copilot.switchModel'] || 'switch model');
      s.type = 'button';
      s.addEventListener('click', function () { modelSel.focus(); });
      d.appendChild(document.createTextNode(' · '));
      d.appendChild(s);
    }
    return add(d, true);
  }
  function markLast() {
    var all = log.querySelectorAll('.copilot-msg.assistant');
    Array.prototype.forEach.call(all, function (n, i) { n.classList.toggle('last', i === all.length - 1); });
  }
  function renderAll() {
    Array.prototype.slice.call(log.querySelectorAll('.copilot-msg')).forEach(function (n) { n.remove(); });
    msgs.forEach(function (m) { log.appendChild(m.role === 'user' ? user(m) : assistant(m)); });
    syncEmpty();
    markLast();
    log.scrollTop = log.scrollHeight;
  }
  function syncEmpty() {
    var none = !log.querySelector('.copilot-msg');
    empty.hidden = !none;
    if (kbd) kbd.hidden = !none;
    syncQuick();
  }

  // Suggestion chips fill the input; they never send (the free tier is
  // about 20 requests per model per day).
  function fillChips() {
    var p = location.pathname, list;
    if (p === '/chart') {
      var sym = (params().get('symbol') || 'BTC').replace(/-USDT$/i, '').toUpperCase(), tf = params().get('tf') || '1h';
      list = [fmt(I['copilot.chip.chart1'], sym, tf), fmt(I['copilot.chip.chart2'], sym), I['copilot.chip.macro'], I['copilot.chip.positions']];
    } else if (p.indexOf('/journal') === 0) {
      list = [I['copilot.chip.recent'], I['copilot.chip.losses'], I['copilot.chip.last']];
    } else {
      list = [I['copilot.chip.positions'], I['copilot.chip.recent'], I['copilot.chip.week'], I['copilot.chip.btc']];
    }
    chips.textContent = '';
    list.filter(Boolean).slice(0, 4).forEach(function (q) {
      var b = el('button', 'copilot-chip', q);
      b.type = 'button';
      b.addEventListener('click', function () { prefill(q); });
      chips.appendChild(b);
    });
  }

  // On /chart, once a conversation exists (the empty state has its own
  // chips), one quick "analyze this chart" chip sits above the input while
  // it is empty. It replaces the page's AI button, hidden while the panel
  // is open. Prefills only.
  var quick = document.getElementById('copilot-quick');
  function syncQuick() {
    if (!quick) return;
    var src = document.getElementById('chart-ai-btn');
    var show = !!src && !!log.querySelector('.copilot-msg') && !input.value.trim() && !busy;
    quick.hidden = !show;
    if (!show) return;
    var q = question(src);
    if (quick.dataset.q === q) return;
    quick.dataset.q = q;
    quick.textContent = '';
    var b = el('button', 'copilot-chip sm', q);
    b.type = 'button';
    b.addEventListener('click', function () { prefill(q); });
    quick.appendChild(b);
  }

  // ---- asking -----------------------------------------------------------------

  function setBusy(ctrl) {
    busy = ctrl;
    send.classList.toggle('stop', !!ctrl);
    var label = ctrl ? (I['copilot.stop'] || 'Stop') : (I['copilot.send'] || 'Send');
    send.title = label; send.setAttribute('aria-label', label);
    syncSend();
  }
  function syncSend() { send.disabled = !busy && !input.value.trim(); syncQuick(); }
  function grow() {
    input.style.height = 'auto';
    input.style.height = Math.min(input.scrollHeight, 200) + 'px';
    syncSend();
  }

  function ask(text) {
    if (busy) return;
    // A question that failed and was not retried is dropped from the history
    // sent, so turns keep alternating user / assistant.
    if (msgs.length && msgs[msgs.length - 1].role === 'user') msgs.pop();
    msgs.push({ role: 'user', text: text, at: Date.now() });
    save();
    add(user(msgs[msgs.length - 1]), true);
    syncEmpty();
    request();
  }
  // request sends the conversation as it is; a retry calls it again after a
  // failure without adding the question twice.
  function request() {
    if (busy || !msgs.length || msgs[msgs.length - 1].role !== 'user') return;
    var ctrl = window.AbortController ? new AbortController() : null;
    setBusy(ctrl || {});
    var started = Date.now();
    var pending = el('div', 'copilot-msg pending');
    var dots = el('span', 'cp-dots');
    dots.appendChild(el('span')); dots.appendChild(el('span')); dots.appendChild(el('span'));
    var secs = el('span', null, fmt(I['copilot.waiting'], 0));
    pending.appendChild(dots); pending.appendChild(secs);
    add(pending, true);
    var timer = setInterval(function () { secs.textContent = fmt(I['copilot.waiting'], Math.round((Date.now() - started) / 1000)); }, 1000);

    var payload = {
      messages: msgs.map(function (m) { return { role: m.role, text: m.text }; }),
      page: { path: location.pathname + location.search, title: document.title, context: '' }
    };
    fetch('/api/copilot/chat', {
      method: 'POST', credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
      signal: ctrl ? ctrl.signal : undefined
    }).then(function (r) {
      return r.json().then(function (j) { return { ok: r.ok, status: r.status, j: j || {} }; });
    }).then(function (res) {
      pending.remove();
      showQuota(res.j.quota);
      if (!res.ok) {
        errorMsg(res.j.error || I['copilot.err.net'], { retry: true, quota: res.status === 429 });
        return;
      }
      var m = { role: 'assistant', text: res.j.reply || '', note: res.j.note || '', tools: res.j.tools || [], model: res.j.model || '', at: Date.now() };
      msgs.push(m);
      save();
      var node = assistant(m);
      log.appendChild(node);
      markLast();
      // Read a long reply from its verdict down, not from its last line.
      log.scrollTop = Math.max(0, node.offsetTop - 12);
    }).catch(function (e) {
      pending.remove();
      if (e && e.name === 'AbortError') add(el('div', 'cp-note', I['copilot.stopped'] || 'Stopped.'), true);
      else errorMsg(I['copilot.err.net'] || 'network error', { retry: true });
    }).then(function () {
      clearInterval(timer);
      setBusy(null);
      syncEmpty();
      input.focus();
    });
  }

  // ---- wiring -----------------------------------------------------------------

  openBtn.addEventListener('click', function () { setOpen(!isOpen()); });
  document.getElementById('copilot-close').addEventListener('click', function () { setOpen(false); openBtn.focus(); });
  document.getElementById('copilot-clear').addEventListener('click', function () {
    if (busy && busy.abort) busy.abort();
    msgs = []; save(); renderAll(); input.focus();
  });
  document.addEventListener('keydown', function (e) {
    // ⌘/ or Ctrl+/: open and focus; focus if open elsewhere; close if the
    // focus is already in the panel.
    if ((e.metaKey || e.ctrlKey) && e.key === '/') {
      e.preventDefault();
      if (!isOpen()) setOpen(true);
      else if (box.contains(document.activeElement)) { setOpen(false); openBtn.focus(); }
      else input.focus();
      return;
    }
    if (e.key === 'Escape' && isOpen()) {
      // A docked panel is part of the layout, like a sidebar: Esc leaves it.
      if (mode === 'dock') { if (box.contains(document.activeElement)) document.activeElement.blur(); return; }
      setOpen(false); openBtn.focus();
    }
  });
  form.addEventListener('submit', function (e) {
    e.preventDefault();
    if (busy) { if (busy.abort) busy.abort(); return; }
    var text = input.value.trim();
    if (!text) return;
    input.value = '';
    grow();
    ask(text);
  });
  input.addEventListener('input', grow);
  input.addEventListener('keydown', function (e) {
    // Enter sends; Shift+Enter is a new line; never while an IME (Chinese
    // input) is still composing, where Enter confirms the characters. Enter
    // never stops a request: only the stop button does.
    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing && e.keyCode !== 229) {
      e.preventDefault();
      if (busy) return;
      form.requestSubmit ? form.requestSubmit() : form.dispatchEvent(new Event('submit', { cancelable: true }));
    }
  });

  // Page buttons with data-copilot open the panel with a question filled in
  // (not sent). data-copilot = symbol | trade | validate.
  function question(el_) {
    var d = el_.dataset;
    switch (d.copilot) {
      case 'symbol': return fmt(I['copilot.ask.symbol'], (d.symbol || '').toUpperCase(), d.tf || '');
      case 'trade': return fmt(I['copilot.ask.trade'], d.id || '');
      case 'validate': return fmt(I['copilot.ask.validate'], (d.side || '').toLowerCase(), (d.symbol || '').toUpperCase(), d.tf || '', d.entry || '');
    }
    return '';
  }
  function prefill(text) {
    setOpen(true, false);
    if (!text) { input.focus(); return; }
    input.value = text;
    grow();
    setTimeout(function () { input.focus(); input.setSelectionRange(text.length, text.length); }, 0);
  }
  document.addEventListener('click', function (e) {
    var t = e.target.closest && e.target.closest('[data-copilot]');
    if (!t || box.contains(t)) return;
    e.preventDefault();
    prefill(question(t));
  });
  window.Copilot = { prefill: prefill };

  fillChips();
  renderAll();
  syncSend();
  // Adopt what the pre-paint script set (a docked panel kept open across
  // pages), or open for a link ending in #copilot.
  if (isOpen()) { apply(true, false); refreshStatus(); loadModels(); }
  else if (location.hash === '#copilot') setOpen(true);
  else apply(false, false);
})();
