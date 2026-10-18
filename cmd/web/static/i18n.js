/* LEGACY client-side i18n, kept only for page bodies not yet moved to the
   server-side dictionaries (cmd/web/i18n.go). The language is no longer
   chosen here: the server renders <html data-lang> from the `lang` cookie
   and the shell's EN/中 link switches it. This file only fills the legacy
   [data-i18n] strings of a page in that language, and shrinks to nothing
   as pages migrate. */
(function () {
  var root = document.documentElement;

  // Legacy dictionary. Do not add keys here — new strings go in
  // cmd/web/i18n/{zh,en}.json and render on the server.
  var DICT = {
    en: {
      'setups.total': 'Total', 'setups.open': 'Open', 'setups.win': 'Win ✓', 'setups.loss': 'Loss ✗',
      'setups.expired': 'Expired', 'setups.skip': 'Skip', 'setups.hitrate': 'Hit-rate',
      'setups.refill': '↻ Backfill', 'setups.slice': 'Engine × Regime hit-rate',
      'setups.r_neutral': 'neutral chop', 'setups.r_trend': 'trend (HH-HL / LH-LL)'
    },
    zh: {
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
      'trend-aligned': '順勢'
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
  }
  window.i18nApply = apply; // let dynamic renderers re-apply after inserting DOM

  document.addEventListener('DOMContentLoaded', function () {
    apply();
  });
})();
