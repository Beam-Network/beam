# Beam Validator

The validator reads BeamCore's materialized epoch summary, sets the returned weights on Bittensor subnet 105, and posts the weight proof back to BeamCore.

## Install

```bash
# From the repository root
pip install -e ".[validator]"
```

## Mainnet Quick Start

```bash
cd neurons/validator

BEAM_VALIDATOR_WALLET_NAME=your_coldkey \
BEAM_VALIDATOR_WALLET_HOTKEY=your_hotkey \
BEAM_VALIDATOR_CORE_SERVER_URL=https://beamcore.b1m.ai \
SUBTENSOR_NETWORK=finney \
NETUID=105 \
python main.py
```

## Configuration

Settings use the `BEAM_VALIDATOR_*` prefix except for shared subnet settings `SUBTENSOR_NETWORK` and `NETUID`.

### Required for production

| Variable | Description | Production value |
|---|---|---|
| `BEAM_VALIDATOR_WALLET_NAME` | Bittensor wallet name | your wallet name |
| `BEAM_VALIDATOR_WALLET_HOTKEY` | Hotkey name within the wallet | your validator hotkey |
| `BEAM_VALIDATOR_CORE_SERVER_URL` | BeamCore HTTP base URL | `https://beamcore.b1m.ai` |
| `SUBTENSOR_NETWORK` | Bittensor network | `finney` |
| `NETUID` | Beam subnet UID | `105` |

### Common optional settings

| Variable | Description | Default |
|---|---|---|
| `BEAM_VALIDATOR_WALLET_PATH` | Wallet directory | `~/.bittensor/wallets` |
| `BEAM_VALIDATOR_PORT` | Local validator API port | `8093` |
| `BEAM_VALIDATOR_LOG_LEVEL` | Logging verbosity | `INFO` |
| `BEAM_VALIDATOR_EXTERNAL_URL` | Public URL advertised in heartbeat | unset |
| `BEAM_VALIDATOR_BLOCKS_BETWEEN_WEIGHTS` | Minimum blocks between weight sets | `100` |
| `BEAM_VALIDATOR_DISABLE_WEIGHT_SET` | Skip on-chain `set_weights` | `false` |
| `LOCAL_MODE` | Local harness mode, unprefixed | `false` |

### Minimum `.env`

```dotenv
BEAM_VALIDATOR_WALLET_NAME=your_coldkey
BEAM_VALIDATOR_WALLET_HOTKEY=your_hotkey
BEAM_VALIDATOR_CORE_SERVER_URL=https://beamcore.b1m.ai
SUBTENSOR_NETWORK=finney
NETUID=105
LOCAL_MODE=false
```

## Runtime Flow

The current runtime path is implemented in `core/validator.py` and `clients/subnet_core_client.py`:

1. `main.py` fetches `GET /config/uid-ranges` from BeamCore before imports finish.
2. The validator initializes wallet, subtensor, metagraph, and the BeamCore client.
3. `_get_persisted_weight_snapshot()` calls `GET /Validator/epoch-summary/latest-epoch`.
4. `_set_weights()` submits the returned `uids` and `weights` to Bittensor.
5. On success, `submit_weight_proof()` posts to `POST /validators/weights/proof`.
6. Heartbeats are sent to `POST /validators/heartbeat`.

The production validator does not compute PRISM weights locally and does not call older scoring, orchestrator-list, spot-check, or generic weight-submission routes.

## BeamCore API Surface Used By The Client

| Endpoint | Method | Purpose |
|---|---|---|
| `/config/uid-ranges` | GET | Startup UID range bootstrap |
| `/config/network` | GET | Optional network config helper |
| `/Validator/epoch-summary/latest-epoch` | GET | Materialized UID/weight vector |
| `/validators/weights/proof` | POST | Record successful on-chain weight set |
| `/validators/heartbeat` | POST | Report validator liveness |

## Local Operator API

The validator serves FastAPI on `0.0.0.0:${BEAM_VALIDATOR_PORT:-8093}`.

| Endpoint | Description |
|---|---|
| `GET /health` | Basic validator health |
| `GET /health/detailed` | Detailed component checks when available |
| `GET /state` | Runtime state |
| `GET /scores` | Local connection scores |
| `GET /weights` | Recent weight-set history |

Logs are written to `${LOG_DIR}/validator.log`; `LOG_DIR` defaults to `/tmp/beam_validator_logs`.

## Health Check

```bash
curl http://localhost:8093/health
```

Minimal response:

```json
{
  "status": "healthy",
  "node_type": "validator",
  "external_url": null
}
```

## Weight Snapshot Behavior

The validator expects BeamCore to return matching `uids` and `weights` arrays:

```json
{
  "epoch": 17925,
  "uids": [12, 47, 52],
  "weights": [0.42, 0.33, 0.25],
  "formula_version": "prism_final_x_task_done_count",
  "params_hash": "..."
}
```

If the summary is unavailable or malformed, the validator skips the weight window. It does not invent fallback weights for production.
