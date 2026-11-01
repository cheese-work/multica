// Static check for the busy jump button. Injected at the end of <body> of a temp copy of A/B/C.html
// and run in headless Chrome with --dump-dom. Not part of the mockup.
(function () {
  const out = {};
  const probe = (b) => {
    let clicks = 0;
    b.addEventListener('click', () => clicks++);
    b.focus();
    const canFocus = document.activeElement === b;
    b.click();
    const frame = b.closest('.fr');
    const tabbable = [...frame.querySelectorAll('button,[href],input,select,textarea,[tabindex]')].filter((e) => !e.matches(':disabled'));
    const d = document.getElementById(b.getAttribute('aria-describedby') || '');
    return { disabledAttr: b.hasAttribute('disabled'), matchesDisabled: b.matches(':disabled'), ariaBusy: b.getAttribute('aria-busy'), canFocus, clicksReceived: clicks, inTabOrder: tabbable.includes(b), describedBy: d ? d.textContent : null };
  };
  out.busyJump = [...document.querySelectorAll('button[data-testid="chat-jump-to-latest"][aria-busy="true"]')].map(probe);
  out.disabledRetry = [...document.querySelectorAll('button.rt[disabled]')].map(probe).map((r) => ({ disabledAttr: r.disabledAttr, canFocus: r.canFocus, clicksReceived: r.clicksReceived, inTabOrder: r.inTabOrder }));
  const en = document.querySelector('button.jb:not([disabled])');
  en.focus();
  const cs = getComputedStyle(en);
  out.enabledJump = { canFocus: document.activeElement === en, matchesFocusVisible: en.matches(':focus-visible'), outline: cs.outlineStyle + ' ' + cs.outlineWidth + ' ' + cs.outlineColor };
  out.enabledFocusExamples = document.querySelectorAll('.jb.focus').length;
  const pre = document.createElement('pre');
  pre.id = 'disabled-check';
  pre.textContent = JSON.stringify(out);
  document.body.appendChild(pre);
})();
