# Beam Orchestrator

The Go orchestrator connects to BeamCore over Core NATS, routes `transfer.multipart` and `room.transfer` work to WCP workers, and relays results to BeamCore.

## Requirements

- Go 1.24+
- Bittensor miner hotkey registered on subnet 105
- BeamCore orchestrator registration response with `orchestrator_id` and `api_key`
- Public orchestrator gateway URL
- WCP TLS certificate and key
- Room tunnel coordinator URL and worker token for `room.transfer`

## Install

```bash
git clone https://github.com/Beam-Network/beam.git
cd beam
mkdir -p bin
go build -o bin/beam-orchestrator ./cmd/beam-orchestrator
go build -o bin/beam-worker ./cmd/beam-worker
```

## Register

```bash
btcli subnet register --netuid 105 --subtensor.network finney \
  --wallet.name your_coldkey \
  --wallet.hotkey your_hotkey
```

Register the orchestrator with BeamCore. Sign `<orchestrator_hotkey_ss58>:<fee_percentage>` with the orchestrator hotkey:

```bash
curl -X POST https://beamcore.b1m.ai/orchestrators/register \
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

## Configure

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

## Run

```bash
./bin/beam-orchestrator serve \
  --hotkey "$BEAM_BITTENSOR_HOTKEY" \
  --netuid 105
```

## Worker Membership

```bash
curl -X POST http://127.0.0.1:8781/v1/orchestrator/memberships \
  -H 'Content-Type: application/json' \
  -d '{"OrchestratorID":"orchestrator-id","WorkerID":"worker-id","NodeID":"worker-node-id","Status":"active"}'
```

## More Detail

See [../../docs/orchestrator.md](../../docs/orchestrator.md).
