let inventory = [];
let selected = null;
let stream = null;
let paused = false;

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
      row.innerHTML = `<div>${item.name}</div><div class="meta">${item.type} | ${item.target_id} | ${item.status}</div>`;
      row.onclick = () => selectItem(item);
      host.appendChild(row);
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
    const c = await res.json();
    renderStats([
      ['Image', c.image],
      ['Restart count', c.restart_count],
      ['Uptime', c.uptime],
      ['Ports', (c.ports || []).join(', ') || '-'],
      ['Container ID', c.container_id],
      ['Target', c.target_id],
    ]);
    $('details').textContent = JSON.stringify(c, null, 2);
    startLogs(c.id);
  } else if (item.type === 'compose') {
    const res = await fetch(`/v1/items/${encodeURIComponent(item.id)}/children`);
    const children = await res.json();
    renderStats([
      ['Project', item.name],
      ['Services', children.length],
      ['Target', item.target_id],
      ['Status', item.status],
      ['Stack ID', item.id],
    ]);
    $('details').textContent = JSON.stringify(children, null, 2);
    stopLogs();
  } else {
    renderStats([['Error', item.name]]);
    $('details').textContent = JSON.stringify(item, null, 2);
    stopLogs();
  }
}

function renderStats(items) {
  $('stats').innerHTML = items.map(([k, v]) => `<div class="stat"><div class="k">${k}</div><div class="v">${v}</div></div>`).join('');
}

function stopLogs() {
  if (stream) {
    stream.close();
    stream = null;
  }
}

function startLogs(id) {
  stopLogs();
  $('logs').textContent = '';
  stream = new EventSource(`/v1/containers/${encodeURIComponent(id)}/logs/stream?tail=200`);
  stream.onmessage = (evt) => {
    if (paused) return;
    $('logs').textContent += evt.data + '\n';
    if ($('autoScroll').checked) {
      $('logs').scrollTop = $('logs').scrollHeight;
    }
  };
}

async function post(url) {
  const token = localStorage.getItem('ccm_token') || '';
  const headers = token ? { Authorization: `Bearer ${token}` } : {};
  const res = await fetch(url, { method: 'POST', headers });
  const body = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(body.error || `request failed (${res.status})`);
  return body;
}

$('search').addEventListener('input', renderItems);
$('btnPause').onclick = () => {
  paused = !paused;
  $('btnPause').textContent = paused ? 'Resume' : 'Pause';
};
$('btnClear').onclick = () => {
  $('logs').textContent = '';
};
$('btnStart').onclick = async () => {
  if (selected?.type === 'container') await post(`/v1/containers/${encodeURIComponent(selected.id)}/start`);
};
$('btnStop').onclick = async () => {
  if (selected?.type === 'container') await post(`/v1/containers/${encodeURIComponent(selected.id)}/stop`);
};
$('btnRestart').onclick = async () => {
  if (selected?.type === 'container') await post(`/v1/containers/${encodeURIComponent(selected.id)}/restart`);
};
$('btnRedeploy').onclick = async () => {
  if (selected?.type === 'compose') await post(`/v1/compose/${encodeURIComponent(selected.id)}/redeploy`);
};

document.querySelectorAll('.tab').forEach(btn => btn.onclick = () => {
  document.querySelectorAll('.tab').forEach(t => t.classList.remove('active'));
  btn.classList.add('active');
  document.querySelectorAll('.panel').forEach(p => p.classList.remove('panel-active'));
  document.getElementById(`panel${btn.dataset.tab[0].toUpperCase() + btn.dataset.tab.slice(1)}`).classList.add('panel-active');
});

function tickClock() {
  const now = new Date();
  $('clock').textContent = now.toLocaleTimeString();
}

(async function init() {
  tickClock();
  setInterval(tickClock, 1000);
  await fetchInventory();
  setInterval(fetchInventory, 4000);
})();
