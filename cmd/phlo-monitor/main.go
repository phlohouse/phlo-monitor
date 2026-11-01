// Phlo Monitor samples a supplied Linux cgroup inventory without Docker access.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	monitor "github.com/phlohouse/phlo-monitor"
)

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("phlo-monitor", flag.ContinueOnError)
	flags.SetOutput(stderr)
	inventory := flags.String("inventory", "", "trusted Phlo inventory JSON file (required)")
	root := flags.String("root", "/sys/fs/cgroup", "host cgroup v2 filesystem root")
	output := flags.String("output", "-", "history file, or - for standard output")
	interval := flags.Duration("interval", 15*time.Second, "sampling interval, at least one second")
	count := flags.Int("count", 0, "stop after this many samples; zero runs until interrupted")
	maxBytes := flags.Int64("max-bytes", 64<<20, "maximum bytes per history file; one rotation retained")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if runtime.GOOS != "linux" {
		return errors.New("collection requires a Linux host with readable cgroup v2 files")
	}
	if *inventory == "" || *interval < time.Second || *count < 0 || *maxBytes <= 0 || flags.NArg() != 0 {
		return errors.New("supply --inventory, interval >= 1s, count >= 0 and max-bytes > 0")
	}
	c, err := monitor.NewCollector(*root)
	if err != nil {
		return err
	}
	defer c.Close()
	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	history := monitor.History{Path: *output, MaxBytes: *maxBytes}
	encoder := json.NewEncoder(stdout)
	for i := 0; *count == 0 || i < *count; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return nil
			case <-ticker.C:
			}
		}
		f, err := os.Open(*inventory)
		if err != nil {
			return fmt.Errorf("read inventory: %w", err)
		}
		inv, loadErr := monitor.LoadInventory(f)
		closeErr := f.Close()
		if err := errors.Join(loadErr, closeErr); err != nil {
			return fmt.Errorf("load inventory: %w", err)
		}
		sample, err := c.Sample(inv, time.Now())
		if err != nil {
			return err
		}
		if *output == "-" {
			err = encoder.Encode(sample)
		} else {
			err = history.Append(sample)
		}
		if err != nil {
			return fmt.Errorf("write sample: %w", err)
		}
	}
	return nil
}

func main() {
	runtime.GOMAXPROCS(1)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
