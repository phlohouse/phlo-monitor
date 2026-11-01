// Package monitor reads Linux cgroup v2 counters without Docker or network access.
package monitor

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Inventory is a versioned, trusted mapping supplied by the deployment owner.
// Paths are relative to the collector's cgroup root, never host PIDs.
type Inventory struct {
	Version int      `json:"version"`
	Targets []Target `json:"targets"`
}

type Target struct {
	Service string `json:"service"`
	ID      string `json:"id"`
	Path    string `json:"path"`
}

func LoadInventory(r io.Reader) (Inventory, error) {
	var inv Inventory
	b, err := io.ReadAll(io.LimitReader(r, (1<<20)+1))
	if err != nil || len(b) > 1<<20 {
		return inv, errors.New("inventory unreadable or exceeds 1 MiB")
	}
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.DisallowUnknownFields()
	if err := d.Decode(&inv); err != nil {
		return inv, err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return inv, errors.New("inventory must contain one JSON object")
	}
	return inv, inv.Validate()
}

func (inv Inventory) Validate() error {
	if inv.Version != 1 || len(inv.Targets) == 0 || len(inv.Targets) > 256 {
		return errors.New("inventory version must be 1 with 1–256 targets")
	}
	ids, paths := map[string]bool{}, map[string]bool{}
	for _, t := range inv.Targets {
		if t.Service == "" || t.ID == "" || len(t.Path) > 4095 || strings.ContainsRune(t.Path, 0) || !filepath.IsLocal(t.Path) || filepath.Clean(t.Path) != t.Path || t.Path == "." {
			return errors.New("targets require service, id and a clean relative cgroup path")
		}
		if ids[t.ID] || paths[t.Path] {
			return errors.New("target IDs and paths must be unique")
		}
		ids[t.ID], paths[t.Path] = true, true
	}
	return nil
}

type CPULimit struct {
	Quota  *uint64 `json:"quota_usec"`
	Period uint64  `json:"period_usec"`
}

type PressureLine struct {
	Avg10     float64 `json:"avg10"`
	Avg60     float64 `json:"avg60"`
	Avg300    float64 `json:"avg300"`
	TotalUsec uint64  `json:"total_usec"`
}

type Pressure struct {
	Some PressureLine  `json:"some"`
	Full *PressureLine `json:"full,omitempty"`
}

type Group struct {
	Path          string              `json:"path"`
	CPU           map[string]uint64   `json:"cpu,omitempty"`
	CPUPercent    *float64            `json:"cpu_percent,omitempty"`
	CPUMax        *CPULimit           `json:"cpu_max,omitempty"`
	MemoryCurrent *uint64             `json:"memory_current_bytes,omitempty"`
	MemoryMax     *uint64             `json:"memory_max_bytes"`
	MemoryEvents  map[string]uint64   `json:"memory_events,omitempty"`
	Pressure      map[string]Pressure `json:"pressure,omitempty"`
	Errors        map[string]string   `json:"errors,omitempty"`
}

type Snapshot struct {
	Version int       `json:"version"`
	At      time.Time `json:"at"`
	Targets []Target  `json:"targets"`
	Groups  []Group   `json:"groups"`
}

type baseline struct {
	info  os.FileInfo
	id    string
	at    time.Time
	usage uint64
	cpu   map[string]uint64
}

// Collector owns a read-only filesystem root and rate baselines. It is used by
// one sampling loop. os.Root prevents inventory symlinks from escaping the root.
type Collector struct {
	root     *os.Root
	previous map[string]baseline
}

func NewCollector(path string) (*Collector, error) {
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	return &Collector{root: root, previous: map[string]baseline{}}, nil
}

func (c *Collector) Close() error { return c.root.Close() }

// Sample reads targets and deduplicated ancestors. The supplied clock should
// retain its monotonic component. Missing groups and files remain explicit gaps.
func (c *Collector) Sample(inv Inventory, at time.Time) (Snapshot, error) {
	if err := inv.Validate(); err != nil {
		return Snapshot{}, err
	}
	paths := map[string]string{}
	for _, t := range inv.Targets {
		paths[t.Path] = t.ID
		for p := filepath.Dir(t.Path); ; p = filepath.Dir(p) {
			if _, exists := paths[p]; !exists {
				paths[p] = ""
			}
			if p == "." {
				break
			}
		}
	}
	keys := make([]string, 0, len(paths))
	for p := range paths {
		keys = append(keys, p)
	}
	sort.Strings(keys)
	out := Snapshot{Version: 1, At: at.UTC(), Targets: inv.Targets, Groups: make([]Group, 0, len(keys))}
	next := map[string]baseline{}
	for _, p := range keys {
		g := Group{Path: p, Errors: map[string]string{}}
		dir, err := c.root.OpenRoot(p)
		if err != nil {
			g.Errors["cgroup"] = err.Error()
			out.Groups = append(out.Groups, g)
			continue
		}
		info, statErr := dir.Stat(".")
		g = readGroup(dir, p)
		current, err := c.root.Stat(p)
		if statErr != nil || err != nil || !os.SameFile(info, current) {
			g = Group{Path: p, Errors: map[string]string{"cgroup": "cgroup changed or disappeared during collection"}}
		}
		dir.Close()
		usage, exists := g.CPU["usage_usec"]
		if exists {
			old, found := c.previous[p]
			stable := found && os.SameFile(old.info, info) && old.id == paths[p] && at.After(old.at)
			for key, value := range old.cpu {
				v, ok := g.CPU[key]
				stable = stable && ok && v >= value
			}
			if stable {
				percent := float64(usage-old.usage) / at.Sub(old.at).Seconds() / 10000
				g.CPUPercent = &percent
			}
			next[p] = baseline{info: info, id: paths[p], at: at, usage: usage, cpu: g.CPU}
		}
		out.Groups = append(out.Groups, g)
	}
	c.previous = next
	return out, nil
}

func read(root *os.Root, path string) (string, error) {
	f, err := root.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, (64<<10)+1))
	if len(b) > 64<<10 {
		return "", errors.New("counter file exceeds 64 KiB")
	}
	return string(b), err
}

func readGroup(root *os.Root, path string) Group {
	g := Group{Path: path, Pressure: map[string]Pressure{}, Errors: map[string]string{}}
	read := func(name string, parse func(string) error) {
		s, err := read(root, name)
		if err == nil {
			err = parse(s)
		}
		if err != nil {
			g.Errors[name] = err.Error()
		}
	}
	read("cpu.stat", func(s string) (err error) {
		g.CPU, err = counters(s, "usage_usec", "user_usec", "system_usec")
		return
	})
	read("memory.events", func(s string) (err error) {
		g.MemoryEvents, err = counters(s, "low", "high", "max", "oom", "oom_kill")
		return
	})
	read("cpu.max", func(s string) error {
		f := strings.Fields(s)
		if len(f) != 2 {
			return errors.New("expected quota and period")
		}
		quota, err := limit(f[0])
		if err != nil {
			return err
		}
		period, err := strconv.ParseUint(f[1], 10, 64)
		if err == nil && period == 0 {
			err = errors.New("CPU period must be positive")
		}
		if err == nil {
			g.CPUMax = &CPULimit{Quota: quota, Period: period}
		}
		return err
	})
	read("memory.current", func(s string) error {
		n, err := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
		if err == nil {
			g.MemoryCurrent = &n
		}
		return err
	})
	read("memory.max", func(s string) (err error) { g.MemoryMax, err = limit(strings.TrimSpace(s)); return })
	for _, resource := range []string{"cpu", "memory", "io"} {
		read(resource+".pressure", func(s string) error {
			p, err := pressure(s)
			if err == nil {
				g.Pressure[resource] = p
			}
			return err
		})
	}
	return g
}

func limit(s string) (*uint64, error) {
	if s == "max" {
		return nil, nil
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return nil, err
	}
	return &n, err
}

func counters(s string, required ...string) (map[string]uint64, error) {
	out := map[string]uint64{}
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			return nil, errors.New("expected counter name and integer")
		}
		n, err := strconv.ParseUint(f[1], 10, 64)
		if err != nil {
			return nil, err
		}
		if _, exists := out[f[0]]; exists {
			return nil, errors.New("duplicate counter")
		}
		out[f[0]] = n
	}
	for _, key := range required {
		if _, ok := out[key]; !ok {
			return nil, fmt.Errorf("missing counter %s", key)
		}
	}
	return out, nil
}

func pressure(s string) (Pressure, error) {
	var p Pressure
	some := false
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		f := strings.Fields(line)
		if len(f) != 5 || (f[0] != "some" && f[0] != "full") {
			return p, errors.New("invalid PSI line")
		}
		var v PressureLine
		for i, key := range []string{"avg10", "avg60", "avg300", "total"} {
			name, value, ok := strings.Cut(f[i+1], "=")
			if !ok || name != key {
				return p, fmt.Errorf("expected PSI %s", key)
			}
			if key == "total" {
				n, err := strconv.ParseUint(value, 10, 64)
				if err != nil {
					return p, err
				}
				v.TotalUsec = n
			} else {
				n, err := strconv.ParseFloat(value, 64)
				if err != nil || math.IsNaN(n) || n < 0 || n > 100 {
					return p, errors.New("invalid PSI percentage")
				}
				*[]*float64{&v.Avg10, &v.Avg60, &v.Avg300}[i] = n
			}
		}
		if f[0] == "some" {
			if some {
				return p, errors.New("duplicate PSI some")
			}
			p.Some, some = v, true
		} else {
			if p.Full != nil {
				return p, errors.New("duplicate PSI full")
			}
			p.Full = &v
		}
	}
	if !some {
		return p, errors.New("PSI some is missing")
	}
	return p, nil
}
