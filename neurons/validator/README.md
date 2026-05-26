# Beam Validator

Validators read scoring inputs from the public BEAM control plane and set weights on Bittensor subnet 105.

## Install

From the repository root:

```bash
python3 -m venv .venv
source .venv/bin/activate
pip install -e ".[validator]"
```

## Configure

```bash
BEAM_VALIDATOR_CORE_SERVER_URL=https://beamcore.b1m.ai
SUBTENSOR_NETWORK=finney
NETUID=105
BEAM_VALIDATOR_WALLET_NAME=your_coldkey
BEAM_VALIDATOR_WALLET_HOTKEY=your_hotkey
```

## Run

```bash
cd neurons/validator
python main.py
```

## Environment

Settings use the `BEAM_VALIDATOR_*` prefix (see [`core/config.py`](core/config.py)). Chain selection uses the unprefixed `SUBTENSOR_NETWORK` and `NETUID` settings.

### Required

| Variable | Description | Default |
| -------- | ----------- | ------- |
| `BEAM_VALIDATOR_WALLET_NAME` | Bittensor coldkey name | `default` |
| `BEAM_VALIDATOR_WALLET_HOTKEY` | Bittensor hotkey name | `default` |
| `BEAM_VALIDATOR_CORE_SERVER_URL` | BeamCore HTTP base URL | `https://beamcore.b1m.ai` |
| `SUBTENSOR_NETWORK` | Bittensor network (`finney` for mainnet) | `finney` |
| `NETUID` | BEAM subnet UID | `105` |

### Optional

| Variable | Description | Default |
| -------- | ----------- | ------- |
| `BEAM_VALIDATOR_WALLET_PATH` | Wallet directory | `~/.bittensor/wallets` |
| `BEAM_VALIDATOR_PORT` | HTTP port for the validator API | `8093` |
| `BEAM_VALIDATOR_LOG_LEVEL` | Logging verbosity (`DEBUG`, `INFO`, `WARNING`) | `INFO` |
| `LOCAL_MODE` | Disable chain calls for local dev (no prefix) | `false` |
| `BEAM_VALIDATOR_DISABLE_WEIGHT_SET` | Skip on-chain `set_weights` (live-test guard) | `false` |
| `BEAM_VALIDATOR_HEARTBEAT_INTERVAL_SECONDS` | BeamCore heartbeat cadence | `60` |
| `BEAM_VALIDATOR_BLOCKS_BETWEEN_WEIGHTS` | Minimum blocks between weight sets | `100` |
| `BEAM_VALIDATOR_EXTERNAL_URL` | Public URL advertised to BeamCore (e.g. `https://validator.example.com`) | — |

### Minimum production `.env`

```dotenv
BEAM_VALIDATOR_WALLET_NAME=your_coldkey
BEAM_VALIDATOR_WALLET_HOTKEY=your_hotkey
NETUID=105
SUBTENSOR_NETWORK=finney
BEAM_VALIDATOR_CORE_SERVER_URL=https://beamcore.b1m.ai
LOCAL_MODE=false
```

Validator routes are rooted at `BEAM_VALIDATOR_CORE_SERVER_URL` with no `/api` prefix.

See [../../docs/validator.md](../../docs/validator.md) for the full operator guide.
