/* Top-level i18n — mirrors the theme system.
   Default = en (keeps current English UI, no flash); zh is opt-in.
   New UI uses t('key') / [data-i18n="key"]; legacy strings migrate
   incrementally. A no-flash inline <head> script applies the saved lang
   before paint (see templates). Language state = localStorage['tw-lang'];
   a toggle button is injected into header.topbar next to the theme one. */
(function () {
  var KEY = 'tw-lang';
  var root = document.documentElement;

  // Dictionary. en is the source of truth (matches existing markup);
  // zh is the translation. Add keys here as strings migrate.
  var DICT = {
    en: {
      'nav.today': 'Today', 'nav.dashboard': 'Scan', 'nav.portfolio': 'Portfolio', 'nav.validate': 'Validate',
      'nav.chart': 'Chart', 'nav.setups': 'Setups', 'nav.tips': 'Tips', 'nav.stocks': 'Stocks', 'nav.tw': 'TW', 'nav.calendar': 'Calendar', 'nav.onchain': 'On-chain',
      'nav.g.trade': 'Trade', 'nav.g.journal': 'Journal', 'nav.g.research': 'Research', 'nav.g.system': 'System', 'nav.ops': 'Ops',
      'setups.total': 'Total', 'setups.open': 'Open', 'setups.win': 'Win ✓', 'setups.loss': 'Loss ✗',
      'setups.expired': 'Expired', 'setups.skip': 'Skip', 'setups.hitrate': 'Hit-rate',
      'setups.refill': '↻ Backfill', 'setups.slice': 'Engine × Regime hit-rate',
      'setups.r_neutral': 'neutral chop', 'setups.r_trend': 'trend (HH-HL / LH-LL)',
      'lang.toggle': '繁', 'lang.title': 'Switch language / 切換語言'
    },
    zh: {
      'nav.today': '今日', 'nav.dashboard': '掃描', 'nav.portfolio': '投資組合', 'nav.validate': '驗證',
      'nav.chart': '圖表', 'nav.setups': 'Setups', 'nav.tips': '小卡', 'nav.stocks': '個股', 'nav.tw': '台股', 'nav.calendar': '行事曆', 'nav.onchain': '鏈上',
      'nav.g.trade': '交易', 'nav.g.journal': '紀錄', 'nav.g.research': '研究', 'nav.g.system': '維運', 'nav.ops': '維運',
      'setups.total': 'Total', 'setups.open': 'Open', 'setups.win': '勝 ✓', 'setups.loss': '負 ✗',
      'setups.expired': 'Expired', 'setups.skip': 'Skip', 'setups.hitrate': '命中率',
      'setups.refill': '↻ 回填 outcome', 'setups.slice': 'Engine × Regime 命中率切片',
      'setups.r_neutral': 'neutral 拉鋸', 'setups.r_trend': '趨勢 (HH-HL / LH-LL)',
      'lang.toggle': 'EN', 'lang.title': '切換語言 / Switch language'
    }
  };

  function cur() { return root.getAttribute('data-lang') === 'zh' ? 'zh' : 'en'; }
  // Global lookup: current lang → en fallback → raw key.
  window.t = function (k) { var L = cur(); return (DICT[L] && DICT[L][k]) || DICT.en[k] || k; };

  // Fill every [data-i18n] (textContent) and [data-i18n-html] (innerHTML).
  function apply() {
    document.querySelectorAll('[data-i18n]').forEach(function (el) {
      var v = window.t(el.getAttribute('data-i18n')); if (v != null) el.textContent = v;
    });
    document.querySelectorAll('[data-i18n-html]').forEach(function (el) {
      var v = window.t(el.getAttribute('data-i18n-html')); if (v != null) el.innerHTML = v;
    });
    var b = document.getElementById('lang-toggle');
    if (b) { b.textContent = window.t('lang.toggle'); b.title = window.t('lang.title'); }
  }
  window.i18nApply = apply; // let dynamic renderers re-apply after inserting DOM

  function setLang(L) {
    root.setAttribute('data-lang', L);
    try { localStorage.setItem(KEY, L); } catch (e) {}
    apply();
  }

  document.addEventListener('DOMContentLoaded', function () {
    apply();
    // Mark the active nav link by current path (topnav is now a shared
    // partial with no per-page active class).
    var path = location.pathname;
    document.querySelectorAll('.topnav a').forEach(function (a) {
      var h = a.getAttribute('href');
      var on = h === '/' ? (path === '/') : (path === h || path.indexOf(h + '/') === 0);
      if (on) a.classList.add('active');
    });
    var btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'lang-toggle';
    btn.id = 'lang-toggle';
    btn.textContent = window.t('lang.toggle');
    btn.title = window.t('lang.title');
    btn.addEventListener('click', function () { setLang(cur() === 'zh' ? 'en' : 'zh'); });
    var bar = document.querySelector('#sidebar-foot') || document.querySelector('header.topbar');
    if (bar) bar.appendChild(btn);
    else { btn.classList.add('floating'); document.body.appendChild(btn); }
  });
})();
