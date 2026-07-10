# Beam Worker

Workers register with BeamCore, connect to an orchestrator-owned worker gateway, execute transfer chunks, and report task results.

## Requirements

- Python 3.10+
- Bittensor wallet hotkey registered on subnet 105
- Stable upload/download bandwidth
- Network access to BeamCore, the worker gateway, and storage URLs in task offers

## Install

```bash
pip install -e "."
```

The runtime dependencies are declared in `pyproject.toml`; for a manual environment, include `bittensor`, `httpx`, and `websockets`.

## Mainnet Environment

```dotenv
CORE_SERVER_URL=https://beamcore.b1m.ai
WORKER_GATEWAY_URL=https://orchestrator.example.com
SUBTENSOR_NETWORK=finney
NETUID=105
CONNECTION_MODE=websocket
```

Set `WORKER_GATEWAY_URL` to the orchestrator-owned worker gateway origin. The worker converts it to `ws(s)://.../ws/<worker_id>?api_key=<worker-api-key>`.

## Run

```bash
cd neurons/worker
python worker.py --wallet.name your_coldkey --wallet.hotkey your_hotkey --subtensor.network finney
```

## Transport

The worker transport is WebSocket-based. Use `CONNECTION_MODE=websocket` or `CONNECTION_MODE=auto`.

The worker receives `task_offer`, `task_accept_ack`, `task_reject_ack`, `task_result_ack`, and `session_displaced`. The worker sends `task_accept`, `task_reject`, and `task_result` messages. Connection liveness is maintained with WebSocket ping/pong and reconnects.

## Task Offer Protocol

Workers read `WORKER_VERSION` from package metadata. Current public worker builds report `0.2.1`. BeamCore includes `minimum_worker_version` in every `task_offer`; workers compare their SemVer version before fetching source bytes or uploading destination bytes.

BeamCore also includes `signed_url_flow`. `signed_url_v1` is the default object-storage flow and sends direct multipart UploadPart offers. `signed_url_v2` remains selectable by the transfer creator.

## Task Result

After completing an accepted task, the worker sends:

```json
{
  "type": "task_result",
  "task_id": "uuid",
  "offer_id": "uuid",
  "worker_id": "worker-uuid",
  "success": true,
  "bytes_transferred": 8388608,
  "duration_ms": 1234,
  "etag": "\"abc123\"",
  "error": null
}
```

BeamCore acknowledges the result with `task_result_ack`.

## Troubleshooting

- Confirm the hotkey is registered on subnet 105.
- Confirm `CORE_SERVER_URL=https://beamcore.b1m.ai`.
- Confirm `WORKER_GATEWAY_URL` is reachable from the worker host.
- Confirm the owning orchestrator has `READY=true` and at least one connected worker.
- If startup fails with a transport error, remove any polling-mode override.