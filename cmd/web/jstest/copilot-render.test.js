// Tests for cmd/web/static/copilot-render.js. Run: node --test cmd/web/jstest
// (CI runs it in the check job). A tiny fake DOM stands in for the browser;
// it throws on innerHTML so a future edit cannot slip HTML parsing in.
'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const R = require('../static/copilot-render.js');

class Node_ {
  constructor(tag, text) { this.tag = tag; this.kids = []; this.attrs = {}; this.className = ''; this._text = text || ''; }
  appendChild(n) { this.kids.push(n); return n; }
  setAttribute(k, v) { this.attrs[k] = String(v); }
  set textContent(v) { this.kids = []; this._text = String(v); }
  get textContent() { return this._text + this.kids.map((k) => k.textContent).join(''); }
  set innerHTML(_) { throw new Error('innerHTML used'); }
  find(pred, out = []) { if (pred(this)) out.push(this); this.kids.forEach((k) => k.find && k.find(pred, out)); return out; }
}
const doc = {
  createElement: (t) => new Node_(t),
  createTextNode: (s) => new Node_('#text', s),
  createDocumentFragment: () => new Node_('#frag'),
};
const render = (text, opts) => R.render(text, Object.assign({ doc, lang: 'zh' }, opts));
const byClass = (root, cls) => root.find((n) => (' ' + n.className + ' ').includes(' ' + cls + ' '));

const EX1 = [
  '結論:BTC 1h 偏空,反彈到 58,600–58,900 樞紐區站不回就找空,現價不追。',
  '',
  '- 24h -6.30%,1h 連 3 根收在 EMA20 之下',
  '- 11-21 09:00 跌破前低 59,420,結構 BOS 向下',
  '- 引擎 -42/100,與結構同向',
  '',
  '```levels',
  '支撐 | 56,850 | 4h 前低',
  '壓力 | 59,420 | 前低轉壓 / BOS 點',
  '現價 | 57,919.7 |',
  '樞紐區 | 58,600–58,900 | 反彈賣區',
  '```',
  '',
  '進場: 58,750',
  '止損: 59,500',
  '止盈: 56,900',
  'RR: 2.5R',
  '',
  '注意:22:45 美國 PMI 前後波動放大,未成交就撤單。',
].join('\n');

const EX2 = [
  '**結論:** #93 ETH 空單停損出場,-1.0R;方向與 4h 結構一致,問題在進場時點。',
  '',
  '進場: 1,712.4',
  '止損: 1,738.0',
  '出場: 1,738.0',
  '結果: -1.0R',
  '持倉: 3h 20m',
  '',
  '## 執行面',
  '- 進場時 1h 還在反彈,價格未到 1,725–1,735 樞紐區',
  '',
  '| 項目 | 進場時 | 出場時 |',
  '|---|---|---|',
  '| ETH 24h | +3.1% | +4.6% |',
  '| 引擎分數 | -18 | +6 |',
].join('\n');

test('example 1: verdict, list, levels ladder, stat grid, callout', () => {
  const b = R.parse(EX1);
  assert.deepEqual(b.map((x) => x.t), ['verdict', 'list', 'levels', 'stats', 'callout']);
  assert.equal(b[0].tone, 'short');
  // The ladder is sorted high → low whatever order the model wrote.
  assert.deepEqual(b[2].rows.map((r) => r.type), ['res', 'zone', 'now', 'sup']);
  assert.deepEqual(b[3].items.map((i) => i.tone), ['', 'stop', 'tp', 'good']);

  const f = render(EX1);
  assert.equal(byClass(f, 'cp-verdict')[0].className, 'cp-verdict short');
  assert.equal(byClass(f, 'cp-sym')[0].attrs.href, '/chart?symbol=BTC');
  const neg = byClass(f, 'neg').map((n) => n.textContent);
  assert.ok(neg.includes('-6.30%'), neg.join(','));
  // A date-time is not a price, and -42/100 is not coloured.
  const nums = byClass(f, 'cp-num').map((n) => n.textContent);
  assert.ok(!nums.some((n) => n.includes('09') || n.includes('11-21')), nums.join(','));
  assert.ok(!nums.includes('-42'));
  assert.ok(nums.includes('59,420'));
});

test('example 2: bold verdict key, signed R decides the tone, table cells', () => {
  const b = R.parse(EX2);
  assert.deepEqual(b.map((x) => x.t), ['verdict', 'stats', 'h', 'list', 'table']);
  assert.equal(b[0].tone, 'loss');
  assert.equal(b[0].text.startsWith('#93'), true, b[0].text);
  const f = render(EX2);
  assert.equal(byClass(f, 'cp-verdict')[0].className, 'cp-verdict short');
  assert.equal(byClass(f, 'cp-ref')[0].textContent, '#93');
  const tds = f.find((n) => n.tag === 'td');
  assert.equal(tds[1].className, 'num');
  assert.equal(byClass(tds[1], 'pos')[0].textContent, '+3.1%');
  assert.equal(byClass(tds[4], 'cp-num')[0]?.className ?? '', '', 'a signed integer is not coloured');
  // "3h 20m" is a duration, not a price.
  const held = byClass(f, 'cp-stat').find((n) => n.textContent.startsWith('持倉'));
  assert.equal(byClass(held, 'cp-num').length, 0);
});

test('a reply that ignores every convention is still readable', () => {
  const b = R.parse('BTC 現在 57,900,今天 -6.3%。\n先觀望。');
  assert.deepEqual(b.map((x) => x.t), ['p']);
  const f = render('BTC 現在 57,900,今天 -6.3%。\n先觀望。');
  assert.equal(f.find((n) => n.tag === 'br').length, 1);
  assert.equal(byClass(f, 'cp-dir')[0].className, 'cp-dir flat');
});

test('model HTML and links stay text', () => {
  const f = render('<img src=x onerror=alert(1)> see [here](javascript:alert(1)) and <script>x</script>');
  assert.ok(f.textContent.includes('<img src=x onerror=alert(1)>'));
  assert.ok(f.textContent.includes('see here and'));
  const hrefs = f.find((n) => n.attrs && n.attrs.href).map((n) => n.attrs.href);
  assert.deepEqual(hrefs, []);
});

test('only whitelisted symbols become links', () => {
  const f = render('DOGE and NVDA and BTC-USDT', { symbols: ['BTC', 'NVDA'], tf: '4h' });
  assert.deepEqual(byClass(f, 'cp-sym').map((n) => n.attrs.href), ['/chart?symbol=NVDA&tf=4h', '/chart?symbol=BTC&tf=4h']);
});

test('lowercase long/short light up only in cards', () => {
  assert.equal(byClass(render('It took long to fill.'), 'cp-dir').length, 0);
  assert.equal(byClass(render('Entry: 100\nStop: 90\nSide: x'), 'cp-stat').length, 2);
  const t = render('| side | r |\n|---|---|\n| long | +1R |');
  assert.equal(byClass(t, 'cp-pill')[0].className, 'cp-pill long');
});

test('verdict only on the first line; unclosed fence runs to the end', () => {
  assert.equal(R.parse('hello\n結論:x')[0].t, 'p');
  const b = R.parse('```\ncode line\nmore');
  assert.deepEqual(b, [{ t: 'pre', text: 'code line\nmore' }]);
});

test('single KV line stays a paragraph; RR as 1:3 is good', () => {
  assert.equal(R.parse('進場: 100')[0].t, 'p');
  const s = R.parse('止損: 90\nRR: 1:3')[0];
  assert.deepEqual(s.items.map((i) => i.tone), ['stop', 'good']);
});

test('the renderer never touches innerHTML', () => {
  const src = fs.readFileSync(path.join(__dirname, '../static/copilot-render.js'), 'utf8');
  const code = src.replace(/\/\*[\s\S]*?\*\//g, '').replace(/\/\/.*$/gm, '');
  assert.ok(!/innerHTML|insertAdjacentHTML|outerHTML|document\.write/.test(code));
});

// The real production reply that started this (flash-lite, 2026-11-23).
test('a bilingual one-shot reply shows only the zh block, bold lines are headings', () => {
  const real = '[EN]\n## BTC 1h Setup\n\n**1. Structure:**\n*   **Swing:** up\n[/EN]\n\n[ZH-TW]\n## BTC 1h 設置分析\n\n**1. 結構與盤勢判讀：**\n*   **擺盪序列：** 上升\n\n**4. 決策：**\n*   **等待。** 觀望\n[/ZH-TW]';
  const b = R.parse(real, 'zh');
  assert.deepEqual(b.map((x) => x.t), ['h', 'h', 'list', 'h', 'list']);
  assert.equal(b[1].text, '1. 結構與盤勢判讀');
  assert.equal(R.parse(real, 'en')[0].text, 'BTC 1h Setup');
  const f = render(real);
  assert.ok(!f.textContent.includes('[EN]') && !f.textContent.includes('Swing'));
  // Headings are plain text: no symbol chip or timeframe tag inside.
  const h = f.find((n) => n.tag === 'h4')[0];
  assert.equal(h.kids.length, 0);
  assert.equal(h.textContent, 'BTC 1h 設置分析');
});

test('KV lines with the colon inside the bold still make a stat grid', () => {
  const b = R.parse('**進場:** 58,750\n**止損：** 59,500\n- **RR:** 2.5R');
  assert.equal(b[0].t, 'stats');
  assert.deepEqual(b[0].items.map((i) => [i.k, i.v]), [['進場', '58,750'], ['止損', '59,500'], ['RR', '2.5R']]);
});

test('a verdict label alone on its line takes the next line', () => {
  const b = R.parse('**結論：**\n\n- 偏多,等回踩 63,000 再進\n\n其他');
  assert.equal(b[0].t, 'verdict');
  assert.equal(b[0].tone, 'long');
  assert.equal(b[0].text, '偏多,等回踩 63,000 再進');
  assert.equal(b[1].t, 'p');
});
