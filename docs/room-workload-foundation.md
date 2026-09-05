# Room Transfer Workloads

Room transfer lets BeamCore assign file delivery lanes to participant workers inside a Room.

## Contracts

- Schema: `room-transfer/v1`
- Capabilities: `room.transfer`, `room.transfer.direct.v1`, and `room.transfer.e2ee.v1`
- Offer message: `room_task_offer_batch`
- Cancel message: `room_task_cancel`
- Result message: `room_task_result`

## Participant Setup

1. Build `beam-orchestrator` and `beam-worker`.
2. Run the orchestrator with `BEAM_ENV=prod`, BeamCore NATS auth, WCP TLS, and the Room tunnel coordinator URL.
3. Register each WCP worker membership through the orchestrator API.
4. Give direct-capable workers a public HTTP port and run them with
   `--capabilities transfer.multipart,room.transfer,room.transfer.direct.v1,room.transfer.e2ee.v1`,
   `--room-transfer-addr`, and `--room-transfer-advertise-url`.

## Flow

BeamCore sends `room_task_offer_batch` to an eligible orchestrator.

The orchestrator redeems independently scoped source and target path leases,
selects a connected WCP worker that advertises the direct and E2EE capabilities,
and sends a `room.transfer` workload.

The Worker publishes a per-workload bearer and direct runtime URL through
progress. Source and target agents learn that runtime from signed room status,
then connect outbound with both the runtime bearer and their own path token.
The source encrypts each immutable object cell with the room/channel key epoch
and uploads one signed ciphertext chunk at a time. Targets authenticate and
decrypt locally, write plaintext to their inboxes, and return signed range and
final receipts over the ciphertext commitment. The Worker holds at most one
ciphertext chunk per active lane, checkpoints completed cells, and returns the
receipts through the orchestrator.

The orchestrator sends `room_task_result` to BeamCore.

Room transfers do not allocate a relay session or expose an agent listener.
The coordinator controls membership and path authorization, while the selected
Worker owns the shared data service in the same pattern as Worker-hosted media.
Workers validate the required protection envelope and key epoch but never
receive room/channel keys. Missing E2EE capability or invalid protection fails
closed.

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
  --capabilities transfer.multipart,room.transfer,room.transfer.direct.v1,room.transfer.e2ee.v1 \
  --room-transfer-addr 0.0.0.0:9470 \
  --room-transfer-advertise-url https://worker.example.com:9470
```
