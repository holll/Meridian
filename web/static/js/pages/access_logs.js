// Access logs page — per-request logs reported by relay nodes
let accessLogRefreshTimer = null;
let accessLogPage = 1;
let accessLogLoadSeq = 0; // guards against stale responses overwriting newer ones
const accessLogRefreshSeconds = 30;
let accessLogCountdown = accessLogRefreshSeconds;

function renderAccessLogs() {
  const page = document.getElementById('page-access-logs');
  page.innerHTML = `
    <h1 class="section-title fade-up">访问日志</h1>
    <p class="section-sub fade-up stagger-1">Relay 节点上报的逐请求访问日志</p>
    <div class="controls-row fade-up stagger-1">
      <select class="form-select" id="alog-site-select"><option value="">全部站点</option></select>
      <select class="form-select" id="alog-node-select"><option value="">全部节点</option></select>
      <button class="btn-login" id="btn-alog-refresh" style="width:auto;padding:8px 18px">刷新</button>
    </div>
    <div class="glass-card fade-up stagger-2">
      <div class="glass-card-header">
        <div class="glass-card-title">日志明细</div>
        <span class="alog-countdown" id="alog-countdown"></span>
      </div>
      <div style="overflow-x:auto">
        <table>
          <thead><tr>
            <th>时间</th><th>节点</th><th>站点</th><th>客户端 IP</th><th>归属</th><th>方法</th>
            <th>状态</th><th>路径</th><th>延迟</th><th>入站</th><th>出站</th>
          </tr></thead>
          <tbody id="alog-table"><tr><td colspan="11" class="relay-loading">加载中...</td></tr></tbody>
        </table>
      </div>
      <div class="alog-pager" id="alog-pager"></div>
    </div>
  `;

  document.getElementById('alog-site-select').onchange = () => { accessLogPage = 1; loadAccessLogs(); };
  document.getElementById('alog-node-select').onchange = () => { accessLogPage = 1; loadAccessLogs(); };
  document.getElementById('btn-alog-refresh').onclick = () => loadAccessLogs(false);

  loadAccessLogFilters();
  loadAccessLogs();
  scheduleAccessLogRefresh();
}

function scheduleAccessLogRefresh() {
  accessLogCountdown = accessLogRefreshSeconds;
  updateAccessLogCountdown();
  if (accessLogRefreshTimer) clearInterval(accessLogRefreshTimer);
  accessLogRefreshTimer = setInterval(() => {
    accessLogCountdown--;
    updateAccessLogCountdown();
    if (accessLogCountdown <= 0) {
      if (Router.current === 'access-logs') loadAccessLogs(true);
      accessLogCountdown = accessLogRefreshSeconds;
      updateAccessLogCountdown();
    }
  }, 1000);
}

function updateAccessLogCountdown() {
  const el = document.getElementById('alog-countdown');
  if (el) el.textContent = `${accessLogCountdown}s 后自动刷新`;
}

function stopAccessLogRefresh() {
  if (accessLogRefreshTimer) {
    clearInterval(accessLogRefreshTimer);
    accessLogRefreshTimer = null;
  }
}

async function loadAccessLogFilters() {
  try {
    await loadLogFilterSelects('alog-site-select', 'alog-node-select');
  } catch (e) {
    Toast.error('加载筛选选项失败');
  }
}

async function loadAccessLogs(silent) {
  const tbody = document.getElementById('alog-table');
  if (!tbody) return;

  scheduleAccessLogRefresh(); // any load restarts the auto-refresh countdown
  const siteId = document.getElementById('alog-site-select').value;
  const relayName = document.getElementById('alog-node-select').value;
  const seq = ++accessLogLoadSeq;

  try {
    const data = await API.getAccessLogs({
      site_id: siteId,
      relay_name: relayName,
      page: accessLogPage,
      page_size: 50,
    });
    if (seq !== accessLogLoadSeq) return; // a newer request superseded this one
    const logs = data.logs || [];
    const total = data.total || 0;

    // Total may shrink after retention cleanup; reload if the current page
    // fell out of range so the page indicator stays consistent with the data.
    const totalPages = Math.max(1, Math.ceil(total / 50));
    if (accessLogPage > totalPages) {
      accessLogPage = totalPages;
      return loadAccessLogs(silent);
    }

    if (logs.length === 0) {
      tbody.innerHTML = '<tr><td colspan="11" class="relay-empty">暂无访问日志。Relay 节点产生请求后日志将在此显示。</td></tr>';
      renderPager(total);
      return;
    }

    tbody.innerHTML = logs.map(l => `
      <tr>
        <td class="mono">${formatLogTime(l.timestamp)}</td>
        <td>${esc(l.relay_name)}</td>
        <td>${l.site_name ? esc(l.site_name) : `<span style="color:var(--white-38)">site#${l.site_id}</span>`}</td>
        <td><span class="mono">${esc(l.client_ip)}</span></td>
        <td>${geoCell(l.geo)}</td>
        <td><span class="alog-method">${esc(l.method)}</span></td>
        <td><span class="alog-status" style="color:${statusColor(l.status)}">${l.status}</span></td>
        <td style="max-width:320px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap" title="${esc(l.path)}">${esc(l.path)}</td>
        <td>${formatLatency(l.latency_ms)}</td>
        <td>${formatBytes(l.bytes_in)}</td>
        <td>${formatBytes(l.bytes_out)}</td>
      </tr>
    `).join('');
    renderPager(total);
  } catch (e) {
    if (!silent) {
      tbody.innerHTML = '<tr><td colspan="11" class="relay-empty" style="color:var(--red)">加载失败：' + esc(e.message) + '</td></tr>';
    }
  }
}

function renderPager(total) {
  const pager = document.getElementById('alog-pager');
  if (!pager) return;
  const pageSize = 50;
  const totalPages = Math.max(1, Math.ceil(total / pageSize));
  pager.innerHTML = `
    <button class="btn-login" id="alog-prev" style="width:auto;padding:6px 14px" ${accessLogPage <= 1 ? 'disabled' : ''}>上一页</button>
    <span class="alog-page-info">第 ${accessLogPage} / ${totalPages} 页 · 共 ${total} 条</span>
    <button class="btn-login" id="alog-next" style="width:auto;padding:6px 14px" ${accessLogPage >= totalPages ? 'disabled' : ''}>下一页</button>
  `;
  document.getElementById('alog-prev').onclick = () => { if (accessLogPage > 1) { accessLogPage--; loadAccessLogs(); } };
  document.getElementById('alog-next').onclick = () => { if (accessLogPage < totalPages) { accessLogPage++; loadAccessLogs(); } };
}
