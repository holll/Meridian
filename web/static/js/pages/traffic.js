// Traffic statistics page
function renderTraffic() {
  const page = document.getElementById('page-traffic');
  page.innerHTML = `
    <h1 class="section-title fade-up">流量统计</h1>
    <p class="section-sub fade-up stagger-1">查看各站点流量使用情况</p>
    <div class="controls-row fade-up stagger-1">
      <select class="form-select" id="traffic-site-select">
        <option value="">加载中...</option>
      </select>
      <select class="form-select" id="traffic-hours-select">
        <option value="24">最近 24 小时</option>
        <option value="168">最近 7 天</option>
        <option value="720">最近 30 天</option>
      </select>
    </div>
    <div class="chart-wrap fade-up stagger-2">
      <div class="chart-head">
        <h3>流量趋势</h3>
        <div class="chart-legend">
          <div class="legend-item"><div class="legend-dot in"></div>入站流量</div>
          <div class="legend-item"><div class="legend-dot out"></div>出站流量</div>
        </div>
      </div>
      <canvas id="trafficChart"></canvas>
    </div>
    <div class="traffic-totals" id="traffic-totals"></div>
    <div id="traffic-daily-table"></div>
  `;

  loadTrafficSites();
  document.getElementById('traffic-site-select').onchange = loadTrafficChart;
  document.getElementById('traffic-hours-select').onchange = loadTrafficChart;
}

async function loadTrafficSites() {
  try {
    const sites = await API.listSites();
    const sel = document.getElementById('traffic-site-select');
    if (!sites || sites.length === 0) {
      sel.innerHTML = '<option value="">暂无站点</option>';
      return;
    }
    sel.innerHTML = sites.map(s => `<option value="${s.id}">${esc(s.name)}</option>`).join('');
    loadTrafficChart();
  } catch (e) {
    Toast.error('加载站点失败');
  }
}

async function loadTrafficChart() {
  const siteId = document.getElementById('traffic-site-select').value;
  const hours = parseInt(document.getElementById('traffic-hours-select').value);
  if (!siteId) return;

  try {
    const isMultiDay = hours > 24;
    const days = Math.round(hours / 24);
    const [logs, dailyLogs, sites] = await Promise.all([
      isMultiDay ? Promise.resolve([]) : API.getTraffic(siteId, hours),
      isMultiDay ? API.getDailyTraffic(siteId, days) : Promise.resolve([]),
      API.listSites(),
    ]);
    const site = sites.find(s => s.id === parseInt(siteId));

    const data = isMultiDay ? dailyLogs : logs;
    const totalIn = data.reduce((a, l) => a + (l.bytes_in || 0), 0);
    const totalOut = data.reduce((a, l) => a + (l.bytes_out || 0), 0);

    document.getElementById('traffic-totals').innerHTML = `
      <div class="total-card fade-up stagger-3">
        <div class="total-label">入站流量</div>
        <div class="total-value">${formatBytes(totalIn)}</div>
      </div>
      <div class="total-card fade-up stagger-4">
        <div class="total-label">出站流量</div>
        <div class="total-value">${formatBytes(totalOut)}</div>
      </div>
      <div class="total-card fade-up stagger-5">
        <div class="total-label">累计使用</div>
        <div class="total-value">${formatBytes(site ? site.traffic_used : 0)}</div>
        ${site && site.traffic_quota > 0 ? `<div class="total-delta" style="color:var(--white-38)">额度 ${formatBytes(site.traffic_quota)}</div>` : ''}
      </div>
    `;

    if (isMultiDay) {
      drawDailyChart(dailyLogs);
      renderDailyTable(dailyLogs);
    } else {
      drawTrafficChart(logs, hours);
      document.getElementById('traffic-daily-table').innerHTML = '';
    }
  } catch (e) {
    console.error('Traffic load error:', e);
  }
}

function drawTrafficChart(logs, hours) {
  const numPoints = Math.min(hours, 24);
  const inbound = new Array(numPoints).fill(0);
  const outbound = new Array(numPoints).fill(0);

  if (logs.length > 0) {
    const now = Date.now();
    logs.forEach(l => {
      const t = new Date(l.recorded_at).getTime();
      const hoursAgo = (now - t) / 3600000;
      const idx = numPoints - 1 - Math.floor(hoursAgo * numPoints / hours);
      if (idx >= 0 && idx < numPoints) {
        inbound[idx] += l.bytes_in / (1024 * 1024);
        outbound[idx] += l.bytes_out / (1024 * 1024);
      }
    });
  }
  drawChartCanvas(inbound, outbound, null, logs.length === 0);
}

function drawDailyChart(dailyLogs) {
  const labels = dailyLogs.map(l => l.date.slice(5));
  const inbound = dailyLogs.map(l => l.bytes_in / (1024 * 1024));
  const outbound = dailyLogs.map(l => l.bytes_out / (1024 * 1024));
  drawChartCanvas(inbound, outbound, labels, dailyLogs.length === 0);
}

function drawChartCanvas(inbound, outbound, labels, isEmpty) {
  const canvas = document.getElementById('trafficChart');
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

  const pad = { top: 24, right: 24, bottom: labels ? 40 : 28, left: 54 };
  const cw = w - pad.left - pad.right;
  const ch = h - pad.top - pad.bottom;
  const numPoints = inbound.length || 1;
  const maxV = Math.max(1, ...inbound, ...outbound) * 1.2;
  const xPos = i => pad.left + (numPoints <= 1 ? cw / 2 : (i / (numPoints - 1)) * cw);
  const yPos = v => pad.top + (1 - v / maxV) * ch;

  ctx.clearRect(0, 0, w, h);

  // Grid lines + Y labels
  ctx.strokeStyle = 'rgba(255,255,255,.04)';
  ctx.lineWidth = 1;
  for (let i = 0; i <= 4; i++) {
    const yy = pad.top + (i / 4) * ch;
    ctx.beginPath(); ctx.moveTo(pad.left, yy); ctx.lineTo(w - pad.right, yy); ctx.stroke();
    ctx.fillStyle = 'rgba(255,255,255,.2)';
    ctx.font = '11px Inter, system-ui';
    ctx.textAlign = 'right';
    ctx.fillText(((4 - i) / 4 * maxV).toFixed(0) + ' MB', pad.left - 12, yy + 4);
  }

  // X-axis labels for daily chart
  if (labels && labels.length > 0) {
    ctx.fillStyle = 'rgba(255,255,255,.2)';
    ctx.font = '10px Inter, system-ui';
    ctx.textAlign = 'center';
    const step = Math.ceil(labels.length / 10);
    labels.forEach((lbl, i) => {
      if (i % step === 0 || i === labels.length - 1) {
        ctx.fillText(lbl, xPos(i), h - pad.bottom + 16);
      }
    });
  }

  if (isEmpty) {
    ctx.fillStyle = 'rgba(255,255,255,.2)';
    ctx.font = '14px Inter, system-ui';
    ctx.textAlign = 'center';
    ctx.fillText('暂无流量数据', w / 2, h / 2);
    return;
  }

  function smoothLine(data, color, glowColor) {
    if (data.length === 0) return;
    ctx.save();
    ctx.beginPath();
    ctx.moveTo(xPos(0), yPos(data[0]));
    for (let i = 1; i < data.length; i++) {
      const xc = (xPos(i - 1) + xPos(i)) / 2;
      const yc = (yPos(data[i - 1]) + yPos(data[i])) / 2;
      ctx.quadraticCurveTo(xPos(i - 1), yPos(data[i - 1]), xc, yc);
    }
    ctx.lineTo(xPos(data.length - 1), yPos(data[data.length - 1]));
    ctx.shadowColor = glowColor; ctx.shadowBlur = 16;
    ctx.strokeStyle = color; ctx.lineWidth = 2.5; ctx.stroke();
    ctx.shadowBlur = 0;
    ctx.lineTo(xPos(data.length - 1), pad.top + ch);
    ctx.lineTo(xPos(0), pad.top + ch);
    ctx.closePath();
    const grad = ctx.createLinearGradient(0, pad.top, 0, pad.top + ch);
    grad.addColorStop(0, color.replace(')', ',.12)').replace('rgb', 'rgba'));
    grad.addColorStop(1, 'rgba(0,0,0,0)');
    ctx.fillStyle = grad; ctx.fill(); ctx.restore();
  }

  smoothLine(outbound, 'rgb(100,210,255)', 'rgba(100,210,255,.4)');
  smoothLine(inbound, 'rgb(10,132,255)', 'rgba(10,132,255,.4)');
}

function renderDailyTable(dailyLogs) {
  const container = document.getElementById('traffic-daily-table');
  if (!dailyLogs || dailyLogs.length === 0) {
    container.innerHTML = '';
    return;
  }
  const rows = [...dailyLogs].reverse().map(l => `
    <tr>
      <td>${l.date}</td>
      <td>${formatBytes(l.bytes_in)}</td>
      <td>${formatBytes(l.bytes_out)}</td>
      <td>${formatBytes(l.bytes_in + l.bytes_out)}</td>
    </tr>
  `).join('');
  container.innerHTML = `
    <div class="daily-table-wrap fade-up">
      <h3 style="margin:24px 0 12px;font-size:15px;font-weight:600;color:var(--white-87)">每日明细</h3>
      <table class="daily-table">
        <thead><tr><th>日期</th><th>入站</th><th>出站</th><th>合计</th></tr></thead>
        <tbody>${rows}</tbody>
      </table>
    </div>
  `;
}

window.addEventListener('resize', () => {
  if (Router.current === 'traffic') {
    const canvas = document.getElementById('trafficChart');
    if (canvas) loadTrafficChart();
  }
});