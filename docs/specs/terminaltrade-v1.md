# terminalTrade v1 — Design Spec

`ready-for-agent`

## Problem Statement

I trade Indian index options through two brokers (Zerodha Kite and Fyers), but every trading action requires opening a browser, logging into a web app, and navigating through UIs designed for a broad audience. This is slow, heavy (browser + multiple tabs), and impossible to use from a low-bandwidth or SSH-only environment. Large option orders need manual splitting to stay under exchange freeze limits, positions are scattered across two broker apps with incompatible symbol formats, and there is no single terminal that shows my combined exposure, funds, and the option chain with the ability to act on them in seconds.

## Solution

A single Go binary that serves a full-screen terminal UI over SSH. I run `ssh trade.tradeapp.in`, enter a rotating TOTP code, and get a minimal, keyboard-driven trading terminal: netted positions across both brokers, live option chain (WebSocket-fed), funds and margins, and a fast order form that places market-protected or limit orders with automatic slicing under exchange freeze limits. Orders are idempotent, audited, reconciled against broker truth, and closeable with one keypress. The system is designed as a reliable order-placement proxy — the safety properties (never a naked market order, never a double-placed order, never a silently-stale freeze limit) are the core value, not an afterthought.

## User Stories

### Access & Authentication

1. As the trader, I want to type `ssh trade.tradeapp.in` and get the trading terminal directly, so that I need no browser, app install, or setup beyond an SSH client.
2. As the trader, I want to authenticate with my SSH public key, so that only machines I control can reach the terminal.
3. As the trader, I want to enter a 6-digit TOTP code after connecting, so that a stolen or shared SSH key alone cannot place orders with my money.
4. As the trader, I want the connection dropped after 3 wrong TOTP attempts, so that brute force is impractical.
5. As the trader, I want the TUI to render correctly in any reasonable terminal size, so that it works from my laptop, phone SSH app, or tablet.

### Broker Sessions

6. As the trader, I want to connect both my Kite and Fyers accounts by pasting API credentials once, so that daily use never re-enters them.
7. As the trader, I want to press `L` when my Kite session has expired and paste the request token, so that Kite login is one paste from the browser flow I already do.
8. As the trader, I want my Fyers access token refreshed automatically every day from the stored refresh token, so that Fyers works without daily attention.
9. As the trader, I want to be alerted prominently when a broker session is invalid and markets are open, so that I never discover a dead session by a failed order.
10. As the trader, I want broker credentials stored encrypted at rest, so that a leaked database dump does not leak my API secrets.

### Positions

11. As the trader, I want to see all open positions across both brokers on one page, so that I see my true combined exposure.
12. As the trader, I want positions netted by instrument (e.g. +5 NIFTY 25000CE at Kite and -3 at Fyers shown as NET +2, with per-broker rows expandable), so that hedges across brokers read as one position.
13. As the trader, I want position P&L (per-position and day total) refreshed every second while the page is open, so that I always see current exposure value.
14. As the trader, I want the underlying, strike, expiry, and option type displayed canonically (not in each broker's symbol format), so that I read one vocabulary everywhere.
15. As the trader, I want to select a position and press `C` to close it, so that exiting is one keypress with the opposite side order placed at the position's broker.
16. As the trader, I want a square-off-all action that closes every open position one at a time, so that I can flatten everything in a panic without a basket race.

### Funds

17. As the trader, I want to see available cash, used margin, and total margin per broker and combined, so that I know what I can spend before placing an order.
18. As the trader, I want funds refreshed at least every few seconds while visible, so that margin consumption tracks reality.

### Option Chain

19. As the trader, I want a two-sided option chain (`CE LTP | bid | ask | OI — strike — PE LTP | bid | ask | OI`) around ATM, so that I can price and enter option trades from one view.
20. As the trader, I want the chain refreshed from WebSocket ticks continuously, with a 10-second REST refresh as fallback, so that prices stay live even if a socket breaks.
21. As the trader, I want the ATM row highlighted and the underlying LTP in the header, so that I orient instantly.
22. As the trader, I want to press `w` to cycle expiries (nearest weekly → next → monthly), so that I switch contracts without leaving the page.
23. As the trader, I want to move to a strike and press `B`/`S` to open the order form pre-filled with that option, so that ladder trading takes seconds.
24. As the trader, I want the chain marked as stale when data is from a closed market or broken feeds, so that I never mistake yesterday for now.

### Order Placement

25. As the trader, I want an order form pre-filled from the chain or position context, so that I only choose quantity and confirm.
26. As the trader, I want market-protected orders as the default (limit price auto-set at LTP ± a protective band: <₹10 → 5%, ₹10–100 → 3%, ₹100–500 → 2%, >₹500 → 1%), so that market-like speed without a price-runaway fill.
27. As the trader, I want to switch the same form to an explicit LIMIT order with a price field, so that I can work orders at my price when I choose.
28. As the trader, I want quantity in lots with the qty→units→value shown live, so that I always see what I'm committing before confirm.
29. As the trader, I want a confirmation screen showing order value, protection price, and the slice plan (how many child orders, each with qty) before anything is sent, so that fat fingers die on the confirm screen.
30. As the trader, I want a second confirmation when the order exceeds 10 lots, so that big size can't be placed reflexively.
31. As the trader, I want orders above the freeze limit sliced automatically (each slice ≤ freeze qty from the instruments table), so that I never manually split or eat a rejection.
32. As the trader, I want all slices placed at once (fire-all), each with per-slice status shown in the order book, so that large size executes fast with full visibility.
33. As the trader, I want to choose the broker on the form (auto: position's broker for closes, last-used for opens; `Tab` toggles), so that routing is automatic where it can be and manual where I want it.
34. As the trader, I want the market-hours guard to refuse new orders outside 09:15–15:30 IST on trading days, so that I never waste a round-trip on a guaranteed rejection.

### Order Management & Reliability

35. As the trader, I want every order to carry a client-generated idempotency tag, so that a retry after a transport failure can never double-place.
36. As the trader, I want transport failures retried (≤2) with fresh tags, but broker rejections never auto-retried, so that errors surface instead of compounding.
37. As the trader, I want a margin rejection specifically to stop any further slices of the parent order and alert immediately, so that partial positions never silently grow into unaffordable ones.
38. As the trader, I want an order book showing parent orders, their child slices, statuses, and fill progress, so that sliced executions are transparent.
39. As the trader, I want to cancel a pending slice from the order book, so that runaway protected orders can be cleaned up.
40. As the trader, I want to modify a pending order's price (and quantity), so that I can work orders without cancel/re-place.
41. As the trader, I want a reconciliation loop (poll broker order/trade books on schedule and converge local state to broker truth), so that the TUI shows real fills even when callbacks are missed.
42. As the trader, I want every order, modification, cancellation, and login written to an immutable audit log, so that anything odd is explainable after the fact.
43. As the trader, I want today's P&L in the positions header, so that session state is always visible.

### Instruments & Limits

44. As the trader, I want lot sizes and freeze quantities stored per instrument and refreshed nightly, checked at login, and re-checked by a background cron, so that the slicer is never using stale exchange rules.
45. As the trader, I want an NSE freeze-limit revision (like the 2026-10-05 NIFTY 1,800 → 3,510 change) picked up automatically, so that I neither under-slice nor over-slice after exchange circulars.
46. As the trader, I want a loud alert when the instrument/limits refresh fails before market open, so that stale limits stop trading instead of silently applying.
47. As the trader, I want symbol discrepancies between Kite and Fyers resolved through one canonical instrument model, so that cross-broker netting and routing are consistent.

### Risk

48. As the trader, I want a configurable per-order lots cap (default: the freeze lots), so that no single order exceeds what I intend.
49. As the trader, I want an optional day-max lots per underlying (default off), so that I can bound daily size when I choose to.

## Implementation Decisions

### Architecture

- Single Go binary, three cooperating layers inside one process: **SSH+TUI frontend**, **trading core** (positions, chain, order engine), **broker adapters**. The trading core must be usable headless (a future REST/CLI can drive it), so the TUI only renders state and submits intents.
- Go 1.27 module. Charm stack: `charmbracelet/wish` (SSH server serving TUIs) + `bubbletea` (TUI) + `lipgloss` (styling) + `huh` (forms). MongoDB via the official `mongo-go-driver`, localhost binding only.
- Fresh greenfield codebase (repo currently contains only `.gitignore`).

### Module Boundaries

| Module | Owns |
|---|---|
| `sshgateway` | Wish server, pubkey auth, TOTP gate, session management |
| `tui` | Pages: positions, option chain, funds, order form, order book; keybindings; rendering |
| `core` | Canonical instrument model, netting, P&L, order engine (state machine, slicing, idempotency, retries, reconciliation), risk caps, market-hours guard |
| `brokers` | `BrokerAdapter` interface + Kite + Fyers implementations; symbol translation only here |
| `marketdata` | WS fan-in (Kite ticker, Fyers DataSocket), REST fallback pollers, staleness marking |
| `store` | Mongo persistence: credentials, instruments, orders (parent+children), trades, audit log |
| `ops` | Session refresh jobs (Fyers daily cron), instrument refresh (nightly + login check + background cron), alerting |

The **`BrokerAdapter` interface** is the system's key seam: a broker-agnostic contract — place/cancel/modify order, orders/trades/positions/funds snapshots, instrument dump, quote subscription — implemented once per broker behind canonical types. Kite's and Fyers' SDKs (`zerodha/gokiteconnect/v4`, `FyersDev/fyers-go-sdk`) are wrapped behind it; nothing outside `brokers` imports either SDK.

### Key Decisions

1. **Canonical instrument model.** Key: `underlying + instrument-type (OPTIDX/OPTSTK) + expiry-date + strike + CE/PE`. Every broker symbol maps to exactly one canonical key. Netting, chain display, order routing all operate on canonical keys; translation happens at the adapter boundary. Lot size and freeze qty are attributes fetched from exchange/broker data — never hardcoded (NSE revised NIFTY freeze from 1,800 → 3,510 units effective 2026-10-05; any hardcoded limit would already be wrong).
2. **Broker sessions.** Kite: daily manual paste of request_token (press `L`) → GenerateSession → store access token. Fyers: daily auto-refresh from stored refresh token (cron ~08:30 IST); manual fallback identical to Kite's flow. Both access/refresh tokens stored encrypted (envelope encryption: master key from env var, never in DB).
3. **Order state machine.** States: `INTENT → CONFIRMED → PLACING → PARTIALLY_FILLED/FILLED/CANCELLED/REJECTED` per child slice; parent aggregates children. Transitions driven by WS order updates **and** a reconciliation poller (positions@1s, orders every 10s when any pending, trades on each fill event) — poll wins on disagreement, broker is always truth.
4. **Idempotency.** Every child order gets a client-generated unique tag at creation (Kite `tag`, Fyers `clientOrderId`). Retries use a fresh tag (new intent), never resubmit an identical tag after an ambiguous transport failure; a reconciliation pass resolves ambiguity before any retry.
5. **Retries & failures.** Transport errors (timeout, conn reset): retry ≤2 with backoff, fresh tag. Broker *rejections*: never auto-retried; surfaced with the broker's message. **Margin rejection specifically halts remaining slices of the parent** and raises the alert.
6. **Slicing.** Fire-all-at-once: compute slices with each ≤ freeze qty from instruments table (e.g. NIFTY 100 lots = 6,500 units → 2 slices of 3,500 each under the current 3,510 limit; note NSE raised limits ~2× on 2026-10-05, so 27-lot slices are historical). Slices share a parent group ID; order book shows parent + children; brokerage is per-slice anyway. Slice count shown on the confirm screen.
7. **Execution policy.** Default market-protected limit: limit at LTP ± band (<₹10 → 5%, ₹10–100 → 3%, ₹100–500 → 2%, >₹500 → 1%; option bands from Zerodha's published market-protection table). Raw market orders are never sent. Explicit LIMIT mode available in the same form (price field editable, protection hidden). Form order: qty in lots → live units/value → confirm.
8. **Order actions.** Cancel: adapter cancel on pending slice. Modify: adapter modify for price and quantity (subject to freeze limits — a modify exceeding them is rejected client-side with a slice-plan hint). No cancel-all kill switch in v1.
9. **Routing.** Close-position intent → broker that holds the position (forced). Open intent → last-used broker. `Tab` on the form overrides (color-coded per broker). Netted rows are never orderable directly — the trader selects a broker row or the form shows a broker choice.
10. **Market data.** WS-first: Kite ticker (quote mode for chain strikes + position instruments), Fyers DataSocket; both SDKs provide built-in reconnect — wire `OnReconnect`/`OnNoReconnect` (Kite) and equivalent Fyers callbacks to mark feeds stale and flip the page to REST fallback. REST fallback cadence: chain every 10s, positions every 1s. Staleness shown in-page ("LIVE", "POLL", "STALE + timestamp").
10-sec/1-sec targets are met by WS comfortably; polling is the safety net.
11. **Refreshes.** Instruments: nightly job + check on login + background cron (default hourly, configurable); sources: broker instrument dumps (Kite CSV, Fyers master) + NSE `NSE_FO_contract_ddmmyyyy.csv.gz` (freeze qty + lot size; needs browser-like headers); a validated fallback table ships as config. Alert (TUI banner + log) when refresh fails or data is older than 24h at market open — orders are blocked only for instruments whose limits are stale.
12. **Market-hours guard.** Orders blocked outside 09:15–15:30 IST trading days (holiday list refreshed with instruments). Positions/chain/funds pages remain readable pre-open with "market closed / data stale" marking. Blocked order shows clear message; nothing reaches the broker.
13. **Risk caps.** Per-order lots cap (default = freeze lots; 2× cap requires re-tying the quantity). Big-order threshold 10 lots → second confirm. Day cap per underlying optional (default off, enforced client-side at intent time).
14. **Auth.** SSH pubkey (authorized via deployment) + in-app TOTP gate (pquerna/otp): first run generates a QR + otpauth URL; 6 digits; 3 failures → disconnect. IP whitelisting handled at deployment; the app is defense-in-depth behind it.
15. **Data model (Mongo).** Collections: `users` (v1: single user; every other collection carries `user_id`), `broker_credentials` (encrypted blobs, per-broker app keys + tokens), `instruments` (canonical key + per-broker symbol map + lot size + freeze qty + expiries), `orders` (parent docs: intent, routing, state, slice plan; child docs: tag, broker order id, state, fills), `trades`, `audit_log` (append-only), `app_config` (risk caps, refresh schedules, TOTP secret hash). Positions/funds are **not** persisted — always live broker snapshots (WS + poll).
16. **TUI pages & keys.** `1` positions · `2` option chain · `3` funds · `4` order book; `B/S` buy/sell, `C` close, `Q` square-off-all (with per-position confirm), `Tab` broker toggle, `L` broker login/refresh, `w` cycle expiry, arrows navigate, `q` back, `Esc`/`Ctrl+C` exits form not app, `X` cancel order, `M` modify order. Help overlay `?` always available.
17. **Deployment.** Single Linux VPS, static IP (required for Kite/Fyers API whitelisting), one binary + local mongod (localhost bind), systemd units, SSH on 22 serving the TUI. Domain: trade.tradeapp.in. All secrets via environment (encryption master key, TOTP bootstrap); database stores only ciphertext.
18. **Symbol discrepancy handling.** Where Kite and Fyers disagree on tradingsymbol format (they do: `NIFTY26O1030000CE` vs `NSE:NIFTY2610330000CE`-style), each adapter parses its own dump into the canonical key; a nightly diff report logs un-mappable symbols rather than guessing.

## Testing Decisions

**What makes a good test here:** only test external behavior at the module seams — no tests peeking at internal struct fields or goroutine plumbing. The system is a proxy for real money, so tests target the decision logic that can lose money (slicing, protection, idempotency, state convergence) rather than TUI cosmetics.

**Primary seam (one): the `BrokerAdapter` interface.** All broker-agnostic logic (order engine, netting, chain, slicer) is tested against a **simulator adapter** — a fake BrokerAdapter with scriptable behavior: accepts/rejects/fills on command, artificial latency, dropouts, partial fills, disconnects. This is also the seam a future REST/CLI uses, and it lets the entire trading core run in CI without broker credentials or network.

**Modules tested and how:**

- `core` order engine — table-driven tests against the simulator: slicing arithmetic (freeze boundary math incl. non-multiples), protection band prices, retry/fresh-tag rules, margin-rejection halts remaining slices, state machine transitions with out-of-order callbacks, reconciliation convergence (poll wins over missed WS).
- `brokers` adapters — contract tests: one suite, run against both real adapters pointed at SDK mock transports (gokiteconnect ships a mock_responses suite; Fyers SDK responses are JSON — fixture-based), asserting canonical-type round-trips (place → canonical order, positions → canonical positions). Symbol translation tests use recorded instrument-dump fixtures.
- `marketdata` — WS fan-in and staleness: unit-test the merge/reconnect state machine with fake tick sources; REST fallback timer logic with a fake clock.
- `ops` refresh jobs — fake clock + fake fetch: nightly/login/cron refresh paths, NSE file parse (fixture), stale-data blocking rules.
- `sshgateway`/`tui` — light: TOTP gate logic (unit), keybinding→intent mapping (unit against the headless core). Full TUI rendering is smoke-tested, pixel assertions are out of scope.

**Prior art:** greenfield repo — no existing tests. The golang-testing skill's table-driven style and the adapter/contract pattern above set the precedent for everything that follows.

## Out of Scope

- Multi-tenant accounts, signup, billing — the data model is ready (`user_id` everywhere) but v1 ships one user.
- Auto-login for Kite (browser/TOTP automation) — stays manual (`L` + paste).
- Cancel-all kill switch — deliberately excluded in v1.
- IV, Greeks, Black-Scholes anything — v2 candidate.
- Charting / historical candles / indicators.
- Stop-loss / take-profit attached orders (BO/CO are deprecated at Fyers; Kite GTT is a different beast) — v2 candidate.
- Strategy baskets (straddles/multi-leg), spread ordering, TWAP/interval slicing — v1 slices are fire-all.
- Commodity/currency segments — NSE F&O only.
- Mobile app, web UI, REST API — the SSH TUI is v1's only interface (core is headless-capable for later).
- Docker deployment — systemd is v1's deployment story.
- Real-time exchange holiday calendar integration beyond the instruments refresh pipeline.
- Broker rate-limit management beyond simple backoff (v1 usage is well within Kite/Fyers limits).

## Further Notes

- **Reliability posture** (the reason this exists): never a raw market order (market-protected default), never a double-place (idempotency + fresh-tag retries + reconcile-before-retry), never silently-stale limits (three-layer refresh + stale-blocks-trading), never an unnoticed partial failure (per-slice status + margin-halt + audit everything).
- **Freeze limits move**: NSE doubled index freeze limits effective 2026-10-05 (NIFTY 1,800 → 3,510 units, 27 → 54 lots). Any design assuming fixed limits is already wrong — hence instruments-table-driven slicing everywhere, including modify-time re-validation.
- **Broker discrepancy is first-class**: the canonical model exists because two brokers name the same contract differently; netting and routing correctness depend on it, and the nightly diff report catches new drift.
- **Cost of slicing**: brokerage applies per executed slice, exactly as Zerodha's own auto-slice behaves; the confirm screen shows the slice count so cost is visible before commit.
- **Fyers token lifetime**: refresh tokens are long-lived (~15 days) — the daily cron keeps access tokens fresh; when the refresh token itself expires, the manual flow takes over (alert a day ahead if refresh-fail streak > 3).
- **Order of implementation** suggested for ticketing: canonical instruments + adapters → market data → positions/funds pages → order engine (slicer + protection + idempotency) → TUI order form + chain → order book/modify → sessions/refresh jobs → TOTP gate → square-off-all.
- This spec may be deleted after implementation per repo convention; the durable record is the git history and the audit log design itself.