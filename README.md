# SBX Pro — 多机集中管理版

SBX Pro 是 [sbx](https://github.com/k6nfmm7dbr-commits/sbx) 的多机进阶版：**一块面板统一管理多台服务器及其上的 sing-box 节点**。

> 「这是 SBX，只是现在一块面板可以可靠地管理很多台机器。」

## 核心定位

| | 原 SBX（单机版） | SBX Pro（多机版） |
|---|---|---|
| 管理方式 | 一台机器，一个面板 | 一个面板，多台机器 |
| 面板机（A） | 直接转发代理流量 | 只做控制面，**不代理任何用户流量** |
| 节点机（B/C/D） | — | sing-box + 本地自治 Agent |

用户流量始终直达节点机 B/C/D。A 机只负责：机器管理、全局节点管理、任务下发、流量汇总、在线 IP 管理。**Manager 挂掉不影响节点服务**——Agent 本机自治运行（流量统计、Quota、IP Limit 都在 Agent 本地执行）。

## 架构

```
                    ┌─────────────────────────────┐
                    │         A 管理面板           │
                    │   sbx-manager + WebUI       │
                    │   中央 SQLite / 任务系统      │
                    └──────────────┬──────────────┘
                     wss:// (Agent 主动长连接, challenge-response 认证)
          ┌────────────────────────┼────────────────────────┐
          │                        │                        │
   ┌──────▼──────┐          ┌──────▼──────┐          ┌──────▼──────┐
   │  B 节点机    │          │  C 节点机    │          │  D 节点机    │
   │  sbx-agent  │          │  sbx-agent  │          │  sbx-agent  │
   │  sing-box   │          │  sing-box   │          │  sing-box   │
   │  nftables   │          │  nftables   │          │  nftables   │
   └─────────────┘          └─────────────┘          └─────────────┘
     ↑ 用户流量直达（不经 A）
```

**关键原则**：

1. A 只做控制面，不代理流量；
2. B/C/D 主动连接 A（不依赖 SSH、不开放 Agent 端口）；
3. Manager 挂掉不影响节点服务（Agent 本机自治）；
4. 流量限制 / IP 限制由 Agent 本地执行；
5. 节点配置权威 = Manager desired state，Agent 为 actual state 执行者；
6. Agent 不接受任意 shell command，任务走 task_id + 幂等白名单；
7. 配置更新必须 candidate → sing-box check → 原子替换 → health check，失败绝不谎报成功。

## 支持协议

- VLESS Reality
- Shadowsocks 2022（128 / 256）
- Trojan
- AnyTLS
- Snell v5 / v6

协议能力 schema 由 Manager 的 `GET /api/capabilities` 提供（前端表单动态渲染，不硬编码）。

## 安全模型

### 机器身份（Ed25519 challenge-response）

- Agent **本地生成** Ed25519 keypair；私钥只落盘 Agent（`0600`），**永不上传**；
- 注册时 Agent 只把公钥发给 Manager，Manager 只存公钥；
- WebSocket 认证：Manager 发一次性 challenge（30s 过期），Agent 用私钥签名，Manager 用公钥验签，防 replay；
- 签名绑定 `machine_id` + challenge；Origin 校验拒绝第三方浏览器劫持。

### 管理员认证

- 管理令牌恒定时间比较（`crypto/subtle`）；
- WebUI 登录用 HttpOnly Cookie（SameSite=Strict）；API 支持 Bearer；
- 审计日志记录关键操作（注册/节点增删改/分享/额度等），日志不记 token/私钥。

### 节点凭据

- Manager 权威生成节点凭据（uuid / password / psk / X25519 reality keypair），仅 share 接口按需返回；
- 节点列表接口永远脱敏（不返回敏感 config）。

## 流量统计口径

**累计流量来自内核 byte counter，绝不靠实时速率积分。**

- 采集：`nftables` counter → `Collector` 差分 → 本地 SQLite `totals`（绝对累计）；
- 口径：`rx` = 服务器收到 = 用户上传；`tx` = 服务器发出 = 用户下载（与 sbx 一致）；
- epoch 机制：nft 规则重建 / counter reset 时自动衔接累计，不丢不翻倍；
- 同步：Agent 把 totals 的增量写入本地 `traffic_pending`（全局递增 sequence，持久化），发 `traffic_delta`；
- 幂等：Manager 用 `UNIQUE(machine_id, sequence)` 去重；入库后回 `traffic_ack`，Agent 收到才清理 pending；
- 断线 / 崩溃 / ACK 丢失 → pending 保留，重连后重发，幂等去重保证「最多重放、不翻倍」；
- 单位：内部统一 bytes，UI 显示 KiB/MiB/GiB/TiB（1024 进制）。

## Quota / 在线 IP 限制

- **Quota**：Agent 本地累计值判断，`used >= limit` 时用 `nftables` set 阻断该节点端口；提高额度/重置即解除。**Manager 离线时照样执行**。
- **在线 IP 限制**：按 `node/local_port + remote_ip + protocol` 统计（`/proc/net/{tcp,udp}`），超过 N 个公网源 IP 时新 IP 进 `nftables` set 阻断；老 IP 离线后新 IP 补位。
- 两者都在 Agent 本地持久化，重启后恢复 enforcement。

## 节点状态机

```
provisioning ──success──▶ active ──update──▶ update_pending ──success──▶ active
     │ failure                 │ disable/enable                  │ failure
     ▼                         ▼                                 ▼
create_failed              disabled ◀──────────────▶ active    config_error
active ──delete──▶ delete_pending ──success──▶ (记录删除)
```

- 创建失败**不残留永久脏节点**（标记 `create_failed`，可重试/删除）；
- 删除任务在 Agent 真正删除成功前，Manager 不提前永久删除记录；
- `config_revision`（desired）与 `applied_revision` 由心跳/任务回传持续收敛。

## 安装

### Manager（面板机 A，一行命令）

```bash
bash <(curl -fLSs https://raw.githubusercontent.com/k6nfmm7dbr-commits/sbx-pro/main/sbx.sh) manager
```

安装完成后运行：

```bash
sbx
```

打开管理菜单。安装幂等：重复执行即「修复/升级」，保留 DB / 令牌 / 机器 / 节点 / 流量数据。

### Agent（节点机 B/C/D，从 WebUI 生成命令）

在 WebUI「接入」页面生成 enrollment token 后，复制命令到节点机执行：

```bash
bash <(curl -fLSs https://raw.githubusercontent.com/k6nfmm7dbr-commits/sbx-pro/main/sbx.sh) agent -t TOKEN -u https://panel.example.com
```

脚本自动：装 sing-box（sha256 校验）→ 装 sbx-agent → 本地生成身份 → 注册 → 启动。同样安装后运行 `sbx` 打开菜单。

## sbx 菜单

安装后执行 `sbx`，自动识别 Manager/Agent 角色，进入同风格菜单：

```
━━━━━━━━━━━━━━━━━━━━━━━━━━━━
 SBX Pro  (manager / agent)
━━━━━━━━━━━━━━━━━━━━━━━━━━━━
  4) 系统设置
  5) 检查更新
  6) 卸载
  0) 退出
━━━━━━━━━━━━━━━━━━━━━━━━━━━━
```

编号沿用原 SBX 的 4/5/6/0。系统设置角色化：Manager 有面板设置 / 服务管理 / 管理令牌 / 运行自检；Agent 有连接状态 / 服务管理 / 流量统计自检 / 运行自检。

更新也支持命令行：`sbx --update`（或 `--update --force`），与菜单共用同一套更新逻辑（sha256 内容比较 + 备份 + 回滚 + 健康检查）。

## 卸载

`sbx` → 6) 卸载（或 `bash <(curl ...) uninstall`），按角色提供：

- 卸载程序但保留数据；
- 完全卸载并删除数据（路径白名单 + 二次确认）。

卸载 Manager 不会远程卸载任何 Agent，远端节点继续自治（只进入 Manager unavailable）。

## 数据目录

| 目录 | 用途 |
|---|---|
| `/etc/sbx-pro/` | Manager 数据（manager.db / manager.json / role / sbx.sh） |
| `/etc/sbx-agent/` | Agent 身份（agent.json 0600 / agent.db / role） |
| `/etc/sbx/` | 节点数据（nodes.json / traffic.db / nft.conf / certs/） |
| `/etc/sing-box/` | sing-box 配置 |

## API 摘要

管理员 API（Bearer token 或 `sbx_token` Cookie）：

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/healthz` | 健康检查 |
| POST | `/api/enrollment/token` | 生成接入 token（一次性/15min） |
| GET | `/api/machines` | 机器列表 |
| DELETE | `/api/machines/:id` | 删除机器管理关系 |
| GET | `/api/capabilities` | 协议能力 schema |
| GET/POST | `/api/nodes` | 节点列表 / 创建 |
| GET/PUT/DELETE | `/api/nodes/:id` | 详情 / 更新 / 删除 |
| GET | `/api/nodes/:id/share` | 分享链接（完整 URI） |
| POST | `/api/nodes/:id/enable` `/disable` `/restart` | 启停/重启 |
| POST | `/api/nodes/:id/quota` `/quota/reset` `/ip-limit` | 额度/IP限制 |
| POST/GET | `/api/tasks` | 下发 / 查询任务 |
| GET | `/api/traffic` | 流量汇总 |
| GET | `/api/audit` | 审计日志 |

Agent 接入：`POST /api/agent/register`（注册）、`WS /api/agent/ws`（长连接）。

## Agent 协议

统一 envelope：`{"version":1,"type":"...","id":"...","timestamp":0,"payload":{}}`

Agent → Manager：`hello` `heartbeat` `task_result` `traffic_delta` `traffic_snapshot` `ip_sync` `sync_state` 等

Manager → Agent（走任务系统，幂等）：`create_node` `update_node` `delete_node` `enable_node` `disable_node` `restart_singbox` `set_quota` `reset_quota` `set_ip_limit` `sync_config` `request_status` `update_agent` 等

## 故障行为

- **Manager 挂掉**：节点继续工作，流量/Quota/IP Limit 本地自治，累计继续；恢复后重连补传（不翻倍）。
- **Agent 挂掉**：断线重连（指数退避 1~60s），恢复后补传 pending。
- **任务超时**：sweeper 标记 `timeout`，节点状态收敛到 `create_failed`/`config_error`，可重试/删除。
- **配置更新失败**：candidate → check → 备份 → 原子替换 → restart → health check，任一失败回滚旧配置。

## 构建与测试

```bash
go build ./...      # 编译
go vet ./...        # 静态检查
go test ./...       # 单元测试
go test -race ./... # 竞态检测（环境支持时）
bash -n sbx.sh      # 安装脚本语法检查
```

> iOS iSH 环境无法运行 Go 工具链（CPU 模拟限制），构建/测试请在真实 Linux 机器上进行。

## 从 sbx 迁移

- 原 sbx 单机节点可平滑接入：在节点机装 sbx-agent 接入面板即可（Agent 复用原 sbx 的 nodes/traffic/firewall/connection 模块）。
- 建议先在新节点机装 sbx-agent 接入验证，再迁移存量节点。

## 已知的非阻塞 Future Improvements

1. 机器公网 IP 自动探测未实现（`share` 链接目前回退 hostname，可在「机器」页手动维护 public_host）；
2. `update_agent`（Agent 自升级）为安全预留，本轮未启用（当前走 `sbx --update` 手动升级）；
3. Manager 生产部署建议在反代（Nginx/Caddy）后启用 HTTPS。

---

## 技术栈

- Go 1.23+，单二进制优先
- SQLite（modernc.org/sqlite，纯 Go，无 cgo）
- sing-box（1.14.0-rc.1，sha256 pin）
- nftables / netfilter
- WebSocket（gorilla/websocket）
