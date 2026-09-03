# BEAM

BEAM participant software runs orchestrator, worker, and validator processes for subnet 105.

BeamCore provides the public HTTP API, Core NATS control gateway, transfer lifecycle, PRISM evidence, and PRISM scoring.

## Components

| Component | Responsibility |
| --- | --- |
| Orchestrator | Connects to BeamCore over Core NATS, publishes capability updates, routes workload offers to workers over BeamLink/WCP, and relays results |
| Worker | Connects to its orchestrator over BeamLink/WCP, advertises capabilities, executes `transfer.multipart` and `room.transfer`, and returns signed results |
| Validator | Reads BeamCore epoch summaries, sets subnet weights, and posts weight proofs |

Workers move object bytes directly between source and destination endpoints using task-scoped URLs and room-scoped leases.

## Requirements

- Go 1.24+ for orchestrator and worker
- Python 3.10-3.12 for validator
- Bittensor hotkey registered on subnet 105
- Network access to BeamCore, Core NATS, orchestrator WCP, Bittensor, and task storage URLs

## Install

Build orchestrator and worker:

```bash
git clone https://github.com/Beam-Network/beam.git
cd beam
mkdir -p bin
go build -o bin/beam-orchestrator ./cmd/beam-orchestrator
go build -o bin/beam-worker ./cmd/beam-worker
```

Install validator dependencies:

```bash
python3 -m venv .venv
source .venv/bin/activate
python -m pip install --upgrade pip
python -m pip install -e ".[validator]"
```

## Mainnet Settings

```bash
export CORE_SERVER_URL=https://beamcore.b1m.ai
export BEAM_ENV=prod
export BEAMCORE_NATS_URL=tls://orch-gateway.b1m.ai:4222
export SUBTENSOR_NETWORK=finney
export NETUID=105
```

Use the orchestrator API key as `BEAMCORE_NATS_PASSWORD` with `BEAMCORE_NATS_USER` set to the orchestrator hotkey. Credentials-file auth uses `BEAMCORE_NATS_CREDS`. Token auth uses `BEAMCORE_NATS_TOKEN`.

## Run

- [Orchestrator guide](docs/orchestrator.md): run a miner that receives `worker_task_offer_batch` and `room_task_offer_batch`.
- [Worker guide](docs/worker.md): run a worker that advertises and executes `transfer.multipart` and `room.transfer`.
- [Validator guide](docs/validator.md): run a validator that sets weights from BeamCore epoch summaries.

## Runtime Flow

```text
Client -> BeamCore HTTP -> Transfer Runtime -> Core NATS -> orchestrator -> BeamLink/WCP -> worker
Worker -> storage source/destination or Room transfer leases
Worker -> BeamLink/WCP -> orchestrator -> Core NATS -> Transfer Runtime result
Validator -> BeamCore epoch summary -> Bittensor set_weights -> BeamCore weight proof
```

## Links

- Dashboard: https://data.b1m.ai/
- Bittensor: https://bittensor.com
- Public docs: https://github.com/Beam-Network/beam

## License

MIT License. See [LICENSE](LICENSE).
