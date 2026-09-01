# sumzero

A double-entry ledger for Postgres. Every transfer nets to zero, history is
append-only, and balances can be recomputed from the postings at any time.

Use it as a Go package inside your service, or run it as a single binary with a
REST API. Same engine either way.

[![CI](https://github.com/YusufDrymz/sumzero/actions/workflows/ci.yml/badge.svg)](https://github.com/YusufDrymz/sumzero/actions)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

> **Status: early.** The domain model, schema and invariant tests are in place.
> The Postgres store, REST API and CLI are being built. Not ready for anyone's
> money yet — see [Roadmap](#roadmap).

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
go get github.com/YusufDrymz/sumzero
```

## Usage

```go
lg := ledger.New(pool) // *pgxpool.Pool — your database, your migrations

lg.CreateAccount(ctx, ledger.Account{ID: "cash", Type: ledger.Asset, Currency: "TRY"})
lg.CreateAccount(ctx, ledger.Account{ID: "revenue", Type: ledger.Income, Currency: "TRY"})

// A 100.00 TRY sale, 10.00 of it commission.
err := lg.Post(ctx, (&ledger.Transfer{Reference: "order-1071", Description: "sale"}).
    Debit("cash", ledger.Amount(9000, "TRY")).
    Debit("fees", ledger.Amount(1000, "TRY")).
    Credit("revenue", ledger.Amount(10000, "TRY")))
```

`Reference` is your own id for the movement. It is unique, so a retried request
cannot post the same transfer twice.

Account types and the side they carry a positive balance on:

| Type | Normal side |
|------|-------------|
| `Asset`, `Expense` | debit |
| `Liability`, `Equity`, `Income` | credit |

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
| 2 | Postgres store: post, balance, statement, as-of queries | in progress |
| 3 | REST API with mandatory idempotency keys | |
| 4 | `verify` command, reconciliation against external records | |

## Design notes

Decisions and their reasoning live in [`docs/adr/`](docs/adr).

## Development

```bash
go test -race -cover ./...
```

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

**Kapsam dışı:** Bu bir muhasebe sistemi değil, posting motoru. Vergi kuralları,
resmi raporlar, hesap planı şablonları, döviz kuru kaynağı yok. Ne hareket
ettiğini kaydeder ve kaydın bozulmadığını kanıtlar.

</details>

## License

MIT — see [LICENSE](LICENSE).
