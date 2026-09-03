# BEAM Worker Guide

Run a Go worker on BEAM mainnet for `transfer.multipart` and `room.transfer`.

## Requirements

- Go 1.24+
- Bittensor worker hotkey registered on subnet 105
- BeamCore worker registration response with `worker_id`
- Orchestrator WCP address, CA, server name, and membership

## 1. Install

```bash
git clone https://github.com/Beam-Network/beam.git
cd beam
mkdir -p bin
go build -o bin/beam-worker ./cmd/beam-worker
```

## 2. Register

Register the hotkey on subnet 105:

```bash
btcli subnet register --netuid 105 --subtensor.network finney \
  --wallet.name your_coldkey \
  --wallet.hotkey your_hotkey
```

Register the worker with BeamCore. Sign the message `<worker_hotkey_ss58>:<public_ip>:9000` with the worker hotkey and send:

```bash
export CORE_SERVER_URL=https://beamcore.b1m.ai

curl -X POST "$CORE_SERVER_URL/workers/register" \
  -H 'Content-Type: application/json' \
  -d '{
    "hotkey": "worker_hotkey_ss58",
    "coldkey": "worker_coldkey_ss58",
    "ip": "worker_public_ip",
    "port": 9000,
    "claimed_bandwidth_mbps": 100,
    "signature": "0x..."
  }'
```

Store the returned `worker_id` and `api_key`.

## 3. Join The Orchestrator

Create or read the persistent BeamLink node identity:

```bash
./bin/beam-worker node-id --node-key data/worker/node.key
```

Register the worker membership on the orchestrator:

```bash
curl -X POST http://127.0.0.1:8781/v1/orchestrator/memberships \
  -H 'Content-Type: application/json' \
  -d '{"OrchestratorID":"orchestrator-id","WorkerID":"worker-id","NodeID":"worker-node-id","Status":"active"}'
```

## 4. Run

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

Add `BEAM_ORCHESTRATOR_DELEGATION=<base64url-value>` when the membership response includes a delegation.

## Health

```bash
./bin/beam-worker doctor --worker-id "$BEAM_WORKER_ID"
curl http://127.0.0.1:8780/health
```
