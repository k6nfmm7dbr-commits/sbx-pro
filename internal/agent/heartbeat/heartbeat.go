// Package heartbeat 实现 Agent 心跳数据采集（开发提示词第八节）。
//
// 采集 cpu / memory / disk / load / uptime，全部从 /proc、syscall 读取，
// 不依赖 exec 外部命令，保持轻量。
package heartbeat

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/protocol"
)

// Collector 是心跳采集器（保留上次 CPU 快照以计算使用率）。
type Collector struct {
	StartedAt time.Time
	lastCPU   cpuTimes
	lastCPUT  time.Time
	hasCPU    bool
}

type cpuTimes struct {
	user, nice, system, idle, iowait, irq, softirq, steal uint64
}

// New 构造心跳采集器。
func New() *Collector {
	return &Collector{StartedAt: time.Now()}
}

// Build 构造一条 Heartbeat 消息。
func (c *Collector) Build(machineID string, extra func(*protocol.Heartbeat)) protocol.Heartbeat {
	hb := protocol.Heartbeat{
		MachineID:   machineID,
		UptimeSec:   int64(time.Since(c.StartedAt).Seconds()),
		CPUPercent:  c.cpuPercent(),
		MemPercent:  c.memPercent(),
		DiskPercent: c.diskPercent(),
		Load1:       c.load1(),
		Timestamp:   time.Now().Unix(),
	}
	if extra != nil {
		extra(&hb)
	}
	return hb
}

// cpuPercent 计算 CPU 使用率（基于 /proc/stat 两次采样差值）。
func (c *Collector) cpuPercent() float64 {
	t, ok := readCPU()
	if !ok {
		return 0
	}
	now := time.Now()
	defer func() { c.lastCPU, c.lastCPUT, c.hasCPU = t, now, true }()
	if !c.hasCPU {
		return 0
	}
	dt := now.Sub(c.lastCPUT).Seconds()
	if dt <= 0 {
		return 0
	}
	prev := c.lastCPU
	busy := delta(prev.user, t.user) + delta(prev.nice, t.nice) + delta(prev.system, t.system) +
		delta(prev.irq, t.irq) + delta(prev.softirq, t.softirq) + delta(prev.steal, t.steal)
	idle := delta(prev.idle, t.idle) + delta(prev.iowait, t.iowait)
	total := busy + idle
	if total == 0 {
		return 0
	}
	return round1(float64(busy) / float64(total) * 100)
}

func delta(a, b uint64) uint64 {
	if b >= a {
		return b - a
	}
	return 0
}

func readCPU() (cpuTimes, bool) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return cpuTimes{}, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return cpuTimes{}, false
	}
	fields := strings.Fields(sc.Text())
	if len(fields) < 8 || fields[0] != "cpu" {
		return cpuTimes{}, false
	}
	var t cpuTimes
	t.user, _ = strconv.ParseUint(fields[1], 10, 64)
	t.nice, _ = strconv.ParseUint(fields[2], 10, 64)
	t.system, _ = strconv.ParseUint(fields[3], 10, 64)
	t.idle, _ = strconv.ParseUint(fields[4], 10, 64)
	t.iowait, _ = strconv.ParseUint(fields[5], 10, 64)
	t.irq, _ = strconv.ParseUint(fields[6], 10, 64)
	t.softirq, _ = strconv.ParseUint(fields[7], 10, 64)
	if len(fields) > 8 {
		t.steal, _ = strconv.ParseUint(fields[8], 10, 64)
	}
	return t, true
}

// memPercent 计算内存使用率（/proc/meminfo）。
func (c *Collector) memPercent() float64 {
	memTotal, memAvail := readMeminfo()
	if memTotal == 0 {
		return 0
	}
	used := memTotal - memAvail
	return round1(float64(used) / float64(memTotal) * 100)
}

func readMeminfo() (total, avail uint64) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		var v uint64
		switch fields[0] {
		case "MemTotal:":
			v, _ = strconv.ParseUint(fields[1], 10, 64)
			total = v
		case "MemAvailable:":
			v, _ = strconv.ParseUint(fields[1], 10, 64)
			avail = v
		}
	}
	return total, avail
}

// diskPercent 计算根分区使用率（syscall.Statfs）。
func (c *Collector) diskPercent() float64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs("/", &st); err != nil {
		return 0
	}
	total := st.Blocks * uint64(st.Bsize)
	free := st.Bavail * uint64(st.Bsize)
	if total == 0 {
		return 0
	}
	return round1(float64(total-free) / float64(total) * 100)
}

// load1 读取 1 分钟负载（/proc/loadavg）。
func (c *Collector) load1() float64 {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) < 1 {
		return 0
	}
	v, _ := strconv.ParseFloat(fields[0], 64)
	return round1(v)
}

func round1(v float64) float64 {
	return float64(int(v*10+0.5)) / 10
}
