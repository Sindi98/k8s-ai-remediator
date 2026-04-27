// ai-remediator admin GUI — small vanilla JS bundle.
// No external runtime dependencies; one pattern per feature.

(function () {
  // Flash banner used to surface API responses.
  function flash(message, ok) {
    let el = document.getElementById('flash');
    if (!el) {
      el = document.createElement('div');
      el.id = 'flash';
      document.body.appendChild(el);
    }
    el.textContent = message;
    el.className = ok ? 'ok' : 'err';
    el.style.display = 'block';
    clearTimeout(flash._t);
    flash._t = setTimeout(() => { el.style.display = 'none'; }, 6000);
  }

  // postForm submits a plain object as application/x-www-form-urlencoded.
  // Returns parsed JSON or throws with the server-side error message.
  async function postForm(endpoint, data) {
    const body = new URLSearchParams();
    Object.entries(data || {}).forEach(([k, v]) => body.append(k, v == null ? '' : String(v)));
    try {
      const res = await fetch(endpoint, { method: 'POST', body });
      const text = await res.text();
      let json = {};
      try { json = text ? JSON.parse(text) : {}; } catch (_) { json = { error: text }; }
      if (!res.ok) {
        flash(json.error || ('HTTP ' + res.status), false);
        return null;
      }
      flash(json.message || 'OK', true);
      return json;
    } catch (e) {
      flash(String(e), false);
      return null;
    }
  }
  window.postForm = postForm;

  // Auto-bind every <form data-endpoint="...">.
  // For each form we also collect the names of every checkbox in a hidden
  // __bool_fields parameter. The backend uses it to distinguish
  // "checkbox not on this form" from "checkbox unchecked" — HTML omits
  // unchecked checkboxes from the submission entirely.
  document.addEventListener('submit', async (ev) => {
    const form = ev.target;
    if (!(form instanceof HTMLFormElement) || !form.dataset.endpoint) return;
    ev.preventDefault();
    const data = {};
    new FormData(form).forEach((v, k) => { data[k] = v; });
    const boolFields = Array.from(form.querySelectorAll('input[type="checkbox"]'))
      .map((el) => el.name).filter(Boolean);
    if (boolFields.length > 0) {
      data['__bool_fields'] = boolFields.join(',');
      // Default unchecked checkboxes to "false" so the backend can flip
      // them off rather than silently keeping the previous value.
      boolFields.forEach((name) => {
        if (!(name in data)) data[name] = 'false';
      });
    }
    await postForm(form.dataset.endpoint, data);
  });

  // -------- Dashboard --------
  function setText(id, value) {
    const el = document.getElementById(id);
    if (el) el.textContent = value == null ? '—' : value;
  }
  async function refreshStatus() {
    try {
      const res = await fetch('/api/status');
      if (!res.ok) { flash('status: HTTP ' + res.status, false); return; }
      const s = await res.json();
      setText('agent-namespace', s.agent.namespace);
      setText('agent-deployment', s.agent.deployment);
      setText('agent-replicas', s.agent.replicas);
      setText('agent-ready', s.agent.ready_replicas);
      setText('dep-configmap', s.dependencies.configmap);
      setText('dep-secret', s.dependencies.secret);
      setText('dep-lease', s.dependencies.lease);

      const sandbox = document.getElementById('sandbox-list');
      if (sandbox) {
        sandbox.innerHTML = '';
        (s.sandbox_namespaces || []).forEach((n) => {
          const li = document.createElement('li'); li.textContent = n; sandbox.appendChild(li);
        });
      }

      const podRows = document.getElementById('pod-rows');
      if (podRows) {
        podRows.innerHTML = '';
        (s.pods || []).forEach((p) => {
          const tr = document.createElement('tr');
          tr.innerHTML = `<td>${p.name}</td><td>${p.phase}</td><td>${p.ready ? '✓' : '✗'}</td><td>${p.restarts}</td><td>${p.age}</td>`;
          podRows.appendChild(tr);
        });
      }

      const cfgRows = document.getElementById('config-rows');
      if (cfgRows) {
        cfgRows.innerHTML = '';
        Object.entries(s.config || {}).forEach(([k, v]) => {
          const tr = document.createElement('tr');
          tr.innerHTML = `<td>${k}</td><td>${v}</td>`;
          cfgRows.appendChild(tr);
        });
      }
    } catch (e) {
      flash(String(e), false);
    }
  }
  window.refreshStatus = refreshStatus;

  // -------- Logs (SSE) --------
  let logSource = null;
  let logPaused = false;
  function appendLog(line) {
    const view = document.getElementById('log-view');
    if (!view) return;
    const wasNearBottom = (view.scrollHeight - view.scrollTop - view.clientHeight) < 80;
    view.textContent += line + '\n';
    if (wasNearBottom) view.scrollTop = view.scrollHeight;
  }
  function startLogStream() {
    if (logSource) logSource.close();
    logSource = new EventSource('/api/logs/stream');
    logSource.addEventListener('ready', (e) => appendLog('--- ' + e.data + ' ---'));
    logSource.addEventListener('error', (e) => {
      if (e && e.data) appendLog('!!! ' + e.data);
    });
    logSource.onmessage = (e) => { if (!logPaused) appendLog(e.data); };
  }
  function toggleLogStream() {
    logPaused = !logPaused;
    const btn = document.getElementById('log-toggle');
    if (btn) btn.textContent = logPaused ? 'Resume' : 'Pause';
  }
  function clearLogs() {
    const view = document.getElementById('log-view');
    if (view) view.textContent = '';
  }
  window.startLogStream = startLogStream;
  window.toggleLogStream = toggleLogStream;
  window.clearLogs = clearLogs;

  // Auto-init: feature-detect which page we are on and run the right
  // bootstrap. The script tag is at the end of <body> in layout.html so
  // every form/element already exists by the time we run.
  if (document.getElementById('agent-namespace')) refreshStatus();
  if (document.getElementById('log-view')) startLogStream();
})();
