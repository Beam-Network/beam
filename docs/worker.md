# BEAM Worker Guide

Run a worker on BEAM mainnet.

## Requirements

- Python 3.10-3.12
- A Bittensor wallet hotkey registered on subnet 105
- Stable upload and download bandwidth
- Network access to BeamCore, the worker gateway, and task storage URLs

## 1. Install

Install from the repository root, the directory that contains `pyproject.toml`:

```bash
git clone https://github.com/Beam-Network/beam.git
cd beam
python -m venv .venv
source .venv/bin/activate
python -m pip install --upgrade pip
python -m pip install -e .
```

## 2. Register

Register the hotkey on subnet 105 before starting the worker:

```bash
btcli subnet register --netuid 105 --subtensor.network finney \
  --wallet.name your_coldkey \
  --wallet.hotkey your_hotkey
```

## 3. Configure

Ask your orchestrator operator for the worker gateway origin, then export the worker environment:

```bash
export CORE_SERVER_URL=https://beamcore.b1m.ai
export WORKER_GATEWAY_URL=https://your-orchestrator-worker-gateway.example
export SUBTENSOR_NETWORK=finney
export NETUID=105
export CONNECTION_MODE=websocket
```

`WORKER_GATEWAY_URL` is the orchestrator-owned worker gateway that serves `/ws/<worker_id>?api_key=<worker-api-key>`. It is not BeamCore and not `ORCH_GATEWAY_URL`.

## 4. Run

```bash
cd actors/worker
python worker.py --wallet.name your_coldkey --wallet.hotkey your_hotkey --subtensor.network finney
```

The worker registers with BeamCore over HTTP, connects to the worker gateway over WebSocket, advertises `transfer.multipart`, queues valid offers, executes one task at a time, and reports `task_result` until BeamCore returns a terminal acknowledgement.

## Troubleshooting

- Verify the hotkey is registered on subnet 105.
- Verify `CORE_SERVER_URL=https://beamcore.b1m.ai`.
- Verify `WORKER_GATEWAY_URL` points to the orchestrator-owned worker gateway and is reachable from the worker host.
- Verify the owning orchestrator is ready and has at least one connected worker.
- If startup fails with a transport error, remove any polling-mode override.
