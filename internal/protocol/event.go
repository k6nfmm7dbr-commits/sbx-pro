package protocol

// 消息类型白名单。严格区分方向，禁止 Agent 接收任意 shell command。
//
// agent -> manager（Agent 主动上报）
const (
	MsgHello          = "hello"            // 首次连接 / 重新认证握手
	MsgHeartbeat      = "heartbeat"        // 周期心跳（10~30s）
	MsgTaskResult     = "task_result"      // 任务执行结果回传
	MsgNodeStatus     = "node_status"      // 节点状态上报
	MsgTrafficDelta   = "traffic_delta"    // 流量增量
	MsgTrafficSnapshot = "traffic_snapshot" // 流量完整快照（重连补传）
	MsgIPSync         = "ip_sync"          // 在线 IP 快照 / 增量
	MsgLogEvent       = "log_event"        // 日志事件（脱敏）
	MsgSyncState      = "sync_state"       // config_revision 同步状态
)

// manager -> agent（Manager 下发任务，全部走任务系统）
const (
	MsgCreateNode    = "create_node"
	MsgUpdateNode    = "update_node"
	MsgDeleteNode    = "delete_node"
	MsgEnableNode    = "enable_node"
	MsgDisableNode   = "disable_node"
	MsgRestartSingbox = "restart_singbox"
	MsgSetQuota      = "set_quota"
	MsgResetQuota    = "reset_quota"
	MsgSetIPLimit    = "set_ip_limit"
	MsgSyncConfig    = "sync_config"
	MsgRequestStatus = "request_status"
	MsgUpdateAgent   = "update_agent"
)

// 除上述之外，Manager 与 Agent 的握手还使用以下应答消息：
const (
	MsgHelloAck = "hello_ack" // manager -> agent：注册/认证结果
	MsgError    = "error"     // 双向：处理失败
)

// IsKnownType 报告 type 是否为已知消息类型（含应答）。
func IsKnownType(t string) bool {
	switch t {
	case MsgHello, MsgHeartbeat, MsgTaskResult, MsgNodeStatus,
		MsgTrafficDelta, MsgTrafficSnapshot, MsgIPSync, MsgLogEvent, MsgSyncState,
		MsgCreateNode, MsgUpdateNode, MsgDeleteNode, MsgEnableNode, MsgDisableNode,
		MsgRestartSingbox, MsgSetQuota, MsgResetQuota, MsgSetIPLimit, MsgSyncConfig,
		MsgRequestStatus, MsgUpdateAgent,
		MsgHelloAck, MsgError:
		return true
	}
	return false
}

// IsTaskType 报告 type 是否为「需要走任务系统（幂等 + 超时）的指令型消息」。
// 这类消息由 Manager 创建 task 后下发，Agent 执行并回传 task_result。
func IsTaskType(t string) bool {
	switch t {
	case MsgCreateNode, MsgUpdateNode, MsgDeleteNode, MsgEnableNode, MsgDisableNode,
		MsgRestartSingbox, MsgSetQuota, MsgResetQuota, MsgSetIPLimit, MsgSyncConfig,
		MsgUpdateAgent:
		return true
	}
	return false
}
