You are fixing the commission row on the YettiQanot **driver** app home screen.
Client work only — the backend change is already done. Do not change backend code.

## The problem

The home screen shows a row reading **"Buyurtmadan 6%"**. That 6 is hardcoded in
the app. The backend's actual commission rate is **5%** by default, and it is
admin-editable at runtime — an operator can change it in the admin bot at any
time, including to 0.

So the app is currently telling every driver a rate that is wrong, and would stay
wrong after any change. Commission is deducted from the driver's wallet, so this
is a number they will compare against their balance.

## The fix

Read the rate from the backend and render it. **Delete the hardcoded value** — do
not keep 6 as a fallback constant, or the bug survives whenever a fetch fails.

### Where to get it

The home screen already calls `GET /driver/available-requests` for the balance
figures shown above the row (`total_balance`, `promo_balance`, `cash_balance`).
That same response now carries the rate — no extra request needed:

```json
{
  "total_balance": 48080,
  "promo_balance": 48080,
  "cash_balance": 0,
  "commission_percent": 5,
  "commission_charged": true
}
```

`commission_percent` is an integer percentage. Render it where the `6` is now:
`"Buyurtmadan ${commissionPercent}%"`.

If you prefer the row not to depend on the dispatch poll, `GET /driver/tariff`
returns the same rate plus the full fare breakdown (`base_fare`, `tier_0_1_km`,
`tier_1_2_km`, `tier_2_plus_km`, `commission_percent`, `commission_charged`,
`currency`). It supports `If-None-Match` / `304`, so refreshing it is cheap. Use
whichever fits your state management; do not call both on every poll.

### Three states the row must handle

1. **Normal** — `commission_charged: true`, `commission_percent: 5` →
   *"Buyurtmadan 5%"*.
2. **Commission switched off** — `commission_charged: false`, or
   `commission_percent: 0`. Both are legitimate, deliberate settings, not missing
   data. Show *"Komissiya olinmaydi"* (no commission is taken). Do **not** render
   *"Buyurtmadan 0%"* — it reads like a glitch.
3. **Not loaded yet, or the field is absent** (older backend) — show a placeholder
   or hide the row. Never fall back to a guessed number. A hidden row is honest; a
   wrong number is not, and that is exactly the bug being fixed.

### Note on the value's meaning

The percentage is taken from the **fare of each completed trip** and deducted from
the driver's wallet (promo balance first, then cash). The row's wording,
"Buyurtmadan N%" — N% from the order — is accurate; only the number is wrong.

## Verify before calling it done

1. The row shows **5%**, not 6%, against the live backend.
2. Change the rate in the admin bot to 10 and confirm the app shows 10% after a
   refresh — no rebuild.
3. Set it to 0 and confirm the row reads "no commission" rather than "0%".
4. Grep the codebase for a literal `6` used as a commission rate and confirm none
   remains.
