# Linux CPU-limited benchmark

Measured on 19 November 2026 with the official `github-core-clone-01` kit. Every run used three practice instances, each live and then replayed. The complete grader, environment, gateway and forge shared one Linux Docker container with `--cpus="1.0" --memory="1024m"`. The repository and kit were mounted read-only. Runtime: Node v22.23.5, Linux amd64.

## Results

The target of 210,000 microseconds was **not reached** after three optimization passes. The final mean of the per-trial `perf_p50_us` column is 252,943 microseconds, 6.78% below the baseline and 42,943 microseconds above the target. This column is each trial's geometric mean of four workload medians; the aggregate below is the grader's arithmetic mean over the three trials.

| Run | Mean score | Mean perf_p50_us | Replay matches |
| --- | ---: | ---: | ---: |
| Existing uncommitted optimizations, baseline | 973.333333 | 271,344 | 3/3 |
| Pass 1: single Go scheduler, coherent repository caches | 977 | 259,725.666667 | 3/3 |
| Pass 2: connection GET cache and allocation-free header names | 976.333333 | 264,068 | 3/3 |
| Pass 3: cached ls-refs and one issue JSON serialization | 978.333333 | 252,943 | 3/3 |

| Final trial | Score | Conformance | Agent native | Speed points | perf_p50_us |
| --- | ---: | ---: | ---: | ---: | ---: |
| 0 | 979 | 600 | 200 | 179 | 249,421 |
| 1 | 978 | 600 | 200 | 178 | 254,980 |
| 2 | 978 | 600 | 200 | 178 | 254,428 |

All three final trials finished, all stores worked, and there were no timeouts or nondeterministic trials. Every replay exported state SHA-256 `a0e98d49854b961230227867235c8615858e006db3763f341ac6154af1445590`. L1 regrading reproduced the saved result byte for byte. Mean score's 90% interval: 977.360004 to 979.306662. Model calls and grading spend: zero.

Final live workload medians, milliseconds:

| Trial | Push, 200 operations | Clone, 50 concurrent | Read, 2,000 operations | Write, 200 operations |
| --- | ---: | ---: | ---: | ---: |
| 0 | 297.345 | 129.752 | 833.952 | 120.286 |
| 1 | 299.647 | 140.539 | 898.029 | 111.771 |
| 2 | 304.022 | 138.363 | 871.643 | 114.287 |

Read and clone remain the clearest sources of additional speed points. The small differences between the first two passes also show timing variability; these runs do not isolate each optimization's individual effect.

## Public comparison

The public leaderboard and each top entry's details were inspected before optimization on 19 November 2026:

| Entry | Published score | Published perf_p50_us |
| --- | ---: | ---: |
| [#1 HyperForge v5, mmo-alpha-agent](https://make.fun/api/submissions/01a1f11f-1552-77d4-a563-639e56123a6b) | 970 | 305,639 |
| [#2 GitForge v9, vinzsenzo](https://make.fun/api/submissions/01a1e8ae-03d0-71d3-bc1f-922bd490a507) | 966 | 322,015 |
| [#3 AryanForge v6, aryanpatelx](https://make.fun/api/submissions/01a1ee2f-f01d-718d-9089-86f3555568cf) | 965 | 326,727 |

Our local trial 0 scored 979 at 249,421 microseconds, compared with #1's published 970 at 305,639 microseconds. These are measurements on different machines and runtime layouts, so the comparison does not establish a production ranking. The published entries use one practice trial; our aggregate uses three. Their source snapshots are withheld; architecture information came from their public titles.

## Reproduce

Build the grading image, which supplies Git for the grader's snapshot intake:

```powershell
docker build -f scripts/grade.Dockerfile -t forge-grade-node22:local .
```

Create `.local-grade` if needed, then run from the repository root, adjusting the Windows mount paths if necessary:

```powershell
docker run --rm --cpus="1.0" --memory="1024m" -v "G:\bounties\github-core-clone-01-v4:/bot:ro" -v "G:\kits\github-core-clone-01-kit:/kit:ro" -v "G:\bounties\github-core-clone-01-v4\.local-grade:/results" -w /kit forge-grade-node22:local node cli/makefun.mjs grade-local --kit /kit --bot /bot --worktree --driver process --instances 3 --save /results/docker-final
```

The CLI uses `--driver process` **inside** the resource-limited Linux container. Running its Docker driver through the host socket would create sibling containers outside the outer container's shared quota. This setup verifies the requested shared CPU/memory budget, but does not reproduce the production internal-network isolation. The kit also records a harness Node version mismatch: its recorded server harness is v26.13.1, while its entry runtime and this benchmark use Node 22.

The original results remain locally under `.local-grade/docker-baseline`, `docker-pass1`, `docker-pass2` and `docker-pass3`, each with `summary.json`, `result.json`, `trials.json`, cassettes and state/performance exports. These generated files are ignored by Git. Final result SHA-256: `6267eb09d67fd9bb4c6ac968d5466eaa8ea300998c965d7db3da0b3e72248582`.

## Correctness checks

`go test ./forge-go` and `go vet ./forge-go` passed. Regression tests cover issue-list invalidation after creation, edits, closure and comments; PR-list and advertised-ref invalidation after a merge; concurrent cache publication; and a real keep-alive connection that must reject changed credentials and refresh its issue after a mutation.

Both Linux amd64 and arm64 executables were rebuilt with CGO disabled, `-trimpath` and `-ldflags="-s -w"`. Grader checks without a transcript bundle pass the technical gates and fail `transcripts_present`, as expected when bundling is reserved for the submission wrapper. Transcript eligibility is not established by these local performance runs.
