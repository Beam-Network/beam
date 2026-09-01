# Beam Worker

Workers register with BeamCore, connect to an orchestrator-owned worker gateway, execute transfer chunks, advertise capabilities, and report task results.

## Requirements

- Python 3.10-3.12
- A Bittensor wallet hotkey registered on subnet 105
- Stable upload and download bandwidth
- Network access to BeamCore, the worker gateway, and task storage URLs

## Install

Run installation from the repository root, the directory that contains `pyproject.toml`:

```bash
git clone https://github.com/Beam-Network/beam.git
cd beam
python -m venv .venv
source .venv/bin/activate
python -m pip install --upgrade pip
python -m pip install -e .
```

## Register

Register the worker hotkey on Beam subnet 105 before starting the worker:

```bash
btcli subnet register --netuid 105 --subtensor.network finney \
  --wallet.name your_coldkey \
  --wallet.hotkey your_hotkey
```

## Configure

Create or export these environment variables before starting the process:

```bash
export CORE_SERVER_URL=https://beamcore.b1m.ai
export WORKER_GATEWAY_URL=https://your-orchestrator-worker-gateway.example
export SUBTENSOR_NETWORK=finney
export NETUID=105
export CONNECTION_MODE=websocket
```

`WORKER_GATEWAY_URL` must point to the orchestrator-owned worker gateway origin. It is not BeamCore and not `ORCH_GATEWAY_URL`. The worker converts it to `ws(s)://.../ws/<worker_id>?api_key=<worker-api-key>`.

## Run

```bash
cd actors/worker
python worker.py --wallet.name your_coldkey --wallet.hotkey your_hotkey --subtensor.network finney
```

## What The Worker Does

- Registers with BeamCore over HTTP.
- Connects to the worker gateway over WebSocket.
- Advertises the `transfer.multipart` capability.
- Queues valid task offers and executes one task at a time.
- Sends one `task_result` for each completed or failed task and retries until BeamCore returns a terminal acknowledgement.

Workers do not send a pre-result acceptance message and do not reject offers based on a version floor.

## Troubleshooting

- Confirm the hotkey is registered on subnet 105.
- Confirm `CORE_SERVER_URL=https://beamcore.b1m.ai`.
- Confirm `WORKER_GATEWAY_URL` is reachable from the worker host.
- Confirm the owning orchestrator has `READY=true` and at least one connected worker.
- If startup fails with a transport error, remove any polling-mode override.
