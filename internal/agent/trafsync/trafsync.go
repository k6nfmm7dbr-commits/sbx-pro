// Package trafsync 实现 Agent 的本地流量采集 + 增量同步（开发提示词第二十二/二十四节）。
//
// 复用原 sbx 的 traffic.Collector 做本地采集（nftables counter → SQLite totals，
// 累计值来自内核 byte counter，绝不靠速率积分）。同步层负责把 totals 的增量
// 打包成 traffic_delta 发送给 Manager。
//
// 一致性模型（长期不漂移）：
//   - delta 计算基于「已同步基准（sync_base:<scope>）」，基准持久化到本地 meta，
//     进程重启后不丢，绝不重复上报历史累计；
//   - 每条 delta 分配全局递增 sequence（持久化），写入本地 traffic_pending 表；
//   - 发送 pending 时使用固定 sequence，Manager 用 UNIQUE(machine_id, sequence) 去重；
//   - Manager 入库后回 traffic_ack，Agent 收到才删除 pending；
//   - 断线/崩溃/ACK 丢失 → pending 保留，重连后重发，幂等去重保证「最多重放、不翻倍」。
package trafsync

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/config"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/database"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/firewall"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/nodes"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/protocol"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/traffic"
)

// Sender 是发送 traffic_delta 的接口（由 client 实现）。
type Sender interface {
	SendTrafficDelta(td protocol.TrafficDelta) error
}

// Sync 是流量采集 + 同步器。
type Sync struct {
	Cfg     *config.Config
	DB      *database.DB
	Nodes   func() []nodes.Node
	Sender  Sender
	Machine string

	seqMu sync.Mutex
	seq   int64
}

// New 构造 Sync 并确保本地 pending 表存在。
func New(cfg *config.Config, db *database.DB, machine string, nodesFn func() []nodes.Node, snd Sender) *Sync {
	s := &Sync{
		Cfg:     cfg,
		DB:      db,
		Nodes:   nodesFn,
		Sender:  snd,
		Machine: machine,
	}
	s.ensureSchema()
	s.loadSequence()
	return s
}

func (s *Sync) ensureSchema() {
	_, _ = s.DB.Exec(`
		CREATE TABLE IF NOT EXISTS traffic_pending (
			sequence   INTEGER PRIMARY KEY,
			node_uuid  TEXT NOT NULL DEFAULT '',
			rx_bytes   INTEGER NOT NULL DEFAULT 0,
			tx_bytes   INTEGER NOT NULL DEFAULT 0,
			start_time INTEGER NOT NULL DEFAULT 0,
			end_time   INTEGER NOT NULL DEFAULT 0
		)`)
}

// loadSequence 从本地 meta 恢复 sequence（重启后不重复）。
func (s *Sync) loadSequence() {
	var v string
	if err := s.DB.QueryRow("SELECT v FROM meta WHERE k='sync_sequence'").Scan(&v); err == nil {
		n, _ := strconv.ParseInt(v, 10, 64)
		s.seq = n
	}
}

func (s *Sync) saveSequence() {
	_, _ = s.DB.Exec(
		`INSERT INTO meta (k, v) VALUES ('sync_sequence', ?) ON CONFLICT(k) DO UPDATE SET v = excluded.v`,
		strconv.FormatInt(s.seq, 10))
}

// ApplyRules 生成并应用 nft 计数规则（复用原 sbx GenNFT）。
func (s *Sync) ApplyRules() error {
	list := s.Nodes()
	epoch := uint64(time.Now().UnixNano())
	nftConf := s.Cfg.NftConf
	if err := os.MkdirAll(config.AppDir(), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(nftConf, []byte(firewall.GenNFT(list, epoch)), 0o644); err != nil {
		return err
	}
	rc, _, errMsg := firewall.RunCmd(context.Background(), "nft", "-f", nftConf)
	if rc != 0 {
		return fmt.Errorf("nft 应用失败: %s", errMsg)
	}
	_ = firewall.WriteEffectiveBackend("nft")
	slog.Info("nft 计数规则已应用", "nodes", len(list))
	return nil
}

// SyncOnce 执行一轮采集 + 增量同步：
//  1. Collector.Tick 采集（totals 累计）；
//  2. computeDeltas 把新增量写入 pending（递增 sequence + 更新基准）；
//  3. flushPending 发送所有未确认的 pending。
func (s *Sync) SyncOnce(ctx context.Context) error {
	collector := traffic.NewCollector(s.Cfg, s.DB)
	if err := collector.Tick(ctx); err != nil {
		if !firewall.IsLookup(err) {
			return fmt.Errorf("采集失败: %w", err)
		}
		slog.Warn("计数规则不存在，尝试应用规则", "err", err)
		if err := s.ApplyRules(); err != nil {
			return err
		}
		return nil
	}

	if err := s.computeDeltas(); err != nil {
		return err
	}
	return s.flushPending()
}

// computeDeltas 从本地 totals 计算相对已同步基准的增量，写入 pending。
// 每个 scope 分配唯一递增 sequence（持久化），基准同步更新并持久化。
func (s *Sync) computeDeltas() error {
	rows, err := s.DB.Query("SELECT scope, rx, tx FROM totals")
	if err != nil {
		return err
	}
	defer rows.Close()

	now := time.Now().Unix()
	type scopeVal struct {
		scope string
		rx    int64
		tx    int64
	}
	var scopes []scopeVal
	for rows.Next() {
		var sc string
		var rx, tx int64
		if err := rows.Scan(&sc, &rx, &tx); err != nil {
			return err
		}
		scopes = append(scopes, scopeVal{sc, rx, tx})
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for _, sv := range scopes {
		baseRx, baseTx := s.baseline(sv.scope)
		deltaRx := sv.rx - baseRx
		deltaTx := sv.tx - baseTx
		// 计数器归零（reset）会导致 rx < baseRx：开启新基准，不产生负 delta，
		// 历史累计由本地 totals 的 epoch 机制保证已衔接。
		if deltaRx < 0 {
			deltaRx = 0
		}
		if deltaTx < 0 {
			deltaTx = 0
		}
		if deltaRx == 0 && deltaTx == 0 {
			continue
		}

		nodeID := ""
		if len(sv.scope) > 5 && sv.scope[:5] == "node:" {
			nodeID = sv.scope[5:]
		}

		s.seqMu.Lock()
		s.seq++
		seq := s.seq
		s.saveSequence()
		s.seqMu.Unlock()

		if _, err := s.DB.Exec(`
			INSERT INTO traffic_pending (sequence, node_uuid, rx_bytes, tx_bytes, start_time, end_time)
			VALUES (?, ?, ?, ?, ?, ?)`,
			seq, nodeID, deltaRx, deltaTx, now, now); err != nil {
			return fmt.Errorf("写入 pending 失败: %w", err)
		}

		// 基准更新并持久化（pending 已固化 delta，重连后重发，不依赖基准）。
		s.setBaseline(sv.scope, sv.rx, sv.tx)
	}
	return nil
}

// flushPending 发送所有未确认的 pending（固定 sequence，重发安全）。
func (s *Sync) flushPending() error {
	rows, err := s.DB.Query(`
		SELECT sequence, node_uuid, rx_bytes, tx_bytes, start_time, end_time
		FROM traffic_pending ORDER BY sequence`)
	if err != nil {
		return err
	}
	defer rows.Close()

	type pend struct {
		seq         int64
		nodeUUID    string
		rx, tx      int64
		start, end  int64
	}
	var items []pend
	for rows.Next() {
		var p pend
		if err := rows.Scan(&p.seq, &p.nodeUUID, &p.rx, &p.tx, &p.start, &p.end); err != nil {
			return err
		}
		items = append(items, p)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for _, p := range items {
		td := protocol.TrafficDelta{
			MachineID: s.Machine,
			Sequence:  p.seq,
			NodeUUID:  p.nodeUUID,
			RxBytes:   p.rx,
			TxBytes:   p.tx,
			StartTime: p.start,
			EndTime:   p.end,
		}
		if err := s.Sender.SendTrafficDelta(td); err != nil {
			return fmt.Errorf("发送流量增量失败: %w", err)
		}
	}
	return nil
}

// OnAck 处理 Manager 的入库确认：删除已确认的 pending。
// sequence 单调递增，删除 <= ack.Sequence 的所有 pending（幂等）。
func (s *Sync) OnAck(ack protocol.TrafficAck) {
	if ack.MachineID != s.Machine || !ack.Accepted {
		return
	}
	if _, err := s.DB.Exec(`DELETE FROM traffic_pending WHERE sequence <= ?`, ack.Sequence); err != nil {
		slog.Warn("清理 pending 失败", "err", err)
	}
}

// baseline 读取某 scope 的已同步基准（meta 持久化）。
func (s *Sync) baseline(scope string) (rx, tx int64) {
	var v string
	err := s.DB.QueryRow("SELECT v FROM meta WHERE k=?", "sync_base:"+scope).Scan(&v)
	if err != nil {
		return 0, 0
	}
	parts := strings.SplitN(v, ":", 2)
	if len(parts) == 2 {
		rx, _ = strconv.ParseInt(parts[0], 10, 64)
		tx, _ = strconv.ParseInt(parts[1], 10, 64)
	}
	return rx, tx
}

// setBaseline 持久化某 scope 的已同步基准。
func (s *Sync) setBaseline(scope string, rx, tx int64) {
	v := strconv.FormatInt(rx, 10) + ":" + strconv.FormatInt(tx, 10)
	_, _ = s.DB.Exec(
		`INSERT INTO meta (k, v) VALUES (?, ?) ON CONFLICT(k) DO UPDATE SET v = excluded.v`,
		"sync_base:"+scope, v)
}

var _ = database.Open
