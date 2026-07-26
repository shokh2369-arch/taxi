You are updating the **YettiQanot rider Flutter app** to match backend changes that
have just landed. Read this whole brief before editing, then work through the
numbered tasks. Do not change backend code — this app talks to it over HTTP and
WebSocket only. Base URL is the deployed service; auth is a Bearer access token
from the rider auth flow.

## 1. Breaking change — the login code is now 6 digits

`POST /v1/rider/auth/request-code` now generates a **6-digit** code. It used to be
4. `POST /v1/rider/auth/verify-code` rejects anything that is not exactly 6 digits
with `400 invalid_code`.

Find every place the app assumes 4: the OTP input field length, any `maxLength`,
pin-widget cell count, client-side validation, autofill hints, and tests or
fixtures using a 4-digit literal. **If you miss one, login is completely broken
for every user** — this is the highest priority task.

## 2. Login is now rate limited, and the app must show it

Throttling is on by default: one code per phone per **60 seconds**, and at most
**5 per hour**. Previously there was no limit, so the app has probably never
handled these.

- `429` + `{"error":{"code":"code_recently_sent","message":"..."}}` and a
  `Retry-After` header in **seconds**. Disable the resend button and show a
  countdown from `Retry-After`; do not parse the message text.
- `429` + `code: "too_many_codes"` — the hourly cap. Tell the user to try later.
- `400` + `code: "too_many_attempts"` — five wrong codes consumed the code; the
  user must request a new one.

## 3. Auth contract

```
POST /v1/rider/auth/request-code   { "phone": "+998901234567" }
  200 {"ok":true}
  400 invalid_phone | invalid_body
  409 telegram_not_linked      // no users row, or no Telegram linked
  409 bot_blocked              // rider blocked the Telegram bot
  429 code_recently_sent | too_many_codes
  502 telegram_send_failed

POST /v1/rider/auth/verify-code   { "phone": "+998901234567", "code": "123456" }
  200 {"access_token","refresh_token","expires_in"}   // expires_in is seconds
  400 invalid_code | too_many_attempts

POST /v1/rider/auth/refresh       { "refresh_token": "..." }
  200 {"access_token","refresh_token","expires_in"}   // refresh token ROTATES
  401 invalid_refresh_token

POST /v1/rider/auth/logout        Authorization: Bearer <access_token>
```

Refresh **rotates** the refresh token — the old one is revoked and a new pair is
returned. Always persist the new refresh token and discard the old, or the next
refresh fails.

## 4. Ride flow endpoints

All require `Authorization: Bearer <access_token>`.

```
POST /v1/rider/requests                    { pickup_lat, pickup_lng, client_request_id? }
POST /v1/rider/requests/:id/destination    { drop_lat, drop_lng, drop_name? }
POST /v1/rider/requests/:id/confirm
POST /v1/rider/requests/:id/cancel
GET  /v1/rider/trips/active
GET  /v1/rider/trips
POST /v1/rider/trips/:id/cancel
GET  /v1/rider/notifications
GET  /trip/:id                             // trip detail + live driver position
```

Error codes: `legal_required`, `phone_required`, `abuse_blocked`,
`duplicate_pending`, `invalid_coordinates`, `invalid_state`, `not_found`,
`not_your_request`.

A destination within ~100 m of the pickup is rejected with `400
invalid_coordinates` (degenerate order guard — mirrors the client-side check).

Account deletion (Google Play compliance) and WS tickets:

```
DELETE /v1/rider/account          Authorization: Bearer <access_token>
  200 {"ok":true}        // phone/name/Telegram link erased, all sessions revoked,
                         // pending request cancelled; trips are kept anonymized
  409 invalid_state      // a trip is WAITING/ARRIVED/STARTED — finish/cancel first

POST /v1/rider/ws-ticket          Authorization: Bearer <access_token>
  200 {"ticket":"...","expires_in":60}
```

`ws-ticket` is for web builds: open the socket as
`GET /ws?trip_id=<uuid>&ticket=<ticket>` so the long-lived JWT never appears in
a URL. Tickets are one-time and expire in 60 s — request one right before each
connect (including reconnects). `?access_token=` keeps working unchanged.

`client_request_id` is accepted but **not persisted**, so it does not deduplicate
retries. Rely on `duplicate_pending` instead (task 5).

## 5. Timing and lifecycle changes that affect the UI

- **One pending request per rider, enforced in the database.** A second create
  returns `409 duplicate_pending`, and the error object now identifies what is
  blocking you:

  ```json
  { "error": { "code": "duplicate_pending", "message": "...",
               "pending_request_id": "<uuid>", "destination_confirmed": false } }
  ```

  If there is an active trip, open it. Otherwise use `pending_request_id`: when
  `destination_confirmed` is `false` the request was never dispatched, so offer to
  cancel it (`POST /v1/rider/requests/:id/cancel`) and start over rather than
  making the rider wait out the 30-minute abandoned-request timeout.
- **The dispatch window restarts when the rider confirms**, not when they pick a
  destination. The rider now gets the full window (default 120s) to find a driver
  regardless of how long they spent reading the price.
- **Abandoned requests expire after 30 minutes.** A request that never reached
  confirmation is retired automatically. Previously it lived forever and blocked
  the rider from ordering again, so if the app has a workaround for a "stuck
  pending request", remove it.
- **A driver cancelling no longer ends the ride.** The request returns to the pool
  with a fresh window and is re-dispatched. The app will see the active trip
  disappear while the request goes back to searching — handle that as "looking for
  another driver", not "order finished".

## 6. WebSocket events now carry a sequence number

`GET /ws?trip_id=<uuid>` (Bearer or Telegram initData). Event shape:

```json
{ "type": "...", "trip_id": "...", "trip_status": "...",
  "emitted_at": "RFC3339", "seq": 7, "payload": { } }
```

Delivery is best effort — the server drops events when a queue is full, so a
terminal event like `trip_finished` can genuinely go missing. `seq` is monotonic
**per trip**. Track the last seq you saw; if the next jumps by more than 1, you
missed something: refetch `GET /trip/:id` and reconcile rather than trusting local
state. Do not assume the socket is reliable.

## 7. `GET /trip/:id` returns less to unauthenticated callers

This endpoint still works without credentials (the trip UUID acts as a capability
for map and tracking links), but it no longer returns rider or driver **phone
numbers** to anonymous callers. If the app shows the driver's phone so the rider
can call them, it **must** send the Bearer token on this request. Verify that path
specifically — it is easy to miss because the endpoint still returns 200 either
way, just without the number.

## 8. Known backend gaps to design around

These are real, currently unfixed, and will not be solved by the app:

- **The app receives no push or in-app notifications at all.** Telegram messages
  are suppressed for riders seen in the app within the last 120 seconds, and
  `rider_app_notifications` is read by `GET /v1/rider/notifications` but nothing
  ever writes to it. Driver-assigned, arrived, started, finished and
  driver-cancelled reach the app only via WebSocket. Poll
  `GET /v1/rider/trips/active` as a fallback whenever the socket is not connected,
  and do not rely on the notifications endpoint returning anything.
- **The app cannot register a new rider.** Login requires a phone that already
  exists in the backend, and the OTP is delivered through the Telegram rider bot.
  A user who has never opened the bot gets `409 telegram_not_linked`. Show an
  explicit "open our Telegram bot first" path rather than a generic error.
- **The price is an estimate, not a quote.** It comes from routing, or from
  straight-line distance when routing is unavailable, while the final fare is
  computed from the driver's accumulated GPS distance. They can differ. Label it
  as approximate and show the final fare clearly at trip end.
- **There is no rating, feedback, support contact or fare dispute** anywhere in the
  backend. If the design calls for them, they need backend work first.

## 9. Verify before calling it done

1. A full login with a real 6-digit code succeeds.
2. Tapping resend twice quickly shows a countdown, not an error dialog.
3. Creating a second order while one is pending routes to the existing order.
4. Killing the socket mid-trip and reconnecting reconciles state via `seq`.
5. The driver's phone number is visible on an authenticated trip screen.
6. Add or update tests that pin the OTP length so this cannot regress again.
