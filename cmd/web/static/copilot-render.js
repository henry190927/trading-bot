/* Copilot reply renderer — model text in, DOM nodes out.

   The model's text is untrusted. Nothing here uses innerHTML or
   insertAdjacentHTML: parse() turns the text into a small tree of plain
   objects, build() turns that tree into elements with createElement and
   textContent. The only href this file ever writes is /chart?symbol=X for a
   symbol on the server's whitelist; model links render as their text.

   Supported on purpose (docs: the copilot UI spec, copilot/prompt.go):
   - a small markdown subset: headings (all small), lists, pipe tables,
     fenced code, quotes, rules, **bold**, `code`
   - cards: a first line "結論:" (verdict), a ```levels block (price ladder),
     two or more consecutive "進場: / 止損: / …" lines (stat grid), and a
     "注意:" callout
   - inline colour: direction words, signed % and R, prices, symbols, #ids,
     timeframes

   Anything unrecognised is a paragraph. Callers wrap render() in try/catch
   and fall back to plain text, so a renderer bug never loses a reply.
   Works in the browser (window.CopilotRender) and in node for the tests
   (cmd/web/jstest). */
(function (root) {
  'use strict';

  var DEFAULT_SYMBOLS = ['BTC', 'ETH', 'XAU', 'XAG', 'SOL', 'SPCX', 'MSTR', 'APP'];

  var DIR = {
    '做多': 'long', '多單': 'long', '偏多': 'long', '看多': 'long', '多頭': 'long',
    '做空': 'short', '空單': 'short', '偏空': 'short', '看空': 'short', '空頭': 'short',
    '觀望': 'flat', '不做': 'flat',
    'LONG': 'long', 'Long': 'long', 'long': 'long', 'Bullish': 'long', 'bullish': 'long',
    'SHORT': 'short', 'Short': 'short', 'short': 'short', 'Bearish': 'short', 'bearish': 'short'
  };
  // Lowercase long/short only count inside cards and tables: in prose
  // ("how long", "short of the level") they are ordinary words.
  var CARD_ONLY = { long: 1, short: 1 };

  var LABELS = {
    zh: { long: '偏多', short: '偏空', flat: '觀望', win: '獲利', loss: '虧損' },
    en: { long: 'Bullish', short: 'Bearish', flat: 'Wait', win: 'Win', loss: 'Loss' }
  };

  var VERDICT_KEYS = { '結論': 1, '總結': 1, 'verdict': 1, 'tl;dr': 1 };
  var CALLOUT_RE = /^\s*(?:>\s*)?(?:\*\*)?(注意|風險|提醒|Note|Risk|Warning|Caution)(?:\*\*)?\s*[:：]\s*(.*)$/;
  var KV_LABELS = '進場|入場|止損|停損|止盈|目標|TP\\d?|SL|出場|RR|R:R|風報比|結果|倉位|槓桿|持倉|Entry|Stop|Target|Exit|Result|Size|Leverage|Held';
  var KV_RE = new RegExp('^\\s*(?:[-*•‧]\\s+)?(?:\\*\\*)?(' + KV_LABELS + ')\\s*(?:\\*\\*)?\\s*[:：]\\s*(?:\\*\\*\\s*)?(.+?)\\s*$');
  var STOP_K = /^(止損|停損|SL|Stop)$/, TP_K = /^(止盈|目標|TP\d?|Target)$/, RR_K = /^(RR|R:R|風報比)$/;
  var FENCE_RE = /^\s*```\s*([\w-]*)\s*$/;
  var HR_RE = /^\s*(?:-{3,}|\*{3,}|_{3,}|—{2,})\s*$/;
  var H_RE = /^\s*(#{1,6})\s+(.+?)\s*#*\s*$/;
  // A line that is nothing but bold ("**1. 結構判讀：**") is how models write
  // a section heading when they skip "##".
  var BOLD_H_RE = /^\s*\*\*([^*\n]{1,60}?)\s*[:：]?\s*\*\*\s*[:：]?\s*$/;
  var LANG_BLOCK_RE = /\[(EN|ZH-TW)\]([\s\S]*?)(?:\[\/\1\]|(?=\[(?:EN|ZH-TW)\])|$)/g;
  var LIST_RE = /^(\s*)([-*•‧]|\d{1,2}[.)])\s+(.*)$/;
  var TSEP_RE = /^\s*\|?\s*:?-{2,}:?\s*(?:\|\s*:?-{2,}:?\s*)*\|?\s*$/;
  var LINK_RE = /\[([^\]\n]+)\]\((?:[^()\n]|\([^()\n]*\))*\)/g;

  // Inline patterns use lookbehind, which an old browser cannot compile.
  // They are built on first use, inside the caller's try/catch, so such a
  // browser falls back to plain text instead of failing to load this file.
  var INLINE = null;
  function inlineRe() {
    if (INLINE) return INLINE;
    var PRICE = '(?:\\d{1,3}(?:,\\d{3})+(?:\\.\\d+)?|\\d{4,}(?:\\.\\d+)?|\\d+\\.\\d{2,})';
    var parts = [
      '(?<ref>#\\d{1,5}(?!\\d))',
      // Dates and times stay plain text, so "11-21 09:00" is not a price.
      '(?<date>(?<![\\w.,])(?:\\d{4}-\\d{1,2}-\\d{1,2}|\\d{1,2}[-/]\\d{1,2}(?![\\d,.%])(?:\\s+\\d{1,2}:\\d{2})?|\\d{1,2}:\\d{2}))',
      '(?<spct>(?<![\\w.])[+\\-−]\\d+(?:\\.\\d+)?%)',
      '(?<sr>(?<![\\w.])[+\\-−]\\d+(?:\\.\\d+)?\\s?R(?![\\w:]))',
      '(?<upct>(?<![\\w.,])\\d+(?:\\.\\d+)?%)',
      '(?<ur>(?<![\\w.,])\\d+(?:\\.\\d+)?R(?![\\w:]))',
      '(?<price>(?<![\\w.,#])' + PRICE + '(?:\\s?[–~\\-]\\s?' + PRICE + ')?(?![\\d%]|\\s?(?:年|月|日|筆|次|根|天|小時|[hmR]\\b)))',
      '(?<sym>(?<![A-Za-z])(?:' + '%SYMS%' + ')(?:-USDT)?(?![A-Za-z]))',
      '(?<tf>(?<![\\w])(?:5m|15m|30m|1h|2h|4h|1d)(?![\\w]))',
      '(?<dir>' + Object.keys(DIR).sort(function (a, b) { return b.length - a.length; }).map(function (w) {
        return /^[A-Za-z]/.test(w) ? '(?<![A-Za-z])' + w + '(?![A-Za-z])' : w;
      }).join('|') + ')'
    ];
    INLINE = parts.join('|');
    return INLINE;
  }
  var reCache = {};
  function inlineRegex(symbols) {
    var key = symbols.join(',');
    if (!reCache[key]) {
      var syms = symbols.filter(function (s) { return /^[A-Z0-9]{2,10}$/.test(s); }).join('|') || 'BTC';
      reCache[key] = new RegExp(inlineRe().replace('%SYMS%', syms), 'g');
    }
    reCache[key].lastIndex = 0;
    return reCache[key];
  }

  function sign(s) {
    var c = s.replace(/\s/g, '').charAt(0);
    return c === '+' ? 'pos' : (c === '-' || c === '−') ? 'neg' : '';
  }
  function firstNumber(s) {
    var m = String(s).replace(/,/g, '').match(/\d+(?:\.\d+)?/);
    return m ? parseFloat(m[0]) : NaN;
  }

  // ---- inline -----------------------------------------------------------

  // inline(text, opts, card) → tokens:
  //   {t:'text',s} {t:'code',s} {t:'strong',kids} {t:'num',s,sign}
  //   {t:'sym',s,sym} {t:'ref',s} {t:'tf',s} {t:'dir',s,tone}
  function inline(text, opts, card) {
    text = String(text || '').replace(LINK_RE, '$1');
    var out = [];
    // `code` first: nothing inside it is enriched.
    var parts = text.split(/(`[^`\n]+`)/);
    parts.forEach(function (p) {
      if (p.length > 2 && p.charAt(0) === '`' && p.charAt(p.length - 1) === '`') { out.push({ t: 'code', s: p.slice(1, -1) }); return; }
      var bits = p.split(/(\*\*[^*\n]+\*\*)/);
      bits.forEach(function (b) {
        if (b.length > 4 && b.slice(0, 2) === '**' && b.slice(-2) === '**') {
          out.push({ t: 'strong', kids: enrich(b.slice(2, -2), opts, card) });
        } else if (b) {
          Array.prototype.push.apply(out, enrich(b, opts, card));
        }
      });
    });
    return out;
  }

  function enrich(text, opts, card) {
    var re = inlineRegex(opts.symbols || DEFAULT_SYMBOLS);
    var out = [], last = 0, m;
    function text_(s) { if (!s) return; var p = out[out.length - 1]; if (p && p.t === 'text') p.s += s; else out.push({ t: 'text', s: s }); }
    while ((m = re.exec(text)) !== null) {
      if (m[0] === '') { re.lastIndex++; continue; }
      var g = m.groups, tok = null;
      if (g.ref) tok = { t: 'ref', s: g.ref };
      else if (g.date) tok = null;
      else if (g.spct || g.sr) tok = { t: 'num', s: m[0], sign: sign(m[0]) };
      else if (g.upct || g.ur || g.price) tok = { t: 'num', s: m[0], sign: '' };
      else if (g.sym) tok = { t: 'sym', s: g.sym, sym: g.sym.replace(/-USDT$/, '') };
      else if (g.tf) tok = { t: 'tf', s: g.tf };
      else if (g.dir) tok = (CARD_ONLY[g.dir] && !card) ? null : { t: 'dir', s: g.dir, tone: DIR[g.dir] };
      text_(text.slice(last, m.index));
      if (tok) out.push(tok); else text_(m[0]);
      last = m.index + m[0].length;
    }
    text_(text.slice(last));
    return out;
  }

  // ---- blocks -----------------------------------------------------------

  function verdictOf(line) {
    var m = line.match(/^\s*(.{1,24}?)\s*[:：]\s*(.*)$/);
    if (!m) return null;
    var rawKey = m[1], key = rawKey.replace(/\*/g, '').trim();
    if (!VERDICT_KEYS[key.toLowerCase()]) return null;
    var rest = m[2].trim();
    // "**結論:** text" leaves a stray ** at the start of the rest.
    if ((rawKey.match(/\*\*/g) || []).length % 2 === 1 && rest.slice(0, 2) === '**') rest = rest.slice(2).trim();
    if ((rest.match(/\*\*/g) || []).length === 1 && rest.slice(-2) === '**') rest = rest.slice(0, -2).trim();
    return { label: key, text: rest };
  }

  // The verdict's tone: a signed R is a trade result (win/loss); otherwise
  // the first direction word; otherwise neutral.
  function verdictTone(text) {
    var r = text.match(/(?:^|[^\w.])([+\-−])\d+(?:\.\d+)?\s?R(?![\w:])/);
    if (r) return r[1] === '+' ? 'win' : 'loss';
    var keys = Object.keys(DIR).filter(function (k) { return !CARD_ONLY[k]; })
      .sort(function (a, b) { return b.length - a.length; });
    var best = -1, tone = '';
    keys.forEach(function (k) {
      var i = text.indexOf(k);
      if (i >= 0 && (best < 0 || i < best)) { best = i; tone = DIR[k]; }
    });
    return tone;
  }

  function levelType(s) {
    s = s.trim().toLowerCase();
    if (/^(壓力|阻力|resistance|res)/.test(s)) return 'res';
    if (/^(支撐|support|sup)/.test(s)) return 'sup';
    if (/^(樞紐區|樞紐|zone|pivot)/.test(s)) return 'zone';
    if (/^(現價|price|now|last|mark)/.test(s)) return 'now';
    return '';
  }

  function parseLevels(lines) {
    var rows = lines.filter(function (l) { return l.trim(); }).map(function (l, i) {
      var c = l.split(/[|｜]/).map(function (x) { return x.trim(); });
      if (c.length < 2) return { type: '', label: '', px: '', note: l.trim(), key: NaN, i: i };
      return { type: levelType(c[0]), label: c[0], px: c[1], note: c.slice(2).join(' ').trim(), key: firstNumber(c[1]), i: i };
    });
    rows.sort(function (a, b) {
      var an = isNaN(a.key), bn = isNaN(b.key);
      if (an !== bn) return an ? 1 : -1;
      if (!an && a.key !== b.key) return b.key - a.key;
      return a.i - b.i;
    });
    return rows;
  }

  function statOf(k, v) {
    var tone = '';
    if (STOP_K.test(k)) tone = 'stop';
    else if (TP_K.test(k)) tone = 'tp';
    else if (RR_K.test(k)) {
      var n = firstNumber(v);
      var ratio = v.match(/^\s*1\s*[:：]\s*(\d+(?:\.\d+)?)/);
      if (ratio) n = parseFloat(ratio[1]);
      tone = isNaN(n) ? '' : n >= 2 ? 'good' : n < 1 ? 'bad' : '';
    }
    return { k: k, v: v, tone: tone, wide: v.length > 18 };
  }

  function cells(line) {
    var s = line.trim();
    if (s.charAt(0) === '|') s = s.slice(1);
    if (s.charAt(s.length - 1) === '|') s = s.slice(0, -1);
    return s.split('|').map(function (x) { return x.trim(); });
  }

  // parse(text) → [{t:'verdict'|'levels'|'pre'|'stats'|'h'|'hr'|'quote'|
  //                    'callout'|'list'|'table'|'p', …}]
  // pickLanguage keeps one block of a reply written as [EN]…[/EN]
  // [ZH-TW]…[/ZH-TW] (the old one-shot format): lang's block, else the other.
  function pickLanguage(text, lang) {
    if (!/\[(?:EN|ZH-TW)\]/.test(text)) return text;
    var blocks = {}, m;
    LANG_BLOCK_RE.lastIndex = 0;
    while ((m = LANG_BLOCK_RE.exec(text)) !== null) {
      if (m[0] === '') { LANG_BLOCK_RE.lastIndex++; continue; }
      if (!blocks[m[1]]) blocks[m[1]] = m[2].trim();
    }
    var want = lang === 'en' ? 'EN' : 'ZH-TW', other = want === 'EN' ? 'ZH-TW' : 'EN';
    return blocks[want] || blocks[other] || text.replace(/\[\/?(?:EN|ZH-TW)\]/g, '').trim();
  }

  function parse(text, lang) {
    text = pickLanguage(String(text || ''), lang);
    var lines = text.replace(/\r\n?/g, '\n').split('\n');
    var blocks = [], para = [], i = 0;
    function flush() { if (para.length) { blocks.push({ t: 'p', lines: para }); para = []; } }
    function kvAt(j) { return j < lines.length && KV_RE.test(lines[j]); }

    while (i < lines.length) {
      var line = lines[i], m;
      if (!line.trim()) { flush(); i++; continue; }

      if (!blocks.length && !para.length) {
        var v = verdictOf(line);
        if (v) {
          i++;
          // "**結論:**" alone on its line: the sentence is the next line.
          if (!v.text) {
            while (i < lines.length && !lines[i].trim()) i++;
            if (i < lines.length) { v.text = lines[i].replace(LIST_RE, '$3').trim(); i++; }
          }
          blocks.push({ t: 'verdict', label: v.label, text: v.text, tone: verdictTone(v.text) });
          continue;
        }
      }
      if ((m = line.match(FENCE_RE))) {
        flush();
        var body = [];
        for (i++; i < lines.length && !FENCE_RE.test(lines[i]); i++) body.push(lines[i]);
        i++; // the closing fence (an unclosed fence runs to the end)
        if (m[1].toLowerCase() === 'levels') blocks.push({ t: 'levels', rows: parseLevels(body) });
        else blocks.push({ t: 'pre', text: body.join('\n') });
        continue;
      }
      if (kvAt(i) && kvAt(i + 1)) {
        flush();
        var items = [];
        for (; kvAt(i); i++) { var kv = lines[i].match(KV_RE); items.push(statOf(kv[1], kv[2])); }
        blocks.push({ t: 'stats', items: items });
        continue;
      }
      if (HR_RE.test(line)) { flush(); blocks.push({ t: 'hr' }); i++; continue; }
      if ((m = line.match(H_RE))) { flush(); blocks.push({ t: 'h', level: m[1].length, text: m[2].replace(/\*\*/g, '') }); i++; continue; }
      if ((m = line.match(BOLD_H_RE)) && !LIST_RE.test(line)) { flush(); blocks.push({ t: 'h', level: 2, text: m[1] }); i++; continue; }
      if ((m = line.match(CALLOUT_RE))) { flush(); blocks.push({ t: 'callout', label: m[1], text: m[2] }); i++; continue; }
      if (line.indexOf('|') >= 0 && i + 1 < lines.length && TSEP_RE.test(lines[i + 1]) && lines[i + 1].indexOf('-') >= 0) {
        flush();
        var head = cells(line), rows = [];
        for (i += 2; i < lines.length && lines[i].indexOf('|') >= 0 && lines[i].trim(); i++) rows.push(cells(lines[i]));
        blocks.push({ t: 'table', head: head, rows: rows });
        continue;
      }
      if (/^\s*>/.test(line)) {
        flush();
        var q = [];
        for (; i < lines.length && /^\s*>/.test(lines[i]); i++) q.push(lines[i].replace(/^\s*>\s?/, ''));
        blocks.push({ t: 'quote', lines: q });
        continue;
      }
      if ((m = line.match(LIST_RE)) && !HR_RE.test(line)) {
        flush();
        var ordered = /\d/.test(m[2]), list = [];
        for (; i < lines.length; i++) {
          var lm = lines[i].match(LIST_RE);
          if (!lm || HR_RE.test(lines[i])) break;
          if (lm[1].length >= 2 && list.length) list[list.length - 1].sub.push(lm[3]);
          else list.push({ text: lm[3], sub: [] });
        }
        blocks.push({ t: 'list', ordered: ordered, items: list });
        continue;
      }
      para.push(line.trim());
      i++;
    }
    flush();
    return blocks;
  }

  // ---- DOM --------------------------------------------------------------

  function el(doc, tag, cls, text) {
    var e = doc.createElement(tag);
    if (cls) e.className = cls;
    if (text != null) e.textContent = text;
    return e;
  }

  function appendInline(doc, parent, toks, opts, card) {
    toks.forEach(function (k) {
      switch (k.t) {
        case 'text': parent.appendChild(doc.createTextNode(k.s)); break;
        case 'code': parent.appendChild(el(doc, 'code', 'cp-code', k.s)); break;
        case 'strong': {
          var s = el(doc, 'strong');
          appendInline(doc, s, k.kids, opts, card);
          parent.appendChild(s);
          break;
        }
        case 'num': parent.appendChild(el(doc, 'span', 'cp-num' + (k.sign ? ' ' + k.sign : ''), k.s)); break;
        case 'ref': parent.appendChild(el(doc, 'span', 'cp-ref', k.s)); break;
        case 'tf': parent.appendChild(el(doc, 'span', 'cp-tf', k.s)); break;
        case 'dir': parent.appendChild(el(doc, 'span', (card ? 'cp-pill ' : 'cp-dir ') + k.tone, k.s)); break;
        case 'sym': {
          var a = el(doc, 'a', 'cp-sym', k.s);
          a.setAttribute('href', '/chart?symbol=' + encodeURIComponent(k.sym) + (opts.tf ? '&tf=' + encodeURIComponent(opts.tf) : ''));
          parent.appendChild(a);
          break;
        }
      }
    });
  }
  function rich(doc, tag, cls, text, opts, card) {
    var e = el(doc, tag, cls);
    appendInline(doc, e, inline(text, opts, card), opts, card);
    return e;
  }

  var NUMERIC_CELL = /^[+\-−]?[\d,]+(?:\.\d+)?(?:\s?(?:%|R))?(?:\s?[–~\-]\s?[\d,]+(?:\.\d+)?)?$/;

  function build(blocks, doc, opts) {
    opts = opts || {};
    var L = LABELS[opts.lang === 'en' ? 'en' : 'zh'];
    var frag = doc.createDocumentFragment();
    blocks.forEach(function (b) {
      var e;
      switch (b.t) {
        case 'verdict': {
          var cls = { long: 'long', short: 'short', flat: 'flat', win: 'long', loss: 'short' }[b.tone] || '';
          e = el(doc, 'div', 'cp-verdict' + (cls ? ' ' + cls : ''));
          var lab = el(doc, 'div', 'cp-verdict-label');
          lab.appendChild(el(doc, 'span', null, b.label));
          if (b.tone) lab.appendChild(el(doc, 'span', 'cp-pill ' + cls, L[b.tone]));
          e.appendChild(lab);
          e.appendChild(rich(doc, 'div', 'cp-verdict-text', b.text, opts, false));
          break;
        }
        case 'levels': {
          e = el(doc, 'div', 'cp-levels');
          b.rows.forEach(function (r) {
            var row = el(doc, 'div', 'cp-level' + (r.type ? ' ' + r.type : ''));
            row.appendChild(el(doc, 'span', 'cp-level-type', r.label));
            row.appendChild(el(doc, 'span', 'cp-level-px', r.px));
            row.appendChild(rich(doc, 'span', 'cp-level-note', r.note, opts, true));
            e.appendChild(row);
          });
          break;
        }
        case 'pre': {
          e = el(doc, 'pre', 'cp-pre');
          e.appendChild(el(doc, 'code', null, b.text));
          break;
        }
        case 'stats': {
          e = el(doc, 'div', 'cp-stats');
          b.items.forEach(function (it) {
            var c = el(doc, 'div', 'cp-stat' + (it.tone === 'stop' || it.tone === 'tp' ? ' ' + it.tone : '') +
              (it.tone === 'good' || it.tone === 'bad' ? ' rr ' + it.tone : '') + (it.wide ? ' wide' : ''));
            c.appendChild(el(doc, 'div', 'cp-stat-k', it.k));
            c.appendChild(rich(doc, 'div', 'cp-stat-v', it.v, opts, true));
            e.appendChild(c);
          });
          break;
        }
        case 'h': e = el(doc, 'h4', 'cp-h' + (b.level >= 3 ? ' sub' : ''), b.text); break;
        case 'hr': e = el(doc, 'hr', 'cp-hr'); break;
        case 'callout': {
          e = el(doc, 'div', 'cp-callout warn');
          e.appendChild(el(doc, 'strong', 'cp-callout-k', b.label + '：'));
          appendInline(doc, e, inline(b.text, opts, false), opts, false);
          break;
        }
        case 'quote': {
          e = el(doc, 'blockquote', 'cp-quote');
          b.lines.forEach(function (l, i) {
            if (i) e.appendChild(el(doc, 'br'));
            appendInline(doc, e, inline(l, opts, false), opts, false);
          });
          break;
        }
        case 'list': {
          e = el(doc, b.ordered ? 'ol' : 'ul', 'cp-list');
          b.items.forEach(function (it) {
            var li = rich(doc, 'li', null, it.text, opts, false);
            if (it.sub.length) {
              var ul = el(doc, 'ul', 'cp-list');
              it.sub.forEach(function (s) { ul.appendChild(rich(doc, 'li', null, s, opts, false)); });
              li.appendChild(ul);
            }
            e.appendChild(li);
          });
          break;
        }
        case 'table': {
          e = el(doc, 'div', 'cp-table-wrap');
          var t = el(doc, 'table', 'cp-table'), th = el(doc, 'thead'), tr = el(doc, 'tr');
          b.head.forEach(function (h) { tr.appendChild(rich(doc, 'th', null, h, opts, true)); });
          th.appendChild(tr); t.appendChild(th);
          var tb = el(doc, 'tbody');
          b.rows.forEach(function (r) {
            var row = el(doc, 'tr');
            r.forEach(function (c) { row.appendChild(rich(doc, 'td', NUMERIC_CELL.test(c) ? 'num' : null, c, opts, true)); });
            tb.appendChild(row);
          });
          t.appendChild(tb); e.appendChild(t);
          break;
        }
        default: {
          e = el(doc, 'p', 'cp-p');
          b.lines.forEach(function (l, i) {
            if (i) e.appendChild(el(doc, 'br'));
            appendInline(doc, e, inline(l, opts, false), opts, false);
          });
        }
      }
      frag.appendChild(e);
    });
    return frag;
  }

  // render(text, opts) → DocumentFragment. opts: {lang, symbols, tf, doc}.
  function render(text, opts) {
    opts = opts || {};
    return build(parse(text, opts.lang), opts.doc || root.document, opts);
  }

  var api = { parse: parse, inline: inline, build: build, render: render, DEFAULT_SYMBOLS: DEFAULT_SYMBOLS };
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  else root.CopilotRender = api;
})(typeof window !== 'undefined' ? window : this);
