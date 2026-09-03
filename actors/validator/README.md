# Beam Validator

Validators set Bittensor weights from BeamCore PRISM epoch summaries and post weight proofs back to BeamCore.

## Requirements

- Python 3.10-3.12
- Bittensor validator hotkey for subnet 105
- Network access to BeamCore and Bittensor

## Install

```bash
git clone https://github.com/Beam-Network/beam.git
cd beam
python -m venv .venv
source .venv/bin/activate
python -m pip install --upgrade pip
python -m pip install -e ".[validator]"
```

## Configure

```bash
export BEAM_VALIDATOR_WALLET_NAME=validator
export BEAM_VALIDATOR_WALLET_HOTKEY=default
export BEAM_VALIDATOR_CORE_SERVER_URL=https://beamcore.b1m.ai
export SUBTENSOR_NETWORK=finney
export NETUID=105
```

## Run

```bash
cd actors/validator
source ../../.venv/bin/activate
python main.py
```

## Runtime Flow

```text
BeamCore epoch summary -> validator -> Bittensor set_weights
validator -> BeamCore weight proof + heartbeat
```

The validator consumes BeamCore epoch summaries and sets weights from the latest valid scoring payload.

## Health

```bash
curl http://127.0.0.1:8093/health
curl http://127.0.0.1:8093/state
curl http://127.0.0.1:8093/weights
```

## More Detail

See [../../docs/validator.md](../../docs/validator.md).
