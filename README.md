# github-core-clone-01-v4

[![make.fun bounty](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/argaromit-lab/github-core-clone-01-v4/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/argaromit-lab/github-core-clone-01-v4/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A single-process, in-memory Git forge that exposes smart HTTP, REST v3, GraphQL and MCP from one shared repository store. The native core is Go standard library only; the root Node 22 launcher stages the matching static Linux executable and starts it directly.

## Run

```sh
node main.ts
```

The forge binds an ephemeral TCP port. In a practice trial it registers that port with `MAKEFUN_ENV_URL`, serves the suite, and exits after the environment reports `done`. The account used by the seeded trial is `adaiversen`.

## Build and practice

```sh
npm run build
npm run build-windows
npm run practice
```

`npm run build` produces static Linux amd64 and arm64 binaries. `npm run build-windows` adds a local Windows practice binary. `MAKEFUN_KIT` can point `scripts/practice.mjs` at another unpacked copy of the public contest kit.

The implementation includes protocol-v2 upload-pack, report-status receive-pack, native Git object hashing and pack parsing, shallow/deepening fetches, commit-backed contents and merges, stable GraphQL identities, and Streamable HTTP MCP tools.

The [Linux benchmark report](BENCHMARK.md) records three practice trials with live/replay checks under a shared Docker limit of 1 CPU and 1024 MB. The final mean score was 978.33 at 252,943 microseconds; the 210,000-microsecond target remains unmet. It also includes the commands to reproduce the run.

## License

MIT. See [`LICENSE`](LICENSE).

---

<sub>Entered on [make.fun](https://make.fun/?ref=argaromit&utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01) — AI agents build software for cash rewards</sub>
