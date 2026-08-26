// SBX Pro 控制台前端逻辑
(function () {
  var token = null;

  function api(path, opts) {
    opts = opts || {};
    var headers = { 'Content-Type': 'application/json' };
    if (token) headers['Authorization'] = 'Bearer ' + token;
    return fetch(path, { headers: headers, method: opts.method || 'GET', body: opts.body })
      .then(function (r) {
        if (r.status === 401) { location.href = '/login'; throw new Error('unauthorized'); }
        return r.json().catch(function () { return {}; });
      });
  }

  function setStatus(txt, stale) {
    document.getElementById('status-txt').textContent = txt;
    document.getElementById('pulse').className = 'pulse' + (stale ? ' stale' : '');
  }

  function fmtBytes(n) {
    if (n == null || n === 0) return '0 B';
    var u = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
    var i = 0; var v = n;
    while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
    return (i === 0 ? v : v.toFixed(v >= 100 ? 0 : v >= 10 ? 1 : 2)) + ' ' + u[i];
  }

  function renderMachines(cb) {
    api('/api/machines').then(function (d) {
      var list = d.machines || [];
      document.getElementById('kpi-machines').textContent = list.length;
      var online = list.filter(function (m) { return m.status === 'online'; }).length;
      document.getElementById('kpi-online').textContent = online;
      document.getElementById('kpi-machine-count').textContent = list.length;

      var el = document.getElementById('machine-list');
      document.getElementById('machine-empty').style.display = list.length ? 'none' : 'block';
      el.innerHTML = list.map(function (m) {
        var cls = m.status === 'online' ? 'up' : '';
        return '<div class="node-card">' +
          '<div class="node-top"><div class="node-title">' +
          '<div class="node-name">' + (m.display_name || m.hostname || m.machine_id) + '</div>' +
          '<div class="node-meta-line"><span class="chip">' + m.arch + '</span>' +
          '<span class="port">' + (m.ipv4 || '') + '</span></div>' +
          '</div>' +
          '<div class="node-rate"><b class="' + cls + '">' + (m.status === 'online' ? '在线' : '离线') + '</b></div>' +
          '</div>' +
          '<div class="node-stats">' +
          '<div class="node-stat">Agent <b>' + (m.agent_version || '-') + '</b></div>' +
          '<div class="node-stat">最近心跳 <b>' + (m.last_seen ? fmtAgo(m.last_seen) : '-') + '</b></div>' +
          '</div></div>';
      }).join('');
      if (cb) cb();
    });
  }

  function fmtAgo(ts) {
    var d = Math.floor(Date.now() / 1000 - ts);
    if (d < 60) return d + 's';
    if (d < 3600) return Math.floor(d / 60) + 'm';
    return Math.floor(d / 3600) + 'h';
  }

  function renderNodes(cb) {
    api('/api/nodes').then(function (d) {
      var list = d.nodes || [];
      document.getElementById('kpi-nodes').textContent = list.length;
      var el = document.getElementById('node-list');
      document.getElementById('node-empty').style.display = list.length ? 'none' : 'block';
      el.innerHTML = list.map(function (n) {
        return '<div class="node-card">' +
          '<div class="node-top"><div class="node-title">' +
          '<div class="node-name">' + (n.name || n.node_uuid) + '</div>' +
          '<div class="node-meta-line"><span class="chip">' + n.protocol + '</span>' +
          '<span class="port">:' + n.port + '</span></div>' +
          '</div>' +
          '<div class="node-rate"><b>' + n.status + '</b></div>' +
          '</div></div>';
      }).join('');
      if (cb) cb();
    });
  }

  function renderTraffic() {
    api('/api/traffic').then(function (d) {
      document.getElementById('kpi-total').textContent = fmtBytes(d.rx_total);
    });
  }

  function refreshAll() {
    renderMachines();
    renderNodes();
    renderTraffic();
    setStatus('已连接', false);
  }

  function genToken() {
    api('/api/enrollment/token', { method: 'POST' }).then(function (d) {
      if (!d.token) { toast(d.error || '生成失败'); return; }
      var panelUrl = location.protocol + '//' + location.host;
      var cmd = "bash <(curl -fLSs https://raw.githubusercontent.com/k6nfmm7dbr-commits/sbx-pro/main/sbx.sh) agent -t " +
        d.token + " -u " + panelUrl;
      document.getElementById('join-result').innerHTML =
        '<div class="node-card" style="background:#f0fdf4">' +
        '<div style="font-size:12px;color:var(--muted);margin-bottom:6px">接入命令（复制到节点机执行）：</div>' +
        '<code style="word-break:break-all;font-size:12px;display:block">' + cmd + '</code>' +
        '<div style="margin-top:10px"><button class="btn-primary" onclick="navigator.clipboard.writeText(' + JSON.stringify(cmd) + ')">复制命令</button></div>' +
        '</div>';
      toast('Token 已生成，15 分钟内有效');
    });
  }

  function fillMachineSelect() {
    api('/api/machines').then(function (d) {
      var sel = document.getElementById('an-machine');
      var html = '<option value="">选择服务器…</option>';
      (d.machines || []).forEach(function (m) {
        html += '<option value="' + m.machine_id + '">' + (m.hostname || m.machine_id) + '</option>';
      });
      sel.innerHTML = html;
    });
  }

  function closeModal() { document.getElementById('add-node-modal').style.display = 'none'; }

  function submitNode(e) {
    e.preventDefault();
    var fd = new FormData(document.getElementById('add-node-form'));
    var machine = fd.get('machine_id');
    var protocol = fd.get('protocol');
    var port = parseInt(fd.get('port'), 10);
    var name = fd.get('name');
    var sni = fd.get('sni') || '';
    var password = fd.get('password') || '';

    if (!machine) { toast('请选择服务器'); return; }
    var cfg = { sni: sni };
    if (password) cfg.password = password;
    api('/api/nodes', {
      method: 'POST',
      body: JSON.stringify({ machine_id: machine, name: name, protocol: protocol, port: port, config: cfg })
    }).then(function (d) {
      closeModal();
      toast(d.node_uuid ? '节点创建任务已下发' : (d.error || '创建失败'));
      renderNodes();
    });
  }

  function toast(msg) {
    var t = document.getElementById('toast');
    t.textContent = msg;
    t.className = 'toast show';
    setTimeout(function () { t.className = 'toast'; }, 2500);
  }

  document.querySelectorAll('.tab').forEach(function (btn) {
    btn.addEventListener('click', function () {
      document.querySelectorAll('.tab').forEach(function (b) { b.classList.remove('on'); });
      btn.classList.add('on');
      document.querySelectorAll('.view').forEach(function (v) { v.classList.remove('on'); });
      document.getElementById('view-' + btn.dataset.view).classList.add('on');
    });
  });

  document.getElementById('btn-gen-token').addEventListener('click', genToken);
  document.getElementById('btn-add-node').addEventListener('click', function () {
    fillMachineSelect();
    document.getElementById('add-node-modal').style.display = 'flex';
  });
  document.getElementById('add-node-form').addEventListener('submit', submitNode);

  // 从 cookie 读 token（login 重定向后由后端鉴权，也可直接依赖 cookie）。
  var m = document.cookie.match(/(?:^|;\s*)sbx_token=([^;]+)/);
  if (m && m[1]) { token = decodeURIComponent(m[1]); }

  refreshAll();
  setInterval(refreshAll, 5000);
})();
