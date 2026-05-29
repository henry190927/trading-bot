// Mini price chart for each dashboard symbol card.
//
// Reads chart data inlined into the card via `data-chart` (JSON), renders
// a single closes-line via uPlot. Overlays plan entry / stop / TP1 / TP2 as
// horizontal reference lines when the engine has a plan. Live mark gets a
// subtle dashed line of its own — distinct from the engine reference levels.
//
// Sizing: the chart fills the card width, height fixed at ~80px. Mobile-
// first; tap is a no-op (uPlot's default hover tooltips work too).
(function () {
    if (typeof uPlot === 'undefined') return;
    var nodes = document.querySelectorAll('.mini-chart');
    if (!nodes.length) return;

    // Dark palette matched to the site's existing CSS vars.
    var COLORS = {
        line:  '#56d4dd',   // cyan, like the sym labels
        fill:  'rgba(86, 212, 221, 0.10)',
        entry: '#58a6ff',   // blue
        stop:  '#f87171',   // red
        tp:    '#4ade80',   // green
        mark:  '#fbbf24',   // yellow
        grid:  'rgba(139, 148, 158, 0.10)',
        axis:  '#8b949e',
    };

    function plotLine(self, idx) {
        // Custom paths: simple linear segments.
        return null; // use default
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

    function render(node) {
        var raw = node.getAttribute('data-chart');
        if (!raw) return;
        var data;
        try { data = JSON.parse(raw); } catch (e) { return; }
        if (!data || !data.t || !data.c || data.t.length < 2) return;

        // Skip if already rendered (defensive — meta-refresh replaces the
        // whole page so this rarely matters, but useful in dev with SPA-ish
        // partial updates later).
        if (node._uplot) {
            try { node._uplot.destroy(); } catch (e) {}
            node._uplot = null;
        }
        node.innerHTML = '';

        var width = node.clientWidth || 320;
        var height = 80;

        var opts = {
            width: width,
            height: height,
            padding: [4, 8, 4, 8],
            cursor: { drag: { x: false, y: false }, points: { size: 4 } },
            legend: { show: false },
            axes: [
                { show: false },
                {
                    show: true,
                    stroke: COLORS.axis,
                    grid: { show: true, stroke: COLORS.grid, width: 1 },
                    ticks: { show: false },
                    size: 28,
                    font: '9px ui-monospace, monospace',
                    values: function (u, splits) {
                        return splits.map(function (v) { return v.toFixed(0); });
                    },
                }
            ],
            scales: {
                x: { time: true },
                y: { auto: true, range: function (u, dataMin, dataMax) {
                    // Include plan levels and mark in the Y range so reference
                    // lines stay on-screen even if they're outside the close
                    // price's recent range.
                    var lo = dataMin, hi = dataMax;
                    [data.entry, data.stop, data.tp1, data.tp2, data.mark].forEach(function (v) {
                        if (isFinite(v) && v > 0) {
                            if (v < lo) lo = v;
                            if (v > hi) hi = v;
                        }
                    });
                    var pad = (hi - lo) * 0.05 || 1;
                    return [lo - pad, hi + pad];
                }}
            },
            series: [
                {},
                {
                    label: 'close',
                    stroke: COLORS.line,
                    width: 1.5,
                    fill: COLORS.fill,
                    points: { show: false },
                },
            ],
            hooks: {
                draw: [
                    function (u) {
                        // Draw the reference lines AFTER uPlot's own drawing
                        // so they sit on top of the area fill.
                        drawHLine(u, data.mark,  COLORS.mark,  true);
                        if (data.entry > 0) {
                            drawHLine(u, data.entry, COLORS.entry, false);
                            drawHLine(u, data.stop,  COLORS.stop,  false);
                            if (data.tp1 > 0) drawHLine(u, data.tp1, COLORS.tp, false);
                            if (data.tp2 > 0) drawHLine(u, data.tp2, COLORS.tp, false);
                        }
                    }
                ]
            }
        };

        var arr = [data.t, data.c];
        node._uplot = new uPlot(opts, arr, node);
    }

    nodes.forEach(render);

    // Re-render on resize (orientation change, etc.) — debounced.
    var resizeTimer = null;
    window.addEventListener('resize', function () {
        if (resizeTimer) clearTimeout(resizeTimer);
        resizeTimer = setTimeout(function () { nodes.forEach(render); }, 150);
    });
})();
