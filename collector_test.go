package monitor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func fixture(t *testing.T, root, path, cpu string) {
	t.Helper()
	dir := filepath.Join(root, path)
	if !strings.Contains(cpu, "user_usec") {
		cpu += "user_usec 0\nsystem_usec 0\n"
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{
		"cpu.stat": cpu, "cpu.max": "max 100000\n",
		"memory.current": "4096\n", "memory.max": "max\n",
		"memory.events": "low 0\nhigh 2\nmax 3\noom 1\noom_kill 0\n",
		"cpu.pressure":  "some avg10=1.25 avg60=2.5 avg300=3.75 total=901\nfull avg10=0 avg60=0 avg300=0 total=0\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCollectRatesLimitsAndGaps(t *testing.T) {
	root := t.TempDir()
	fixture(t, root, ".", "usage_usec 10000000\n")
	fixture(t, root, "account", "usage_usec 8000000\n")
	fixture(t, root, "account/service", "usage_usec 3000000\nuser_usec 2000000\nsystem_usec 1000000\n")
	if err := os.WriteFile(filepath.Join(root, "account/cpu.max"), []byte("175000 100000\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := NewCollector(root)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	inv := Inventory{Version: 1, Targets: []Target{{Service: "trino", ID: "container-a", Path: "account/service"}}}
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	first, err := c.Sample(inv, at)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Groups) != 3 || first.Groups[2].CPUPercent != nil {
		t.Fatalf("first sample must include ancestors and no invented rate: %+v", first)
	}
	if got := first.Groups[1].CPUMax; got == nil || got.Quota == nil || *got.Quota != 175000 || got.Period != 100000 {
		t.Fatalf("ancestor quota lost: %+v", got)
	}
	if first.Groups[2].MemoryMax != nil || *first.Groups[2].MemoryCurrent != 4096 {
		t.Fatal("unlimited memory and measured usage must remain distinct")
	}
	if first.Groups[2].Pressure["cpu"].Some.TotalUsec != 901 || first.Groups[2].Errors["memory.pressure"] == "" {
		t.Fatal("PSI totals and unavailable pressure must be explicit")
	}
	fixture(t, root, "account/service", "usage_usec 4500000\nuser_usec 3000000\nsystem_usec 1500000\n")
	second, err := c.Sample(inv, at.Add(2*time.Second))
	if err != nil || second.Groups[2].CPUPercent == nil || *second.Groups[2].CPUPercent != 75 {
		t.Fatalf("1.5 CPU-seconds in two seconds must be 75%%: %+v, %v", second, err)
	}
	inv.Targets[0].ID = "replacement"
	third, err := c.Sample(inv, at.Add(3*time.Second))
	if err != nil || third.Groups[2].CPUPercent != nil {
		t.Fatal("container replacement must reset the rate baseline")
	}
	if err := os.RemoveAll(filepath.Join(root, "account/service")); err != nil {
		t.Fatal(err)
	}
	missing, err := c.Sample(inv, at.Add(4*time.Second))
	if err != nil || missing.Groups[2].Errors["cgroup"] == "" || missing.Groups[2].CPUPercent != nil {
		t.Fatal("disappearing cgroup must be a gap, not zero usage")
	}
	fixture(t, root, "account/service", "usage_usec 5000000\n")
	reappeared, _ := c.Sample(inv, at.Add(5*time.Second))
	if reappeared.Groups[2].CPUPercent != nil {
		t.Fatal("missing observation must reset baseline")
	}
	fixture(t, root, "account/service", "usage_usec 100\n")
	rollback, _ := c.Sample(inv, at.Add(6*time.Second))
	if rollback.Groups[2].CPUPercent != nil {
		t.Fatal("counter rollback must reset baseline")
	}
	// Keep the old inode alive so this proves replacement, not inode reuse.
	if err := os.Rename(filepath.Join(root, "account/service"), filepath.Join(root, "old")); err != nil {
		t.Fatal(err)
	}
	fixture(t, root, "account/service", "usage_usec 9000000\n")
	replaced, _ := c.Sample(inv, at.Add(7*time.Second))
	if replaced.Groups[2].CPUPercent != nil {
		t.Fatal("new cgroup at same path must reset baseline")
	}
	inv.Targets = append(inv.Targets, Target{Service: "second", ID: "b", Path: "account/second"})
	shared, _ := c.Sample(inv, at.Add(8*time.Second))
	if len(shared.Groups) != 4 {
		t.Fatalf("shared ancestors must appear once: %+v", shared.Groups)
	}
}

func TestInventoryAndEscapingPaths(t *testing.T) {
	for _, input := range []string{
		`{"version":2,"targets":[]}`, `{"version":1,"targets":[]}`,
		`{"version":1,"targets":[{"id":"a","service":"a","path":"../escape"}]}`,
		`{"version":1,"targets":[{"id":"a","service":"a","path":"/absolute"}]}`,
		`{"version":1,"targets":[{"id":"a","service":"a","path":"a/../b"}]}`,
		`{"version":1,"targets":[{"id":"a","service":"a","path":"a","extra":true}]}`,
		`{"version":1,"targets":[{"id":"a","service":"a","path":"a"},{"id":"a","service":"b","path":"b"}]}`,
		`{"version":1,"targets":[{"id":"a","service":"a","path":"a"}]} {}`,
		strings.Repeat(" ", 1<<20) + `{}`,
	} {
		if _, err := LoadInventory(strings.NewReader(input)); err == nil {
			t.Fatalf("accepted invalid inventory: %.120s", input)
		}
	}
	root, outside := t.TempDir(), t.TempDir()
	fixture(t, outside, ".", "usage_usec 999\n")
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	c, err := NewCollector(root)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	inv := Inventory{Version: 1, Targets: []Target{{Service: "a", ID: "a", Path: "escape"}}}
	s, err := c.Sample(inv, time.Now())
	if err != nil || s.Groups[1].Errors["cgroup"] == "" || s.Groups[1].CPU != nil {
		t.Fatalf("symlink escaped root: %+v %v", s, err)
	}
}

func TestMalformedCountersAreGaps(t *testing.T) {
	root := t.TempDir()
	fixture(t, root, "a", "usage_usec 9007199254740993\n")
	c, err := NewCollector(root)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	inv := Inventory{Version: 1, Targets: []Target{{Service: "a", ID: "a", Path: "a"}}}
	first, _ := c.Sample(inv, time.Now())
	if first.Groups[1].CPU["usage_usec"] != 9007199254740993 {
		t.Fatal("lost integer precision")
	}
	for file, data := range map[string]string{
		"cpu.stat":      "usage_usec 3\n",
		"memory.events": "oom 1\n",
		"memory.max":    "invalid",
		"cpu.pressure":  "some avg10=NaN avg60=0 avg300=0 total=0\n",
	} {
		if err := os.WriteFile(filepath.Join(root, "a", file), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	s, _ := c.Sample(inv, time.Now())
	g := s.Groups[1]
	if g.CPU != nil || g.CPUPercent != nil || g.MemoryMax != nil || g.MemoryEvents != nil || len(g.Pressure) != 0 {
		t.Fatalf("malformed values became observations: %+v", g)
	}
	for _, file := range []string{"cpu.stat", "memory.events", "memory.max", "cpu.pressure"} {
		if g.Errors[file] == "" {
			t.Fatalf("missing error for %s", file)
		}
	}
	if _, err := json.Marshal(s); err != nil {
		t.Fatal(err)
	}
	fixture(t, root, "a", "usage_usec 9007199256740993\n")
	recovered, _ := c.Sample(inv, time.Now())
	if recovered.Groups[1].CPUPercent != nil {
		t.Fatal("missing CPU counters must reset baseline even when usage increases")
	}
}

func TestReplacementDuringReadDiscardsGroup(t *testing.T) {
	root := t.TempDir()
	fixture(t, root, "a", "usage_usec 1\n")
	path := filepath.Join(root, "a/cpu.stat")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	c, err := NewCollector(root)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	done := make(chan Snapshot, 1)
	go func() {
		s, _ := c.Sample(Inventory{Version: 1, Targets: []Target{{Service: "a", ID: "a", Path: "a"}}}, time.Now())
		done <- s
	}()
	// Opening the writer waits until the collector is reading the pinned group.
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := os.Rename(filepath.Join(root, "a"), filepath.Join(root, "old")); err != nil {
		t.Fatal(err)
	}
	fixture(t, root, "a", "usage_usec 9000000\n")
	if _, err := f.WriteString("usage_usec 1\nuser_usec 1\nsystem_usec 0\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	select {
	case s := <-done:
		if s.Groups[1].CPU != nil || s.Groups[1].Errors["cgroup"] == "" {
			t.Fatalf("mixed cgroup observation: %+v", s.Groups[1])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("collection did not finish")
	}
}
