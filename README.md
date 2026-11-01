# Phlo Monitor

Host-side Linux cgroup v2 resource collection with Go's standard library.
The executable reads a supplied inventory and kernel files, then writes JSON
lines or terminal tables. It has no Docker client, network listener, database,
or subprocesses.
It is not installed automatically in Phlo deployments.

## Build and run

Requires Go 1.25 or later and a Linux host with a readable cgroup v2 hierarchy.
Run outside containers, in the host cgroup namespace. Rootless Docker works
when the chosen user can traverse the supplied paths and read their counters.
Docker Desktop and remote daemons need a collector on their Linux host.

```sh
CGO_ENABLED=0 go build -trimpath ./cmd/phlo-monitor
./phlo-monitor --inventory ./inventory.json --count 3
./phlo-monitor --inventory ./inventory.json --format table --count 3
./phlo-monitor --inventory ./inventory.json --output ./history/samples.jsonl
```

The first sample is immediate. The default interval is 15 seconds. `--count 0`
runs until SIGINT or SIGTERM. `--interval` accepts Go durations of at least one
second. `--root` defaults to `/sys/fs/cgroup` and must expose the complete host
hierarchy to include all ancestor limits. `--output -` writes to stdout without
retention. `--max-bytes` defaults to 67108864 bytes per history file.

`--format jsonl|table` selects stdout formatting. The default is `jsonl`, with
the unchanged version-1 schema below. `table` prints a timestamped table per
sample, without clearing the terminal. Columns show service, cgroup path, CPU
percentage, charged memory in MiB, and warnings. Memory rounds to two decimal
places. An unavailable CPU rate or memory value appears as `-`, not zero.
The first sample has no CPU rate. A measured idle interval appears as `0.0`.
Warnings list unavailable counter files. JSONL retains the detailed error text.
Control characters in service names and paths appear as escaped text.

Target rows follow inventory order. Inclusive ancestor measurements appear
once in a separate section and must not be added to target rows. Targets can
also overlap if the inventory explicitly includes a parent and its descendant;
do not sum overlapping target rows. Cgroup paths distinguish service replicas.

Table mode requires `--output -` and rejects a history file path before writing
anything. File output always remains bounded, rotated JSONL. To save a terminal
transcript, use shell redirection with `--format table`; that transcript has no
collector-managed size limit or rotation.

## Download and verify a released binary

Releases contain static Linux binaries only. Choose `amd64` for x86-64 or
`arm64` for AArch64. For a version such as `v0.1.0`, the three assets are:

- `phlo-monitor_v0.1.0_linux_amd64`
- `phlo-monitor_v0.1.0_linux_arm64`
- `phlo-monitor_v0.1.0_SHA256SUMS`

These are raw executables, not archives or installers. The example version
works only after a maintainer publishes it. On Linux with curl and GNU
coreutils, download the chosen asset and verify it before execution:

```sh
set -eu
version=v0.1.0
arch=amd64
asset="phlo-monitor_${version}_linux_${arch}"
sums="phlo-monitor_${version}_SHA256SUMS"
base="https://github.com/phlohouse/phlo-monitor/releases/download/$version"
curl --fail --location --output "$asset" "$base/$asset"
curl --fail --location --output "$sums" "$base/$sums"
sha256sum --strict --check --ignore-missing "$sums"
chmod 0755 "$asset"
test "$(./"$asset" --version)" = "phlo-monitor $version linux/$arch"
./"$asset" --version
```

The checksum command ignores the manifest entry for the architecture you did
not download. SHA-256 detects altered bytes, but the manifest is downloaded
from the same release and is not an independent signature. Trust the repository
and its release maintainers. No executable or service is installed automatically.

`--version` prints one line, for example `phlo-monitor v0.1.0 linux/amd64`,
and exits without reading inventory, cgroups, or history. Ordinary source builds
report `dev`. A future plugin can pin an exact release and platform, verify
the checksum, and compare this line before invoking the collector. It must also
respect the versioned inventory and JSONL contracts. There is no Python plugin
or automatic download/update behaviour in this repository.

## Publish a version as a maintainer

The manual **Release Linux binaries** workflow is the only release trigger.
Pushing a tag alone does not publish anything. The workflow must be dispatched
from `main`, and the supplied tag must already exist and point to a commit
reachable from current remote `main`. Merge reviewed changes and confirm green
CI before following this procedure. Do not run it to test packaging.

Only stable tags of the form `vMAJOR.MINOR.PATCH` are accepted, without leading
zeroes or prerelease/build suffixes. The tag is the version source of truth:
asset names and the executable version use exactly that tag. There is no
separate version file to keep in sync. Use a new patch version rather than
reusing or moving an existing published tag. Consumers should pin exact
versions, particularly before version 1.0.

From this repository, with GitHub CLI authenticated as a release maintainer:

```sh
git fetch origin main --tags
tag=v0.1.0
git tag -a "$tag" origin/main -m "Release $tag"
git push origin "refs/tags/$tag"
gh workflow run release.yml --ref main -f tag="$tag"
gh run list --workflow release.yml --limit 5
```

Use the displayed run ID with `gh run watch RUN_ID --exit-status`. After it
succeeds, use `gh release view "$tag"` and follow the download/checksum/version
verification above. Creating or dispatching a release is a deliberate publish
action, not part of normal CI or a feature-branch push.

The workflow tests the tagged source and builds with `CGO_ENABLED=0`. It checks
both architectures' Go build metadata, executes the matching host binary's
version command, and generates and verifies SHA-256 checksums. CI also exercises
this packaging path with the unpublished validation version `v0.0.0`.
Arm64 is cross-compiled, not executed on the amd64 runner.

Only the publish job has repository write permission. It rechecks that the tag
still resolves to the built commit and refuses any existing release, including
drafts. Per-tag concurrency serialises release attempts. It uploads into a draft,
downloads and checks the uploaded files, rechecks the tag, then publishes.
No upload uses an overwrite option. A failure after draft creation can leave
an unpublished draft. Inspect its assets and the run logs before deciding how
to recover; retries deliberately refuse that existing draft. Never delete or
replace a published release to make a retry pass. These workflow guards do not
prevent an administrator from manually changing tags or releases outside it.

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
bash scripts/package-release.sh v0.0.0 .amp/in/release-check
```

The packaging command runs on Linux with Bash, Go, and GNU coreutils. Its
output directory must not already exist. It builds both architectures, checks
native version output and both checksums, and never creates a tag or release.

Tests use disposable directories, not Docker or running workloads. Long-term
footprint, power-loss durability, production sandbox restrictions, and Phlo
inventory publication or API integration are not established by these tests.
