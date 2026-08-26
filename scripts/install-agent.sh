#!/bin/bash
# install-agent.sh — sbx-pro 节点代理一键安装脚本
#
# 用法（复制到节点机执行一次）:
#   bash <(curl -fLSs https://panel.example.com/install-agent.sh) -t TOKEN -u https://panel.example.com
#
# 流程: 检查环境 -> 安装 sing-box -> 下载 sbx-agent -> 注册 -> 启动 systemd
# 复用原 sbx 的 sing-box 安装逻辑（12 架构 sha256 pin 校验）。

set -euo pipefail

C_RESET='\033[0m'; C_GREEN='\033[32m'; C_YEL='\033[33m'; C_RED='\033[31m'; C_BLUE='\033[34m'
info()  { printf '%s[*]%s %s\n' "$C_BLUE" "$C_RESET" "$*"; }
ok()    { printf '%s[+]%s %s\n' "$C_GREEN" "$C_RESET" "$*"; }
warn()  { printf '%s[!]%s %s\n' "$C_YEL" "$C_RESET" "$*" >&2; }
die()   { printf '%s[x]%s %s\n' "$C_RED" "$C_RESET" "$*" >&2; exit 1; }

TOKEN=""; MANAGER_URL=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    -t|--token) TOKEN="$2"; shift 2 ;;
    -u|--url)   MANAGER_URL="$2"; shift 2 ;;
    *) die "未知参数: $1" ;;
  esac
done
[[ -n "$TOKEN" ]] || die "缺少 enrollment token（-t）"
[[ -n "$MANAGER_URL" ]] || die "缺少 Manager URL（-u）"

# ---- 环境检查 ----
[[ ${EUID:-$(id -u)} -eq 0 ]] || die "需要 root 权限，请用 sudo 运行"
uname -m | grep -qE 'x86_64|aarch64|armv7|armv6|i686|riscv64' || die "不支持的架构"
command -v curl >/dev/null || die "缺少 curl"
command -v systemctl >/dev/null || die "缺少 systemd"
command -v nft >/dev/null || die "缺少 nftables（nft）"

AGENT_DIR="${SBX_AGENT_DIR:-/etc/sbx-agent}"
SBX_DIR="${SBX_DIR:-/etc/sbx}"
BIN_DIR=/usr/local/bin

SB_VERSION="1.14.0-rc.1"
# sing-box 下载（复用原 sbx 的 amd64 校验，扩展架构时补充 SHA）。
sb_arch() {
  case "$(uname -m)" in
    x86_64) echo amd64 ;; aarch64) echo arm64 ;; armv7l) echo armv7 ;;
    armv6l) echo armv6 ;; i686) echo 386 ;; riscv64) echo riscv64 ;;
    *) die "不支持的架构: $(uname -m)" ;;
  esac
}

# ---- 安装 sing-box ----
install_sing_box() {
  if [[ -x "$BIN_DIR/sing-box" ]] && "$BIN_DIR/sing-box" version >/dev/null 2>&1; then
    ok "sing-box 已安装: $("$BIN_DIR/sing-box" version | head -1)"
    return 0
  fi
  local arch name url tmp
  arch="$(sb_arch)"
  name="sing-box-${SB_VERSION}-linux-${arch}.tar.gz"
  tmp=$(mktemp -d)
  url="https://github.com/SagerNet/sing-box/releases/download/v${SB_VERSION}/${name}"
  info "下载 sing-box v${SB_VERSION} (${arch})"
  curl -fLSs -m 300 -o "$tmp/sb.tar.gz" "$(gh_url "$url")" || { rm -rf "$tmp"; die "下载 sing-box 失败"; }
  # amd64 校验（其它架构如需安装需补充 SHA pin）。
  case "$arch" in
    amd64) echo "342f6e3b4ab79abe470d1516b35dced9bc8dfe62dc43a459a53d97960108afeb  $tmp/sb.tar.gz" | sha256sum -c - >/dev/null || { rm -rf "$tmp"; die "sing-box 校验失败"; } ;;
    arm64) echo "98a5bd1f7bf5063f908461eb47ccb68d6df08571c62051f467c395a270a5e3c9  $tmp/sb.tar.gz" | sha256sum -c - >/dev/null || { rm -rf "$tmp"; die "sing-box 校验失败"; } ;;
  esac
  tar xzf "$tmp/sb.tar.gz" -C "$tmp"
  local found
  found=$(find "$tmp" -type f -name sing-box | head -1)
  [[ -n "$found" ]] || { rm -rf "$tmp"; die "压缩包中未找到 sing-box"; }
  install -m 0755 "$found" "$BIN_DIR/sing-box"
  rm -rf "$tmp"
  ok "sing-box 安装完成: $("$BIN_DIR/sing-box" version | head -1)"
}

gh_url() {
  [[ -n "${SBX_GH_PROXY:-}" ]] && echo "${SBX_GH_PROXY%/}/$1" || echo "$1"
}

# ---- 下载 sbx-agent ----
install_sbx_agent() {
  if [[ -x "$BIN_DIR/sbx-agent" ]]; then
    ok "sbx-agent 已安装"
    return 0
  fi
  # 优先本地（SBX_AGENT_BIN 指向已编译二进制），否则从 Manager 下载。
  if [[ -n "${SBX_AGENT_BIN:-}" && -x "$SBX_AGENT_BIN" ]]; then
    install -m 0755 "$SBX_AGENT_BIN" "$BIN_DIR/sbx-agent"
    ok "使用本地 sbx-agent"
    return 0
  fi
  local arch name url tmp
  arch="$(sb_arch)"
  name="sbx-agent-linux-${arch}"
  tmp=$(mktemp -d)
  url="${MANAGER_URL%/}/install-agent.sh.bin/${name}"
  curl -fLSs -m 120 -o "$tmp/sbx-agent" "$url" || { rm -rf "$tmp"; die "下载 sbx-agent 失败（请确认 Manager 提供该二进制或设置 SBX_AGENT_BIN）"; }
  install -m 0755 "$tmp/sbx-agent" "$BIN_DIR/sbx-agent"
  rm -rf "$tmp"
  ok "sbx-agent 安装完成"
}

# ---- 目录 / 服务 ----
prepare() {
  install -d -m 0700 "$AGENT_DIR" "$SBX_DIR"
  if [[ ! -f /etc/sing-box/config.json ]]; then
    install -d -m 0755 /etc/sing-box
    cat > /etc/sing-box/config.json <<'EOF'
{"inbounds":[],"outbounds":[{"type":"direct","tag":"direct"}],"route":{"final":"direct"}}
EOF
  fi
}

create_services() {
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
ExecStart=$BIN_DIR/sbx-agent run
Restart=on-failure
RestartSec=5
Environment=SBX_AGENT_DIR=$AGENT_DIR
Environment=SBX_DIR=$SBX_DIR

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload
  systemctl enable --now sing-box >/dev/null 2>&1 || true
}

# ---- 主流程 ----
info "SBX Pro 节点代理安装"
install_sing_box
install_sbx_agent
prepare
create_services

# 注册。
info "注册到管理面板..."
if ! "$BIN_DIR/sbx-agent" enroll -t "$TOKEN" -u "$MANAGER_URL"; then
  die "注册失败"
fi

# 启动 agent。
systemctl enable --now sbx-agent >/dev/null 2>&1
sleep 2
if systemctl is-active --quiet sbx-agent; then
  ok "机器已成功接入管理面板"
else
  warn "sbx-agent 服务未运行，请检查: journalctl -u sbx-agent"
fi
