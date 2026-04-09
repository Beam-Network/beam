# Beam Network Worker

A worker node for the Beam Network — an open coordination layer for distributed data transfer built on Bittensor.

Workers receive data transfer tasks, fetch chunks from a source, deliver them to a destination, and report completion with bandwidth metrics.

## Requirements

- Python 3.10+
- CPU: 2+ cores
- RAM: 4 GB+
- Storage: 20 GB SSD
- Network: 100 Mbps symmetric (upload/download)
- OS: Ubuntu 22.04+ / Debian 12+ / macOS 13+

## Installation

```bash
pip install bittensor httpx websockets
```

## Usage

```bash
# Default wallet
python3 worker.py

# Custom wallet
python3 worker.py --wallet.name my_wallet --wallet.hotkey my_hotkey

# Testnet
python3 worker.py --subtensor.network test
```

## Connection Modes

The worker connects to the network via WebSocket by default (instant task push). Set the `CONNECTION_MODE` environment variable to override:

| Value | Behavior |
|---|---|
| `auto` (default) | WebSocket, falls back to HTTP polling if unavailable |
| `websocket` | WebSocket only |
| `http` | HTTP polling only (5s interval) |

```bash
CONNECTION_MODE=http python3 worker.py
```

## How It Works

1. Registers with the network using your Bittensor wallet (signed authentication)
2. Connects via WebSocket to receive tasks instantly as they are assigned
3. For each task: fetches data chunks from the source and delivers them to the destination
4. Reports completion with proof-of-bandwidth metrics (bytes transferred, speed, duration)
5. Sends periodic heartbeats to stay registered

## Environment Variables

| Variable | Default | Description |
|---|---|---|
| `SUBNET_CORE_URL` | mainnet endpoint | Override the API endpoint |
| `CONNECTION_MODE` | `auto` | Connection mode (see above) |
