package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	monitor "github.com/phlohouse/phlo-monitor"
)

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
	for _, extra := range [][]string{{"--interval", "0s"}, {"--count", "-1"}, {"--output", filepath.Join(dir, "history"), "--max-bytes", "1"}} {
		if err := run(context.Background(), append(args, extra...), &out, &stderr); err == nil {
			t.Fatalf("accepted invalid options: %v", extra)
		}
	}
	if err := os.WriteFile(inv, []byte(`{"version":1,"targets":[{"id":"a","service":"a","path":"../escape"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), args, &out, &stderr); err == nil || !strings.Contains(err.Error(), "load inventory") {
		t.Fatalf("invalid inventory: %v", err)
	}
}
