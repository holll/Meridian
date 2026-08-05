// Access log analysis page — aggregations over relay-reported access logs
function renderAccessAnalysis() {
  const page = document.getElementById('page-access-analysis');
  page.innerHTML = `
    <h1 class="section-title fade-up">日志分析</h1>
    <p class="section-sub fade-up stagger-1">基于 Relay 访问日志的请求与流量分析</p>
    <div class="controls-row fade-up stagger-1">
      <select class="form-select" id="aan-site-select"><option value="">全部站点</option></select>
      <select class="form-select" id="aan-node-select"><option value="">全部节点</option></select>
      <select class="form-select" id="aan-range-select">
        <option value="24">最近 24 小时</option>
        <option value="168">最近 7 天</option>
      </select>
      <button class="btn-login" id="btn-aan-refresh" style="width:auto;padding:8px 18px">刷新</button>
    </div>
    <div class="stats-row" id="aan-stats"></div>
    <div class="chart-wrap fade-up">
      <div class="chart-head">
        <h3>请求量与出站流量趋势</h3>
        <div class="chart-legend">
          <div class="legend-item"><div class="legend-dot" style="background:#1e63ff"></div>请求数</div>
          <div class="legend-item"><div class="legend-dot out"></div>出站流量</div>
        </div>
      </div>
      <canvas id="aanChart"></canvas>
    </div>
    <div class="aan-cols fade-up">
      <div class="glass-card">
        <div class="glass-card-header"><div class="glass-card-title">状态码分布</div></div>
        <div class="aan-status-grid" id="aan-status"></div>
      </div>
      <div class="glass-card">
        <div class="glass-card-header"><div class="glass-card-title">TOP 资源</div></div>
        <div style="overflow-x:auto"><table>
          <thead><tr><th>路径</th><th>次数</th><th>占比</th><th>流量</th></tr></thead>
          <tbody id="aan-paths"></tbody>
        </table></div>
      </div>
    </div>
    <div class="aan-cols fade-up">
      <div class="glass-card">
        <div class="glass-card-header"><div class="glass-card-title">TOP 国家/地区</div></div>
        <div style="overflow-x:auto"><table>
          <thead><tr><th>国家/地区</th><th>次数</th><th>占比</th><th>流量</th></tr></thead>
          <tbody id="aan-countries"></tbody>
        </table></div>
      </div>
      <div class="glass-card">
        <div class="glass-card-header"><div class="glass-card-title">TOP 运营商</div></div>
        <div style="overflow-x:auto"><table>
          <thead><tr><th>运营商</th><th>次数</th><th>占比</th><th>流量</th></tr></thead>
          <tbody id="aan-orgs"></tbody>
        </table></div>
      </div>
    </div>
    <div class="glass-card fade-up">
      <div class="glass-card-header"><div class="glass-card-title">TOP 客户端 IP</div></div>
      <div style="overflow-x:auto"><table>
        <thead><tr><th>IP</th><th>归属</th><th>次数</th><th>占比</th><th>出站流量</th><th>平均延迟</th></tr></thead>
        <tbody id="aan-ips"></tbody>
      </table></div>
    </div>
  `;

  loadAnalysisFilters();
  document.getElementById('aan-site-select').onchange = loadAnalysis;
  document.getElementById('aan-node-select').onchange = loadAnalysis;
  document.getElementById('aan-range-select').onchange = loadAnalysis;
  document.getElementById('btn-aan-refresh').onclick = loadAnalysis;
}

async function loadAnalysisFilters() {
  try {
    await loadLogFilterSelects('aan-site-select', 'aan-node-select');
  } catch (e) {
    Toast.error('加载筛选选项失败');
  }
  loadAnalysis();
}

async function loadAnalysis() {
  const siteId = document.getElementById('aan-site-select').value;
  const relayName = document.getElementById('aan-node-select').value;
  const hours = parseInt(document.getElementById('aan-range-select').value) || 24;
  const now = Math.floor(Date.now() / 1000);

  try {
    const stats = await API.getAccessLogStats({
      site_id: siteId,
      relay_name: relayName,
      from: now - hours * 3600,
      to: now,
    });

    const trend = stats.trend || [];
    const status = stats.status || [];
    const paths = stats.top_paths || [];
    const ips = stats.top_ips || [];
    const totalReq = trend.reduce((s, t) => s + (t.requests || 0), 0);

    document.getElementById('aan-stats').innerHTML = `
      <div class="stat-card c-blue fade-up">
        <div class="stat-number">${totalReq}</div>
        <div class="stat-title">请求总数</div>
      </div>
      <div class="stat-card c-teal fade-up">
        <div class="stat-number">${formatLatency(stats.avg_latency_ms || 0)}</div>
        <div class="stat-title">平均延迟</div>
      </div>
      <div class="stat-card c-orange fade-up">
        <div class="stat-number">${formatLatency(stats.max_latency_ms || 0)}</div>
        <div class="stat-title">最大延迟</div>
      </div>
      <div class="stat-card c-green fade-up">
        <div class="stat-number">${status.length}</div>
        <div class="stat-title">状态码种类</div>
      </div>
    `;

    drawTrendChart(trend, hours);
    renderStatusBars(status, totalReq);
    renderTopList('aan-paths', paths.map(p =>
      p.is_other
        ? [`<span style="color:var(--white-38)">${esc(p.path)}</span>`, p.count, p.bytes]
        : [esc(p.path), p.count, p.bytes]));
    renderTopList('aan-countries', (stats.countries || []).map(c => [c.code ? `${c.code} · ${esc(c.name)}` : esc(c.name), c.count, c.bytes]));
    renderTopList('aan-orgs', (stats.orgs || []).map(o => [esc(o.name), o.count, o.bytes]));
    renderTopIPs(ips);
  } catch (e) {
    Toast.error('加载分析数据失败：' + e.message);
  }
}

function drawTrendChart(trend, hours) {
  const canvas = document.getElementById('aanChart');
  if (!canvas) return;
  const ctx = canvas.getContext('2d');
  const dpr = window.devicePixelRatio || 1;
  const w = canvas.parentElement.clientWidth;
  const h = 280;
  canvas.width = w * dpr;
  canvas.height = h * dpr;
  canvas.style.width = w + 'px';
  canvas.style.height = h + 'px';
  ctx.scale(dpr, dpr);
  ctx.clearRect(0, 0, w, h);

  const pad = { top: 24, right: 48, bottom: 32, left: 48 };
  const cw = w - pad.left - pad.right;
  const ch = h - pad.top - pad.bottom;

  if (trend.length === 0) {
    ctx.fillStyle = 'rgba(255,255,255,.2)';
    ctx.font = '14px Inter, system-ui';
    ctx.textAlign = 'center';
    ctx.fillText('暂无分析数据', w / 2, h / 2);
    return;
  }

  const maxReq = Math.max(1, ...trend.map(t => t.requests || 0));
  const maxBytes = Math.max(1, ...trend.map(t => t.bytes_out || 0)) / (1024 * 1024);
  const n = trend.length;

  // Grid + left axis (requests)
  ctx.strokeStyle = 'rgba(255,255,255,.04)';
  ctx.lineWidth = 1;
  ctx.font = '10px Inter, system-ui';
  ctx.textAlign = 'right';
  for (let i = 0; i <= 4; i++) {
    const yy = pad.top + (i / 4) * ch;
    ctx.beginPath(); ctx.moveTo(pad.left, yy); ctx.lineTo(w - pad.right, yy); ctx.stroke();
    ctx.fillStyle = 'rgba(255,255,255,.2)';
    ctx.fillText(Math.round((4 - i) / 4 * maxReq), pad.left - 8, yy + 4);
  }
  // Right axis (MB)
  ctx.textAlign = 'left';
  for (let i = 0; i <= 4; i++) {
    const yy = pad.top + (i / 4) * ch;
    ctx.fillStyle = 'rgba(100,210,255,.5)';
    ctx.fillText(((4 - i) / 4 * maxBytes).toFixed(0) + ' MB', w - pad.right + 8, yy + 4);
  }

  const slot = cw / n;
  const barW = Math.max(2, slot * 0.6);

  // Request bars
  trend.forEach((t, i) => {
    const bh = (t.requests || 0) / maxReq * ch;
    const x = pad.left + i * slot + (slot - barW) / 2;
    const y = pad.top + ch - bh;
    const grad = ctx.createLinearGradient(0, y, 0, pad.top + ch);
    grad.addColorStop(0, 'rgba(30,99,255,.55)');
    grad.addColorStop(1, 'rgba(30,99,255,.08)');
    ctx.fillStyle = grad;
    ctx.fillRect(x, y, barW, bh);
  });

  // Traffic line
  const lineX = i => pad.left + i * slot + slot / 2;
  const lineY = v => pad.top + ch - (v / (1024 * 1024)) / maxBytes * ch;
  ctx.beginPath();
  trend.forEach((t, i) => {
    const x = lineX(i), y = lineY(t.bytes_out || 0);
    if (i === 0) ctx.moveTo(x, y); else ctx.lineTo(x, y);
  });
  ctx.strokeStyle = 'rgb(100,210,255)';
  ctx.lineWidth = 2;
  ctx.stroke();

  // X labels (every ~10th)
  ctx.fillStyle = 'rgba(255,255,255,.25)';
  ctx.textAlign = 'center';
  const step = Math.ceil(n / 10);
  trend.forEach((t, i) => {
    if (i % step !== 0 && i !== n - 1) return;
    const d = new Date(t.bucket * 1000);
    const p = v => String(v).padStart(2, '0');
    const label = hours > 24 ? `${p(d.getMonth() + 1)}-${p(d.getDate())}` : `${p(d.getHours())}:${p(d.getMinutes())}`;
    ctx.fillText(label, lineX(i), h - pad.bottom + 16);
  });
}

function renderStatusBars(status, totalReq) {
  const el = document.getElementById('aan-status');
  if (!el) return;
  if (status.length === 0) {
    el.innerHTML = '<div class="relay-empty">暂无数据</div>';
    return;
  }
  const maxCount = Math.max(1, ...status.map(s => s.count));
  const statusName = code => code >= 500 ? '服务端错误' : code >= 400 ? '客户端错误' : code >= 300 ? '重定向' : code >= 200 ? '成功' : '其他';
  el.innerHTML = status.map(s => `
    <div class="aan-status-item">
      <div class="aan-status-head">
        <span class="aan-status-dot" style="background:${statusColor(s.status)}"></span>
        <span class="alog-status" style="color:${statusColor(s.status)}">${s.status}</span>
        <span class="aan-status-name">${statusName(s.status)}</span>
      </div>
      <div class="aan-status-nums">
        <span class="mono">${s.count}</span>
        <span class="aan-status-pct">${totalReq ? (s.count / totalReq * 100).toFixed(1) : 0}%</span>
      </div>
      <div class="aan-status-bar"><div class="aan-status-fill" style="width:${(s.count / maxCount * 100).toFixed(1)}%;background:${statusColor(s.status)}"></div></div>
    </div>
  `).join('');
}

function renderTopList(tbodyId, rows) {
  const tbody = document.getElementById(tbodyId);
  if (!tbody) return;
  if (rows.length === 0) {
    tbody.innerHTML = '<tr><td colspan="4" class="relay-empty">暂无数据</td></tr>';
    return;
  }
  const total = rows.reduce((s, r) => s + (r[1] || 0), 0);
  const cols = rows[0].length;
  tbody.innerHTML = rows.map(r => `
    <tr>
      <td style="max-width:320px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap" title="${r[0]}">${r[0]}</td>
      <td>${r[1]}</td>
      <td>${total ? ((r[1] / total) * 100).toFixed(1) : 0}%</td>
      <td>${formatBytes(r[2] || 0)}</td>
      ${r[3] !== undefined ? `<td>${r[3]}</td>` : ''}
    </tr>
  `).join('');
}

function renderTopIPs(ips) {
  const tbody = document.getElementById('aan-ips');
  if (!tbody) return;
  if (ips.length === 0) {
    tbody.innerHTML = '<tr><td colspan="6" class="relay-empty">暂无数据</td></tr>';
    return;
  }
  const total = ips.reduce((s, i) => s + (i.count || 0), 0);
  tbody.innerHTML = ips.map(i => `
    <tr>
      <td><span class="mono">${esc(i.ip)}</span></td>
      <td>${geoCell(i.geo)}</td>
      <td>${i.count}</td>
      <td>${total ? ((i.count / total) * 100).toFixed(1) : 0}%</td>
      <td>${formatBytes(i.bytes || 0)}</td>
      <td>${formatLatency(i.avg_latency_ms || 0)}</td>
    </tr>
  `).join('');
}

window.addEventListener('resize', () => {
  if (Router.current === 'access-analysis') loadAnalysis();
});
