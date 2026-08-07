(function() {
  'use strict';

  const loginEl = document.getElementById('page-login');
  const shellEl = document.getElementById('app-shell');
  const loginFooterEl = document.getElementById('login-footer');
  const loginButtonEl = document.getElementById('btn-login');
  const setupTokenGroupEl = document.getElementById('setup-token-group');
  const setupTokenInputEl = document.getElementById('inp-setup-token');
  let dashboardRefreshTimer = null;
  let appBootstrapped = false;
  let modalBackdropClosable = false;
  let modalPreviousFocus = null;
  let authStatus = {
    needs_setup: false,
    mode: 'single_admin',
    jwt_secret_ephemeral: false,
    setup_token_required: false,
  };

  window.openModal = function(options) {
    modalBackdropClosable = !!(options && options.closeOnBackdrop);
    modalPreviousFocus = document.activeElement;
    const overlay = document.getElementById('modal-overlay');
    document.getElementById('modal-body').scrollTop = 0;
    overlay.classList.add('active');
    overlay.setAttribute('aria-hidden', 'false');
    document.body.classList.add('modal-open');
  };

  window.closeModal = function() {
    modalBackdropClosable = false;
    const overlay = document.getElementById('modal-overlay');
    overlay.classList.remove('active');
    overlay.setAttribute('aria-hidden', 'true');
    document.body.classList.remove('modal-open');
    if (modalPreviousFocus && modalPreviousFocus.isConnected) modalPreviousFocus.focus();
    modalPreviousFocus = null;
  };

  document.getElementById('modal-overlay').addEventListener('click', function(e) {
    if (e.target === this && modalBackdropClosable) closeModal();
  });

  document.getElementById('modal-close').addEventListener('click', closeModal);

  document.addEventListener('keydown', function(e) {
    if (e.key === 'Escape' && document.getElementById('modal-overlay').classList.contains('active')) closeModal();
  });

  async function checkAuth() {
    try {
      const res = await API.checkSetup();
      authStatus = Object.assign({}, authStatus, res || {});
      window.ROUTE_PREFIX = res.route_prefix || '';
      if (API.token) {
        enterApp();
        return;
      }
      if (res.needs_setup) {
        showSetupMode();
        return;
      }
    } catch (e) {
      if (API.token) {
        enterApp();
        return;
      }
    }

    showLoginMode();
  }

  function renderLoginFooter(isSetup) {
    const lines = [];
    if (authStatus.mode === 'single_admin') {
      lines.push(isSetup
        ? '当前为单管理员模式，请创建唯一的管理员账号。'
        : '当前为单管理员模式。首次使用？<a href="#" id="link-register">创建管理员账号</a>');
    } else {
      lines.push(isSetup
        ? '首次使用，请创建管理员账号。'
        : '首次使用？<a href="#" id="link-register">创建管理员账号</a>');
    }

    if (authStatus.jwt_secret_ephemeral) {
      lines.push('<span class="login-note warn">当前未固定 JWT_SECRET，服务重启后需要重新登录。</span>');
    }

    return lines.join('');
  }

  function showSetupMode() {
    loginButtonEl.textContent = '注册';
    loginButtonEl.disabled = false;
    loginFooterEl.innerHTML = renderLoginFooter(true);
    loginEl._isSetup = true;
    setupTokenGroupEl.hidden = !authStatus.setup_token_required;
    setupTokenInputEl.required = !!authStatus.setup_token_required;
  }

  function showLoginMode() {
    loginButtonEl.textContent = '登录';
    loginButtonEl.disabled = false;
    loginFooterEl.innerHTML = renderLoginFooter(false);
    loginEl._isSetup = false;
    setupTokenGroupEl.hidden = true;
    setupTokenInputEl.required = false;
  }

  function startDashboardRefresh() {
    if (dashboardRefreshTimer) clearInterval(dashboardRefreshTimer);
    dashboardRefreshTimer = setInterval(() => {
      if (Router.current === 'dashboard') loadDashboardData();
    }, 15000);
  }

  function stopDashboardRefresh() {
    if (!dashboardRefreshTimer) return;
    clearInterval(dashboardRefreshTimer);
    dashboardRefreshTimer = null;
  }

  function teardownAppRuntime() {
    stopDashboardRefresh();
    if (typeof stopDashSSE === 'function') stopDashSSE();
    if (typeof stopRelayRefresh === 'function') stopRelayRefresh();
    if (typeof stopAccessLogRefresh === 'function') stopAccessLogRefresh();
  }

  document.getElementById('loginForm').addEventListener('submit', async function(e) {
    e.preventDefault();
    const username = document.getElementById('inp-username').value.trim();
    const password = document.getElementById('inp-password').value;
    const setupToken = setupTokenInputEl.value.trim();

    if (!username || !password) {
      Toast.error('请填写用户名和密码');
      return;
    }

    if (loginEl._isSetup && password.length < 8) {
      Toast.error('管理员密码至少需要 8 位');
      return;
    }

    if (loginEl._isSetup && authStatus.setup_token_required && !setupToken) {
      Toast.error('请填写启动日志中的初始化令牌');
      return;
    }

    loginButtonEl.disabled = true;
    loginButtonEl.textContent = '处理中...';

    try {
      let res;
      if (loginEl._isSetup) {
        res = await API.setup(username, password, setupToken);
        Toast.success('管理员创建成功');
      } else {
        res = await API.login(username, password);
        Toast.success('欢迎回来, ' + res.username + '!');
      }
      API.token = res.token;
      API.username = res.username;
      enterApp();
    } catch (err) {
      Toast.error(err.message);
      loginButtonEl.disabled = false;
      loginButtonEl.textContent = loginEl._isSetup ? '注册' : '登录';
    }
  });

  loginFooterEl.addEventListener('click', function(e) {
    const registerLink = e.target.closest('#link-register');
    if (!registerLink) return;
    e.preventDefault();
    showSetupMode();
  });

  function enterApp() {
    loginEl.classList.add('hidden');
    shellEl.classList.add('active');

    const avatar = document.getElementById('avatar-btn');
    avatar.textContent = (API.username || 'A')[0].toUpperCase();

    if (!appBootstrapped) {
      Router.register('dashboard', renderDashboard);
      Router.register('sites', renderSites);
      Router.register('access-logs', renderAccessLogs);
      Router.register('access-analysis', renderAccessAnalysis);
      Router.register('relay', renderRelay);
      if (typeof renderDiag === 'function') {
        Router.register('diagnostics', renderDiag);
      } else {
        console.error('renderDiag is not defined; diagnostics page script failed to load');
        Router.register('diagnostics', function() {
          var page = document.getElementById('page-diagnostics');
          if (page) {
            page.innerHTML = '<div class="diag-card diag-card-wide"><div class="diag-empty">诊断页面脚本加载失败，请强制刷新浏览器缓存后重试。</div></div>';
          }
        });
      }
      Router.init();
      appBootstrapped = true;
    }

    Router.resolve();
    startDashboardRefresh();
  }

  // Mobile sidebar (drawer) — opened from the "more" tab in the bottom bar
  const sidebarEl = document.getElementById('sidebar');
  const sidebarOverlayEl = document.getElementById('sidebar-overlay');

  function openSidebar() {
    sidebarEl.classList.add('open');
    sidebarOverlayEl.classList.add('active');
    sidebarOverlayEl.setAttribute('aria-hidden', 'false');
  }

  function closeSidebar() {
    sidebarEl.classList.remove('open');
    sidebarOverlayEl.classList.remove('active');
    sidebarOverlayEl.setAttribute('aria-hidden', 'true');
  }

  document.getElementById('btn-sidebar-open').addEventListener('click', openSidebar);
  document.getElementById('btn-sidebar-close').addEventListener('click', closeSidebar);
  sidebarOverlayEl.addEventListener('click', closeSidebar);
  document.querySelectorAll('.sidebar-link[data-page]').forEach(link => {
    link.addEventListener('click', closeSidebar);
  });

  function logout() {
    if (!confirm('确认退出登录？')) return;
    teardownAppRuntime();
    closeSidebar();
    API.logout();
    loginEl.classList.remove('hidden');
    shellEl.classList.remove('active');
    showLoginMode();
    document.getElementById('inp-password').value = '';
    Toast.info('已退出登录');
  }

  // Avatar dropdown menu (settings / about / logout)
  const avatarBtn = document.getElementById('avatar-btn');
  const avatarMenu = document.getElementById('avatar-menu');
  function toggleAvatarMenu(open) {
    avatarMenu.classList.toggle('open', open);
    avatarMenu.setAttribute('aria-hidden', open ? 'false' : 'true');
  }
  avatarBtn.addEventListener('click', function(e) {
    e.stopPropagation();
    toggleAvatarMenu(!avatarMenu.classList.contains('open'));
  });
  document.addEventListener('click', function() { toggleAvatarMenu(false); });
  avatarMenu.addEventListener('click', function(e) { e.stopPropagation(); });
  document.getElementById('menu-settings').addEventListener('click', function() {
    toggleAvatarMenu(false);
    showSettingsModal();
  });
  document.getElementById('menu-about').addEventListener('click', function() {
    toggleAvatarMenu(false);
    showAboutModal();
  });
  document.getElementById('menu-logout').addEventListener('click', function() {
    toggleAvatarMenu(false);
    logout();
  });
  document.getElementById('btn-sidebar-logout').addEventListener('click', logout);

  checkAuth();
})();

// showSettingsModal displays panel information and a password change form.
async function showSettingsModal() {
  document.getElementById('modal-title').textContent = '设置';
  document.getElementById('modal-body').innerHTML = '<div class="relay-loading">加载中...</div>';
  openModal({ closeOnBackdrop: true });

  let info;
  try {
    info = await API.getAdminSettings();
  } catch (e) {
    document.getElementById('modal-body').innerHTML = '<p style="color:var(--red)">加载设置失败：' + esc(e.message) + '</p>';
    return;
  }

  document.getElementById('modal-body').innerHTML = `
    <div style="font-size:.85rem;margin-bottom:18px">
      <div class="settings-row"><span>面板版本</span><b>${esc(info.version || '—')}</b></div>
      <div class="settings-row"><span>面板地址</span><span class="mono">${esc(info.panel_url || '—')}</span></div>
      <div class="settings-row"><span>路由前缀</span><span class="mono">${esc(info.route_prefix || '—')}</span></div>
      <div class="settings-row"><span>Relay API</span><span>${info.relay_api_enabled ? '已启用' : '未启用'}</span></div>
      <div class="settings-row"><span>GeoLite</span><span>${info.geolite_enabled ? '已启用' : '未启用'}</span></div>
    </div>
    <div class="avatar-menu-sep" style="margin:10px 0"></div>
    <div class="form-group">
      <label>当前密码</label>
      <input type="password" class="form-input" id="pw-old" autocomplete="current-password">
    </div>
    <div class="form-group">
      <label>新密码（8–72 位）</label>
      <input type="password" class="form-input" id="pw-new" autocomplete="new-password">
    </div>
    <div class="form-group">
      <label>确认新密码</label>
      <input type="password" class="form-input" id="pw-confirm" autocomplete="new-password">
    </div>`;
  document.getElementById('modal-footer').innerHTML = `
    <button class="btn-modal secondary" id="pw-cancel">取消</button>
    <button class="btn-modal primary" id="pw-submit">修改密码</button>`;
  document.getElementById('pw-cancel').addEventListener('click', closeModal);
  document.getElementById('pw-submit').addEventListener('click', async () => {
    const oldPw = document.getElementById('pw-old').value;
    const newPw = document.getElementById('pw-new').value;
    const confirmPw = document.getElementById('pw-confirm').value;
    if (!oldPw || !newPw) { Toast.error('请填写完整密码'); return; }
    if (newPw !== confirmPw) { Toast.error('两次输入的新密码不一致'); return; }
    try {
      await API.changePassword(oldPw, newPw);
      closeModal();
      Toast.success('密码已修改，请重新登录');
      API.logout();
      window.location.reload();
    } catch (e) {
      Toast.error(e.message);
    }
  });
}

// showAboutModal displays version info and GitHub repository status.
async function showAboutModal() {
  document.getElementById('modal-title').textContent = '关于';
  document.getElementById('modal-body').innerHTML = '<div class="relay-loading">加载中...</div>';
  openModal({ closeOnBackdrop: true });

  let current = '—';
  let latest = '';
  try {
    const res = await API.updateCheck();
    current = res.current || current;
    latest = res.latest || '';
  } catch (e) { /* keep placeholders */ }

  let repo = null;
  try {
    const info = await API.getRepoInfo();
    if (info && !info.error) repo = info;
  } catch (e) { /* degrade to version-only display */ }

  const stats = repo
    ? `<div class="about-stats">
         <div class="about-stat"><b>${repo.stars || 0}</b><span>Star</span></div>
         <div class="about-stat"><b>${repo.forks || 0}</b><span>Fork</span></div>
         <div class="about-stat"><b>${repo.open_issues || 0}</b><span>Issues</span></div>
         <div class="about-stat"><b>${esc(repo.license || '—')}</b><span>License</span></div>
       </div>
       ${repo.description ? `<div style="color:var(--white-60);font-size:.82rem;margin-top:12px;text-align:center">${esc(repo.description)}</div>` : ''}
       <a href="${esc(repo.html_url)}" target="_blank" rel="noopener" class="about-link">${esc(repo.html_url)}</a>`
    : '<div style="color:var(--white-38);font-size:.82rem;margin-top:12px">无法获取仓库信息</div>';

  document.getElementById('modal-body').innerHTML = `
    <div style="text-align:center;padding:8px 0 16px">
      <div style="font-size:1.05rem;font-weight:600">Meridian</div>
      <div style="color:var(--white-38);font-size:.85rem;margin-top:4px">版本 ${esc(current)}</div>
      <div style="color:var(--white-38);font-size:.85rem;margin-top:2px">${latest ? (latest !== current ? `最新版本 <b style="color:var(--green)">${esc(latest)}</b>` : '已是最新版本') : ''}</div>
      ${stats}
    </div>`;
  document.getElementById('modal-footer').innerHTML = `
    <button class="btn-modal secondary" id="about-close">关闭</button>`;
  document.getElementById('about-close').addEventListener('click', closeModal);
}
