// Small progressive-enhancement layer for the Lighthouse UI. Server renders the
// pages; this handles action buttons (scan / per-container update) against the
// JSON API and streams the live log over SSE. htmx handles fragment refreshes.
const lh = {
  async post(url) {
    return fetch(url, { method: 'POST', headers: { 'X-Requested-With': 'fetch' } });
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
