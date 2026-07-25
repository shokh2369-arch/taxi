You are updating the **YettiQanot driver Flutter app** to match backend changes that
have just landed. Read this whole brief before editing, then work through the
numbered tasks. Do not change backend code — this app talks to it over HTTP and
WebSocket only.

Existing wire contract lives in `docs/DRIVER_CLIENT.md` and `docs/AUTH.md`. This
brief covers what changed and is authoritative where the two disagree.

## 1. Breaking change — going online is now an explicit call

`POST /driver/location` and `POST /driver/location/app` **no longer clear
`manual_offline`**. Previously any location ping put the driver back online, which
meant the OFFLINE toggle silently undid itself: a driver who ended their shift and
drove home kept receiving orders because the app was still reporting position in
the background.

There is a new endpoint:

```
POST /driver/online     Authorization: Bearer <token>   (or X-Driver-Id when enabled)
  200 {"ok":true,"manual_offline":false,"note":"..."}
  401 driver auth required
```

**The app must call `POST /driver/online` when the user toggles ONLINE.** If it
does not, a driver who ever taps OFFLINE stays offline no matter how much location
data the app sends, and will report that the app is broken.

`POST /driver/offline` is unchanged and still required when the user toggles OFFLINE.

Safety net: sharing Telegram live location also clears the flag, so a driver on an
old build is not permanently stuck — but they will have to go through the bot.

## 2. Accepting a ride can now fail because *you* have an unfinished trip

A driver can no longer hold two active trips. `POST /driver/accept-request` may now
return a distinct error:

```
409 { "ok": false,
      "error": "driver_has_active_trip",
      "message": "Finish or cancel your current trip before accepting a new one.",
      "active_trip_id": "<uuid>",
      "request_id": "<uuid>" }
```

This is different from the existing `409 "request no longer available"` (someone
else took it). Handle it separately: show the driver that they have an unfinished
trip and offer to jump straight to `active_trip_id`. Do not show "ride no longer
available" — the offer will still be visible in their list, so they will tap it
again and loop.

## 3. Repeated cancellations now pause dispatch

A driver who cancels after accepting is recorded. Past **3 cancellations in 24
hours**, they get an escalating dispatch cooldown (15 minutes, lengthening, capped
at 4 hours) during which they receive no offers.

The API does not expose the cooldown yet, so the app cannot show a countdown. What
it should do is make cancellation deliberate: confirm before cancelling, and warn
that frequent cancellations temporarily stop new orders. Do not present cancel as
a one-tap action next to accept.

## 4. Trip lifecycle also exists in the Telegram bot now

Arrived / start / finish / cancel are now available as inline buttons in the driver
bot, not only in the app. A driver may move a trip forward from either surface, so
**the app cannot assume it is the only writer**. On resume, and on reconnect,
refetch `GET /trip/:id` rather than trusting cached local state — the status may
have advanced without the app doing anything.

## 5. WebSocket events now carry a sequence number

`GET /ws?trip_id=<uuid>` event shape:

```json
{ "type": "...", "trip_id": "...", "trip_status": "...",
  "emitted_at": "RFC3339", "seq": 7, "payload": { } }
```

Delivery is best effort — the server drops events when a queue is full. `seq` is
monotonic **per trip**. Track the last one seen; if the next jumps by more than 1,
refetch `GET /trip/:id` and reconcile. Combined with task 4, treat the socket as a
hint that something changed, never as the source of truth.

## 6. Long polling actually works now

`GET /driver/available-requests?wait_sec=25` previously woke only one waiting
client per dispatch event, so most clients sat out the full timeout and the
endpoint behaved like 1-second polling. It now wakes every waiter.

If the app currently uses a short `wait_sec` or a tight poll loop as a workaround,
switch to a real long poll (up to `wait_sec=25`) and drop the extra polling. That
is a meaningful battery and data saving for a driver on shift all day, and it
reduces backend cost.

## 7. `GET /trip/:id` returns less to unauthenticated callers

The endpoint still answers without credentials, but no longer returns rider or
driver **phone numbers** to anonymous callers. If the app shows the rider's phone
so the driver can call them, it **must** send the Bearer token on this request.
Easy to miss: the endpoint still returns 200 either way, just without the number.

## 8. Login unchanged, but the lockout is now real

The driver OTP is still 6 digits — no change needed. What changed is that the
5-attempt lockout is now enforced atomically, so concurrent or rapid retries can no
longer slip past it. Make sure the UI does not fire overlapping verify requests
(debounce the submit button); previously that accidentally worked around the limit,
now it will consume attempts and consume the code.

## 9. Known backend gaps to design around

- **Registration is Telegram-only.** Phone, name, car, plate and both document
  photos are collected in the driver bot, and approval happens there. The app
  cannot onboard a new driver. Route unregistered users to the bot explicitly.
- **The driver's phone number must be a real Uzbek mobile.** Registration now
  validates `+998XXXXXXXXX`; previously any text was accepted. This only affects
  the bot flow, but expect existing accounts with junk phone values.
- **There is no rating or feedback for drivers**, and no in-app way to report a
  rider or dispute a fare. If the design calls for them, they need backend work.
- **Balance top-up is manual.** When a driver's wallet runs out they stop receiving
  orders. The bot now tells them why and gives a support contact; the app should do
  the same rather than showing an empty offer list with no explanation. Read the
  balance from the driver status/profile endpoint and surface it before it hits zero.

## 10. Verify before calling it done

1. Toggle OFFLINE, leave the app reporting location for a few minutes, confirm no
   offers arrive; toggle ONLINE and confirm they resume.
2. Accept a ride while an unfinished trip exists — confirm the app routes to the
   existing trip instead of showing "no longer available".
3. Advance a trip from the Telegram bot, then foreground the app — confirm it picks
   up the new status.
4. Kill the socket mid-trip, reconnect, confirm state reconciles via `seq`.
5. The rider's phone number is visible on an authenticated trip screen.
6. Long poll at `wait_sec=25` returns promptly when a new offer appears.
