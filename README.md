# BEAM

BEAM participant software runs orchestrator, worker, and validator processes for the Beam subnet. Participants contribute bandwidth, execute transfer chunks, and set Bittensor weights from BeamCore's verified performance data.

BeamCore operates the public HTTP API, orchestrator gateway, transfer lifecycle, PRISM evidence, and PRISM scoring services.

## Components

| Component | Runs at | Responsibility |
| --- | --- | --- |
| Orchestrator | Operator | Maintains the orch-gateway WebSocket, advertises a worker gateway, routes task offers to workers, and reports worker decisions/results |
| Worker gateway | Operator | WebSocket edge for worker sessions at `/ws/<worker_id>?api_key=...` |
| Worker | Operator | Registers with BeamCore, connects to the worker gateway, executes chunk transfers, and sends `task_result` receipts |
| Validator | Bittensor validator | Reads BeamCore epoch summaries, sets subnet weights, and posts weight proofs |

Object bytes do not pass through BeamCore. Workers receive only task-scoped, short-lived source and destination URLs for the chunk they are assigned.

## Mainnet Endpoints

| Setting | Value |
| --- | --- |
| `CORE_SERVER_URL` | `https://beamcore.b1m.ai` |
| `ORCH_GATEWAY_URL` | `https://orch-gateway.b1m.ai` |
| `WORKER_GATEWAY_URL` | Your orchestrator-owned worker gateway origin |
| `ORCHESTRATOR_WORKER_GATEWAY_URL` | The worker gateway origin your orchestrator advertises |
| `SUBTENSOR_NETWORK` | `finney` |
| `NETUID` | `105` |

`WORKER_GATEWAY_URL` is not BeamCore and not the orchestrator gateway. It must point at the worker WebSocket gateway that serves `/ws/<worker_id>`.

## Run A Node

- [Orchestrator guide](docs/orchestrator.md): run a miner that receives BeamCore task batches and routes work to connected workers.
- [Worker guide](docs/worker.md): run a worker that executes chunk transfers.
- [Validator guide](docs/validator.md): run a validator that sets weights from BeamCore epoch summaries.

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
Client -> BeamCore HTTP -> orch-gateway -> orchestrator -> worker gateway -> worker
Worker -> storage source/destination directly
Worker -> worker gateway -> orchestrator -> orch-gateway -> BeamCore task_result
Validator -> BeamCore epoch summary -> Bittensor set_weights -> BeamCore weight proof
```

## Links

- Dashboard: https://data.b1m.ai/
- Bittensor: https://bittensor.com
- Public docs: https://github.com/Beam-Network/beam

## License

MIT License. See [LICENSE](LICENSE).