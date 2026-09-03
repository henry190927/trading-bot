// Performance-group tab switcher: each .pf-tab[data-tab="X"] shows the
// .pf-tab-panel[data-panel="X"] inside the same .pf-group.
//
// Shared by /journal and /ops/autotrade (the perfPanels partial). It scopes to
// each .pf-group independently rather than the first one on the page, because
// /journal has several pf-groups and only one of them carries tabs.
//
// The remembered tab is keyed per page path: "equity" on the journal and
// "calendar" on the auto panel are different intents, and one localStorage key
// for both meant opening either page reset the other.
(function () {
    var groups = document.querySelectorAll('.pf-group');
    if (!groups.length) return;
    var key = 'pf-tab:' + location.pathname;

    groups.forEach(function (group) {
        var tabs = group.querySelectorAll('.pf-tab');
        var panels = group.querySelectorAll('.pf-tab-panel');
        if (!tabs.length || !panels.length) return;

        function activate(name) {
            tabs.forEach(function (t) {
                t.classList.toggle('active', t.dataset.tab === name);
            });
            panels.forEach(function (p) {
                p.classList.toggle('active', p.dataset.panel === name);
            });
            try { localStorage.setItem(key, name); } catch (e) {}
        }

        tabs.forEach(function (t) {
            t.addEventListener('click', function () { activate(t.dataset.tab); });
        });

        var saved = null;
        try { saved = localStorage.getItem(key); } catch (e) {}
        // Only restore a tab this group actually has — a stale key from an
        // older layout must not blank every panel.
        if (saved && Array.from(tabs).some(function (t) { return t.dataset.tab === saved; })) {
            activate(saved);
        }
    });
})();
