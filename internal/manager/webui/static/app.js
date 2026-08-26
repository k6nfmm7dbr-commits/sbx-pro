// SBX Pro 控制台前端逻辑
// 原则：不使用 inline onclick；所有动态内容经 escapeHTML；fetch 严格检查 response.ok。
(function () {
  'use strict';

  function $(sel) { return document.querySelector(sel); }

  function escapeHTML(s) {
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
    if (d < 60) return d + ' 秒前';
    if (d < 3600) return Math.floor(d / 60) + ' 分钟前';
    if (d < 86400) return Math.floor(d / 3600) + ' 小时前';
    return Math.floor(d / 86400) + ' 天前';
  }

  // ===== API（严格错误处理）=====
  function api(path, opts) {
    opts = opts || {};
    var method = opts.method || 'GET';
    var headers = {};
    if (opts.body !== undefined && opts.body !== null) headers['Content-Type'] = 'application/json';
    var ctrl = new AbortController();
    var timer = setTimeout(function () { ctrl.abort(); }, 15000);
    return fetch(path, {
      method: method, headers: headers, body: opts.body,
      credentials: 'same-origin', signal: ctrl.signal
    }).then(function (r) {
      clearTimeout(timer);
      if (r.status === 401) { location.href = '/login'; throw new Error('未登录'); }
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

  // ===== 连接状态 =====
  function setStatus(txt, cls) {
    $('#status-txt').textContent = txt;
    $('#pulse').className = 'pulse' + (cls ? ' ' + cls : '');
  }

  // ===== Toast =====
  var toastTimer = null;
  function toast(msg, isError) {
    var t = $('#toast');
    t.textContent = msg;
    t.className = 'toast show' + (isError ? ' toast-error' : '');
    clearTimeout(toastTimer);
    toastTimer = setTimeout(function () { t.className = 'toast'; }, 3000);
  }

  // ===== 缓存 =====
  var machines = [];
  var nodes = [];

  // ===== 渲染：机器 =====
  function renderMachines() {
    api('/api/machines').then(function (d) {
      machines = d.machines || [];
      $('#kpi-machines').textContent = machines.length;
      $('#kpi-machine-count').textContent = machines.length;
      $('#machines-count').textContent = machines.length;
      var online = machines.filter(function (m) { return m.status === 'online'; }).length;
      $('#kpi-online').textContent = online;

      var html = machines.map(function (m) {
        var on = m.status === 'online';
        return '<div class="node-card">' +
          '<div class="node-top"><div class="node-title">' +
          '<div class="node-name">' + escapeHTML(m.display_name || m.hostname || m.machine_id) + '</div>' +
          '<div class="node-meta-line">' +
          '<span class="chip">' + escapeHTML(m.arch || '-') + '</span>' +
          '<span class="port">' + escapeHTML(m.ipv4 || '') + '</span>' +
          '<span class="chip chip-rev">rev ' + (m.applied_revision || 0) + '/' + (m.config_revision || 0) + '</span>' +
          '</div></div>' +
          '<div class="node-rate"><b class="' + (on ? 'up' : 'down') + '">' + (on ? '在线' : '离线') + '</b>' +
          '<span class="port">' + escapeHTML(m.agent_version || '-') + '</span></div>' +
          '</div>' +
          '<div class="node-stats">' +
          '<div class="node-stat">最近心跳 <b>' + escapeHTML(fmtAgo(m.last_seen)) + '</b></div>' +
          '<div class="node-stat">' + escapeHTML(m.os || '-') + '</div>' +
          '</div>' +
          '<div class="node-actions"><button class="btn-mini btn-ghost" data-del-machine="' + escapeHTML(m.machine_id) + '">删除管理关系</button></div>' +
          '</div>';
      }).join('');

      $('#machine-list').innerHTML = html;
      $('#home-machine-list').innerHTML = html;
      $('#machine-empty').style.display = machines.length ? 'none' : 'block';
      $('#home-machine-empty').style.display = machines.length ? 'none' : 'block';
    }).catch(function (e) { throw e; });
  }

  // ===== 渲染：节点 =====
  function renderNodes() {
    return api('/api/nodes').then(function (d) {
      nodes = d.nodes || [];
      $('#kpi-nodes').textContent = nodes.length;
      $('#node-list').innerHTML = nodes.map(function (n) {
        return '<div class="node-card">' +
          '<div class="node-top"><div class="node-title">' +
          '<div class="node-name">' + escapeHTML(n.name || n.node_uuid) + '</div>' +
          '<div class="node-meta-line">' +
          '<span class="chip">' + escapeHTML(protoLabel(n.protocol)) + '</span>' +
          '<span class="port">:' + escapeHTML(n.port) + '</span>' +
          '</div></div>' +
          '<div class="node-rate"><b class="' + statusClass(n.status) + '">' + escapeHTML(statusText(n.status)) + '</b></div>' +
          '</div>' +
          '<div class="node-actions">' +
          '<button class="btn-mini btn-ghost" data-node-detail="' + escapeHTML(n.node_uuid) + '">详情</button>' +
          '<button class="btn-mini btn-ghost" data-node-share="' + escapeHTML(n.node_uuid) + '">分享</button>' +
          '</div></div>';
      }).join('');
      $('#node-empty').style.display = nodes.length ? 'none' : 'block';

      // 首页异常节点
      var bad = nodes.filter(function (n) {
        return n.status === 'create_failed' || n.status === 'config_error' || n.status === 'provisioning';
      });
      $('#home-bad-nodes').innerHTML = bad.map(function (n) {
        return '<div class="node-card"><div class="node-top"><div class="node-title">' +
          '<div class="node-name">' + escapeHTML(n.name || n.node_uuid) + '</div>' +
          '<div class="node-meta-line"><span class="chip">' + escapeHTML(protoLabel(n.protocol)) + '</span></div>' +
          '</div><div class="node-rate"><b class="down">' + escapeHTML(statusText(n.status)) + '</b></div></div></div>';
      }).join('');
      $('#home-bad-empty').style.display = bad.length ? 'none' : 'block';
    });
  }

  // ===== 渲染：流量 =====
  function renderTraffic() {
    return api('/api/traffic').then(function (d) {
      $('#kpi-total').textContent = fmtBytes((d.rx_total || 0) + (d.tx_total || 0));
      $('#traffic-rx').textContent = fmtBytes(d.rx_total);
      $('#traffic-tx').textContent = fmtBytes(d.tx_total);
      var totals = d.totals || [];
      $('#traffic-list').innerHTML = totals.map(function (t) {
        return '<div class="node-card"><div class="node-top"><div class="node-title">' +
          '<div class="node-name">' + escapeHTML(t.node_uuid || t.machine_id || 'system') + '</div>' +
          '<div class="node-meta-line"><span class="port">' + escapeHTML(t.machine_id) + '</span></div>' +
          '</div></div>' +
          '<div class="node-stats">' +
          '<div class="node-stat">上传 rx <b>' + escapeHTML(fmtBytes(t.rx)) + '</b></div>' +
          '<div class="node-stat">下载 tx <b>' + escapeHTML(fmtBytes(t.tx)) + '</b></div>' +
          '</div></div>';
      }).join('');
      $('#traffic-empty').style.display = totals.length ? 'none' : 'block';
    });
  }

  function protoLabel(t) {
    var m = { vless: 'VLESS Reality', shadowsocks: 'Shadowsocks 2022', trojan: 'Trojan', anytls: 'AnyTLS', snell: 'Snell' };
    return m[t] || t;
  }
  function statusText(s) {
    var m = { provisioning: '下发中', active: '正常', create_failed: '创建失败', update_pending: '待同步', delete_pending: '删除中', disabled: '已停用', config_error: '配置异常', sync_pending: '待同步' };
    return m[s] || s;
  }
  function statusClass(s) {
    if (s === 'active') return 'up';
    if (s === 'create_failed' || s === 'config_error') return 'down';
    return '';
  }

  // ===== 刷新 =====
  function refreshAll() {
    var failures = 0;
    var done = function () {
      setStatus(failures === 0 ? '已连接' : '连接异常', failures === 0 ? '' : 'stale');
    };
    return Promise.all([
      renderMachines().catch(function () { failures++; }),
      renderNodes().catch(function () { failures++; }),
      renderTraffic().catch(function () { failures++; })
    ]).then(done, done);
  }

  // ===== Modal 管理 =====
  function openModal(el) {
    el.hidden = false;
    document.body.classList.add('modal-open');
  }
  function closeModal(el) {
    el.hidden = true;
    document.body.classList.remove('modal-open');
  }

  // ===== 添加节点 =====
  var caps = [];
  function loadCaps() {
    return api('/api/capabilities').then(function (d) {
      caps = d.protocols || [];
      var sel = $('#an-protocol');
      sel.innerHTML = caps.map(function (c) {
        return '<option value="' + escapeHTML(c.type) + '">' + escapeHTML(c.label) + '</option>';
      }).join('');
      renderDynamicFields(caps.length ? caps[0].type : '');
    });
  }

  function renderDynamicFields(protoType) {
    var cap = null;
    for (var i = 0; i < caps.length; i++) if (caps[i].type === protoType) cap = caps[i];
    var box = $('#an-dynamic');
    box.innerHTML = '';
    if (!cap) return;
    cap.fields.forEach(function (f) {
      var label = document.createElement('label');
      label.className = 'field-label';
      label.textContent = f.label + (f.required ? ' *' : '');
      box.appendChild(label);

      var el;
      if (f.type === 'select') {
        el = document.createElement('select');
        el.name = 'f_' + f.key;
        el.dataset.key = f.key;
        f.options.forEach(function (o) {
          var op = document.createElement('option');
          op.value = o;
          op.textContent = o || '（默认）';
          el.appendChild(op);
        });
      } else {
        el = document.createElement('input');
        el.type = f.type === 'password' ? 'password' : f.type === 'number' ? 'number' : 'text';
        if (f.type === 'number') el.inputMode = 'numeric';
        el.name = 'f_' + f.key;
        el.dataset.key = f.key;
        el.placeholder = f.placeholder || '';
        if (f.required) el.required = true;
      }
      box.appendChild(el);
    });
  }

  function fillMachineSelect() {
    return api('/api/machines').then(function (d) {
      var list = d.machines || [];
      var sel = $('#an-machine');
      if (!list.length) {
        sel.innerHTML = '<option value="">暂无已接入机器，请先接入</option>';
        return;
      }
      sel.innerHTML = '<option value="">选择服务器…</option>' + list.map(function (m) {
        var label = m.display_name || m.hostname || m.machine_id;
        var suffix = m.status === 'online' ? '' : '（离线）';
        return '<option value="' + escapeHTML(m.machine_id) + '">' + escapeHTML(label + suffix) + '</option>';
      }).join('');
    });
  }

  function submitNode(e) {
    e.preventDefault();
    var machineID = $('#an-machine').value;
    if (!machineID) { showFormError('请选择服务器'); return; }
    var name = $('#an-name').value.trim();
    var port = parseInt($('#an-port').value, 10);
    var protocol = $('#an-protocol').value;
    if (!name) { showFormError('请填写节点名称'); return; }
    if (!port || port < 1 || port > 65535) { showFormError('端口需在 1-65535'); return; }

    var cfg = {};
    var fields = $('#an-dynamic').querySelectorAll('[data-key]');
    fields.forEach(function (f) {
      var v = f.value;
      if (v !== '' && v !== undefined) cfg[f.dataset.key] = v;
    });

    var btn = $('#btn-submit-node');
    btn.disabled = true;
    btn.textContent = '提交中…';
    hideFormError();

    api('/api/nodes', {
      method: 'POST',
      body: JSON.stringify({ machine_id: machineID, name: name, protocol: protocol, port: port, config: cfg })
    }).then(function (d) {
      closeModal($('#add-node-modal'));
      resetAddForm();
      toast('任务已提交，等待 Agent 执行（' + (d.status || '') + '）');
      refreshAll();
    }).catch(function (e) {
      showFormError(e.message || '创建失败');
    }).finally(function () {
      btn.disabled = false;
      btn.textContent = '创建节点';
    });
  }

  function showFormError(msg) { var el = $('#add-node-error'); el.textContent = msg; el.hidden = false; }
  function hideFormError() { $('#add-node-error').hidden = true; }
  function resetAddForm() {
    $('#add-node-form').reset();
    $('#an-dynamic').innerHTML = '';
    hideFormError();
  }

  // ===== 节点详情 / 操作 =====
  function openNodeDetail(uuid) {
    api('/api/nodes/' + encodeURIComponent(uuid)).then(function (n) {
      $('#node-detail-title').textContent = n.name || n.node_uuid;
      var html = '<div class="detail-row"><span>协议</span><b>' + escapeHTML(protoLabel(n.protocol)) + '</b></div>' +
        '<div class="detail-row"><span>端口</span><b>' + escapeHTML(n.port) + '</b></div>' +
        '<div class="detail-row"><span>状态</span><b class="' + statusClass(n.status) + '">' + escapeHTML(statusText(n.status)) + '</b></div>' +
        '<div class="detail-row"><span>流量额度</span><b>' + (n.quota_limit ? fmtBytes(n.quota_limit) : '无限制') + '</b></div>' +
        '<div class="detail-row"><span>在线 IP 限制</span><b>' + (n.ip_limit || '无限制') + '</b></div>';
      $('#node-detail-body').innerHTML = html;
      $('#share-result').innerHTML = '';

      // 操作按钮
      var ops = document.createElement('div');
      ops.className = 'node-actions';
      var mk = function (label, action, danger) {
        var b = document.createElement('button');
        b.className = 'btn-mini ' + (danger ? 'btn-danger' : 'btn-ghost');
        b.textContent = label;
        b.addEventListener('click', function () {
          closeModal($('#node-detail-modal'));
          nodeAction(uuid, action, danger);
        });
        return b;
      };
      if (n.status === 'disabled') ops.appendChild(mk('启用', 'enable'));
      else if (n.status === 'active') ops.appendChild(mk('停用', 'disable'));
      ops.appendChild(mk('重启 sing-box', 'restart'));
      ops.appendChild(mk('删除', 'delete', true));
      $('#node-detail-body').appendChild(ops);

      openModal($('#node-detail-modal'));
    }).catch(function (e) { toast(e.message, true); });
  }

  function nodeAction(uuid, action, danger) {
    var map = {
      enable: { text: '启用该节点？', fn: function (ok) { return ok && api('/api/nodes/' + encodeURIComponent(uuid) + '/enable', { method: 'POST' }); } },
      disable: { text: '停用该节点？（凭据保留，不删除）', fn: function (ok) { return ok && api('/api/nodes/' + encodeURIComponent(uuid) + '/disable', { method: 'POST' }); } },
      restart: { text: '重启该机器的 sing-box？', fn: function (ok) { return ok && api('/api/nodes/' + encodeURIComponent(uuid) + '/restart', { method: 'POST' }); } },
      delete: { text: '删除该节点？（将下发删除任务，不可撤销）', fn: function (ok) { return ok && api('/api/nodes/' + encodeURIComponent(uuid), { method: 'DELETE' }); } }
    };
    var cfg = map[action];
    if (!cfg) return;
    confirmDialog('确认操作', cfg.text, function () {
      cfg.fn(true).then(function () {
        toast('任务已下发');
        refreshAll();
      }).catch(function (e) { toast(e.message, true); });
    });
  }

  function showShare(uuid) {
    api('/api/nodes/' + encodeURIComponent(uuid) + '/share').then(function (d) {
      var box = $('#share-result');
      var html = '<div class="detail-row"><span>分享链接</span></div>';
      if (d.link) {
        html += '<code class="share-code">' + escapeHTML(d.link) + '</code>';
      }
      (d.alternate_links || []).forEach(function (l) {
        html += '<code class="share-code">' + escapeHTML(l) + '</code>';
      });
      html += '<button class="btn-mini btn-ghost" id="btn-copy-share">复制链接</button>';
      box.innerHTML = html;
      var copyBtn = $('#btn-copy-share');
      if (copyBtn) copyBtn.addEventListener('click', function () {
        var txt = d.link || '';
        if (navigator.clipboard && navigator.clipboard.writeText) {
          navigator.clipboard.writeText(txt).then(function () { toast('已复制'); });
        } else {
          toast('复制失败，请手动长按选择');
        }
      });
    }).catch(function (e) { toast(e.message, true); });
  }

  // ===== 确认弹层 =====
  var currentConfirmOk = null;
  function confirmDialog(title, text, onOk) {
    $('#confirm-title').textContent = title;
    $('#confirm-text').textContent = text;
    currentConfirmOk = onOk;
    openModal($('#confirm-modal'));
  }

  // ===== 接入 =====
  function genToken() {
    var btn = $('#btn-gen-token');
    btn.disabled = true;
    api('/api/enrollment/token', { method: 'POST' }).then(function (d) {
      if (!d.token) { toast(d.error || '生成失败', true); return; }
      var panelUrl = location.protocol + '//' + location.host;
      var cmd = 'bash <(curl -fLSs https://raw.githubusercontent.com/k6nfmm7dbr-commits/sbx-pro/main/sbx.sh) agent -t ' + d.token + ' -u ' + panelUrl;
      var box = $('#join-result');
      box.innerHTML = '<div class="node-card" style="background:#f0fdf4">' +
        '<div style="font-size:12px;color:var(--muted);margin-bottom:6px">接入命令（复制到节点机执行，' + (d.expires_in / 60) + ' 分钟内有效）：</div>' +
        '<code class="share-code">' + escapeHTML(cmd) + '</code>' +
        '<button class="btn-mini btn-ghost" id="btn-copy-join">复制命令</button></div>';
      var cb = $('#btn-copy-join');
      if (cb) cb.addEventListener('click', function () {
        if (navigator.clipboard && navigator.clipboard.writeText) {
          navigator.clipboard.writeText(cmd).then(function () { toast('已复制'); });
        } else { toast('复制失败，请手动长按选择'); }
      });
      toast('Token 已生成，一次性、15 分钟有效');
    }).catch(function (e) { toast(e.message, true); }).finally(function () { btn.disabled = false; });
  }

  // ===== 事件绑定 =====
  function init() {
    // 底部导航
    document.querySelectorAll('.tab').forEach(function (btn) {
      btn.addEventListener('click', function () {
        document.querySelectorAll('.tab').forEach(function (b) { b.classList.remove('on'); });
        btn.classList.add('on');
        document.querySelectorAll('.view').forEach(function (v) { v.classList.remove('on'); });
        $('#view-' + btn.dataset.view).classList.add('on');
      });
    });

    // 添加节点
    $('#btn-add-node').addEventListener('click', function () {
      fillMachineSelect().then(function () {
        if (!caps.length) loadCaps();
        resetAddForm();
        openModal($('#add-node-modal'));
      }).catch(function (e) { toast(e.message, true); });
    });
    $('#btn-cancel-node').addEventListener('click', function () { closeModal($('#add-node-modal')); });
    $('#add-node-form').addEventListener('submit', submitNode);
    $('#an-protocol').addEventListener('change', function () { renderDynamicFields(this.value); });

    // 遮罩点击关闭
    [['#add-node-modal', '#add-node-modal .modal-box'], ['#node-detail-modal', '#node-detail-modal .modal-box'], ['#confirm-modal', '#confirm-modal .modal-box']].forEach(function (pair) {
      var modal = $(pair[0]);
      modal.addEventListener('click', function (e) {
        if (e.target === modal) closeModal(modal);
      });
    });
    $('#btn-close-detail').addEventListener('click', function () { closeModal($('#node-detail-modal')); });
    $('#btn-confirm-cancel').addEventListener('click', function () { closeModal($('#confirm-modal')); currentConfirmOk = null; });
    $('#btn-confirm-ok').addEventListener('click', function () {
      closeModal($('#confirm-modal'));
      if (currentConfirmOk) { var f = currentConfirmOk; currentConfirmOk = null; f(); }
    });

    // Esc 关闭
    document.addEventListener('keydown', function (e) {
      if (e.key === 'Escape') {
        ['#add-node-modal', '#node-detail-modal', '#confirm-modal'].forEach(function (sel) {
          var m = $(sel);
          if (m && !m.hidden) closeModal(m);
        });
      }
    });

    // 生成 token
    $('#btn-gen-token').addEventListener('click', genToken);

    // 事件委托：机器删除 / 节点详情 / 节点分享
    document.addEventListener('click', function (e) {
      var del = e.target.closest && e.target.closest('[data-del-machine]');
      if (del) {
        var mid = del.getAttribute('data-del-machine');
        confirmDialog('删除机器', '解除该机器的管理关系？（只删 Manager 侧，远端 Agent 不会自动卸载）', function () {
          api('/api/machines/' + encodeURIComponent(mid), { method: 'DELETE' }).then(function () {
            toast('已解除管理关系');
            refreshAll();
          }).catch(function (err) { toast(err.message, true); });
        });
        return;
      }
      var detail = e.target.closest && e.target.closest('[data-node-detail]');
      if (detail) { openNodeDetail(detail.getAttribute('data-node-detail')); return; }
      var share = e.target.closest && e.target.closest('[data-node-share]');
      if (share) { showShare(share.getAttribute('data-node-share')); return; }
    });

    // 初始加载
    loadCaps().catch(function () {});
    refreshAll();
    setInterval(refreshAll, 5000);
  }

  init();
})();
