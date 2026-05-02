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

      // Probe details
      function fmtProbe(p) {
        if (!p) return '—';
        const dot = p.ok ? '✓' : '✗';
        const lat = (p.latency_ms != null) ? ` (${p.latency_ms} ms)` : '';
        return `${dot} ${p.detail || ''}${lat}`;
      }
      setText('dep-ollama', fmtProbe(s.dependencies.ollama));
      setText('dep-redis',  fmtProbe(s.dependencies.redis));
    } catch (e) {
      flash(String(e), false);
    }
    refreshDecisions();
  }
  window.refreshStatus = refreshStatus;

  // -------- Recent decisions feed --------
  function severityClass(sev) {
    return 'sev-' + (String(sev || '').toLowerCase() || 'unknown');
  }
  function outcomeClass(o) {
    return 'oc-' + (String(o || '').toLowerCase() || 'unknown');
  }
  async function refreshDecisions() {
    const tbody = document.getElementById('decisions-rows');
    if (!tbody) return;
    try {
      const res = await fetch('/api/decisions/recent');
      if (!res.ok) return;
      const j = await res.json();
      tbody.innerHTML = '';
      (j.decisions || []).forEach((d) => {
        const tr = document.createElement('tr');
        const t = new Date(d.time).toLocaleTimeString();
        const target = `${d.kind}/${d.name}`;
        const conf = (d.confidence != null) ? d.confidence.toFixed(2) : '';
        tr.innerHTML = `
          <td>${t}</td>
          <td>${d.namespace}</td>
          <td>${target}</td>
          <td>${d.event_reason || ''}</td>
          <td>${d.action}</td>
          <td><span class="chip ${severityClass(d.severity)}">${d.severity || '?'}</span></td>
          <td>${conf}</td>
          <td><span class="chip ${outcomeClass(d.outcome)}">${d.outcome}</span>${d.outcome_error ? ` <span class="hint">${d.outcome_error}</span>` : ''}</td>
        `;
        tbody.appendChild(tr);
      });
      if ((j.decisions || []).length === 0) {
        tbody.innerHTML = '<tr><td colspan="8" class="hint">Nessuna decisione ancora registrata.</td></tr>';
      }
    } catch (_e) {}
  }
  window.refreshDecisions = refreshDecisions;

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

  // -------- Cluster page --------
  let clusterDebounceTimer = null;
  let clusterLastPodMeta = null;

  async function loadClusterNamespaces() {
    const sel = document.getElementById('cluster-ns');
    if (!sel) return;
    try {
      const res = await fetch('/api/cluster/namespaces');
      if (!res.ok) { flash('namespaces: HTTP ' + res.status, false); return; }
      const j = await res.json();
      const list = j.namespaces || [];
      sel.innerHTML = '';
      if (list.length === 0) {
        sel.innerHTML = '<option value="">(no INCLUDE_NAMESPACES configured)</option>';
        return;
      }
      list.forEach((n) => {
        const o = document.createElement('option');
        o.value = n; o.textContent = n;
        sel.appendChild(o);
      });
      refreshClusterPods();
    } catch (e) { flash(String(e), false); }
  }

  function podPhaseClass(p) { return 'phase-' + String(p || '').toLowerCase(); }

  async function refreshClusterPods() {
    const ns = document.getElementById('cluster-ns')?.value;
    if (!ns) return;
    const phase = document.getElementById('cluster-phase')?.value || '';
    const name = document.getElementById('cluster-name')?.value || '';
    const url = `/api/cluster/pods?namespace=${encodeURIComponent(ns)}` +
      (phase ? `&phase=${encodeURIComponent(phase)}` : '') +
      (name  ? `&name=${encodeURIComponent(name)}`   : '');
    try {
      const res = await fetch(url);
      if (!res.ok) {
        let msg = 'HTTP ' + res.status;
        try { const j = await res.json(); msg = j.error || msg; } catch (_e) {}
        flash('pods: ' + msg, false); return;
      }
      const j = await res.json();
      const tbody = document.getElementById('cluster-pod-rows');
      if (!tbody) return;
      tbody.innerHTML = '';
      (j.pods || []).forEach((p) => {
        const tr = document.createElement('tr');
        tr.innerHTML = `
          <td>${p.name}</td>
          <td><span class="chip ${podPhaseClass(p.phase)}">${p.phase}</span></td>
          <td>${p.ready ? '✓' : '✗'}</td>
          <td>${p.restarts}</td>
          <td>${p.last_term_reason || ''}</td>
          <td>${(p.containers || []).join(', ')}</td>
          <td>${p.age}</td>
          <td>${p.node || ''}</td>
          <td><button class="btn-link" data-pod="${p.name}" data-ns="${p.namespace}" data-c="${(p.containers || [])[0] || ''}">logs</button></td>
        `;
        tr.querySelector('button').addEventListener('click', (e) => {
          const b = e.currentTarget;
          openLogModal(b.dataset.ns, b.dataset.pod, b.dataset.c);
        });
        tbody.appendChild(tr);
      });
      const counts = j.counts || {};
      const summary = Object.entries(counts).map(([k, v]) => `${k}:${v}`).join(' · ');
      const cEl = document.getElementById('cluster-counts');
      if (cEl) cEl.textContent = `${(j.pods || []).length} pods (${summary || 'none'})`;
    } catch (e) { flash(String(e), false); }
  }
  function refreshClusterPodsDebounced() {
    clearTimeout(clusterDebounceTimer);
    clusterDebounceTimer = setTimeout(refreshClusterPods, 250);
  }

  function openLogModal(ns, pod, container) {
    clusterLastPodMeta = { ns, pod, container };
    document.getElementById('log-modal-title').textContent = `${ns}/${pod} (${container || 'default'})`;
    document.getElementById('log-modal-previous').checked = false;
    document.getElementById('log-modal').style.display = 'flex';
    reloadModalLogs();
  }
  function closeLogModal() {
    document.getElementById('log-modal').style.display = 'none';
  }
  async function reloadModalLogs() {
    if (!clusterLastPodMeta) return;
    const { ns, pod, container } = clusterLastPodMeta;
    const previous = document.getElementById('log-modal-previous').checked;
    const url = `/api/cluster/pods/logs?namespace=${encodeURIComponent(ns)}&pod=${encodeURIComponent(pod)}` +
      (container ? `&container=${encodeURIComponent(container)}` : '') +
      `&previous=${previous}&tail=500`;
    const body = document.getElementById('log-modal-body');
    body.textContent = 'loading...';
    try {
      const res = await fetch(url);
      const text = await res.text();
      body.textContent = res.ok ? (text || '(empty)') : `[error] ${text}`;
      body.scrollTop = body.scrollHeight;
    } catch (e) {
      body.textContent = `[error] ${e}`;
    }
  }
  window.refreshClusterPods = refreshClusterPods;
  window.refreshClusterPodsDebounced = refreshClusterPodsDebounced;
  window.closeLogModal = closeLogModal;
  window.reloadModalLogs = reloadModalLogs;

  // -------- Scenarios monitor --------
  // Per-card live state: each scenario card has a chip + summary + pod
  // list that flips between not_applied / pending / error / resolved as
  // the underlying pods come up, fail, and (hopefully) recover.
  const SCENARIO_STATES = ['not_applied', 'pending', 'error', 'resolved'];
  function scenarioStateLabel(state) {
    switch (state) {
      case 'resolved':    return 'resolved';
      case 'error':       return 'error';
      case 'pending':     return 'pending';
      case 'not_applied': return 'not applied';
      default:            return state || 'unknown';
    }
  }
  function applyScenarioStatus(card, status) {
    const chip = card.querySelector('[data-role="state"]');
    const sum  = card.querySelector('[data-role="summary"]');
    const pods = card.querySelector('[data-role="pods"]');
    if (chip) {
      SCENARIO_STATES.forEach((s) => chip.classList.remove('state-' + s));
      const st = status.state || 'not_applied';
      chip.classList.add('state-' + st);
      chip.textContent = scenarioStateLabel(st);
    }
    if (sum) sum.textContent = status.summary || '';
    if (pods) {
      pods.innerHTML = '';
      (status.pods || []).forEach((p) => {
        const li = document.createElement('li');
        const ready = p.ready ? '✓' : '✗';
        const reason = p.reason ? ` · ${p.reason}` : '';
        const restarts = p.restarts ? ` · restarts=${p.restarts}` : '';
        li.textContent = `${ready} ${p.name} (${p.phase})${reason}${restarts}`;
        pods.appendChild(li);
      });
    }
  }
  async function refreshScenariosStatus() {
    const cards = document.querySelectorAll('.scenario-card');
    if (cards.length === 0) return;
    const info = document.getElementById('scenarios-monitor-info');
    try {
      const res = await fetch('/api/scenarios/status');
      if (!res.ok) {
        if (info) info.textContent = 'monitor: HTTP ' + res.status;
        return;
      }
      const j = await res.json();
      const byName = {};
      (j.scenarios || []).forEach((s) => { byName[s.name] = s; });
      cards.forEach((card) => {
        const name = card.dataset.scenario;
        const st = byName[name];
        if (st) applyScenarioStatus(card, st);
      });
      if (info) {
        const now = new Date().toLocaleTimeString();
        info.textContent = `Last update ${now}`;
      }
    } catch (e) {
      if (info) info.textContent = 'monitor: ' + String(e);
    }
  }
  window.refreshScenariosStatus = refreshScenariosStatus;

  // Auto-init: feature-detect which page we are on and run the right
  // bootstrap. The script tag is at the end of <body> in layout.html so
  // every form/element already exists by the time we run.
  if (document.getElementById('agent-namespace')) refreshStatus();
  if (document.getElementById('log-view')) startLogStream();
  if (document.getElementById('cluster-ns')) loadClusterNamespaces();
  if (document.querySelector('.scenario-card')) {
    refreshScenariosStatus();
    setInterval(refreshScenariosStatus, 5000);
  }
})();
