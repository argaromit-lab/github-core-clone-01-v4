# Docker HTTP benchmark and memory regression

The previous local process-driver figures did not predict production behavior. The user reported that commit `f8a4932` scored 918, with `perf_p50_us` 625,003 and 118 speed points, while baseline `3b435a03bf42dc26750afcd6a3c18114462cf545` measured 426,075 microseconds on the server. Those server figures are user-provided; local measurements below are separate.

## Implementation

`forge-go/main.go` and `main.ts` have exactly the baseline commit's contents. The Go runtime uses its default GC, memory and scheduler policies: no `SetGCPercent`, `SetMemoryLimit`, fixed `GOMAXPROCS`, 64 KiB reader/scratch pools or per-connection response cache. The baseline transport already enables `TCP_NODELAY`, handles persistent HTTP/1.1 connections and writes replies immediately.

The two retained optimization areas are:

- Issue JSON: each issue's serialized body shares its immutable HTTP response backing bytes. Detail GETs read the per-repository concurrent index, and list GETs concatenate those already-serialized bodies. Each repository retains at most one list query variant, published through an atomic pointer; an issue mutation atomically replaces its detail response and invalidates the list under the repository write lock. Pagination, filtering, sort order, comments and authentication are preserved.
- Git pack pass-through: receive-pack validates and initially inflates incoming objects to verify their hashes and inspect commits/trees. It owns the request's compressed bytes and retains each non-delta entry's raw slice. Upload-pack hashes those slices and appends them directly to sideband frames, avoiding recompression, repeated decompression and the previous intermediate outgoing pack buffer. Delta objects and server-created objects still use the correct standard-library fallback encoder.

## Test environment

All new grades use the official CLI's **default Docker driver**, not `--driver process`. Docker 29.10 runs inside an outer Linux container with a shared 1 CPU/1024 MiB cgroup and no additional swap. The grader creates separate environment, gateway, agent and scorer containers with its own internal HTTP network. Their resource usage, the grader and the nested daemon all fall under the outer cgroup.

Verified outer limits: `cpu.max = 100000 100000`, `memory.max = 1073741824`, Docker `MemorySwap = 1073741824`. Storage backend: `vfs`, because nested overlay2 was unsupported on this host. The trial runtime is the kit's pinned image `node@sha256:de170b39439eb0e5a3444ad7d60b015614a381e10fecae2e30ec9e735122f38d`. The outer grader uses Node 22.23.5. The CLI notes that its recorded server harness uses Node 26.13.1.

One compatibility adjustment is explicit: the CLI mounts `/tmp` without `exec`, which made the unmodified baseline launcher fail with `spawn /tmp/forge-bin EACCES`. `scripts/grade-docker.mjs` adds `exec` to that tmpfs for both baseline and candidate. The contest explicitly permits staged static binaries. No evaluator, workload, score formula, CPU/memory limit or network rule is changed. Without this adjustment the default-driver baseline never starts and its zero score is not a performance measurement.

This Docker-in-Docker setup includes extra daemon/grader overhead and uses the kit's split-container layout; production entries report `one_pod`. Consequently, local timings are not a guarantee of the next official score.

## Measured baseline

The unmodified `3b435a0` snapshot was graded on all three practice instances, each live and then replayed, in `.local-grade/http-baseline-exec`:

| Trial | Score | Conformance | MCP | Speed | perf_p50_us |
| --- | ---: | ---: | ---: | ---: | ---: |
| 0 | 963 | 600 | 200 | 163 | 328,896 |
| 1 | 971 | 600 | 200 | 171 | 287,719 |
| 2 | 972 | 600 | 200 | 172 | 281,892 |
| Mean | 968.666667 | 600 | 200 | 168.666667 | 299,502.333333 |

All three replay exports matched and L1 regrading reproduced the result. Highest observed forge `VmHWM`: 18,176 KiB (17.75 MiB). The process's high-water RSS is sampled every 250 ms and includes every observed live and replay forge PID.

`perf_p50_us` is the geometric mean of each trial's four workload medians. The grader's aggregate is the arithmetic mean of those three trial values.

## Final candidate

The final application binaries were graded under `.local-grade/http-final` after removing duplicate retained issue JSON storage:

| Trial | Score | Conformance | MCP | Speed | perf_p50_us |
| --- | ---: | ---: | ---: | ---: | ---: |
| 0 | 971 | 600 | 200 | 171 | 300,785 |
| 1 | 964 | 600 | 200 | 164 | 319,499 |
| 2 | 965 | 600 | 200 | 165 | 311,279 |
| Mean | 966.666667 | 600 | 200 | 166.666667 | 310,521 |

All three live trials exited 0, all three replays matched and L1 regrading reproduced the result. All six forge processes stayed below 60 MiB; the highest observed high-water RSS was **17,920 KiB (17.5 MiB)**. The runner's conformance/MCP, replay and memory assertions passed. Final result SHA-256: `fcb1588fc26cb4f82bd839dce0fe8d8b1f67b0c3965c29f7ad18b8677ce4dea0`. Every live and replay state SHA-256 is `a0e98d49854b961230227867235c8615858e006db3763f341ac6154af1445590`.

| Final workload median, ms | Trial 0 | Trial 1 | Trial 2 |
| --- | ---: | ---: | ---: |
| Push, 200 operations | 413.302 | 351.920 | 340.518 |
| Clone, 50 concurrent | 179.151 | 179.391 | 183.487 |
| Read, 2,000 operations | 911.635 | 1,098.505 | 1,012.890 |
| Write, 200 operations | 121.260 | 150.256 | 148.352 |

The intermediate candidate, `.local-grade/http-candidate`, measured a mean 304,472 microseconds, score 968.666667 and peak forge RSS 17.75 MiB. The final 310,521 is 27.1% lower than the user-reported server baseline of 426,075, but **3.7% higher than the local baseline of 299,502.333333**. These runs establish low forge RSS and full correctness; they do not establish a speed improvement over the baseline on the same local setup or guarantee the next production score. The old 210,000-microsecond target is also unmet.

No OOM or OOM-kill event occurred. The outer cgroup's `memory.events max` counter increased during both baseline and candidate grading (39,405 for the baseline; 37,693 during the final run), while `high`, `oom` and `oom_kill` stayed zero. This includes the nested daemon and its image/filesystem cache, so the measurement does not establish zero memory reclaim for the aggregate sandbox, or prove the reported production regression's cause.

The public leaderboard was inspected before implementation and its top entry rechecked after testing. [#1 HyperForge v5](https://make.fun/api/submissions/01a1f11f-1552-77d4-a563-639e56123a6b) publishes score 970 / 305,639 microseconds; [#2 GitForge v9](https://make.fun/api/submissions/01a1e8ae-03d0-71d3-bc1f-922bd490a507) 966 / 322,015; [#3 AryanForge v6](https://make.fun/api/submissions/01a1ee2f-f01d-718d-9089-86f3555568cf) 965 / 326,727. Those are production trial-0 values; our final local trial 0 is 971 / 300,785. Different hosts, layouts and trial aggregates prevent a ranking claim.

## Reproduce

Build the nested grading image from the repository root:

```powershell
docker build -f scripts/grade.Dockerfile -t forge-grade-dind:local .
docker run --rm -d --privileged --name forge-http-grade --cpus="1.0" --memory="1024m" --memory-swap="1024m" -e DOCKER_TLS_CERTDIR= -v "G:\bounties\github-core-clone-01-v4:/bot:ro" -v "G:\kits\github-core-clone-01-kit:/kit:ro" -v "G:\bounties\github-core-clone-01-v4\.local-grade:/results" -w /kit forge-grade-dind:local dockerd --storage-driver=vfs --host=unix:///var/run/docker.sock --exec-opt=native.cgroupdriver=cgroupfs
```

After `docker exec forge-http-grade docker info` succeeds, run one grade at a time:

```powershell
docker exec forge-http-grade node /bot/scripts/grade-default.mjs --commit 3b435a03bf42dc26750afcd6a3c18114462cf545 --save /results/baseline
docker exec forge-http-grade node /bot/scripts/grade-default.mjs --worktree --save /results/candidate
docker stop forge-http-grade
```

The runner invokes `node /kit/cli/makefun.mjs grade-local --kit /kit --bot /bot --instances 3` plus the provided snapshot/save options, with **no driver argument**. This kit accepts `--bot`, not `--repo`. It saves the official summary, cassettes and state/performance exports, plus a sibling `.log` and `.resources.json`. The resource record includes per-process high-water RSS and cgroup memory events; its exit code also checks full conformance/MCP, matching replays and observed forge RSS below 60 MiB. Generated results stay in the ignored `.local-grade/` directory.

## Verification

`go test ./forge-go` and `go vet ./forge-go` pass. Tests exercise cache invalidation across issue changes, one live keep-alive socket with changing authorization and issue state, raw compressed entry retention despite request-buffer reuse, pack checksums, and sideband framing across object and 64 KiB boundaries. Both static Linux architectures are rebuilt with CGO disabled, `-trimpath` and `-ldflags="-s -w"`.

Transcript bundling remains the submission wrapper's responsibility; a check without the bundle cannot establish transcript eligibility.
