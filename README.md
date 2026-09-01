# sumzero

A double-entry ledger for Postgres. Every transfer nets to zero, history is
append-only, and balances can be recomputed from the postings at any time.

Use it as a Go package inside your service, or run it as a single binary with a
REST API. Same engine either way.

[![CI](https://github.com/YusufDrymz/sumzero/actions/workflows/ci.yml/badge.svg)](https://github.com/YusufDrymz/sumzero/actions)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

> **Status: early.** The engine and `verify` work against Postgres and are
> tested there. The REST API is not written yet. Not ready for anyone's money —
> see [Roadmap](#roadmap).

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

Account types and the side they carry a positive balance on:

| Type | Normal side |
|------|-------------|
| `Asset`, `Expense` | debit |
| `Liability`, `Equity`, `Income` | credit |

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
| 4 | REST API with mandatory idempotency keys | next |
| 5 | Reconciliation against external records | |

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

**`sumzero verify`** cache'e güvenmez: her bakiyeyi posting'lerden yeniden
hesaplar, her transferi sum-zero için yeniden kontrol eder, hash zincirini baştan
yürür. Çıkış kodları cron/CI için: 0 temiz, 1 sorun var, 2 kontrol koşamadı.

**Kapsam dışı:** Bu bir muhasebe sistemi değil, posting motoru. Vergi kuralları,
resmi raporlar, hesap planı şablonları, döviz kuru kaynağı yok. Ne hareket
ettiğini kaydeder ve kaydın bozulmadığını kanıtlar.

</details>

## License

MIT — see [LICENSE](LICENSE).
