# BEAM Validator Onboarding Guide

This guide covers the public mainnet validator path for Beam subnet 105. The validator reads BeamCore's materialized epoch summary, sets weights on Bittensor, and posts a proof of the on-chain weight update back to BeamCore.

## Runtime Responsibilities

The validator process:

1. Loads wallet, subnet, BeamCore, and HTTP API settings from environment variables.
2. Fetches UID range configuration from `GET /config/uid-ranges` before validator imports complete.
3. Initializes the Bittensor wallet/subtensor connection.
4. Fetches the latest materialized weight snapshot from `GET /Validator/epoch-summary/latest-epoch`.
5. Calls `set_weights(netuid=105, uids, weights)` when the configured block interval allows it.
6. Posts the successful transcript to `POST /validators/weights/proof`.
7. Sends liveness to `POST /validators/heartbeat`.

The validator does not move transfer payloads, does not manage workers, does not run the worker gateway, and does not compute production PRISM weights locally. PRISM is computed server-side and exposed as the epoch summary.

## Architecture

```text
Bittensor subnet 105       BeamCore                    Validator
metagraph                  https://beamcore.b1m.ai     neurons/validator
    ▲                                │                     │
    │ set_weights                    │ epoch summary        │
    └────────────────────────────────┴─────────────────────┘
                                      ▲
                                      │ weights proof, heartbeat
```

## Requirements

| Component | Requirement |
|---|---|
| Python | 3.10+ |
| Bittensor package | Use the repository dependency from `pyproject.toml` |
| Wallet | Registered validator hotkey on subnet 105 |
| Network | Outbound access to BeamCore and Bittensor |
| Port | Optional local HTTP API on `8093` |

## Installation

```bash
git clone https://github.com/Beam-Network/beam.git
cd beam
python3 -m venv .venv
source .venv/bin/activate
pip install -e ".[validator]"
cd neurons/validator
```

## Mainnet Configuration

Create `neurons/validator/.env` or set these variables in your process manager:

```dotenv
BEAM_VALIDATOR_WALLET_NAME=validator
BEAM_VALIDATOR_WALLET_HOTKEY=default
BEAM_VALIDATOR_CORE_SERVER_URL=https://beamcore.b1m.ai
NETUID=105
SUBTENSOR_NETWORK=finney

# Optional
BEAM_VALIDATOR_WALLET_PATH=~/.bittensor/wallets
BEAM_VALIDATOR_PORT=8093
BEAM_VALIDATOR_LOG_LEVEL=INFO
BEAM_VALIDATOR_EXTERNAL_URL=https://validator.example.com
BEAM_VALIDATOR_BLOCKS_BETWEEN_WEIGHTS=100
BEAM_VALIDATOR_DISABLE_WEIGHT_SET=false
```

Validator-specific settings use the `BEAM_VALIDATOR_` prefix. `NETUID` and `SUBTENSOR_NETWORK` are intentionally unprefixed shared subnet settings.

## Running

```bash
cd neurons/validator
source ../../.venv/bin/activate
python main.py
```

The startup path prints the UID range fetched from BeamCore, then starts the FastAPI service on `0.0.0.0:${BEAM_VALIDATOR_PORT:-8093}`.

## Health And State

The validator writes logs to `${LOG_DIR}/validator.log`; `LOG_DIR` defaults to `/tmp/beam_validator_logs`.

```bash
curl http://localhost:8093/health
```

Typical minimal response:

```json
{
  "status": "healthy",
  "node_type": "validator",
  "external_url": "https://validator.example.com"
}
```

When the health monitor is active, `/health` also returns component `checks`, `consecutive_failures`, and `uptime_seconds`.

Useful local endpoints:

| Endpoint | Purpose |
|---|---|
| `GET /health` | Basic validator health |
| `GET /health/detailed` | Full component status when available |
| `GET /state` | Current UID, epoch, last weight block, and runtime state |
| `GET /scores` | Local connection score view |
| `GET /weights` | Recent weight-set history |

## BeamCore Endpoints Used

| Endpoint | Purpose |
|---|---|
| `GET /config/uid-ranges` | Startup UID range bootstrap |
| `GET /config/network` | Optional network config helper |
| `GET /Validator/epoch-summary/latest-epoch` | Materialized UID/weight vector |
| `POST /validators/weights/proof` | Weight-set proof after a successful chain call |
| `POST /validators/heartbeat` | Validator liveness and health |

Routes are rooted at `BEAM_VALIDATOR_CORE_SERVER_URL` with no `/api` prefix.

## Weight Setting

The production path uses the latest BeamCore epoch summary:

```python
snapshot = await subnet_core_client.get_latest_epoch_summary()
uids = snapshot["uids"]
weights = snapshot["weights"]
subtensor.set_weights(netuid=settings.netuid, uids=uids, weights=weights)
await subnet_core_client.submit_weight_proof(...)
```

If BeamCore does not return a valid `uids`/`weights` vector, the validator skips that weight window. It does not fall back to local PRISM scoring for production.

## Troubleshooting

### BeamCore epoch summary unavailable

Verify the production URL and API reachability:

```bash
curl https://beamcore.b1m.ai/health
```

### Not registered on subnet 105

Check the wallet and registration:

```bash
btcli wallet overview --wallet.name validator --subtensor.network finney
```

Register the hotkey on subnet 105 if needed:

```bash
btcli subnet register --netuid 105 --subtensor.network finney \
  --wallet.name validator --wallet.hotkey default
```

### Weight setting failed

Confirm the hotkey is available on the server, the process can reach Bittensor, and `BEAM_VALIDATOR_DISABLE_WEIGHT_SET` is not set to `true`.

### Debug logging

```bash
BEAM_VALIDATOR_LOG_LEVEL=DEBUG python main.py
```

## Production Service Example

Use your actual clone path in place of `/srv/beam`:

```ini
[Unit]
Description=BEAM Validator
After=network.target

[Service]
Type=simple
User=beam
WorkingDirectory=/srv/beam/neurons/validator
Environment="PATH=/srv/beam/.venv/bin:/usr/local/bin:/usr/bin:/bin"
EnvironmentFile=/srv/beam/neurons/validator/.env
ExecStart=/srv/beam/.venv/bin/python main.py
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
```
