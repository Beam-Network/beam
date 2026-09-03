# BEAM Orchestrator Guide

Run a Go orchestrator on BEAM mainnet for `worker_task_offer_batch` and `room_task_offer_batch`.

## Requirements

- Go 1.24+
- Bittensor miner hotkey registered on subnet 105
- BeamCore orchestrator registration response with `orchestrator_id` and `api_key`
- Public orchestrator gateway URL
- WCP TLS certificate and key
- Room tunnel coordinator URL and worker token for `room.transfer`

## 1. Install

```bash
git clone https://github.com/Beam-Network/beam.git
cd beam
mkdir -p bin
go build -o bin/beam-orchestrator ./cmd/beam-orchestrator
go build -o bin/beam-worker ./cmd/beam-worker
```

## 2. Register

Register the hotkey on subnet 105:

```bash
btcli subnet register --netuid 105 --subtensor.network finney \
  --wallet.name your_coldkey \
  --wallet.hotkey your_hotkey
```

Register the orchestrator with BeamCore. Sign `<orchestrator_hotkey_ss58>:<fee_percentage>` with the orchestrator hotkey and send:

```bash
export CORE_SERVER_URL=https://beamcore.b1m.ai

curl -X POST "$CORE_SERVER_URL/orchestrators/register" \
  -H 'Content-Type: application/json' \
  -d '{
    "hotkey": "orchestrator_hotkey_ss58",
    "signature": "0x...",
    "fee_percentage": 10,
    "name": "my-orchestrator",
    "region": "global",
    "url": "https://orchestrator.example.com",
    "max_workers": 1000
  }'
```

Store the returned `orchestrator_id` and `api_key`.

## 3. Configure

```bash
export CORE_SERVER_URL=https://beamcore.b1m.ai
export BEAM_ENV=prod
export BEAM_BITTENSOR_HOTKEY=orchestrator_hotkey_ss58
export BEAMCORE_NATS_URL=tls://orch-gateway.b1m.ai:4222
export BEAMCORE_NATS_USER=orchestrator_hotkey_ss58
export BEAMCORE_NATS_PASSWORD=orchestrator-api-key
export BEAMCORE_GATEWAY_URL=https://orchestrator.example.com
export BEAM_WCP_LISTEN_ADDR=0.0.0.0:8782
export BEAM_WCP_TLS_CERT=/path/to/wcp.crt
export BEAM_WCP_TLS_KEY=/path/to/wcp.key
export BEAM_ROOM_TUNNEL_COORDINATOR_URL=https://room-coordinator.example.com
export BEAM_ROOM_TUNNEL_WORKER_TOKEN=room-tunnel-token
```

Credentials-file auth uses `BEAMCORE_NATS_CREDS`. Token auth uses `BEAMCORE_NATS_TOKEN`.

## 4. Run

```bash
./bin/beam-orchestrator serve \
  --hotkey "$BEAM_BITTENSOR_HOTKEY" \
  --netuid 105
```

The startup log and `data/orchestrator/registry.json` contain the local `orchestrator_id`.

## 5. Register Worker Membership

```bash
curl -X POST http://127.0.0.1:8781/v1/orchestrator/memberships \
  -H 'Content-Type: application/json' \
  -d '{"OrchestratorID":"orchestrator-id","WorkerID":"worker-id","NodeID":"worker-node-id","Status":"active"}'
```

## Capabilities

`transfer.multipart` handles normal transfer chunks from `worker_task_offer_batch`.

`room.transfer` handles Room data-transfer lanes from `room_task_offer_batch`.

The orchestrator publishes `capability_update` from connected WCP worker manifests and live capacity.

## Health

```bash
curl http://127.0.0.1:8781/v1/orchestrator/health
```
