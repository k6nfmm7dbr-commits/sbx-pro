// Package trafsync 实现 Agent 的本地流量采集 + 增量同步（开发提示词第二十二/二十四节）。
//
// 复用原 sbx 的 traffic.Collector 做本地采集（nftables counter → SQLite），
// 然后定期读取本地 totals 的增量，打包 traffic_delta 发送给 Manager。
// sequence 本地持久化，断线重连可从断点继续，Manager 用 (machine_id, sequence) 防重复。
package trafsync

import (
	"context"
	"fmt"
	"log/slog"
	"os"
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

	seqMu  sync.Mutex
	seq    int64
	lastRx map[string]int64 // scope -> 上次已同步的累计 rx
	lastTx map[string]int64
}

// New 构造 Sync。
func New(cfg *config.Config, db *database.DB, machine string, nodesFn func() []nodes.Node, snd Sender) *Sync {
	s := &Sync{
		Cfg:     cfg,
		DB:      db,
		Nodes:   nodesFn,
		Sender:  snd,
		Machine: machine,
		lastRx:  map[string]int64{},
		lastTx:  map[string]int64{},
	}
	s.loadSequence()
	return s
}

// loadSequence 从本地 meta 恢复 sequence。
func (s *Sync) loadSequence() {
	var v string
	if err := s.DB.QueryRow("SELECT v FROM meta WHERE k='sync_sequence'").Scan(&v); err == nil {
		fmt.Sscanf(v, "%d", &s.seq)
	}
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

// SyncOnce 执行一轮采集 + 增量同步。
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

	deltas, err := s.computeDeltas()
	if err != nil {
		return err
	}
	for _, d := range deltas {
		if d.RxBytes == 0 && d.TxBytes == 0 {
			continue
		}
		if err := s.Sender.SendTrafficDelta(d); err != nil {
			return fmt.Errorf("发送流量增量失败: %w", err)
		}
		s.seqMu.Lock()
		s.seq++
		s.saveSequence()
		s.seqMu.Unlock()
	}
	return nil
}

// computeDeltas 从本地 totals 计算相对上次的增量。
func (s *Sync) computeDeltas() ([]protocol.TrafficDelta, error) {
	rows, err := s.DB.Query("SELECT scope, rx, tx FROM totals")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	now := time.Now().Unix()
	var out []protocol.TrafficDelta
	for rows.Next() {
		var scope string
		var rx, tx int64
		if err := rows.Scan(&scope, &rx, &tx); err != nil {
			return nil, err
		}
		deltaRx := rx - s.lastRx[scope]
		deltaTx := tx - s.lastTx[scope]
		s.lastRx[scope] = rx
		s.lastTx[scope] = tx

		nodeID := ""
		if len(scope) > 5 && scope[:5] == "node:" {
			nodeID = scope[5:]
		}
		s.seqMu.Lock()
		seq := s.seq
		s.seqMu.Unlock()
		out = append(out, protocol.TrafficDelta{
			MachineID: s.Machine,
			Sequence:  seq,
			NodeUUID:  nodeID,
			RxBytes:   deltaRx,
			TxBytes:   deltaTx,
			StartTime: now,
			EndTime:   now,
		})
	}
	return out, rows.Err()
}

// saveSequence 持久化 sequence。
func (s *Sync) saveSequence() {
	_, _ = s.DB.Exec(
		`INSERT INTO meta (k, v) VALUES ('sync_sequence', ?) ON CONFLICT(k) DO UPDATE SET v = excluded.v`,
		fmt.Sprintf("%d", s.seq))
}

var _ = database.Open
