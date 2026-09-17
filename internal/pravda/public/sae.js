(function () {
  'use strict';
  if (window.PravdaSAE) return;
  const palette = ['#56b4e9', '#e69f00', '#cc79a7', '#009e73', '#f0e442', '#d55e00', '#8c91f2', '#8bd5ca'];
  const instances = new WeakMap();
  const active = new Set();
  function el(tag, cls, text) {
    const node = document.createElement(tag);
    if (cls) node.className = cls;
    if (text !== undefined) node.textContent = text;
    return node;
  }
  function button(text, action, cls) {
    const node = el('button', cls, text); node.type = 'button'; node.addEventListener('click', action); return node;
  }
  function link(text, href) { const n = el('a', '', text); n.href = href; n.setAttribute('hx-boost', 'false'); return n; }
  function read(storage, key) { try { return JSON.parse(window[storage].getItem(key)); } catch (_) { return null; } }
  function write(storage, key, value) { try { window[storage].setItem(key, JSON.stringify(value)); return true; } catch (_) { return false; } }
  class Workbench {
    constructor(root, data) {
      this.root = root; this.settings = root.dataset.saeSettings === 'true'; this.data = data;
      this.query = ''; this.selectedMessage = null; this.expanded = false; this.setup(); this.render();
    }
    setup() {
      this.features = (this.data.features || []).slice().sort((a, b) => b.count - a.count || a.id - b.id);
      this.messages = this.data.messages || []; this.byFeature = new Map(this.features.map(f => [String(f.id), f]));
      this.key = 'pravda.sae.v1.' + (this.data.dictionary?.id || 'none');
      this.sessionKey = this.key + '.' + (this.data.session || location.pathname);
      const colors = {}; this.features.slice().sort((a, b) => a.id - b.id).forEach((f, i) => { colors[String(f.id)] = palette[i % palette.length]; });
      this.features.slice(0, 3).forEach((f, i) => { colors[String(f.id)] = palette[i]; });
      const defaults = {selected: this.features.slice(0, 3).map(f => String(f.id)), colors, threshold: 35, mode: 'tracks'};
      const saved = read('localStorage', this.key);
      if (!saved) write('localStorage', this.key, defaults);
      const session = this.settings ? null : read('sessionStorage', this.sessionKey);
      this.prefs = Object.assign(defaults, saved || {}, session || {});
      this.prefs.selected = Array.isArray(this.prefs.selected) ? this.prefs.selected.map(String).filter(id => this.byFeature.has(id)) : defaults.selected;
      this.prefs.colors = Object.assign(colors, this.prefs.colors || {});
      this.prefs.threshold = Math.min(100, Math.max(0, Number(this.prefs.threshold) || 0));
      if (!['tracks', 'overview', 'heatmap'].includes(this.prefs.mode)) this.prefs.mode = 'tracks';
    }
    color(id) { const c = this.prefs.colors[String(id)]; return /^#[0-9a-f]{6}$/i.test(c || '') ? c : palette[Math.abs(Number(id) || 0) % palette.length]; }
    save() {
      const ok = write(this.settings ? 'localStorage' : 'sessionStorage', this.settings ? this.key : this.sessionKey, this.prefs);
      if (this.notice) this.notice.textContent = ok ? (this.settings ? 'Defaults saved in this browser for this dictionary.' : 'Selection saved for this session in this tab.') : 'Browser storage is unavailable. Changes apply until this page reloads.';
    }
    toggle(id) { id = String(id); this.prefs.selected = this.prefs.selected.includes(id) ? this.prefs.selected.filter(x => x !== id) : [...this.prefs.selected, id]; this.save(); this.renderSelection(); this.renderResults(); this.renderMap(); }
    render() {
      clearTimeout(this.pollTimer); this.root.replaceChildren(); this.root.classList.toggle('sae-collapsed', this.expanded);
      const header = el('div', 'sae-toolbar');
      const state = this.data.status || {};
      header.append(el('strong', '', this.settings ? 'Current status' : 'SAE'), el('span', 'sae-state sae-state-' + state.state, state.state || 'unavailable'));
      header.append(button(this.settings ? 'Refresh status' : 'Refresh', () => { if (this.data.refresh_url) window.PravdaSAE.refresh(); else { window.PravdaLoading?.navigate('Refreshing status…'); location.reload(); } }));
      const coverage = el('span', 'sae-muted sae-coverage', this.settings ? `${state.records || 0} saved records` : `${state.saved ?? state.tagged ?? 0} / ${state.eligible || 0} text events saved`);
      coverage.dataset.compact = `${state.saved ?? state.tagged ?? 0} / ${state.eligible || 0} saved`;
      coverage.title = coverage.textContent; header.append(coverage);
      if (!this.settings) { header.append(link('Settings & status', this.data.settings_url || '/sae/settings')); header.append(button(this.expanded ? 'Expand' : 'Collapse', e => { this.expanded = !this.expanded; this.root.classList.toggle('sae-collapsed', this.expanded); this.renderMap(); e.currentTarget.textContent = this.expanded ? 'Expand' : 'Collapse'; })); }
      this.root.append(header);
      if (state.eligible > 0) { const progress = el('p', 'sae-notice', state.detail); progress.setAttribute('role', 'status'); this.root.append(progress); }
      if (this.settings) this.renderStatus();
      this.body = el('div', 'sae-body'); this.root.append(this.body);
      if (this.settings) { this.body.append(el('h2', '', 'Feature display'), el('p', 'sae-muted', 'Defaults apply to this dictionary in this browser. Conversation selections override them for the current tab.')); }
      this.selection = el('div', 'sae-selection'); this.body.append(this.selection); this.renderSelection();
      this.picker = el('details', 'sae-picker'); const summary = el('summary', '', 'Select features'); this.picker.append(summary);
      this.search = el('input', 'sae-search'); this.search.type = 'search'; this.search.placeholder = 'Search feature names, descriptions, or numbers'; this.search.setAttribute('aria-label', 'Search features'); this.search.autocomplete = 'off';
      this.search.addEventListener('input', () => { this.query = this.search.value.toLowerCase(); this.renderResults(); });
      this.picker.addEventListener('toggle', () => { if (this.picker.open) this.search.focus(); });
      this.picker.addEventListener('keydown', e => {
        if (e.key === 'Escape') { this.picker.open = false; summary.focus(); }
        if (e.key === 'ArrowDown' || e.key === 'ArrowUp') { const choices = [...this.picker.querySelectorAll('input[type=checkbox]')]; const i = choices.indexOf(document.activeElement); const next = e.key === 'ArrowDown' ? Math.min(i + 1, choices.length - 1) : Math.max(i - 1, 0); if (choices[next]) { e.preventDefault(); choices[next].focus(); } }
      });
      this.results = el('div', 'sae-options'); this.results.setAttribute('aria-label', 'Features and saved activation counts'); const bulk = el('div', 'sae-picker-actions');
      bulk.append(button('Select results', () => { this.prefs.selected = [...new Set([...this.prefs.selected, ...this.matches().map(f => String(f.id))])]; this.save(); this.renderSelection(); this.renderResults(); this.renderMap(); }), button('Clear selection', () => { this.prefs.selected = []; this.save(); this.renderSelection(); this.renderResults(); this.renderMap(); }), button('Done', () => { this.picker.open = false; summary.focus(); }));
      this.picker.append(this.search, this.results, bulk); this.body.append(this.picker); this.renderResults();
      const controls = el('div', 'sae-controls'); const modeLabel = el('label', '', 'View '); const mode = el('select'); mode.setAttribute('aria-label', 'Visualization style');
      [['tracks', 'Separate tracks'], ['overview', 'Compact overview'], ['heatmap', 'Heatmap']].forEach(([value, label]) => { const o = el('option', '', label); o.value = value; o.selected = this.prefs.mode === value; mode.append(o); });
      mode.addEventListener('change', () => { this.prefs.mode = mode.value; this.save(); this.renderMap(); }); modeLabel.append(mode);
      const thresholdLabel = el('label', '', 'Minimum value '); const threshold = el('input'); threshold.type = 'range'; threshold.min = 0; threshold.max = 100; threshold.value = this.prefs.threshold; threshold.setAttribute('aria-label', 'Minimum displayed activation'); const output = el('output', '', `${this.prefs.threshold}%`);
      threshold.addEventListener('input', () => { this.prefs.threshold = Number(threshold.value); output.textContent = `${threshold.value}%`; this.save(); this.renderMap(); }); thresholdLabel.append(threshold, output);
      controls.append(modeLabel, thresholdLabel);
      const options = el('details', 'sae-display-options'); options.append(el('summary', '', 'Display options'), controls);
      if (this.settings) this.body.append(controls); else this.body.append(options);
      if (this.settings) this.body.append(el('p', 'sae-muted', 'Values below the minimum are hidden, but the message remains processed. Colors can be changed beside each selected feature.'));
      this.map = el('div', 'sae-map'); this.body.append(this.map);
      this.inspector = el('section', 'sae-inspector'); this.inspector.hidden = true; this.inspector.setAttribute('aria-label', 'Message activation inspector'); this.body.append(this.inspector);
      this.notice = el('p', 'sae-notice'); this.notice.setAttribute('role', 'status'); this.body.append(this.notice);
      const actions = el('div', 'sae-actions');
      actions.append(button(this.settings ? 'Restore defaults' : 'Use dictionary defaults', () => { if (this.settings) { try { localStorage.removeItem(this.key); } catch (_) {} } else { try { sessionStorage.removeItem(this.sessionKey); } catch (_) {} } this.setup(); this.render(); }));
      if (!this.settings) actions.append(button('Save as dictionary defaults', () => { this.notice.textContent = write('localStorage', this.key, this.prefs) ? 'Dictionary defaults saved in this browser.' : 'Browser storage is unavailable.'; }));
      if (this.settings) this.body.append(actions); else options.append(actions); this.renderMap();
      if (this.settings) this.renderExplanation();
      if (this.root.dataset.saeEvent) { const message = this.messages.find(m => m.index === Number(this.root.dataset.saeEvent)); if (message) this.inspect(message); }
      this.schedulePoll();
    }
    renderStatus() {
      const d = this.data.dictionary || {}, state = this.data.status || {}; const section = el('section', 'sae-status-card');
      if (this.data.session) section.append(el('p', '', `${state.tagged || 0} / ${state.eligible || 0} text events have saved results.`));
      section.append(el('p', '', state.detail || 'No processing status is available.'));
      section.append(el('p', 'sae-muted', `Selected dictionary: ${d.name || d.id || 'none'} · ${d.features || 0} features${state.updated_at ? ' · Imported ' + state.updated_at : ''}`));
      const label = el('label', '', 'Find a dictionary '); const search = el('input', 'sae-search'); search.type = 'search'; search.placeholder = 'Search dictionaries'; label.append(search);
      const form = el('form', 'sae-dictionary-form'); form.method = 'post'; form.action = '/settings/sae-encoder';
      const apply = el('button', '', 'Use dictionary'); apply.type = 'submit';
      const empty = el('p', 'sae-muted', 'No matching dictionaries.'); empty.hidden = true; empty.setAttribute('role', 'status');
      const select = el('select'); select.name = 'encoder'; select.setAttribute('aria-label', 'Dictionary'); const dictionaries = this.data.dictionaries || [];
      function options(query) { select.replaceChildren(); dictionaries.filter(item => `${item.name} ${item.id} ${item.created_at}`.toLowerCase().includes(query)).forEach(item => { const o = el('option', '', `${item.name || item.id} · ${item.features || 0} features · ${item.created_at || ''}`); o.value = item.id; o.selected = item.id === d.id; select.append(o); }); apply.disabled = select.options.length === 0; select.disabled = apply.disabled; empty.hidden = !apply.disabled; }
      options(''); search.addEventListener('input', () => options(search.value.toLowerCase()));
      form.append(select, apply, empty);
      section.append(label, form, el('p', 'sae-muted', 'Switching changes the features and saved values you explore. Opening a conversation requests missing values when scoring is available. Each dictionary keeps its own display defaults.'), link('Browse feature descriptions and coverage', this.data.features_url || '/sae/features'));
      this.root.append(section);
    }
    matches() { return this.features.filter(f => `${f.label} ${f.description || ''} ${f.id}`.toLowerCase().includes(this.query)); }
    renderResults() {
      if (!this.results) return; this.results.replaceChildren(); const matches = this.matches();
      if (!matches.length) this.results.append(el('p', 'sae-muted', 'No matching features.'));
      matches.forEach(f => { const label = el('label', 'sae-option'); const input = el('input'); input.type = 'checkbox'; input.checked = this.prefs.selected.includes(String(f.id)); input.addEventListener('change', () => { const id = String(f.id); this.toggle(id); const replacement = [...this.results.querySelectorAll('input')].find(n => n.value === id); if (replacement) replacement.focus(); }); input.value = String(f.id); const dot = el('span', 'sae-dot'); dot.style.backgroundColor = this.color(f.id); const text = el('span', 'sae-option-text', f.label); if (f.description) text.title = f.description; label.append(input, dot, text, el('span', 'sae-muted', String(f.count || 0))); this.results.append(label); });
    }
    renderSelection() {
      this.selection.replaceChildren(); this.prefs.selected.forEach(id => { const f = this.byFeature.get(id); if (!f) return; const chip = el('span', 'sae-chip'); chip.style.setProperty('--sae-color', this.color(id)); chip.append(el('span', 'sae-dot'), el('span', '', f.label)); if (this.settings) { const color = el('input', 'sae-color'); color.type = 'color'; color.value = this.color(id); color.setAttribute('aria-label', 'Color for ' + f.label); color.addEventListener('input', () => { this.prefs.colors[id] = color.value; this.save(); chip.style.setProperty('--sae-color', color.value); this.renderResults(); this.renderMap(); }); chip.append(color); } const remove = button('×', () => this.toggle(id)); remove.setAttribute('aria-label', 'Remove ' + f.label); chip.append(remove); this.selection.append(chip); });
      if (!this.prefs.selected.length) this.selection.append(el('span', 'sae-muted', 'Select features to explore their activations.'));
    }
    value(message, id) { return (message.values || []).find(v => String(v.feature) === String(id))?.value || 0; }
    renderMap() {
      if (!this.map) return; this.map.replaceChildren(); const messages = this.messages;
      if (!messages.length) { this.map.append(el('p', 'sae-muted', this.settings ? 'Live preview uses the selected colors above. Open a conversation to see actual activation tracks.' : 'No text events are available in this conversation.')); if (this.settings) this.renderPreview(); return; }
      const modes = (this.expanded || this.prefs.mode === 'overview') ? ['overview'] : this.prefs.selected;
      modes.forEach(id => { const row = el('div', 'sae-track'); const name = id === 'overview' ? 'Overview' : this.byFeature.get(id)?.label; const label = el('span', 'sae-track-label', name); label.title = name; row.append(label); const plot = el('div', 'sae-plot'); plot.setAttribute('role', 'group'); plot.setAttribute('aria-label', `${name} activations, arrow keys move between messages`);
        messages.forEach((message, i) => {
          let feature = id, value = this.value(message, id);
          if (id === 'overview') { feature = this.prefs.selected.reduce((best, f) => this.value(message, f) > this.value(message, best) ? f : best, this.prefs.selected[0]); value = this.value(message, feature); }
          const visible = message.processed && value >= this.prefs.threshold && value > 0;
          const cell = button('', () => { this.inspect(message); this.jump(message); }, 'sae-mark'); cell.tabIndex = i === 0 ? 0 : -1; cell.dataset.index = message.index;
          const title = `Message ${message.index} · ${message.kind || ''} · ${!message.processed ? (message.state === 'pending' ? 'Queued for SAE scoring' : message.state === 'running' ? 'SAE scoring running' : message.retrying ? 'Awaiting SAE retry' : message.state === 'failed' ? 'SAE scoring failed' : 'No stored result') : visible ? (this.byFeature.get(String(feature))?.label || 'Feature') + ' ' + value + '%' : 'No selected activation above threshold'}`;
          cell.title = title; cell.setAttribute('aria-label', title); cell.classList.toggle('sae-unprocessed', !message.processed);
          const fill = el('span'); fill.style.backgroundColor = visible ? this.color(feature) : 'transparent'; fill.style.height = this.prefs.mode === 'heatmap' ? '100%' : Math.max(8, value) + '%'; fill.style.opacity = this.prefs.mode === 'heatmap' ? Math.max(.1, value / 100) : '1'; cell.append(fill);
          cell.addEventListener('focus', () => this.inspect(message)); cell.addEventListener('mouseenter', () => this.inspect(message));
          cell.addEventListener('keydown', e => { let target; if (e.key === 'ArrowRight') target = cell.nextElementSibling; if (e.key === 'ArrowLeft') target = cell.previousElementSibling; if (e.key === 'Home') target = plot.firstElementChild; if (e.key === 'End') target = plot.lastElementChild; if (target) { e.preventDefault(); cell.tabIndex = -1; target.tabIndex = 0; target.focus(); } });
          plot.append(cell);
        }); row.append(plot); this.map.append(row);
      });
      if (!modes.length) this.map.append(el('p', 'sae-muted', 'Choose at least one feature.'));
      this.map.append(el('div', 'sae-map-caption', `Message order → · ${messages.length} text events · striped = no stored result · empty = below threshold or no selected activation`));
      this.observe(); this.syncTags();
    }
    syncTags() {
      if (this.settings) return;
      this.messages.forEach(message => {
        const article = document.getElementById('e-' + message.index);
        const content = article?.querySelector(':scope > .content'); if (!content) return;
        let tags = content.querySelector(':scope > .sae-tags');
        if (!tags) { tags = el('div', 'sae-tags'); content.prepend(tags); }
        tags.replaceChildren();
        (message.values || []).filter(v => this.prefs.selected.includes(String(v.feature)) && v.value >= this.prefs.threshold && v.value > 0).sort((a, b) => b.value - a.value).forEach(v => {
          const f = this.byFeature.get(String(v.feature));
          const tag = link((f?.label || 'Feature ' + v.feature) + ' ' + v.value + '%', '/sae/similar/' + encodeURIComponent(message.id) + '?encoder=' + encodeURIComponent(this.data.dictionary?.id || ''));
          tag.className = 'sae-tag'; tag.style.setProperty('--sae-color', this.color(v.feature)); tag.style.borderColor = this.color(v.feature); tag.style.color = this.color(v.feature); tag.title = f?.description || ''; tags.append(tag);
        });
        tags.hidden = !tags.children.length;
      });
    }
    renderPreview() {
      const preview = el('div', 'sae-preview'); preview.append(el('strong', '', 'Display preview — illustrative values'));
      this.prefs.selected.forEach(id => { const row = el('div', 'sae-track'); row.append(el('span', 'sae-track-label', this.byFeature.get(id)?.label)); const plot = el('div', 'sae-plot'); [12, 28, 56, 82, 43, 19, 65, 95, 34, 71].forEach(value => { const mark = el('span', 'sae-mark'); const fill = el('span'); fill.style.backgroundColor = value >= this.prefs.threshold ? this.color(id) : 'transparent'; fill.style.height = this.prefs.mode === 'heatmap' ? '100%' : value + '%'; fill.style.opacity = this.prefs.mode === 'heatmap' ? value / 100 : 1; mark.append(fill); plot.append(mark); }); row.append(plot); preview.append(row); }); this.map.append(preview);
    }
    inspect(message) {
      this.selectedMessage = message; this.inspector.hidden = false; this.inspector.replaceChildren(); const header = el('div', 'sae-toolbar'); header.append(el('strong', '', `Message ${message.index} · ${message.kind || ''}`), button('Close', () => { this.inspector.hidden = true; })); this.inspector.append(header);
      if (message.preview) this.inspector.append(el('p', 'sae-snippet', message.preview));
      if (!message.processed) this.inspector.append(el('p', 'sae-muted', message.detail || 'No stored result is available for this message in the selected dictionary.'));
      else if (!(message.values || []).length) this.inspector.append(el('p', 'sae-muted', 'A stored result exists with no activations for this message.'));
      (message.values || []).slice().sort((a, b) => b.value - a.value).forEach(v => { const row = el('div', 'sae-value-row'); const name = this.byFeature.get(String(v.feature))?.label || 'Feature ' + v.feature; row.append(el('span', '', name)); const bar = el('meter'); bar.min = 0; bar.max = 100; bar.value = v.value; bar.setAttribute('aria-label', name); bar.style.setProperty('--sae-color', this.color(v.feature)); row.append(bar, el('span', 'mono', v.value + '%')); this.inspector.append(row); });
      const actions = el('div', 'sae-actions'); actions.append(button('Jump to message', () => this.jump(message))); if (message.processed && message.id) actions.append(link('Find similar moments', '/sae/similar/' + encodeURIComponent(message.id) + '?encoder=' + encodeURIComponent(this.data.dictionary?.id || ''))); this.inspector.append(actions, el('p', 'sae-muted', 'Percentages are scaled activation values, not confidence. All stored values are shown here, including values below the display threshold.'));
    }
    jump(message) {
      if (this.root.dataset.saeEvent && this.data.session) { const url = new URL('/session/' + this.data.session, location.origin); url.searchParams.set('encoder', this.data.dictionary?.id || ''); url.searchParams.set('limit', '0'); url.hash = 'e-' + message.index; location.href = url.href; return; }
      const target = document.getElementById('e-' + message.index);
      if (target && target.getClientRects().length) { target.scrollIntoView({behavior: matchMedia('(prefers-reduced-motion: reduce)').matches ? 'instant' : 'smooth', block: 'center'}); target.classList.add('sae-message-focus'); setTimeout(() => target.classList.remove('sae-message-focus'), 1800); }
      else { const url = new URL(this.settings && this.data.session ? '/session/' + this.data.session : location.href, location.origin); url.searchParams.set('encoder', this.data.dictionary?.id || ''); url.searchParams.set('event', message.index); location.href = url.href; }
    }
    observe() {
      if (this.observer) this.observer.disconnect(); if (!('IntersectionObserver' in window) || this.settings) return;
      this.observer = new IntersectionObserver(entries => { entries.forEach(entry => { const index = entry.target.id.slice(2); this.map.querySelectorAll('[data-index="' + index + '"]').forEach(cell => cell.classList.toggle('sae-in-view', entry.isIntersecting)); }); }, {threshold: 0.1});
      this.messages.forEach(m => { const target = document.getElementById('e-' + m.index); if (target) this.observer.observe(target); });
    }
    renderExplanation() {
      const section = el('section', 'sae-explanation'); section.append(el('h2', '', 'Understanding the values'));
      [['What is a feature?', 'A sparse autoencoder (SAE) extracts recurring patterns from model representations. Feature names describe observed patterns; they are interpretations, not definitive labels for a message.'], ['What does the percentage mean?', 'The displayed value is the activation divided by its exported feature scale, multiplied by 100, rounded, and capped at 100. It is not a probability or confidence score; 100% is not certainty.'], ['Why do features overlap?', 'A message can express several patterns at once. Separate tracks show their stored activations independently.'], ['What does an empty track mean?', 'The selected features may have no stored activation, or their values may be below your display threshold. Striped marks identify messages without saved results. Missing results do not mean zero activation.'], ['What does status cover?', 'Message status shows queued, running, failed, and stored SAE executions when on-demand scoring is configured. Stored results are reused. Dictionary import time is separate from execution progress; global coverage and model training progress are not measured here.']].forEach(([title, text]) => { section.append(el('h3', '', title), el('p', '', text)); }); this.root.append(section);
    }
    schedulePoll() {
      clearTimeout(this.pollTimer);
      if (this.root.isConnected && this.data.refresh_url && (this.data.status?.poll ?? this.data.status?.active > 0) && !document.hidden) this.pollTimer = setTimeout(() => { if (this.root.isConnected) window.PravdaSAE.refresh(); }, 2500);
    }
    update(data) {
      const prefs = this.prefs;
      const pickerOpen = this.picker?.open;
      const optionsOpen = this.root.querySelector('.sae-display-options')?.open;
      const inspected = this.inspector && !this.inspector.hidden ? this.selectedMessage?.index : null;
      const focused = document.activeElement === this.search;
      this.data = data; this.setup(); this.prefs = prefs; this.prefs.selected = prefs.selected.filter(id => this.byFeature.has(id)); this.render();
      this.search.value = this.query; this.renderResults(); this.picker.open = pickerOpen;
      const options = this.root.querySelector('.sae-display-options'); if (options) options.open = optionsOpen;
      if (inspected !== null) { const message = this.messages.find(m => m.index === inspected); if (message) this.inspect(message); }
      if (focused) this.search.focus();
    }
  }
  function init(scope) {
    scope = scope || document;
    active.forEach(instance => { if (!instance.root.isConnected) { instance.observer?.disconnect(); clearTimeout(instance.pollTimer); active.delete(instance); } });
    const roots = [...(scope.matches?.('[data-sae]') ? [scope] : []), ...scope.querySelectorAll('[data-sae]')];
    roots.forEach(root => { if (instances.has(root)) return; try { const data = JSON.parse(root.dataset.sae); const instance = new Workbench(root, data); instances.set(root, instance); active.add(instance); } catch (e) { root.textContent = 'SAE display could not load. Open Settings & status to review saved data.'; root.append(link('Settings & status', '/sae/settings')); } });
  }
  window.PravdaSAE = {init, refresh: async function () { for (const root of document.querySelectorAll('[data-sae]')) { const instance = instances.get(root); if (!instance || !instance.data.refresh_url || instance.refreshing) continue; instance.refreshing = true; const finish = window.PravdaLoading?.start('Refreshing SAE activations…', root); try { const response = await fetch(instance.data.refresh_url, {headers: {Accept: 'application/json'}}); if (!response.ok) throw new Error('Refresh failed'); if (root.isConnected) instance.update(await response.json()); } catch (_) { if (instance.notice) instance.notice.textContent = 'Could not refresh saved results. Current values are still displayed; try Refresh again.'; } finally { instance.refreshing = false; finish?.(); instance.schedulePoll(); } } }};
  document.addEventListener('visibilitychange', () => { if (!document.hidden) active.forEach(instance => instance.schedulePoll()); });
  document.addEventListener('DOMContentLoaded', () => init());
  document.addEventListener('htmx:afterSettle', () => init());
  document.addEventListener('htmx:oobAfterSwap', () => init());
  if (document.readyState !== 'loading') init();
})();
