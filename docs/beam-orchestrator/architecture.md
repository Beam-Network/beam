# Beam Orchestrator Architecture

The Beam orchestrator connects BeamCore task routing to participant WCP workers.

## Components

| Component | Role |
| --- | --- |
| BeamCore HTTP | Participant registration and metadata |
| Core NATS | Orchestrator control messages and task offers |
| Go orchestrator | Core NATS connector, WCP server, capability aggregation, result relay |
| Go worker | WCP workload execution with `transfer.multipart` and `room.transfer` |
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
Go worker -> Room tunnel source and target leases
Go worker -> BeamLink/WCP -> Go orchestrator -> Core NATS -> BeamCore room_task_result
```

## Capabilities

`transfer.multipart` handles normal transfer chunks.

`room.transfer` handles Room data-transfer lanes.

Workers publish WCP capability manifests. The orchestrator sends `capability_update` after registration and whenever aggregated capabilities or capacity change. BeamCore keeps the last accepted manifest until replacement. Heartbeat/session state determines liveness.

## Setup Guides

- [Orchestrator guide](../orchestrator.md)
- [Worker guide](../worker.md)
- [Validator guide](../validator.md)
- [Room transfer workloads](../room-workload-foundation.md)
