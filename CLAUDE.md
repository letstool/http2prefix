# CLAUDE.md — http2prefix

This file provides context for AI-assisted development on the `http2prefix` project.

---

## Project overview

`http2prefix` is a single-binary HTTP gateway that exposes BGP routing intelligence as a JSON REST API.
It is written entirely in Go and embeds all static assets (web UI, favicon, OpenAPI spec) at compile
time using `//go:embed` directives, so the resulting binary has zero runtime file dependencies.

The server accepts `POST /api/v1/asprefix` requests containing either an IP address or an AS number
and auto-detects which of its two internal databases to query:

- **IP address** → queries the **prefix2as** MMDB (maps IP prefixes to AS numbers)
- **AS number** (`AS<n>`, case-insensitive) → queries the **as2prefix** MMDB (maps AS numbers to their announced prefixes)

Both databases are **built automatically** from gzipped CSVs fetched from the **letstool CDN**
and refreshed once per day at **noon (UTC time)**. An optional `LICENSE_KEY` enables licensed
(higher-quota) CDN access; the server works anonymously without one.

---

## Repository layout

```
.
├── api/
│   └── swagger.yaml              # OpenAPI 3.1 source (human-editable)
├── build/
│   └── Dockerfile                # Two-stage Docker build (builder + scratch runtime)
├── cmd/
│   └── http2prefix/
│       ├── main.go               # Entire application — single file
│       └── static/
│           ├── favicon.png       # Embedded at build time
│           ├── index.html        # Embedded web UI (dark/light, 9 languages, RTL support)
│           └── openapi.json      # Embedded OpenAPI spec (generated from swagger.yaml)
├── scripts/
│   ├── 000_init.sh               # go mod tidy
│   ├── 999_test.sh               # Integration smoke tests (curl)
│   ├── linux_build.sh            # Native static binary build
│   ├── linux_run.sh              # Run binary on Linux
│   ├── docker_build.sh           # Build Docker image
│   ├── docker_run.sh             # Run Docker container
│   ├── windows_build.cmd         # Native build on Windows
│   └── windows_run.cmd           # Run binary on Windows
├── go.mod
├── go.sum
├── LICENSE                       # Apache 2.0
├── README.md
└── CLAUDE.md                     # This file
```

---

## Key design decisions

- **Single `main.go`**: the entire server logic lives in `cmd/http2prefix/main.go`. There are no internal packages.
- **Embedded assets**: `favicon.png`, `index.html`, and `openapi.json` are embedded with `//go:embed`. Any change to these files is picked up at the next `go build`.
- **Static binary**: the build uses `CGO_ENABLED=0` and `-ldflags "-extldflags -static"`. Do not introduce `cgo` dependencies.
- **No framework**: the HTTP layer uses only the standard library (`net/http`). Do not add a router or web framework.
- **Two MMDB databases**: managed via two `sync/atomic.Value` stores (`prefix2asDB`, `as2prefixDB`). Hot-swap on refresh via `swapDB()`.
- **Custom mmdb**: built with `github.com/maxmind/mmdbwriter` and read with `github.com/oschwald/maxminddb-golang`. **Important**: in `mmdbwriter v1.0.0` the insertion method is `writer.Insert(network, record)` — not `InsertNetwork`.
- **Two update modes** controlled by `DB_URL`:
  - **CDN CSV mode** (default): fetches a gzipped CSV from `https://cdn.letstool.net/{as2prefix|prefix2as}/csv`, decompresses it on the fly, parses it with `encoding/csv`, and compiles a fresh `as2prefix.mmdb` or `prefix2as.mmdb`. The as2prefix CSV has 5 columns (`asn,prefixes,peers,upstreams,downstreams`); the three neighbour columns are merged into a single `peers` list in the MMDB.
  - **Peer mode** (`DB_URL` set): downloads both `mmdb` from `/db/as2prefix` or `/db/prefix2as` of another `http2prefix` instance.
- **CDN protocol**: `fetchCSVFromCDN` sends `If-Modified-Since` (read from `.last_modified_tor`) on every request. The switch on the CDN status code handles five cases:
  - **304 Not Modified** — costs no quota; treated as success (timestamp refreshed, build skipped). Returns the sentinel `errNotModified`.
  - **429 Too Many Requests** — returns `*errRateLimited` containing the `Retry-After` unix timestamp.
  - **410 Gone** — product is disabled on the CDN side; returns `*errProductGone` with the JSON body message.
  - **401 Unauthorized** — license level insufficient; returns `*errUnauthorized` with the human-readable `message` field extracted from the CDN JSON body via `extractJSONMessage`.
  - **200 OK** — `Last-Modified` is stored in `.last_modified_tor` for subsequent `If-Modified-Since` requests; CSV stream returned to caller.
- **Hot database swap**: the active `*maxminddb.Reader` is stored in a `sync/atomic.Value` via `swapDB()`.

---

## Environment variables & CLI flags

Every configuration value can be set via an environment variable **or** a command-line flag. The flag always takes priority. Resolution order: **CLI flag → environment variable → hard-coded default**.

| Environment variable | CLI flag        | Default          | Description                                                      |
|----------------------|-----------------|------------------|------------------------------------------------------------------|
| `LISTEN_ADDR`        | `-listen-addr`  | `127.0.0.1:8080` | Listen address and port.                                         |
| `DB_DIR`             | `-db-dir`       | `/data`          | Directory for the `.mmdb` files and marker files.                |
| `DB_URL`             | `-db-url`       | *(none)*         | Base URL of a peer http2prefix instance. When set, enables peer mode.  |
| `LICENSE_KEY`        | `-license-key`  | *(none)*         | CDN license token. Sent as `Authorization: Basic <token>`.       |

**Proxy variables** (no CLI flag — curl-compatible convention):

| Variable                     | Description                          |
|------------------------------|--------------------------------------|
| `HTTPS_PROXY` / `https_proxy` | Proxy URL for HTTPS traffic.        |
| `HTTP_PROXY` / `http_proxy`   | Proxy URL for plain HTTP traffic.   |
| `NO_PROXY` / `no_proxy`       | Comma-separated bypass list.        |

---

## prefix2as database

### CDN source
`https://cdn.letstool.net/prefix2as/csv` — gzipped CSV with required columns `prefix` and `asnum`, plus optional `asname` and `country`.

### MMDB design
- **IPVersion: 6**, `IncludeReservedNetworks: true`.
- IPv4 prefixes (parsed by `net.ParseCIDR`) are mapped to the IPv4-in-IPv6 space (`::ffff:0:0/96`) automatically by mmdbwriter.
- **Insertion order is critical**: prefixes MUST be sorted by prefix length **ascending** (least specific first = shortest mask first) before insertion. This ensures parent networks are stored before child networks so that more-specific child records correctly override the parent in the MMDB trie when queried. Inserting a less-specific prefix AFTER a more-specific one would overwrite the child record.
- **Lookup**: `net.ParseIP(ipStr).To16()` gives the 16-byte key for both IPv4 and IPv6 queries.

### Record schema (`PrefixASRecord`)

| mmdb key  | Go type    | Description                                          |
|-----------|------------|------------------------------------------------------|
| `prefix`  | string     | Canonical prefix string (e.g. `1.0.0.0/24`)         |
| `origins` | []uint32   | All originating ASNs — MOAS-safe (mmdbtype.Slice)   |

DatabaseType: `db2prefix-PREFIX2AS` (matches db2prefix for interoperability).

IPv4 prefixes are inserted using their 4-byte `.To4()` representation so that mmdbwriter maps them into the standard `::ffff:0:0/96` subtree, exactly as db2prefix does.

---

## as2prefix database

### CDN source
`https://cdn.letstool.net/as2prefix/csv` — gzipped CSV with columns `asn`, `prefixes`, `peers`, `upstreams`, and `downstreams`. One row per AS:
- `prefixes` — space-separated CIDR prefixes announced by this AS
- `peers` — space-separated ASNs of symmetric BGP peers (classified by db2prefix)
- `upstreams` — space-separated ASNs of provider ASes (classified by db2prefix)
- `downstreams` — space-separated ASNs of customer ASes (classified by db2prefix)

All three neighbour columns are merged into a single deduplicated, ascending-sorted set stored in the MMDB and API as `peers`. Directionality (provider vs customer vs peer) is discarded; the union represents the full set of BGP neighbours.

### MMDB design — synthetic ULA IPv6 keyspace (db2prefix compatible)

ASNs are stored using the **exact same synthetic ULA IPv6 keyspace as db2prefix**, ensuring that MMDBs produced by both tools are fully interchangeable.

```
Key layout (16 bytes):
  [0-9]   = fd:ac:db:01:00:00:00:00:00:00   (fixed ULA prefix, 80 bits)
  [10-13] = ASN as big-endian uint32
  [14-15] = 0x00, 0x00 (zero)

Network mask = /128  (one exact address per ASN, no subnet)
Lookup key   = exact 128-bit address
```

`fdac:db01::/32` is within the ULA range (`fc00::/7`) and never routes publicly.

**Example**: AS 13335 (0x00003417):
```
key:    fdac:db01:0000:0000:0000:0000:0000:3417 /128
lookup: fdac:db01:0000:0000:0000:0000:0000:3417
```

**db2prefix format compatibility:**

| Property         | db2prefix               | http2prefix             |
|------------------|-------------------------|-------------------------|
| ULA base         | `fdac:db01::/80`        | `fdac:db01::/80` ✓      |
| Mask             | `/128`                  | `/128` ✓                |
| MMDB field (ASN) | `"asn"`                 | `"asn"` ✓               |
| DatabaseType     | `db2prefix-AS2PREFIX`   | `db2prefix-AS2PREFIX` ✓ |

### Record schema (`ASPrefixRecord`)

| mmdb key   | Go type    | Description                                        |
|------------|------------|----------------------------------------------------|
| `asn`      | uint32     | AS number (field name matches db2prefix)           |
| `prefixes` | []string   | Announced prefixes, sorted: IPv4↑ then IPv6↑       |

---

## API contract

### Endpoint

```
POST /api/v1/asprefix
Content-Type: application/json
```

**Request body**:
```json
{ "query": "<IP address or AS number>" }
```

**Query type auto-detection**:
- `net.ParseIP(query) != nil` → IP query → prefix2as DB
- `(?i)^AS[0-9]+$` → AS number query → as2prefix DB
- Otherwise → `status: "ERROR"`, `query_type: "unknown"`

### Response status values

| Value      | Meaning                                                                  |
|------------|--------------------------------------------------------------------------|
| `SUCCESS`  | Record found; `answer` is fully populated                                |
| `NOTFOUND` | Valid query, no matching record; `answer` may contain partial data        |
| `ERROR`    | Malformed query, unknown type, or database not yet initialised           |

### Other endpoints

| Method | Path              | Description                                                  |
|--------|-------------------|--------------------------------------------------------------|
| `GET`  | `/`               | Embedded interactive web UI                                  |
| `GET`  | `/openapi.json`   | OpenAPI 3.1 specification                                    |
| `GET`  | `/favicon.png`    | Application icon                                             |
| `GET`  | `/db/prefix2as`   | Serves `prefix2as.mmdb` for peer download                   |
| `GET`  | `/db/as2prefix`   | Serves `as2prefix.mmdb` for peer download                   |

---

## Web UI

The UI is a self-contained single-file HTML/JS/CSS application embedded in the binary.

- **Themes**: dark and light, switchable via toggle. Persisted in `localStorage`.
- **Languages**: 15 locales — Arabic (`ar`), Bengali (`bn`), German (`de`), English (`en`), Spanish (`es`), French (`fr`), Hindi (`hi`), Indonesian (`id`), Japanese (`ja`), Korean (`ko`), Portuguese (`pt-BR`), Russian (`ru`), Urdu (`ur`), Vietnamese (`vi`), Chinese (`zh-CN`).
- **RTL support**: Arabic switches layout to right-to-left.
- **Query type indicator**: real-time client-side detection shows whether the input is an IP address or AS number before submitting.
- **IP result**: displays matched prefix, ASN, AS name, country.
- **AS result**: displays AS number, name, country, and all announced prefixes as color-coded chips (blue for IPv4, teal for IPv6).
- **Example queries**: pre-filled chips for common IPs and AS numbers.

---

## Scheduler

Updates fire once per day at **noon (UTC time)**. `nextNoonDuration()` computes the delay.

`loadOrBuild()` is called at startup for each DB:
- If the `.mmdb` file exists AND was last updated today after noon → load from disk (no CDN call).
- Otherwise → call `updatePrefix2as()` or `updateAs2prefix()` immediately.

After startup, `schedulePeriodicUpdate()` fires at the next noon, then every 24 hours.

CDN status codes alter the schedule (same protocol as `http2mac`):
- **304 Not Modified** — skips rebuild; refreshes local timestamp. Costs no CDN quota.
- **429 Too Many Requests** — defers next attempt to the CDN-supplied `Retry-After` timestamp.
- **410 Gone** — retries after 24 h, 48 h, 72 h, 96 h, then stops permanently.
- **401 Unauthorized** — logs the server message and stops permanently.
- **Any other error** — logs and retries at the next noon.

Marker files in `DB_DIR`:
- `.last_update_prefix2as` — Unix timestamp of the last successful prefix2as build/download.
- `.last_modified_prefix2as` — `Last-Modified` HTTP header from the last CDN 200 response for prefix2as.
- `.last_update_as2prefix` — Same for as2prefix.
- `.last_modified_as2prefix` — Same for as2prefix.

---

## Peer mode

When `DB_URL` is set, both DBs are downloaded from the peer instead of built from CDN:
- `/db/prefix2as` → downloads `prefix2as.mmdb`
- `/db/as2prefix` → downloads `as2prefix.mmdb`

The peer serves these files via its own `/db/prefix2as` and `/db/as2prefix` GET endpoints.

---

## Constraints & conventions

- Go version: **1.24+**
- No `cgo`. Keep `CGO_ENABLED=0`.
- No additional HTTP frameworks or routers.
- All logic stays in `cmd/http2prefix/main.go`.
- Error responses always return a `LookupResponse` JSON body — never plain-text.
- The server never logs request bodies; avoid logging queried IPs or AS numbers.
- All code, identifiers, comments, and documentation must be in **English**.
- Every configuration environment variable must have a corresponding CLI flag.

---

## Build & run commands

```bash
# Initialise / tidy dependencies
bash scripts/000_init.sh

# Build native static binary -> ./out/http2prefix
bash scripts/linux_build.sh

# Run
bash scripts/linux_run.sh

# Build Docker image -> letstool/http2prefix:latest
bash scripts/docker_build.sh

# Run Docker container
bash scripts/docker_run.sh

# Smoke tests (server must be running on 127.0.0.1:8080)
bash scripts/999_test.sh
```

---

## AI-assisted development

This project was developed with the assistance of **Claude Sonnet 4.6** by Anthropic.
