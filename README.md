# Phlo Monitor

Host-side Linux cgroup v2 resource collection with Go's standard library.
The executable reads a supplied inventory and kernel files, then writes JSON
lines. It has no Docker client, network listener, database, or subprocesses.
It is not installed automatically in Phlo deployments.

## Build and run

Requires Go 1.25 or later and a Linux host with a readable cgroup v2 hierarchy.
Run outside containers, in the host cgroup namespace. Rootless Docker works
when the chosen user can traverse the supplied paths and read their counters.
Docker Desktop and remote daemons need a collector on their Linux host.

```sh
CGO_ENABLED=0 go build -trimpath ./cmd/phlo-monitor
./phlo-monitor --inventory ./inventory.json --count 3
./phlo-monitor --inventory ./inventory.json --output ./history/samples.jsonl
```

The first sample is immediate. The default interval is 15 seconds. `--count 0`
runs until SIGINT or SIGTERM. `--interval` accepts Go durations of at least one
second. `--root` defaults to `/sys/fs/cgroup` and must expose the complete host
hierarchy to include all ancestor limits. `--output -` writes to stdout without
retention. `--max-bytes` defaults to 67108864 bytes per history file.

## Inventory contract, version 1

```json
{
  "version": 1,
  "targets": [
    {
      "service": "trino",
      "id": "full-container-id",
      "path": "user.slice/user-1001.slice/user@1001.service/user.slice/docker-full-container-id.scope"
    }
  ]
}
```

Phlo or another deployment owner supplies this file. The collector does not
discover containers. The publisher resolves actual host cgroup membership,
uses a full container ID, and replaces the inventory atomically after changes.
Paths are clean, relative to `--root`, and cannot be `.` or contain `..`
components. IDs and paths are unique. Service names may repeat for replicas.
There must be 1 to 256 targets. Unknown fields, unsupported versions, trailing
JSON, and files larger than 1 MiB are rejected.

The CLI reloads the file before every sample. An invalid or unreadable refresh
exits nonzero rather than continuing with stale identity. A missing target
instead produces a sample with an explicit gap. An externally recreated
container remains untracked until its publisher refreshes the inventory.

## Sample schema, version 1

Each line is a JSON object with `version`, `at`, `targets`, and `groups`.
`at` is the UTC sweep start time in RFC 3339 format. `targets` repeats the
inventory mapping. `groups` contains each target and every ancestor once,
sorted by path. `.` represents the configured root. Each group has these fields:

| Field | Meaning |
| --- | --- |
| `path` | Relative cgroup path |
| `cpu` | Raw integer `cpu.stat` keys, including usage, user, system, and available throttle counters |
| `cpu_percent` | Interval usage, where one fully used CPU is 100%; absent without a valid baseline |
| `cpu_max` | `quota_usec` and `period_usec`; a null quota means unlimited at this group |
| `memory_current_bytes` | Charged memory, including cache, not process RSS |
| `memory_max_bytes` | Integer limit, or null for unlimited or unavailable; consult `errors` to distinguish |
| `memory_events` | Raw integer event counters |
| `pressure` | Available `cpu`, `memory`, and `io` PSI, each with `some` and optional `full` |
| `errors` | File name to error text; `cgroup` means the directory is missing, inaccessible, or changed during the sweep |

PSI lines contain `avg10`, `avg60`, and `avg300` percentages and integer
`total_usec`. Unavailable or malformed files produce errors, not zero values.
CPU requires `usage_usec`, `user_usec`, and `system_usec`. Memory events require
`low`, `high`, `max`, `oom`, and `oom_kill`. Optional newer counters are retained.
Individual files are limited to 64 KiB. JSON consumers must preserve unsigned
64-bit integers rather than decoding all counters through floating point.

CPU percent is delta usage in microseconds divided by monotonic elapsed
seconds and by 10000. Restarting the collector, changing the container ID or
cgroup inode, losing a CPU observation, or rolling back a CPU counter resets
the baseline. The first valid observation after a reset has no rate. Reads
use a pinned directory and discard the group if its path changes identity
during collection. Kernel files are not an atomic multi-counter snapshot.

Ancestor usage already includes descendants. Do not sum parent and child
usage. An unlimited leaf can still share a finite ancestor CPU or memory cap
with its siblings. The narrowest ancestor quota is an upper bound, not a
guaranteed entitlement. CPU percent is not quota-normalised. Throttled duration
is not a wall-clock utilisation percentage, and PSI measures stalls rather
than usage. There is no job-level attribution or effective-capacity calculation.

## Retention and failures

File output keeps `samples.jsonl` and one older `samples.jsonl.1`. Each file
stays within `--max-bytes` for a new history with an unchanged limit. A record
larger than that limit fails before modifying history. Rotation replaces the
old `.1` file, so readers process `.1` before the active file. Readers must
tolerate rotation and ignore an unterminated last line while a writer runs.
Retention duration depends on target count, path lengths, and errors.

Use one writer and a private output directory. New directories use mode 0700
and new files use 0600. Existing permissions are not changed. Existing output
must be a regular file, not a symlink. The parent directory must not be writable
by an adversary; output path checks are not a filesystem sandbox.

Before appending, the writer truncates an unterminated crash tail back to the
last newline. Failed writes attempt to truncate back to the pre-write size.
Open, rotation, write, truncation, and close errors propagate to stderr and
exit nonzero. There is no silent drop or storage retry loop. No per-sample
fsync is performed. Power loss can lose recent records or rotation metadata;
tail recovery is not a power-loss durability guarantee or corruption repair.

## Optional restricted host service

This is an administrator-adapted example, not an installer or a tested security
boundary. Create a dedicated unprivileged `phlo-monitor` account with no Docker
group membership. Put the executable at `/usr/local/bin/phlo-monitor`. Publish
the inventory at `/etc/phlo-monitor/inventory.json` with directory traversal and
file-read permissions for that account, but no collector write permission.
Do not reuse a deployment user's broad socket authority.

```ini
[Unit]
Description=Phlo cgroup resource collector
After=local-fs.target

[Service]
User=phlo-monitor
Group=phlo-monitor
ExecStart=/usr/local/bin/phlo-monitor --inventory /etc/phlo-monitor/inventory.json --output /var/lib/phlo-monitor/samples.jsonl
StateDirectory=phlo-monitor
StateDirectoryMode=0700
UMask=0077
NoNewPrivileges=yes
CapabilityBoundingSet=
ProtectSystem=strict
ProtectHome=yes
ProtectControlGroups=yes
PrivateTmp=yes
PrivateDevices=yes
PrivateNetwork=yes
RestrictAddressFamilies=none
SystemCallFilter=~@network-io
RestrictNamespaces=yes
Nice=19
CPUQuota=5%
MemoryMax=32M
TasksMax=16
Restart=no

[Install]
WantedBy=multi-user.target
```

The example needs system systemd and its namespace and syscall-filter support.
Check readable cgroup paths and denied socket access under the actual service
identity before enabling it. `ProtectControlGroups` makes cgroups read-only;
it must not hide ancestor files. The collector needs no capabilities or cgroup
delegation. Merely avoiding Docker calls does not revoke a user's existing
socket access. `os.Root` confines inventory reads against symlink escapes, but
does not protect against privileged mount changes or grant a process sandbox.

## Development checks

```sh
test -z "$(gofmt -l .)"
go vet ./...
go test -race -cover ./...
CGO_ENABLED=0 go build -trimpath ./cmd/phlo-monitor
```

Tests use disposable directories, not Docker or running workloads. Long-term
footprint, power-loss durability, production sandbox restrictions, and Phlo
inventory publication or API integration are not established by these tests.
