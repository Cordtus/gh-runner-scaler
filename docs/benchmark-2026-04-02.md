# Storage Benchmark Results (2026-04-02)

Host: 192.168.0.170 (nodev2)

## Storage Layout

| Pool | Devices | Type | Capacity | Used |
|------|---------|------|----------|------|
| cstor | 4x Samsung SSD 870 2TB | ZFS RAIDZ1 | 7.27T | 27% |
| pool7 | 1x Samsung SSD 990 EVO Plus 2TB | ZFS single | 1.81T | 31% |
| pool9 | 1x Samsung SSD 990 EVO Plus 2TB | ZFS single | 1.81T | 7% |

OS/boot on a separate 500GB Kingston NVMe (nvme1n1).

## LXC Copy (Scale-Up Operation)

Same-pool copies use ZFS clone (metadata-only, near-instant).
Cross-pool copies require full data transfer (~6.5GB per container).

| Operation | Time |
|-----------|------|
| cstor -> cstor (same pool) | **0.38s** |
| pool7 -> pool7 (same pool) | 0.77s |
| pool9 -> pool9 (same pool) | 0.66s |
| cstor -> pool7 (cross-pool) | 12.9s |
| cstor -> pool9 (cross-pool) | 14.6s |

### Concurrent Same-Pool Copies (3x parallel)

| Pool | Time |
|------|------|
| cstor (RAIDZ1) | **0.85s** |
| pool7 (NVMe) | 1.78s |

## Sequential I/O (direct=1, 1M blocks, iodepth=32)

| Pool | Write | Read |
|------|-------|------|
| cstor | 3,516 MiB/s (3.7 GB/s) | 4,040 MiB/s (4.2 GB/s) |
| pool7 | 4,403 MiB/s (4.6 GB/s) | 4,106 MiB/s (4.3 GB/s) |
| pool9 | 4,323 MiB/s (4.5 GB/s) | 4,228 MiB/s (4.4 GB/s) |

NVMe ~25% faster on sequential write. Read is comparable.

## Random 4K IOPS (direct=1, iodepth=32)

| Pool | Read IOPS | Write IOPS |
|------|-----------|------------|
| cstor | 76.3K | 66.9K |
| pool7 | 65.2K | 62.2K |
| pool9 | 62.1K | 68.5K |

### Mixed Random 4K (70/30 R/W)

| Pool | Read IOPS | Write IOPS |
|------|-----------|------------|
| cstor | 75.3K | 32.3K |
| pool7 | 67.2K | 28.8K |
| pool9 | 78.3K | 33.6K |

All pools comparable. RAIDZ1 benefits from striped reads across 4 devices.

## Concurrent Cross-Pool I/O

All three pools running mixed 70/30 r/w simultaneously (4 jobs each):

| Pool | Read IOPS | Write IOPS |
|------|-----------|------------|
| cstor | 142K | 61.0K |
| pool7 | 142K | 61.1K |
| pool9 | 156K | 66.9K |

No significant cross-pool contention -- each pool operates on independent physical devices.

## Conclusions

1. **Template must stay on cstor.** Same-pool ZFS clone (0.38s) is 30-40x faster than cross-pool copy (13-15s). This is the critical path for scale-up latency.
2. **NVMe pools offer no advantage for runner clones** when the template lives on the same pool.
3. **Random 4K IOPS are equivalent** across all pools -- ZFS ARC and write coalescing level the playing field.
4. **Cross-pool I/O doesn't contend** -- pools can be used independently for different workloads without degradation.
5. **Potential use for NVMe pools:** persistent build cache volumes or dedicated workloads that benefit from sequential write throughput.
