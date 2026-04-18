# YettiQanot driver HTTP API (parity with driver bot)

**Audience:** Backend team and native/Flutter client authors.  
**Contract source of truth:** [`docs/DRIVER_CLIENT.md`](DRIVER_CLIENT.md) and [`docs/AUTH.md`](AUTH.md).

**Goal:** Expose a single public driver HTTP + WebSocket surface on the Go service. Do **not** require the Flutter app to call `/mini/...`, `/api/v1/...`, or undocumented paths for core driver login, dispatch, or trip flows.

---

## Base URL

`API_BASE_URL` = `https://<host>` with **no trailing slash**.

---

## Auth (every driver request)

- **Primary (native):** header `X-Driver-Id: <digits only>` — internal `users.id` or Telegram user id; driver must be **approved** in DB.
- **Optional (Mini App / WebView):** header `X-Telegram-Init-Data: <initData>` (and/or query `init_data` where documented for WebSocket).
- **`Content-Type: application/json`** on POST bodies.
- **Never** log full init data or secrets in app or server logs at info level.

---

## Routes (driver bot parity)

| Method | Path | Notes |
|--------|------|--------|
| `GET` | `/health` | Optional; plain `OK` is fine. |
| `GET` | `/driver/available-requests` | Poll for offers + `assigned_trip`; merge queue aliases; dedupe by `request_id`. |
| `POST` | `/driver/accept-request` | Body: `{"request_id":"..."}` and/or `{"trip_id":"..."}`; **409** on conflict. |
| `POST` | `/driver/location` | Body: `lat`, `lng`, optional `accuracy` (number), optional `timestamp` (Unix seconds, int — GPS fix). Do **not** require ISO timestamps. Success **200** with e.g. `{"ok":true}`. Server uses **UTC wall clock** for `last_seen_at` / `last_live_location_at` / `live_location_active`; optional client timestamp is accepted but **must not** block row updates / freshness (GPS lag). Accuracy **> 50** m may skip trip polyline / WS only, not dispatch freshness (see `DRIVER_CLIENT.md`). |
| `GET` | `/trip/:id` | `:id` = trip UUID; full trip for map/UI. |
| `POST` | `/trip/arrived` | Body: `{"trip_id":"..."}` only. |
| `POST` | `/trip/start` | Body: `{"trip_id":"..."}` only. |
| `POST` | `/trip/finish` | Body: `{"trip_id":"..."}` only. |
| `POST` | `/trip/cancel/driver` | Body: `{"trip_id":"..."}` — driver cancel. |
| `GET` | `/legal/active` | When **403** `LEGAL_ACCEPTANCE_REQUIRED`. |
| `POST` | `/legal/accept` | Accept active legal docs (JSON per server schema). |
| `GET` | `/driver/promo-program` | Promo / program JSON. |
| `GET` | `/driver/referral-status` | Referral JSON. |
| `GET` | `/driver/referral-link` | Referral link (string or JSON with link / url). |

### CORS (web clients)

Allow headers including `Content-Type`, `X-Driver-Id`, `X-Telegram-Init-Data`, and `Authorization` if used. Today `internal/server/server.go` sets `Access-Control-Allow-Origin: *` and the methods/headers above; tighten origins only if you introduce credentialed cross-origin requests.

---

## WebSocket (active trip)

- **URL:** `wss://<same-host>/ws?trip_id=<trip_uuid>` (or documented override).
- **Auth before upgrade:** same as HTTP — prefer `X-Telegram-Init-Data` or query `init_data` in Telegram; otherwise `X-Driver-Id` when header mode is enabled. Only **assigned driver** or **rider** for that trip may connect.
- **Messages:** JSON with `type`, `trip_id`, `trip_status`, `emitted_at`, `payload` — emit `trip_started`, `trip_arrived`, `trip_finished`, `trip_cancelled`, `driver_location_update`; ignore unknown `type` on the client.

---

## Not primary driver API

- `/rider/...`, `POST /trip/cancel/rider` — rider client.
- `/admin/...`, generic `/api/...` — admin/dashboard; not for normal driver app unless you build a separate admin client.

---

## Server flags (ops / QA)

| Flag | Notes |
|------|--------|
| **`ENABLE_DRIVER_HTTP_LIVE_LOCATION`** | Server-only; **default on** (HTTP `POST /driver/location` refreshes `last_live_location_at` / `live_location_active` like Telegram live). **`=false`** = opt-out (Telegram-only live for HTTP pings). Clients do **not** need a compile-time flag; they keep posting while ONLINE when GPS + auth exist. |
| **`DISPATCH_DEBUG`** | Server-side only for investigating matching. |

---

## Dispatch eligibility (document for support)

- ~**90s** freshness on live location (see code / `DRIVER_CLIENT.md`).
- **Approved** driver, **legal** accepted, **balance** rules (unless “infinite”), **profile** fields per docs.
- **403** `LEGAL_ACCEPTANCE_REQUIRED` on location or other routes → client runs `GET /legal/active` + `POST /legal/accept` before expecting offers.

---

## Verification checklist (backend)

1. `POST /driver/location` returns **2xx** for valid driver with GPS body; **403** only for auth/legal, not for “stale GPS timestamp” or coarse accuracy blocking dispatch rows (**2c0c900+** behavior).
2. `GET /driver/available-requests` returns queue + `assigned_trip` shapes documented in `DRIVER_CLIENT.md`.
3. WebSocket auth and trip lifecycle events match `DRIVER_CLIENT.md`.
4. After **`TryAssign`** (app or bot), Telegram offer messages for that request are cleaned up for **all** notified drivers including the accepter (**assignment_service**).

---

## Related

- [`DRIVER_CLIENT.md`](DRIVER_CLIENT.md) — JSON shapes, errors, examples.
- [`AUTH.md`](AUTH.md) — middleware order, `X-Driver-Id`, legal.
- Root [`README.md`](../README.md) — env table, deployment.
