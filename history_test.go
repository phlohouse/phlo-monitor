package monitor

import (
	"encoding/json"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func TestHistoryRotatesWithoutSplittingRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "samples.jsonl")
	h := History{Path: path, MaxBytes: 40}
	for _, id := range []int{1, 2, 3, 4, 5} {
		if err := h.Append(map[string]int{"sample": id}); err != nil {
			t.Fatal(err)
		}
	}
	for file, expected := range map[string][]int{path + ".1": {1, 2, 3}, path: {4, 5}} {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSpace(string(b)), "\n")
		if len(lines) != len(expected) {
			t.Fatalf("wrong retained history %s: %s", file, b)
		}
		for i, line := range lines {
			var got map[string]int
			if err := json.Unmarshal([]byte(line), &got); err != nil || got["sample"] != expected[i] {
				t.Fatalf("record split or lost: %s", line)
			}
		}
	}
	before, _ := os.ReadFile(path)
	if err := h.Append(strings.Repeat("x", 100)); err == nil {
		t.Fatal("oversized record must fail rather than break the storage bound")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("oversized append damaged retained history")
	}
	if err := (History{Path: filepath.Join(path, "not-a-directory"), MaxBytes: 40}).Append(1); err == nil {
		t.Fatal("unwritable history must return an error")
	}
}

func TestHistoryRecoversCrashTail(t *testing.T) {
	for _, tail := range []string{`{"sample":`, strings.Repeat("x", 9000), ""} {
		t.Run(strconv.Itoa(len(tail)), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "samples.jsonl")
			if err := os.WriteFile(path, []byte("1\n"+tail), 0600); err != nil {
				t.Fatal(err)
			}
			h := History{Path: path, MaxBytes: 8}
			if err := h.Append(2); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(path)
			if err != nil || string(got) != "1\n2\n" {
				t.Fatalf("tail recovery: %q %v", got, err)
			}
		})
	}
	path := filepath.Join(t.TempDir(), "samples.jsonl")
	if err := os.WriteFile(path, []byte(`{"partial":`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := (History{Path: path, MaxBytes: 4}).Append(3); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "3\n" {
		t.Fatalf("first record recovery: %q %v", got, err)
	}
	if err := os.Mkdir(path+".1", 0700); err != nil {
		t.Fatal(err)
	}
	if err := (History{Path: path, MaxBytes: 3}).Append(4); err == nil {
		t.Fatal("rotation failure must be reported")
	}
	got, _ = os.ReadFile(path)
	if string(got) != "3\n" {
		t.Fatal("failed rotation damaged current history")
	}
}

func TestHistoryPartialWriteFailure(t *testing.T) {
	if os.Getenv("PHLO_TEST_WRITE_LIMIT") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestHistoryPartialWriteFailure$")
		cmd.Env = append(os.Environ(), "PHLO_TEST_WRITE_LIMIT=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("write-limit subprocess: %s %v", out, err)
		}
		return
	}
	// Impose an actual kernel write failure on this subprocess only.
	signal.Ignore(syscall.SIGXFSZ)
	var original syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &original); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &syscall.Rlimit{Cur: 512, Max: original.Max}); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &original); err != nil {
			t.Fatal(err)
		}
	}()
	path := filepath.Join(t.TempDir(), "samples.jsonl")
	h := History{Path: path, MaxBytes: 4096}
	if err := h.Append(1); err != nil {
		t.Fatal(err)
	}
	if err := h.Append(strings.Repeat("x", 1024)); err == nil {
		t.Fatal("kernel write failure was hidden")
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "1\n" {
		t.Fatalf("partial write damaged committed record: %q %v", b, err)
	}
	if err := h.Append(2); err != nil {
		t.Fatal(err)
	}
	b, err = os.ReadFile(path)
	if err != nil || string(b) != "1\n2\n" {
		t.Fatalf("append after failure: %q %v", b, err)
	}
}
