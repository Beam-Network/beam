# Beam Orchestrator Runtime

The public participant runtime includes Go orchestrators, Go workers, Room transfer support, and validators for subnet 105.

## Included Runtime Paths

| Path | Command | Capabilities |
| --- | --- | --- |
| Go orchestrator | `./bin/beam-orchestrator serve` | Core NATS, WCP routing, `worker_task_offer_batch`, `room_task_offer_batch`, `capability_update` |
| Go worker | `./bin/beam-worker serve` | `transfer.multipart`, `room.transfer`, `room.transfer.direct.v1`, `room.transfer.e2ee.v2` |
| Validator | `python actors/validator/main.py` | BeamCore PRISM weight setting |

## Build

```bash
git clone https://github.com/Beam-Network/beam.git
cd beam
mkdir -p bin
go build -o bin/beam-orchestrator ./cmd/beam-orchestrator
go build -o bin/beam-worker ./cmd/beam-worker
```

Validator setup:

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

## Room Transfer Contracts

- Schema: `room-transfer/v1`
- Worker capabilities: `room.transfer`, `room.transfer.direct.v1`, and `room.transfer.e2ee.v2`
- Transfer capability: `transfer.multipart`
- Offer: `room_task_offer_batch`
- Cancel: `room_task_cancel`
- Result: `room_task_result`
- Manifest update: `capability_update`, sent after registration and on capability/capacity changes

## Guides

- [Orchestrator guide](../orchestrator.md)
- [Worker guide](../worker.md)
- [Room transfer workloads](../room-workload-foundation.md)
