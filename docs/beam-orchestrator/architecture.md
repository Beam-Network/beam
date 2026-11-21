# Beam Orchestrator Architecture

The Beam orchestrator connects BeamCore task routing to participant WCP workers.

## Components

| Component | Role |
| --- | --- |
| BeamCore HTTP | Participant registration and metadata |
| Core NATS | Orchestrator control messages and task offers |
| Go orchestrator | Core NATS connector, WCP server, capability aggregation, result relay |
| Go worker | WCP execution with `transfer.multipart` and ciphertext-only `room.transfer` using `room.transfer.direct.v1` plus `room.transfer.e2ee.v2` |
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

`room.transfer.e2ee.v2` requires `btr.object.chunk.aead.v1` ciphertext from
source agents. Room/channel keys and plaintext remain outside Workers.

`room.transfer.lease_renewal.v1` keeps a running Room lane on its attempt
across lease boundaries. BeamCore sends `room_lane_lease_update` with the later
expiry of the lane's own leases; the orchestrator forwards it to the lane's
Worker (WCP `workload.extend`), which moves its deadline and reports its
runtime at that expiry. The orchestrator advertises it while a selectable Room
Worker does.

`room.transfer.stream_relay.v1` marks a Worker that holds a Room lane's source
upload until it can accept the chunk and holds target downloads until the chunk
arrives, on kept-alive connections. Targets receive the bytes while the upload
is in progress; the final byte follows once the Worker verified the whole chunk
against the source's signed range hash. The orchestrator advertises it while a
selectable Room Worker does.

## Setup Guides

- [Orchestrator guide](../orchestrator.md)
- [Worker guide](../worker.md)
- [Validator guide](../validator.md)
- [Room transfer workloads](../room-workload-foundation.md)
