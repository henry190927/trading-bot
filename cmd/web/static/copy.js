// Click-to-copy via event delegation: tap any .copy-btn to write its
// data-copy value (or text content) to the clipboard, with a 1s "✓ copied"
// flash for tactile feedback on mobile.
document.addEventListener('click', function (e) {
    var btn = e.target.closest('.copy-btn');
    if (!btn) return;
    e.preventDefault();
    var value = btn.dataset.copy || btn.textContent.trim();
    var done = function () {
        var original = btn.textContent;
        btn.textContent = '✓ ' + value;
        btn.classList.add('copied');
        setTimeout(function () {
            btn.textContent = original;
            btn.classList.remove('copied');
        }, 1000);
    };
    if (navigator.clipboard && window.isSecureContext) {
        navigator.clipboard.writeText(value).then(done).catch(function (err) {
            console.error('clipboard write failed', err);
            fallback(value, done);
        });
    } else {
        fallback(value, done);
    }
});

// Fallback for non-HTTPS or older Safari: use a hidden textarea + execCommand.
function fallback(value, done) {
    var ta = document.createElement('textarea');
    ta.value = value;
    ta.style.position = 'fixed';
    ta.style.opacity = '0';
    document.body.appendChild(ta);
    ta.select();
    try {
        document.execCommand('copy');
        done();
    } catch (e) {
        console.error('fallback copy failed', e);
    } finally {
        document.body.removeChild(ta);
    }
}
