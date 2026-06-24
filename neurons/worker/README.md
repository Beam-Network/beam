# Beam Worker

Workers register with BeamCore, connect to an orchestrator-owned worker gateway, execute transfer chunks, report task results, and post payment evidence.

## Requirements

- Python 3.10+
- Bittensor wallet hotkey registered on subnet 105
- Stable upload/download bandwidth
- Network access to BeamCore, the worker gateway, and storage URLs in task offers

## Install

```bash
# From the repository root
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

`WORKER_GATEWAY_URL` must point to the orchestrator-owned worker gateway origin. The worker converts it to `ws(s)://.../ws/<worker_id>?api_key=<worker-api-key>`. Do not point it at BeamCore or `ORCH_GATEWAY_URL`.

## Run

```bash
cd neurons/worker
python worker.py --wallet.name your_coldkey --wallet.hotkey your_hotkey --subtensor.network finney
```

## Transport

The worker transport is WebSocket-based. `CONNECTION_MODE` may be `websocket` or `auto`; values such as `polling` are rejected by the runtime.

The WebSocket session receives:

| Message | Purpose |
|---|---|
| `connected` | Worker gateway accepted the session |
| `task_offer` | Executable transfer chunk offer |
| `task_accept_ack` | Acceptance acknowledged upstream |
| `task_reject_ack` | Rejection acknowledged upstream |
| `task_result_ack` | Result acknowledged and optionally completed |

The worker sends `task_accept`, `task_reject`, and `task_result` messages. Connection liveness is maintained with WebSocket ping/pong and reconnects.

## Task Offer Protocol

Workers read `WORKER_VERSION` from package metadata. BeamCore includes `minimum_worker_version` in every `task_offer`; workers reject offers below that SemVer with `unsupported_worker_version` before fetching source bytes or uploading destination bytes.

BeamCore also includes `signed_url_flow`. `signed_url_v1` is the default object-storage flow and requires `dest_headers.Content-MD5` on UploadPart offers. Workers reject checksum-less v1 object-storage offers before upload. `signed_url_v2` remains selectable by the transfer creator.

## Payment Evidence

After a successful `task_result_ack`, the worker signs and posts:

```text
POST /workers/<worker_id>/tasks/<task_id>/payment-evidence
```

Payload:

```json
{
  "offer_id": "uuid",
  "success": true,
  "chunk_hash": "abc123...",
  "worker_signature": "0x...",
  "required_payment": true
}
```

## Troubleshooting

- Confirm the hotkey is registered on subnet 105.
- Confirm `CORE_SERVER_URL=https://beamcore.b1m.ai`.
- Confirm `WORKER_GATEWAY_URL` is reachable from the worker host.
- Confirm the owning orchestrator has `READY=true` and at least one connected worker.
- If startup fails with a transport error, remove any polling-mode override.
