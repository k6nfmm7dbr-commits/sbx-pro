#!/bin/bash
# sbx.sh — SBX Pro 一键安装入口
#
# 装面板（A 机）:
#   bash <(curl -fLSs https://raw.githubusercontent.com/k6nfmm7dbr-commits/sbx-pro/main/sbx.sh) manager
#
# 装节点（B/C/D 机）:
#   bash <(curl -fLSs https://raw.githubusercontent.com/k6nfmm7dbr-commits/sbx-pro/main/sbx.sh) agent -t TOKEN -u https://panel.example.com
#
# 二进制从 dist 分支下载（rolling latest），sha256 校验后安装。

set -euo pipefail

RAW_BASE="${SBX_RAW_BASE:-https://raw.githubusercontent.com/k6nfmm7dbr-commits/sbx-pro/dist}"
RAW_URL="${SBX_RAW_URL:-https://raw.githubusercontent.com/k6nfmm7dbr-commits/sbx-pro/main/sbx.sh}"
BIN_DIR=/usr/local/bin

C_RESET='\033[0m'; C_GREEN='\033[32m'; C_YEL='\033[33m'; C_RED='\033[31m'; C_BLUE='\033[34m'
info() { printf '%s[*]%s %s\n' "$C_BLUE" "$C_RESET" "$*"; }
ok()   { printf '%s[+]%s %s\n' "$C_GREEN" "$C_RESET" "$*"; }
warn() { printf '%s[!]%s %s\n' "$C_YEL" "$C_RESET" "$*" >&2; }
die()  { printf '%s[x]%s %s\n' "$C_RED" "$C_RESET" "$*" >&2; exit 1; }

[[ ${EUID:-$(id -u)} -eq 0 ]] || die "需要 root 权限，请用 sudo 运行"
command -v curl >/dev/null || die "缺少 curl"

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

# 下载二进制 + SHA256SUMS + 校验 + 安装。
download_bin() { # download_bin <name>
  local name="$1"
  local file="${name}-linux-${ARCH}"
  info "下载 ${file}"
  curl -fLSs -m 300 -o "/tmp/${file}" "${RAW_BASE}/${file}" || die "下载 ${file} 失败（可设 SBX_RAW_BASE 镜像）"
  curl -fLSs -m 60 -o /tmp/SHA256SUMS "${RAW_BASE}/SHA256SUMS" || die "下载 SHA256SUMS 失败"
  (cd /tmp && grep -E "${file}$" SHA256SUMS | sha256sum -c -) || die "${name} 校验失败，已中止"
  install -m 0755 "/tmp/${file}" "${BIN_DIR}/${name}"
  ok "${name} 安装完成"
}

# ---------- manager ----------
install_manager() {
  install -d -m 0700 /etc/sbx-pro
  download_bin sbx-manager
  local token
  token=$("${BIN_DIR}/sbx-manager" ensure-admin-token)
  command -v systemctl >/dev/null && cat > /etc/systemd/system/sbx-manager.service <<EOF
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
  if command -v systemctl >/dev/null; then
    systemctl daemon-reload; systemctl enable --now sbx-manager >/dev/null 2>&1 || true
    sleep 2
  else
    nohup "${BIN_DIR}/sbx-manager" serve >/var/log/sbx-manager.log 2>&1 &
    sleep 2
  fi
  echo ""
  ok "Manager 安装完成"
  echo "  面板地址: http://<本机IP>:8080"
  echo "  管理令牌: $token"
}

# ---------- agent ----------
install_agent() {
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

  command -v systemctl >/dev/null || die "缺少 systemd"

  # sing-box
  if ! [[ -x "$BIN_DIR/sing-box" ]] || ! "$BIN_DIR/sing-box" version >/dev/null 2>&1; then
    install_sing_box
  else
    ok "sing-box 已安装"
  fi

  install -d -m 0700 /etc/sbx-agent /etc/sbx
  download_bin sbx-agent

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
  systemctl enable --now sing-box >/dev/null 2>&1 || true

  info "注册到管理面板..."
  "${BIN_DIR}/sbx-agent" enroll -t "$TOKEN" -u "$MANAGER_URL" || die "注册失败"

  systemctl enable --now sbx-agent >/dev/null 2>&1
  sleep 2
  if systemctl is-active --quiet sbx-agent; then
    ok "机器已成功接入管理面板"
  else
    warn "sbx-agent 未运行，请检查: journalctl -u sbx-agent"
  fi
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
    *) warn "当前架构未内置 sing-box 校验哈希，跳过校验" ;;
  esac
  tar xzf "$tmp/sb.tar.gz" -C "$tmp"
  local found; found=$(find "$tmp" -type f -name sing-box | head -1)
  [[ -n "$found" ]] || { rm -rf "$tmp"; die "未找到 sing-box"; }
  install -m 0755 "$found" "$BIN_DIR/sing-box"
  rm -rf "$tmp"
  ok "sing-box 安装完成"
}

# ---------- 入口 ----------
case "${1:-}" in
  manager) install_manager ;;
  agent) shift; install_agent "$@" ;;
  version|--version|-v)
    curl -fLSs "$RAW_BASE/SHA256SUMS" | head -5 ;;
  *) 
    echo "SBX Pro 一键安装"
    echo "  装面板: bash <(curl -fLSs $RAW_URL) manager"
    echo "  装节点: bash <(curl -fLSs $RAW_URL) agent -t TOKEN -u https://panel.example.com"
    exit 2 ;;
esac
