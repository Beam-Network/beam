# BEAM Orchestrator Onboarding Guide

This guide covers the public mainnet orchestrator path for Beam subnet 105. An orchestrator connects to BeamCore over NATS, advertises a worker gateway, selects connected workers for task offers, and forwards worker decisions/results back to BeamCore.

## Runtime Responsibilities

The orchestrator process:

1. Registers with BeamCore using wallet-signed NATS control messages.
2. Advertises its HTTP URL and worker gateway URL.
3. Maintains an in-process worker gateway at `/ws/<worker_id>?api_key=...` unless `ORCHESTRATOR_WORKER_GATEWAY_URL` points at an externally reachable gateway origin.
4. Receives `worker_task_offer_batch` messages from BeamCore through NATS.
5. Selects connected local workers and sends `task_offer` messages.
6. Relays `task_accept`, `task_reject`, and `task_result` messages upstream over NATS.
7. Stays `READY=true` when it should receive routed production work.

Workers use BeamCore HTTP for registration; runtime task delivery and results use the worker gateway relay path.

## Mainnet Endpoints

| Setting | Value |
|---|---|
| `CORE_SERVER_URL` | `https://beamcore.b1m.ai` |
| `ORCH_GATEWAY_URL` | `tls://orch-gateway.b1m.ai:4222` |
| `ORCHESTRATOR_WORKER_GATEWAY_URL` | Your externally reachable worker gateway origin |
| `SUBTENSOR_NETWORK` | `finney` |
| `NETUID` | `105` |

Set `ORCH_GATEWAY_URL` to a NATS endpoint using `nats://` or `tls://`. `ORCHESTRATOR_WORKER_GATEWAY_URL` and worker `WORKER_GATEWAY_URL` should refer to the same worker gateway origin when workers connect through a public domain or reverse proxy.

## Requirements

| Component | Requirement |
|---|---|
| Python | 3.10-3.12 |
| Wallet | Registered miner hotkey on subnet 105 |
| Network | Stable outbound access to BeamCore HTTP, BeamCore NATS, Bittensor, and storage backends |
| Port | Default orchestrator HTTP/worker-gateway port `8000` unless `API_PORT` is changed |

## Install

```bash
git clone https://github.com/Beam-Network/beam.git
cd beam
python3 -m venv .venv
source .venv/bin/activate
pip install -e "."
```

## Register On Subnet 105

```bash
btcli subnet register --netuid 105 --subtensor.network finney \
  --wallet.name orchestrator --wallet.hotkey default
```

Confirm the hotkey is registered:

```bash
btcli wallet overview --wallet.name orchestrator --subtensor.network finney
```

## Configure

Create `neurons/orchestrator/.env` or set these variables in your process manager:

```dotenv
WALLET_NAME=orchestrator
WALLET_HOTKEY=default
CORE_SERVER_URL=https://beamcore.b1m.ai
ORCH_GATEWAY_URL=tls://orch-gateway.b1m.ai:4222
ORCHESTRATOR_WORKER_GATEWAY_URL=https://orchestrator.example.com
SUBTENSOR_NETWORK=finney
NETUID=105
READY=true

# Optional
API_PORT=8000
LOG_LEVEL=INFO
REGION=global
FEE_PERCENTAGE=0
MAX_WORKERS=10000
```

Important settings:

| Variable | Purpose |
|---|---|
| `CORE_SERVER_URL` | BeamCore HTTP base used for registration/auth bootstrap |
| `ORCH_GATEWAY_URL` | BeamCore NATS control endpoint |
| `ORCHESTRATOR_WORKER_GATEWAY_URL` | Public worker gateway origin advertised to BeamCore |
| `READY` | `true` opts the orchestrator into routed work; default is `false` |
| `API_PORT` | FastAPI port and in-process worker-gateway port |

The documented production path uses BeamCore HTTP registration, BeamCore NATS control, and the orchestrator-owned worker gateway.

## Run

```bash
cd neurons/orchestrator
source ../../.venv/bin/activate
python main.py
```

## Health And Readiness

```bash
curl http://localhost:8000/health
```

Actual basic health response:

```json
{
  "status": "healthy",
  "service": "beam-orchestrator"
}
```

Use `/ready` for readiness checks:

```bash
curl http://localhost:8000/ready | jq
```

The readiness response includes wallet, subtensor, metagraph, worker availability, background task checks, and `active_workers`.

Other useful endpoints:

| Endpoint | Purpose |
|---|---|
| `GET /state` | Full orchestrator state |
| `GET /workers/stats` | Connected worker stats |
| `GET /metrics` | Prometheus metrics |
| `GET /metrics/json` | JSON metrics |

Logs default to `/tmp/beam_logs/orchestrator.log` unless `LOG_DIR` is set.

## Worker Gateway

The in-process worker gateway accepts:

```text
ws(s)://<worker-gateway-origin>/ws/<worker_id>?api_key=<worker-api-key>
```

Workers derive this URL from `WORKER_GATEWAY_URL`. If the orchestrator is reachable directly on `https://orchestrator.example.com`, set:

```dotenv
ORCHESTRATOR_WORKER_GATEWAY_URL=https://orchestrator.example.com
```

Then each worker owned by this orchestrator should use:

```dotenv
WORKER_GATEWAY_URL=https://orchestrator.example.com
```

The gateway relays:

| Direction | Message types |
|---|---|
| BeamCore/orchestrator to worker | `task_offer`, `task_accept_ack`, `task_reject_ack`, `task_result_ack` |
| Worker to BeamCore/orchestrator | `task_accept`, `task_reject`, `task_result` |

## Task Offer Flow

```text
BeamCore -> NATS -> orchestrator -> worker gateway -> worker
worker -> worker gateway -> orchestrator -> NATS -> BeamCore
worker -> worker gateway -> orchestrator -> NATS -> BeamCore task_result
```

Each task offer includes executable URLs, headers, `signed_url_flow`, and `minimum_worker_version`. `signed_url_v1` object-storage upload offers use direct multipart URLs. Current public workers report `0.2.1`. The orchestrator chooses a connected worker; BeamCore owns stalled-task recovery and reassignment.

## Troubleshooting

### No tasks are assigned

- Confirm `READY=true`.
- Confirm the NATS endpoint in `ORCH_GATEWAY_URL` is reachable.
- Confirm the hotkey is registered on subnet 105.
- Confirm at least one worker is connected to the worker gateway.
- Check `/ready` for failed readiness checks.

### Worker cannot connect

- Confirm `WORKER_GATEWAY_URL` points to the worker gateway origin.
- Confirm the gateway is reachable from the worker host.
- Confirm the worker registered with BeamCore and has a worker API key.

### BeamCore NATS connection fails

```bash
curl https://beamcore.b1m.ai/health
```

Check network egress, DNS, wallet signing errors, API-key validity, and `ORCH_GATEWAY_URL`.

## Production Service Example

Use your actual clone path in place of `/srv/beam`:

```ini
[Unit]
Description=BEAM Orchestrator
After=network.target

[Service]
Type=simple
User=beam
WorkingDirectory=/srv/beam/neurons/orchestrator
Environment="PATH=/srv/beam/.venv/bin:/usr/local/bin:/usr/bin:/bin"
EnvironmentFile=/srv/beam/neurons/orchestrator/.env
ExecStart=/srv/beam/.venv/bin/python main.py
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
```
