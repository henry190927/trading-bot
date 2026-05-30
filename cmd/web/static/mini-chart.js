// Mini price chart for each dashboard symbol card.
//
// Two render modes:
//   - "line"   : closes-only line + area fill
//   - "candle" : OHLC candlesticks via uPlot series.paths hook (default)
//
// Mode preference (line vs candle) lives in localStorage.mini-chart-mode.
// Toggle buttons [data-mode="..."] in the topbar flip it for all charts.
(function () {
    if (typeof uPlot === 'undefined') return;

    var STORE_KEY = 'mini-chart-mode';
    var DEFAULT_MODE = 'candle';

    var COLORS = {
        line:   '#56d4dd',
        fill:   'rgba(86, 212, 221, 0.10)',
        up:     '#4ade80',
        down:   '#f87171',
        entry:  '#58a6ff',
        stop:   '#f87171',
        tp:     '#4ade80',
        mark:   '#fbbf24',
        poc:    'rgba(192, 132, 252, 0.75)',  // soft purple — POC
        va:     'rgba(192, 132, 252, 0.35)',  // faint purple — VAH/VAL edges
        vaFill: 'rgba(192, 132, 252, 0.06)',  // very faint purple band fill
        grid:   'rgba(139, 148, 158, 0.10)',
        axis:   '#8b949e',
    };

    function getMode() {
        try {
            var v = localStorage.getItem(STORE_KEY);
            return v === 'line' ? 'line' : 'candle';
        } catch (e) { return DEFAULT_MODE; }
    }
    function setMode(m) { try { localStorage.setItem(STORE_KEY, m); } catch (e) {} }

    function drawHLine(u, y, color, dashed, label) {
        if (!isFinite(y) || y <= 0) return;
        var yPx = u.valToPos(y, 'y', true);
        if (yPx < 0 || yPx > u.bbox.height + u.bbox.top) return;
        var ctx = u.ctx;
        ctx.save();
        ctx.strokeStyle = color;
        ctx.lineWidth = 1;
        if (dashed) ctx.setLineDash([4, 3]);
        ctx.beginPath();
        ctx.moveTo(u.bbox.left, yPx);
        ctx.lineTo(u.bbox.left + u.bbox.width, yPx);
        ctx.stroke();
        // Italic label near the right edge — only emitted when the
        // caller supplies one (i.e. the expanded modal). Dark outline
        // around the glyph keeps it readable over candles without an
        // ugly opaque chip behind it.
        if (label) {
            ctx.setLineDash([]);
            ctx.font = 'italic bold 19px ui-monospace, monospace';
            ctx.textBaseline = 'middle';
            var tw = ctx.measureText(label).width;
            var x = u.bbox.left + u.bbox.width - tw - 10;
            ctx.lineJoin = 'round';
            ctx.lineWidth = 4;
            ctx.strokeStyle = 'rgba(13, 17, 23, 0.95)';
            ctx.strokeText(label, x, yPx);
            ctx.fillStyle = color;
            ctx.fillText(label, x, yPx);
        }
        ctx.restore();
    }

    function candlePaths(u, seriesIdx, idx0, idx1) {
        var ctx = u.ctx;
        var t = u.data[0], o = u.data[1], h = u.data[2], l = u.data[3], c = u.data[4];

        var spacing = (u.bbox.width / (idx1 - idx0 + 1));
        var bodyW = Math.max(1, Math.min(spacing * 0.7, 18));

        ctx.save();
        ctx.lineWidth = 1;
        for (var i = idx0; i <= idx1; i++) {
            var xPx = u.valToPos(t[i], 'x', true);
            var oPx = u.valToPos(o[i], 'y', true);
            var hPx = u.valToPos(h[i], 'y', true);
            var lPx = u.valToPos(l[i], 'y', true);
            var cPx = u.valToPos(c[i], 'y', true);
            var isUp = c[i] >= o[i];
            var color = isUp ? COLORS.up : COLORS.down;

            ctx.strokeStyle = color;
            ctx.beginPath();
            ctx.moveTo(xPx, hPx);
            ctx.lineTo(xPx, lPx);
            ctx.stroke();

            var bodyTop = Math.min(oPx, cPx);
            var bodyH = Math.abs(cPx - oPx);
            if (bodyH < 1) bodyH = 1;
            ctx.fillStyle = color;
            ctx.fillRect(xPx - bodyW / 2, bodyTop, bodyW, bodyH);
        }
        ctx.restore();
        return null;
    }

    function yRangeWithRefs(data) {
        return function (u, dataMin, dataMax) {
            var lo = dataMin, hi = dataMax;
            for (var i = 0; i < data.l.length; i++) {
                if (data.l[i] < lo) lo = data.l[i];
                if (data.h[i] > hi) hi = data.h[i];
            }
            [data.entry, data.stop, data.tp1, data.tp2, data.mark,
             data.poc, data.vah, data.val].forEach(function (v) {
                if (isFinite(v) && v > 0) {
                    if (v < lo) lo = v;
                    if (v > hi) hi = v;
                }
            });
            var pad = (hi - lo) * 0.05 || 1;
            return [lo - pad, hi + pad];
        };
    }

    // Faint horizontal band fill between VAH and VAL.
    function drawVABand(u, val, vah) {
        if (!(val > 0 && vah > 0 && vah > val)) return;
        var yTop = u.valToPos(vah, 'y', true);
        var yBot = u.valToPos(val, 'y', true);
        if (yTop > yBot) { var t = yTop; yTop = yBot; yBot = t; }
        var ctx = u.ctx;
        ctx.save();
        ctx.fillStyle = COLORS.vaFill;
        ctx.fillRect(u.bbox.left, yTop, u.bbox.width, yBot - yTop);
        ctx.restore();
    }

    function buildOpts(data, mode, width, height, withLabels) {
        // Y-axis label format auto-adjusts to magnitude so big prices
        // (BTC ~100k) and small (XAG ~50) both fit cleanly.
        function fmtPrice(v) {
            var a = Math.abs(v);
            if (a >= 10000) return v.toFixed(0);
            if (a >= 100)   return v.toFixed(1);
            if (a >= 10)    return v.toFixed(2);
            return v.toFixed(3);
        }
        var axisSize = withLabels ? 96 : 64;
        var axisFont = withLabels ? '16px ui-monospace, monospace' : '11px ui-monospace, monospace';
        var commonAxes = [
            { show: false },
            {
                show: true, stroke: COLORS.axis,
                grid: { show: true, stroke: COLORS.grid, width: 1 },
                ticks: { show: false }, size: axisSize,
                font: axisFont,
                values: function (u, splits) { return splits.map(fmtPrice); },
            }
        ];
        // Labels only in the expanded modal so the small dashboard chart
        // stays uncluttered.
        var lbl = withLabels ? function (s) { return s; } : function () { return null; };
        var hookDraw = function (u) {
            // VA band first so plan/mark lines render on top.
            drawVABand(u, data.val, data.vah);
            drawHLine(u, data.val,  COLORS.va,    true,  lbl('VAL'));
            drawHLine(u, data.vah,  COLORS.va,    true,  lbl('VAH'));
            drawHLine(u, data.poc,  COLORS.poc,   false, lbl('POC'));
            drawHLine(u, data.mark, COLORS.mark,  true,  lbl('mark'));
            if (data.entry > 0) {
                drawHLine(u, data.entry, COLORS.entry, false, lbl('entry'));
                drawHLine(u, data.stop,  COLORS.stop,  false, lbl('stop'));
                if (data.tp1 > 0) drawHLine(u, data.tp1, COLORS.tp, false, lbl('TP1'));
                if (data.tp2 > 0) drawHLine(u, data.tp2, COLORS.tp, false, lbl('TP2'));
            }
        };
        var cursor = {
            drag: { x: false, y: false },
            points: { size: 4 },
        };

        if (mode === 'candle') {
            return {
                width: width, height: height,
                padding: [4, 8, 4, 8],
                cursor: cursor,
                legend: { show: false },
                axes: commonAxes,
                scales: { x: { time: true }, y: { auto: true, range: yRangeWithRefs(data) } },
                series: [
                    {},
                    { label: 'o', show: false }, { label: 'h', show: false },
                    { label: 'l', show: false },
                    { label: 'c', paths: candlePaths, points: { show: false } },
                ],
                hooks: { draw: [hookDraw] },
            };
        }
        return {
            width: width, height: height,
            padding: [4, 8, 4, 8],
            cursor: cursor,
            legend: { show: false },
            axes: commonAxes,
            scales: { x: { time: true }, y: { auto: true, range: yRangeWithRefs(data) } },
            series: [
                {},
                { label: 'close', stroke: COLORS.line, width: 1.5, fill: COLORS.fill, points: { show: false } },
            ],
            hooks: { draw: [hookDraw] },
        };
    }

    function dataArrFor(data, mode) {
        if (mode === 'candle') return [data.t, data.o, data.h, data.l, data.c];
        return [data.t, data.c];
    }

    function render(node) {
        var raw = node.getAttribute('data-chart');
        if (!raw) return;
        var data;
        try { data = JSON.parse(raw); } catch (e) { return; }
        if (!data || !data.t || !data.c || data.t.length < 2) return;

        // Skip uPlot init entirely when the card is collapsed — saves a
        // bunch of canvas work on every 30s refresh.
        var sym = node.getAttribute('data-sym') || '';
        if (sym && isHidden(sym)) {
            if (node._uplot) {
                try { node._uplot.destroy(); } catch (e) {}
                node._uplot = null;
            }
            // Keep the expand button intact even when collapsed-hidden so
            // re-expanding the chart doesn't drop it; but here the parent
            // is display:none so the button is hidden anyway.
            return;
        }

        if (node._uplot) {
            try { node._uplot.destroy(); } catch (e) {}
            node._uplot = null;
        }
        // Preserve the expand button on re-renders — only wipe canvases.
        node.querySelectorAll('.uplot').forEach(function (el) { el.remove(); });

        var mode = getMode();
        var width = node.clientWidth || 320;
        var height = node.clientHeight || 130;
        var opts = buildOpts(data, mode, width, height);

        node._uplot = new uPlot(opts, dataArrFor(data, mode), node);

        ensureExpandIcon(node);
    }

    // ─── Expand to modal ───────────────────────────────────────────────
    // Click the ⤢ icon to open a fixed-size larger view with the same
    // reference lines (POC, VA band, plan, mark). No drag/pinch zoom —
    // just a bigger render for visual inspection.

    function ensureExpandIcon(node) {
        if (node.querySelector('.mini-chart-expand')) return;
        var btn = document.createElement('button');
        btn.type = 'button';
        btn.className = 'mini-chart-expand';
        btn.setAttribute('aria-label', 'expand chart');
        btn.title = 'expand';
        btn.textContent = '⤢';
        btn.addEventListener('click', function (ev) {
            ev.preventDefault();
            ev.stopPropagation();
            openFullscreen(node);
        });
        node.appendChild(btn);
    }

    var modal = null, modalChart = null, modalSym = '';

    function buildModal() {
        if (modal) return modal;
        modal = document.createElement('div');
        modal.className = 'chart-modal hidden';
        modal.innerHTML =
            '<div class="chart-modal-backdrop"></div>' +
            '<div class="chart-modal-card">' +
            '  <div class="chart-modal-head">' +
            '    <span class="chart-modal-title"></span>' +
            '    <button type="button" class="chart-modal-close" aria-label="close">×</button>' +
            '  </div>' +
            '  <div class="chart-modal-body"></div>' +
            '</div>';
        document.body.appendChild(modal);
        modal.querySelector('.chart-modal-close').addEventListener('click', closeFullscreen);
        modal.querySelector('.chart-modal-backdrop').addEventListener('click', closeFullscreen);
        document.addEventListener('keydown', function (ev) {
            if (ev.key === 'Escape' && modal && !modal.classList.contains('hidden')) closeFullscreen();
        });
        return modal;
    }

    function openFullscreen(node) {
        var raw = node.getAttribute('data-chart');
        if (!raw) return;
        var data;
        try { data = JSON.parse(raw); } catch (e) { return; }
        modalSym = node.getAttribute('data-sym') || '';

        buildModal();
        modal.classList.remove('hidden');
        document.body.style.overflow = 'hidden';
        var mode = getMode();
        modal.querySelector('.chart-modal-title').textContent = modalSym + ' · ' + mode;

        var host = modal.querySelector('.chart-modal-body');
        host.innerHTML = '';
        if (modalChart) {
            try { modalChart.destroy(); } catch (e) {}
            modalChart = null;
        }
        var width  = host.clientWidth  || Math.min(window.innerWidth - 60, 1140);
        var height = host.clientHeight || Math.min(Math.round(window.innerHeight * 0.86), 940);
        var opts = buildOpts(data, mode, width, height, true /* labels */);
        modalChart = new uPlot(opts, dataArrFor(data, mode), host);
    }

    function closeFullscreen() {
        if (!modal) return;
        modal.classList.add('hidden');
        document.body.style.overflow = '';
        if (modalChart) {
            try { modalChart.destroy(); } catch (e) {}
            modalChart = null;
        }
        modalSym = '';
    }

    function reopenModalIfMode() {
        if (!modal || modal.classList.contains('hidden') || !modalSym) return;
        var src = document.querySelector('.mini-chart[data-sym="' + modalSym + '"]');
        if (src) openFullscreen(src);
    }

    function renderAll() { document.querySelectorAll('.mini-chart').forEach(render); }

    // ─── Per-card collapse toggle ──────────────────────────────────────
    // Each symbol card gets a tiny chevron next to the score. Charts are
    // collapsed by default to keep the dashboard dense; click to expand.
    // The "shown" key holds '1' iff the user has opted to keep this
    // chart open across the 30s meta-refresh.
    function shownKey(sym) { return 'mini-chart-shown:' + sym; }
    function isHidden(sym) {
        try { return localStorage.getItem(shownKey(sym)) !== '1'; }
        catch (e) { return true; }
    }
    function setHidden(sym, v) {
        try {
            if (v) localStorage.removeItem(shownKey(sym));
            else   localStorage.setItem(shownKey(sym), '1');
        } catch (e) {}
    }

    function applyHiddenState(card, sym) {
        var chart = card.querySelector('.mini-chart');
        var btn   = card.querySelector('.chart-toggle');
        if (!btn) return;
        var hidden = isHidden(sym);
        if (chart) chart.style.display = hidden ? 'none' : '';
        btn.textContent = hidden ? '▸' : '▾';
        btn.setAttribute('aria-expanded', hidden ? 'false' : 'true');
        btn.setAttribute('title', hidden ? 'show chart' : 'hide chart');
    }

    function installToggles() {
        document.querySelectorAll('.symbol-card').forEach(function (card) {
            var chart = card.querySelector('.mini-chart');
            if (!chart) return;
            var sym = chart.getAttribute('data-sym') || '';
            var head = card.querySelector('.card-head');
            if (!head) return;
            if (card.querySelector('.chart-toggle')) {
                applyHiddenState(card, sym);
                return;
            }
            var btn = document.createElement('button');
            btn.type = 'button';
            btn.className = 'chart-toggle';
            btn.addEventListener('click', function () {
                var nowHidden = !isHidden(sym);
                setHidden(sym, nowHidden);
                applyHiddenState(card, sym);
                if (!nowHidden) render(chart); // lazy-render on expand
            });
            head.appendChild(btn);
            applyHiddenState(card, sym);
        });
    }

    function syncToggle() {
        var mode = getMode();
        document.querySelectorAll('.cs-btn').forEach(function (b) {
            var active = b.dataset.mode === mode;
            b.classList.toggle('active', active);
            b.setAttribute('aria-pressed', active ? 'true' : 'false');
        });
    }
    document.querySelectorAll('.cs-btn').forEach(function (b) {
        b.addEventListener('click', function () {
            setMode(b.dataset.mode);
            syncToggle();
            renderAll();
            reopenModalIfMode();
        });
    });

    syncToggle();
    installToggles();
    renderAll();

    var resizeTimer = null;
    window.addEventListener('resize', function () {
        if (resizeTimer) clearTimeout(resizeTimer);
        resizeTimer = setTimeout(renderAll, 150);
    });
})();
