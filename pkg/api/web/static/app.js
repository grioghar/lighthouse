// Small progressive-enhancement layer for the Lighthouse UI. Server renders the
// pages; this handles action buttons (scan / per-container update) against the
// JSON API and streams the live log over SSE. htmx handles fragment refreshes.
const lh = {
  csrf() {
    const m = document.cookie.match(/(?:^|; )lighthouse_csrf=([^;]+)/);
    return m ? decodeURIComponent(m[1]) : '';
  },
  async post(url) {
    return fetch(url, { method: 'POST', headers: { 'X-Requested-With': 'fetch', 'X-CSRF-Token': lh.csrf() } });
  },
  async saveSettings(e) {
    e.preventDefault();
    const f = document.getElementById('settings-form');
    const st = document.getElementById('settings-status');
    const body = {
      cleanup: f.cleanup.checked,
      monitorOnly: f.monitorOnly.checked,
      noRestart: f.noRestart.checked,
      noPull: f.noPull.checked,
      lifecycleHooks: f.lifecycleHooks.checked,
      rollingRestart: f.rollingRestart.checked,
      healthGated: f.healthGated.checked,
      healthTimeoutSeconds: parseInt(f.healthTimeoutSeconds.value || '0', 10),
    };
    try {
      const r = await fetch('/api/v1/settings', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': lh.csrf() },
        body: JSON.stringify(body),
      });
      if (r.ok) {
        st.textContent = 'saved';
      } else {
        const j = await r.json().catch(() => ({}));
        st.textContent = 'error: ' + (j.error || r.status);
      }
    } catch {
      st.textContent = 'error';
    }
    return false;
  },
  async scan() {
    const s = document.getElementById('scan-status');
    try {
      const r = await lh.post('/api/v1/scan');
      s.textContent = r.status === 202 ? 'started…' : r.status === 409 ? 'already running' : 'error';
    } catch { s.textContent = 'error'; }
    lh.refreshSoon();
  },
  async update(id, btn) {
    btn.disabled = true; const prev = btn.textContent; btn.textContent = '…';
    try {
      const r = await lh.post('/api/v1/containers/' + encodeURIComponent(id) + '/update');
      btn.textContent = r.status === 202 ? 'started' : r.status === 409 ? 'busy' : 'error';
    } catch { btn.textContent = 'error'; }
    setTimeout(() => { btn.disabled = false; btn.textContent = prev; }, 4000);
    lh.refreshSoon();
  },
  refreshSoon() {
    setTimeout(() => {
      if (window.htmx) {
        htmx.trigger(document.body, 'refresh');
        htmx.trigger('#status', 'load');
      }
    }, 1500);
  },
};

// Live log via Server-Sent Events.
(function () {
  const log = document.getElementById('log');
  if (!log || !window.EventSource) return;
  const es = new EventSource('/api/v1/events');
  es.onmessage = (e) => {
    log.textContent += e.data + '\n';
    const lines = log.textContent.split('\n');
    if (lines.length > 400) log.textContent = lines.slice(-400).join('\n');
    log.scrollTop = log.scrollHeight;
  };
})();
