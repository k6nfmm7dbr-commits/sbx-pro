package heartbeat

import (
	"testing"

	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/protocol"
)

func TestReadCPU(t *testing.T) {
	tm, ok := readCPU()
	if !ok {
		t.Fatal("readCPU failed (非 Linux 或有权限问题)")
	}
	// CPU 时间字段至少 idle 非零（真实系统上）。
	if tm.idle == 0 && tm.user == 0 && tm.system == 0 {
		t.Error("CPU times all zero, 异常")
	}
}

func TestReadMeminfo(t *testing.T) {
	total, avail := readMeminfo()
	if total == 0 {
		t.Skip("meminfo 不可读")
	}
	if avail > total {
		t.Errorf("avail(%d) > total(%d)", avail, total)
	}
}

func TestCPUPercentMonotonic(t *testing.T) {
	c := New()
	// 首次采样返回 0（无基线）。
	if c.cpuPercent() != 0 {
		t.Error("首次采样应返回 0（无基线）")
	}
	// 后续采样在 [0,100]。
	p := c.cpuPercent()
	if p < 0 || p > 100 {
		t.Errorf("cpuPercent = %f, 超出 [0,100]", p)
	}
}

func TestMemPercentRange(t *testing.T) {
	c := New()
	p := c.memPercent()
	if p < 0 || p > 100 {
		t.Errorf("memPercent = %f, 超出 [0,100]", p)
	}
}

func TestDiskPercentRange(t *testing.T) {
	c := New()
	p := c.diskPercent()
	if p < 0 || p > 100 {
		t.Errorf("diskPercent = %f, 超出 [0,100]", p)
	}
}

func TestLoad1(t *testing.T) {
	c := New()
	l := c.load1()
	if l < 0 {
		t.Errorf("load1 = %f, 不能为负", l)
	}
}

func TestRound1(t *testing.T) {
	cases := []struct{ in, want float64 }{
		{12.34, 12.3},
		{12.35, 12.4},
		{99.99, 100.0},
		{0.04, 0.0},
	}
	for _, c := range cases {
		if got := round1(c.in); got != c.want {
			t.Errorf("round1(%f) = %f, want %f", c.in, got, c.want)
		}
	}
}

func TestBuildHeartbeat(t *testing.T) {
	c := New()
	hb := c.Build("machine-1", func(h *protocol.Heartbeat) {
		h.Hostname = "test-host"
	})
	if hb.MachineID != "machine-1" {
		t.Errorf("MachineID = %s, want machine-1", hb.MachineID)
	}
	if hb.Hostname != "test-host" {
		t.Errorf("Hostname = %s, want test-host", hb.Hostname)
	}
	if hb.UptimeSec < 0 {
		t.Errorf("UptimeSec = %d, 不能为负", hb.UptimeSec)
	}
}
