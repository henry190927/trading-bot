/* Theme: dark (default) | light. Colours only — the layout never changes
   with the theme. The <head> inline script applied the saved value before
   paint; this wires the top bar's [data-theme-set] buttons and keeps the
   browser-chrome colour in step.

   Saved values from the old pair (slate / industrial) are not "light", so
   they fall through to dark — the CSS default — without a migration. */
(function () {
  var KEY = 'tw-theme';
  var root = document.documentElement;
  var META = { dark: '#0e1013', light: '#f5f3ee' };

  function cur() { return root.getAttribute('data-theme') === 'light' ? 'light' : 'dark'; }

  function mark() {
    var t = cur();
    document.querySelectorAll('[data-theme-set]').forEach(function (b) {
      var on = b.getAttribute('data-theme-set') === t;
      b.classList.toggle('on', on);
      b.setAttribute('aria-pressed', on ? 'true' : 'false');
    });
    var m = document.querySelector('meta[name="theme-color"]');
    if (m) m.setAttribute('content', META[t]);
  }

  function apply(t) {
    root.setAttribute('data-theme', t);
    try { localStorage.setItem(KEY, t); } catch (e) {}
    mark();
    // Canvas charts paint their colours once; they listen for this.
    document.dispatchEvent(new CustomEvent('themechange', { detail: t }));
  }
  window.themeToggle = function () { apply(cur() === 'light' ? 'dark' : 'light'); };

  document.addEventListener('DOMContentLoaded', function () {
    mark();
    document.querySelectorAll('[data-theme-set]').forEach(function (b) {
      b.addEventListener('click', function () { apply(b.getAttribute('data-theme-set')); });
    });
  });
})();
