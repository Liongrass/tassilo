# Tassilo

A terminal UI wallet for [Taproot Assets](https://github.com/lightninglabs/taproot-assets) built on top of [Lightning Terminal (litd)](https://github.com/lightninglabs/lightning-terminal).

## Requirements

- A running `litd` instance (which bundles `lnd` + `tapd`)
- Go 1.21+

## Installation

```sh
go install github.com/lightninglabs/tassilo@latest
```

Or build from source:

```sh
git clone https://github.com/lightninglabs/tassilo
cd tassilo
go build -o tassilo .
```

## Usage

```sh
tassilo [flags]
```

Tassilo connects to your local `litd` instance at startup and opens the TUI dashboard.

### Flags

| Flag | Default | Env var | Description |
|---|---|---|---|
| `--rpcserver` | `127.0.0.1:8443` | `TASSILO_RPCSERVER` | litd gRPC host:port |
| `--tlscertpath` | `~/.lit/tls.cert` | `TASSILO_TLSCERTPATH` | Path to litd TLS certificate |
| `--macaroonpath` | `~/.lit/mainnet/lit.macaroon` | `TASSILO_MACAROONPATH` | Path to lit.macaroon |
| `--network` | `mainnet` | `TASSILO_NETWORK` | `mainnet`, `testnet`, `regtest`, or `simnet` |

Default paths are platform-specific: `~/Library/Application Support/Lit` on macOS and `%LOCALAPPDATA%\Lit` on Windows.

### Example

```sh
# regtest node on a non-default port
tassilo --network regtest --rpcserver localhost:8443

# or via environment variables
TASSILO_NETWORK=testnet tassilo
```

## Dashboard

The dashboard shows your current balances (on-chain and off-chain, BTC and assets) and a menu of actions:

| Key | Action |
|---|---|
| `r` | Receive — create a BTC or asset invoice |
| `s` | Send — pay a bolt11 or asset invoice |
| `p` | List payments — full payment history |
| `c` | List channels — all BTC and asset channels |
| `o` | Open channel — open a BTC or asset channel |
| `a` | List assets — all known Taproot Assets |
| `f` | Refresh balances |
| `q` | Quit |

Press `Esc` in any view to return to the dashboard.
