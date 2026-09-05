# Beam Orchestrator Architecture

The Beam orchestrator connects BeamCore task routing to participant WCP workers.

## Components

| Component | Role |
| --- | --- |
| BeamCore HTTP | Participant registration and metadata |
| Core NATS | Orchestrator control messages and task offers |
| Go orchestrator | Core NATS connector, WCP server, capability aggregation, result relay |
| Go worker | WCP execution with `transfer.multipart` and ciphertext-only `room.transfer` using `room.transfer.direct.v1` plus `room.transfer.e2ee.v1` |
| Validator | Bittensor weight setting from BeamCore PRISM epoch summaries |

## Transfer Flow

```text
BeamCore -> Core NATS -> Go orchestrator -> BeamLink/WCP -> Go worker
Go worker -> storage source/destination
Go worker -> BeamLink/WCP -> Go orchestrator -> Core NATS -> BeamCore task_result
```

## Room Transfer Flow

```text
BeamCore -> Core NATS -> Go orchestrator -> BeamLink/WCP -> Go worker
Go worker -> shared direct HTTP runtime <- outbound source/target agents
Go worker -> BeamLink/WCP -> Go orchestrator -> Core NATS -> BeamCore room_task_result
```

## Capabilities

`transfer.multipart` handles normal transfer chunks.

`room.transfer` identifies Room data-transfer lanes.

`room.transfer.direct.v1` selects workers that host the token-authenticated
direct runtime. The base capability alone is not eligible for these lanes.

`room.transfer.e2ee.v1` requires `btr.object.chunk.aead.v1` ciphertext from
source agents. Room/channel keys and plaintext remain outside Workers.

Workers publish WCP capability manifests. The orchestrator sends `capability_update` after registration and whenever aggregated capabilities or capacity change. BeamCore keeps the last accepted manifest until replacement. Heartbeat/session state determines liveness.

## Setup Guides

- [Orchestrator guide](../orchestrator.md)
- [Worker guide](../worker.md)
- [Validator guide](../validator.md)
- [Room transfer workloads](../room-workload-foundation.md)
