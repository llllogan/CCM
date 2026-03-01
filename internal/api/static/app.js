let inventory = [];
let selected = null;
let stream = null;
let streamContainerID = null;
let paused = false;
const composeChildrenByID = {};
const expandedCompose = new Set();

const $ = (id) => document.getElementById(id);

async function fetchInventory() {
  const res = await fetch('/v1/inventory');
  const data = await res.json();
  inventory = data.items || [];
  renderItems();
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
      if (selected && selected.id === item.id) row.classList.add('active');
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
          if (selected && selected.id === c.id) child.classList.add('active');
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
    const res = await fetch(`/v1/containers/${encodeURIComponent(item.id)}`);
    if (!res.ok) {
      $('details').textContent = `Failed to load container details (${res.status})`;
      return;
    }
    const c = await res.json();
    renderStats([
      ['Image', c.image],
      ['Restart count', c.restart_count],
      ['Uptime', c.uptime],
      ['Ports', (c.ports || []).join(', ') || '-'],
      ['Container ID', c.container_id],
      ['Host machine', c.target_id],
    ]);
    $('details').textContent = JSON.stringify(c, null, 2);
    startLogs(c.id);
    switchTab('logs');
  } else if (item.type === 'compose') {
    const children = await ensureComposeChildren(item.id);
    renderStats([
      ['Project', item.name],
      ['Services', children.length],
      ['Host machine', item.target_id],
      ['Status', item.status],
      ['Stack ID', item.id],
    ]);
    $('details').textContent = JSON.stringify(children, null, 2);
    stopLogs();
    switchTab('details');
  } else {
    renderStats([['Error', item.name]]);
    $('details').textContent = JSON.stringify(item, null, 2);
    stopLogs();
  }
}

function renderStats(items) {
  $('stats').innerHTML = items.map(([k, v]) => `<div class="stat"><div class="k">${k}</div><div class="v">${v}</div></div>`).join('');
}

function setStreamIndicator(state) {
  const dot = $('streamDot');
  if (!dot) return;
  dot.classList.remove('is-active', 'is-inactive', 'is-connecting');
  if (state === 'active') {
    dot.classList.add('is-active');
    dot.title = 'Log stream connected';
    return;
  }
  if (state === 'connecting') {
    dot.classList.add('is-connecting');
    dot.title = 'Log stream reconnecting';
    return;
  }
  dot.classList.add('is-inactive');
  dot.title = 'Log stream disconnected';
}

function stopLogs({ clearSelection = true } = {}) {
  if (stream) {
    stream.close();
    stream = null;
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
    $('logs').textContent = '';
  }
  setStreamIndicator('connecting');
  stream = new EventSource(`/v1/containers/${encodeURIComponent(id)}/logs/stream?tail=200`);
  stream.onopen = () => {
    setStreamIndicator('active');
  };
  stream.onmessage = (evt) => {
    if (paused) return;
    $('logs').textContent += evt.data + '\n';
    if ($('autoScroll').checked) {
      $('logs').scrollTop = $('logs').scrollHeight;
    }
  };
  stream.onerror = () => {
    setStreamIndicator('inactive');
    $('logs').textContent += '[stream error or disconnected]\n';
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

async function runAction(label, fn) {
  try {
    setActionResult(`${label}...`);
    const result = await fn();
    setActionResult(`${label} complete${result && result.exit_code !== undefined ? ` (exit ${result.exit_code})` : ''}.`);
    await fetchInventory();
  } catch (err) {
    const msg = err?.message || String(err);
    setActionResult(`${label} failed: ${msg}`, true);
  }
}

$('search').addEventListener('input', renderItems);
$('btnPause').onclick = () => {
  paused = !paused;
  $('btnPause').textContent = paused ? 'Resume' : 'Pause';
};
$('btnClear').onclick = () => {
  $('logs').textContent = '';
};
$('btnCopyLast100').onclick = async () => {
  const lines = $('logs').textContent.split('\n').filter(Boolean);
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
  await runAction('Restart', () => post(`/v1/containers/${encodeURIComponent(selected.id)}/restart`));
};
$('btnRedeploy').onclick = async () => {
  if (selected?.type !== 'compose') {
    setActionResult('Select a compose stack first.', true);
    return;
  }
  await runAction('Redeploy', () => post(`/v1/compose/${encodeURIComponent(selected.id)}/redeploy`));
};

function switchTab(tab) {
  document.querySelectorAll('.tab').forEach((t) => {
    t.classList.toggle('active', t.dataset.tab === tab);
  });
  document.querySelectorAll('.panel').forEach((p) => p.classList.remove('panel-active'));
  document.getElementById(`panel${tab[0].toUpperCase() + tab.slice(1)}`).classList.add('panel-active');
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
  await fetchInventory();
  setInterval(fetchInventory, 4000);
})();
