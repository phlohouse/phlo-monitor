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
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	monitor "github.com/phlohouse/phlo-monitor"
)

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("phlo-monitor", flag.ContinueOnError)
	flags.SetOutput(stderr)
	inventory := flags.String("inventory", "", "trusted Phlo inventory JSON file (required)")
	root := flags.String("root", "/sys/fs/cgroup", "host cgroup v2 filesystem root")
	output := flags.String("output", "-", "history file, or - for standard output")
	format := flags.String("format", "jsonl", "stdout format: jsonl or table (table requires --output -)")
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
	if *format != "jsonl" && *format != "table" {
		return errors.New("format must be jsonl or table")
	}
	if *format == "table" && *output != "-" {
		return errors.New("table format requires --output -; history files always use jsonl")
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
		if *format == "table" {
			err = writeTable(stdout, sample)
		} else if *output == "-" {
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

func writeTable(out io.Writer, sample monitor.Snapshot) error {
	var b strings.Builder
	fmt.Fprintf(&b, "Sample %s\nTargets (per cgroup; do not sum overlapping targets)\n", sample.At.Format(time.RFC3339Nano))
	w := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "SERVICE\tCGROUP\tCPU %\tMEMORY (MiB)\tWARNINGS")
	groups := make(map[string]monitor.Group, len(sample.Groups))
	targets := make(map[string]bool, len(sample.Targets))
	label := func(s string) string {
		quoted := strconv.QuoteToGraphic(s)
		return quoted[1 : len(quoted)-1]
	}
	row := func(service string, g monitor.Group) {
		cpu, memory := "-", "-"
		var warnings []string
		if g.CPUPercent != nil {
			cpu = fmt.Sprintf("%.1f", *g.CPUPercent)
		} else {
			warnings = append(warnings, "CPU rate unavailable")
		}
		if g.MemoryCurrent != nil {
			memory = fmt.Sprintf("%.2f", float64(*g.MemoryCurrent)/(1<<20))
		}
		files := make([]string, 0, len(g.Errors))
		for name := range g.Errors {
			files = append(files, name)
		}
		sort.Strings(files)
		if len(files) > 0 {
			warnings = append(warnings, "unavailable: "+strings.Join(files, ", "))
		}
		warning := strings.Join(warnings, "; ")
		if warning == "" {
			warning = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", label(service), label(g.Path), cpu, memory, warning)
	}
	for _, g := range sample.Groups {
		groups[g.Path] = g
	}
	for _, t := range sample.Targets {
		targets[t.Path] = true
		row(t.Service, groups[t.Path])
	}
	w.Flush()
	fmt.Fprintln(&b, "\nAncestors (inclusive totals; do not add to target rows)")
	fmt.Fprintln(w, "SERVICE\tCGROUP\tCPU %\tMEMORY (MiB)\tWARNINGS")
	for _, g := range sample.Groups {
		if !targets[g.Path] {
			row("-", g)
		}
	}
	w.Flush()
	fmt.Fprintln(&b)
	_, err := io.WriteString(out, b.String())
	return err
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
