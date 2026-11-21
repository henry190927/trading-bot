/* Copilot chat drawer — docs/ai_copilot_design.md, server side in
   cmd/web/copilot.go. The conversation lives in this tab (sessionStorage);
   each question sends the recent turns and the page the user is on.
   Everything the model writes is inserted as text, never as HTML. */
(function () {
  'use strict';
  var I = window.I18N || {};
  var box = document.getElementById('copilot');
  var openBtn = document.getElementById('copilot-open');
  if (!box || !openBtn) return;
  var log = document.getElementById('copilot-log');
  var form = document.getElementById('copilot-form');
  var input = document.getElementById('copilot-input');
  var send = document.getElementById('copilot-send');
  var quotaEl = document.getElementById('copilot-quota');
  var modelSel = document.getElementById('copilot-model');
  var modelsLoaded = false;
  var KEY = 'copilot.msgs';
  var msgs = load();
  var busy = false;

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
  function bubble(role, text, extra) {
    var d = document.createElement('div');
    d.className = 'copilot-msg ' + role;
    var t = document.createElement('div');
    t.className = 'copilot-text';
    t.textContent = text;
    d.appendChild(t);
    if (extra) {
      var e = document.createElement('div');
      e.className = 'copilot-meta';
      e.textContent = extra;
      d.appendChild(e);
    }
    log.appendChild(d);
    log.scrollTop = log.scrollHeight;
    return d;
  }
  function renderAll() {
    Array.prototype.slice.call(log.querySelectorAll('.copilot-msg')).forEach(function (n) { n.remove(); });
    msgs.forEach(function (m) { bubble(m.role, m.text, m.meta); });
  }
  function showQuota(q) {
    if (!quotaEl || !Array.isArray(q) || !q.length) return;
    var live = q.filter(function (m) { return !m.blocked; })[0];
    if (!live) { quotaEl.textContent = fmt(I['copilot.quotaOut'], q[0].model); quotaEl.className = 'copilot-quota out'; return; }
    quotaEl.className = 'copilot-quota';
    quotaEl.textContent = live.remaining ? fmt(I['copilot.quotaLeft'], live.model, live.remaining) : fmt(I['copilot.quota'], live.model, live.used || 0);
  }
  function refreshStatus() {
    fetch('/api/copilot/status', { credentials: 'same-origin' }).then(function (r) { return r.json(); }).then(function (j) {
      if (j && j.enabled) showQuota(j.quota);
      else if (quotaEl) { quotaEl.textContent = I['copilot.err.off'] || ''; quotaEl.className = 'copilot-quota out'; }
    }).catch(function () {});
  }
  // The model selector is the same setting the server persists for every
  // AI call (/api/ai/model); the chosen model is tried first, configured
  // fallbacks after it.
  function loadModels() {
    if (!modelSel || modelsLoaded) return;
    fetch('/api/ai/model', { credentials: 'same-origin' }).then(function (r) { return r.json(); }).then(function (j) {
      var list = (j && j.available) || [];
      if (j && j.current && list.indexOf(j.current) < 0) list.unshift(j.current);
      if (!list.length) return;
      modelSel.textContent = '';
      list.forEach(function (m) {
        var o = document.createElement('option');
        o.value = m; o.textContent = m;
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
      bubble('error', I['copilot.modelErr'] || 'model switch failed');
    }).then(function () { modelSel.disabled = false; });
  });
  function setOpen(open) {
    box.hidden = !open;
    openBtn.setAttribute('aria-expanded', open ? 'true' : 'false');
    if (open) { refreshStatus(); loadModels(); setTimeout(function () { input.focus(); }, 0); }
  }

  function ask(text) {
    if (busy) return;
    busy = true; send.disabled = true;
    msgs.push({ role: 'user', text: text });
    save();
    bubble('user', text);
    var pending = bubble('assistant pending', I['copilot.thinking'] || '…');
    var body = {
      messages: msgs.map(function (m) { return { role: m.role, text: m.text }; }),
      page: { path: location.pathname + location.search, title: document.title, context: '' }
    };
    fetch('/api/copilot/chat', {
      method: 'POST', credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body)
    }).then(function (r) {
      return r.json().then(function (j) { return { ok: r.ok, j: j || {} }; });
    }).then(function (res) {
      pending.remove();
      showQuota(res.j.quota);
      if (!res.ok) { bubble('error', res.j.error || I['copilot.err.net']); return; }
      var reply = res.j.reply || '';
      if (res.j.note) reply = reply ? reply + '\n\n— ' + res.j.note : res.j.note;
      var used = (res.j.tools || []).map(function (t) { return t.name + (t.error ? ' ✕' : ''); });
      var meta = used.length ? (I['copilot.used'] || '') + ': ' + used.join(', ') : '';
      msgs.push({ role: 'assistant', text: reply, meta: meta });
      save();
      bubble('assistant', reply, meta);
    }).catch(function () {
      pending.remove();
      bubble('error', I['copilot.err.net'] || 'network error');
    }).then(function () {
      busy = false; send.disabled = false; input.focus();
    });
  }

  openBtn.addEventListener('click', function () { setOpen(box.hidden); });
  document.getElementById('copilot-close').addEventListener('click', function () { setOpen(false); });
  document.getElementById('copilot-clear').addEventListener('click', function () { msgs = []; save(); renderAll(); });
  document.addEventListener('keydown', function (e) { if (e.key === 'Escape' && !box.hidden) setOpen(false); });
  form.addEventListener('submit', function (e) {
    e.preventDefault();
    var text = input.value.trim();
    if (!text) return;
    input.value = '';
    ask(text);
  });
  input.addEventListener('keydown', function (e) {
    // Enter sends; Shift+Enter is a new line; never while an IME (Chinese
    // input) is still composing, where Enter confirms the characters.
    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing && e.keyCode !== 229) {
      e.preventDefault();
      form.requestSubmit ? form.requestSubmit() : form.dispatchEvent(new Event('submit', { cancelable: true }));
    }
  });
  renderAll();
  // A link ending in #copilot opens the drawer (e.g. from another page).
  if (location.hash === '#copilot') setOpen(true);
})();
