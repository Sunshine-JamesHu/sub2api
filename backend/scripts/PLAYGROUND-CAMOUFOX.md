# Playground Camoufox sessions

Playground now accepts **Camoufox browser login only**. Existing Cookie and Chrome-RT accounts must open their edit form, complete browser login and save. There is no automatic conversion from a copied Cookie to a renewable profile.

## Runtime

The generation process still requires the matching SpiderMonkey build. Camoufox is a separate process and is closed after login/capture, timeout or cancellation. At most two browsers are active in a server process; other recoveries wait with cancellation.

Use Camoufox `152.0.4-beta.29`, matching the existing Firefox 152 fingerprint. The application validates `application.ini` and rejects mismatched versions. Set `CAMOUFOX_PATH` to an installed executable, or preinstall it:

```powershell
$env:DATA_DIR = 'E:\sub2api-data'
.\server-playground-windows-amd64.exe --install-playground-browser
```

The installer downloads the pinned upstream release into `DATA_DIR/runtime/camoufox`. `CAMOUFOX_DOWNLOAD_PROXY` optionally supplies the install command's download proxy. First browser login can also prepare the runtime, using the selected account proxy; preinstallation avoids a long first login request. Normal HTTP/SOCKS account proxies are reused through the existing local proxy relay.

## Storage and deployment

* **Redis:** exported cookies, localStorage and fingerprint, with an atomic **five-minute sliding TTL**. Every request reads Redis and extends that TTL. Every received Set-Cookie is merged and written under the current lease. Redis errors fail the request; there is no fallback to old SQL cookies.
* **Disk:** each account has a persistent isolated Firefox Profile under `PLAYGROUND_PROFILE_DIR`, default `DATA_DIR/aistudio/profiles` (`data/aistudio/profiles` when DATA_DIR is unset). Docker uses `/app/data/aistudio/profiles/<profile UUID>`. The browser's own cookie database and login state necessarily remain inside this durable profile. Deleting this volume loses the ability to recover sessions.
* **PostgreSQL:** profile ID, verified email, source metadata, model mapping and account configuration. Reauthorization removes old Cookie/storage-state/Chrome OAuth credentials from the account.

Cache expiry means five minutes **without a cache touch**, not a forced browser launch every five minutes during continuous traffic. An authentication failure invalidates the cache immediately and permits one browser recovery/retry. Permission, quota and region errors do not trigger recovery. If Google requires login, a separate Redis marker prevents repeated browser launches for that profile for 24 hours; reauthorizing creates a new profile and bypasses the marker immediately.

Browser restoration verifies the same email before exporting state. It does not guarantee that Google will accept a saved login. MFA, CAPTCHA and expired Google login require interactive action in the account's browser window.

Login completion reads AI Studio account data, with a fallback to visible Google account controls when the page does not expose its email in `WIZ_global_data`. Emails in chat/prompt text are never used as identity. Completion failures distinguish a page outside AI Studio (`not-ai-studio`), missing account email (`account-email-missing`), ambiguous account controls (`account-ambiguous`) and a missing session cookie (`session-cookie-missing`). Failed completion leaves the login browser open so the user can finish login and retry.

The browser is controlled by authenticated admin endpoints (screenshot, pointer, keyboard, finish/cancel). The view works for local and remote deployments, expires after ten minutes, and closes when the form is unmounted. Use normal authenticated HTTPS access for remote administration. A load balancer must keep an active login's requests on the server that owns the browser job. Generation across replicas requires the **same persistent profile volume** and shared Redis; per-profile Redis leases serialize access across replicas.

Deleting an account (including batch deletion) also removes its bound Profile directory and Redis session variants after the database deletion commits. Cleanup waits for the profile's active request lease. Failed database deletions preserve the profile. Cleanup intents in `profiles/.pending-deletions` survive restarts; interrupted/failed cleanup is retried on startup and every minute. Profiles from cancelled logins and previous reauthorizations are still retained. Profiles contain login material and must be restricted to the service user. Do not commit the data directory.

## Docker

The standard image is Alpine/musl; the official Camoufox binary requires glibc. Build the dedicated target:

```sh
docker build --target camoufox -t sub2api:camoufox .
docker run --rm -v sub2api-data:/app/data sub2api:camoufox --install-playground-browser
```

Use that image in the existing Compose service, retaining its database/Redis settings and `/app/data` volume. The browser target includes PostgreSQL 18 client tools and GTK/Firefox libraries. Browser profiles and installed runtime live in the mounted data directory.

All supplied Compose variants mount `/app/data` (named volume or host `./data`), so `/app/data/aistudio/profiles` survives container restarts and recreation with the same volume. Do not remove that volume with `docker compose down -v` if you want to keep logins. If overriding `PLAYGROUND_PROFILE_DIR`, mount the overridden directory too. Installations that used the earlier `DATA_DIR/playground-profiles` default should move their existing UUID directories into `DATA_DIR/aistudio/profiles` while the service is stopped, or explicitly retain the old path with `PLAYGROUND_PROFILE_DIR`.

## Verification

Unit/integration tests use synthetic sessions and miniredis to verify sliding expiry, cold recovery, cross-service serialization, stale-writer fencing, single-use login grants, and authentication-only retries. Set `CAMOUFOX_SMOKE=1` to run the opt-in browser test; it creates a new synthetic profile, verifies screenshot capture, closes/reopens it, and checks a test cookie survives. It never reads your existing Chrome profiles or Google credentials.

Real Google acceptance needs a manual login followed by calls at 0/4/8 minutes, a gap longer than five minutes, and a service restart with the profile volume preserved. Observe successful responses and browser process exit; synthetic tests alone cannot establish long-term Google session stability.
