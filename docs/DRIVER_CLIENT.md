# Driver HTTP / WebSocket client (native / Flutter)

This document describes the **stable wire contract** for clients that call the same Go API as the Telegram Mini App and driver bot (e.g. native or Flutter driver apps). It is **additive** to **`docs/AUTH.md`** and the main **`README.md`** HTTP table.

## Environment variables (relevant to drivers)

| Variable | Default | Effect |
|----------|---------|--------|
| **`ENABLE_DRIVER_HTTP_LIVE_LOCATION`** | off (unset / not `true` or `1`) | When **off**, `POST /driver/location` updates `last_lat` / `last_lng` / `last_seen_at` / `grid_id` only. **Telegram** live location remains the primary signal for dispatch eligibility. When **`true`**, the same endpoint also refreshes **`last_live_location_at`** / **`live_location_active`** and can mark the driver online for matching (see **`README.md`**). |
| **`ENABLE_DRIVER_ID_HEADER`** | on | When **on** (default), `X-Driver-Id` may authenticate drivers on HTTP (and WebSocket when initData is absent). Set to `false`, `0`, `no`, or `off` to require Telegram initData only. |
| **`DRIVER_AUTH_DEBUG`** | off | Logs non-sensitive booleans about header paths; never logs header values. |

Production behavior when new vars are **unset** matches existing defaults above.

## Authentication modes

1. **`X-Telegram-Init-Data`** (Mini App): HMAC-validated; maps Telegram user → internal user. Used by `Authorization` is allowed by CORS but **not validated** by this server today (reserved for gateways / future use).
2. **`X-Driver-Id`**: Digits only — internal **`users.id`** (same as driver `user_id`) **or** Telegram **`users.telegram_id`**. Resolver requires **`drivers.verification_status = approved`**. See **`docs/AUTH.md`** for status codes.

**Route order:** Middleware runs **`TryDriverIDHeader`** first, then **`RequireDriverAuth`**, so a valid **`X-Driver-Id`** can satisfy driver routes without initData when header mode is enabled.

## CORS (e.g. Flutter web)

Preflight **`OPTIONS`** returns **204**. Allowed methods: **`GET`**, **`POST`**, **`OPTIONS`**. Allowed request headers include:

`Content-Type`, `Authorization`, `X-Telegram-Init-Data`, `X-Driver-Id`

`Access-Control-Allow-Origin` is `*` (clients sending credentials should confirm behavior matches their security model).

---

## `GET /driver/available-requests`

**Auth:** Driver (`X-Driver-Id` and/or initData as above).

**200 JSON** (all keys are **snake_case**):

| Field | Type | Notes |
|-------|------|--------|
| **`assigned_trip`** | object or `null` | If the driver has a trip in **`WAITING`**, **`ARRIVED`**, or **`STARTED`**, `{ "trip_id": "<uuid>", "status": "<status>" }`. Otherwise `null`. |
| **`available_requests`**, **`requests`**, **`pending_requests`**, **`queue`**, **`orders`**, **`jobs`** | array | **The same array** repeated under six aliases; clients may read one field or dedupe by **`request_id`**. |

Each queue item (**`DriverAvailableOffer`**) has:

| Field | Type | Notes |
|-------|------|--------|
| **`request_id`** | string | Stable id for the pending ride request (accept with **`POST /driver/accept-request`**). |
| **`trip_id`** | string | Omitted when empty; usually absent until assigned. |
| **`pickup_lat`**, **`pickup_lng`** | number | Pickup coordinates (map). |
| **`distance_km`** | number | Haversine distance from driver’s last known position to pickup (km); **0** if driver has no `last_lat`/`last_lng`. |
| **`radius_km`** | number | Request search radius. |
| **`expires_at`** | string | Optional; may be empty string if null in DB. |

**Example**

```bash
curl -sS -H "X-Driver-Id: YOUR_INTERNAL_OR_TELEGRAM_ID" \
  "$BASE/driver/available-requests"
```

---

## `POST /driver/accept-request`

**Auth:** Driver.

**Body (JSON):** at least one of **`trip_id`** or **`request_id`** (strings). Whitespace is trimmed.

| Scenario | HTTP | Body (typical) |
|----------|------|------------------|
| Accept pending offer by **`request_id`** | **200** | `{ "ok": true, "trip_id": "<new uuid>", "request_id": "<same>", "assigned": true }` |
| Idempotent check: body has only **`trip_id`**, trip exists, **this driver** is already assigned | **200** | `{ "ok": true, "trip_id": "<id>", "status": "<WAITING|ARRIVED|…>", "result": "already_assigned" }` |
| Request no longer assignable (taken, expired, etc.) | **409** | `{ "ok": false, "error": "request no longer available", "request_id": "<id>" }` |
| **`TryAssign`** returns a domain error (e.g. legal not accepted) | **400** | `{ "ok": false, "error": "<message>", "request_id": "<id>" }` |
| Assignment service unavailable | **503** | `{ "error": "assignment unavailable" }` |
| Trip not found (**`trip_id`**-only path) | **404** | `{ "ok": false, "error": "trip not found", "trip_id": "<id>" }` |
| **`trip_id`**-only but trip belongs to another driver | **403** | `{ "ok": false, "error": "not assigned to this trip" }` |

**Examples**

```bash
# Accept by request id
curl -sS -X POST -H "Content-Type: application/json" \
  -H "X-Driver-Id: YOUR_ID" \
  -d '{"request_id":"REQUEST_UUID"}' \
  "$BASE/driver/accept-request"

# Idempotent: already assigned to this trip
curl -sS -X POST -H "Content-Type: application/json" \
  -H "X-Telegram-Init-Data: <initDataFromTelegram>" \
  -d '{"trip_id":"TRIP_UUID"}' \
  "$BASE/driver/accept-request"
```

---

## `POST /driver/location`

**Auth:** Driver. If the driver has **no** active trip (**`WAITING`/`ARRIVED`/`STARTED`**) and has **not** accepted active legal documents, the handler returns **403** with **`{"error":"LEGAL_ACCEPTANCE_REQUIRED"}`** (see `legal.ErrCodeRequired`).

**Body (JSON)** — only **`lat`** / **`lng`** are required; there is **no** `latitude` / `longitude` alias in the struct (use **`lat`** / **`lng`**).

| Field | Required | Notes |
|-------|----------|--------|
| **`lat`**, **`lng`** | yes | Coordinates. |
| **`accuracy`** | no | Meters; updates with accuracy **> 50** m are ignored (**200** with `"ignored": "accuracy too low"`). |
| **`timestamp`** | no | Unix **seconds** for fix time; optional staleness checks against `last_seen_at`. |

**200:** `{ "ok": true }`, or `{ "ok": true, "ignored": "<reason>" }` when ignored (accuracy, stale, or trip point not recorded).

---

## Trip lifecycle (driver)

All use **`trip_id`** in the JSON body (**no alternate keys** in the handler structs).

| Endpoint | Body |
|----------|------|
| `POST /trip/arrived` | `{ "trip_id": "<uuid>" }` |
| `POST /trip/start` | `{ "trip_id": "<uuid>" }` |
| `POST /trip/finish` | `{ "trip_id": "<uuid>" }` |

**200 success:** `{ "ok": true, "trip_id": "...", "status": "...", "result": "updated" }` or `{ "ok": true, "result": "noop" }` on idempotent no-ops. Error shapes include **`trip_id`** and **`error`** (see `writeTripError` in **`internal/handlers/trip.go`**).

**Examples**

```bash
curl -sS -X POST -H "Content-Type: application/json" -H "X-Driver-Id: YOUR_ID" \
  -d '{"trip_id":"TRIP_UUID"}' "$BASE/trip/arrived"

curl -sS -X POST -H "Content-Type: application/json" -H "X-Driver-Id: YOUR_ID" \
  -d '{"trip_id":"TRIP_UUID"}' "$BASE/trip/start"

curl -sS -X POST -H "Content-Type: application/json" -H "X-Driver-Id: YOUR_ID" \
  -d '{"trip_id":"TRIP_UUID"}' "$BASE/trip/finish"
```

---

## `GET /ws?trip_id=<uuid>`

**Auth order** (see **`internal/ws/handler.go`** — **`ServeWsWithAuth`**):

1. Read **`X-Telegram-Init-Data`** header, or query **`init_data`** if the header is empty.
2. If initData is **non-empty**: verify with **driver** bot token, then **rider** bot token; resolve user + role.
3. **Else** if **`ENABLE_DRIVER_ID_HEADER`** is **true**: require **`X-Driver-Id`** and resolve an **approved** driver (same rules as HTTP).
4. **Else**: **401** `missing init data`.

Then **`AuthorizeTripAccess`**: only the **assigned driver** or the **rider** for that trip may subscribe.

**Wire format:** Messages are **JSON text frames** with shape **`internal/ws.Event`**:

```json
{
  "type": "string",
  "trip_id": "string",
  "trip_status": "string",
  "emitted_at": "RFC3339",
  "payload": {}
}
```

`trip_id` is duplicated on the object for convenience; **`BroadcastToTrip`** sets it if omitted.

**Event `type` values clients should handle** (non-exhaustive; tolerate unknown types):

| `type` | Notes |
|--------|--------|
| **`trip_started`** | Payload includes `trip_status`, `distance_m`, `distance_km`, `fare`. |
| **`trip_arrived`** | Driver at pickup. |
| **`trip_finished`** | Payload includes `distance_m`, `distance_km`, `fare`, `fare_amount`. |
| **`trip_cancelled`** | Payload includes `by`: `"driver"` or `"rider"` (map shape). |
| **`driver_location_update`** | During **`STARTED`**: payload may include `lat`, `lng`, `distance_km`, `fare`. |

Connection lifecycle: server sends **ping** frames on an interval; clients should respond to **pong** (standard WebSocket). Inbound client messages are read but not interpreted (keep-alive / close only).

**Example**

```bash
# Browser / tool that supports WebSocket; initData from Telegram WebApp
curl -sS -H "Connection: Upgrade" -H "Upgrade: websocket" \
  -H "X-Telegram-Init-Data: <initData>" \
  "$BASE/ws?trip_id=TRIP_UUID"
```

(Use a real WebSocket client in production; `curl` is illustrative only.)

---

## Related

- **`docs/AUTH.md`** — full auth matrix and security notes.
- **`README.md`** — deployment, `ENABLE_DRIVER_HTTP_LIVE_LOCATION`, and Mini App URLs.
