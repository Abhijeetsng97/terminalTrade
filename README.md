# terminalTrade

`ssh trade.tradeapp.in` → a full-screen terminal trading UI for Indian index options, across Kite (Zerodha) and Fyers. Plus a REST API driving the same trading core.

Built as a **reliable order-placement proxy**: never a raw market order, never a double-placed order, never a silently-stale freeze limit.

## Features (v1)

- **SSH terminal** (pubkey + TOTP second factor) — no browser needed, works over any SSH client
- **Multi-broker**: Kite + Fyers behind one canonical instrument model; positions **netted** across brokers
- **Option chain** with WS-fed quotes (REST 10s fallback), ATM-centered, ladder trade in seconds
- **Positions page** (1s refresh), funds page, today's P&L
- **Orders**: market-protected limit orders by default (band: <₹10→5%, ₹10–100→3%, ₹100–500→2%, >₹500→1%), explicit LIMIT available, **automatic freeze-quantity slicing** (fire-all, per-slice status), cancel + modify
- **Reliability core**: idempotency tags on every placement, transport-only retries with fresh tags, **margin rejection halts remaining slices**, reconciliation loop (broker truth wins), full audit log
- **Dynamic limits**: lot sizes and freeze quantities loaded from broker dumps + refreshed (startup/hourly/nightly) — NSE revises them by circular (NIFTY: 1800→3510 on 2026-10-05)
- **Market-hours guard** (09:15–15:30 IST, holiday-aware), risk caps (per-order lots cap, big-order second confirm, optional day cap)
- **Sessions**: Kite daily manual paste (`L` in TUI or `POST /api/sessions/KITE/token`); Fyers daily auto-refresh from its refresh token
- **REST API** with Postman collection (`postman/terminaltrade-api.postman_collection.json`)

## Quick start

```bash
cp .env.example .env         # fill in keys; TT_ENCRYPTION_KEY is required
openssl rand -hex 32         # -> TT_ENCRYPTION_KEY
go build -o terminaltrade ./cmd/terminaltrade
./terminaltrade
```

First run: the TOTP secret is generated and printed (add it to your authenticator; it's persisted to Mongo). Connect: `ssh -p 2222 localhost`, enter the 6-digit code.

Config lives in `.env` (see `.env.example`): broker API keys, Mongo URI, SSH/API ports, authorized SSH public keys, TOTP secret, encryption key.

## Architecture

```
ssh/terminal ──┐               ┌── REST API (bearer-token)
               ├─ app core ───┤
  bubbletea UI ─┘   (engine)   └── future CLI

engine ── BrokerAdapter seam ── Kite adapter (gokiteconnect)
                                  Fyers adapter (fyers-go-sdk)
                                  Simulator (tests: no broker, no network)

Mongo: users, broker_credentials (AES-GCM envelope-encrypted),
instruments, orders (parent+slices), trades, audit_log, app_config
```

The **BrokerAdapter interface** is the system's one seam: all broker-agnostic logic (slicing, protection pricing, netting, retries, halts, reconciliation) is tested against the simulator adapter in CI.

## Order path (the reliability story)

1. Intent validated: lots > 0, broker lists the instrument, market open, caps ok, price on tick
2. MKT-PROT priced: limit at LTP ± protection band (never a raw market order)
3. Sliced: each slice ≤ freeze qty from the instruments table (NIFTY 100 lots → 3510 + 2990)
4. Each slice gets a unique idempotency tag; transport failures retry ≤2 with **fresh tags**; broker rejections never retry
5. **Margin rejection halts remaining slices** and alerts
6. Reconciliation poll converges local state to broker truth
7. Everything audited

## Testing

```bash
go test ./...
```

Money-logic is table-driven against the simulator: slicing math (freeze boundaries, old and new NSE limits), protection bands, market-hours boundaries, netting, margin-halt, fresh-tag retries, reconciliation.

## Deployment (VPS)

- Static-IP Linux VPS (broker API whitelists need it), DNS `trade.tradeapp.in` → VPS
- `mongod` bound to localhost; one binary; systemd units
- SSH port per `TT_SSH_PORT`; API binds 127.0.0.1 — front with nginx/Caddy + TLS if remote API access is needed
- Set `TT_AUTHORIZED_KEYS` to a file with your SSH public key (fail-closed: unset denies all)

## Docs

- `docs/specs/terminaltrade-v1.md` — the v1 design spec
- `docs/design/tui-pages.md` — page-by-page TUI design
- `postman/terminaltrade-api.postman_collection.json` — API collection

## Status

v1 implemented: core, engine, adapters, market data, API, TUI, SSH, ops, tests. WS quote streaming wiring for live broker feeds (Kite ticker / Fyers DataSocket callbacks into the quote cache) and NSE freeze-file parsing are the next integration steps; freeze fallbacks are in place.