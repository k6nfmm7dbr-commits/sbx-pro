#!/bin/bash
# sbx.sh — SBX Pro 一键安装 + 交互管理菜单
#
# 安装 Manager（A 机）:
#   bash <(curl -fLSs https://raw.githubusercontent.com/k6nfmm7dbr-commits/sbx-pro/main/sbx.sh) manager
#
# 安装 Agent（B/C/D 机）:
#   bash <(curl -fLSs https://raw.githubusercontent.com/k6nfmm7dbr-commits/sbx-pro/main/sbx.sh) agent -t TOKEN -u https://panel.example.com
#
# 安装完成后运行 `sbx` 打开管理菜单。
# 二进制从 dist 分支下载（rolling latest），sha256 校验后安装。

set -euo pipefail

RAW_BASE="${SBX_RAW_BASE:-https://raw.githubusercontent.com/k6nfmm7dbr-commits/sbx-pro/dist}"
RAW_URL="${SBX_RAW_URL:-https://raw.githubusercontent.com/k6nfmm7dbr-commits/sbx-pro/main/sbx.sh}"
BIN_DIR=/usr/local/bin
SELF_PATH=/usr/local/bin/sbx
ROLE_MANAGER=/etc/sbx-pro/role
ROLE_AGENT=/etc/sbx-agent/role

C_RESET='\033[0m'; C_GREEN='\033[32m'; C_YEL='\033[33m'; C_RED='\033[31m'; C_BLUE='\033[34m'
info() { printf '%s[*]%s %s\n' "$C_BLUE" "$C_RESET" "$*"; }
ok()   { printf '%s[+]%s %s\n' "$C_GREEN" "$C_RESET" "$*"; }
warn() { printf '%s[!]%s %s\n' "$C_YEL" "$C_RESET" "$*" >&2; }
die()  { printf '%s[x]%s %s\n' "$C_RED" "$C_RESET" "$*" >&2; exit 1; }

sb_arch() {
  case "$(uname -m)" in
    x86_64)  echo amd64 ;;
    aarch64) echo arm64 ;;
    armv7l)  echo armv7 ;;
    armv6l)  echo armv6 ;;
    i686)    echo 386 ;;
    riscv64) echo riscv64 ;;
    *) die "不支持的架构: $(uname -m)" ;;
  esac
}
ARCH="$(sb_arch)"

# 角色识别
detect_role() {
  if [[ -f "$ROLE_AGENT" ]]; then echo agent
  elif [[ -f "$ROLE_MANAGER" ]]; then echo manager
  else echo ""; fi
}

# 下载二进制 + SHA256SUMS + 校验 + 原子安装（失败不替换旧二进制）。
download_bin() { # download_bin <name>
  local name="$1"
  local file="${name}-linux-${ARCH}"
  local tmp; tmp=$(mktemp -d)
  info "下载 ${file}"
  curl -fLSs -m 300 -o "$tmp/${file}" "${RAW_BASE}/${file}" || { rm -rf "$tmp"; die "下载 ${file} 失败（可设 SBX_RAW_BASE 镜像）"; }
  curl -fLSs -m 60 -o "$tmp/SHA256SUMS" "${RAW_BASE}/SHA256SUMS" || { rm -rf "$tmp"; die "下载 SHA256SUMS 失败"; }
  (cd "$tmp" && grep -E "${file}$" SHA256SUMS | sha256sum -c -) || { rm -rf "$tmp"; die "${name} 校验失败，已中止"; }
  install -m 0755 "$tmp/${file}" "${BIN_DIR}/${name}" || { rm -rf "$tmp"; die "${name} 安装失败"; }
  rm -rf "$tmp"
  ok "${name} 安装完成"
}

# 安装/更新本地 sbx 脚本（curl 到稳定路径），保证 `sbx` 命令可用。
install_self() {
  mkdir -p /etc/sbx-pro
  curl -fLSs -m 60 -o /etc/sbx-pro/sbx.sh "${RAW_URL}" || die "下载 sbx.sh 失败"
  bash -n /etc/sbx-pro/sbx.sh || die "sbx.sh 语法校验失败"
  printf '#!/bin/sh\nexec bash /etc/sbx-pro/sbx.sh menu "$@"\n' > "$SELF_PATH"
  chmod 0755 "$SELF_PATH"
  ok "已安装 sbx 命令（运行 sbx 打开菜单）"
}

# ---------- Manager 安装 ----------
install_manager() {
  [[ ${EUID:-$(id -u)} -eq 0 ]] || die "需要 root 权限"
  command -v curl >/dev/null || die "缺少 curl"
  local already=false
  [[ -x "${BIN_DIR}/sbx-manager" ]] && already=true

  install -d -m 0700 /etc/sbx-pro
  echo manager > "$ROLE_MANAGER"
  download_bin sbx-manager
  install_self

  # 管理令牌：已有则保留（幂等），无则生成。
  local token
  token=$("${BIN_DIR}/sbx-manager" ensure-admin-token)

  # systemd 服务（幂等覆盖）。
  if command -v systemctl >/dev/null; then
    cat > /etc/systemd/system/sbx-manager.service <<EOF
[Unit]
Description=sbx-pro manager
After=network.target

[Service]
ExecStart=${BIN_DIR}/sbx-manager serve
Restart=on-failure
RestartSec=5
Environment=SBX_PRO_DIR=/etc/sbx-pro

[Install]
WantedBy=multi-user.target
EOF
    systemctl daemon-reload
    systemctl enable sbx-manager >/dev/null 2>&1
    systemctl restart sbx-manager || die "Manager 服务启动失败，请检查: journalctl -u sbx-manager"
    sleep 2
    systemctl is-active --quiet sbx-manager || die "Manager 服务未运行，请检查: journalctl -u sbx-manager"
  else
    pkill -f 'sbx-manager serve' 2>/dev/null || true
    nohup "${BIN_DIR}/sbx-manager" serve >/var/log/sbx-manager.log 2>&1 &
    sleep 2
  fi

  # 健康检查。
  local port=8080
  curl -fsS -m 5 "http://127.0.0.1:${port}/healthz" >/dev/null 2>&1 || warn "healthz 未通过（服务可能仍在启动）"

  echo ""
  if $already; then
    ok "Manager 已升级/修复（数据保留）"
  else
    ok "Manager 安装完成"
  fi
  echo "  面板地址: http://<本机IP>:${port}"
  echo "  管理令牌: ${token}"
  echo "  随时运行: sbx"
}

# ---------- Agent 安装 ----------
install_agent() {
  [[ ${EUID:-$(id -u)} -eq 0 ]] || die "需要 root 权限"
  command -v curl >/dev/null || die "缺少 curl"
  local TOKEN="" MANAGER_URL=""
  while [[ $# -gt 0 ]]; do
    case "$1" in
      -t|--token) TOKEN="$2"; shift 2 ;;
      -u|--url)   MANAGER_URL="$2"; shift 2 ;;
      *) die "未知参数: $1" ;;
    esac
  done
  [[ -n "$TOKEN" ]] || die "缺少 enrollment token（-t）"
  [[ -n "$MANAGER_URL" ]] || die "缺少 Manager URL（-u）"
  command -v systemctl >/dev/null || die "缺少 systemd（SBX Pro Agent 当前仅支持 systemd）"

  # 已注册则提示，不盲目重新 enrollment。
  if [[ -f /etc/sbx-agent/agent.json ]]; then
    local mid
    mid=$(grep -o '"machine_id":"[^"]*"' /etc/sbx-agent/agent.json | head -1 | cut -d'"' -f4 || true)
    [[ -n "$mid" ]] && warn "该机器已接入（machine_id=${mid}）。如需更换 Manager 请在 WebUI 删除机器后重新注册。"
  fi

  # sing-box
  if ! [[ -x "$BIN_DIR/sing-box" ]] || ! "$BIN_DIR/sing-box" version >/dev/null 2>&1; then
    install_sing_box
  else
    ok "sing-box 已安装"
  fi

  install -d -m 0700 /etc/sbx-agent /etc/sbx
  echo agent > "$ROLE_AGENT"
  download_bin sbx-agent
  install_self

  # 初始 sing-box 配置
  if [[ ! -f /etc/sing-box/config.json ]]; then
    install -d -m 0755 /etc/sing-box
    printf '%s\n' '{"inbounds":[],"outbounds":[{"type":"direct","tag":"direct"}],"route":{"final":"direct"}}' > /etc/sing-box/config.json
  fi

  cat > /etc/systemd/system/sing-box.service <<'EOF'
[Unit]
Description=sing-box
After=network.target

[Service]
ExecStart=/usr/local/bin/sing-box run -c /etc/sing-box/config.json
Restart=on-failure
RestartSec=3

[Install]
WantedBy=multi-user.target
EOF
  cat > /etc/systemd/system/sbx-agent.service <<EOF
[Unit]
Description=sbx-pro agent
After=network.target sing-box.service

[Service]
ExecStart=${BIN_DIR}/sbx-agent run
Restart=on-failure
RestartSec=5
Environment=SBX_AGENT_DIR=/etc/sbx-agent
Environment=SBX_DIR=/etc/sbx

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload
  systemctl enable sing-box >/dev/null 2>&1
  systemctl restart sing-box || die "sing-box 启动失败: journalctl -u sing-box"

  # 注册（幂等：已有身份则跳过）。
  if [[ -f /etc/sbx-agent/agent.json ]] && grep -q '"machine_id"' /etc/sbx-agent/agent.json; then
    ok "已接入，跳过注册"
  else
    info "注册到管理面板..."
    "${BIN_DIR}/sbx-agent" enroll -t "$TOKEN" -u "$MANAGER_URL" || die "注册失败"
  fi

  systemctl enable sbx-agent >/dev/null 2>&1
  systemctl restart sbx-agent || die "sbx-agent 启动失败: journalctl -u sbx-agent"
  sleep 2
  systemctl is-active --quiet sbx-agent || die "sbx-agent 未运行: journalctl -u sbx-agent"

  echo ""
  ok "Agent 安装完成，机器已接入管理面板"
  echo "  Manager: ${MANAGER_URL}"
  [[ -n "${mid:-}" ]] && echo "  machine_id: ${mid}"
  echo "  Agent: 运行中"
  echo "  sing-box: 运行中"
  echo "  随时运行: sbx"
}

install_sing_box() {
  local version="1.14.0-rc.1"
  local name="sing-box-${version}-linux-${ARCH}.tar.gz"
  local tmp; tmp=$(mktemp -d)
  info "下载 sing-box v${version} (${ARCH})"
  curl -fLSs -m 300 -o "$tmp/sb.tar.gz" "https://github.com/SagerNet/sing-box/releases/download/v${version}/${name}" \
    || { rm -rf "$tmp"; die "下载 sing-box 失败（可设 SBX_GH_PROXY 镜像）"; }
  case "$ARCH" in
    amd64) echo "342f6e3b4ab79abe470d1516b35dced9bc8dfe62dc43a459a53d97960108afeb  $tmp/sb.tar.gz" | sha256sum -c - >/dev/null || { rm -rf "$tmp"; die "sing-box 校验失败"; } ;;
    arm64) echo "98a5bd1f7bf5063f908461eb47ccb68d6df08571c62051f467c395a270a5e3c9  $tmp/sb.tar.gz" | sha256sum -c - >/dev/null || { rm -rf "$tmp"; die "sing-box 校验失败"; } ;;
    *) die "当前架构 ${ARCH} 未内置 sing-box 校验哈希，拒绝安装（安全 fail-closed）" ;;
  esac
  tar xzf "$tmp/sb.tar.gz" -C "$tmp"
  local found; found=$(find "$tmp" -type f -name sing-box | head -1)
  [[ -n "$found" ]] || { rm -rf "$tmp"; die "未找到 sing-box"; }
  install -m 0755 "$found" "$BIN_DIR/sing-box"
  rm -rf "$tmp"
  ok "sing-box 安装完成"
}

# ---------- 检查更新 ----------
do_update() {
  local force="${1:-}"
  local role; role=$(detect_role)
  [[ -n "$role" ]] || die "未检测到安装角色，请先安装 Manager 或 Agent"

  info "检查更新（角色: ${role}）..."
  local bin="sbx-${role}"
  local file="${bin}-linux-${ARCH}"
  local tmp; tmp=$(mktemp -d)
  curl -fLSs -m 60 -o "$tmp/SHA256SUMS" "${RAW_BASE}/SHA256SUMS" || { rm -rf "$tmp"; die "下载 SHA256SUMS 失败"; }
  local remote_hash; remote_hash=$(grep -E "${file}$" "$tmp/SHA256SUMS" | awk '{print $1}' | head -1)
  local local_hash; local_hash=$(sha256sum "${BIN_DIR}/${bin}" 2>/dev/null | awk '{print $1}')
  echo "  本地: ${local_hash:0:12}..."
  echo "  远端: ${remote_hash:0:12}..."

  if [[ "$force" != "--force" && -n "$remote_hash" && "$remote_hash" == "$local_hash" ]]; then
    rm -rf "$tmp"
    ok "已是最新版本"
    return 0
  fi

  # 备份当前二进制（失败 fail-closed）。
  [[ -x "${BIN_DIR}/${bin}" ]] && cp -a "${BIN_DIR}/${bin}" "${BIN_DIR}/${bin}.bak" || true

  if ! download_bin "$bin"; then
    [[ -f "${BIN_DIR}/${bin}.bak" ]] && mv -f "${BIN_DIR}/${bin}.bak" "${BIN_DIR}/${bin}"
    rm -rf "$tmp"
    die "升级失败，已回滚"
  fi
  install_self
  rm -rf "$tmp"

  # 重启服务 + 健康检查。
  if command -v systemctl >/dev/null; then
    local svc
    svc=$([[ "$role" == manager ]] && echo sbx-manager || echo sbx-agent)
    systemctl restart "$svc" || { [[ -f "${BIN_DIR}/${bin}.bak" ]] && mv -f "${BIN_DIR}/${bin}.bak" "${BIN_DIR}/${bin}"; systemctl restart "$svc" || true; die "重启失败，已回滚二进制"; }
    sleep 2
    systemctl is-active --quiet "$svc" || { [[ -f "${BIN_DIR}/${bin}.bak" ]] && mv -f "${BIN_DIR}/${bin}.bak" "${BIN_DIR}/${bin}"; systemctl restart "$svc" || true; die "服务未运行，已回滚"; }
  fi
  rm -f "${BIN_DIR}/${bin}.bak"
  ok "升级完成（数据保留：机器/节点/流量/令牌均不受影响）"
}

# ---------- 卸载 ----------
uninstall() {
  [[ ${EUID:-$(id -u)} -eq 0 ]] || die "需要 root 权限"
  local role; role=$(detect_role)
  [[ -n "$role" ]] || { warn "未检测到安装角色，执行通用清理"; role=unknown; }

  echo "即将卸载 SBX Pro（角色: ${role}）"
  echo "  1) 卸载程序，但保留数据"
  echo "  2) 完全卸载并删除数据"
  echo "  0) 取消"
  read -rp "请选择 [0/1/2]: " choice || choice=0
  case "$choice" in
    1) remove_program "$role" ;;
    2) remove_all "$role" ;;
    *) echo "已取消"; exit 0 ;;
  esac
}

remove_program() {
  local role="$1"
  if command -v systemctl >/dev/null; then
    if [[ "$role" == manager ]]; then
      systemctl stop sbx-manager 2>/dev/null || true
      systemctl disable sbx-manager 2>/dev/null || true
      rm -f /etc/systemd/system/sbx-manager.service
    elif [[ "$role" == agent ]]; then
      systemctl stop sbx-agent 2>/dev/null || true
      systemctl disable sbx-agent 2>/dev/null || true
      rm -f /etc/systemd/system/sbx-agent.service
    fi
    systemctl daemon-reload
  fi
  pkill -f 'sbx-manager serve' 2>/dev/null || true
  pkill -f 'sbx-agent run' 2>/dev/null || true
  rm -f "$BIN_DIR/sbx-manager" "$BIN_DIR/sbx-agent" "$BIN_DIR/sbx" /etc/sbx-pro/sbx.sh
  ok "程序已卸载（数据保留）。若该机器接入过面板，请在 WebUI 删除机器记录。"
}

remove_all() {
  local role="$1"
  remove_program "$role"
  # 路径白名单校验，避免 rm -rf 灾难。
  local dir
  for dir in /etc/sbx-pro /etc/sbx-agent /etc/sbx /etc/sing-box; do
    case "$dir" in
      /etc/sbx-pro|/etc/sbx-agent|/etc/sbx|/etc/sing-box) rm -rf "$dir" ;;
      *) die "拒绝删除非白名单目录: $dir" ;;
    esac
  done
  rm -f "$BIN_DIR/sing-box"
  # 清理 nftables 规则。
  if command -v nft >/dev/null; then
    nft delete table inet sbx_traffic 2>/dev/null || true
    nft delete table inet sbx_quota   2>/dev/null || true
    nft delete table inet sbx_iplimit 2>/dev/null || true
  fi
  ok "已完全卸载"
}

# ---------- 菜单 ----------
menu_main() {
  local role; role=$(detect_role)
  local ver
  ver=$([[ -x "${BIN_DIR}/sbx-${role}" ]] && "${BIN_DIR}/sbx-${role}" version 2>/dev/null | head -1 || echo "未安装")
  clear 2>/dev/null || true
  echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
  echo " SBX Pro  ($role)"
  echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
  echo "  版本: ${ver}"
  echo "  服务: $([[ -n "$role" ]] && service_status "$role")"
  echo ""
  echo "  4) 系统设置"
  echo "  5) 检查更新"
  echo "  6) 卸载"
  echo "  0) 退出"
  echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
  read -rp "请选择: " c || c=0
  case "$c" in
    4) menu_settings ;;
    5) do_update ;;
    6) uninstall ;;
    0) echo "再见"; exit 0 ;;
    *) menu_main ;;
  esac
}

service_status() {
  local role="$1"
  local svc
  svc=$([[ "$role" == manager ]] && echo sbx-manager || echo sbx-agent)
  if systemctl is-active --quiet "$svc" 2>/dev/null; then echo "● 运行中"; else echo "○ 已停止"; fi
}

menu_settings() {
  local role; role=$(detect_role)
  clear 2>/dev/null || true
  echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
  echo " 系统设置 · ${role}"
  echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
  if [[ "$role" == manager ]]; then
    echo "  1) 面板设置"
    echo "  2) 服务管理"
    echo "  3) 管理令牌"
    echo "  4) 运行自检"
  else
    echo "  1) 连接状态"
    echo "  2) 服务管理"
    echo "  3) 流量统计自检"
    echo "  4) 运行自检"
  fi
  echo "  0) 返回"
  echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
  read -rp "请选择: " c || c=0
  case "$c" in
    1) [[ "$role" == manager ]] && settings_manager_panel || settings_agent_conn ;;
    2) menu_service ;;
    3) [[ "$role" == manager ]] && settings_manager_token || settings_agent_traffic ;;
    4) self_check ;;
    0) menu_main ;;
    *) menu_settings ;;
  esac
}

settings_manager_panel() {
  local port
  port=$(grep -o '"port":[0-9]*' /etc/sbx-pro/manager.json 2>/dev/null | head -1 | grep -oE '[0-9]+' || echo 8080)
  echo "当前监听端口: ${port}"
  echo "监听地址: $(grep -o '"listen":"[^"]*"' /etc/sbx-pro/manager.json 2>/dev/null | cut -d'"' -f4 || echo 0.0.0.0)"
  echo "面板 URL: http://<本机IP>:${port}"
  read -rp "修改端口？(留空跳过): " np || true
  if [[ -n "$np" ]]; then
    [[ "$np" =~ ^[0-9]+$ ]] && [[ "$np" -ge 1 && "$np" -le 65535 ]] || die "端口非法"
    # 用 sed 修改 manager.json（简单实现，candidate + 回滚）。
    cp /etc/sbx-pro/manager.json /etc/sbx-pro/manager.json.bak || die "备份失败"
    sed -i "s/\"port\":[0-9]*/\"port\":${np}/" /etc/sbx-pro/manager.json
    systemctl restart sbx-manager || { cp /etc/sbx-pro/manager.json.bak /etc/sbx-pro/manager.json; systemctl restart sbx-manager || true; die "端口修改后启动失败，已回滚"; }
    sleep 2
    systemctl is-active --quiet sbx-manager || { cp /etc/sbx-pro/manager.json.bak /etc/sbx-pro/manager.json; systemctl restart sbx-manager || true; die "端口修改后服务未运行，已回滚"; }
    rm -f /etc/sbx-pro/manager.json.bak
    ok "端口已改为 ${np}"
  fi
  read -rp "按回车继续..." _ || true
  menu_settings
}

settings_manager_token() {
  local token
  token=$("${BIN_DIR}/sbx-manager" ensure-admin-token)
  echo "管理令牌状态: 已配置"
  echo "  1) 显示当前管理令牌（敏感）"
  echo "  2) 重新生成管理令牌"
  echo "  0) 返回"
  read -rp "请选择: " c || c=0
  case "$c" in
    1) echo "管理令牌: ${token}" ;;
    2) echo "重新生成将导致现有登录失效。"; read -rp "确认？(y/N): " y || y=n; [[ "$y" == y ]] && { sed -i 's/"admin_token":"[^"]*"/"admin_token":""/' /etc/sbx-pro/manager.json; token=$("${BIN_DIR}/sbx-manager" ensure-admin-token); echo "新管理令牌: ${token}"; } ;;
    0) : ;;
  esac
  read -rp "按回车继续..." _ || true
  menu_settings
}

settings_agent_conn() {
  echo "=== 连接状态 ==="
  [[ -f /etc/sbx-agent/agent.json ]] && grep -E 'machine_id|manager_url' /etc/sbx-agent/agent.json | sed 's/^ *//' || echo "未注册"
  echo "Agent 版本: $("${BIN_DIR}/sbx-agent" version 2>/dev/null | head -1)"
  echo "sing-box 版本: $("${BIN_DIR}/sing-box" version 2>/dev/null | head -1)"
  echo "连接状态: $(service_status agent)"
  echo "节点数: $(grep -c '"id"' /etc/sbx/nodes.json 2>/dev/null || echo 0)"
  read -rp "按回车继续..." _ || true
  menu_settings
}

menu_service() {
  local role; role=$(detect_role)
  local svc; svc=$([[ "$role" == manager ]] && echo sbx-manager || echo sbx-agent)
  echo "=== 服务管理 (${svc}) ==="
  echo "状态: $(service_status "$role")"
  echo "  1) 重启  2) 停止  3) 启动  4) 查看日志  0) 返回"
  read -rp "请选择: " c || c=0
  case "$c" in
    1) systemctl restart "$svc" && ok "已重启" || warn "重启失败" ;;
    2) systemctl stop "$svc" && ok "已停止" || warn "停止失败" ;;
    3) systemctl start "$svc" && ok "已启动" || warn "启动失败" ;;
    4) journalctl -u "$svc" -n 50 --no-pager ;;
    0) menu_settings; return ;;
  esac
  read -rp "按回车继续..." _ || true
  menu_service
}

settings_agent_traffic() {
  echo "=== 流量统计自检 ==="
  command -v nft >/dev/null && nft list tables 2>/dev/null | grep -q sbx_traffic && echo "nft 计数表: ✓" || echo "nft 计数表: ✗"
  [[ -f /etc/sbx/traffic.db ]] && echo "traffic.db: ✓" || echo "traffic.db: ✗"
  [[ -f /etc/sbx/nft.conf ]] && echo "nft.conf: ✓" || echo "nft.conf: ✗"
  echo "  1) 重建流量计数规则（不清历史累计）  0) 返回"
  read -rp "请选择: " c || c=0
  [[ "$c" == 1 ]] && { systemctl restart sbx-agent; ok "已重启 agent（会重建计数规则并衔接累计）"; }
  read -rp "按回车继续..." _ || true
  menu_settings
}

self_check() {
  local role; role=$(detect_role)
  echo "=== 运行自检 (${role}) ==="
  [[ -x "${BIN_DIR}/sbx-${role}" ]] && echo "二进制: PASS" || echo "二进制: FAIL"
  service_status "$role" | grep -q 运行 && echo "服务: PASS" || echo "服务: WARN"
  if [[ "$role" == manager ]]; then
    curl -fsS -m 3 http://127.0.0.1:8080/healthz >/dev/null 2>&1 && echo "healthz: PASS" || echo "healthz: WARN"
    [[ -f /etc/sbx-pro/manager.db ]] && echo "SQLite: PASS" || echo "SQLite: FAIL"
  else
    "${BIN_DIR}/sing-box" check -c /etc/sing-box/config.json >/dev/null 2>&1 && echo "sing-box check: PASS" || echo "sing-box check: WARN"
    [[ -f /etc/sbx-agent/agent.db ]] && echo "本地 DB: PASS" || echo "本地 DB: WARN"
    command -v nft >/dev/null && echo "nftables: PASS" || echo "nftables: FAIL"
  fi
  read -rp "按回车继续..." _ || true
  menu_settings
}

# ---------- 入口 ----------
case "${1:-}" in
  manager) install_manager ;;
  agent) shift; install_agent "$@" ;;
  menu) menu_main ;;
  --update|-u|update) shift; do_update "${1:-}" ;;
  uninstall) uninstall ;;
  version|--version|-v)
    curl -fLSs "$RAW_BASE/SHA256SUMS" | head -5 ;;
  *)
    echo "SBX Pro 一键安装"
    echo "  装面板: bash <(curl -fLSs $RAW_URL) manager"
    echo "  装节点: bash <(curl -fLSs $RAW_URL) agent -t TOKEN -u https://panel.example.com"
    echo "  更新:   sbx --update [--force]"
    echo "  卸载:   bash <(curl -fLSs $RAW_URL) uninstall"
    exit 2 ;;
esac
