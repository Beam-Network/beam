# Room Transfer Workloads

Room transfer lets BeamCore assign file delivery lanes to participant workers inside a Room.

## Contracts

- Schema: `room-transfer/v1`
- Capability: `room.transfer`
- Offer message: `room_task_offer_batch`
- Cancel message: `room_task_cancel`
- Result message: `room_task_result`

## Participant Setup

1. Build `beam-orchestrator` and `beam-worker`.
2. Run the orchestrator with `BEAM_ENV=prod`, BeamCore NATS auth, WCP TLS, and Room tunnel coordinator settings.
3. Register each WCP worker membership through the orchestrator API.
4. Run workers with `--capabilities transfer.multipart,room.transfer`.

## Flow

BeamCore sends `room_task_offer_batch` to an eligible orchestrator.

The orchestrator redeems scoped Room tunnel leases, selects a connected WCP worker, and sends a `room.transfer` workload.

The worker reads assigned source chunk ranges, writes target members, checkpoints delivered cells, and returns signed receipts.

The orchestrator sends `room_task_result` to BeamCore.

## Commands

```bash
export BEAM_ENV=prod
export BEAM_BITTENSOR_HOTKEY=orchestrator_hotkey_ss58
export BEAMCORE_NATS_URL=tls://orch-gateway.b1m.ai:4222
export BEAMCORE_NATS_USER=orchestrator_hotkey_ss58
export BEAMCORE_NATS_PASSWORD=orchestrator-api-key
export BEAMCORE_GATEWAY_URL=https://orchestrator.example.com
export BEAM_WCP_LISTEN_ADDR=0.0.0.0:8782
export BEAM_WCP_TLS_CERT=/path/to/wcp.crt
export BEAM_WCP_TLS_KEY=/path/to/wcp.key
export BEAM_ROOM_TUNNEL_COORDINATOR_URL=https://coordinator.b1m.ai

./bin/beam-orchestrator serve \
  --hotkey "$BEAM_BITTENSOR_HOTKEY" \
  --netuid 105
```

```bash
./bin/beam-worker node-id --node-key data/worker/node.key
```

```bash
curl -X POST http://127.0.0.1:8781/v1/orchestrator/memberships \
  -H 'Content-Type: application/json' \
  -d '{"OrchestratorID":"orchestrator-id","WorkerID":"worker-id","NodeID":"worker-node-id","Status":"active"}'
```

```bash
export BEAM_WORKER_ID=worker-id
export BEAM_ORCHESTRATOR_ID=orchestrator-id
export BEAM_WCP_ADDRESS=orchestrator.example.com:8782
export BEAM_WCP_CA=/path/to/wcp-ca.pem
export BEAM_WCP_SERVER_NAME=orchestrator.example.com

./bin/beam-worker serve \
  --node-key data/worker/node.key \
  --capabilities transfer.multipart,room.transfer
```
