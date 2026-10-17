// SBX Pro 控制台前端逻辑。
//
// 视觉与交互骨架与 sbx 单机版完全一致：wrap / view / hero / stats /
// node-card / dock 底部导航 / drawer 抽屉 / toast。pro 只是把「单机」换成了
// 「多机」——卡片上多了机器归属，操作里多了接入机器。
//
// 原则：不使用 inline onclick 与内联样式（CSP 友好）；所有动态内容经 esc()；
// fetch 严格检查 response.ok。
(function () {
  'use strict';

  var state = {
    machines: [],
    nodes: [],
    traffic: { rx_total: 0, tx_total: 0, totals: [] },
    caps: [],
    lastSync: 0
  };
  var activeView = 'home';
  var currentMachine = null;
  var currentNode = null;
  var confirmAction = null;

  function $(id) { return document.getElementById(id); }

  function esc(s) {
    return String(s == null ? '' : s).replace(/[&<>"']/g, function (c) {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c];
    });
  }

  function fmtBytes(n) {
    if (n == null || isNaN(n)) return '0 B';
    n = Number(n);
    if (n === 0) return '0 B';
    var u = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB'];
    var i = 0, v = n;
    while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
    var s = (i === 0) ? String(v) : (v >= 100 ? v.toFixed(0) : v >= 10 ? v.toFixed(1) : v.toFixed(2));
    return s + ' ' + u[i];
  }

  function fmtAgo(ts) {
    if (!ts) return '—';
    var d = Math.floor(Date.now() / 1000 - ts);
    if (d < 0) return '刚刚';
    if (d < 60) return d + ' 秒前';
    if (d < 3600) return Math.floor(d / 60) + ' 分钟前';
    if (d < 86400) return Math.floor(d / 3600) + ' 小时前';
    return Math.floor(d / 86400) + ' 天前';
  }

  // ===== API（严格错误处理，8 秒超时）=====
  function api(path, opts) {
    opts = opts || {};
    var method = opts.method || 'GET';
    var headers = {};
    if (opts.body !== undefined && opts.body !== null) headers['Content-Type'] = 'application/json';
    var ctrl = new AbortController();
    var timer = setTimeout(function () { ctrl.abort(); }, 8000);
    return fetch(path, {
      method: method, headers: headers, body: opts.body,
      credentials: 'same-origin', cache: 'no-store', signal: ctrl.signal
    }).then(function (r) {
      clearTimeout(timer);
      if (r.status === 401) { location.replace('/login'); throw new Error('未登录'); }
      return r.text().then(function (txt) {
        var data = {};
        if (txt) { try { data = JSON.parse(txt); } catch (e) { data = {}; } }
        if (!r.ok) {
          var msg = data.error || ('请求失败 (' + r.status + ')');
          var err = new Error(msg);
          err.status = r.status;
          throw err;
        }
        return data;
      });
    }).catch(function (e) {
      clearTimeout(timer);
      if (e.name === 'AbortError') throw new Error('请求超时');
      if (e instanceof TypeError) throw new Error('无法连接管理面板');
      throw e;
    });
  }

  // ===== Toast =====
  function toast(msg) {
    var el = $('toast');
    el.textContent = msg;
    el.classList.add('show');
    clearTimeout(toast._t);
    toast._t = setTimeout(function () { el.classList.remove('show'); }, 3600);
  }

  // ===== 抽屉 =====
  function openDrawer(id) {
    $('drawer-mask').classList.add('on');
    $(id).classList.add('on');
  }
  function closeDrawer(id) {
    $(id).classList.remove('on');
    if (!document.querySelector('.drawer.on')) $('drawer-mask').classList.remove('on');
  }
  function closeAllDrawers() {
    document.querySelectorAll('.drawer.on').forEach(function (d) { d.classList.remove('on'); });
    $('drawer-mask').classList.remove('on');
  }

  // ===== 视图切换（hash 路由，刷新后停留在原页）=====
  function show(name, push) {
    if (!document.getElementById('view-' + name)) name = 'home';
    activeView = name;
    document.querySelectorAll('.view').forEach(function (v) { v.classList.remove('on'); });
    $('view-' + name).classList.add('on');
    document.querySelectorAll('.tab[data-view]').forEach(function (b) {
      b.classList.toggle('on', b.dataset.view === name);
    });
    if (push && location.hash !== '#' + name) history.pushState(null, '', '#' + name);
    window.scrollTo(0, 0);
  }

  // ===== 数据视图模型 =====
  function machineName(id) {
    for (var i = 0; i < state.machines.length; i++) {
      var m = state.machines[i];
      if (m.machine_id === id) return m.display_name || m.hostname || m.machine_id;
    }
    return id || '未知机器';
  }

  // 流量行按节点汇总，按机器求和得到机器维度的累计。
  function trafficByMachine(id) {
    var sum = 0;
    (state.traffic.totals || []).forEach(function (t) {
      if (t.machine_id === id) sum += (t.rx || 0) + (t.tx || 0);
    });
    return sum;
  }

  function trafficByNode(uuid) {
    var rows = state.traffic.totals || [];
    for (var i = 0; i < rows.length; i++) {
      if (rows[i].node_uuid === uuid) return (rows[i].rx || 0) + (rows[i].tx || 0);
    }
    return 0;
  }

  function nodesOfMachine(id) {
    return state.nodes.filter(function (n) { return n.machine_id === id; }).length;
  }

  function isBadStatus(s) {
    return s === 'create_failed' || s === 'config_error' || s === 'provisioning' ||
      s === 'update_pending' || s === 'sync_pending' || s === 'delete_pending';
  }

  function protoLabel(t) {
    var m = {
      vless: 'VLESS Reality', shadowsocks: 'Shadowsocks 2022',
      trojan: 'Trojan', anytls: 'AnyTLS', snell: 'Snell'
    };
    return m[t] || t || '—';
  }

  // 状态文案与颜色：与 sbx 的 status-pill 语义一致（ok / warn / danger / paused）。
  function statusOf(s) {
    var m = {
      active: { text: '正常', pill: 'ok' },
      provisioning: { text: '下发中', pill: 'warn' },
      update_pending: { text: '待同步', pill: 'warn' },
      sync_pending: { text: '待同步', pill: 'warn' },
      delete_pending: { text: '删除中', pill: 'warn' },
      disabled: { text: '已停用', pill: 'paused' },
      create_failed: { text: '创建失败', pill: 'danger' },
      config_error: { text: '配置异常', pill: 'danger' }
    };
    return m[s] || { text: s || '未知', pill: '' };
  }

  // ===== 卡片 =====
  function chip(text) { return '<span class="chip">' + esc(text) + '</span>'; }

  function machineCard(m) {
    var on = m.status === 'online';
    var name = m.display_name || m.hostname || m.machine_id;
    var rev = (m.applied_revision || 0) + '/' + (m.config_revision || 0);
    var synced = (m.applied_revision || 0) === (m.config_revision || 0);
    return '<article class="node-card">' +
      '<div class="node-top">' +
        '<div class="node-title">' +
          '<div class="node-name">' + esc(name) + '</div>' +
          '<div class="node-meta-line">' +
            chip(m.os || 'linux') +
            '<span class="port">' + esc(m.ipv4 || '地址未知') + '</span>' +
            '<span class="chip">rev ' + esc(rev) + '</span>' +
          '</div>' +
        '</div>' +
        '<div class="node-rate">' +
          '<b class="' + (on ? 'up' : 'down') + '">' + (on ? '在线' : '离线') + '</b>' +
          '<span class="port">' + esc(m.agent_version || '—') + '</span>' +
        '</div>' +
      '</div>' +
      '<div class="node-stats">' +
        '<div class="node-stat"><span>最近心跳</span><b>' + esc(fmtAgo(m.last_seen)) + '</b></div>' +
        '<div class="node-stat"><span>累计流量</span><b>' + fmtBytes(trafficByMachine(m.machine_id)) + '</b></div>' +
        '<div class="node-stat"><span>节点</span><b>' + nodesOfMachine(m.machine_id) + ' 个</b></div>' +
      '</div>' +
      '<div class="node-foot">' +
        '<span class="status-pill ' + (synced ? 'ok' : 'warn') + '">' + (synced ? '配置已同步' : '配置待同步') + '</span>' +
        '<div class="node-actions">' +
          '<button class="mini-btn" type="button" data-machine-open="' + esc(m.machine_id) + '">详情</button>' +
        '</div>' +
      '</div>' +
    '</article>';
  }

  function nodeCard(n) {
    var st = statusOf(n.status);
    var quota = n.quota_limit ? fmtBytes(n.quota_limit) : '无限制';
    var ipLimit = n.ip_limit ? (n.ip_limit + ' 个') : '无限制';
    return '<article class="node-card">' +
      '<div class="node-top">' +
        '<div class="node-title">' +
          '<div class="node-name">' + esc(n.name || n.node_uuid) + '</div>' +
          '<div class="node-meta-line">' +
            chip(protoLabel(n.protocol)) +
            '<span class="port">端口 ' + esc(n.port) + '</span>' +
            '<span class="port">' + esc(machineName(n.machine_id)) + '</span>' +
          '</div>' +
        '</div>' +
        '<div class="node-rate"><span class="port">已用流量</span><b>' + fmtBytes(trafficByNode(n.node_uuid)) + '</b></div>' +
      '</div>' +
      '<div class="node-stats">' +
        '<div class="node-stat"><span>流量额度</span><b class="' + (n.quota_limit ? '' : 'muted') + '">' + esc(quota) + '</b></div>' +
        '<div class="node-stat"><span>在线 IP 限制</span><b class="' + (n.ip_limit ? '' : 'muted') + '">' + esc(ipLimit) + '</b></div>' +
        '<div class="node-stat"><span>所属机器</span><b class="muted">' + esc(machineName(n.machine_id)) + '</b></div>' +
      '</div>' +
      '<div class="node-foot">' +
        '<span class="status-pill ' + st.pill + '">' + esc(st.text) + '</span>' +
        '<div class="node-actions">' +
          '<button class="mini-btn" type="button" data-node-detail="' + esc(n.node_uuid) + '">详情</button>' +
          '<button class="mini-btn" type="button" data-node-share="' + esc(n.node_uuid) + '">分享</button>' +
        '</div>' +
      '</div>' +
    '</article>';
  }

  function badNodeCard(n) {
    var st = statusOf(n.status);
    return '<article class="node-card">' +
      '<div class="node-top">' +
        '<div class="node-title">' +
          '<div class="node-name">' + esc(n.name || n.node_uuid) + '</div>' +
          '<div class="node-meta-line">' + chip(protoLabel(n.protocol)) +
            '<span class="port">' + esc(machineName(n.machine_id)) + '</span></div>' +
        '</div>' +
        '<div class="node-rate"><b class="down">' + esc(st.text) + '</b></div>' +
      '</div>' +
      '<div class="node-foot">' +
        '<span class="status-pill ' + st.pill + '">' + esc(st.text) + '</span>' +
        '<div class="node-actions">' +
          '<button class="mini-btn" type="button" data-node-detail="' + esc(n.node_uuid) + '">详情</button>' +
        '</div>' +
      '</div>' +
    '</article>';
  }

  function emptyBox(text) { return '<div class="empty">' + esc(text) + '</div>'; }

  // ===== 渲染 =====
  function renderMachines() {
    var html = state.machines.length
      ? state.machines.map(machineCard).join('')
      : emptyBox('暂无机器，请到「接入」生成安装命令');
    $('machine-list').innerHTML = html;
    $('home-machine-cards').innerHTML = html;
  }

  function renderNodes() {
    $('node-cards').innerHTML = state.nodes.length
      ? state.nodes.map(nodeCard).join('')
      : emptyBox('暂无节点，点右上角「添加节点」创建');

    var bad = state.nodes.filter(function (n) { return isBadStatus(n.status); });
    $('home-bad-nodes').innerHTML = bad.length
      ? bad.map(badNodeCard).join('')
      : emptyBox('没有异常节点');
    $('kpi-bad').textContent = bad.length;
  }

  function renderTraffic() {
    var rx = state.traffic.rx_total || 0;
    var tx = state.traffic.tx_total || 0;
    $('hero-total').textContent = fmtBytes(rx + tx);
    $('hero-rx').textContent = fmtBytes(rx);
    $('hero-tx').textContent = fmtBytes(tx);
  }

  function renderKPI() {
    var online = state.machines.filter(function (m) { return m.status === 'online'; }).length;
    $('kpi-machines').textContent = state.machines.length;
    $('kpi-online').textContent = online;
    $('kpi-nodes').textContent = state.nodes.length;
    $('kpi-active').textContent = state.nodes.filter(function (n) { return n.status === 'active'; }).length;
  }

  function renderSync() {
    var el = $('sync-label');
    if (el) el.textContent = state.lastSync ? '更新于 ' + fmtAgo(Math.floor(state.lastSync / 1000)) : '';
  }

  // ===== 加载 =====
  function loadMachines() {
    return api('/api/machines').then(function (d) {
      state.machines = d.machines || [];
      renderMachines();
      renderKPI();
    });
  }

  function loadNodes() {
    return api('/api/nodes').then(function (d) {
      state.nodes = d.nodes || [];
      renderNodes();
      renderKPI();
      // 详情抽屉开着时同步刷新其内容（额度/IP 限制可能刚从别处改过）。
      if (currentNode) {
        var fresh = state.nodes.filter(function (n) { return n.node_uuid === currentNode.node_uuid; })[0];
        if (fresh) { currentNode = fresh; renderNodeDetail(fresh); }
      }
    });
  }

  function loadTraffic() {
    return api('/api/traffic').then(function (d) {
      state.traffic = d || state.traffic;
      renderTraffic();
      // 卡片上的流量来自 traffic，重渲染保持数字新鲜。
      renderMachines();
      renderNodes();
    });
  }

  function refresh() {
    if (document.hidden) return Promise.resolve();
    var failures = 0;
    var fail = function () { failures++; };
    return Promise.all([
      loadMachines().catch(fail),
      loadNodes().catch(fail),
      loadTraffic().catch(fail)
    ]).then(function () {
      if (failures < 3) state.lastSync = Date.now();
      renderSync();
    });
  }

  // ===== 机器详情抽屉 =====
  function openMachine(machineId) {
    var m = state.machines.filter(function (x) { return x.machine_id === machineId; })[0];
    if (!m) return;
    currentMachine = m;
    $('machine-drawer-name').textContent = m.display_name || m.hostname || m.machine_id;
    var on = m.status === 'online';
    var rev = (m.applied_revision || 0) + '/' + (m.config_revision || 0);
    $('machine-drawer-body').innerHTML =
      '<section class="sect">' +
        '<div class="info-grid">' +
          '<div class="info-cell"><span>状态</span><b>' + (on ? '在线' : '离线') + '</b></div>' +
          '<div class="info-cell"><span>最近心跳</span><b>' + esc(fmtAgo(m.last_seen)) + '</b></div>' +
          '<div class="info-cell"><span>系统</span><b>' + esc((m.os || '—') + ' / ' + (m.arch || '—')) + '</b></div>' +
          '<div class="info-cell"><span>Agent 版本</span><b>' + esc(m.agent_version || '—') + '</b></div>' +
          '<div class="info-cell"><span>IPv4</span><b>' + esc(m.ipv4 || '—') + '</b></div>' +
          '<div class="info-cell"><span>配置版本</span><b>rev ' + esc(rev) + '</b></div>' +
          '<div class="info-cell"><span>节点数</span><b>' + nodesOfMachine(m.machine_id) + ' 个</b></div>' +
          '<div class="info-cell"><span>累计流量</span><b>' + fmtBytes(trafficByMachine(m.machine_id)) + '</b></div>' +
          '<div class="info-cell wide"><span>机器 ID</span><b>' + esc(m.machine_id) + '</b></div>' +
        '</div>' +
      '</section>' +
      '<p class="hint-text">删除管理关系只影响控制台侧；远端 Agent 与其上的节点不会被自动卸载。</p>';
    openDrawer('machine-drawer');
  }

  // ===== 节点详情抽屉 =====
  function openNode(uuid) {
    var n = state.nodes.filter(function (x) { return x.node_uuid === uuid; })[0];
    if (!n) return;
    currentNode = n;
    renderNodeDetail(n);
    openDrawer('node-detail-drawer');
    api('/api/nodes/' + encodeURIComponent(uuid)).then(function (fresh) {
      if (!currentNode || currentNode.node_uuid !== uuid) return;
      currentNode = fresh;
      renderNodeDetail(fresh);
    }).catch(function (e) { if (e.message !== '未登录') toast(e.message); });
  }

  function renderNodeDetail(n) {
    var st = statusOf(n.status);
    var used = trafficByNode(n.node_uuid);
    var quotaText = n.quota_limit ? fmtBytes(n.quota_limit) : '无限制';
    var ipText = n.ip_limit ? (n.ip_limit + ' 个') : '无限制';
    $('node-detail-name').textContent = n.name || n.node_uuid;

    $('node-detail-body').innerHTML =
      '<section class="sect">' +
        '<div class="info-grid">' +
          '<div class="info-cell"><span>状态</span><b>' + esc(st.text) + '</b></div>' +
          '<div class="info-cell"><span>协议</span><b>' + esc(protoLabel(n.protocol)) + '</b></div>' +
          '<div class="info-cell"><span>端口</span><b>' + esc(n.port) + '</b></div>' +
          '<div class="info-cell"><span>所属机器</span><b>' + esc(machineName(n.machine_id)) + '</b></div>' +
          '<div class="info-cell"><span>已用流量</span><b>' + fmtBytes(used) + '</b></div>' +
          '<div class="info-cell"><span>流量额度</span><b class="' + (n.quota_limit ? '' : 'muted') + '">' + esc(quotaText) + '</b></div>' +
          '<div class="info-cell"><span>在线 IP 限制</span><b class="' + (n.ip_limit ? '' : 'muted') + '">' + esc(ipText) + '</b></div>' +
        '</div>' +
      '</section>' +

      '<section class="sect">' +
        '<div class="field"><small>设置流量额度（GiB，0 = 无限制）</small>' +
          '<div class="setting-row">' +
            '<input type="number" id="nd-quota" min="0" step="0.1" inputmode="decimal" placeholder="例如 100">' +
            '<button class="btn" type="button" id="nd-quota-save">保存</button>' +
          '</div>' +
        '</div>' +
        '<div class="field"><small>设置同时在线 IP 数（0 = 无限制）</small>' +
          '<div class="setting-row">' +
            '<input type="number" id="nd-iplimit" min="0" step="1" inputmode="numeric" placeholder="例如 2">' +
            '<button class="btn" type="button" id="nd-iplimit-save">保存</button>' +
          '</div>' +
        '</div>' +
      '</section>' +

      '<section class="sect">' +
        '<div class="switch-row"><div><b>在线 IP</b><small>当前活跃连接来源，按 IP 去重</small></div>' +
          '<button class="btn" type="button" id="nd-ips-load">查看</button></div>' +
        '<div class="ip-box" id="nd-ips"></div>' +
      '</section>' +

      '<section class="sect">' +
        '<p class="hint-text">删除后该节点会从机器上移除，凭据作废且无法恢复；历史流量记录保留。</p>' +
        '<button class="btn danger" type="button" id="nd-delete">删除节点</button>' +
      '</section>';

    var toggle = $('node-act-toggle');
    toggle.textContent = n.status === 'disabled' ? '启用' : '停用';
    toggle.dataset.action = n.status === 'disabled' ? 'enable' : 'disable';

    bindNodeDetailActions(n);
  }

  function bindNodeDetailActions(n) {
    var quotaInput = $('nd-quota');
    if (quotaInput && n.quota_limit) quotaInput.placeholder = '当前 ' + (n.quota_limit / 1073741824).toFixed(2) + ' GiB';
    var ipInput = $('nd-iplimit');
    if (ipInput && n.ip_limit) ipInput.placeholder = '当前 ' + n.ip_limit + ' 个';

    $('nd-quota-save').addEventListener('click', function () {
      var gb = parseFloat($('nd-quota').value || '0');
      if (isNaN(gb) || gb < 0) { toast('请输入合法额度（GiB，≥ 0）'); return; }
      var bytes = Math.round(gb * 1073741824);
      var used = trafficByNode(n.node_uuid);
      var doSave = function () {
        api('/api/nodes/' + encodeURIComponent(n.node_uuid) + '/quota', {
          method: 'POST', body: JSON.stringify({ limit_bytes: bytes })
        }).then(function () { toast('流量额度已下发'); closeDrawer('node-detail-drawer'); refresh(); })
          .catch(function (e) { if (e.message !== '未登录') toast(e.message); });
      };
      if (bytes > 0 && bytes < used) {
        askConfirm('确认设置额度',
          '额度（' + fmtBytes(bytes) + '）小于已用流量（' + fmtBytes(used) + '），保存后该节点会立即被阻断。继续？',
          doSave);
      } else {
        doSave();
      }
    });

    $('nd-iplimit-save').addEventListener('click', function () {
      var v = parseInt($('nd-iplimit').value || '0', 10);
      if (isNaN(v) || v < 0) { toast('请输入合法数字（≥ 0）'); return; }
      api('/api/nodes/' + encodeURIComponent(n.node_uuid) + '/ip-limit', {
        method: 'POST', body: JSON.stringify({ ip_limit: v })
      }).then(function () { toast('IP 限制已下发'); refresh(); })
        .catch(function (e) { if (e.message !== '未登录') toast(e.message); });
    });

    $('nd-ips-load').addEventListener('click', function () {
      var box = $('nd-ips');
      box.innerHTML = '<span class="hint-text">读取中…</span>';
      api('/api/nodes/' + encodeURIComponent(n.node_uuid) + '/active-ips').then(function (d) {
        var ips = d.active_ips || [];
        if (!ips.length) {
          box.innerHTML = '<span class="hint-text">暂无在线 IP（没有活跃连接，或节点刚创建）</span>';
          return;
        }
        box.innerHTML = ips.map(function (a) {
          return '<span class="chip">' + esc(a.ip) + ' <small>' + esc(a.proto || '') + '</small></span>';
        }).join('');
      }).catch(function (e) { box.innerHTML = ''; if (e.message !== '未登录') toast(e.message); });
    });

    $('nd-delete').addEventListener('click', function () {
      askConfirm('删除节点', '删除「' + (n.name || n.node_uuid) + '」？将下发删除任务，不可撤销。', function () {
        api('/api/nodes/' + encodeURIComponent(n.node_uuid), { method: 'DELETE' }).then(function () {
          toast('删除任务已下发');
          closeDrawer('node-detail-drawer');
          currentNode = null;
          refresh();
        }).catch(function (e) { if (e.message !== '未登录') toast(e.message); });
      });
    });
  }

  // ===== 分享抽屉 =====
  function copyText(text) {
    if (navigator.clipboard && window.isSecureContext) {
      return navigator.clipboard.writeText(text);
    }
    return new Promise(function (resolve, reject) {
      var ta = document.createElement('textarea');
      ta.value = text;
      ta.style.position = 'fixed';
      ta.style.opacity = '0';
      document.body.appendChild(ta);
      ta.focus();
      ta.select();
      var ok = false;
      try { ok = document.execCommand('copy'); } catch (e) { ok = false; }
      document.body.removeChild(ta);
      if (ok) resolve(); else reject(new Error('复制失败，请长按文本手动复制'));
    });
  }

  function openShare(uuid) {
    var n = state.nodes.filter(function (x) { return x.node_uuid === uuid; })[0];
    $('node-share-name').textContent = n ? (n.name || n.node_uuid) : uuid;
    $('node-share-list').innerHTML = '<div class="hint-text">读取中…</div>';
    openDrawer('node-share-drawer');
    api('/api/nodes/' + encodeURIComponent(uuid) + '/share').then(function (d) {
      var items = [];
      if (d.link) items.push({ label: '分享链接', text: d.link });
      (d.alternate_links || []).forEach(function (l, i) {
        items.push({ label: '备用链接 ' + (i + 1), text: l });
      });
      if (!items.length) {
        $('node-share-list').innerHTML = '<div class="empty">该节点暂无可分享链接</div>';
        return;
      }
      $('node-share-list').innerHTML = items.map(function (it, i) {
        return '<div class="link-field"><small>' + esc(it.label) + '</small>' +
          '<div class="link-row"><pre>' + esc(it.text) + '</pre>' +
          '<button class="mini-btn" type="button" data-copy="' + i + '">复制</button></div></div>';
      }).join('');
      $('node-share-list').dataset.items = JSON.stringify(items);
    }).catch(function (e) {
      $('node-share-list').innerHTML = '<div class="empty">' + esc(e.message) + '</div>';
    });
  }

  // ===== 添加节点抽屉 =====
  function openCreateNode() {
    $('nc-error').classList.add('hidden');
    $('nc-name').value = '';
    $('nc-port').value = '';
    $('nc-dynamic').innerHTML = '';
    openDrawer('node-create-drawer');
    Promise.all([
      api('/api/machines').then(function (d) {
        var list = d.machines || [];
        $('nc-machine').innerHTML = list.length
          ? '<option value="">选择服务器…</option>' + list.map(function (m) {
              var label = m.display_name || m.hostname || m.machine_id;
              return '<option value="' + esc(m.machine_id) + '">' +
                esc(label + (m.status === 'online' ? '' : '（离线）')) + '</option>';
            }).join('')
          : '<option value="">暂无已接入机器，请先接入</option>';
      }),
      loadCaps()
    ]).catch(function (e) { if (e.message !== '未登录') toast(e.message); });
  }

  function loadCaps() {
    if (state.caps.length) { fillProtocols(); return Promise.resolve(); }
    return api('/api/capabilities').then(function (d) {
      state.caps = d.protocols || [];
      fillProtocols();
    });
  }

  function fillProtocols() {
    $('nc-protocol').innerHTML = state.caps.map(function (c) {
      return '<option value="' + esc(c.type) + '">' + esc(c.label || c.type) + '</option>';
    }).join('');
    renderDynamicFields(state.caps.length ? state.caps[0].type : '');
  }

  function renderDynamicFields(protoType) {
    var cap = null;
    for (var i = 0; i < state.caps.length; i++) if (state.caps[i].type === protoType) cap = state.caps[i];
    var box = $('nc-dynamic');
    if (!cap || !(cap.fields || []).length) { box.innerHTML = ''; box.classList.add('hidden'); return; }
    box.classList.remove('hidden');
    box.innerHTML = cap.fields.map(function (f) {
      var head = '<div class="field"><small>' + esc(f.label + (f.required ? ' *' : '')) + '</small>';
      if (f.type === 'select') {
        return head + '<select data-key="' + esc(f.key) + '">' +
          (f.options || []).map(function (o) {
            return '<option value="' + esc(o) + '">' + esc(o || '（默认）') + '</option>';
          }).join('') + '</select></div>';
      }
      var type = f.type === 'password' ? 'password' : f.type === 'number' ? 'number' : 'text';
      return head + '<input type="' + type + '" data-key="' + esc(f.key) + '"' +
        (f.type === 'number' ? ' inputmode="numeric"' : '') +
        ' placeholder="' + esc(f.placeholder || '') + '"></div>';
    }).join('');
  }

  function submitNode() {
    var machineID = $('nc-machine').value;
    var name = $('nc-name').value.trim();
    var port = parseInt($('nc-port').value, 10);
    var protocol = $('nc-protocol').value;
    var err = $('nc-error');
    var fail = function (msg) { err.textContent = msg; err.classList.remove('hidden'); };

    if (!machineID) return fail('请选择服务器');
    if (!name) return fail('请填写节点名称');
    if (!port || port < 1 || port > 65535) return fail('端口需在 1-65535');

    var cfg = {};
    $('nc-dynamic').querySelectorAll('[data-key]').forEach(function (el) {
      if (el.value !== '' && el.value !== undefined) cfg[el.dataset.key] = el.value;
    });

    var btn = $('node-create-save');
    btn.disabled = true;
    btn.textContent = '提交中…';
    err.classList.add('hidden');
    api('/api/nodes', {
      method: 'POST',
      body: JSON.stringify({ machine_id: machineID, name: name, protocol: protocol, port: port, config: cfg })
    }).then(function () {
      closeDrawer('node-create-drawer');
      toast('任务已提交，等待 Agent 执行');
      show('nodes', true);
      refresh();
    }).catch(function (e) {
      fail(e.message || '创建失败');
    }).finally(function () {
      btn.disabled = false;
      btn.textContent = '创建节点';
    });
  }

  // ===== 确认抽屉 =====
  function askConfirm(title, text, onOk) {
    $('confirm-title').textContent = title;
    $('confirm-text').textContent = text;
    confirmAction = onOk;
    openDrawer('confirm-drawer');
  }

  // ===== 接入 =====
  function genToken() {
    var btn = $('btn-gen-token');
    btn.disabled = true;
    api('/api/enrollment/token', { method: 'POST' }).then(function (d) {
      if (!d.token) { toast(d.error || '生成失败'); return; }
      var panelUrl = location.protocol + '//' + location.host;
      var cmd = 'bash <(curl -fLSs https://raw.githubusercontent.com/k6nfmm7dbr-commits/sbx-pro/main/sbx.sh) agent -t ' +
        d.token + ' -u ' + panelUrl;
      $('join-result').innerHTML =
        '<div class="link-field"><small>接入命令（' + Math.round((d.expires_in || 900) / 60) + ' 分钟内有效，仅可使用一次）</small>' +
        '<div class="link-row"><pre>' + esc(cmd) + '</pre>' +
        '<button class="mini-btn" type="button" data-copy-join="1">复制</button></div></div>';
      $('join-result').dataset.cmd = cmd;
      toast('接入命令已生成');
    }).catch(function (e) {
      if (e.message !== '未登录') toast(e.message);
    }).finally(function () { btn.disabled = false; });
  }

  // ===== 节点操作（footer 按钮）=====
  function nodeAction(action) {
    if (!currentNode) return;
    var n = currentNode;
    var map = {
      enable: { title: '启用节点', text: '恢复「' + (n.name || n.node_uuid) + '」的对外服务？', path: 'enable' },
      disable: { title: '停用节点', text: '停用「' + (n.name || n.node_uuid) + '」？凭据保留，可随时重新启用。', path: 'disable' },
      restart: {
        title: '重启 sing-box',
        text: '重启机器「' + machineName(n.machine_id) + '」上的 sing-box？该机器上所有节点会短暂中断。',
        path: 'restart'
      }
    };
    var cfg = map[action];
    if (!cfg) return;
    askConfirm(cfg.title, cfg.text, function () {
      api('/api/nodes/' + encodeURIComponent(n.node_uuid) + '/' + cfg.path, { method: 'POST' })
        .then(function () {
          toast('任务已下发');
          closeDrawer('node-detail-drawer');
          refresh();
        })
        .catch(function (e) { if (e.message !== '未登录') toast(e.message); });
    });
  }

  // ===== 事件绑定 =====
  function bind() {
    document.querySelectorAll('.tab[data-view]').forEach(function (b) {
      b.addEventListener('click', function () { show(b.dataset.view, true); });
    });
    window.addEventListener('popstate', function () {
      show((location.hash || '#home').slice(1), false);
    });

    $('drawer-mask').addEventListener('click', closeAllDrawers);
    document.addEventListener('keydown', function (e) {
      if (e.key === 'Escape') closeAllDrawers();
    });

    // 卡片上的动态按钮统一事件委托（重渲染后句柄不失效）。
    document.addEventListener('click', function (e) {
      if (!e.target.closest) return;
      var t = e.target.closest('[data-machine-open]');
      if (t) { openMachine(t.getAttribute('data-machine-open')); return; }
      t = e.target.closest('[data-node-detail]');
      if (t) { openNode(t.getAttribute('data-node-detail')); return; }
      t = e.target.closest('[data-node-share]');
      if (t) { openShare(t.getAttribute('data-node-share')); return; }
      t = e.target.closest('[data-copy]');
      if (t) {
        var items = JSON.parse($('node-share-list').dataset.items || '[]');
        var item = items[Number(t.getAttribute('data-copy'))];
        if (item) {
          copyText(item.text).then(function () { toast('已复制到剪贴板'); })
            .catch(function (err) { toast(err.message); });
        }
        return;
      }
      t = e.target.closest('[data-copy-join]');
      if (t) {
        copyText($('join-result').dataset.cmd || '').then(function () { toast('已复制到剪贴板'); })
          .catch(function (err) { toast(err.message); });
      }
    });

    $('machine-drawer-close').addEventListener('click', function () { closeDrawer('machine-drawer'); });
    $('machine-drawer-done').addEventListener('click', function () { closeDrawer('machine-drawer'); });
    $('machine-delete').addEventListener('click', function () {
      if (!currentMachine) return;
      var m = currentMachine;
      askConfirm('删除管理关系', '解除对「' + (m.display_name || m.hostname || m.machine_id) +
        '」的管理？只删控制台侧记录，远端 Agent 不会自动卸载。', function () {
        api('/api/machines/' + encodeURIComponent(m.machine_id), { method: 'DELETE' }).then(function () {
          toast('已解除管理关系');
          closeDrawer('machine-drawer');
          currentMachine = null;
          refresh();
        }).catch(function (e) { if (e.message !== '未登录') toast(e.message); });
      });
    });

    $('node-detail-close').addEventListener('click', function () { closeDrawer('node-detail-drawer'); });
    $('node-act-toggle').addEventListener('click', function () {
      nodeAction(this.dataset.action || 'disable');
    });
    $('node-act-restart').addEventListener('click', function () { nodeAction('restart'); });
    $('node-act-share').addEventListener('click', function () {
      if (!currentNode) return;
      var uuid = currentNode.node_uuid;
      closeDrawer('node-detail-drawer');
      openShare(uuid);
    });

    $('node-share-close').addEventListener('click', function () { closeDrawer('node-share-drawer'); });
    $('node-share-done').addEventListener('click', function () { closeDrawer('node-share-drawer'); });

    $('node-create-open').addEventListener('click', openCreateNode);
    $('node-create-close').addEventListener('click', function () { closeDrawer('node-create-drawer'); });
    $('node-create-cancel').addEventListener('click', function () { closeDrawer('node-create-drawer'); });
    $('node-create-save').addEventListener('click', submitNode);
    $('nc-protocol').addEventListener('change', function () { renderDynamicFields(this.value); });

    $('confirm-close').addEventListener('click', function () { closeDrawer('confirm-drawer'); });
    $('confirm-cancel').addEventListener('click', function () { closeDrawer('confirm-drawer'); confirmAction = null; });
    $('confirm-ok').addEventListener('click', function () {
      var fn = confirmAction;
      confirmAction = null;
      closeDrawer('confirm-drawer');
      if (fn) fn();
    });

    $('btn-gen-token').addEventListener('click', genToken);
    $('machines-refresh').addEventListener('click', function () {
      refresh().then(function () { toast('已刷新'); });
    });

    $('logout-button').addEventListener('click', function () {
      api('/api/logout', { method: 'POST' }).then(function () {
        location.replace('/login');
      }).catch(function () { location.replace('/login'); });
    });

    window.addEventListener('online', function () { refresh(); });
    document.addEventListener('visibilitychange', function () {
      if (!document.hidden) refresh();
    });
  }

  // ===== 启动 =====
  bind();
  show((location.hash || '#home').slice(1), false);
  refresh();
  setInterval(function () {
    refresh();
    renderSync();
  }, 5000);
})();
