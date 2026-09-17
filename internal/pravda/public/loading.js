(function () {
  'use strict';
  if (window.PravdaLoading) return;
  const pending = new Map();
  const requests = new WeakMap();
  const busy = new Map();
  let panel, message, failure = '', navigation;
  function render() {
    if (!panel?.isConnected) {
      panel = document.createElement('div'); panel.className = 'pravda-loading';
      panel.setAttribute('role', 'status'); panel.setAttribute('aria-live', 'polite');
      const indicator = document.createElement('span'); indicator.className = 'pravda-loading-indicator'; indicator.setAttribute('aria-hidden', 'true');
      message = document.createElement('span');
      const dismiss = document.createElement('button'); dismiss.type = 'button'; dismiss.textContent = 'Dismiss'; dismiss.className = 'pravda-loading-dismiss';
      dismiss.addEventListener('click', () => { failure = ''; render(); });
      panel.append(indicator, message, dismiss); document.body.append(panel);
    }
    panel.hidden = !pending.size && !failure;
    panel.classList.toggle('pravda-loading-error', !pending.size && !!failure);
    message.textContent = pending.size ? [...pending.values()].at(-1) : failure;
  }
  function start(label = 'Loading…', target) {
    const token = {};
    failure = ''; pending.set(token, label);
    if (target) {
      if (!busy.has(target)) busy.set(target, {count: 0, previous: target.getAttribute('aria-busy')});
      busy.get(target).count++; target.setAttribute('aria-busy', 'true');
    }
    render();
    return function finish(error) {
      if (!pending.delete(token)) return;
      if (target && busy.has(target)) {
        const state = busy.get(target);
        if (--state.count === 0) {
          if (state.previous === null) target.removeAttribute('aria-busy'); else target.setAttribute('aria-busy', state.previous);
          busy.delete(target);
        }
      }
      if (error) failure = error;
      render();
    };
  }
  function navigate(label) { navigation?.(); navigation = start(label, document.querySelector('main')); }
  window.PravdaLoading = {start, navigate};
  document.addEventListener('htmx:beforeRequest', event => {
    const detail = event.detail;
    if (event.defaultPrevented || !detail.xhr) return;
    const finish = start('Loading…', detail.target);
    requests.set(detail.xhr, finish);
    queueMicrotask(() => { if (event.defaultPrevented) { finish(); requests.delete(detail.xhr); } });
  });
  document.addEventListener('htmx:afterRequest', event => {
    const detail = event.detail;
    requests.get(detail.xhr)?.(detail.failed && detail.xhr.status ? 'Could not load this view. Please try again.' : undefined);
    requests.delete(detail.xhr);
  });
  ['htmx:sendError', 'htmx:timeout', 'htmx:responseError', 'htmx:swapError'].forEach(name => document.addEventListener(name, event => {
    const xhr = event.detail.xhr;
    const finish = requests.get(xhr);
    if (finish) { finish('Could not load this view. Please try again.'); requests.delete(xhr); }
    else { failure = 'Could not load this view. Please try again.'; render(); }
  }));
  document.addEventListener('click', event => {
    const anchor = event.target.closest?.('a[href]');
    if (!anchor || event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey || anchor.hasAttribute('download') || (anchor.target && anchor.target !== '_self')) return;
    const url = new URL(anchor.href, location.href);
    if (!['http:', 'https:'].includes(url.protocol) || (url.pathname === location.pathname && url.search === location.search && url.hash)) return;
    queueMicrotask(() => { if (!event.defaultPrevented) navigate('Loading page…'); });
  });
  document.addEventListener('submit', event => {
    const form = event.target;
    if (form.method === 'dialog' || (form.target && form.target !== '_self')) return;
    queueMicrotask(() => { if (!event.defaultPrevented) navigate('Applying changes…'); });
  });
  window.addEventListener('keydown', event => { if (event.key === 'Escape' && navigation) { navigation(); navigation = undefined; } });
  window.addEventListener('pageshow', () => {
    pending.clear(); failure = ''; navigation = undefined;
    busy.forEach((state, target) => { if (state.previous === null) target.removeAttribute('aria-busy'); else target.setAttribute('aria-busy', state.previous); });
    busy.clear(); render();
  });
})();
