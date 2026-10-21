# SoroVault

[![CI](https://github.com/soroworks/sorovault/actions/workflows/ci.yml/badge.svg)](https://github.com/soroworks/sorovault/actions/workflows/ci.yml)
[![Security](https://github.com/soroworks/sorovault/actions/workflows/security.yml/badge.svg)](https://github.com/soroworks/sorovault/actions/workflows/security.yml)
[![Release](https://img.shields.io/github/v/release/soroworks/sorovault)](https://github.com/soroworks/sorovault/releases/latest)
[![Go](https://img.shields.io/github/go-mod/go-version/soroworks/sorovault)](go.mod)
[![License](https://img.shields.io/github/license/soroworks/sorovault)](LICENSE)

A contract metadata and interface (ABI) registry for the Stellar/Soroban network.

SoroVault fetches a deployed contract's WASM, decodes the interface embedded in
it, stores it, and serves a searchable registry — so tools and humans can
discover what functions and types a contract exposes.

**Status:** MVP. The core is complete and tested; see [open ground](#open-ground)
for where contributions are most welcome.

---

## Why

A deployed Soroban contract is a black box unless you have its interface.

The interface is right there in the uploaded WASM — Soroban embeds it as a
`contractspecv0` custom section — but getting at it means knowing which ledger
entries to read, how to walk a WASM binary, and how to decode a stream of XDR.
Every tool that wants to know what a contract does re-implements that.

SoroVault does it once and serves the result as clean JSON:

```console
$ curl -s localhost:8080/api/contracts/CDZZ…4PAN | jq '.interface.functions[].name'
"vote"
"has_voted"
"initialize"
"get_proposal"
"get_total_votes"
"get_vote_params"
"get_proposal_count"
```

It is the discovery layer for contract interfaces, and the natural companion to
a deployer (SoroForge) and a simulator (SoroProbe).

## What it does

- **Register** a contract by ID — fetch its WASM, decode the interface, store it
- **Search** the registry by contract ID or name
- **Serve** the decoded ABI as JSON: functions, arguments, return types, UDTs, events
- **Version** interfaces — when a contract is upgraded, the prior interface is kept
- **Find the contracts that share a WASM** — the question a vulnerability in a
  shared piece of code raises
- **Browse** it all in a small server-rendered UI

Records are namespaced by network, so the same contract ID on testnet and
mainnet are separate entries.

## Verified against

Contract spec handling evolves, so these were confirmed against live sources
rather than recalled. Verified **2026-08-07**:

| | Version | Note |
|---|---|---|
| Go | **1.25** | required by the SDK; `go.mod` pins `1.25.0` |
| [`github.com/stellar/go-stellar-sdk`](https://github.com/stellar/go-stellar-sdk) | **v0.7.1** | RPC client and XDR |
| Stellar RPC (testnet) | 27.1.1, protocol 27 | endpoint the decode path was exercised against |
| soroban-sdk (test fixture) | 25.3.0 | the contract in `internal/spec/testdata` |
| [SEP-48](https://github.com/stellar/stellar-protocol/blob/master/ecosystem/sep-0048.md) | 1.1.0 | contract interface specification |

> **On the SDK:** the original plan named `github.com/stellar/go`. That module is
> now formally deprecated — its `go.mod` carries
> `// Deprecated: Use github.com/stellar/go-stellar-sdk instead` — so SoroVault
> builds against the successor. This is also why the Go floor is 1.25 rather
> than the 1.22 originally targeted: the new SDK requires it.

### How the interface is actually retrieved

Worth writing down, because it is the part that is easy to get wrong:

1. **Contract instance.** Build a `LedgerKey` for `ContractData` with the
   contract's `ScAddress`, the key `ScvLedgerKeyContractInstance`, and
   `Persistent` durability. `getLedgerEntries` returns a `ContractDataEntry`
   whose `Val.Instance.Executable.WasmHash` names the module.
   - A Stellar asset contract has no `WasmHash` — it is built into the host, so
     there is no interface to decode. SoroVault reports this distinctly.
2. **Contract code.** Build a `LedgerKey` for `ContractCode` keyed by that hash.
   `getLedgerEntries` returns a `ContractCodeEntry` holding the module bytes.
   The hash is the SHA-256 of the module, which is what makes it a usable
   version identifier.
3. **Spec section.** Walk the WASM's section framing for the custom section
   named `contractspecv0`.
4. **Decode.** Its body is a **bare stream of XDR-encoded `SCSpecEntry` values**
   — no header, no delimiter, no length prefix between entries. Read it by
   calling `xdr.Unmarshal` in a loop until the reader is drained.
5. **Metadata.** The optional `contractmetav0` section holds `SCMetaEntry`
   key/value pairs (`rsver`, `rssdkver`). Its absence is not an error.
6. **Environment metadata.** The optional `contractenvmetav0` section holds the
   `SCEnvMetaEntry` that records which host interface the contract was built
   against — effectively the minimum protocol it needs. Its absence is not an
   error either.

The SDK does not parse WASM custom sections, so `internal/wasm` implements the
small amount of the binary format that step 3 needs.

## Quickstart

The fastest path is Docker, which brings up Postgres, applies migrations and
starts the server:

```console
$ git clone https://github.com/soroworks/sorovault && cd sorovault
$ make up
```

Then register a contract and browse it:

```console
$ curl -X POST localhost:8080/api/contracts \
    -H 'content-type: application/json' \
    -d '{"contract_id":"CDZZXVGUCLBUTE3WDD6TF2NXPTBPOYTVPPBBIZMSPVIQLUG226XJ4PAN"}'
```

Open <http://localhost:8080> for the browse UI.

### Kubernetes with Helm

The chart at [`deploy/helm/sorovault`](deploy/helm/sorovault) deploys the API
and runs database migrations before install and upgrade. It expects an
existing PostgreSQL database; it does not install or manage PostgreSQL. Create
a Secret containing the connection URL, then install the chart:

```console
$ kubectl create secret generic sorovault-db \\
  --from-literal=DATABASE_URL='postgres://sorovault:password@postgres:5432/sorovault?sslmode=disable'
$ helm upgrade --install sorovault ./deploy/helm/sorovault \\
  --set database.existingSecret=sorovault-db
```

The secret name and key, image, service, and SoroVault environment settings can
be overridden in a values file. `RPC_URL`, `NETWORK_PASSPHRASE`, `HTTP_ADDR`,
`LOG_LEVEL`, `RPC_TIMEOUT`, and `REFRESH_INTERVAL` are exposed under `config`;
use `extraEnv` for other environment variables. The server and migration Job
run non-root with read-only root filesystems.

### Running locally

```console
$ cp .env.example .env          # adjust as needed
$ set -a && source .env && set +a

$ docker compose up -d db       # or point DATABASE_URL at your own Postgres
$ go run ./cmd/sorovault migrate
$ go run ./cmd/sorovault add CDZZXVGUCLBUTE3WDD6TF2NXPTBPOYTVPPBBIZMSPVIQLUG226XJ4PAN
$ go run ./cmd/sorovault serve
```

## Configuration

All configuration is environment variables. Only `DATABASE_URL` (or the path in `DATABASE_URL_FILE`, read from the filesystem) is required.

| Variable | Default | Purpose |
|---|---|---|
| `DATABASE_URL` | — | Postgres connection string. **Required** (or set `DATABASE_URL_FILE` to read it from a file). |
| `MAX_CONCURRENT_REGISTRATIONS` | `4` | how many registrations and refreshes run at once |
| `RPC_URL` | `https://soroban-testnet.stellar.org` | Stellar RPC endpoint |
| `NETWORK_PASSPHRASE` | `Test SDF Network ; September 2015` | the network you expect `RPC_URL` to serve |
| `HTTP_ADDR` | `:8080` | listen address |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `RPC_TIMEOUT` | `30s` | bound on a single contract fetch |
| `REFRESH_INTERVAL` | `0` (off) | re-check registered contracts this often while serving; minimum `1m` |
| `REFRESH_WEBHOOK_URL` | — | post a JSON message here whenever a refresh finds an upgraded contract |
| `REFRESH_WEBHOOK_SECRET` | — | sign webhook payloads with HMAC-SHA256 |

`NETWORK_PASSPHRASE` is a guard, not a source of truth. SoroVault reads the real
passphrase from the RPC endpoint and refuses to proceed if the two disagree, so
a stale `RPC_URL` cannot quietly file mainnet contracts under `testnet`.

### Automatic refresh

Set `REFRESH_INTERVAL` (for example `6h`) and `sorovault serve` re-checks, on
that schedule, every contract on its network that has not been refreshed
within the interval. An upgraded contract gets a new interface version
exactly as `POST /api/contracts/{id}/refresh` would record it; an unchanged
one costs a single `getLedgerEntries` call and is not re-downloaded. Refreshes
are spread at even offsets across the interval to avoid an RPC traffic burst.

A contract that fails to refresh — expired, or an RPC error — is logged and
skipped, and the sweep carries on. One sweep runs at startup and then one per
interval; a sweep in progress at shutdown is cancelled before the database is
closed.

### Telling the world about an upgrade

Set `REFRESH_WEBHOOK_URL` and every contract whose interface changes during a
sweep is posted to it, one message per contract:

```json
{
  "contract_id": "CDZZ…4PAN",
  "network": "testnet",
  "old_wasm_hash": "c618da…0781",
  "new_wasm_hash": "9f2b71…4c3a",
  "interface_url": "/api/contracts/CDZZ…4PAN?network=testnet&wasm_hash=9f2b71…4c3a",
  "changed_at": "2026-08-07T12:00:00Z"
}
```

`interface_url` is a path, not an absolute URL: the instance does not know
the address it is served under, so it says where the interface is and lets
the receiver put its own prefix on it.

Delivery is best effort. The webhook is posted to once, with a five-second
timeout and no retries: a receiver that is down must not be able to stall the
sweep, and the sweep's job is to keep the registry up to date rather than to
keep a receiver informed. A delivery that fails is logged.

Set `REFRESH_WEBHOOK_SECRET` to sign the payloads. The signature is the
HMAC-SHA256 of the raw request body, keyed with the secret, hex-encoded and
sent in the `X-SoroVault-Signature` header:

```
X-SoroVault-Signature: sha256=6f3a…
```

A receiver recomputes it over the body it received and compares the two in
constant time. A mismatch means the payload was tampered with, or did not
come from SoroVault at all.

### Contracts that left the network

A contract instance expires once it has not been used for a while. When a
refresh cannot find a contract, the registry records when it first noticed —
`missing_since` — rather than failing the refresh and losing what it knew:

```json
{
  "contract_id": "CDZZ…4PAN",
  "network": "testnet",
  "current_wasm_hash": "c618da…0781",
  "missing_since": "2026-08-07T12:00:00Z"
}
```

The interface is still served — it is the best description of a contract that
is no longer there — and the browse UI marks the contract as missing. A
contract that comes back is marked live again, and the mark is cleared. A
sweep counts a missing contract apart from a failure, and says so once, when
it happens, rather than on every sweep.

### Bounded registrations

Registering a contract downloads its WASM module and decodes it in memory,
so the number of registrations running at once is what keeps peak memory
finite. `MAX_CONCURRENT_REGISTRATIONS` (default `4`) bounds it, across every
client: the API, the CLI and the background refresher all draw on the same
budget, because they all download modules.

A request that cannot be given a slot waits a couple of seconds for one and
is then refused with **503** and a `Retry-After` header, rather than queueing
without limit:

```console
$ curl -si -X POST localhost:8080/api/contracts \
    -H 'content-type: application/json' \
    -d '{"contract_id":"CDZZ…4PAN"}'
HTTP/1.1 503 Service Unavailable
Retry-After: 2
…
{"error":"registry: too many registrations in flight: 4 already running"}
```

The wait is short on purpose: a client that is told to come back shortly is
better served than one left hanging behind a queue the server cannot drain.
The refresher treats a busy registry the same way — it skips the contract and
picks it up on the next sweep, since a contract that was not refreshed stays
stale.

## HTTP API

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/contracts` | list contracts — `?q=`, `?function=`, `?type=`, `?network=`, `?limit=`, `?offset=` |
| `POST` | `/api/contracts` | register a contract — `{"contract_id": "C…"}` |
| `POST` | `/api/contracts:batch` | register up to 20 contracts in one request — `{"contract_ids": ["C…", "C…"]}` |
| `POST` | `/api/wasm/{hash}` | catalog uploaded code before a contract runs it |
| `GET` | `/api/wasm/{hash}` | read a catalogued uploaded-code interface — `?network=` |
| `GET` | `/api/wasm/{hash}/contracts` | every contract running a given WASM — `?network=`, `?ever=`, `?limit=`, `?offset=` |
| `GET` | `/api/contracts/{id}` | the decoded ABI — `?network=`, `?wasm_hash=` |
| `PUT` | `/api/contracts/{id}/source` | attach a `source_url` and optional `source_commit` |
| `GET` | `/api/contracts/{id}/functions/{fn}` | one function in detail |
| `GET` | `/api/contracts/{id}/client.ts` | typed TypeScript client — `?wasm_hash=`, `?download=1` |
| `GET` | `/api/openapi.json` | OpenAPI 3.1 description of this API |
| `GET` | `/api/contracts/{id}/versions` | every stored interface version |
| `POST` | `/api/contracts/{id}/refresh` | re-check against the network |
| `GET` | `/healthz` | liveness |

`POST /api/contracts` returns **201** when the contract is new and **200** when
it was already registered, so it is safe to retry. Passing `?wasm_hash=` to a
read selects a superseded version instead of the current one.

**Batches.** `POST /api/contracts:batch` registers up to 20 contracts in one
request, which is how a tool seeds a whole deployment without making twenty
requests. The contracts are processed one after another, so the batch draws on
the registry's concurrency budget once per contract rather than all at once,
and one failure does not stop the rest. Every contract gets a result of its
own:

```json
{
  "results": [
    { "contract_id": "CDZZ…4PAN", "status": "created", "created": true, "changed": true, "contract": {…}, "interface": {…} },
    { "contract_id": "CDZZ…2V", "status": "error", "created": false, "changed": false,
      "error": "stellar: contract not found on network: CDZZ…2V", "code": 404 }
  ],
  "created": 1, "unchanged": 0, "changed": 0, "failed": 1
}
```

`status` is `created`, `unchanged`, `changed` (already registered, but
upgraded since it was last fetched) or `error`. A failure carries the status
code the contract would have had on a request of its own. The request itself
is bounded at 20 contract IDs, which is about 40 RPC round trips: enough to
seed a deployment, few enough to finish inside the server's request timeout.

**Search.** `?q=` is a case-insensitive substring match against the contract
ID, its name, and the names of every function, type and event in the
contract's **current** interface — so `?q=transfer` finds every token-like
contract in the registry. When a symbol matched, the listing says which:

```json
{ "contract_id": "CDZZ…4PAN", "network": "testnet", "matches": ["has_voted", "vote"] }
```

A function removed by an upgrade stops matching; older versions stay
retrievable by hash but are not searched.

**Exact names.** `?q=` is a substring, so `?q=transfer` also finds
`transfer_from` and `transferable`. When a tool wants the contracts that
implement an interface, it wants the names to match exactly:

```
GET /api/contracts?function=transfer&function=balance
GET /api/contracts?type=Balance
```

`?function=` matches a function of that exact name, `?type=` a user-defined
type (struct, union, enum or error enum) of that exact name. Both are
repeatable, and every name has to match: two `?function=` parameters ask for
a contract with both functions. The names are matched case-sensitively, since
an interface is spelled a particular way. The filters combine with `?q=` and
`?network=`, and all of them have to match.

Status codes worth knowing: **422** means the contract exists but has no
interface to serve (a Stellar asset contract, or a module with no spec section),
as distinct from **404**, which means it is not there at all.

**Contracts that share a WASM.** Many contracts run the same code: every
instance of a token template, every vault a factory deployed. That is the
question a vulnerability in a shared WASM raises, and
`GET /api/wasm/{hash}/contracts` answers it:

```console
GET /api/wasm/c618dae264864ccf446a3c7db27da80c7c83e840131242cc2fe9cd32a2a20781/contracts
```

```json
{
  "wasm_hash": "c618dae264864ccf446a3c7db27da80c7c83e840131242cc2fe9cd32a2a20781",
  "network": "testnet",
  "ever": false,
  "contracts": [
    { "contract_id": "CDZZ…4PAN", "network": "testnet", "current_wasm_hash": "c618dae…", "current": true, … }
  ],
  "total": 1,
  "limit": 50,
  "offset": 0
}
```

`?ever=true` widens the answer to the contracts that ran the code in the past
and have since been upgraded; each result says whether the contract still runs
it. The contract page links its WASM hash to this listing.

## CLI

```console
sorovault add <contract_id>...      # fetch, decode and register
sorovault refresh <contract_id>...  # re-check for an upgraded interface
sorovault list                      # -q, --function, --type, --network, --limit, --offset
sorovault get <contract_id>         # --function, --wasm-hash, --json
sorovault codegen <contract_id>     # --lang ts|md; -o, --wasm-hash
sorovault serve                     # JSON API + browse UI
sorovault migrate                   # apply schema migrations
sorovault doctor                    # check a deployment's health
```

`add` and `refresh` accept several contract IDs and report per-contract
failures without abandoning the rest. `list` takes `--function` and `--type`
for the same exact-name filters the API offers. Every read command takes
`--json`, which emits exactly what the API serves:

```console
$ sorovault get CDZZ…4PAN --function has_voted
fn has_voted(nullifier: BytesN<32>) -> bool

inputs:
  nullifier  BytesN<32>

returns: bool
```

### `sorovault doctor`

When a deployment misbehaves, the same four questions come up: can it reach
Postgres, are the migrations up to date, does the RPC endpoint answer, and
is it serving the network this instance expects. `doctor` answers all four in
one go:

```console
$ sorovault doctor
PASS  database    the database answers
PASS  migrations  the database is at version 2, the latest is 2
PASS  rpc         the endpoint is reachable, serving testnet
PASS  passphrase  the endpoint serves the configured network
```

Every check runs, so one failure does not hide the others, and the command
exits non-zero if any of them failed. `--json` emits the same report for a
script to read:

```console
$ sorovault doctor --json
{
  "ok": false,
  "checks": [
    { "name": "database", "ok": true, "detail": "the database answers" },
    { "name": "migrations", "ok": false, "error": "the database is at version 1, but version 2 is available; run `sorovault migrate`" },
    { "name": "rpc", "ok": true, "detail": "the endpoint is reachable, serving testnet" },
    { "name": "passphrase", "ok": true, "detail": "the endpoint serves the configured network" }
  ]
}
```

The command is strictly read-only: it opens a connection and reads, and
never writes to the database, applies a migration, or changes anything on
the network. It reads the same configuration as everything else, so a
deployment missing its `DATABASE_URL` is reported as a configuration error
rather than a passing check.

## Typed clients

SoroVault generates a typed TypeScript client for any registered contract —
from the stored interface, so it needs no network access, and for any
version by `?wasm_hash=`:

```console
$ sorovault codegen CDZZ…4PAN -o zkvote.ts
$ curl -s localhost:8080/api/contracts/CDZZ…4PAN/client.ts > zkvote.ts
```

The file declares every struct, union and enum, an `Errors` code table, and a
`Client` interface with one method per contract function. `connect()` hands
back `@stellar/stellar-sdk`'s `contract.Client` narrowed to that interface:

```ts
import { connect } from "./zkvote";

const client = await connect({
  rpcUrl: "https://soroban-testnet.stellar.org",
  networkPassphrase: "Test SDF Network ; September 2015",
});
const tx = await client.get_proposal({ index: 0 });   // args and result are typed
console.log(tx.result);                               // sdk.Result<Proposal>
```

Type mappings follow the Stellar CLI's own bindings (`u64`→`bigint`,
`BytesN<32>`→`Buffer`, unions as `{ tag, values }`), so values pass through
the SDK unchanged. It requires `@stellar/stellar-sdk` 14 or later and
`buffer`.

CI compiles the generated client for the real testnet fixture with
`tsc --strict` against a pinned SDK, so a change to the generator that would
produce broken TypeScript fails the build.

## Markdown documentation

Generate a standalone Markdown reference from a registered contract's ABI,
including function signatures and comments, user-defined types, events, and
the selected WASM version:

```console
$ sorovault codegen CDZZ…4PAN --lang md -o CONTRACT.md
```

The browse UI renders the same documentation as Markdown, because that is
how contract authors write it — the testnet fixture's `vote` function has a
table and a bullet list. The rendering is sanitised, since documentation
comes from an untrusted chain: raw HTML in a doc never reaches the page, and
every link is marked `rel="nofollow noopener"`.

The browse UI also offers a copy button next to the values people paste into
other tools — contract IDs, WASM hashes and function signatures. The buttons
are revealed by a small script, so without JavaScript the values stay plain
selectable text.

## Uploaded code and source links

`POST /api/wasm/{hash}` catalogs uploaded code by its 64-character SHA-256
hash, even before a contract instance uses it. Its decoded interface is then
available from `GET /api/wasm/{hash}`. Attach a contract repository URL and an
optional 40- or 64-character commit hash with `PUT /api/contracts/{id}/source`;
the browse page displays the repository and commit with the interface.

## The ABI JSON

This is the project's public contract — the shape other tooling should build
against. `abi_version` is bumped if it ever changes incompatibly.

```jsonc
{
  "abi_version": "1",
  "meta": { "rsver": "1.93.0-nightly", "rssdkver": "25.3.0#dcbea445…" },
  "env_meta": { "interface_version": "25", "protocol": 25 },

  "functions": [
    {
      "name": "vote",
      "doc": "Cast a vote using a ZK Firma Digital Groth16 proof.…",
      "inputs": [
        { "name": "voter",       "type": { "kind": "address", "display": "Address" } },
        { "name": "pub_signals", "type": { "kind": "vec", "display": "Vec<BytesN<32>>",
                                           "element": { "kind": "bytes_n", "display": "BytesN<32>", "n": 32 } } }
      ],
      "outputs": [
        { "kind": "result", "display": "Result<(), Error>",
          "ok":    { "kind": "void",  "display": "()" },
          "error": { "kind": "error", "display": "Error" } }
      ]
    }
  ],

  "types": {
    "structs":     [ { "name": "Proposal", "fields": [ … ], "is_tuple": false } ],
    "unions":      [ { "name": "DataKey",  "cases":  [ { "name": "Admin", "values": [] } ] } ],
    "enums":       [ { "name": "Status",   "cases":  [ { "name": "Active", "value": 0 } ] } ],
    "error_enums": [ { "name": "Error",    "cases":  [ { "name": "AlreadyVoted", "value": 5 } ] } ]
  },

  "events": [
    {
      "name": "Voted",
      "prefix_topics": ["voted"],
      "params": [
        { "name": "voter",          "type": { "kind": "address", "display": "Address" }, "location": "topic-list" },
        { "name": "proposal_index", "type": { "kind": "u32",     "display": "u32"     }, "location": "data" }
      ],
      "data_format": "map"
    }
  ]
}
```

### Types

Every type is an object with a `kind` and a `display`. `display` is the
human-readable rendering of the whole type (`"Option<Vec<u32>>"`) so that UIs
and documentation generators do not each have to re-implement the traversal.

**Scalars** — `val`, `bool`, `void`, `error`, `u32`, `i32`, `u64`, `i64`,
`timepoint`, `duration`, `u128`, `i128`, `u256`, `i256`, `bytes`, `string`,
`symbol`, `address`, `muxed_address`. These carry no extra fields.

**Containers** — each adds the fields it needs:

| `kind` | Extra fields |
|---|---|
| `option` | `inner` |
| `vec` | `element` |
| `map` | `key`, `value` |
| `result` | `ok`, `error` |
| `tuple` | `elements` (array) |
| `bytes_n` | `n` |
| `udt` | `name` — refers to an entry in `types` |

A few things worth relying on:

- `outputs` is an array, but Soroban allows at most one entry. **Empty means the
  function returns nothing.**
- Empty collections are `[]`, never `null`, so you can iterate without a check.
- `is_tuple` on a struct marks a Rust tuple struct, which Soroban encodes as a
  struct whose fields are named `"0"`, `"1"`, … — flagged so consumers need not
  re-derive it.
- `meta` is omitted entirely when a contract carries no metadata section.
- `env_meta` is omitted entirely when a contract carries no environment
  metadata section. It says which host interface the contract was built
  against, which is the minimum protocol it needs: a host runs a contract
  built for the same or an older protocol version. `interface_version` is
  that version written as a version number; `protocol` and `pre_release` are
  the two numbers it is made of. The section used to carry them as one
  64-bit value — the protocol number in the high half, the pre-release
  number in the low half — and the XDR now spells them out separately. The
  bytes mean the same thing either way, so a section written by an older
  toolchain still decodes.

`abi_version` is **1**. The `env_meta` field was added without a bump,
because adding an optional field is not a breaking change.

## How it fits together

```
cmd/sorovault      cobra CLI; also serves the API and UI
internal/config    environment configuration
internal/model     the ABI model — no dependencies, the public JSON contract
internal/wasm      WASM custom-section reader
internal/spec      contract spec decoding: XDR -> model
internal/stellar   RPC client and contract fetch
internal/store     Postgres registry, migrations, and an in-memory store
internal/registry  register/refresh orchestration
internal/refresher scheduled re-checks of registered contracts
internal/codegen   Markdown documentation and typed client generation from a stored ABI
internal/api       chi JSON handlers
internal/web       html/template + htmx browse UI; goldmark + bluemonday for the docs
```

Three seams are interfaces, and each one is the natural place to extend the
project: **`stellar.Fetcher`** (where contract code comes from),
**`spec.Decoder`** (how an interface is derived), and **`store.Store`** (where
the registry lives). Nothing above a seam depends on what is below it, which is
why the whole stack can be tested without a network or a database.

### Data model

```
contracts (network, contract_id)  PK
  current_wasm_hash, name, source_url, source_commit, first_seen, last_refreshed,
  missing_since

specs     (network, contract_id, wasm_hash)  UNIQUE
  spec jsonb, decoded_at, symbols

wasm_specs (network, wasm_hash)  PK
  spec jsonb, decoded_at
```

Refreshing an upgraded contract **appends** a `specs` row and repoints
`current_wasm_hash`. Prior versions are never overwritten, so an interface that
was live at some point stays retrievable by its hash.

## Development

```console
$ make test        # no database, no network needed
$ make test-all    # also runs the store suite against a throwaway Postgres
$ make lint
$ make build
$ make help        # everything else
```

`go test ./...` **never touches the network.** The decode path is exercised
against a real testnet contract checked in at
[`internal/spec/testdata/zkvote.wasm`](internal/spec/testdata/) — its provenance
and a hash check are documented alongside it. Layers above `internal/stellar`
run against a fake network (`internal/stellar/stellartest`).

The Postgres store suite skips unless `TEST_DATABASE_URL` is set; `make
test-all` and CI both set it. Both store implementations run the **same
conformance suite**, so the in-memory one other packages test against cannot
drift from the real one.

## Open ground

Contributions welcome. Deliberately **not** built yet:

- **More codegen targets.** TypeScript clients ship today (see
  [Typed clients](#typed-clients)); Go, Python and Rust generators would
  slot in beside `internal/codegen/typescript.go`, reusing its golden-file
  and compile-in-CI test pattern.
- **Resolving contract names.** The schema carries a `name`, but nothing
  populates it yet.
Explicitly out of scope: authentication, and any write operation against
contracts themselves. SoroVault reads chain state; it never sends a transaction.

## License

Apache-2.0. See [LICENSE](LICENSE).
