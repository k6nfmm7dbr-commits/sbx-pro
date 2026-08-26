package protocol

import "encoding/json"

// 任务系统数据模型（见开发提示词第十一节 / 五十六节）。
//
// 状态机：pending → sent → running → success | failed | timeout
//
// Agent 必须实现幂等：同一 task_id 只执行一次，重复下发返回此前结果。

// TaskStatus 是任务生命周期状态。
type TaskStatus string

const (
	TaskPending   TaskStatus = "pending"
	TaskSent      TaskStatus = "sent"
	TaskRunning   TaskStatus = "running"
	TaskSuccess   TaskStatus = "success"
	TaskFailed    TaskStatus = "failed"
	TaskTimeout   TaskStatus = "timeout"
)

// Task 是 Manager 下发的一条指令。Type 必须是 IsTaskType 白名单成员。
type Task struct {
	ID        string          `json:"task_id"`
	MachineID string          `json:"machine_id"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload,omitempty"`
	CreatedAt int64           `json:"created_at"`
}

// TaskResult 是 Agent 执行后的回传。
type TaskResult struct {
	TaskID    string     `json:"task_id"`
	Status    TaskStatus `json:"status"` // success | failed
	Message   string     `json:"message,omitempty"`
	AppliedRevision int64 `json:"applied_revision,omitempty"`
	CompletedAt  int64    `json:"completed_at"`
}

// Heartbeat 是 Agent 周期上报的心跳（第八节）。
type Heartbeat struct {
	MachineID      string `json:"machine_id"`
	Hostname       string `json:"hostname"`
	AgentVersion   string `json:"agent_version"`
	SingboxVersion string `json:"singbox_version"`
	UptimeSec      int64  `json:"uptime_sec"`
	CPUPercent     float64 `json:"cpu_percent"`
	MemPercent     float64 `json:"mem_percent"`
	DiskPercent    float64 `json:"disk_percent"`
	Load1          float64 `json:"load1"`
	IPv4           string `json:"ipv4,omitempty"`
	IPv6           string `json:"ipv6,omitempty"`
	SingboxRunning bool   `json:"singbox_running"`
	NodeCount      int    `json:"node_count"`
	AppliedRevision int64 `json:"applied_revision"`
	Timestamp      int64  `json:"timestamp"`
}

// Hello 是 Agent 首次连接 / 重连时发送的握手消息。
type Hello struct {
	MachineID    string `json:"machine_id,omitempty"` // 首次为空，注册后回填
	EnrollToken  string `json:"enroll_token,omitempty"`
	MachineSecret string `json:"machine_secret,omitempty"` // 已注册时的认证凭据
	Hostname     string `json:"hostname"`
	AgentVersion string `json:"agent_version"`
	OS           string `json:"os"`
	Kernel       string `json:"kernel"`
	Arch         string `json:"arch"`
}

// HelloAck 是 Manager 对 Hello 的应答。
type HelloAck struct {
	MachineID       string `json:"machine_id"`
	MachineSecret   string `json:"machine_secret,omitempty"` // 首次注册时下发
	Accepted        bool   `json:"accepted"`
	Reason          string `json:"reason,omitempty"`
}

// TrafficDelta 是流量增量（第二十四节）。Manager 以 (machine_id, sequence)
// 唯一约束防重复。
type TrafficDelta struct {
	MachineID string `json:"machine_id"`
	Sequence  int64  `json:"sequence"`
	NodeUUID  string `json:"node_uuid,omitempty"` // 空 = system 汇总
	RxBytes   int64  `json:"rx_bytes"`
	TxBytes   int64  `json:"tx_bytes"`
	StartTime int64  `json:"start_time"`
	EndTime   int64  `json:"end_time"`
}

// IPSnapshot 是在线 IP 快照（第二十九/三十节）。
type IPSnapshot struct {
	MachineID  string      `json:"machine_id"`
	NodeUUID   string      `json:"node_uuid"`
	LocalPort  int         `json:"local_port"`
	ActiveIPs  []ActiveIP  `json:"active_ips"`
}

// ActiveIP 是一个活跃公网源 IP。
type ActiveIP struct {
	IP       string `json:"ip"`
	Proto    string `json:"proto"` // tcp | udp
	LastSeen int64  `json:"last_seen"`
}

// SyncState 是配置同步状态（第二十一节）。
type SyncState struct {
	MachineID        string `json:"machine_id"`
	DesiredRevision  int64  `json:"desired_revision"`
	AppliedRevision  int64  `json:"applied_revision"`
}
