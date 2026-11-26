// Package status gathers what the status page shows: runner slots, job
// history, machine health, stick usage, builders and the host-disk check.
package status

import (
	"bufio"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Rightbracket/usb-gha-runner/ghr/internal/config"
	"github.com/Rightbracket/usb-gha-runner/ghr/internal/supervisor"
)

type Disk struct {
	Name      string   `json:"name"`
	Model     string   `json:"model,omitempty"`
	SizeGB    float64  `json:"size_gb"`
	BootStick bool     `json:"boot_stick"`
	ReadOnly  bool     `json:"read_only"`
	Mounted   []string `json:"mounted,omitempty"`  // mounted partitions/devices
	Holders   []string `json:"holders,omitempty"`  // dm/md devices built on it
	Swap      bool     `json:"swap"`
	OK        bool     `json:"ok"` // host disk: read-only, unmounted, no holders, no swap
}

type Machine struct {
	Hostname    string   `json:"hostname"`
	UptimeSec   int64    `json:"uptime_sec"`
	Load1       float64  `json:"load1"`
	CPUs        int      `json:"cpus"`
	MemTotalMB  int      `json:"mem_total_mb"`
	MemAvailMB  int      `json:"mem_avail_mb"`
	TempC       float64  `json:"temp_c,omitempty"`
	IPs         []string `json:"ips"`
	GitHubOK    bool     `json:"github_ok"`
	GitHubError string   `json:"github_error,omitempty"`
}

type Stick struct {
	DataTotalGB float64 `json:"data_total_gb"`
	DataFreeGB  float64 `json:"data_free_gb"`
	DataUsedPct float64 `json:"data_used_pct"`
	CacheGB     float64 `json:"cache_gb"` // Docker + Podman storage, from /run/ghr/cache.json
}

type Builders struct {
	Docker string   `json:"docker"` // systemd ActiveState
	Sysbox string   `json:"sysbox"`
	Podman bool     `json:"podman"`
	QEMU   []string `json:"qemu"` // registered binfmt arches
}

type Status struct {
	Time       time.Time                 `json:"time"`
	Configured bool                      `json:"configured"`
	Target     string                    `json:"target,omitempty"`
	Workers    int                       `json:"workers"`
	Slots      []supervisor.SlotState    `json:"slots"`
	History    []supervisor.HistoryEntry `json:"history"`
	Machine    Machine                   `json:"machine"`
	Stick      Stick                     `json:"stick"`
	Builders   Builders                  `json:"builders"`
	Disks      []Disk                    `json:"disks"`
	DisksOK    bool                      `json:"disks_ok"`
	SSH        string                    `json:"ssh"`
}

// Paths are overridable for tests.
type Collector struct {
	Sys        string // /sys
	Proc       string // /proc
	SlotsDir   string
	History    string
	BootDisk   string // file holding the boot stick's kernel name
	CacheFile  string
	DataDir    string
	ConfigPath string

	ghMu      sync.Mutex
	ghChecked time.Time
	ghOK      bool
	ghErr     string
}

func Default() *Collector {
	return &Collector{Sys: "/sys", Proc: "/proc", SlotsDir: config.SlotsDir, History: config.HistoryPath,
		BootDisk: config.BootDiskRun, CacheFile: config.RunDir + "/cache.json", DataDir: config.DataDir, ConfigPath: config.ConfigPath}
}

func (c *Collector) Collect() *Status {
	st := &Status{Time: time.Now()}
	if cfg, err := config.Load(c.ConfigPath); err == nil {
		st.Configured = cfg.Configured()
		st.Target = cfg.URL
		if st.Configured {
			st.Workers = cfg.WorkerCount()
		}
	}
	st.Slots = c.slots()
	st.History = c.history(25)
	st.Machine = c.machine()
	st.Stick = c.stick()
	st.Builders = c.builders()
	st.Disks = c.Disks()
	st.DisksOK = true
	for _, d := range st.Disks {
		if !d.BootStick && !d.OK {
			st.DisksOK = false
		}
	}
	st.SSH = unitState("ssh.service")
	return st
}

func (c *Collector) slots() []supervisor.SlotState {
	var out []supervisor.SlotState
	files, _ := filepath.Glob(filepath.Join(c.SlotsDir, "*.json"))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var s supervisor.SlotState
		if json.Unmarshal(b, &s) == nil {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slot < out[j].Slot })
	return out
}

func (c *Collector) history(n int) []supervisor.HistoryEntry {
	f, err := os.Open(c.History)
	if err != nil {
		return nil
	}
	defer f.Close()
	var all []supervisor.HistoryEntry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e supervisor.HistoryEntry
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			all = append(all, e)
		}
	}
	if len(all) > n {
		all = all[len(all)-n:]
	}
	for i, j := 0, len(all)-1; i < j; i, j = i+1, j-1 { // newest first
		all[i], all[j] = all[j], all[i]
	}
	return all
}

func (c *Collector) machine() Machine {
	m := Machine{CPUs: runtime.NumCPU()}
	m.Hostname, _ = os.Hostname()
	if b, err := os.ReadFile(filepath.Join(c.Proc, "uptime")); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			v, _ := strconv.ParseFloat(f[0], 64)
			m.UptimeSec = int64(v)
		}
	}
	if b, err := os.ReadFile(filepath.Join(c.Proc, "loadavg")); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			m.Load1, _ = strconv.ParseFloat(f[0], 64)
		}
	}
	if b, err := os.ReadFile(filepath.Join(c.Proc, "meminfo")); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			f := strings.Fields(line)
			if len(f) < 2 {
				continue
			}
			kb, _ := strconv.Atoi(f[1])
			switch f[0] {
			case "MemTotal:":
				m.MemTotalMB = kb / 1024
			case "MemAvailable:":
				m.MemAvailMB = kb / 1024
			}
		}
	}
	zones, _ := filepath.Glob(filepath.Join(c.Sys, "class/thermal/thermal_zone*/temp"))
	for _, z := range zones {
		if b, err := os.ReadFile(z); err == nil {
			v, _ := strconv.Atoi(strings.TrimSpace(string(b)))
			if t := float64(v) / 1000; t > m.TempC && t < 150 {
				m.TempC = t
			}
		}
	}
	ifaces, _ := net.Interfaces()
	for _, i := range ifaces {
		if i.Flags&net.FlagLoopback != 0 || strings.HasPrefix(i.Name, "docker") || strings.HasPrefix(i.Name, "veth") || strings.HasPrefix(i.Name, "br-") {
			continue
		}
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLinkLocalUnicast() {
				m.IPs = append(m.IPs, ipn.IP.String())
			}
		}
	}
	m.GitHubOK, m.GitHubError = c.github()
	return m
}

func (c *Collector) github() (bool, string) {
	c.ghMu.Lock()
	defer c.ghMu.Unlock()
	if time.Since(c.ghChecked) < 30*time.Second {
		return c.ghOK, c.ghErr
	}
	c.ghChecked = time.Now()
	cl := &http.Client{Timeout: 5 * time.Second}
	resp, err := cl.Get("https://api.github.com/zen")
	if err != nil {
		c.ghOK, c.ghErr = false, err.Error()
	} else {
		resp.Body.Close()
		c.ghOK, c.ghErr = resp.StatusCode < 500, ""
	}
	return c.ghOK, c.ghErr
}

func (c *Collector) stick() Stick {
	var s Stick
	var fs syscall.Statfs_t
	if syscall.Statfs(c.DataDir, &fs) == nil {
		total := float64(fs.Blocks) * float64(fs.Bsize)
		free := float64(fs.Bavail) * float64(fs.Bsize)
		s.DataTotalGB, s.DataFreeGB = total/1e9, free/1e9
		if total > 0 {
			s.DataUsedPct = 100 * (1 - float64(fs.Bfree)/float64(fs.Blocks))
		}
	}
	if b, err := os.ReadFile(c.CacheFile); err == nil {
		var v struct {
			Bytes float64 `json:"bytes"`
		}
		if json.Unmarshal(b, &v) == nil {
			s.CacheGB = v.Bytes / 1e9
		}
	}
	return s
}

func (c *Collector) builders() Builders {
	b := Builders{Docker: unitState("docker.service"), Sysbox: unitState("sysbox.service")}
	_, err := exec.LookPath("podman")
	b.Podman = err == nil
	ents, _ := os.ReadDir(filepath.Join(c.Proc, "sys/fs/binfmt_misc"))
	for _, e := range ents {
		if n := e.Name(); strings.HasPrefix(n, "qemu-") {
			b.QEMU = append(b.QEMU, strings.TrimPrefix(n, "qemu-"))
		}
	}
	sort.Strings(b.QEMU)
	return b
}

func unitState(unit string) string {
	out, err := exec.Command("systemctl", "show", "-p", "ActiveState", "--value", unit).Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

// physical reports whether a /sys/block entry is a real disk worth checking.
func physical(name string) bool {
	for _, p := range []string{"loop", "ram", "zram", "dm-", "md", "nbd", "sr", "fd"} {
		if strings.HasPrefix(name, p) {
			return false
		}
	}
	return true
}

// Disks lists every physical disk with the host-disk safety checks.
func (c *Collector) Disks() []Disk {
	boot := ""
	if b, err := os.ReadFile(c.BootDisk); err == nil {
		boot = strings.TrimSpace(string(b))
	}
	mounts := map[string][]string{}
	if b, err := os.ReadFile(filepath.Join(c.Proc, "mounts")); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			f := strings.Fields(line)
			if len(f) >= 2 && strings.HasPrefix(f[0], "/dev/") {
				dev := strings.TrimPrefix(f[0], "/dev/")
				mounts[dev] = append(mounts[dev], f[1])
			}
		}
	}
	swaps := map[string]bool{}
	if b, err := os.ReadFile(filepath.Join(c.Proc, "swaps")); err == nil {
		for _, line := range strings.Split(string(b), "\n")[1:] {
			if f := strings.Fields(line); len(f) > 0 {
				swaps[strings.TrimPrefix(f[0], "/dev/")] = true
			}
		}
	}
	ents, _ := os.ReadDir(filepath.Join(c.Sys, "block"))
	var out []Disk
	for _, e := range ents {
		name := e.Name()
		if !physical(name) {
			continue
		}
		dir := filepath.Join(c.Sys, "block", name)
		d := Disk{Name: name, BootStick: name == boot}
		d.Model = strings.TrimSpace(readStr(filepath.Join(dir, "device/model")))
		sectors, _ := strconv.ParseFloat(strings.TrimSpace(readStr(filepath.Join(dir, "size"))), 64)
		d.SizeGB = sectors * 512 / 1e9
		d.ReadOnly = strings.TrimSpace(readStr(filepath.Join(dir, "ro"))) == "1"
		// The disk itself plus its partitions.
		devs := []string{name}
		parts, _ := os.ReadDir(dir)
		for _, p := range parts {
			if strings.HasPrefix(p.Name(), name) {
				devs = append(devs, p.Name())
				if strings.TrimSpace(readStr(filepath.Join(dir, p.Name(), "ro"))) != "1" {
					if !d.BootStick {
						d.ReadOnly = false
					}
				}
			}
		}
		for _, dev := range devs {
			for _, mp := range mounts[dev] {
				d.Mounted = append(d.Mounted, "/dev/"+dev+" on "+mp)
			}
			if swaps[dev] {
				d.Swap = true
			}
			hp := filepath.Join(dir, "holders")
			if dev != name {
				hp = filepath.Join(dir, dev, "holders")
			}
			hs, _ := os.ReadDir(hp)
			for _, h := range hs {
				d.Holders = append(d.Holders, dev+"→"+h.Name())
			}
		}
		d.OK = d.ReadOnly && len(d.Mounted) == 0 && len(d.Holders) == 0 && !d.Swap
		out = append(out, d)
	}
	return out
}

func readStr(p string) string {
	b, _ := os.ReadFile(p)
	return string(b)
}
