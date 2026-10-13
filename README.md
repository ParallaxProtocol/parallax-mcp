parallax-mcp
============

A [Model Context Protocol](https://modelcontextprotocol.io) server that
gives AI assistants and agents structured, read-only access to the
Parallax network: chain data from a node, indexed data from the
[explorer](https://explorer.parallaxprotocol.org), network health from the
[status page](https://status.parallaxprotocol.org), and the Parallax
documentation and PIPs.

It runs locally over stdio (Claude Desktop, Claude Code) or as a remote
Streamable HTTP service, and never holds or accepts private keys.

- [Tools](#tools)
- [Resources and prompts](#resources-and-prompts)
- [Using it](#using-it)
- [Configuration](#configuration)
- [Security model](#security-model)
- [Deployment](#deployment)
- [Development](#development)

## Tools

Tools are grouped, and each group can be turned off. Every result is
compact JSON with LAX amounts as `{"wei": "...", "lax": "..."}`, and
anything third parties can write is wrapped as `{"untrusted": "..."}` (see
[Security model](#security-model)).

**chain**: from the node's JSON-RPC

| Tool | What it returns |
|---|---|
| `chain_info` | Chain ID check, client, head and its age, sync state, peers, gas price, the node's RPC namespaces |
| `get_block` | A block by number, hash or `latest`, with block time, subsidy and optionally transaction summaries |
| `get_transaction` | A transaction with its calldata decoded (verified ABI from the explorer, or the standard token ABI) |
| `get_receipt` | Status, gas used, fee and decoded event logs |
| `get_balance` | Balance and nonce, at any block on an archive node |
| `get_code`, `get_storage_at`, `get_proof` | Bytecode, storage slots and EIP-1186 proofs |
| `call_contract` | A read-only `eth_call`, encoding arguments and decoding results from an ABI, the explorer's verified ABI or a signature such as `balanceOf(address) returns (uint256)` |
| `estimate_gas` | Gas and fee for a transaction |
| `get_logs` | Event logs, bounded by block range and count |
| `gas_price` | `eth_gasPrice` and Parallax's Bitcoin Core-style fee estimates for 1 to 144 blocks |
| `audit_token_approvals` | Every ERC-20 allowance and NFT approval an address has granted that is still live, checked against current contract state, with exposure, risk flags, severity and the exact revoke call |
| `wait_for_new_block` | Waits (up to 10 minutes) for the next block; for clients without resource subscriptions |
| `trace_transaction`, `trace_call` | Call trees, pre-state and selectors (needs the `debug` namespace) |

**mining**: derived from block headers and the emission schedule; each derivation is documented in the tool description

| Tool | What it returns |
|---|---|
| `mining_stats` | Block time statistics, hashrate estimate, difficulty and its trend, expected block time, miner shares, and the status page's figures for comparison |
| `supply_info` | Issued supply (node and schedule cross-checked), share of the 21M cap, current reward and era, next halving at the target and observed pace |
| `block_reward` | A block's subsidy plus fees, and when it matures |

**explorer**: from Blockscout's REST API v2

`explorer_address`, `explorer_token_balances`, `explorer_transactions`,
`explorer_token`, `explorer_token_holders`, `explorer_token_transfers`,
`explorer_contract` (interface, metadata and optionally source),
`explorer_internal_transactions`, `explorer_search`, `explorer_stats`
(including the stats service's counters), and `explorer_charts`: 21 time
series (accounts, transactions, fees, blocks, gas, contracts) at day, week,
month or year resolution from Blockscout's stats service. Lists are
paginated with an opaque `next_cursor`.

**network**: from the status page and the node

| Tool | What it returns |
|---|---|
| `network_health` | One report: alerts, ongoing incidents, mining decentralization and recommended confirmations, nodes, propagation, reorgs, mempool, and the node's own head, peers and sync state, with a list of issues |
| `network_nodes` | The nodes reporting to the status page |
| `network_incidents` | Incident history and verified nodes' uptime |
| `network_propagation` | Block propagation delays, stale blocks and reorgs |
| `network_trends` | Hashrate, difficulty, block time, hashprice, security budget, nodes, latency, uptime and mempool over 7 days in 4-hour slices |
| `node_peers` | The node's peers and address book, with IP addresses withheld (needs `admin`) |

**mempool**: `mempool_status` (node and network-wide) and
`mempool_transactions` (needs `txpool`).

**tx**: off by default

| Tool | What it returns |
|---|---|
| `build_unsigned_tx` | An unsigned legacy or EIP-2930 transaction with nonce, gas and gas price filled in, its signing payload and hash, for an external signer |
| `decode_tx` | Any raw transaction or signing payload, decoded, with the sender recovered from the signature |
| `broadcast_signed_tx` | Submits a signed transaction. Registered only with `--allow-broadcast` |

### Which tools a node supports

The public RPC endpoint serves only `eth`, `net` and `web3`. Tools that need
more (`trace_*` need `debug`, `node_peers` needs `admin`,
`mempool_transactions` needs `txpool`) are left out when the node does not
serve their namespace; `chain_info` lists what it does. To get them all,
point the server at your own node:

```sh
parallaxd --http --http.addr 127.0.0.1 \
  --http.api eth,net,web3,txpool,debug,admin,xhash --gcmode archive --txlookuplimit 0
```

Exposing these namespaces to parallax-mcp is safe: it calls them through
an allowlist of read-only methods (see [Security model](#security-model)).
Do not expose them to anything else. Historical state (balances, calls and
traces at old blocks) needs `--gcmode archive`.

## Resources and prompts

| Resource | Content |
|---|---|
| `parallax://chain/parameters` | Chain ID, block time, PoW and ASERT, reward schedule and cap, coinbase maturity, EVM level (Berlin) and transaction types, endpoints |
| `parallax://glossary` | Parallax and proof-of-work terms used by the tools |
| `parallax://docs`, `parallax://docs/{path}` | The documentation of [docs.parallaxprotocol.org](https://docs.parallaxprotocol.org) (v2.1.x) |
| `parallax://pips`, `parallax://pips/pip-000N` | PIP-0002 to PIP-0006 |
| `parallax://chain/head` | The latest block; **subscribable**: a `resources/updated` notification on every new block (and reorg) |
| `parallax://network/alerts` | The status page's active alerts; **subscribable**: a notification when one appears, clears or changes level |

Subscriptions work on stdio and on the HTTP transport's sessions (not
with `--http-stateless`). The server polls the node every
`--head-poll-interval` (15 s) only while someone is subscribed or waiting,
and stops ten minutes after the last interest. Clients without
subscription support can call `wait_for_new_block` instead.

The documents are a snapshot embedded in the binary; refresh them with
`make sync-content` (it reads `../parallax` with `git archive` at `main`
and the PIP discussions with the `gh` CLI).

Prompts: `explain_transaction` (hash), `audit_address` (address),
`network_health_report`.

## Using it

### Claude Code

Local, over stdio:

```sh
claude mcp add parallax -- /path/to/parallax-mcp
# with the transaction tools, against your own node:
claude mcp add parallax -- /path/to/parallax-mcp --groups chain,mining,explorer,network,mempool,tx --rpc-url http://127.0.0.1:8545
```

Remote, over HTTP:

```sh
claude mcp add --transport http parallax https://mcp.example.org/mcp
```

### Claude Desktop

Add to `claude_desktop_config.json` (Settings → Developer → Edit Config):

```json
{
  "mcpServers": {
    "parallax": {
      "command": "/path/to/parallax-mcp",
      "args": ["--log-level", "warn"]
    }
  }
}
```

or with Docker:

```json
{
  "mcpServers": {
    "parallax": {
      "command": "docker",
      "args": ["run", "-i", "--rm", "ghcr.io/parallaxprotocol/parallax-mcp", "--transport", "stdio"]
    }
  }
}
```

A remote HTTP deployment is added in Claude Desktop as a custom connector
(Settings → Connectors → Add custom connector) with its URL, such as
`https://mcp.example.org/mcp`.

### From a shell

`bin/mcpcall` (`make tools`) is a small MCP client for trying tools:

```sh
bin/mcpcall tools
bin/mcpcall call get_block '{"block":"latest"}'
bin/mcpcall call call_contract '{"to":"0xd1060183552A88455f037a58B2aC639C7966a214","function":"symbol() returns (string)"}'
bin/mcpcall read parallax://glossary
bin/mcpcall -http http://127.0.0.1:8080/mcp call network_health
bin/mcpcall call trace_transaction '{"hash":"0x…"}' -- --rpc-url http://127.0.0.1:8545
```

## Configuration

Settings come from flags, `PARALLAX_MCP_*` environment variables and an
optional YAML file (`--config`, see [config.example.yaml](config.example.yaml)),
in that order of precedence. `parallax-mcp -help` lists them all.

| Flag | Default | |
|---|---|---|
| `--transport` | `stdio` | `stdio` or `http` |
| `--rpc-url` | `https://rpc.parallaxprotocol.org` | Node JSON-RPC (http/https) |
| `--explorer-url` | `https://explorer.parallaxprotocol.org` | Blockscout |
| `--status-url` | `https://status.parallaxprotocol.org` | Status page |
| `--chain-id` | `2110` | The server refuses to start against another chain |
| `--groups` | `chain,mining,explorer,network,mempool` | Add `tx` for the transaction tools |
| `--allow-broadcast` | off | Registers `broadcast_signed_tx` (needs `tx`) |
| `--http-addr`, `--http-path` | `127.0.0.1:8080`, `/mcp` | HTTP transport |
| `--http-stateless` | off | No sessions: replicas need no sticky load balancing, but resource subscriptions are unavailable |
| `--head-poll-interval` | `15s` | How often the head is polled while clients are subscribed or waiting |
| `--explorer-stats-url` | `<explorer-url>/stats-service` | Blockscout stats service, for charts and counters |
| `--rate-limit`, `--rate-burst` | `2`/s, `20` | Per client address, HTTP transport |
| `--max-concurrent` | `64` | Requests in flight, HTTP transport |
| `--trusted-proxies` | none | Proxies allowed to set `X-Forwarded-For` |
| `--behind-cloudflare` | off | Take client addresses from `CF-Connecting-IP` on requests from Cloudflare's edge ranges |
| `--metrics-addr` | off | Serve Prometheus `/metrics` on a separate listener |
| `--max-response-bytes` | `262144` | Larger results fail with advice to narrow the query |
| `--max-log-range`, `--max-logs` | `10000`, `500` | `get_logs` bounds |
| `--status-cache-ttl` | `15s` | The status page allows 60 requests a minute per address |
| `--expose-peer-addresses` | off | Show peer IPs in `node_peers`; stdio only |

## Security model

**Untrusted data.** Token names and symbols, contract source, event and
revert strings, decoded string arguments, explorer tags and node names are
chosen by anyone and may be written to manipulate a model reading them.
Such text is stripped of control, bidirectional and zero-width characters,
capped in length, and wrapped as `{"untrusted": "..."}`; the server's
instructions tell the model to treat it as data only.

**Read-only by construction.** Every request to the node passes an
allowlist of read-only JSON-RPC methods before it leaves the process, so
even a node with `personal`, `admin`, `miner` and `debug` fully exposed
cannot be made to unlock accounts, sign, change peers, mine, rewind the
chain or read its database. Trace calls must name a built-in tracer;
JavaScript tracers are refused. `eth_sendRawTransaction` is allowed only
with `--allow-broadcast`, on either transport, and the HTTP transport is
read-only unless that flag is given.

**No keys.** The server builds unsigned transactions and decodes signed
ones; signing happens elsewhere. It refuses 32-byte inputs that look like
private keys without echoing them back. `broadcast_signed_tx` refuses
unsigned, unprotected (pre-EIP-155) and wrong-chain transactions.

**Bounded queries.** Block ranges, page sizes, list lengths, response sizes,
backend response sizes and every call's duration are capped. Each HTTP
client address has a token-bucket rate limit, and the number of requests
in flight is capped.

**Privacy.** Peer IP addresses and node IDs from `admin_peers` are never
shown over HTTP, and on stdio only with `--expose-peer-addresses`.

**Resilience.** Backend requests have timeouts and retries with backoff
(not for broadcasts). When one source fails, tools return what the others
provided with the failure under `"errors"`. Status page responses are cached
and shared between concurrent callers, and a copy up to ten minutes old is
served, marked stale, while the page is unreachable.

## Deployment

The image is published to `ghcr.io/parallaxprotocol/parallax-mcp` for
amd64 and arm64: a static binary on distroless, running as a non-root user,
listening on 8080 with the HTTP transport.

```sh
docker run -d -p 8080:8080 \
  -e PARALLAX_MCP_RPC_URL=http://your-node:8545 \
  -e PARALLAX_MCP_TRUSTED_PROXIES=10.0.0.0/8 \
  ghcr.io/parallaxprotocol/parallax-mcp
```

Put it behind a TLS-terminating reverse proxy and set `--trusted-proxies`
to the proxy's address so rate limits apply per real client.

**Behind Cloudflare**, also set `--behind-cloudflare`
(`PARALLAX_MCP_BEHIND_CLOUDFLARE=true`): the client address is then read
from `CF-Connecting-IP`, but only on requests whose hop into your proxy is
one of Cloudflare's published edge ranges, so nobody reaching the origin
directly can choose their address. Cloudflare closes responses that stay
silent for 100 seconds; the server keeps them open by pinging the client
every 25 seconds on a running tool call's stream (which also sends the
response headers early) and writing comment lines on idle notification
streams. Exempt the MCP hostname from Bot Fight Mode and other challenge
features (Security → WAF → custom rule: hostname equals your MCP host,
action Skip), since MCP clients cannot solve browser challenges.

### Coolify

Create a **Docker Image** resource with `ghcr.io/parallaxprotocol/parallax-mcp:<version>`,
set the domain and **Ports Exposes** `8080`, and add:

```
PARALLAX_MCP_TRUSTED_PROXIES=10.0.0.0/8,172.16.0.0/12
PARALLAX_MCP_BEHIND_CLOUDFLARE=true      # when the domain is proxied by Cloudflare
PARALLAX_MCP_RPC_URL=http://your-node:8545   # optional: a node with more namespaces
```

Leave Coolify's own health check off (it runs `curl` in the container,
which the distroless image does not have); the image's Docker
`HEALTHCHECK` runs `parallax-mcp healthcheck` instead. HTTP sessions
live in memory, so several replicas need sticky load balancing on the
`Mcp-Session-Id` header, or `--http-stateless`, which gives up resource
subscriptions. The proxy must allow long-lived responses (server-sent events
and `wait_for_new_block`); the keep-alives described above keep them busy.
`/healthz` answers `ok`; with `--metrics-addr`, Prometheus metrics count
tool calls by tool and outcome (`ok`, `error`, `partial`), tool and backend
latency, backend retries and errors, and rate-limited requests. Logs are
structured JSON on stderr.

## Development

```sh
make            # lint, unit tests, build
make race       # unit tests with the race detector
make live       # integration tests against the real endpoints
make live-full RPC=http://127.0.0.1:8545   # also the debug, admin and txpool tools
make docker
```

Unit tests run against fake backends (`internal/testutil`: a JSON-RPC node,
the explorer and the status page as `httptest` servers) through an
in-memory MCP client. The integration suite in `test/integration` runs only
with `PARALLAX_MCP_LIVE=1` and checks invariants, such as the node's supply
matching the emission schedule and its hashrate estimate agreeing with the
status page, rather than values that change with every block.

Layout:

```
cmd/parallax-mcp      the server
cmd/mcpcall           a command-line MCP client
internal/server       tool registration: timeouts, logging, metrics, size limits
internal/transport    stdio and Streamable HTTP (rate limits, health check)
internal/backend      node (RPC allowlist), blockscout, status (cache)
internal/tools/*      the tool groups (live: head and alert subscriptions, wait_for_new_block)
internal/watch        the head and alert poller behind subscriptions
internal/resources    resources and the embedded docs and PIPs
internal/prompts      prompts
internal/evm          ABI encoding and decoding, RPC types
internal/lax          units and the emission schedule
internal/untrusted    sanitizing and wrapping third-party text
```

The node backend uses Parallax's own `rpc` package and types rather than
go-ethereum's, because Parallax headers carry an `epochStartTime` field
that go-ethereum's types drop, which would change block hashes.

## License

GNU General Public License v3.0; see [LICENSE](LICENSE).
