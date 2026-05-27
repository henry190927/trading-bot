// Partials calculator: lets the user enter multiple exit legs (% of size @ price),
// computes the weighted-average exit price, and fills the form's exit_price /
// outcome / close_notes fields on demand. No CSV schema change — the legs live
// in close_notes as a human-readable tag.
(function () {
    var rows = document.getElementById('partials-rows');
    if (!rows) return;

    var addBtn = document.getElementById('add-partial');
    var summary = document.getElementById('partials-summary');
    var exitInput = document.getElementById('exit_price');
    var outcomeSel = document.getElementById('outcome');
    var closeNotes = document.getElementById('close_notes');

    function rowTemplate() {
        var d = document.createElement('div');
        d.className = 'partial-row';
        d.innerHTML =
            '<input type="number" step="any" min="0" max="100" class="partial-weight" placeholder="% size" inputmode="decimal">' +
            '<input type="number" step="any" min="0" class="partial-price" placeholder="exit price" inputmode="decimal">' +
            '<button type="button" class="btn-remove-partial" aria-label="remove leg">×</button>';
        return d;
    }

    function recompute() {
        var totW = 0, totWP = 0, legs = [];
        rows.querySelectorAll('.partial-row').forEach(function (r) {
            var w = parseFloat(r.querySelector('.partial-weight').value);
            var p = parseFloat(r.querySelector('.partial-price').value);
            if (w > 0 && p > 0) {
                totW += w;
                totWP += w * p;
                legs.push(w + '% @ ' + p);
            }
        });
        if (totW === 0) {
            summary.innerHTML = '';
            summary.dataset.avg = '';
            summary.dataset.legs = '';
            return;
        }
        var avg = totWP / totW;
        var warn = (totW > 100.5 || totW < 99.5)
            ? ' <span class="partials-warn">(Σ ≠ 100%)</span>'
            : '';
        summary.innerHTML =
            '<span>Σ ' + totW.toFixed(1) + '%' + warn + ' · avg <strong>' +
            avg.toFixed(4) + '</strong></span>' +
            '<button type="button" id="apply-partials" class="btn-apply-partials">apply →</button>';
        summary.dataset.avg = avg.toFixed(4);
        summary.dataset.legs = legs.join(', ');
    }

    rows.addEventListener('input', recompute);
    rows.addEventListener('click', function (e) {
        if (e.target.classList.contains('btn-remove-partial')) {
            var all = rows.querySelectorAll('.partial-row');
            if (all.length > 1) {
                e.target.closest('.partial-row').remove();
                recompute();
            } else {
                // last row — just clear it
                all[0].querySelectorAll('input').forEach(function (i) { i.value = ''; });
                recompute();
            }
        }
    });

    // delegated handler: id="apply-partials" is re-rendered each time so we
    // listen on the summary container, not the button itself.
    summary.addEventListener('click', function (e) {
        if (e.target.id !== 'apply-partials') return;
        var avg = summary.dataset.avg;
        var legs = summary.dataset.legs;
        if (!avg) return;
        if (exitInput) exitInput.value = avg;
        if (outcomeSel && !outcomeSel.value) outcomeSel.value = 'manual';
        if (closeNotes && legs) writeLegsTag(legs);
        // visual confirm
        var btn = document.getElementById('apply-partials');
        if (btn) {
            var orig = btn.textContent;
            btn.textContent = '✓ applied';
            btn.disabled = true;
            setTimeout(function () { btn.textContent = orig; btn.disabled = false; }, 1200);
        }
    });

    if (addBtn) addBtn.addEventListener('click', function () {
        rows.appendChild(rowTemplate());
    });

    // writeLegsTag inserts or replaces the "legs: …" segment in close_notes.
    // Segments are delimited by " | " so other notes can co-exist before/after.
    function writeLegsTag(legs) {
        var tag = 'legs: ' + legs;
        var cur = closeNotes.value;
        var start = cur.indexOf('legs:');
        if (start === -1) {
            closeNotes.value = cur ? tag + ' | ' + cur : tag;
            return;
        }
        // Find end of legs segment: next " | " separator, or end of string.
        var rest = cur.slice(start);
        var sepIdx = rest.indexOf(' | ');
        if (sepIdx === -1) {
            closeNotes.value = cur.slice(0, start) + tag;
        } else {
            closeNotes.value = cur.slice(0, start) + tag + cur.slice(start + sepIdx);
        }
    }

    // restoreFromNotes re-populates the widget from a "legs: 50% @ 3100, …"
    // string in close_notes, so editing an already-applied trade brings the
    // legs back instead of forcing the user to retype them.
    function restoreFromNotes() {
        if (!closeNotes || !closeNotes.value) return;
        var idx = closeNotes.value.indexOf('legs:');
        if (idx === -1) return;
        var chunk = closeNotes.value.slice(idx + 5);
        var sep = chunk.indexOf(' | ');
        if (sep !== -1) chunk = chunk.slice(0, sep);
        var re = /(\d+(?:\.\d+)?)\s*%\s*@\s*(\d+(?:\.\d+)?)/g;
        var found = [], m;
        while ((m = re.exec(chunk)) !== null) {
            found.push({ w: m[1], p: m[2] });
        }
        if (!found.length) return;
        rows.innerHTML = '';
        found.forEach(function (leg) {
            var r = rowTemplate();
            r.querySelector('.partial-weight').value = leg.w;
            r.querySelector('.partial-price').value = leg.p;
            rows.appendChild(r);
        });
        recompute();
        var det = document.querySelector('.partials-helper');
        if (det) det.open = true;
    }
    restoreFromNotes();
})();
