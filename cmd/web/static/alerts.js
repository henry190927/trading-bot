// Alert bell — on every page via the topnav partial (cmd/web/alerts.go).
//
// Polls /api/alerts rather than holding an SSE stream open. The rules are
// evaluated every 15s, so a push channel would buy at most 20s of latency,
// and it would cost a connection per tab: over plain HTTP/1.1 a browser
// allows six per host, and /chart and /today already hold one for prices.
// Three open tabs would exhaust them.
//
// The /alerts page renders from the same poll: listen for 'alerts:update'.
(function () {
  var POLL_MS = 20000;
  var SEEN_KEY = 'alert-last-seen';
  var bells = document.querySelectorAll('[data-alert-bell]');
  var panel = document.getElementById('alert-panel');
  var list = document.getElementById('alert-panel-list');
  var baseTitle = document.title;
  var last = null;

  // Strings come from the server's dictionary via the shell (window.I18N),
  // so the bell speaks the page's language.
  var L = window.I18N || {};
  function T(k) { return L[k] || k; }

  function fmtTime(iso) {
    if (!iso) return '';
    return new Date(iso).toLocaleString('sv-SE', {
      timeZone: 'Asia/Taipei', month: '2-digit', day: '2-digit',
      hour: '2-digit', minute: '2-digit', hour12: false
    });
  }
  function fmtDur(ms) {
    var s = Math.max(0, Math.round(ms / 1000));
    if (s < 60) return s + ' ' + T('dur.s');
    if (s < 3600) return Math.round(s / 60) + ' ' + T('dur.m');
    if (s < 86400) return Math.floor(s / 3600) + ' ' + T('dur.h') + ' ' + Math.round((s % 3600) / 60) + ' ' + T('dur.m');
    return Math.floor(s / 86400) + ' ' + T('dur.d') + ' ' + Math.floor((s % 86400) / 3600) + ' ' + T('dur.h');
  }
  function isOpen(a) { return !a.resolved_at; }
  function isUnread(a) { return a.severity !== 'info' && !a.acked_at; }

  // status: "進行中 · 4 分" or "已恢復 · 持續 4 分"
  function statusText(a) {
    if (a.severity === 'info' && a.fired_at === a.resolved_at) return '';
    var start = new Date(a.fired_at).getTime();
    if (isOpen(a)) return T('alerts.open') + ' · ' + fmtDur(Date.now() - start);
    return T('alerts.resolvedFor') + ' ' + fmtDur(new Date(a.resolved_at).getTime() - start);
  }

  function el(tag, cls, text) {
    var e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text != null) e.textContent = text; // textContent: details carry raw error strings
    return e;
  }

  // One alert row, shared by the dropdown and the /alerts page.
  function renderItem(a, opts) {
    var li = el('li', 'alert-item sev-' + a.severity + (isOpen(a) ? ' is-open' : ' is-resolved') + (isUnread(a) ? ' is-unread' : ''));
    var head = el('div', 'alert-item-head');
    head.appendChild(el('span', 'alert-sev', T('alerts.sev.' + a.severity)));
    head.appendChild(el('span', 'alert-title', a.title));
    li.appendChild(head);
    if (a.detail) li.appendChild(el('div', 'alert-detail', a.detail));
    var meta = el('div', 'alert-meta');
    meta.appendChild(el('span', '', fmtTime(a.fired_at)));
    var st = statusText(a);
    if (st) meta.appendChild(el('span', 'alert-status', st));
    if (a.acked_at) meta.appendChild(el('span', 'alert-acked', T('alerts.acked') + ' ' + fmtTime(a.acked_at)));
    if (isUnread(a)) {
      var b = el('button', 'btn-ops alert-ack', T('alerts.ack'));
      b.type = 'button';
      b.addEventListener('click', function (ev) { ev.stopPropagation(); ack(a.id); });
      meta.appendChild(b);
    }
    li.appendChild(meta);
    return li;
  }

  function renderBells(s) {
    var critUnread = s.alerts.some(function (a) { return a.severity === 'critical' && isUnread(a); });
    bells.forEach(function (b) {
      var badge = b.querySelector('.alert-badge');
      b.classList.toggle('has-critical', s.critical_open > 0);
      if (!badge) return;
      badge.hidden = s.unread === 0;
      badge.textContent = s.unread > 99 ? '99+' : String(s.unread);
      badge.classList.toggle('crit', critUnread || s.critical_open > 0);
    });
    document.title = (s.unread ? '(' + s.unread + ') ' : '') + baseTitle;
  }

  function renderPanel(s) {
    if (!list) return;
    list.innerHTML = '';
    var shown = s.alerts.filter(function (a) { return a.severity !== 'info'; }).slice(0, 8);
    if (!shown.length) {
      list.appendChild(el('li', 'alert-empty', T('alerts.empty')));
      return;
    }
    shown.forEach(function (a) { list.appendChild(renderItem(a)); });
  }

  // Toast what arrived since this browser last looked. The high-water mark
  // lives in localStorage so moving between pages does not re-toast; the
  // first visit ever only records it, rather than replaying the history.
  function toastNew(s) {
    var maxID = 0;
    s.alerts.forEach(function (a) { if (a.id > maxID) maxID = a.id; });
    var seen = null;
    try { seen = localStorage.getItem(SEEN_KEY); } catch (e) {}
    if (seen !== null && window.toast) {
      var mark = Number(seen) || 0;
      s.alerts.forEach(function (a) {
        if (a.id > mark && isUnread(a) && isOpen(a)) {
          var crit = a.severity === 'critical';
          window.toast((crit ? '🚨 ' : '⚠ ') + a.title, crit ? 'err' : 'warn', crit ? 12000 : 7000);
        }
      });
    }
    try { localStorage.setItem(SEEN_KEY, String(Math.max(maxID, Number(seen) || 0))); } catch (e) {}
  }

  function apply(s) {
    if (!s || !s.alerts) return;
    var changed = !last || last.version !== s.version;
    last = s;
    renderBells(s);
    if (changed || (panel && !panel.hidden)) renderPanel(s);
    if (changed) toastNew(s);
    document.dispatchEvent(new CustomEvent('alerts:update', { detail: s }));
  }

  function refresh() {
    return fetch('/api/alerts', { cache: 'no-store' })
      .then(function (r) { return r.json(); })
      .then(apply)
      .catch(function () { /* the bell keeps its last state; web-down is daemon-watch's job */ });
  }

  function ack(id) {
    var body = new URLSearchParams({ id: String(id || 0) });
    return fetch('/api/alerts/ack', { method: 'POST', body: body }).then(refresh);
  }
  window.alertsAck = ack;
  window.alertsRefresh = refresh;
  window.alertsRenderItem = renderItem;

  // ── Dropdown ──────────────────────────────────────────────────────
  function place(bell) {
    var r = bell.getBoundingClientRect();
    var w = Math.min(380, window.innerWidth - 16);
    var left = Math.min(Math.max(8, r.left), window.innerWidth - w - 8);
    panel.style.width = w + 'px';
    panel.style.left = left + 'px';
    panel.style.top = (r.bottom + 8) + 'px';
  }
  bells.forEach(function (b) {
    b.addEventListener('click', function (ev) {
      ev.stopPropagation();
      if (!panel) return;
      if (!panel.hidden) { panel.hidden = true; return; }
      place(b);
      panel.hidden = false;
      if (last) renderPanel(last);
      refresh();
    });
  });
  document.addEventListener('click', function (ev) {
    if (panel && !panel.hidden && !panel.contains(ev.target)) panel.hidden = true;
  });
  document.addEventListener('keydown', function (ev) {
    if (ev.key === 'Escape' && panel) panel.hidden = true;
  });
  var ackAll = document.getElementById('alert-ackall');
  if (ackAll) ackAll.addEventListener('click', function (ev) { ev.stopPropagation(); ack(0); });

  // Poll only while visible, and catch up the moment the tab comes back —
  // a phone that was locked through an incident should show it on unlock.
  refresh();
  setInterval(function () { if (!document.hidden) refresh(); }, POLL_MS);
  document.addEventListener('visibilitychange', function () { if (!document.hidden) refresh(); });
})();
