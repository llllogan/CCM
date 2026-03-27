let inventory = [];
let selected = null;
let stream = null;
let streamContainerID = null;
let paused = false;
const LOG_MAX_LINES = 2000;
const LOG_FLUSH_INTERVAL_MS = 100;
let suppressNextStreamError = false;
const composeChildrenByID = {};
const expandedCompose = new Set();
const scheduledTasksByStackID = {};
let actionLogName = '';
let actionLogCountdownTimer = null;
let actionLogCountdownRemaining = 0;
let actionLogShouldAutoClose = false;
const stacksByID = {};
let logLines = [];
let pendingLogLines = [];
let logFlushTimer = null;
let droppedLogLineCount = 0;

const $ = (id) => document.getElementById(id);

function resetSelectionUI() {
  selected = null;
  stopLogs();
  setScheduledTabVisible(false);
  $('title').textContent = 'Select an item';
  $('subtitle').textContent = 'No host machine selected';
  $('status').textContent = 'idle';
}

function reconcileSelection() {
  if (!selected) return;
  if (selected.type === 'container') {
    // Container rows under compose stacks are not part of top-level inventory items.
    // Keep current selection during background polling to avoid UI deselection jitter.
    const cachedChildren = Object.values(composeChildrenByID).flat();
    const byID = cachedChildren.find((c) => c.id === selected.id);
    if (byID) {
      selected = {
        type: 'container',
        id: byID.id,
        name: byID.name,
        target_id: byID.target_id,
        status: byID.status,
      };
    }
    return;
  }
  const byID = inventory.find((i) => i.id === selected.id && i.type === selected.type);
  if (byID) {
    selected = byID;
    return;
  }
  const byIdentity = inventory.find((i) => i.type === selected.type && i.name === selected.name && i.target_id === selected.target_id);
  if (byIdentity) {
    selected = byIdentity;
    return;
  }
  resetSelectionUI();
}

function isSameSelection(selectedItem, candidate) {
  if (!selectedItem || !candidate) return false;
  if (selectedItem.id && candidate.id && selectedItem.id === candidate.id) return true;
  return selectedItem.type === candidate.type
    && selectedItem.name === candidate.name
    && selectedItem.target_id === candidate.target_id;
}

async function fetchInventory({ silent = false, reconcile = true } = {}) {
  try {
    const res = await fetch('/v1/inventory');
    if (!res.ok) throw new Error(`inventory request failed (${res.status})`);
    const data = await res.json();
    inventory = data.items || [];
    if (reconcile) {
      reconcileSelection();
    }
    renderItems();
    return true;
  } catch (err) {
    if (!silent) {
      setActionResult(`Inventory refresh failed: ${err?.message || String(err)}`, true);
    }
    return false;
  }
}

async function fetchStacks({ silent = false } = {}) {
  try {
    const res = await fetch('/v1/stacks');
    if (!res.ok) throw new Error(`stacks request failed (${res.status})`);
    const rows = await res.json();
    Object.keys(stacksByID).forEach((k) => delete stacksByID[k]);
    (rows || []).forEach((row) => {
      if (row?.id) stacksByID[row.id] = row;
    });
    return true;
  } catch (err) {
    if (!silent) {
      setActionResult(`Stack metadata refresh failed: ${err?.message || String(err)}`, true);
    }
    return false;
  }
}

function renderItems() {
  const q = $('search').value.toLowerCase();
  const host = $('items');
  host.innerHTML = '';

  inventory
    .filter(i => i.name.toLowerCase().includes(q) || i.target_id.toLowerCase().includes(q))
    .forEach(item => {
      const row = document.createElement('div');
      row.className = 'item';
      if (isSameSelection(selected, item)) row.classList.add('active');
      const marker = item.type === 'compose' ? (expandedCompose.has(item.id) ? '[-] ' : '[+] ') : '';
      row.innerHTML = `<div>${marker}${item.name}</div><div class="meta">${item.type} | ${item.target_id} | ${item.status}</div>`;
      row.onclick = async () => {
        if (item.type === 'compose') {
          if (expandedCompose.has(item.id)) {
            expandedCompose.delete(item.id);
          } else {
            expandedCompose.add(item.id);
            await ensureComposeChildren(item.id);
          }
          await selectItem(item);
          renderItems();
          return;
        }
        await selectItem(item);
      };
      host.appendChild(row);

      if (item.type === 'compose' && expandedCompose.has(item.id)) {
        const children = composeChildrenByID[item.id] || [];
        children.forEach((c) => {
          const child = document.createElement('div');
          child.className = 'item item-child';
          if (isSameSelection(selected, { type: 'container', id: c.id, name: c.name, target_id: c.target_id })) child.classList.add('active');
          child.innerHTML = `<div>└─ ${c.name}</div><div class="meta">${c.status} | ${c.container_id}</div>`;
          child.onclick = async (evt) => {
            evt.stopPropagation();
            await selectItem({
              type: 'container',
              id: c.id,
              name: c.name,
              target_id: c.target_id,
              status: c.status,
            });
          };
          host.appendChild(child);
        });
      }
    });
}

async function selectItem(item) {
  selected = item;
  renderItems();

  $('title').textContent = item.name;
  $('subtitle').textContent = item.target_id;
  $('status').textContent = item.status;

  if (item.type === 'container') {
    setScheduledTabVisible(false);
    const res = await fetch(`/v1/containers/${encodeURIComponent(item.id)}`);
    if (!res.ok) {
      if (res.status === 404) {
        const refreshed = await fetchInventory({ silent: true });
        if (refreshed) {
          const replacement = inventory.find((i) => i.type === 'container' && i.name === item.name && i.target_id === item.target_id);
          if (replacement && replacement.id !== item.id) {
            await selectItem(replacement);
            return;
          }
        }
      }
      $('details').textContent = `Failed to load container details (${res.status})`;
      return;
    }
    const c = await res.json();
    renderStats([
      ['Image', c.image],
      ['Uptime', c.uptime],
      ['Ports', (c.ports || []).join(', ') || '-'],
      ['Container ID', c.container_id],
      ['Restart count', c.restart_count],
      ['Host machine', c.target_id],
    ], { restart: c.restart });
    $('details').textContent = JSON.stringify(c, null, 2);
    startLogs(c.id);
    switchTab('logs');
  } else if (item.type === 'compose') {
    setScheduledTabVisible(true);
    if (!stacksByID[item.id]) {
      await fetchStacks({ silent: true });
    }
    const children = await ensureComposeChildren(item.id);
    const stackRow = stacksByID[item.id];
    renderStats([
      ['Project', item.name],
      ['Services', children.length],
      ['Host machine', item.target_id],
      ['Status', item.status],
      ['Stack ID', item.id],
    ], { restart: stackRow?.restart || null });
    $('details').textContent = JSON.stringify(children, null, 2);
    await fetchScheduledTasks(item.id, { silent: true });
    renderScheduledTasks(item.id);
    stopLogs();
    switchTab('details');
  } else {
    setScheduledTabVisible(false);
    renderStats([['Error', item.name]]);
    $('details').textContent = JSON.stringify(item, null, 2);
    stopLogs();
  }
}

function renderStats(items, options = {}) {
  const cards = [];
  items.forEach(([k, v]) => {
    cards.push(`<div class="stat"><div class="k">${escapeHTML(k)}</div><div class="v">${escapeHTML(String(v ?? '-'))}</div></div>`);
  });
  if (options.restart) {
    cards.push(renderRestartCard(options.restart));
  }
  $('stats').innerHTML = cards.join('');
}

function renderRestartCard(restart) {
  const disabled = restart?.enabled === false;
  const source = restart?.source ? ` (${restart.source})` : '';
  let body = '-';
  if (disabled) {
    body = escapeHTML(restart?.note || 'Not enabled');
  } else {
    const parts = [];
    if (restart?.strategy) parts.push(`name: ${restart.strategy}`);
    if (restart?.cron) parts.push(`cron: ${restart.cron}`);
    if (restart?.timezone) parts.push(`tz: ${restart.timezone}`);
    body = escapeHTML(parts.join(' | ') || 'Configured');
  }
  return `<div class="stat stat-restart${disabled ? ' is-disabled' : ''}"><div class="k">Restart Strategy${escapeHTML(source)}</div><div class="v">${body}</div></div>`;
}

function escapeHTML(value) {
  return String(value)
    .replaceAll('&', '&amp;')
    .replaceAll('<', '&lt;')
    .replaceAll('>', '&gt;')
    .replaceAll('"', '&quot;')
    .replaceAll("'", '&#39;');
}

function setStreamIndicator(state) {
  const dot = $('streamDot');
  const leopardDotMin = $('leopardDotMin') || $('lionDotMin');
  const leopardDotZoom = $('leopardDotZoom') || $('lionDotZoom');
  const title = state === 'active'
    ? 'Log stream connected'
    : (state === 'connecting' ? 'Log stream reconnecting' : 'Log stream disconnected');

  if (leopardDotMin && leopardDotZoom) {
    leopardDotMin.classList.remove('is-connecting');
    leopardDotZoom.classList.remove('is-active');

    if (state === 'connecting') {
      leopardDotMin.classList.add('is-connecting');
    } else if (state === 'active') {
      leopardDotZoom.classList.add('is-active');
    }

    leopardDotMin.title = title;
    leopardDotZoom.title = title;
    if (dot) dot.title = title;
    return;
  }

  if (!dot) return;
  dot.classList.remove('is-active', 'is-inactive', 'is-connecting');
  if (state === 'active') {
    dot.classList.add('is-active');
    dot.title = title;
    return;
  }
  if (state === 'connecting') {
    dot.classList.add('is-connecting');
    dot.title = title;
    return;
  }
  dot.classList.add('is-inactive');
  dot.title = title;
}

function resetLogOutput() {
  logLines = [];
  pendingLogLines = [];
  droppedLogLineCount = 0;
  if (logFlushTimer) {
    clearTimeout(logFlushTimer);
    logFlushTimer = null;
  }
  $('logs').textContent = '';
}

function flushLogOutput() {
  logFlushTimer = null;
  if (pendingLogLines.length === 0) return;

  const out = $('logs');
  const shouldScroll = $('autoScroll').checked;
  const nextLines = pendingLogLines;
  pendingLogLines = [];
  logLines.push(...nextLines);

  let needsRebuild = false;
  if (logLines.length > LOG_MAX_LINES) {
    droppedLogLineCount += logLines.length - LOG_MAX_LINES;
    logLines = logLines.slice(-LOG_MAX_LINES);
    needsRebuild = true;
  }

  if (needsRebuild) {
    const prefix = droppedLogLineCount > 0
      ? [`[truncated log output; keeping last ${LOG_MAX_LINES} lines, dropped ${droppedLogLineCount}]`]
      : [];
    out.textContent = prefix.concat(logLines).join('\n') + '\n';
  } else {
    out.append(document.createTextNode(nextLines.join('\n') + '\n'));
  }

  if (shouldScroll) {
    out.scrollTop = out.scrollHeight;
  }
}

function scheduleLogFlush() {
  if (logFlushTimer) return;
  logFlushTimer = setTimeout(flushLogOutput, LOG_FLUSH_INTERVAL_MS);
}

function appendLogLine(line) {
  pendingLogLines.push(line);
  scheduleLogFlush();
}

function stopLogs({ clearSelection = true, resetSuppress = true } = {}) {
  if (stream) {
    stream.close();
    stream = null;
  }
  flushLogOutput();
  if (resetSuppress) {
    suppressNextStreamError = false;
  }
  setStreamIndicator('inactive');
  if (clearSelection) {
    streamContainerID = null;
  }
}

function startLogs(id, { resetOutput = true } = {}) {
  streamContainerID = id;
  stopLogs({ clearSelection: false });
  if (resetOutput) {
    resetLogOutput();
  }
  setStreamIndicator('connecting');
  const tail = resetOutput ? 200 : 0;
  stream = new EventSource(`/v1/containers/${encodeURIComponent(id)}/logs/stream?tail=${tail}`);
  stream.onopen = () => {
    setStreamIndicator('active');
  };
  stream.onmessage = (evt) => {
    if (paused) return;
    appendLogLine(evt.data);
  };
  stream.addEventListener('terminal-error', (evt) => {
    suppressNextStreamError = true;
    setStreamIndicator('inactive');
    appendLogLine(`[stream error] ${evt.data || 'log stream failed'}`);
    stopLogs({ clearSelection: false, resetSuppress: false });
  });
  stream.addEventListener('done', () => {
    suppressNextStreamError = true;
    stopLogs({ clearSelection: false, resetSuppress: false });
  });
  stream.onerror = () => {
    if (suppressNextStreamError) {
      suppressNextStreamError = false;
      return;
    }
    setStreamIndicator('inactive');
    appendLogLine('[stream error or disconnected]');
  };
}

function reconnectLogsIfNeeded() {
  if (document.hidden) return;
  if (!streamContainerID) return;
  if (selected?.type !== 'container') return;
  if (stream) return;
  startLogs(streamContainerID, { resetOutput: false });
}

async function ensureComposeChildren(composeID) {
  if (composeChildrenByID[composeID]) return composeChildrenByID[composeID];
  const res = await fetch(`/v1/items/${encodeURIComponent(composeID)}/children`);
  if (!res.ok) {
    composeChildrenByID[composeID] = [];
    $('details').textContent = `Failed to load compose services (${res.status})`;
    return [];
  }
  const children = await res.json();
  composeChildrenByID[composeID] = children;
  return children;
}

function setScheduledTabVisible(visible) {
  const tab = $('tabScheduled');
  const panel = $('panelScheduled');
  if (!tab || !panel) return;
  tab.hidden = !visible;
  panel.hidden = !visible;
  if (!visible && document.querySelector('.tab.active')?.dataset.tab === 'scheduled') {
    switchTab('details');
  }
}

function fmtTime(raw) {
  if (!raw) return '-';
  const d = new Date(raw);
  if (Number.isNaN(d.getTime())) return String(raw);
  return d.toLocaleString();
}

function renderScheduledTasks(stackID) {
  const host = $('scheduledTasks');
  if (!host) return;
  host.innerHTML = '';
  const tasks = scheduledTasksByStackID[stackID] || [];
  if (tasks.length === 0) {
    host.textContent = 'No scheduled scripts configured for this stack.';
    return;
  }

  const table = document.createElement('table');
  table.className = 'scheduled-table';
  table.innerHTML = `
    <thead>
      <tr>
        <th>Name</th>
        <th>Cron</th>
        <th>Timezone</th>
        <th>File</th>
        <th>Last Attempt</th>
        <th>Exit</th>
        <th>Result</th>
        <th>Action</th>
      </tr>
    </thead>
    <tbody></tbody>
  `;
  const tbody = table.querySelector('tbody');

  tasks.forEach((task) => {
    const row = document.createElement('tr');
    const result = task.running ? 'running' : (task.last_result || '-');
    row.innerHTML = `
      <td>${escapeHTML(task.name || '-')}</td>
      <td>${escapeHTML(task.cron || '-')}</td>
      <td>${escapeHTML(task.timezone || 'Local')}</td>
      <td>${escapeHTML(task.file || '-')}</td>
      <td>${escapeHTML(fmtTime(task.last_attempt_at))}</td>
      <td>${escapeHTML(task.last_exit_code === 0 || task.last_exit_code ? String(task.last_exit_code) : '-')}</td>
      <td>${escapeHTML(result)}</td>
      <td></td>
    `;
    const actionCell = row.querySelector('td:last-child');
    const runBtn = document.createElement('button');
    runBtn.textContent = task.running ? 'Running...' : 'Run now';
    runBtn.disabled = Boolean(task.running);
    runBtn.onclick = async () => {
      if (!selected || selected.type !== 'compose') return;
      runBtn.disabled = true;
      runBtn.textContent = 'Running...';
      setActionResult(`Running script ${task.name}...`);
      try {
        const out = await post(`/v1/scripts/${encodeURIComponent(stackID)}/${encodeURIComponent(task.name)}/run`);
        const updated = out?.script;
        const resultRow = out?.result || {};
        if (updated) {
          const rows = scheduledTasksByStackID[stackID] || [];
          const idx = rows.findIndex((r) => r.name === updated.name);
          if (idx >= 0) rows[idx] = updated;
          else rows.push(updated);
        }
        renderScheduledTasks(stackID);
        const exit = resultRow?.exit_code;
        const failed = Number.isInteger(exit) && exit !== 0;
        setActionResult(`Script ${task.name} finished (exit ${exit ?? 'unknown'}).`, failed);
      } catch (err) {
        setActionResult(`Script ${task.name} failed: ${err?.message || String(err)}`, true);
        await fetchScheduledTasks(stackID, { silent: true });
      }
    };
    actionCell.appendChild(runBtn);
    tbody.appendChild(row);
  });

  host.appendChild(table);
}

async function fetchScheduledTasks(stackID, { silent = false } = {}) {
  if (!stackID) return false;
  try {
    const res = await fetch(`/v1/scripts/${encodeURIComponent(stackID)}`);
    if (!res.ok) throw new Error(`scheduled task request failed (${res.status})`);
    const rows = await res.json();
    scheduledTasksByStackID[stackID] = rows || [];
    if (selected?.type === 'compose' && selected.id === stackID) {
      renderScheduledTasks(stackID);
    }
    return true;
  } catch (err) {
    if (!silent) {
      setActionResult(`Scheduled tasks refresh failed: ${err?.message || String(err)}`, true);
    }
    return false;
  }
}

async function refreshSelectedDetails() {
  if (!selected) return;

  if (selected.type === 'container') {
    let c = null;
    for (let attempt = 0; attempt < 4; attempt += 1) {
      const res = await fetch(`/v1/containers/${encodeURIComponent(selected.id)}`);
      if (res.ok) {
        c = await res.json();
        break;
      }
      if (res.status === 404) {
        const refreshed = await fetchInventory({ silent: true, reconcile: false });
        if (refreshed) {
          const replacement = inventory.find((i) => i.type === 'container' && i.name === selected.name && i.target_id === selected.target_id);
          if (replacement && replacement.id !== selected.id) {
            selected = { ...selected, id: replacement.id, status: replacement.status };
            continue;
          }
        }
      }
      if (attempt < 3) {
        await new Promise((resolve) => setTimeout(resolve, 900));
      }
    }
    if (!c) {
      $('details').textContent = 'Failed to refresh container details after action.';
      return;
    }
    selected = {
      ...selected,
      id: c.id,
      name: c.name,
      target_id: c.target_id,
      status: c.status,
    };
    renderStats([
      ['Image', c.image],
      ['Restart count', c.restart_count],
      ['Uptime', c.uptime],
      ['Ports', (c.ports || []).join(', ') || '-'],
      ['Container ID', c.container_id],
      ['Host machine', c.target_id],
    ], { restart: c.restart });
    $('details').textContent = JSON.stringify(c, null, 2);
    $('title').textContent = c.name;
    $('subtitle').textContent = c.target_id;
    $('status').textContent = c.status;
    return;
  }

  if (selected.type === 'compose') {
    if (!stacksByID[selected.id]) {
      await fetchStacks({ silent: true });
    }
    delete composeChildrenByID[selected.id];
    const children = await ensureComposeChildren(selected.id);
    await fetchScheduledTasks(selected.id, { silent: true });
    const stackRow = stacksByID[selected.id];
    renderStats([
      ['Project', selected.name],
      ['Services', children.length],
      ['Host machine', selected.target_id],
      ['Status', selected.status],
      ['Stack ID', selected.id],
    ], { restart: stackRow?.restart || null });
    $('details').textContent = JSON.stringify(children, null, 2);
    renderScheduledTasks(selected.id);
  }
}

async function post(url) {
  const res = await fetch(url, { method: 'POST' });
  const body = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(body.error || `request failed (${res.status})`);
  return body;
}

function setActionResult(message, isError = false) {
  const el = $('actionResult');
  el.textContent = message;
  el.classList.remove('ok', 'err');
  el.classList.add(isError ? 'err' : 'ok');
}

function appendActionLogLine(line) {
  const out = $('actionLogs');
  if (!out) return;
  out.textContent += line + '\n';
  out.scrollTop = out.scrollHeight;
}

function appendCommandResultToActionLogs(result, label = 'command') {
  if (!result) return;
  appendActionLogLine(`[${label}] exit=${result.exit_code}`);
  const stdout = (result.stdout || '').trim();
  if (stdout) {
    stdout.split('\n').forEach((line) => appendActionLogLine(`[stdout] ${line}`));
  }
  const stderr = (result.stderr || '').trim();
  if (stderr) {
    stderr.split('\n').forEach((line) => appendActionLogLine(`[stderr] ${line}`));
  }
}

function stopActionLogCountdown() {
  if (actionLogCountdownTimer) {
    clearInterval(actionLogCountdownTimer);
    actionLogCountdownTimer = null;
  }
}

function isActionLogTabActive() {
  return document.querySelector('.tab.active')?.dataset.tab === 'actionLogs';
}

function setActionLogTabLabel() {
  const tab = $('tabActionLogs');
  if (!tab) return;
  if (!actionLogShouldAutoClose || isActionLogTabActive()) {
    tab.textContent = `${actionLogName} logs`;
    return;
  }
  tab.textContent = `${actionLogName} logs (will close in ${actionLogCountdownRemaining}s)`;
}

function hideActionLogTab() {
  stopActionLogCountdown();
  actionLogShouldAutoClose = false;
  actionLogCountdownRemaining = 0;
  const tab = $('tabActionLogs');
  const panel = $('panelActionLogs');
  if (tab) tab.hidden = true;
  if (panel) panel.hidden = true;
  if (document.querySelector('.tab.active')?.dataset.tab === 'actionLogs') {
    switchTab('logs');
  }
}

function maybeStartActionLogCloseCountdown() {
  if (!actionLogShouldAutoClose) return;
  if (isActionLogTabActive()) {
    stopActionLogCountdown();
    setActionLogTabLabel();
    return;
  }
  stopActionLogCountdown();
  setActionLogTabLabel();
  actionLogCountdownTimer = setInterval(() => {
    if (isActionLogTabActive()) {
      stopActionLogCountdown();
      setActionLogTabLabel();
      return;
    }
    actionLogCountdownRemaining -= 1;
    if (actionLogCountdownRemaining <= 0) {
      hideActionLogTab();
      return;
    }
    setActionLogTabLabel();
  }, 1000);
}

function queueActionLogAutoClose(seconds = 10) {
  actionLogShouldAutoClose = true;
  actionLogCountdownRemaining = seconds;
  const tab = $('tabActionLogs');
  if (!tab) return;
  setActionLogTabLabel();
  maybeStartActionLogCloseCountdown();
}

function showActionLogTab(actionName) {
  stopActionLogCountdown();
  actionLogShouldAutoClose = false;
  actionLogCountdownRemaining = 0;
  actionLogName = actionName;
  const tab = $('tabActionLogs');
  const panel = $('panelActionLogs');
  if (!tab || !panel) return;
  tab.hidden = false;
  panel.hidden = false;
  setActionLogTabLabel();
  $('actionLogs').textContent = '';
  switchTab('actionLogs');
}

async function runAction(label, fn, { showActionLogs = false } = {}) {
  try {
    setActionResult(`${label}...`);
    if (showActionLogs) {
      showActionLogTab(label);
      appendActionLogLine(`[meta] ${label} started`);
    }
    const activeTab = document.querySelector('.tab.active')?.dataset.tab;
    const result = await fn();
    if (showActionLogs) {
      appendCommandResultToActionLogs(result, label.toLowerCase());
      appendActionLogLine(`[meta] ${label} complete`);
      queueActionLogAutoClose(10);
    }
    setActionResult(`${label} complete${result && result.exit_code !== undefined ? ` (exit ${result.exit_code})` : ''}.`);
    await fetchInventory();
    await refreshSelectedDetails();
    if (activeTab) switchTab(activeTab);
  } catch (err) {
    const msg = err?.message || String(err);
    if (showActionLogs) {
      appendActionLogLine(`[meta] ${label} failed: ${msg}`);
      queueActionLogAutoClose(10);
    }
    setActionResult(`${label} failed: ${msg}`, true);
  }
}

async function waitForServiceRecovery(timeoutMs = 45000, intervalMs = 1500) {
  const started = Date.now();
  while (Date.now() - started < timeoutMs) {
    try {
      const res = await fetch('/healthz', { cache: 'no-store' });
      if (res.ok) return true;
    } catch (_) {
      // ignore transient reconnect failures while CCM restarts
    }
    await new Promise((resolve) => setTimeout(resolve, intervalMs));
  }
  return false;
}

async function reloadPageAfterCCMRecovery() {
  setActionResult('Waiting for CCM recovery...');
  const recovered = await waitForServiceRecovery(60000, 1500);
  if (!recovered) {
    setActionResult('CCM did not recover within 60s after redeploy attempt.', true);
    return;
  }
  setActionResult('CCM is back online after redeploy. Reloading page...');
  setTimeout(() => window.location.reload(), 300);
}

$('search').addEventListener('input', renderItems);
$('btnPause').onclick = () => {
  paused = !paused;
  $('btnPause').textContent = paused ? 'Resume' : 'Pause';
};
$('btnClear').onclick = () => {
  resetLogOutput();
};
$('btnCopyLast100').onclick = async () => {
  flushLogOutput();
  const lines = logLines.slice();
  const tail = lines.slice(-100).join('\n');
  if (!tail) {
    setActionResult('No log lines to copy.', true);
    return;
  }
  try {
    await navigator.clipboard.writeText(tail + '\n');
    setActionResult(`Copied ${Math.min(100, lines.length)} log lines.`);
  } catch (err) {
    setActionResult(`Copy failed: ${err?.message || String(err)}`, true);
  }
};
$('btnPopoutRaw').onclick = () => {
  if (selected?.type !== 'container') {
    setActionResult('Select a container first.', true);
    return;
  }
  // Backend allows one log-stream SSH slot per target, so hand off stream to popout.
  stopLogs();
  const url = `/raw-logs.html?id=${encodeURIComponent(selected.id)}&tail=200`;
  window.open(url, '_blank', 'noopener,noreferrer');
  setActionResult('Opened raw log popout (main stream paused).');
};
$('btnScheduledRefresh').onclick = async () => {
  if (selected?.type !== 'compose') {
    setActionResult('Select a compose stack first.', true);
    return;
  }
  await fetchScheduledTasks(selected.id);
  renderScheduledTasks(selected.id);
  setActionResult('Scheduled tasks refreshed.');
};
$('btnStart').onclick = async () => {
  if (selected?.type !== 'container') {
    setActionResult('Select a container first.', true);
    return;
  }
  await runAction('Start', () => post(`/v1/containers/${encodeURIComponent(selected.id)}/start`));
};
$('btnStop').onclick = async () => {
  if (selected?.type !== 'container') {
    setActionResult('Select a container first.', true);
    return;
  }
  await runAction('Stop', () => post(`/v1/containers/${encodeURIComponent(selected.id)}/stop`));
};
$('btnRestart').onclick = async () => {
  if (selected?.type !== 'container') {
    setActionResult('Select a container first.', true);
    return;
  }
  await runAction('Restart', () => post(`/v1/containers/${encodeURIComponent(selected.id)}/restart`), { showActionLogs: true });
};
$('btnRedeploy').onclick = async () => {
  if (selected?.type !== 'compose') {
    setActionResult('Select a compose stack first.', true);
    return;
  }
  const redeployURL = `/v1/compose/${encodeURIComponent(selected.id)}/redeploy`;
  try {
    setActionResult('Redeploying...');
    const isCCMSelfRedeploy = selected.id === 'ccm';
    if (!isCCMSelfRedeploy) {
      showActionLogTab('Redeploy');
      appendActionLogLine('[meta] Redeploy started');
      appendActionLogLine(`[meta] Stack: ${selected.id}`);
    }
    const out = await post(redeployURL);
    const resolvedLogPath = (out?.log_path && out?.deploy_path && !String(out.log_path).startsWith('/'))
      ? `${out.deploy_path}/${out.log_path}`
      : out?.log_path;
    const pid = out?.steps?.[1]?.stdout?.trim?.() || out?.steps?.[0]?.stdout?.trim?.();
    if (out?.async && out?.log_path) {
      setActionResult(`Redeploy started in background${pid ? ` (pid ${pid})` : ''}. Log: ${resolvedLogPath}`);
      if (out?.stack === 'ccm') {
        await reloadPageAfterCCMRecovery();
        return;
      }
    } else if (out?.log_path) {
      setActionResult(`Redeploy complete. Log: ${resolvedLogPath}`);
    } else {
      setActionResult('Redeploy complete.');
    }
    if (!isCCMSelfRedeploy) {
      appendActionLogLine(`[meta] Log path: ${resolvedLogPath || '(none)'}`);
      const steps = Array.isArray(out?.steps) ? out.steps : [];
      steps.forEach((step, idx) => appendCommandResultToActionLogs(step, `step-${idx + 1}`));
      appendActionLogLine('[meta] Redeploy complete');
      queueActionLogAutoClose(10);
    }
    await fetchInventory();
  } catch (err) {
    const msg = err?.message || String(err);
    if (msg.includes('502')) {
      setActionResult('Redeploy request interrupted while CCM restarted.');
      await reloadPageAfterCCMRecovery();
      return;
    }
    if (selected?.id !== 'ccm') {
      appendActionLogLine(`[meta] Redeploy failed: ${msg}`);
      queueActionLogAutoClose(10);
    }
    setActionResult(`Redeploy failed: ${msg}`, true);
  }
};

function switchTab(tab) {
  if (tab === 'actionLogs' && $('tabActionLogs')?.hidden) {
    return;
  }
  if (tab === 'scheduled' && $('tabScheduled')?.hidden) {
    return;
  }
  const previousTab = document.querySelector('.tab.active')?.dataset.tab;
  document.querySelectorAll('.tab').forEach((t) => {
    t.classList.toggle('active', t.dataset.tab === tab);
  });
  document.querySelectorAll('.panel').forEach((p) => p.classList.remove('panel-active'));
  const panel = document.getElementById(`panel${tab[0].toUpperCase() + tab.slice(1)}`);
  if (panel) panel.classList.add('panel-active');
  if (tab === 'actionLogs') {
    stopActionLogCountdown();
    setActionLogTabLabel();
  } else if (previousTab === 'actionLogs') {
    maybeStartActionLogCloseCountdown();
  }
}

document.querySelectorAll('.tab').forEach(btn => btn.onclick = () => switchTab(btn.dataset.tab));

function tickClock() {
  const now = new Date();
  $('clock').textContent = now.toLocaleTimeString();
}

document.addEventListener('visibilitychange', () => {
  if (document.hidden) {
    stopLogs({ clearSelection: false });
    return;
  }
  reconnectLogsIfNeeded();
});

(async function init() {
  setStreamIndicator('inactive');
  tickClock();
  setInterval(tickClock, 1000);
  await fetchInventory({ silent: true, reconcile: false });
  await fetchStacks({ silent: true });
  setInterval(() => {
    fetchInventory({ silent: true, reconcile: false });
  }, 4000);
  setInterval(() => {
    fetchStacks({ silent: true });
  }, 15000);
  setInterval(() => {
    if (selected?.type === 'compose') {
      fetchScheduledTasks(selected.id, { silent: true });
    }
  }, 15000);
})();
