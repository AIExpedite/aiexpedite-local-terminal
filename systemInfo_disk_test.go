package main

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/shirou/gopsutil/v4/disk"
)

// gbBytes converts a GB figure to the byte count gopsutil would report.
func gbBytes(gb float64) uint64 { return uint64(gb * float64(1<<30)) }

// fakeUsage answers disk.Usage from a fixed table keyed by mount point.
func fakeUsage(table map[string]*disk.UsageStat) func(string) (*disk.UsageStat, error) {
	return func(path string) (*disk.UsageStat, error) {
		if u, ok := table[path]; ok {
			return u, nil
		}
		return nil, errors.New("no such mount")
	}
}

// The standard Apple Silicon layout: "/" is the sealed system snapshot, and
// gopsutil's Device is the device node, never the mount point.
func TestDiskEntriesFromPartitions_ReportsDeviceAndMountPoint(t *testing.T) {
	parts := []disk.PartitionStat{
		{Device: "/dev/disk3s1s1", Mountpoint: "/"},
		{Device: "/dev/disk3s5", Mountpoint: "/System/Volumes/Data"},
		{Device: "C:", Mountpoint: "C:"},
	}
	usage := fakeUsage(map[string]*disk.UsageStat{
		"/":                    {Total: gbBytes(926.4), Free: gbBytes(395.4)},
		"/System/Volumes/Data": {Total: gbBytes(926.4), Free: gbBytes(395.4)},
		"C:":                   {Total: gbBytes(476), Free: gbBytes(12.3)},
	})

	got := diskEntriesFromPartitions(parts, usage)
	want := []diskEntry{
		{Drive: "/dev/disk3s1s1", Mount: "/", SizeGB: 926.4, FreeGB: 395.4},
		{Drive: "/dev/disk3s5", Mount: "/System/Volumes/Data", SizeGB: 926.4, FreeGB: 395.4},
		{Drive: "C:", Mount: "C:", SizeGB: 476, FreeGB: 12.3},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestDiskEntriesFromPartitions_SkipsZeroTotalAndUnreadableVolumes(t *testing.T) {
	parts := []disk.PartitionStat{
		{Device: "/dev/disk3s1s1", Mountpoint: "/"},
		{Device: "map auto_home", Mountpoint: "/System/Volumes/Data/home"}, // autofs: Total 0
		{Device: "/dev/disk9s1", Mountpoint: "/Volumes/Gone"},              // usage fails
		{Device: "/dev/disk10s1", Mountpoint: "/Volumes/Nil"},              // usage returns nil
	}
	usage := func(path string) (*disk.UsageStat, error) {
		switch path {
		case "/":
			return &disk.UsageStat{Total: gbBytes(926.4), Free: gbBytes(395.4)}, nil
		case "/System/Volumes/Data/home":
			return &disk.UsageStat{Total: 0, Free: 0}, nil
		case "/Volumes/Nil":
			return nil, nil
		}
		return nil, errors.New("statfs: no such file or directory")
	}

	got := diskEntriesFromPartitions(parts, usage)
	if len(got) != 1 || got[0].Mount != "/" {
		t.Fatalf("expected only the readable, non-empty volume, got %+v", got)
	}
}

// A free figure that rounds to 0.0 GB must be ABSENT from the JSON, not 0:
// terminal-service's smallest-volume fallback reads only numeric freeGB, and
// a numeric 0 on every Mac's 0.1 GB disk images would make it read 0 MB.
func TestDiskEntriesFromPartitions_FreeUnderPointZeroFiveGBIsOmitted(t *testing.T) {
	parts := []disk.PartitionStat{{Device: "/dev/disk5s1", Mountpoint: "/Volumes/Image"}}
	usage := fakeUsage(map[string]*disk.UsageStat{
		"/Volumes/Image": {Total: gbBytes(0.1), Free: 40 << 20}, // 40 MB free
	})

	got := diskEntriesFromPartitions(parts, usage)
	if len(got) != 1 {
		t.Fatalf("expected one entry, got %+v", got)
	}
	if got[0].FreeGB != 0 {
		t.Fatalf("FreeGB = %v, want 0 (rounded)", got[0].FreeGB)
	}
	b, err := json.Marshal(got[0])
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"drive":"/dev/disk5s1","mount":"/Volumes/Image","sizeGB":0.1}`
	if string(b) != want {
		t.Fatalf("JSON = %s, want %s", b, want)
	}
}

// Golden wire shape: the field order and names the server reads.
func TestDiskEntry_JSONGolden(t *testing.T) {
	parts := []disk.PartitionStat{{Device: "/dev/disk3s1s1", Mountpoint: "/"}}
	usage := fakeUsage(map[string]*disk.UsageStat{
		"/": {Total: gbBytes(926.4), Free: gbBytes(395.4)},
	})
	got := diskEntriesFromPartitions(parts, usage)
	if len(got) != 1 {
		t.Fatalf("expected one entry, got %+v", got)
	}
	b, err := json.Marshal(got[0])
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"drive":"/dev/disk3s1s1","mount":"/","sizeGB":926.4,"freeGB":395.4}`
	if string(b) != want {
		t.Fatalf("JSON = %s, want %s", b, want)
	}
}

// An entry with no mount point (none is expected from gopsutil) keeps the old
// three-field shape, so nothing changes for a server that ignores mount.
func TestDiskEntry_EmptyMountIsOmitted(t *testing.T) {
	b, err := json.Marshal(diskEntry{Drive: "C:", SizeGB: 476, FreeGB: 12.3})
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"drive":"C:","sizeGB":476,"freeGB":12.3}`
	if string(b) != want {
		t.Fatalf("JSON = %s, want %s", b, want)
	}
}
