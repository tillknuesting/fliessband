# Fliessband: a software factory for Go gRPC microservices

Fliessband turns a short service spec (YAML) into a runnable, tested, deployable Go
gRPC microservice, sends it through a line of 21 stations, and measures what it
produced. A web UI shows the line running, the generated code with the origin of
every file, the measurements, and a history of runs to compare.

This document explains the approach, the research it rests on, and the
trade-offs. Version 0.2 added the parts that make the factory and its output
fit for production use; section 7 lists them.

## 1. The approach: a hybrid factory

| Approach | What writes code | Strengths | Weaknesses |
|---|---|---|---|
| Templates only | Deterministic templates | Same output every run, cheap, reviewed once | Business logic stays a stub |
| LLM only | A model, from a prompt | Writes business logic | Different every run, drifts from conventions, hard to trust or measure |
| **Hybrid** | Templates for structure, an LLM for marked regions, tools as judges | Most code is deterministic; the model gets a small, typed target; every result is verified | More moving parts |

Fliessband is a hybrid, and every recent account of software factories points the
same way: what matters is the harness around the model, and tools decide what
is good.

- **The contract comes first.** The spec becomes a `.proto` file; Go types,
  server interface and client come from `buf generate`.
- **Templates own the structure:** server, config, interceptors, TLS, health,
  storage (in memory and PostgreSQL), handlers, validation, tests, Dockerfile,
  Kubernetes manifests, CI.
- **The LLM owns two files.** Custom RPCs are stubs marked `fliessband:implement`
  in `internal/service/custom.go`. The model may rewrite that file and
  `custom_test.go`, nothing else; other paths in its answer are rejected.
- **The spec carries executable examples.** Methods list examples (seed
  records, a request, expected fields or an error code). Fliessband turns them into
  tests in a file the model cannot change. Examples marked `holdout` are also
  hidden from the model.
- **Tools are the judges.** A policy check, the compiler, vet, linters, the
  race detector, acceptance examples, mutation testing, fuzzing, a real
  PostgreSQL and a running binary decide whether a run passes. Gate failures in
  LLM-owned code go back to the model (at most 3 repairs).
- **Every file has a known origin.** `.fliessband/manifest.json` records whether a
  file came from a template, from `buf` or from the LLM, with its SHA-256. The
  provenance statement (section 6) carries the same digests.

## 2. The research behind the design

| Finding | Source | What Fliessband does with it |
|---|---|---|
| Agents special-case tests when they can see them: StrongDM saw agents return `true` to pass narrow tests and moved its end-to-end scenarios out of the codebase, as a holdout set | [StrongDM software factory](https://simonwillison.net/2026/Feb/7/software-factory/) (Feb 2026) | Spec examples become tests the model cannot edit; `holdout: true` examples are never shown to it, and their failures are not sent back in repair prompts |
| Frontier models exploit test cases on impossible tasks (GPT-5: 76% on one ImpossibleBench variant). Restricting test access cut cheating to near zero | [ImpossibleBench](https://arxiv.org/abs/2510.20270) (ICLR 2026) | The judge (`acceptance_test.go`) is excluded from the model's context and write set; the policy station rejects skipped tests and tests without assertions |
| The work shifts from writing code to designing the environment: structure, docs and feedback loops the agent can act on | [OpenAI, Harness engineering](https://openai.com/index/harness-engineering/) (Feb 2026) | Policy violations are written for the model ("import X is not allowed … Allowed: …"), and repair prompts carry exact tool output |
| Statement coverage is not strongly correlated with test effectiveness | Inozemtseva & Holmes, ICSE 2014 | Mutation score is worth 20 points of the quality score; coverage 10 |
| Mutation testing works at scale when it skips noise ("arid" code) and reports survivors as prompts, not a hard gate | Petrović et al., *Practical Mutation Testing at Scale*, IEEE TSE 2021 | Mutation is a check, not a gate; logging, lock and lifecycle calls and constant arithmetic are not mutated; survivors are listed with the exact line |
| LLMs can write tests that kill surviving mutants, and judge equivalent mutants; 73% of such tests were accepted by engineers | [Meta ACH](https://arxiv.org/abs/2501.12862) (FSE 2025) | When the LLM wrote code, its surviving mutants go back to it with a request for tests that kill them; tests are kept only if they pass on the real code and follow the policy; the model may name equivalent mutants |
| LLM equivalent-mutant detection beats traditional methods | Tian et al., ISSTA 2024 ([arXiv](https://arxiv.org/abs/2408.01760)) | Equivalent mutants named by the model are left out of the score and counted separately |
| Generating several solutions plus tests, then choosing the solution that agrees with the most independently generated tests ("dual execution agreement"), beats picking one sample | Chen et al., *CodeT*, ICLR 2023 ([arXiv](https://arxiv.org/abs/2207.10397)) | The implement tournament: each model's code runs against every other model's tests; agreement decides together with the visible examples |
| Separating the test author from the code author catches errors that self-written tests share with the code | Huang et al., *AgentCoder* ([arXiv](https://arxiv.org/abs/2312.13010)) | Cross-tests come from other models, which never saw the code they test; a model's own tests weigh least; losers' tests that pass on the winner are merged into its test file, kept only if the whole file passes |
| Choosing among candidates by the evaluation set biases that set | standard holdout practice | Holdout examples run with every candidate but never count towards the choice, so they stay an unbiased check of the winner |
| Self-repair helps, but its gains are bounded by feedback quality | Olausson et al., *Is Self-Repair a Silver Bullet for Code Generation?*, ICLR 2024 | At most 3 repairs, only when the failure is in files the model owns, each carrying the exact tool output; a reviewer's "bug" findings get one more repair, kept only if every gate (holdouts included) still passes and nothing that passed before fails |
| Property-based tests check that code matches its spec, not only examples | [Kiro: property-based testing](https://kiro.dev/blog/property-based-testing/) (Nov 2025) | Generated Go fuzz targets state properties (page tokens round-trip; validation never panics and always reports field violations); the fuzz station explores them |
| Go 1.25 to 1.27 bring container-aware `GOMAXPROCS`, `http.CrossOriginProtection`, `go fix` modernizers, Green Tea GC, a `uuid` package and a goroutine leak profile | Go [1.25](https://go.dev/doc/go1.25), [1.26](https://go.dev/doc/go1.26), [1.27](https://go.dev/doc/go1.27) release notes | Generated services target Go 1.27: UUIDv7 ids, a goroutine leak check in the service, server and store test packages, modernizer findings in the lint station; the factory uses `CrossOriginProtection` |
| Standard gRPC metrics: `grpc.server.call.duration` and friends | [gRFC A66 / gRPC OpenTelemetry metrics](https://grpc.io/docs/guides/opentelemetry-metrics/) | Roadmap: switch the optional telemetry from `otelgrpc` to the A66 plugin |

The fuzz station justified itself during development: it found that the
generated page-token decoder accepted tokens with embedded line breaks, because
Go's base64 decoder skips `\r` and `\n` even in strict mode. The template now
compares against the canonical encoding, and that input is a permanent seed.
Mutation testing likewise showed that the templates never tested values exactly
at a limit; the generated tests now include boundary cases.

## 3. Architecture

```
            spec.yaml
                │
   ┌────────────▼──────────────┐     HTTP + SSE      ┌──────────────┐
   │  fliessband (one Go binary)    │◄───────────────────►│  Web UI      │
   │  spec     validate        │  CSRF, host check,  │  spec editor │
   │  gen      templates       │  token auth, CSP    │  live line   │
   │  pipeline 21 stations     │                     │  measures    │
   │  policy   LLM file rules  │                     │  code, logs  │
   │  mutate   mutation tests  │                     │  history     │
   │  supply   SBOM, SLSA      │                     └──────────────┘
   │  llm      implement/repair/harden
   └────────────┬──────────────┘
                │ data/runs/<id>/ ── run.json, service/, sbom.cdx.json, provenance.intoto.json
                ▼
   child processes: scrubbed environment, own process group, timeouts, offline after deps
   Docker (optional): throwaway PostgreSQL per run
```

## 4. The line

| Phase | Station | Tool | Gate | Output |
|---|---|---|---|---|
| Contract | Spec | `internal/spec` | yes | Normalised spec, problems, examples |
| | Scaffold | templates, `go/format` | yes | Source tree and manifest |
| | Proto lint | `buf lint` STANDARD | yes | API style findings |
| | Compatibility | `buf breaking` against the last passing run of the service | yes | Breaking changes; a new major version (v2) skips the check |
| | Codegen | `buf generate` | yes | `*.pb.go` |
| Build | Implement | LLM (optional) | no | Custom RPCs and tests |
| | Policy | `internal/policy` | yes, repairable | Rule violations in LLM files |
| | Dependencies | `go mod tidy` | yes | `go.mod`; later stations run offline (`GOPROXY=off`) |
| | Build | `go build` | yes, repairable | Binary |
| | Vet | `go vet` | yes, repairable | Findings |
| | Lint | golangci-lint v2 (staticcheck, gosec, errcheck, errorlint, …), `go fix -diff` | no | Findings, modernizations |
| Verify | Test | `go test -race -cover`, goroutine leak check | yes, repairable | Results, coverage |
| | Acceptance | spec examples | yes, repairable (visible examples only) | Pass, fail or pending per example |
| | Mutation | `internal/mutate` + LLM hardening | no | Score, survivors |
| | Fuzz | `go test -fuzz`, 3 s per target | no | Inputs tried, failing inputs saved as seeds |
| | Vulnerabilities | `govulncheck` | no | Reachable known vulnerabilities |
| Run | Integration | PostgreSQL 17 in Docker | yes (skipped without Docker) | Store contract on a real database |
| | Smoke | the binary, against PostgreSQL when available | yes | Startup, p50/p99, RSS, graceful shutdown |
| Ship | Supply chain | `debug/buildinfo` | no | CycloneDX 1.6 SBOM, in-toto/SLSA provenance |
| | Measure | `internal/metrics` | always | Report and score |

A full run of the inventory example takes about 80 s; mutation testing is
about half of that (150 mutants, parallel `go test -overlay` runs).

## 5. The generated service

```
proto/…/v1/<name>.proto            AIP-style API: CRUD + custom RPCs
gen/…                              buf output
cmd/server/main.go                 config, store selection, TLS, signals, drain, graceful stop
internal/config                    env config with validation (all errors at once)
internal/server                    interceptors (recovery, logging, load shedding, deadlines),
                                   TLS 1.3 with certificate reload, health (liveness/readiness)
internal/service                   handlers, validation, field masks, keyset paging
internal/store                     Store interface; Memory; Postgres (pgx, migrations, CHECKs)
internal/service/acceptance_test.go  the spec's examples (owned by the factory)
internal/service/fuzz_test.go      properties as fuzz targets
deploy/k8s                         Deployment, Service, PodDisruptionBudget, kustomization
.github/workflows/ci.yml           buf lint/breaking, vet, race tests on PostgreSQL, golangci-lint, govulncheck, image
Dockerfile, .golangci.yml, Makefile, README.md
```

Choices:

- **Two stores, one contract.** `DATABASE_URL` selects PostgreSQL; without it
  the service uses the in-memory store and logs a warning. One contract test
  suite runs against both, so the fast store behaves like the real one.
- **PostgreSQL done properly.** pgx pool, embedded migrations under an advisory
  lock, `SELECT … FOR UPDATE` for read-modify-write, partial unique indexes,
  and CHECK constraints that mirror the spec's validation rules.
- **UUIDv7 ids and keyset paging.** Ids are time-ordered (RFC 9562), so id
  order is creation order, index locality is good, and page tokens (opaque,
  AIP-158) stay stable while records are added or removed.
- **Operability.** Liveness and readiness are separate health services;
  readiness follows the database. `DRAIN_DELAY` reports NOT_SERVING before
  stopping, `MAX_CONNECTION_AGE` lets load balancers rebalance long-lived
  HTTP/2 connections, `MAX_IN_FLIGHT` sheds load with RESOURCE_EXHAUSTED, and
  calls without a deadline get one.
- **Kubernetes-ready.** gRPC probes, non-root, read-only root file system, all
  capabilities dropped, seccomp, topology spread, PDB, and a CPU limit that Go
  1.25+ turns into `GOMAXPROCS`.

## 6. Factory security and operations

The factory runs code that an LLM partly wrote, on its own host. Defences:

| Threat | Defence |
|---|---|
| Generated code reads credentials | Child processes get an allowlisted environment: no API keys, tokens or cloud credentials (tested) |
| Generated code reaches the network or other programs | The policy rejects imports outside an allowlist (no `os`, `net`, `os/exec`, `unsafe`, cgo) before anything compiles; after the dependency stage Go runs with `GOPROXY=off` |
| Runaway tests or servers | Every command has a timeout and runs in its own process group, which is killed as a whole |
| The model edits its judge | Write set limited to two files; the acceptance tests are hidden from it |
| A web page drives the local factory (CSRF, DNS rebinding) | `http.CrossOriginProtection`, Host allowlist on loopback, strict CSP, no third-party assets |
| Exposure beyond localhost | `fliessband serve` refuses non-loopback addresses without `FLIESSBAND_TOKEN`; bearer token or HttpOnly SameSite cookie; slowed failed sign-ins |
| Registry credentials | Docker runs with an isolated client config; only public images are pulled |

Operations: `/healthz`, Prometheus `/metrics` (runs, stage durations and
failures, tokens), JSON logs, a bounded queue (429 with `Retry-After`), run
cancellation, graceful shutdown, recovery of runs interrupted by a crash,
retention (keeps the newest 200 runs and the newest passing run per service,
the compatibility baseline), and binaries deleted after each run.

Supply chain: each run writes a CycloneDX 1.6 SBOM read from the binary's
build information and an in-toto statement with SLSA provenance v1, covering
the binary and every generated file. It also records the spec digest, the
templates digest, the tool versions, and the digest of every LLM prompt. It is
unsigned; sign it in a release pipeline (Sigstore cosign, for example).

## 7. Measurements and the score

| Group | Metrics |
|---|---|
| Test effectiveness | Mutation score and test strength, survivors by file and operator, LLM hardening results |
| Spec conformance | Examples passed, failed, pending; holdouts; RPCs implemented |
| Size and origin | Lines by kind and by origin (template, buf, LLM) |
| Complexity | Cyclomatic and cognitive per function, Halstead, Maintainability Index |
| Tests | Counts, statement coverage (unit and integration merged), goroutine leaks |
| Findings | Policy, buf lint, vet, golangci-lint, fuzz failures, modernizations, vulnerabilities |
| Contract | Breaking changes against the baseline run |
| Runtime | Startup, p50/p90/p99, throughput, RSS, shutdown; against PostgreSQL when available |
| Supply chain | Components, toolchain, binary digest |
| Factory | Stage times, LLM calls, tokens (and cached tokens), repairs, rejected writes |

Quality score (0–100): mutation 20, coverage 10, acceptance 15, completeness
10, complexity 15, maintainability 5, findings 15, compatibility 10. Parts that
a run did not measure (no examples, no baseline, mutation off) are left out and
the total is scaled over the rest, so runs with different options stay
comparable. A failed gate caps the score at 40.

## 8. Not done yet

- protovalidate (v1.0 since September 2025) annotations, so clients in other
  languages see the same rules; today the Go validation and the CHECK
  constraints are both generated from the spec.
- The gRPC A66 OpenTelemetry metrics plugin instead of `otelgrpc`.
- AIP-154 etags and AIP-155 request ids for optimistic concurrency and
  idempotent creates.
- Container sandboxing for test runs (gVisor or similar) on shared factories.
- Signed provenance and SBOMs; image builds in the line.
