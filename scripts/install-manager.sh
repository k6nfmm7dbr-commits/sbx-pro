#!/bin/bash
# install-manager.sh — sbx-pro 中央管理面板安装脚本
#
# 用法: bash install-manager.sh
#
# 流程: 检查环境 -> 下载 sbx-manager -> 生成配置 -> 启动 systemd

set -euo pipefail

C_RESET='\033[0m'; C_GREEN='\033[32m'; C_YEL='\033[33m'; C_RED='\033[31m'; C_BLUE='\033[34m'
info()  { printf '%s[*]%s %s\n' "$C_BLUE" "$C_RESET" "$*"; }
ok()    { printf '%s[+]%s %s\n' "$C_GREEN" "$C_RESET" "$*"; }
warn()  { printf '%s[!]%s %s\n' "$C_YEL" "$C_RESET" "$*" >&2; }
die()   { printf '%s[x]%s %s\n' "$C_RED" "$C_RESET" "$*" >&2; exit 1; }

[[ ${EUID:-$(id -u)} -eq 0 ]] || die "需要 root 权限"
command -v curl >/dev/null || die "缺少 curl"
command -v systemctl >/dev/null || die "缺少 systemd"

PRO_DIR="${SBX_PRO_DIR:-/etc/sbx-pro}"
BIN_DIR=/usr/local/bin

sb_arch() {
  case "$(uname -m)" in
    x86_64) echo amd64 ;; aarch64) echo arm64 ;; *) die "不支持的架构: $(uname -m)" ;;
  esac
}

install_sbx_manager() {
  if [[ -x "$BIN_DIR/sbx-manager" ]]; then
    ok "sbx-manager 已安装"
    return 0
  fi
  if [[ -n "${SBX_MANAGER_BIN:-}" && -x "$SBX_MANAGER_BIN" ]]; then
    install -m 0755 "$SBX_MANAGER_BIN" "$BIN_DIR/sbx-manager"
    ok "使用本地 sbx-manager"
    return 0
  fi
  die "请提供 sbx-manager 二进制（SBX_MANAGER_BIN）或预置到 $BIN_DIR"
}

install_sbx_manager
install -d -m 0700 "$PRO_DIR"

# 生成 admin token（幂等）。
ADMIN_TOKEN=$("$BIN_DIR/sbx-manager" ensure-admin-token)

cat > /etc/systemd/system/sbx-manager.service <<EOF
[Unit]
Description=sbx-pro manager
After=network.target

[Service]
ExecStart=$BIN_DIR/sbx-manager serve
Restart=on-failure
RestartSec=5
Environment=SBX_PRO_DIR=$PRO_DIR

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable --now sbx-manager >/dev/null 2>&1
sleep 2

if systemctl is-active --quiet sbx-manager; then
  ok "Manager 已启动"
  echo ""
  echo "  管理面板: http://<本机IP>:8080"
  echo "  管理令牌: $ADMIN_TOKEN"
  echo "  （请登录后修改端口/令牌，并通过面板生成接入 token）"
else
  warn "Manager 未运行，请检查: journalctl -u sbx-manager"
fi
