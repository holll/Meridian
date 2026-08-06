// Relay nodes management page
let relayRefreshTimer = null;

const ispLabel = { telecom: '电信', unicom: '联通', mobile: '移动', hk: '港澳', oversea: '海外' };

function renderRelay() {
  const page = document.getElementById('page-relay');
  page.innerHTML = `
    <h1 class="section-title fade-up">节点管理</h1>
    <p class="section-sub fade-up stagger-1">Relay 分布式节点状态与流量贡献</p>
    <div class="stats-row" id="relay-stats">
      <div class="stat-card c-blue fade-up stagger-1">
        <div class="stat-icon-wrap blue">
          <svg viewBox="0 0 24 24"><circle cx="12" cy="12" r="10"/><path d="M2 12h20M12 2a15.3 15.3 0 0 1 4 10 15.3 15.3 0 0 1-4 10 15.3 15.3 0 0 1-4-10 15.3 15.3 0 0 1 4-10z"/></svg>
        </div>
        <div class="stat-number" id="relay-total">—</div>
        <div class="stat-title">节点总数</div>
      </div>
      <div class="stat-card c-green fade-up stagger-2">
        <div class="stat-icon-wrap green">
          <svg viewBox="0 0 24 24"><path d="M22 11.08V12a10 10 0 1 1-5.93-9.14"/><polyline points="22 4 12 14.01 9 11.01"/></svg>
        </div>
        <div class="stat-number" id="relay-online">—</div>
        <div class="stat-title">在线节点</div>
      </div>
      <div class="stat-card c-teal fade-up stagger-3">
        <div class="stat-icon-wrap teal">
          <svg viewBox="0 0 24 24"><polyline points="22 12 18 12 15 21 9 3 6 12 2 12"/></svg>
        </div>
        <div class="stat-number" id="relay-traffic-in">0 B</div>
        <div class="stat-title">节点流量入</div>
      </div>
      <div class="stat-card c-orange fade-up stagger-4">
        <div class="stat-icon-wrap orange">
          <svg viewBox="0 0 24 24"><polyline points="22 12 18 12 15 21 9 3 6 12 2 12"/></svg>
        </div>
        <div class="stat-number" id="relay-traffic-out">0 B</div>
        <div class="stat-title">节点流量出</div>
      </div>
    </div>
    <div class="glass-card fade-up stagger-4">
      <div class="glass-card-header">
        <div class="glass-card-title">节点列表</div>
        <div style="display:flex;gap:8px">
          <button class="btn-relay-refresh" id="btn-relay-install-cmd" title="复制新节点安装命令">
            <svg viewBox="0 0 24 24"><rect x="9" y="9" width="13" height="13" rx="2"/><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"/></svg>
          </button>
          <button class="btn-relay-refresh" id="btn-relay-refresh" title="刷新">
            <svg viewBox="0 0 24 24"><polyline points="23 4 23 10 17 10"/><polyline points="1 20 1 14 7 14"/><path d="M3.51 9a9 9 0 0 1 14.85-3.36L23 10M1 14l4.64 4.36A9 9 0 0 0 20.49 15"/></svg>
          </button>
        </div>
      </div>
      <div id="relay-table-wrap" style="overflow-x:auto">
        <table>
          <thead><tr>
            <th>节点名称</th><th>运营商</th><th>公网 IP</th><th>版本</th>
            <th>最后心跳</th><th>流量入</th><th>流量出</th><th>状态</th><th>操作</th>
          </tr></thead>
          <tbody id="relay-table"><tr><td colspan="9" class="relay-loading">加载中...</td></tr></tbody>
        </table>
      </div>
    </div>
  `;

  document.getElementById('btn-relay-refresh').onclick = loadRelayNodes;
  document.getElementById('btn-relay-install-cmd').onclick = showInstallCmdModal;
  loadRelayNodes();

  if (relayRefreshTimer) clearInterval(relayRefreshTimer);
  relayRefreshTimer = setInterval(() => {
    if (Router.current === 'relay') loadRelayNodes();
  }, 30000);
}

// showInstallCmdModal fetches the one-line install command and lets the
// operator pick a node name before copying it to the clipboard.
async function showInstallCmdModal() {
  let command;
  try {
    const data = await API.getRelayInstallCmd();
    command = data.command;
  } catch (e) {
    Toast.error(e.message);
    return;
  }

  document.getElementById('modal-title').textContent = '安装新节点';
  document.getElementById('modal-body').innerHTML = `
    <div class="form-group">
      <label>节点名称（全局唯一，如 Unicom-SH）</label>
      <input type="text" class="form-input" id="relay-node-name" placeholder="如：Unicom-SH" maxlength="100" autocomplete="off" spellcheck="false">
    </div>
    <div class="form-group">
      <label>安装命令（随节点名称更新，点击复制）</label>
      <textarea readonly class="form-input mono" id="relay-install-cmd" rows="4" style="font-size:.76rem;white-space:pre-wrap"></textarea>
    </div>`;
  document.getElementById('modal-footer').innerHTML = `
    <button class="btn-modal secondary" id="relay-cmd-cancel">取消</button>
    <button class="btn-modal primary" id="relay-cmd-copy">复制命令</button>`;

  const nameInput = document.getElementById('relay-node-name');
  const cmdArea = document.getElementById('relay-install-cmd');
  const renderCmd = () => {
    const name = nameInput.value.trim() || '__NODE__';
    cmdArea.value = command.replace('__NODE__', name);
  };
  nameInput.addEventListener('input', renderCmd);
  renderCmd();

  document.getElementById('relay-cmd-cancel').addEventListener('click', closeModal);
  document.getElementById('relay-cmd-copy').addEventListener('click', async () => {
    await copyToClipboard(cmdArea.value);
    closeModal();
    Toast.success('安装命令已复制');
  });

  openModal({ closeOnBackdrop: true });
  nameInput.focus();
}

// updateRelayNode sends a one-shot self-update signal to a node; the node
// performs the update on its next heartbeat (60s).
window.updateRelayNode = async function(name) {
  try {
    await API.updateRelayNode(name);
    Toast.success('更新指令已下发，节点将在下次心跳时执行（约 1 分钟内）');
  } catch (e) {
    Toast.error('下发失败：' + e.message);
  }
};

async function copyToClipboard(text) {
  try {
    await navigator.clipboard.writeText(text);
  } catch (e) {
    const ta = document.createElement('textarea');
    ta.value = text;
    document.body.appendChild(ta);
    ta.select();
    document.execCommand('copy');
    ta.remove();
  }
}

function stopRelayRefresh() {
  if (relayRefreshTimer) {
    clearInterval(relayRefreshTimer);
    relayRefreshTimer = null;
  }
}

async function loadRelayNodes() {
  const refreshBtn = document.getElementById('btn-relay-refresh');
  if (refreshBtn) refreshBtn.classList.add('spinning');

  try {
    const data = await API.getRelayNodes();
    const nodes = data.nodes || [];
    const now = Math.floor(Date.now() / 1000);

    // Update stat cards
    const onlineCount = nodes.filter(n => now - n.last_seen < 300).length;
    const totalIn  = nodes.reduce((s, n) => s + (n.traffic_in  || 0), 0);
    const totalOut = nodes.reduce((s, n) => s + (n.traffic_out || 0), 0);

    setText('relay-total',       nodes.length);
    setText('relay-online',      onlineCount);
    setText('relay-traffic-in',  formatBytes(totalIn));
    setText('relay-traffic-out', formatBytes(totalOut));

    // Render table
    const tbody = document.getElementById('relay-table');
    if (!tbody) return;

    if (nodes.length === 0) {
      tbody.innerHTML = '<tr><td colspan="9" class="relay-empty">暂无注册节点。部署 meridian-relay 后节点将自动出现在此处。</td></tr>';
      return;
    }

    tbody.innerHTML = nodes.map(n => {
      const online  = now - n.last_seen < 300;
      const stale   = !online && n.last_seen > 0;
      const ago     = n.last_seen > 0 ? relayTimeAgo(now - n.last_seen) : '从未';
      const ispText = ispLabel[n.isp] || (n.isp ? esc(n.isp) : '<span style="color:var(--white-38)">—</span>');
      const ipText  = n.public_ip ? `<span class="mono">${esc(n.public_ip)}</span>` : '<span style="color:var(--white-38)">—</span>';
      const verText = n.version   ? `<span class="mono relay-ver">${esc(n.version)}</span>` : '<span style="color:var(--white-38)">—</span>';

      return `<tr>
        <td style="font-weight:600">${esc(n.name)}</td>
        <td>${ispText}</td>
        <td>${ipText}</td>
        <td>${verText}</td>
        <td><span class="relay-ago ${online ? 'fresh' : stale ? 'stale' : ''}">${esc(ago)}</span></td>
        <td>${formatBytes(n.traffic_in  || 0)}</td>
        <td>${formatBytes(n.traffic_out || 0)}</td>
        <td><span class="status-badge">
          <span class="status-led ${online ? 'on' : 'off'}"></span>
          ${online ? '在线' : '离线'}
        </span></td>
        <td><button class="btn-relay-refresh" title="一键更新此节点（节点将在下次心跳时执行）" onclick="updateRelayNode('${esc(n.name)}')">
          <svg viewBox="0 0 24 24"><polyline points="23 4 23 10 17 10"/><polyline points="1 20 1 14 7 14"/><path d="M3.51 9a9 9 0 0 1 14.85-3.36L23 10M1 14l4.64 4.36A9 9 0 0 0 20.49 15"/></svg>
        </button></td>
      </tr>`;
    }).join('');
  } catch (e) {
    const tbody = document.getElementById('relay-table');
    if (tbody) {
      tbody.innerHTML = '<tr><td colspan="8" class="relay-empty" style="color:var(--red)">加载失败：' + esc(e.message) + '</td></tr>';
    }
  } finally {
    if (refreshBtn) refreshBtn.classList.remove('spinning');
  }
}

function relayTimeAgo(seconds) {
  if (seconds < 60)   return seconds + ' 秒前';
  if (seconds < 3600) return Math.floor(seconds / 60) + ' 分钟前';
  if (seconds < 86400) return Math.floor(seconds / 3600) + ' 小时前';
  return Math.floor(seconds / 86400) + ' 天前';
}

function setText(id, val) {
  const el = document.getElementById(id);
  if (el) el.textContent = val;
}
