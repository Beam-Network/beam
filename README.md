# BEAM

BEAM participant software runs orchestrator, worker, and validator processes for subnet 105.

BeamCore provides the public HTTP API, Core NATS control gateway, transfer lifecycle, PRISM evidence, and PRISM scoring.

## Components

| Component | Responsibility |
| --- | --- |
| Orchestrator | Connects to BeamCore over Core NATS, publishes capability updates, routes workload offers to workers over BeamLink/WCP, and relays results |
| Worker | Connects to its orchestrator over BeamLink/WCP, advertises capabilities, executes `transfer.multipart`, carries E2EE `room.transfer` ciphertext, and returns signed results |
| Validator | Reads BeamCore epoch summaries, sets subnet weights, and posts weight proofs |

Agent-only room transfers are end-to-end encrypted; workers do not receive
plaintext or room keys. Room transfers involving object storage use TLS with
worker-visible plaintext. Workers do not receive storage credentials or room keys.

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

Use the orchestrator API key as `BEAMCORE_NATS_PASSWORD` with `BEAMCORE_NATS_USER` set to the orchestrator hotkey. Set `BEAM_PUBLIC_API_URL=https://beamcore.b1m.ai` for connection diagnostics. Credentials-file auth uses `BEAMCORE_NATS_CREDS`. Token auth uses `BEAMCORE_NATS_TOKEN`.

For production, use the TLS-first control endpoint
`tls://orch-gateway.b1m.ai:4222`. The official Go client uses NATS protocol 1;
an `invalid client protocol` error indicates that the running binary should be
checked and rebuilt from the official source. See the orchestrator guide for
provenance commands.

## Run

- [Orchestrator guide](docs/orchestrator.md): run a miner that receives `worker_task_offer_batch` and `room_task_offer_batch`.
- [Worker guide](docs/worker.md): run a worker that advertises and executes `transfer.multipart` and direct E2EE `room.transfer` workloads.
- [Validator guide](docs/validator.md): run a validator that sets weights from BeamCore epoch summaries.

## Links

- Dashboard: https://data.b1m.ai/
- Bittensor: https://bittensor.com
- Public docs: https://github.com/Beam-Network/beam

## License

MIT License. See [LICENSE](LICENSE).
