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
      // engine-card scoring factors (validator.Factor.Name → 中文); EN mode falls back to the raw key
      'direction aligned with engine': '方向與引擎一致',
      'direction opposes engine': '方向與引擎相反',
      'engine flat — neutral on direction': '引擎中性(無方向)',
      'engine has tradeable score': '引擎達可交易分數',
      'entry at recent sweep level': '進場在近期掃單位',
      'entry at sweep but wrong side': '進場在掃單位但方向錯',
      'entry at fib 0.618': '進場在 fib 0.618 回撤',
      'entry at lower Bollinger': '進場在布林下軌',
      'entry at upper Bollinger': '進場在布林上軌',
      'entry at POC': '進場在 POC(均衡區)',
      'entry below non-POC HVN (chip support)': '進場在 HVN 下方(籌碼支撐)',
      'entry above non-POC HVN (chip resistance)': '進場在 HVN 上方(籌碼壓力)',
      'entry near HVN but wrong side for direction': '進場貼 HVN 但方向錯',
      'POC reachable as target': 'POC 可達為目標',
      'POC far in trade direction': 'POC 太遠(順向)',
      'fading away from chip zone': '逆離籌碼區',
      'entry at VAL — mean-rev long': '進場在 VAL(均值回歸多)',
      'entry at VAH — mean-rev short': '進場在 VAH(均值回歸空)',
      'entry at VAL fighting value-area floor': '進場在 VAL 逆價值下緣',
      'entry at VAH fighting value-area ceiling': '進場在 VAH 逆價值上緣',
      'entry above VAH — extension short': '進場在 VAH 上(延伸空)',
      'entry above VAH — chasing trend': '進場在 VAH 上(追勢)',
      'entry below VAL — extension long': '進場在 VAL 下(延伸多)',
      'entry below VAL — chasing trend': '進場在 VAL 下(追勢)',
      'entry inside VA — chop zone': '進場在 VA 內(震盪區)',
      'chasing market significantly': '大幅追價',
      'chasing market': '追價',
      'fee math healthy': '手續費健康',
      'fee math acceptable': '手續費可接受',
      'fee math thin': '手續費偏高(edge 薄)',
      'fee math fatal': '手續費吃掉 edge',
      'bounce short in falling POC regime': '下跌 POC 中反彈放空',
      'trend-aligned': '順勢',
      'lang.toggle': 'EN', 'lang.title': '切換語言 / Switch language'
    }
  };

  function cur() { return root.getAttribute('data-lang') === 'zh' ? 'zh' : 'en'; }
  // Keep the real <html lang> in sync with the active UI language (a11y/SEO).
  // Templates hardcode lang="en" server-side; this makes it authoritative &
  // dynamic. Runs at top level (defer → after parse) so it tracks the value
  // the no-flash inline <head> script already set.
  function syncHtmlLang() { root.setAttribute('lang', cur() === 'zh' ? 'zh-Hant' : 'en'); }
  syncHtmlLang();
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
    syncHtmlLang();
    try { localStorage.setItem(KEY, L); } catch (e) {}
    apply();
  }

  document.addEventListener('DOMContentLoaded', function () {
    apply();
    // Mark the active nav link by current path (topnav is now a shared
    // partial with no per-page active class).
    // Activate only the MOST-SPECIFIC matching link: a bare prefix test lights up
    // both a parent tab and its child (e.g. /ops and /ops/autotrade), so pick the
    // longest matching href and mark just that one.
    var path = location.pathname;
    var best = null, bestLen = -1;
    document.querySelectorAll('.topnav a').forEach(function (a) {
      var h = a.getAttribute('href');
      var on = h === '/' ? (path === '/') : (path === h || path.indexOf(h + '/') === 0);
      if (on && h.length > bestLen) { best = a; bestLen = h.length; }
    });
    if (best) best.classList.add('active');
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
