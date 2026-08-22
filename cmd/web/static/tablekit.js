/* tablekit — client-side sort + pagination for server-rendered tables.
   Zero deps. Enhances any <table data-tablekit> on the page:
     - click a column header to sort (numeric-aware; toggles asc/desc)
     - paginate at data-page="N" rows/page (omit / 0 = no pagination)
   Works with the mobile card-view (rows are reordered / show-hidden, and
   each <tr> renders as a card via CSS), so nothing here assumes a table
   visual. Non-sortable headers: empty text or [data-nosort]. */
(function () {
  // Strict numeric parse: only real numbers (with %, thousands commas)
  // count as numeric — so "01-02 15:04", "—", "5W/3L" fall back to string
  // sort instead of being mangled into a bogus number.
  function num(str) {
    var s = str.replace(/,/g, '').replace(/%/g, '').trim();
    return /^-?\d*\.?\d+$/.test(s) ? parseFloat(s) : NaN;
  }
  function cellVal(row, i) {
    var td = row.cells[i];
    if (!td) return '';
    return (td.getAttribute('data-sort') || td.textContent || '').trim();
  }
  function cmp(a, b) {
    var na = num(a), nb = num(b), aN = !isNaN(na), bN = !isNaN(nb);
    if (aN && bN) return na - nb;
    if (aN) return -1;          // numbers sort before non-numbers
    if (bN) return 1;
    return a.localeCompare(b);
  }

  function enhance(table) {
    var tbody = table.tBodies[0];
    if (!tbody || !table.tHead) return;
    var rows = Array.prototype.slice.call(tbody.rows);
    if (rows.length === 0) return;
    var headCells = table.tHead.rows[0].cells;
    var pageSize = parseInt(table.getAttribute('data-page') || '0', 10) || 0;
    var page = 1;
    var sortCol = -1, sortDir = 1;

    // ---- pagination ----
    var pager = null;
    function pageCount() { return pageSize ? Math.max(1, Math.ceil(rows.length / pageSize)) : 1; }
    function renderPage() {
      if (!pageSize) return;
      if (page > pageCount()) page = pageCount();
      var start = (page - 1) * pageSize, end = start + pageSize;
      rows.forEach(function (r, i) { r.style.display = (i >= start && i < end) ? '' : 'none'; });
      if (pager) {
        var n = rows.length, a = n ? start + 1 : 0, b = Math.min(end, n);
        pager.querySelector('.tk-range').textContent = a + '–' + b + ' of ' + n;
        pager.querySelector('.tk-prev').disabled = page <= 1;
        pager.querySelector('.tk-next').disabled = page >= pageCount();
      }
    }
    function buildPager() {
      if (!pageSize || rows.length <= pageSize) return;
      pager = document.createElement('div');
      pager.className = 'tablekit-pager';
      pager.innerHTML =
        '<button type="button" class="tk-btn tk-prev" aria-label="previous page">‹ prev</button>' +
        '<span class="tk-range"></span>' +
        '<button type="button" class="tk-btn tk-next" aria-label="next page">next ›</button>';
      var wrap = table.closest('.dt-scroll') || table.parentElement || table;
      wrap.insertAdjacentElement('afterend', pager);
      pager.querySelector('.tk-prev').addEventListener('click', function () { if (page > 1) { page--; renderPage(); } });
      pager.querySelector('.tk-next').addEventListener('click', function () { if (page < pageCount()) { page++; renderPage(); } });
    }

    // ---- sorting ----
    function applySort(col) {
      if (sortCol === col) sortDir = -sortDir; else { sortCol = col; sortDir = 1; }
      rows.sort(function (r1, r2) { return cmp(cellVal(r1, col), cellVal(r2, col)) * sortDir; });
      rows.forEach(function (r) { tbody.appendChild(r); });   // reorder DOM
      // header arrows
      for (var i = 0; i < headCells.length; i++) {
        var ar = headCells[i].querySelector('.tk-arrow');
        if (ar) ar.textContent = (i === col) ? (sortDir > 0 ? ' ▲' : ' ▼') : '';
      }
      page = 1; renderPage();
    }
    for (var i = 0; i < headCells.length; i++) {
      var th = headCells[i];
      if (th.hasAttribute('data-nosort') || !(th.textContent || '').trim()) continue;
      th.classList.add('tk-sortable');
      var arrow = document.createElement('span'); arrow.className = 'tk-arrow';
      th.appendChild(arrow);
      (function (col) { th.addEventListener('click', function () { applySort(col); }); })(i);
    }

    buildPager();
    renderPage();
  }

  document.addEventListener('DOMContentLoaded', function () {
    document.querySelectorAll('table[data-tablekit]').forEach(enhance);
  });
})();
