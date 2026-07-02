// Shared renderer for AI analyze responses.
//
// The Quant advisor produces bilingual output wrapped in [EN]...[/EN]
// and [ZH-TW]...[/ZH-TW] delimiters. This module:
//   - parses the two language blocks
//   - renders one at a time as Markdown
//   - attaches a language-toggle button ([繁] / [EN]) inside the meta row
//   - persists the user's language choice in localStorage
//
// Used by dashboard.html, journal_list.html (per-trade Analyze), and
// validate_result.html — one code path, one toggle behavior.

(function () {
    if (window.aiRender) return; // idempotent — templates each include this

    var LS_KEY = 'ai-lang'; // 'en' | 'zh-tw'

    function fmtCost(usd) {
        if (usd === 0) return '(free / $0)';
        if (!usd) return '';
        return '($' + usd.toFixed(4) + ')';
    }

    // Minimal Markdown → HTML — headers, bold/italic, code, tables. Not a
    // full parser. Same subset used by the earlier renderMD implementations
    // in each template; centralized here so all 3 endpoints render the
    // same way.
    function renderMD(s) {
        s = s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
        s = s.replace(/```([\s\S]*?)```/g, function (_, code) { return '<pre><code>' + code + '</code></pre>'; });
        s = s.replace(/^#### (.+)$/gm, '<h5>$1</h5>');
        s = s.replace(/^### (.+)$/gm, '<h4>$1</h4>');
        s = s.replace(/^## (.+)$/gm, '<h3>$1</h3>');
        s = s.replace(/^# (.+)$/gm,  '<h2>$1</h2>');
        s = s.replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>');
        s = s.replace(/\*([^*]+)\*/g,     '<em>$1</em>');
        s = s.replace(/`([^`]+)`/g,       '<code>$1</code>');

        // Very light table support (pipe-row → <table>). Skip separator rows.
        var lines = s.split('\n');
        var out = [];
        var inTable = false;
        for (var i = 0; i < lines.length; i++) {
            var ln = lines[i];
            if (ln.match(/^\s*\|.*\|\s*$/)) {
                if (!inTable) { out.push('<table class="ai-md-table">'); inTable = true; }
                if (ln.match(/^\s*\|?\s*:?-+:?(\s*\|\s*:?-+:?)+\s*\|?\s*$/)) continue;
                var cells = ln.replace(/^\s*\||\|\s*$/g, '').split('|').map(function (c) { return c.trim(); });
                var tag = (i > 0 && lines[i + 1] && lines[i + 1].match(/^\s*\|?\s*:?-+/)) ? 'th' : 'td';
                out.push('<tr><' + tag + '>' + cells.join('</' + tag + '><' + tag + '>') + '</' + tag + '></tr>');
            } else {
                if (inTable) { out.push('</table>'); inTable = false; }
                out.push(ln);
            }
        }
        if (inTable) out.push('</table>');
        var joined = out.join('\n').replace(/\n\n+/g, '<br><br>').replace(/\n/g, '<br>');
        // Block-level HTML (headers h2-h5, tables, pre) already carries its
        // own vertical margin. Any <br>s we inserted next to them stack on
        // top of that margin — the gap balloons. Strip <br>s immediately
        // before or after block-level tags so spacing comes from CSS only.
        return joined
            .replace(/(<\/(?:h[2-5]|table|pre)>)(<br>)+/g, '$1')
            .replace(/(<br>)+(<(?:h[2-5]|table|pre)[\s>])/g, '$2');
    }

    // Extract [EN]/[ZH-TW] blocks. Returns {en, zh, hasBoth}. If either
    // block is missing (older responses or model misbehavior), falls back
    // to the raw text in the other language slot.
    function parseBilingual(text) {
        if (!text) return { en: '', zh: '', hasBoth: false };
        var enMatch = text.match(/\[EN\]([\s\S]*?)\[\/EN\]/);
        var zhMatch = text.match(/\[ZH-TW\]([\s\S]*?)\[\/ZH-TW\]/);
        var en = enMatch ? enMatch[1].trim() : '';
        var zh = zhMatch ? zhMatch[1].trim() : '';
        if (!en && !zh) {
            // No delimiters at all — treat entire text as one language.
            // Heuristic: presence of CJK chars → treat as zh.
            var hasCJK = /[一-鿿]/.test(text);
            if (hasCJK) zh = text; else en = text;
        }
        if (en && !zh) zh = en; // fallback if model only produced one side
        if (zh && !en) en = zh;
        return { en: en, zh: zh, hasBoth: !!(enMatch && zhMatch) };
    }

    function preferredLang() {
        try {
            var v = localStorage.getItem(LS_KEY);
            if (v === 'en' || v === 'zh-tw') return v;
        } catch (e) {}
        return 'zh-tw'; // default — user primarily writes in zh
    }

    function saveLang(v) {
        try { localStorage.setItem(LS_KEY, v); } catch (e) {}
    }

    // Render an AI response body into the given pane element. Handles
    // error responses, cached-flag display, token/cost meta, and the
    // language toggle.
    function renderInto(pane, resBody, opts) {
        opts = opts || {};
        if (resBody.error) {
            pane.innerHTML = '<div class="ai-error">⚠ ' + resBody.error + '</div>';
            return;
        }
        var parsed = parseBilingual(resBody.text || '');
        var lang = preferredLang();
        var current = (lang === 'en') ? parsed.en : parsed.zh;

        var tok = (resBody.input_tokens + resBody.output_tokens) > 0
            ? (resBody.input_tokens + '+' + resBody.output_tokens + ' tok ')
            : '';
        var cached = resBody.cached ? ' · cached' : '';

        var toggleHTML = '';
        if (parsed.hasBoth) {
            toggleHTML = ''
                + '<div class="ai-lang-toggle" role="group" aria-label="analysis language">'
                +   '<button type="button" class="ai-lang-btn' + (lang === 'zh-tw' ? ' active' : '') + '" data-lang="zh-tw">繁</button>'
                +   '<button type="button" class="ai-lang-btn' + (lang === 'en'    ? ' active' : '') + '" data-lang="en">EN</button>'
                + '</div>';
        }

        pane.innerHTML = ''
            + toggleHTML
            + '<div class="ai-body" data-lang-view>' + renderMD(current) + '</div>'
            + '<div class="ai-meta">' + tok + fmtCost(resBody.cost_usd) + cached + '</div>';

        if (parsed.hasBoth) {
            pane.querySelectorAll('.ai-lang-btn').forEach(function (b) {
                b.addEventListener('click', function () {
                    var target = b.dataset.lang;
                    saveLang(target);
                    var next = (target === 'en') ? parsed.en : parsed.zh;
                    var body = pane.querySelector('[data-lang-view]');
                    if (body) body.innerHTML = renderMD(next);
                    pane.querySelectorAll('.ai-lang-btn').forEach(function (x) {
                        x.classList.toggle('active', x.dataset.lang === target);
                    });
                });
            });
        }
    }

    window.aiRender = {
        renderInto: renderInto,
        renderMD: renderMD,             // exposed for older call sites still using it
        parseBilingual: parseBilingual, // exposed for tests / debugging
    };
})();
