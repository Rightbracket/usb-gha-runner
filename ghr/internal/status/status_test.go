package status

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, p, s string) {
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte(s), 0o644)
}

func TestDisks(t *testing.T) {
	d := t.TempDir()
	sys, proc := filepath.Join(d, "sys"), filepath.Join(d, "proc")
	// sda: boot stick (writable). nvme0n1: host disk, read-only. sdb: host disk with a writable partition that's mounted.
	write(t, sys+"/block/sda/size", "125000000\n")
	write(t, sys+"/block/sda/ro", "0\n")
	write(t, sys+"/block/sda/sda1/ro", "1\n")
	write(t, sys+"/block/nvme0n1/size", "1000000000\n")
	write(t, sys+"/block/nvme0n1/ro", "1\n")
	write(t, sys+"/block/nvme0n1/nvme0n1p1/ro", "1\n")
	write(t, sys+"/block/sdb/size", "2000\n")
	write(t, sys+"/block/sdb/ro", "1\n")
	write(t, sys+"/block/sdb/sdb1/ro", "0\n")
	write(t, sys+"/block/loop0/ro", "1\n")
	write(t, proc+"/mounts", "/dev/sda1 /run/live/medium vfat ro 0 0\n/dev/sdb1 /mnt ext4 rw 0 0\n")
	write(t, proc+"/swaps", "Filename Type Size Used Priority\n")
	write(t, d+"/boot-disk", "sda\n")
	c := &Collector{Sys: sys, Proc: proc, BootDisk: d + "/boot-disk"}
	disks := c.Disks()
	got := map[string]Disk{}
	for _, x := range disks {
		got[x.Name] = x
	}
	if len(disks) != 3 {
		t.Fatalf("want 3 disks (no loop), got %+v", disks)
	}
	if !got["sda"].BootStick {
		t.Error("sda should be the boot stick")
	}
	if !got["nvme0n1"].OK {
		t.Errorf("nvme0n1 should be OK: %+v", got["nvme0n1"])
	}
	if got["sdb"].OK || got["sdb"].ReadOnly || len(got["sdb"].Mounted) != 1 {
		t.Errorf("sdb should be flagged: %+v", got["sdb"])
	}
}
