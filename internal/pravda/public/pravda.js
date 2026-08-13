// pravda.js -- progressive enhancement only. The page is fully usable with JS
// disabled. This script adds:
//   - body.js class so CSS can reveal JS-only affordances (scroll chip)
//   - .scroll-chip visibility toggling based on timeline scroll position
//   - copy-to-clipboard for .copy-btn buttons (with a floating toast)
//   - hot reload of the page when the server broadcasts 'reload' on /ws
(function () {
  // <html class="js"> -- body gets replaced on hx-boost swaps, but <html>
  // does not, so keeping the class marker on the root element makes it
  // persist across htmx navigations.
  document.documentElement.classList.add('js');

  // --- cookie-driven settings ------------------------------------------

  // Two bool toggles; default on when the cookie is missing (same as
  // server-side `readBool(..., default=true)`).
  const getBool = (name) => {
    const m = document.cookie.match(new RegExp('(?:^|;\\s*)' + name + '=([^;]*)'));
    if (!m) return true;
    return m[1] === '1';
  };
  const jumpLines = () => getBool('cw_jump_lines');
  const jumpFile  = () => getBool('cw_jump_file');

  // --- clipboard ---------------------------------------------------------

  function showToast(anchor, text = 'Copied') {
    const r = anchor.getBoundingClientRect();
    const t = document.createElement('div');
    t.className = 'copy-toast';
    t.textContent = text;
    t.style.left = (r.left + r.width / 2) + 'px';
    t.style.top = r.top + 'px';
    document.body.appendChild(t);
    setTimeout(() => t.remove(), 950);
  }

  async function copyText(s) {
    try { await navigator.clipboard.writeText(s); return true; }
    catch (_) {
      const ta = document.createElement('textarea');
      ta.value = s; ta.style.position = 'fixed'; ta.style.opacity = '0';
      document.body.appendChild(ta); ta.select();
      try { document.execCommand('copy'); return true; }
      finally { ta.remove(); }
    }
  }

  document.addEventListener('click', (e) => {
    const btn = e.target.closest('.copy-btn');
    if (!btn) return;
    e.preventDefault();
    e.stopPropagation();
    const text = btn.dataset.copy ?? '';
    copyText(text).then(() => showToast(btn));
  });

  // Auto-submit on change for .auto-submit checkboxes. Inline onchange
  // handlers would be simpler but the CSP forbids inline script.
  document.addEventListener('change', (e) => {
    const el = e.target;
    if (el.matches && el.matches('input.auto-submit')) el.form?.submit();
  });

  // --- scroll chip -------------------------------------------------------

  function wireScrollChip() {
    const t = document.querySelector('.timeline');
    const chip = document.querySelector('.scroll-chip');
    if (!t || !chip) return;
    if (t._wired) return;  // OOB swaps reuse the node -- skip re-wiring
    t._wired = true;
    const update = () => {
      const gap = t.scrollHeight - t.scrollTop - t.clientHeight;
      chip.classList.toggle('visible', gap > 120);
    };
    t.addEventListener('scroll', update, { passive: true });
    new ResizeObserver(update).observe(t);
    chip.addEventListener('click', () => { t.scrollTop = t.scrollHeight; });
    update();
  }
  wireScrollChip();
  // After an htmx boost swap or ws OOB swap, the .timeline is a new DOM
  // node and needs fresh listeners. Events bubble up to <html>, which
  // survives the swap.
  document.documentElement.addEventListener('htmx:afterSettle', wireScrollChip);

  // After a live WS fragment applies:
  // 1. Deduplicate entries that were appended via beforeend when the
  //    client already had them from a later page load (rare race).
  // 2. Scroll to newest if the user has the setting on (default).
  document.documentElement.addEventListener('htmx:oobAfterSwap', () => {
    var root = document.getElementById('timeline-root');
    if (root) {
      var seen = {};
      for (var el = root.firstElementChild; el; ) {
        var next = el.nextElementSibling;
        if (el.id) {
          if (seen[el.id]) el.remove();
          else seen[el.id] = true;
        }
        el = next;
      }
    }
    if (!jumpLines()) return;
    var t = document.querySelector('.timeline');
    if (t) t.scrollTop = t.scrollHeight;
  });

  // --- search / filter ---------------------------------------------------

  function applySearch(q) {
    const lower = q.toLowerCase().trim();
    document.querySelectorAll('[data-search]').forEach(el => {
      el.style.display = (!lower || el.dataset.search.toLowerCase().includes(lower)) ? '' : 'none';
    });
    // Keep all search inputs in sync
    document.querySelectorAll('.search-input').forEach(el => {
      if (el.value !== q) el.value = q;
    });
  }

  document.addEventListener('input', (e) => {
    if (e.target.matches && e.target.matches('.search-input')) {
      const q = e.target.value;
      sessionStorage.setItem('pravda_search', q);
      applySearch(q);
    }
  });

  // Restore search after HTMX navigation
  document.documentElement.addEventListener('htmx:afterSettle', () => {
    const saved = sessionStorage.getItem('pravda_search') || '';
    if (saved) applySearch(saved);
  });

  // --- asset / log reload ------------------------------------------------

  // Swap only the main-content region. Server returns the main-content
  // fragment on HX-Request (pages.go:handleSession) so the 4.8 MB sidebar
  // stays untouched.
  function loadMain(target) {
    if (typeof htmx === 'undefined') { location.href = target; return; }
    htmx.ajax('GET', target, { target: 'main.main-content', swap: 'innerHTML' });
  }

  function setSidebarActive(target, path) {
    document.querySelectorAll('.session-wrap.active-wrap, .file-item.active')
      .forEach((el) => el.classList.remove('active-wrap', 'active'));
    let item = document.querySelector('.file-item[href="' + target + '"]');
    if (item) {
      item.classList.add('active');
      const wrap = item.closest('.session-wrap');
      if (wrap) wrap.classList.add('active-wrap');
      return;
    }
    // Not in sidebar yet -- prepend a minimal row. Full metadata arrives
    // on the next page load; until then the user sees the session exists.
    const parts = path.split('/');
    const name = parts[parts.length - 1] || path;
    const folder = parts.length >= 2 ? parts[parts.length - 2] : '';
    const list = document.querySelector('.file-list');
    if (!list) return;
    const wrap = document.createElement('div');
    wrap.className = 'session-wrap active-wrap';
    wrap.dataset.search = name + ' ' + folder + ' ' + path;
    const a = document.createElement('a');
    a.className = 'file-item session active';
    a.href = target;
    a.title = path;
    const title = document.createElement('div');
    title.className = 'file-title';
    title.textContent = name;
    const meta = document.createElement('div');
    meta.className = 'file-meta';
    const fs = document.createElement('span');
    fs.className = 'file-folder';
    fs.textContent = folder;
    const ts = document.createElement('span');
    ts.textContent = 'just now';
    meta.appendChild(fs); meta.appendChild(ts);
    a.appendChild(title); a.appendChild(meta);
    wrap.appendChild(a);
    list.insertBefore(wrap, list.firstChild);
  }

  window.addEventListener('popstate', () => {
    loadMain(location.pathname + location.search);
  });

  function connectWS() {
    const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
    const ws = new WebSocket(proto + '//' + location.host + '/ws');
    ws.addEventListener('message', (ev) => {
      const data = String(ev.data || '');
      if (data === 'reload') {
        location.reload();
      } else if (data.startsWith('new-session:') && jumpFile()) {
        const path = data.slice('new-session:'.length);
        const target = '/session/' + path;
        if (location.pathname === target) return;
        loadMain(target);
        history.pushState({}, '', target);
        setSidebarActive(target, path);
      }
    });
    ws.addEventListener('close', () => setTimeout(connectWS, 2000));
    ws.addEventListener('error', () => ws.close());
  }
  connectWS();
})();
