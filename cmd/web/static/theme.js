/* Theme switcher — slate (default GitHub-dark) <-> industrial (amber).
   The <head> inline script already applied the saved theme before paint;
   this only builds the toggle control and persists clicks. */
(function () {
  var KEY = 'tw-theme';
  var root = document.documentElement;

  function cur()  { return root.getAttribute('data-theme') === 'industrial' ? 'industrial' : 'slate'; }
  function label(t) { return t === 'slate' ? '◑ Slate' : '◐ Industrial'; }

  // Keep the browser-chrome / PWA status-bar color in sync with the theme
  // (the one color that can't be a CSS var). slate = GitHub-dark, industrial
  // = warm charcoal.
  function applyMeta(t) {
    var m = document.querySelector('meta[name="theme-color"]');
    if (m) m.setAttribute('content', t === 'industrial' ? '#0f0b07' : '#0d1117');
  }

  function apply(t) {
    root.setAttribute('data-theme', t);
    applyMeta(t);
    try { localStorage.setItem(KEY, t); } catch (e) {}
  }

  document.addEventListener('DOMContentLoaded', function () {
    applyMeta(cur());   // sync meta to the theme the inline head script applied
    var btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'theme-toggle';
    btn.textContent = label(cur());
    btn.title = '切換配色主題 (工業風 / 原深藍灰)';
    btn.addEventListener('click', function () {
      var next = cur() === 'slate' ? 'industrial' : 'slate';
      apply(next);
      btn.textContent = label(next);
    });

    // Prefer the top bar; fall back to a floating control on pages
    // without one (e.g. the full-screen chart view).
    var bar = document.querySelector('#sidebar-foot') || document.querySelector('header.topbar');
    if (bar) {
      bar.appendChild(btn);
    } else {
      btn.classList.add('floating');
      document.body.appendChild(btn);
    }
  });
})();
