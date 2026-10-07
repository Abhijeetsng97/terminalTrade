# terminalTrade v1 — TUI Page Design

Companion to `docs/specs/terminaltrade-v1.md` (the what & why). This doc defines the **how** for the UI layer: every screen, its layout, data sources, refresh cadence, keys, and states. An implementer should build the TUI from this without re-deriving decisions.

## Conventions

- Target terminal: **100–120 cols × 24+ rows**; minimum usable: **80×24**. Below 100 cols, optional columns drop out in the order noted per page. Below 80, the app renders a "terminal too small" screen.
- All prices Indian format: `₹1,23,456.75`, quantities with Indian digit grouping.
- Broker colors everywhere a broker is identifiable: **Kite = green**, **Fyers = orange**.
- Freshness tag (every data region): `LIVE` (green, WS), `POLL` (yellow, REST fallback active), `STALE` (red, last update timestamp shown).
- Instrument text is always canonical: `NIFTY 26DEC 25000 CE` — never a broker symbol.

## Global Chrome

Present on every page (except gate/modals which take over the full screen).

```
┌──────────────────────────────────────────────────────────────────────────────────┐
│ terminalTrade   [1]Pos [2]Chain [3]Funds [4]Book   ● KITE  ● FYERS  14:32:07 IST  │  ← top bar
│                                                                  Mkt: OPEN   LIVE │
├──────────────────────────────────────────────────────────────────────────────────┤
│                                                                                  │
│                        (page content — see per-page sections)                    │
│                                                                                  │
├──────────────────────────────────────────────────────────────────────────────────┤
│ ⚠ 2 alerts [!]                              Day P&L: +₹12,450.00                  │  ← alert bar
├──────────────────────────────────────────────────────────────────────────────────┤
│ 1/2/3/4 pages · ↑↓ navigate · B buy · S sell · L broker login · ? help · qq quit │  ← key bar
└──────────────────────────────────────────────────────────────────────────────────┘
```

| Region | Content | Source | Refresh |
|---|---|---|---|
| Top bar | Page tabs, broker status dots (green ok / orange token-expiring / red expired), IST clock, market status (`OPEN` / `CLOSED` / `PRE-OPEN`), freshness of the *active page's* feed | session state, market-hours guard | on change |
| Alert bar | Unread alert count; `!` opens the alert list overlay | order engine + ops alerts | on change; Day P&L piggybacks position feed (1s) |
| Key bar | Context-sensitive hints for the active page | static per page | on page change |

Keys valid everywhere (not inside a text input): `1` `2` `3` `4` switch pages, `L` broker login modal, `?` help overlay, `!` alert list, `qq` (double-tap) quit app.

---

## Login Gate (first screen, full-screen takeover)

```
        terminalTrade

        Enter TOTP code

        [ _ _ _ _ _ _ ]          attempts left: 3

        6-digit code from your authenticator.
        3 wrong attempts disconnects this session.
```

- After SSH pubkey auth succeeds, this screen is mandatory before any page renders.
- TOTP entry: 6-digit auto-advance input; `Enter` submits.
- 3 failures → disconnect (per spec auth decision).
- **First-run mode**: instead of the code prompt, displays the `otpauth://` URL + terminal QR to enroll, then asks to re-enter a code once to confirm.

---

## Page 1 — Positions (`1`)

Purpose: combined, netted exposure at a glance; the close-from-here surface.

```
┌────────────────────────────────────────────────────────────────────────────────────┐
│ POSITIONS  NIFTY fut: 26,120.50   LIVE                          Day P&L +₹12,450  │
├────────────────────────────────────────────────────────────────────────────────────┤
│ INSTRUMENT                    BROKER   LOTS   AVG      LTP      P&L        P&L%    │
│ ▸ NIFTY 26DEC 25000 CE         NET      +2    112.40   138.65   +₹3,412    +2.31%  │
│ ▾ NIFTY 26DEC 25100 PE         NET      +8    84.20    77.10    -₹4,680    -3.02%  │
│     ├ KITE                              +5    85.10    77.10    -₹3,250           │
│     └ FYERS                             +3    82.70    77.10    -₹1,430           │
│ ▸ SENSEX 26DEC 82000 CE        NET      -3    201.00   188.40   +₹3,780    +1.24%  │
├────────────────────────────────────────────────────────────────────────────────────┤
│ Margin: KITE ₹2.1L used · FYERS ₹0.8L used          [C] close  [e] expand  [Q] all  │
└────────────────────────────────────────────────────────────────────────────────────┘
```

| Element | Source | Refresh |
|---|---|---|
| Net rows (group headers) | core netting over both brokers' position snapshots | **1s** (poll) with WS position-instrument ticks improving LTP/P&L between polls |
| Broker sub-rows (expanded) | per-broker snapshots | same cadence |
| Day P&L (header) | core P&L aggregator | 1s |
| Margin line (footer) | funds snapshot | 5s |

Behavior:

- Rows are **grouped by canonical instrument**; the group header is the NET row. Broker rows are hidden until expanded (`e` on selected group, or auto-expand a group that has a single broker).
- **`C` close on a single-broker or expanded broker row** → order form pre-filled: opposite side, exact held qty, broker locked to that broker (per routing decision).
- **`C` on a multi-broker NET row** → group expands (if not already) and the form's broker field becomes an explicit choice; nothing is pre-ordered until a broker row is chosen. NET rows are never directly orderable.
- `Q` → square-off-all: iterates positions one at a time; a confirm step lists each position with its broker, then processes sequentially with a live progress line.
- Sort: fixed order — netted groups by underlying then expiry then strike (stable, predictable); no v1 re-sorting.
- If a held instrument is missing from the instruments table (mapping failure), the row still renders with the raw broker symbol and a `⚠unmapped` marker; closing it is blocked with an alert (order placement requires a canonical key).

<sub>80-col fallback: drop P&L% and AVG columns.</sub>

---

## Page 2 — Option Chain (`2`)

Purpose: the trading surface — price the ladder, act on a strike in seconds.

```
┌────────────────────────────────────────────────────────────────────────────────────┐
│ OPTION CHAIN  NIFTY 26,120.50  LIVE         Expiry: 29 OCT 26 [w]    OI window: 10 │
├────────────────────────────────────────────────────────────────────────────────────┤
│   CE OI     CE ASK   CE BID   CE LTP  │ STRIKE │  PE LTP   PE BID   PE ASK   PE OI │
│──────────────────────────────────────┼────────┼───────────────────────────────────│
│  184,200    212.05    208.40   209.95 │ 25900  │   3.10     2.95     3.25    92,400│
│  221,500    152.80    149.95   151.10 │ 26000  │   5.75     5.40     6.05   143,800│
│  268,300     98.65     95.20   96.90  │ 26100  │  12.40    11.85    12.75   201,600│
│  312,900     54.20     51.75   52.95  │ 26200  │  27.90    26.50    28.40   289,700│
│▓ 421,000     21.35     18.90   20.15  │ 26300  │  61.20    58.75    63.05   365,100│ ← ATM
│  539,700      8.65      6.90    7.85   │ 26400  │  142.30   138.90   145.60  448,200│
├────────────────────────────────────────────────────────────────────────────────────┤
│ ←→ side · B buy · S sell · w expiry · a ATM · +/- window   [2] chain [1] positions  │
└────────────────────────────────────────────────────────────────────────────────────┘
```

| Element | Source | Refresh |
|---|---|---|
| Row quotes (LTP, bid, ask, OI) | WS quotes for all visible strikes (both brokers' feeds merge by canonical key; each row is one broker's data — the page renders **Kite's chain with Fyers as fallback source** if Kite feed is stale) | continuous WS; **10s REST poll fallback** if WS down (shows `POLL`) |
| Underlying LTP (header) | WS underlying subscription | continuous; 10s fallback |
| Expiry list | instruments table | on refresh jobs |

Behavior:

- Load centers on **ATM** (highlighted row, reverse video); `a` re-centers after scrolling.
- `w` cycles expiries: nearest weekly → next weekly → current monthly, wrapping. Header shows active expiry; rows reload, cursor moves to ATM.
- Horizontal cursor has **three zones per row**: CE side, strike, PE side (`←`/`→`). The active zone is underlined; `B`/`S` opens the order form for that zone's option.
- `+`/`-` widens/narrows the visible strike window (default 5 strikes each side of ATM, max 10 — bounded by WS subscription budget).
- Out-of-market-hours: chain renders from the last session's data with `STALE (yday 15:30)` in the header; keys still work but `B`/`S` hits the market-hours guard message.
- If OI isn't ticking for a row (feed gap), the cell shows the last value dimmed with the row's freshness inheriting the page tag.

<sub>80-col fallback: drop both OI columns first, then BID/ASK pairs (keep LTP|strike|LTP).</sub>

---

## Page 3 — Funds (`3`)

Purpose: know what you can spend before you place.

```
┌────────────────────────────────────────────────────────────────────────────────────┐
│ FUNDS   LIVE (poll 5s)                                                             │
├────────────────────────────────────────────────────────────────────────────────────┤
│ BROKER     AVAILABLE       USED MARGIN      TOTAL                                 │
│ ● KITE     ₹1,42,300.00    ₹2,10,450.00    ₹3,52,750.00                          │
│ ● FYERS    ₹86,150.00      ₹78,900.00      ₹1,65,050.00                          │
│ COMBINED   ₹2,28,450.00    ₹2,89,350.00    ₹5,17,800.00                          │
├────────────────────────────────────────────────────────────────────────────────────┤
│ [3] funds · values from broker funds snapshots · margins may lag fills by seconds  │
└────────────────────────────────────────────────────────────────────────────────────┘
```

- Source: both adapters' funds endpoints, REST poll **every 5s** while the page is visible (funds don't need WS).
- COMBINED row is a plain sum (available + used = total per broker; no cross-margin logic in v1).
- Broker dot carries the session color; a red dot means funds shown are the last known cached snapshot with timestamp.

---

## Page 4 — Order Book (`4`)

Purpose: full visibility of parent orders, their slices, and the levers (cancel, modify).

```
┌────────────────────────────────────────────────────────────────────────────────────┐
│ ORDER BOOK  today                                          2 pending · 1 alert     │
├────────────────────────────────────────────────────────────────────────────────────┤
│ TIME   INSTRUMENT              SIDE  QTY(FILL/TOT)  TYPE       PRICE    BROKER  ST │
│ ▾ 10:31 NIFTY 26DEC 26300 CE   BUY   3,510/6,500    MKT-PROT   20.65    ●KITE   2/2│
│     ├ 10:31 slice 1/2          BUY   3,510/3,510    MKT-PROT   20.65    ●KITE   FILL│
│     └ 10:31 slice 2/2          BUY   0/2,990        MKT-PROT   21.05    ●KITE   OPEN│
│ ▸ 10:28 NIFTY 26DEC 26300 PE   SELL  0/3,250        LIMIT      25.00    ●FYERS  OPEN│
│ ▸ 10:12 NIFTY 26DEC 25900 CE   BUY   1,300/1,300    MKT-PROT   152.80   ●KITE   DONE│
├────────────────────────────────────────────────────────────────────────────────────┤
│ [X] cancel slice · [M] modify · [e] expand · rejected slices show reason on expand  │
└────────────────────────────────────────────────────────────────────────────────────┘
```

| Element | Source | Refresh |
|---|---|---|
| Parent + child rows | orders store (parent docs + child slice docs), enriched by WS order updates | WS push; reconciliation poll every **10s** while any child is pending |
| Qty(fill/total) | broker truth via reconcile | same |

Behavior:

- Default view: today's parent orders, newest first; children shown when expanded (`e`). Collapsed parents show aggregated fill progress.
- Parent row `ST` column: child states summarized — `2/2`, `OPEN` (any open), `DONE`, `HALT` (margin-rejected, remaining slices stopped), `REJ`.
- `X` on an **open child slice** → confirm → adapter cancel. On an open parent with all children open → cancels all its open children (one parent at a time, not a global kill switch).
- `M` on an open parent or slice → modify modal (price, qty). Modify re-validates freeze limits at submit — exceeding them shows the slice-plan hint instead of sending.
- Rejected children show the broker's rejection reason inline when expanded (margin rejections also raise the alert-bar event, per order-engine rules).

---

## Order Form (modal over any page; `B`/`S` from chain or positions)

Purpose: one screen, zero surprises, minimal keystrokes.

```
┌────────────────────────────────────────────────────────────────────┐
│ BUY  NIFTY 26DEC 26300 CE                          LTP ₹20.15       │
├────────────────────────────────────────────────────────────────────┤
│ Side        (B) BUY   (S) SELL                                      │
│ Lots        [ 54          ]  = 3,510 units   ≈ ₹70,801             │
│ Type        (p) MKT-PROT   (l) LIMIT                                 │
│ Price       auto: ₹20.90 (band +3%)      ← hidden unless LIMIT      │
│ Broker      ● KITE  (Tab to switch: ○ FYERS)                        │
├────────────────────────────────────────────────────────────────────┤
│ Freeze: 54 lots · Plan: 1 slice · Est. value ₹70,801               │
│                                                    [Enter] review  │
└────────────────────────────────────────────────────────────────────┘
```

- **Pre-fill rules**: from chain — the option under the cursor, side = key pressed (`B`/`S`). From positions close — opposite side, held qty, broker locked. Broker defaults otherwise: last-used, `Tab` cycles, color-coded.
- Fields are one screen — up/down/arrows move, type to edit; `Enter` goes straight to review (no wizard).
- Live-derived line under the form: lots → units → est. value; freeze lots for the instrument; computed slice plan; protection price preview (band from LTP bucket: <₹10→5%, ₹10–100→3%, ₹100–500→2%, >₹500→1%).
- `LIMIT` type reveals the price field (pre-filled at LTP, editable, tick-size validated).
- Validation at entry time, before review: lots > 0 and ≤ per-order cap; day-cap check; market-hours check. Violations show inline — the form never forwards an impossible intent.

**Review/confirm screen** (next step, replaces form content):

```
┌────────────────────────────────────────────────────────────────────┐
│ CONFIRM ORDER                                                       │
│ BUY 54 lots (3,510) NIFTY 26DEC 26300 CE @ MKT-PROT ≤ ₹20.90       │
│ Broker: KITE   Est. value ₹70,801   Slices: 1                       │
├────────────────────────────────────────────────────────────────────┤
│   (y) place order        (n/Esc) back to edit                      │
└────────────────────────────────────────────────────────────────────┘
```

- Every order passes review — value, protection price (or limit), broker, **slice plan** (count + per-slice qty when >1).
- **>10 lots**: review requires typing the lot quantity back (second-confirm, per risk caps).
- **>2× freeze lots**: additionally re-types the quantity (per risk cap rule).
- `y` places: engine assigns idempotency tags, fires all slices, and the form closes back to the underlying page; the alert bar shows placement result.
- Esc from form never kills the app (global `qq` only, and not while an input is focused).

---

## Broker Login Modal (`L`)

```
┌────────────────────────────────────────────────────────────────────┐
│ BROKER SESSIONS                                                     │
│ ● KITE    token OK (expires ~09:00 daily)                           │
│ ● FYERS   token OK (auto-refreshed 08:30)                           │
├────────────────────────────────────────────────────────────────────┤
│ KITE login: 1) open  https://kite.zerodha.com/connect/login?api_key=…  │
│ 2) login, 3) copy request_token from redirect URL, 4) paste below   │
│ request_token: [______________________________]  [Enter] submit    │
└────────────────────────────────────────────────────────────────────┘
```

- Shows both brokers with live session status; Kite presents the paste flow. Fyers normally says "auto-refreshed"; its manual fallback (auth code paste) appears only when the daily refresh has failed (streak ≥1) — same modal shape as Kite's.
- Submitting a token: validate via the adapter (GenerateSession / token exchange), store encrypted, re-subscribe feeds. Failure stays on the modal with the broker's error.

## Alert List Overlay (`!`)

- Full-screen list of unacknowledged alerts (newest first): margin-rejection halts, stale instruments at open, refresh failures, session expirations during market hours, reconcile corrections.
- Each row: time, severity (`HALT`/`WARN`/`INFO`), one-line description. `Enter` expands detail; `d` acknowledges/dismisses. The alert bar badge shows unread count.

## Help Overlay (`?`)

- Two-column keymap: global keys (left) + active-page keys (right). One screen, no scrolling at 100 cols.

---

## Keymap Summary

| Key | Context | Action |
|---|---|---|
| `1`/`2`/`3`/`4` | global (not in input) | positions / chain / funds / order book |
| `↑` `↓` | pages | move selection |
| `←` `→` | chain | cursor zone: CE side / strike / PE side |
| `B` / `S` | chain, positions | order form (buy/sell prefilled) |
| `C` | positions | close selected (broker row or choice if netted multi-broker) |
| `Q` | positions | square-off-all (confirm + sequential) |
| `e` | positions, order book | expand/collapse group |
| `w` | chain | cycle expiry |
| `a` | chain | re-center on ATM |
| `+` / `-` | chain | widen/narrow strike window |
| `X` | order book | cancel open slice (or open parent's children) |
| `M` | order book | modify open order |
| `Tab` | order form | switch broker (where not locked by close routing) |
| `Enter` | order form | submit to review; review `y` places |
| `Esc` | forms/modals | back; never quits the app |
| `L` | global | broker login modal |
| `!` | global | alert list |
| `?` | global | help overlay |
| `qq` | global (not in input) | quit app |

## State & Freshness Rules (all pages)

1. Every data region carries one of `LIVE` / `POLL` / `STALE(ts)`. WS connected → LIVE; WS down, REST fallback running → POLL; both quiet beyond cadence → STALE with timestamp. Transition WS→POLL is automatic on socket drop (SDK reconnect callbacks wired to the marketdata staleness machine).
2. Broker dot colors: green = session valid; orange = expiring-soon/refresh-failed (Fyers); red = invalid during market hours (raises alert).
3. Market status `PRE-OPEN` (09:00–09:15): all pages readable (data stale-marked), order placement blocked by the market-hours guard with an explicit message — nothing reaches the broker.
4. Loading (first paint / page switch): skeleton rows with the last known data dimmed, replaced as the first snapshot lands — never a blank screen.
5. If Mongo is unreachable at startup, the gate screen renders with an error banner; SSH stays up (no crash-loop) since recovery shouldn't require an SSH reconnect.