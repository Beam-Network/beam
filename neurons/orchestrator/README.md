# Beam Orchestrator

Orchestrators coordinate data transfers and manage worker pools on the Beam subnet. They connect to BeamCore over NATS through `ORCH_GATEWAY_URL`, advertise a worker gateway, route task offers to connected workers, and relay worker decisions/results back to BeamCore.

## Install

```bash
# From the repository root
python3 -m venv .venv
source .venv/bin/activate
pip install -e "."
```

## Mainnet Quick Start

```bash
cd neurons/orchestrator

WALLET_NAME=your_coldkey \
WALLET_HOTKEY=your_hotkey \
SUBTENSOR_NETWORK=finney \
NETUID=105 \
CORE_SERVER_URL=https://beamcore.b1m.ai \
ORCH_GATEWAY_URL=tls://orch-gateway.b1m.ai:4222 \
ORCHESTRATOR_WORKER_GATEWAY_URL=https://orchestrator.example.com \
READY=true \
python main.py
```

## Configuration

| Variable                          | Description                                         | Production value              |
| --------------------------------- | --------------------------------------------------- | ----------------------------- |
| `WALLET_NAME`                     | Bittensor wallet name                               | your wallet name              |
| `WALLET_HOTKEY`                   | Bittensor hotkey name                               | your miner hotkey             |
| `SUBTENSOR_NETWORK`               | Bittensor network                                   | `finney`                      |
| `NETUID`                          | Beam subnet UID                                     | `105`                         |
| `CORE_SERVER_URL`                 | BeamCore HTTP base                                  | `https://beamcore.b1m.ai`     |
| `ORCH_GATEWAY_URL`                | BeamCore NATS control endpoint                      | `tls://orch-gateway.b1m.ai:4222` |
| `ORCHESTRATOR_WORKER_GATEWAY_URL` | Public worker gateway origin advertised to BeamCore | operator-provided             |
| `READY`                           | Opt in to routed work                               | `true`                        |
| `API_PORT`                        | Local HTTP and in-process worker-gateway port       | `8000`                        |

Set `ORCH_GATEWAY_URL` to a NATS endpoint using `nats://` or `tls://`. `READY` defaults to `false` in code. Set it to `true` when the orchestrator should receive production task offers.

## Worker Gateway

Workers owned by this orchestrator connect to:

```text
ws(s)://<worker-gateway-origin>/ws/<worker_id>?api_key=<worker-api-key>
```

The worker derives this from `WORKER_GATEWAY_URL`. If `ORCHESTRATOR_WORKER_GATEWAY_URL=https://orchestrator.example.com`, workers should use:

```dotenv
WORKER_GATEWAY_URL=https://orchestrator.example.com
```

## Run With `.env`

```bash
cd neurons/orchestrator
python main.py
```

## Health

```bash
curl http://localhost:8000/health
```

```json
{
	"status": "healthy",
	"service": "beam-orchestrator"
}
```

Use `GET /ready` for wallet, subtensor, metagraph, worker availability, and background task readiness.

## More Detail

See [`../../docs/orchestrator.md`](../../docs/orchestrator.md).
