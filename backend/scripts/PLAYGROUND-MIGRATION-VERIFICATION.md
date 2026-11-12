# Playground batch/performance migration verification

Date: 2026-11-09. Target: `E:\Projects\sub2api`, branch `codex/ais`, base `04644d9c0`. Reference: clean checkout `D:\Projects\cockpit-tools1-private2` at `894219d`.

## Delivered scope

- Recoverable batch Google/Playground login with password/TOTP import, encrypted queue/screenshots, exclusive queue ownership, proxy pinning, manual takeover, expiry, cancellation and duplicate-save reconciliation.
- Browser admission by priority and Linux host/cgroup memory; background preparation yields to real recovery and busy profile leases.
- Lease-scoped native callbacks, bounded resident VM retention, lightweight hibernation, 8 MiB native VM threads, background prewarming and idle-ready scheduling preference.
- Full-admin batch/warmup dialogs, group selection, masked inputs, state polling guards, audit-body omission for credential-bearing imports and keyboard input.

Preserved: model-specific quota cooldowns, confirmed account-error persistence, confirmed-login Redis markers, configurable sliding Cookie cache TTL (30 minutes by default), two-second foreground profile wait, idle sticky routing, existing MCP/OAuth/image behavior and dependency versions. No database migration is required. No live account data was copied from the reference project.

## Verification

| Check | Result |
| --- | --- |
| Frontend batch/warmup, proxy parser, login inspector, account creation and locale regressions | 83 tests passed across six files |
| Frontend TypeScript and changed-file ESLint | Passed; production build also runs TypeScript checking |
| Frontend production bundle | Passed; existing Browserslist age and large-chunk warnings remain |
| Windows CGO=0 service, admin handler, middleware, routes and server cleanup regressions | Passed; routes compiled with no matching tests |
| Restart with expired credentials | Passed; expired password removed from the persisted queue before dispatch |
| Administrator revoked before worker dispatch | Passed; browser login and account creation never start |
| Linux CGO foundation tests, including real SpiderMonkey warmup/hibernation and recursion | Passed |
| Linux native Playground/Gemini compatibility, quota, account failure, batch and warmup tests | Passed |
| Native service tests shuffled with seed 42, repeated three times, network disabled | Passed |
| Race detector: native sessions/VM management | Passed |
| Race detector: browser capacity/priorities/cancellation | Passed |
| Race detector: batch queue and warmup service | Passed with native CGO enabled |
| Repository Playground credential/error compare-and-swap regressions | Passed in Linux CGO=0 |
| Real Windows Camoufox with a fresh temporary profile | Passed: durable Cookie after reopening, screenshot, keyboard input and account identity cases |
| Embedded Linux production backend | Passed with the final frontend bundle; `--version` executes with networking disabled; static executable confirmed |

Added migration boundary regressions cover stale list/frame responses, full-admin entry visibility, immediate background busy-lease return, retained foreground lock timeout, disabled/expired/removed required proxies, priority/load/platform scheduling preservation and protection of a profile claimed by a late database commit.

### Reproducible commands

Run backend commands from `backend/`. Linux native commands need the pinned SpiderMonkey headers/archive and Zig toolchain configured as in the project Dockerfile. Apply the static archive adjustment documented in `PLAYGROUND-CAMOUFOX.md`.

```sh
CGO_ENABLED=0 go test -p 1 -timeout 240s \
  ./internal/service ./internal/handler/admin ./internal/server/middleware \
  ./internal/server/routes ./cmd/server \
  -run 'Playground|ProvideCleanup|ScopedAdmin|CustomAdmin|AccountArchive|ImportAccount|ExportAccount' -count=1

CGO_ENABLED=0 go test -p 1 ./internal/repository -run Playground -count=1

go test -p 1 -timeout 240s \
  -ldflags="-linkmode external -extldflags '-static -Wl,-z,stack-size=8388608'" \
  ./internal/pkg/aistudio/... ./internal/pkg/camoufox/... -count=1

go test -p 1 -timeout 240s \
  -ldflags="-linkmode external -extldflags '-static -Wl,-z,stack-size=8388608'" \
  ./internal/service -run 'Playground|GeminiResponsesPlayground' -count=1

go test -race -p 1 -timeout 240s \
  -ldflags="-linkmode external -extldflags '-static -Wl,-z,stack-size=8388608'" \
  ./internal/pkg/aistudio/... ./internal/pkg/camoufox/... ./internal/service \
  -run 'Test(Manager|SessionWarmup|CanceledBackground|SpiderMonkey|BrowserCapacity|PlaygroundBatch|PlaygroundWarmup|PlaygroundBackground|PlaygroundBusy)' -count=1
```

Run frontend commands from `frontend/`:

```sh
pnpm exec vitest run \
  src/components/account/__tests__/PlaygroundBatchLogin.spec.ts \
  src/components/account/__tests__/PlaygroundWarmup.spec.ts \
  src/api/__tests__/playgroundBatchProxy.spec.ts \
  src/__tests__/google-login-inspector.spec.ts \
  src/components/account/__tests__/CreateAccountModal.spec.ts \
  src/i18n/__tests__/localeKeyCompleteness.spec.ts
pnpm run typecheck
pnpm run build
```

Browser smoke: set `CAMOUFOX_PATH` to the installed pinned executable and `CAMOUFOX_SMOKE=1`, then run `go test -v ./internal/pkg/camoufox -run '^TestBrowserPersistentProfileSmoke$' -count=1`. The test uses a new temporary profile and synthetic pages/Cookies, without signing into Google.

## Findings resolved during testing

1. The broad native suite exposed a crash on `os.Setenv` even in an isolated test before any VM was started. Symbolization identified the prebuilt Mozilla environment interposer trying `dlsym(RTLD_NEXT)` inside a static-musl binary. The Docker build now removes that archive object so musl handles the environment; `TestSpiderMonkeyStaticEnvironmentMutation` prevents recurrence.
2. Initial Windows queue tests correctly paused because the system disk had less than 512 MiB free. Tests were rerun with temporary files on D:. The Go build cache was preserved at `D:\CodexBuild\sub2api-go-build-cache-20261109`, with a junction at its original `C:\Users\PayAndHtr\AppData\Local\go-build` path, freeing approximately 9 GiB on C:.
3. Windows refused to execute `repository.test.exe` (access denied). The corresponding repository tests passed in Linux; no host security policy was changed.

## Practical limits

No real Google passwords/TOTP/accounts or upstream generation were exercised. Synthetic tests verify queue/state/proxy/identity invariants and reuse of a real native VM, not Google's live challenge acceptance or a measured production throughput gain. The 20-job concurrency test uses fake browsers; effective real concurrency is reduced by available memory. The browser smoke uses the actual installed Windows Camoufox, while native SpiderMonkey was verified on Linux amd64. No deployment or database/account mutation was performed.

Operation and persistence requirements are in [PLAYGROUND-CAMOUFOX.md](PLAYGROUND-CAMOUFOX.md). Detailed local logs and the resource-bounded verification Dockerfiles are in the ignored `.tmp/` directory.

Final local artifact: `D:\CodexBuild\sub2api-playground-artifacts-20261109\sub2api-linux-amd64`; image `sub2api:playground-migration-build-final` is a **build/test image**, not a deployed application container. Binary SHA-256: `302855b6aea8678ee909568ebf1ef7f9a1b0d9e2819680f1ce21df19b29ff25d`. The binary retains the current development version `0.2.20.4`; no release version was assigned.
