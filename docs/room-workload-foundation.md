# Room Transfer Workloads

Room transfer lets BeamCore assign file delivery lanes to participant workers inside a Room.

Forward signed lease intents unchanged, including the exact `expires_at`
string. Parse deadlines separately for expiry checks; reformatting a signed
timestamp invalidates the intent even when it represents the same instant.

## Capabilities

- Capabilities: `room.transfer`, `room.transfer.direct.v1`, and `room.transfer.e2ee.v2`

## Participant Setup

1. Build `beam-orchestrator` and `beam-worker`.
2. Run the orchestrator with `BEAM_ENV=prod`, BeamCore NATS auth, WCP TLS, and the Room tunnel coordinator URL.
3. Register each WCP worker membership through the orchestrator API.
4. Give direct-capable workers a public HTTP port and run them with
   `--capabilities transfer.multipart,room.transfer,room.transfer.direct.v1,room.transfer.e2ee.v2`,
   `--room-transfer-addr`, and `--room-transfer-advertise-url`.

## Network and privacy

Room agents connect outbound to the worker's advertised endpoint and do not
need a public listening port. Agent-only transfers are end-to-end encrypted;
workers do not receive plaintext or room keys.

For publications involving object storage, use the
[hybrid room configuration](room-storage-hybrid.md).

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
  --capabilities transfer.multipart,room.transfer,room.transfer.direct.v1,room.transfer.e2ee.v2 \
  --room-transfer-addr 0.0.0.0:9470 \
  --room-transfer-advertise-url https://worker.example.com:9470
```
