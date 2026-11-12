# Playground Camoufox sessions

Playground now accepts **Camoufox browser login only**. Existing Cookie and Chrome-RT accounts must open their edit form, complete browser login and save. There is no automatic conversion from a copied Cookie to a renewable profile.

The retired Chrome credential importer, manual Cookie-file parsers and disk session-export code have been removed. Redis stores exported sessions; the persistent Camoufox Profile is the recovery source.

## Runtime

The generation process still requires the matching SpiderMonkey build. Each VM owns a native thread with an 8 MiB stack. Camoufox is a separate process and is closed after login/capture, timeout or cancellation. Browser concurrency defaults to two and is configurable through batch login. Linux memory/cgroup headroom can lower the effective concurrency; foreground recovery and manual login take precedence over batch automation and speculative prewarming.

The Docker static-musl build removes the prebuilt library's `Unified_cpp_mozglue_interposers0.o`: its environment-variable wrappers use `dlsym(RTLD_NEXT)`, which cannot resolve libc in a static executable. Environment operations use musl instead. Custom static builds using this same SpiderMonkey archive must apply the same archive adjustment; verify with `TestSpiderMonkeyStaticEnvironmentMutation`.

Use Camoufox `152.0.4-beta.29`, matching the existing Firefox 152 fingerprint. The application validates `application.ini` and rejects mismatched versions. Set `CAMOUFOX_PATH` to an installed executable, or preinstall it:

```powershell
$env:DATA_DIR = 'E:\sub2api-data'
.\server-playground-windows-amd64.exe --install-playground-browser
```

The installer downloads the pinned upstream release into `DATA_DIR/runtime/camoufox`. `CAMOUFOX_DOWNLOAD_PROXY` optionally supplies the install command's download proxy. First browser login can also prepare the runtime, using the selected account proxy; preinstallation avoids a long first login request. Normal HTTP/SOCKS account proxies are reused through the existing local proxy relay.

## Storage and deployment

* **Redis:** exported cookies, localStorage and fingerprint, with an atomic **30-minute default sliding TTL**, configurable with `gateway.playground_session_cache_ttl_seconds`. Every request reads Redis and extends that TTL. Every received Set-Cookie is merged and written under the current lease. Redis errors fail the request; there is no fallback to old SQL cookies.
* **Disk:** each account has a persistent isolated Firefox Profile under `PLAYGROUND_PROFILE_DIR`, default `DATA_DIR/aistudio/profiles` (`data/aistudio/profiles` when DATA_DIR is unset). Docker uses `/app/data/aistudio/profiles/<profile UUID>`. The browser's own cookie database and login state necessarily remain inside this durable profile. Deleting this volume loses the ability to recover sessions.
* **PostgreSQL:** profile ID, verified email, source metadata, model mapping and account configuration. Reauthorization removes old Cookie/storage-state/Chrome OAuth credentials from the account.

Cache expiry means the configured interval **without a cache touch**, not a forced browser launch at that interval during continuous traffic. An authentication failure invalidates the cache immediately and permits one browser recovery/retry. Permission, quota and region errors do not trigger recovery. Only a recovery that still observes a Google sign-in or verification page when its time budget expires creates a separate Redis marker preventing repeated browser launches for 24 hours; reauthorizing creates a new profile and bypasses the marker immediately. Slow page loading, proxy/network failures and request cancellation remain retryable. Legacy markers written by older versions (which could mistake timeouts for logout) are cleared on the next request under the profile lease, allowing recovery again.

## Batch login and prewarming

Full administrators can open **Batch login** from the account page or the Playground account creation form. Import one `email----password----TOTP secret` per line; the TOTP field can be empty. Select eligible proxies and explicitly choose account groups (or opt into unassigned accounts). A proxy is mandatory. Each concurrent job reserves its own proxy, pins it after credential submission, verifies the captured email, and saves through the existing one-time login grant. CAPTCHA and verification challenges enter manual handling with screenshot and keyboard controls.

Set a fixed `TOTP_ENCRYPTION_KEY` before starting the service. Without an explicitly configured key, batch login is unavailable while the rest of the server continues to start. The encrypted queue and screenshots live in `DATA_DIR/aistudio/batches`; persist this directory together with profiles and retain the encryption key across restarts. Only one server process can own a batch directory. Route all batch administration requests to that process. Different batch-owner processes need separate queue directories. Shared Redis/profile leases continue to coordinate generation.

Queue changes survive restarts, interrupted jobs resume, and an uncertain database reply is reconciled before another account is created. Passwords/TOTP expire after 24 hours and are cleared after success or cancellation. Batch creation and keyboard-input request bodies are omitted from audit logs. Cancelling/expiring abandoned batch jobs queues profile cleanup; cleanup rechecks committed accounts before deleting a profile, including late successful account creation. Disk headroom below 512 MiB pauses new jobs.

The **Prewarming** dialog controls a default target of 8 ready sessions, 16 resident VMs and 30 minutes of idle retention. Settings are stored in Redis and shared across replicas; VM memory budgets and readiness are per process. Prewarming prepares browser state and WAA without calling generation, yields to foreground recovery, skips busy account leases immediately, and backs off after failures. Foreground requests retain the two-second profile-lock wait before account failover. Active responses are never evicted. Idle VMs can hibernate while retaining lightweight page state.

Warm idle Playground accounts are preferred only after the existing priority, availability, model/quota and load filters. Existing model-specific cooldowns and idle sticky-account behavior remain in effect. Accounts created by batch login require a valid bound proxy for generation and cannot fall back to a direct connection.

Generation forwards the account's actual `GetAiStudioBenefitTier` result in `X-AIStudio-G1-Tier`, matching AIStudio2API: Pro uses `TIER1`, Ultra uses `TIER2`, Plus uses `TIER0`, and Free omits the header. The tier is reloaded after session recovery.

OpenAI `/v1/responses` (including Codex), `/v1/chat/completions`, Claude Messages and native Gemini requests share the Playground generation gateway. Responses returns JSON or native Responses SSE events, including tool calls and terminal events. Codex custom tools and namespace tools are lowered to Gemini functions and restored on the return path. When built-in Google Search/code execution and function declarations coexist, the AI Studio encoder defaults `includeServerSideToolInvocations` to true, as required by Google; an explicitly supplied value is preserved.

Browser restoration verifies the same email before exporting state. It does not guarantee that Google will accept a saved login. MFA, CAPTCHA and expired Google login require interactive action in the account's browser window.

A confirmed `GenerateContent` permission denial (RPC code 7 in HTTP 403 or a stream trailer) marks the account as `error`, stores the tested model and Google's reason, and disables scheduling. This applies to account tests and forwarded requests. Network failures, quota limits, missing models, region errors and optional bootstrap RPC errors do not use this permission-error marker. The existing successful-account-test recovery flow can clear the error after the upstream issue is resolved. This account status is separate from the Redis browser-login-required marker.

Login completion reads AI Studio account data, with a fallback to visible Google account controls when the page does not expose its email in `WIZ_global_data`. Emails in chat/prompt text are never used as identity. Completion failures distinguish a page outside AI Studio (`not-ai-studio`), missing account email (`account-email-missing`), ambiguous account controls (`account-ambiguous`) and a missing session cookie (`session-cookie-missing`). Failed completion leaves the login browser open so the user can finish login and retry.

The browser is controlled by authenticated admin endpoints (screenshot, pointer, keyboard, finish/cancel). The view works for local and remote deployments, expires after ten minutes, and closes when the form is unmounted. Use normal authenticated HTTPS access for remote administration. A load balancer must keep an active login's requests on the server that owns the browser job. Generation across replicas requires the **same persistent profile volume** and shared Redis; per-profile Redis leases serialize access across replicas.

Deleting an account (including batch deletion) also removes its bound Profile directory and Redis session variants after the database deletion commits. Cleanup waits for the profile's active request lease. Failed database deletions preserve the profile. Cleanup intents in `profiles/.pending-deletions` survive restarts; interrupted/failed cleanup is retried on startup and every minute. Profiles from cancelled logins and previous reauthorizations are still retained. Profiles contain login material and must be restricted to the service user. Do not commit the data directory.

## Account ZIP backups

Account exports containing any Playground account automatically download a ZIP. Mixed selections include ordinary accounts in the same archive. Selections without Playground accounts continue to export JSON. The ZIP contains `accounts.json` and durable browser files under `profiles/<source profile UUID>/`; disposable caches and browser lock files are excluded. Export waits for the profile's active request lease and renews it while copying, so it does not copy a running recovery browser's database.

The existing account import dialog accepts JSON and ZIP files, including multiple files and drag/drop. ZIP import reuses or creates proxies through the usual flow, restores each Playground profile under a new UUID, and creates the account through a one-time proxy-bound login grant. Existing profiles are never overwritten. A failed account creation removes its newly restored profile. Old JSON files containing only Playground profile IDs cannot restore login state and report that a ZIP backup is required. Groups still need manual binding after import.

ZIP uploads are limited to 256 MiB compressed, 1 GiB uncompressed, 256 MiB per file, 20,000 entries and an 8 MiB account manifest. Unsafe paths, links, special files and duplicate/conflicting paths are rejected. Backups contain browser login material and are not encrypted. The destination must have the supported Camoufox runtime and Redis configured. Redis sessions are rebuilt from the restored profile on the first request; Google may still require interactive reauthorization.

## Docker

The standard image is Alpine/musl; the official Camoufox binary requires glibc. Build the dedicated target:

```sh
docker build --target camoufox -t sub2api:camoufox .
docker run --rm -v sub2api-data:/app/data sub2api:camoufox --install-playground-browser
```

Use that image in the existing Compose service, retaining its database/Redis settings and `/app/data` volume. The browser target uses Debian slim with PostgreSQL 18 client tools, GTK/Firefox libraries and the pinned Camoufox browser under `/app/runtime/camoufox`, outside the data mount. It omits the PostgreSQL server and unused macOS fonts while retaining Windows/Linux fonts for the Windows fingerprint and rendering fallback. Browser profiles remain in the mounted data directory. A separately installed runtime under `/app/data/runtime/camoufox` takes precedence over the bundled browser.

All supplied Compose variants mount `/app/data` (named volume or host `./data`), so `/app/data/aistudio/profiles` survives container restarts and recreation with the same volume. Do not remove that volume with `docker compose down -v` if you want to keep logins. If overriding `PLAYGROUND_PROFILE_DIR`, mount the overridden directory too. Installations that used the earlier `DATA_DIR/playground-profiles` default should move their existing UUID directories into `DATA_DIR/aistudio/profiles` while the service is stopped, or explicitly retain the old path with `PLAYGROUND_PROFILE_DIR`.

For multiple instances sharing PostgreSQL and Redis, separate `/app/data` mounts do **not** share browser profiles. Add the same persistent profiles mount to every instance's permanent Compose configuration, for example:

```yaml
volumes:
  - type: bind
    source: /srv/sub2api/shared/playground-profiles
    target: /app/data/aistudio/profiles
    bind:
      create_host_path: false
```

Prepare that source directory with the existing authorized profiles and service-user permissions before recreating containers. Instances on different hosts need shared storage at this path. New logins then write directly into the shared directory; copying profiles between instances after each account addition is unnecessary. With separate directories, an instance can create an empty profile for an account authorized elsewhere, and older versions can write a shared 24-hour login-required marker that blocks the healthy instance too. After repairing the mounts, retry recovery using the corrected login-marker handling described above.

## Verification

Unit/integration tests use synthetic sessions and miniredis to verify sliding expiry, cold recovery, cross-service serialization, stale-writer fencing, single-use login grants, authentication-only retries, legacy login-marker migration, and retries after transient recovery failures. Set `CAMOUFOX_SMOKE=1` to run the opt-in browser test; it uses offline data pages and a new synthetic profile, verifies screenshot capture, closes/reopens it, and checks a test cookie survives. It never reads your existing Chrome profiles or Google credentials.

Real Google acceptance needs a manual login followed by calls at 0/9/18 minutes, a gap longer than ten minutes, and a service restart with the profile volume preserved. Observe successful responses and browser process exit; synthetic tests alone cannot establish long-term Google session stability.

## Recovery diagnostics

Recovery emits `playground.session_recovery_started`, `playground.session_recovery_completed` or `playground.session_recovery_failed` with account/profile IDs, trigger (`cache_miss` or `upstream_authentication`) and elapsed milliseconds. The start event also reports whether an account proxy is configured. A failure's `requires_login=true` means a confirmed Google sign-in/verification page; temporary failures report `false`. Cookies and proxy credentials are not included by these events.

* `[profile-missing]`, `[profile-empty]` or `[profile-unreadable]`: verify `DATA_DIR`, `PLAYGROUND_PROFILE_DIR`, volume mounts and service-user permissions. Recovery requires an existing nonempty `cookies.sqlite`; it never creates an empty recovery profile. Migrating only the database/account JSON does not migrate the browser profile. Restore the account ZIP or the original profile volume.
* `[recovery-timeout:...]`: the browser did not finish loading a usable AI Studio session within the 90-second startup/recovery budget. Check proxy connectivity and server load, then retry. This does not set the 24-hour login marker.
* `[browser-login-required]`: recovery observed Google requesting sign-in/verification. Open the account edit form, complete browser authorization and save. A cookie's nominal expiry alone cannot prove Google still accepts it, especially after moving between server IPs.

For a server-only failure, compare the failed event with the preceding start event and inspect the same profile's mounted directory. Preserve the failure's reason and duration when collecting logs; omit cookie values, tokens and proxy passwords.
