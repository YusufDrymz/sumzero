# sumzero

A double-entry ledger for Postgres. Every transfer nets to zero, history is
append-only, and balances can be recomputed from the postings at any time.

Use it as a Go package inside your service, or run it as a single binary with a
REST API. Same engine either way.

[![CI](https://github.com/YusufDrymz/sumzero/actions/workflows/ci.yml/badge.svg)](https://github.com/YusufDrymz/sumzero/actions)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

> **Status: v0.1.0.** Engine, REST API, `verify` and `reconcile` work against
> Postgres and are tested there on 16 and 17. Nobody has run it in anger yet.
> Treat it accordingly — see [Roadmap](#roadmap).

## Why

Most services grow their own ledger: a `balance` column, an `UPDATE`, and a
transactions table nobody trusts. It works until someone asks what the balance
was last Tuesday, or why it is off by one kuruş.

sumzero is the boring version of that table, done properly:

- **Integer money.** Minor units and an ISO 4217 code. No floats, and amounts
  cross the API as strings so no JSON parser rounds them.
- **Sum zero, always.** Debits equal credits within each currency, or the
  transfer is rejected atomically. A transfer may span currencies as long as it
  balances in each one.
- **Append-only history.** No `UPDATE`, no `DELETE` — enforced by a database
  trigger, not just convention. Corrections are reversing transfers, which is
  what makes an as-of balance meaningful.
- **Tamper-evident.** Each transfer carries the digest of the previous one, so
  a rewritten or deleted row is detectable rather than silent.
- **Your schema.** Plain tables, documented, queryable with `psql`. No lock-in.

## Not this

Scope discipline matters more than features in a ledger, so: this is a posting
engine, **not an accounting system**. No tax rules, no statutory reports, no
chart-of-accounts templates, no FX rate sourcing. It records what moved and
proves the record is intact. What that means for your books is your domain.

## Install

```bash
go get github.com/YusufDrymz/sumzero              # library
go install github.com/YusufDrymz/sumzero/cmd/sumzero@latest   # CLI
```

## Usage

Apply [`migrations/0001_init.sql`](migrations/0001_init.sql) with whatever tool
you already use, then:

```go
lg := ledger.New(pool) // *pgxpool.Pool — your database

lg.CreateAccount(ctx, ledger.Account{ID: "cash", Type: ledger.Asset, Currency: "TRY"})
lg.CreateAccount(ctx, ledger.Account{ID: "fees", Type: ledger.Expense, Currency: "TRY"})
lg.CreateAccount(ctx, ledger.Account{ID: "revenue", Type: ledger.Income, Currency: "TRY"})

// A 100.00 TRY sale, 10.00 of it commission.
_, err := lg.Post(ctx, (&ledger.Transfer{Reference: "order-1071", Description: "sale"}).
    Debit("cash", ledger.Amount(9000, "TRY")).
    Debit("fees", ledger.Amount(1000, "TRY")).
    Credit("revenue", ledger.Amount(10000, "TRY")))

bal, _ := lg.Balance(ctx, "cash")                        // 9000 TRY
was, _ := lg.BalanceAsOf(ctx, "cash", lastTuesday)       // recomputed from history
lines, _ := lg.Statement(ctx, "cash", ledger.StatementOptions{Limit: 50})
```

`Reference` is your own id for the movement. It is unique, so a retried request
cannot post the same transfer twice.

### Inside your own transaction

This is the part a separate ledger service cannot do. `NewTx` joins the
transaction you are already in, so the entry and the thing it describes commit
together or not at all — no "payment saved, ledger missed it" window:

```go
tx, _ := pool.Begin(ctx)
defer tx.Rollback(ctx)

order.MarkPaid(ctx, tx, orderID)
_, err := ledger.NewTx(tx).Post(ctx, (&ledger.Transfer{Reference: orderID}).
    Debit("cash", ledger.Amount(9000, "TRY")).
    Credit("revenue", ledger.Amount(9000, "TRY")))
if err != nil {
    return err // nothing was written
}
return tx.Commit(ctx)
```

One caveat: `Post` takes the hash-chain lock, and a lock lives until the
transaction ends — which in embedded mode is *your* commit. Post the transfer
as late as you can and commit promptly; a caller that posts first and then
spends seconds on other work is holding every other writer behind it.

Account types and the side they carry a positive balance on:

| Type | Normal side |
|------|-------------|
| `Asset`, `Expense` | debit |
| `Liability`, `Equity`, `Income` | credit |

## REST API

```bash
sumzero serve --dsn postgres://... --addr :8080
```

| | |
|---|---|
| `POST /v1/accounts` | open an account |
| `GET /v1/accounts/{id}` | one account |
| `POST /v1/accounts/{id}/archive` | close it to new postings |
| `GET /v1/accounts/{id}/balance` | current balance, or `?as_of=<RFC3339>` |
| `GET /v1/accounts/{id}/available` | balance minus active holds |
| `GET /v1/accounts/{id}/statement` | postings, `?from=&to=&limit=` |
| `POST /v1/accounts/{id}/reconcile` | compare the account with an external record |
| `POST /v1/transfers` | post a transfer — **`Idempotency-Key` required** |
| `POST /v1/holds` | reserve an amount — key required |
| `GET /v1/holds/{reference}` | one hold |
| `POST /v1/holds/{reference}/capture` | move the money, close the hold — key required |
| `POST /v1/holds/{reference}/release` | cancel the hold — key required |
| `GET /v1/transfers/{reference}` | one transfer with its postings |
| `GET /v1/verify` | verification report; 409 when the books do not check out |
| `GET /healthz` `GET /readyz` | liveness and readiness, deliberately separate |

```bash
curl -X POST localhost:8080/v1/transfers   -H 'Idempotency-Key: 4f3c…'   -d '{"reference":"order-9","postings":[
        {"account":"cash",   "amount":{"amount":"10000","currency":"TRY"},"direction":"debit"},
        {"account":"revenue","amount":{"amount":"10000","currency":"TRY"},"direction":"credit"}]}'
```

Amounts are strings on the wire, in both directions, and a JSON number is
rejected rather than rounded.

The key is not optional on any write: a transfer that cannot be retried safely
is a transfer whose client has no answer after a timeout. Repeating a request replays the
original response (`Idempotent-Replay: true`); reusing a key with a different
body is a 422. Keys live in Postgres, via
[go-idempotent](https://github.com/YusufDrymz/go-idempotent) — see
[ADR-0004](docs/adr/0004-idempotency-key-is-mandatory.md).

Errors always have the same shape:

```json
{"error": {"code": "unbalanced", "message": "sumzero: transfer does not balance: TRY off by 100"}}
```

400 means the request was malformed, 422 means the books refuse it, 409 means it
collided with something already recorded.

## Holds and the overdraft guard

By default the ledger records and never refuses: a journal does not argue with
its bookkeeper. An account that models a real, spendable balance is different.
Open it with `allow_negative: false` and it gets a guard — a transfer that
would push it below zero is refused and rolled back.

Holds are how a guarded account handles "we intend to take this, but not yet":
the card was authorised, the payout is queued, the withdrawal is under review.

```go
lg.CreateAccount(ctx, ledger.Account{ID: "wallet", Type: ledger.Asset, Currency: "TRY", AllowNegative: false})

h, _ := lg.Hold(ctx, ledger.HoldRequest{Account: "wallet", Reference: "auth-9", Amount: ledger.Amount(6000, "TRY"),
    ExpiresAt: time.Now().Add(7 * 24 * time.Hour)})

avail, _ := lg.Available(ctx, "wallet") // balance − active holds

// Later: take 5500 of the 6000. The rest is released in the same transaction.
lg.Capture(ctx, "auth-9", (&ledger.Transfer{Reference: "order-9"}).
    Credit("wallet", ledger.Amount(5500, "TRY")).
    Debit("merchant", ledger.Amount(5500, "TRY")))

// Or never take it.
lg.Release(ctx, "auth-9")
```

A hold moves nothing and is not part of history: `verify` does not audit it.
It reserves until captured, released, or expired — and expiry is applied by a
sweep (`serve` runs one every minute; embedded callers schedule `ExpireHolds`
themselves). [ADR-0006](docs/adr/0006-holds-and-the-overdraft-guard.md).

An account holds one currency. A customer with TRY and USD has two accounts;
an FX movement is a four-leg transfer between them.
[ADR-0007](docs/adr/0007-one-currency-per-account.md) says why that will stay.

## Reconcile

The ledger says what should have moved. The bank says what did. `reconcile`
lines the two up by reference and reports the difference:

```console
$ sumzero reconcile --account cash --file payouts-june.csv --from 2026-06-01 --to 2026-06-30
cash (TRY) 2026-06-01 → 2026-06-30
  matched 1418 · amount mismatch 3 · missing in ledger 2 · missing externally 1
  ledger 48120050 · external 48118810 · difference 1240
  ~ pay-88213                ledger 5000, external 4990
  + pay-90001                3000  (external only: money moved, not recorded)
  - pay-88790                7000  (ledger only: recorded, money did not move)
```

The CSV is `reference,amount[,date]`, amounts as signed minor units — there is
deliberately no decimal parsing at the file boundary. Matching is by reference
and by nothing else; fuzzy pairing on amount and date finds more matches and
also invents some, and this tool would rather say "unmatched" than be quietly
wrong ([ADR-0005](docs/adr/0005-reconcile-on-reference-only.md)). Exit 1 when
the sides disagree.

The same comparison is available as `POST /v1/accounts/{id}/reconcile` with the
entries in the body.

## Verify

`account_balances` is a cache, so the tool refuses to trust it:

```console
$ sumzero verify --dsn postgres://...
14 accounts, 2841 transfers, 7106 postings (243ms)
ok: balances match the postings and the chain is intact
```

Every balance is recomputed from the postings, every transfer is re-checked for
sum zero, and the hash chain is walked from the first row. When something is
wrong it says what and where:

```console
$ sumzero verify
14 accounts, 2841 transfers, 7106 postings (251ms)

3 problem(s):
  balance-drift cash: cached 918400, postings say 918300 (off by 100)
  hash-mismatch transfer 1904 (order-7781): stored hash does not describe the stored postings
  trial-balance TRY: accounts sum to 100, expected 0
```

Exit codes: `0` clean, `1` problems found, `2` the check could not run — so it
works as a cron job or a CI step. Add `--json` for machine output.

## Schema

Four tables: `accounts`, `transfers`, `postings`, `account_balances`. See
[`migrations/0001_init.sql`](migrations/0001_init.sql) — it is commented, and
the comments explain the decisions, not the syntax.

`account_balances` is a cache. The postings are the truth, and `sumzero verify`
recomputes every balance from them and re-walks the hash chain.

## Roadmap

| Phase | Contents | State |
|-------|----------|-------|
| 1 | Domain model, sum-zero invariant, schema, hash chain | done |
| 2 | Postgres store: post, balance, as-of, statement, embedded mode | done |
| 3 | `verify`: recompute balances, re-walk the chain | done |
| 4 | REST API with mandatory idempotency keys | done |
| 5 | Reconciliation against external records | done |
| 6 | v0.1.0 | done |
| 7 | Holds, overdraft guard | done |
| 8 | Per-book chains (only if the write ceiling ever binds) | later |

## Throughput

Writes are serialised: the hash chain is one sequence, so `Post` takes a lock
while it links a transfer. That puts the ceiling at roughly one Postgres
round-trip per transfer, which suits a service recording its own payments and
does not suit exchange-grade volume. [ADR-0003](docs/adr/0003-chain-serialises-writes.md)
has the reasoning and the way out if it ever binds.

## Design notes

Decisions and their reasoning live in [`docs/adr/`](docs/adr).

## Development

```bash
go test -short ./...              # unit tests only
go test -race -cover ./...        # adds e2e against a throwaway Postgres (needs Docker)
```

`SUMZERO_TEST_PG_IMAGE` picks the image; CI runs the suite against 16 and 17.

<details>
<summary>🇹🇷 Türkçe</summary>

Postgres üstünde çift taraflı defter. Her transfer para birimi başına sıfırda
dengelenir, geçmiş append-only'dir, bakiyeler her an posting'lerden yeniden
hesaplanabilir.

İki kullanım biçimi, tek motor: kendi servisinin içinde Go paketi olarak
(`ledger.New(pool)`), ya da REST API'li tek binary olarak.

**Durum: erken.** Domain modeli, şema ve invariant testleri hazır; Postgres
store, REST API ve CLI yazılıyor. Henüz kimsenin parasına hazır değil.

İlkeler: para **int64 minor unit** (kuruş) + ISO 4217 kodu, float yok ve API'de
string olarak taşınır (JSON parser yuvarlamasın); her transferde borç = alacak,
aksi halde atomik reddedilir; `UPDATE`/`DELETE` yok — trigger seviyesinde
yasak, düzeltme ters kayıtla yapılır (as-of bakiye ancak böyle anlamlı olur);
her transfer bir öncekinin hash'ini taşır, silinen/değiştirilen satır sessiz
kalmaz; şema public ve dokümante, `psql` ile sorgulanır, lock-in yok.

**Gömülü mod** ayrı bir defter servisinin yapamadığı şey: `ledger.NewTx(tx)` ile
çağıranın açık transaction'ına katılır, yani kayıt ile onu doğuran iş birlikte
commit olur ya da birlikte geri alınır. "Ödeme kaydedildi ama ledger kaçırdı"
aralığı mimari olarak yok.

**Yazma serileşir:** hash zinciri tek sıra olduğu için `Post` link atarken kilit
alır. Tavan, transfer başına bir Postgres round-trip'i civarı — kendi
ödemelerini kaydeden bir servise uygun, borsa hacmine değil (ADR-0003).

**REST API** (`sumzero serve`): hesap/transfer/bakiye/ekstre uçları, `as_of`
destekli bakiye, `/healthz` ve `/readyz` ayrı. `POST /v1/transfers` için
**`Idempotency-Key` zorunlu** — aynı key orijinal yanıtı replay eder, farklı
gövdeyle gelen aynı key 422 olur. Anahtarlar Postgres'te (go-idempotent'ın
`Store`'u), ADR-0004. Para alanları telde **string**, JSON number reddedilir.

**Hold / capture ve overdraft guard (v0.2):** `allow_negative: false` ile
açılan hesap eksiye düşemez; `Hold` tutar rezerve eder (para hareket etmez),
`Capture` transferi postlayıp hold'u aynı transaction'da kapatır (kısmi
capture kalanı serbest bırakır), `Release` iptal eder, süresi dolan hold'lar
dakikalık sweep ile düşer. `available = bakiye − aktif hold'lar`. Hesap tek
para birimi tutar, bu değişmeyecek (ADR-0007).

**`sumzero reconcile`**: banka/PSP CSV'sini (`reference,amount[,date]`,
kuruş cinsinden imzalı) defterle referans üzerinden eşleştirir; eşleşen, tutar
farkı, defterde eksik, dışta eksik olarak dört kovaya ayırır ve toplam farkı
verir. Bilerek yalnız referansla eşleşir — tutar/tarih bulanık eşleşmesi yok
(ADR-0005). API'de `POST /v1/accounts/{id}/reconcile`.

**`sumzero verify`** cache'e güvenmez: her bakiyeyi posting'lerden yeniden
hesaplar, her transferi sum-zero için yeniden kontrol eder, hash zincirini baştan
yürür. Çıkış kodları cron/CI için: 0 temiz, 1 sorun var, 2 kontrol koşamadı.

**Kapsam dışı:** Bu bir muhasebe sistemi değil, posting motoru. Vergi kuralları,
resmi raporlar, hesap planı şablonları, döviz kuru kaynağı yok. Ne hareket
ettiğini kaydeder ve kaydın bozulmadığını kanıtlar.

</details>

## License

MIT — see [LICENSE](LICENSE).
