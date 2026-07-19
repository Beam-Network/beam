# BEAM

BEAM participant software runs orchestrator, worker, and validator processes for the Beam subnet. Participants contribute bandwidth, execute transfer chunks, and set Bittensor weights from BeamCore's verified performance data.

BeamCore operates the public HTTP API, NATS orchestrator control endpoint, transfer coordination, PRISM evidence, and PRISM scoring services.

## Components

| Component | Runs at | Responsibility |
| --- | --- | --- |
| Orchestrator | Operator | Connects to BeamCore over NATS, advertises a worker gateway, routes task offers to workers, and relays worker results |
| Worker gateway | Operator | WebSocket edge for worker sessions at `/ws/<worker_id>?api_key=...` |
| Worker | Operator | Registers with BeamCore, connects to the worker gateway, executes chunk transfers, and sends `task_result` receipts |
| Validator | Bittensor validator | Reads BeamCore epoch summaries, sets subnet weights, and posts weight proofs |

Workers move object bytes directly between storage endpoints using task-scoped, short-lived source and destination URLs.

## Mainnet Endpoints

| Setting | Value |
| --- | --- |
| `CORE_SERVER_URL` | `https://beamcore.b1m.ai` |
| `ORCH_GATEWAY_URL` | `tls://orch-gateway.b1m.ai:4222` |
| `WORKER_GATEWAY_URL` | Your orchestrator-owned worker gateway origin |
| `ORCHESTRATOR_WORKER_GATEWAY_URL` | The worker gateway origin your orchestrator advertises |
| `SUBTENSOR_NETWORK` | `finney` |
| `NETUID` | `105` |

Set `ORCH_GATEWAY_URL` to a NATS endpoint using `nats://` or `tls://`. Set `WORKER_GATEWAY_URL` to the worker WebSocket gateway that serves `/ws/<worker_id>`.

## Run A Node

- [Orchestrator guide](docs/orchestrator.md): run a miner that receives BeamCore task batches and routes work to connected workers.
- [Worker guide](docs/worker.md): run a worker that executes chunk transfers.
- [Validator guide](docs/validator.md): run a validator that sets weights from BeamCore epoch summaries.
- [Public guide](guide/intro.md): public dashboard, PRISM, transfer, and scoring docs copied from `beam-core-dashboard/docs/docs`.

## Quick Install

```bash
git clone https://github.com/Beam-Network/beam.git
cd beam
python3 -m venv .venv
source .venv/bin/activate
pip install -e "."
```

Install validator extras when running the validator:

```bash
pip install -e ".[validator]"
```

## Runtime Flow

```text
Client -> BeamCore -> NATS -> orchestrator -> worker gateway -> worker
Worker -> storage source/destination directly
Worker -> worker gateway -> orchestrator -> NATS -> BeamCore task_result
Validator -> BeamCore epoch summary -> Bittensor set_weights -> BeamCore weight proof
```

## Links

- Dashboard: https://data.b1m.ai/
- Bittensor: https://bittensor.com
- Public docs: https://github.com/Beam-Network/beam

## License

MIT License. See [LICENSE](LICENSE).
