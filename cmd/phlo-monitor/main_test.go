package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	monitor "github.com/phlohouse/phlo-monitor"
)

func TestRunVersionWithoutCollection(t *testing.T) {
	dir := t.TempDir()
	output := filepath.Join(dir, "must-not-be-created")
	var out, stderr bytes.Buffer
	err := run(context.Background(), []string{"--version", "--inventory", filepath.Join(dir, "missing"), "--root", filepath.Join(dir, "missing-root"), "--output", output}, &out, &stderr)
	want := "phlo-monitor dev " + runtime.GOOS + "/" + runtime.GOARCH + "\n"
	if err != nil || out.String() != want || stderr.Len() != 0 {
		t.Fatalf("version contract: stdout=%q stderr=%q error=%v", out.String(), stderr.String(), err)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("version request accessed history: %v", err)
	}
}

func TestRunBoundedAndErrors(t *testing.T) {
	dir := t.TempDir()
	inv := filepath.Join(dir, "inventory.json")
	if err := os.WriteFile(inv, []byte(`{"version":1,"targets":[{"id":"a","service":"a","path":"missing"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{"--inventory", inv, "--root", dir, "--count", "1"}
	var out, stderr bytes.Buffer
	if err := run(context.Background(), args, &out, &stderr); err != nil {
		t.Fatal(err)
	}
	var s monitor.Snapshot
	if err := json.Unmarshal(out.Bytes(), &s); err != nil || len(s.Groups) != 2 || s.Groups[1].Errors["cgroup"] == "" {
		t.Fatalf("bounded CLI lost missing-target gap: %s %v", out.Bytes(), err)
	}
	for _, extra := range [][]string{{"--interval", "0s"}, {"--count", "-1"}, {"--output", filepath.Join(dir, "history"), "--max-bytes", "1"}, {"--format", "csv"}} {
		if err := run(context.Background(), append(args, extra...), &out, &stderr); err == nil {
			t.Fatalf("accepted invalid options: %v", extra)
		}
	}
	// Table mode cannot silently turn bounded JSONL history into text output.
	history := filepath.Join(dir, "history")
	if err := os.WriteFile(history, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), append(args, "--format", "table", "--output", history), &out, &stderr); err == nil || !strings.Contains(err.Error(), "history files always use jsonl") {
		t.Fatalf("table/file combination: %v", err)
	}
	if b, err := os.ReadFile(history); err != nil || string(b) != "{}\n" {
		t.Fatalf("table mode modified history: %q %v", b, err)
	}
	out.Reset()
	if err := run(context.Background(), append(args, "--format", "jsonl", "--output", history, "--max-bytes", "65536"), &out, &stderr); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatal("file output must not also write to stdout")
	}
	b, err := os.ReadFile(history)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(bytes.TrimPrefix(b, []byte("{}\n")), &s); err != nil || s.Version != 1 || len(s.Groups) != 2 {
		t.Fatalf("explicit JSONL file format changed schema: %s %v", b, err)
	}
	closed, err := os.CreateTemp(dir, "closed-output")
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()
	if err := run(context.Background(), append(args, "--format", "table"), closed, &stderr); err == nil || !strings.Contains(err.Error(), "write sample") {
		t.Fatalf("table stdout failure was hidden: %v", err)
	}
	if err := os.WriteFile(inv, []byte(`{"version":1,"targets":[{"id":"a","service":"a","path":"../escape"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), args, &out, &stderr); err == nil || !strings.Contains(err.Error(), "load inventory") {
		t.Fatalf("invalid inventory: %v", err)
	}
}

func TestRunTableSeparatesTargetsAncestorsAndGaps(t *testing.T) {
	dir := t.TempDir()
	for path, memory := range map[string]string{".": "8388608", "account": "3407872", "account/worker": "1572864"} {
		group := filepath.Join(dir, path)
		if err := os.MkdirAll(group, 0700); err != nil {
			t.Fatal(err)
		}
		for name, value := range map[string]string{
			"cpu.stat": "usage_usec 3000000\nuser_usec 2000000\nsystem_usec 1000000\n",
			"cpu.max":  "max 100000\n", "memory.current": memory, "memory.max": "max\n",
			"memory.events":   "low 0\nhigh 0\nmax 0\noom 0\noom_kill 0\n",
			"cpu.pressure":    "some avg10=0 avg60=0 avg300=0 total=0\n",
			"memory.pressure": "some avg10=0 avg60=0 avg300=0 total=0\n",
		} {
			if err := os.WriteFile(filepath.Join(group, name), []byte(value), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	inv := filepath.Join(dir, "inventory.json")
	if err := os.WriteFile(inv, []byte(`{"version":1,"targets":[{"id":"a","service":"worker\n\u001b[31m","path":"account/worker"},{"id":"b","service":"missing","path":"account/missing"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	if err := run(context.Background(), []string{"--inventory", inv, "--root", dir, "--format", "table", "--interval", "1s", "--count", "2"}, &out, &stderr); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(out.String(), "\x1b\t") {
		t.Fatal("table contains raw terminal controls or unexpanded tabs")
	}
	samples := strings.Split(out.String(), "Sample ")[1:]
	if len(samples) != 2 {
		t.Fatalf("expected two table samples: %s", out.String())
	}
	for i, sample := range samples {
		sections := strings.Split(sample, "Ancestors (inclusive totals; do not add to target rows)")
		if len(sections) != 2 || !strings.Contains(sections[0], "do not sum overlapping targets") {
			t.Fatalf("missing anti-double-counting labels: %s", sample)
		}
		var worker, missing, ancestor []string
		for _, line := range strings.Split(sections[0], "\n") {
			fields := strings.Fields(line)
			if strings.HasPrefix(line, `worker\n\x1b[31m `) {
				worker = fields
			}
			if strings.HasPrefix(line, "missing ") {
				missing = fields
			}
		}
		for _, line := range strings.Split(sections[1], "\n") {
			fields := strings.Fields(line)
			if len(fields) > 1 && fields[1] == "account" {
				ancestor = fields
			}
		}
		cpu := "-"
		if i == 1 {
			cpu = "0.0"
		}
		if len(worker) < 5 || worker[1] != "account/worker" || worker[2] != cpu || worker[3] != "1.50" || !strings.Contains(strings.Join(worker[4:], " "), "unavailable: io.pressure") {
			t.Fatalf("worker row lost rate, memory, or warning: %v", worker)
		}
		if len(missing) < 5 || missing[2] != "-" || missing[3] != "-" || !strings.Contains(strings.Join(missing[4:], " "), "unavailable: cgroup") {
			t.Fatalf("missing target became zero: %v", missing)
		}
		if len(ancestor) < 5 || ancestor[3] != "3.25" || strings.Contains(sections[1], "account/worker") || strings.Contains(sections[1], "account/missing") {
			t.Fatalf("ancestor totals mixed with targets: %s", sections[1])
		}
		if (i == 0) != strings.Contains(strings.Join(worker[4:], " "), "CPU rate unavailable") {
			t.Fatalf("baseline and measured zero are indistinguishable: %v", worker)
		}
	}
}
