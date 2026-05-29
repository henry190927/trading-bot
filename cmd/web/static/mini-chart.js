// Mini price chart for each dashboard symbol card.
//
// Two render modes:
//   - "line"   : closes-only line + area fill (lighter)
//   - "candle" : OHLC candlesticks drawn via uPlot's series.paths hook
//
// Mode is a single global preference stored in localStorage.mini-chart-mode
// (default: candle). Toggle buttons in the topbar [data-mode="line|candle"]
// flip the preference and re-render all charts on the page.
//
// Reference lines (drawn on top regardless of mode):
//   - Live mark         — yellow dashed
//   - Plan entry / stop / TP1 / TP2 (when plan exists) — solid blue / red /
//     green / green
(function () {
    if (typeof uPlot === 'undefined') return;

    var STORE_KEY = 'mini-chart-mode';
    var DEFAULT_MODE = 'candle';

    var COLORS = {
        line:   '#56d4dd',
        fill:   'rgba(86, 212, 221, 0.10)',
        up:     '#4ade80',
        down:   '#f87171',
        wick:   'rgba(139, 148, 158, 0.85)',
        entry:  '#58a6ff',
        stop:   '#f87171',
        tp:     '#4ade80',
        mark:   '#fbbf24',
        grid:   'rgba(139, 148, 158, 0.10)',
        axis:   '#8b949e',
    };

    function getMode() {
        try {
            var v = localStorage.getItem(STORE_KEY);
            return v === 'line' ? 'line' : 'candle';
        } catch (e) {
            return DEFAULT_MODE;
        }
    }
    function setMode(m) {
        try { localStorage.setItem(STORE_KEY, m); } catch (e) {}
    }

    function drawHLine(u, y, color, dashed) {
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
        ctx.restore();
    }

    // Custom series renderer that paints OHLC candles directly to the canvas.
    // uPlot calls this once per draw with the visible index range; we draw a
    // wick line + body rect per candle.
    function candlePaths(u, seriesIdx, idx0, idx1) {
        var ctx = u.ctx;
        var t  = u.data[0];
        var o  = u.data[1];
        var h  = u.data[2];
        var l  = u.data[3];
        var c  = u.data[4];

        // Body width: half the spacing between consecutive candles, clamped.
        var spacing = (u.bbox.width / (idx1 - idx0 + 1));
        var bodyW = Math.max(1, Math.min(spacing * 0.7, 12));

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

            // Wick
            ctx.strokeStyle = color;
            ctx.beginPath();
            ctx.moveTo(xPx, hPx);
            ctx.lineTo(xPx, lPx);
            ctx.stroke();

            // Body
            var bodyTop = Math.min(oPx, cPx);
            var bodyH = Math.abs(cPx - oPx);
            if (bodyH < 1) bodyH = 1; // doji floor
            ctx.fillStyle = color;
            ctx.fillRect(xPx - bodyW / 2, bodyTop, bodyW, bodyH);
        }
        ctx.restore();
        return null; // tells uPlot not to do its own series drawing
    }

    function yRangeWithRefs(data) {
        // Combine OHLC + reference lines into the y-axis range so plan
        // markers always stay on-screen.
        return function (u, dataMin, dataMax) {
            var lo = dataMin, hi = dataMax;
            // Highs/lows from candles are the true extremes (line mode
            // dataMin/Max come from closes only — fold OHLC in too).
            for (var i = 0; i < data.l.length; i++) {
                if (data.l[i] < lo) lo = data.l[i];
                if (data.h[i] > hi) hi = data.h[i];
            }
            [data.entry, data.stop, data.tp1, data.tp2, data.mark].forEach(function (v) {
                if (isFinite(v) && v > 0) {
                    if (v < lo) lo = v;
                    if (v > hi) hi = v;
                }
            });
            var pad = (hi - lo) * 0.05 || 1;
            return [lo - pad, hi + pad];
        };
    }

    function buildOpts(data, mode, width, height) {
        var commonAxes = [
            { show: false },
            {
                show: true, stroke: COLORS.axis,
                grid: { show: true, stroke: COLORS.grid, width: 1 },
                ticks: { show: false }, size: 32,
                font: '9px ui-monospace, monospace',
                values: function (u, splits) { return splits.map(function (v) { return v.toFixed(1); }); },
            }
        ];
        var hookDraw = function (u) {
            drawHLine(u, data.mark, COLORS.mark, true);
            if (data.entry > 0) {
                drawHLine(u, data.entry, COLORS.entry, false);
                drawHLine(u, data.stop,  COLORS.stop,  false);
                if (data.tp1 > 0) drawHLine(u, data.tp1, COLORS.tp, false);
                if (data.tp2 > 0) drawHLine(u, data.tp2, COLORS.tp, false);
            }
        };

        if (mode === 'candle') {
            return {
                width: width, height: height,
                padding: [4, 8, 4, 8],
                cursor: { drag: { x: false, y: false }, points: { size: 4 } },
                legend: { show: false },
                axes: commonAxes,
                scales: { x: { time: true }, y: { auto: true, range: yRangeWithRefs(data) } },
                // 5 series total: x + o + h + l + c. Only the LAST one draws
                // (candle paths); the first 4 are data-only (no visual).
                series: [
                    {},
                    { label: 'o', show: false }, { label: 'h', show: false },
                    { label: 'l', show: false },
                    { label: 'c', paths: candlePaths, points: { show: false } },
                ],
                hooks: { draw: [hookDraw] },
            };
        }
        // line mode
        return {
            width: width, height: height,
            padding: [4, 8, 4, 8],
            cursor: { drag: { x: false, y: false }, points: { size: 4 } },
            legend: { show: false },
            axes: commonAxes,
            scales: { x: { time: true }, y: { auto: true, range: yRangeWithRefs(data) } },
            series: [
                {},
                {
                    label: 'close', stroke: COLORS.line, width: 1.5,
                    fill: COLORS.fill, points: { show: false },
                },
            ],
            hooks: { draw: [hookDraw] },
        };
    }

    function render(node) {
        var raw = node.getAttribute('data-chart');
        if (!raw) return;
        var data;
        try { data = JSON.parse(raw); } catch (e) { return; }
        if (!data || !data.t || !data.c || data.t.length < 2) return;

        if (node._uplot) {
            try { node._uplot.destroy(); } catch (e) {}
            node._uplot = null;
        }
        node.innerHTML = '';

        var mode = getMode();
        var width = node.clientWidth || 320;
        var height = node.clientHeight || 130;

        var arr;
        if (mode === 'candle') {
            arr = [data.t, data.o, data.h, data.l, data.c];
        } else {
            arr = [data.t, data.c];
        }
        var opts = buildOpts(data, mode, width, height);
        node._uplot = new uPlot(opts, arr, node);
    }

    function renderAll() {
        document.querySelectorAll('.mini-chart').forEach(render);
    }

    // Wire the toggle buttons.
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
        });
    });

    syncToggle();
    renderAll();

    var resizeTimer = null;
    window.addEventListener('resize', function () {
        if (resizeTimer) clearTimeout(resizeTimer);
        resizeTimer = setTimeout(renderAll, 150);
    });
})();
