# BEAM Orchestrator Guide

Run a Go orchestrator on BEAM mainnet for `worker_task_offer_batch` and `room_task_offer_batch`.

## Requirements

- Go 1.24+
- Bittensor miner hotkey registered on subnet 105
- BeamCore orchestrator registration response with `orchestrator_id` and `api_key`
- Public orchestrator gateway URL
- WCP TLS certificate and key
- Room tunnel coordinator URL for `room.transfer` and generic room workloads

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
export BEAM_ROOM_TUNNEL_COORDINATOR_URL=https://coordinator.b1m.ai
```

Credentials-file auth uses `BEAMCORE_NATS_CREDS`. Token auth uses `BEAMCORE_NATS_TOKEN`.

Production uses `tls://orch-gateway.b1m.ai:4222`. The official client performs
the TLS handshake before the NATS `INFO` exchange whenever the URL uses the
`tls://` scheme.

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

`room.transfer` identifies Room data-transfer lanes from `room_task_offer_batch`.
For current Room transfers, the orchestrator advertises eligibility only when
connected workers also expose `room.transfer.direct.v1` and
`room.transfer.e2ee.v2`.

The orchestrator publishes `capability_update` after registration and whenever advertised capabilities or capacity change. BeamCore keeps the last accepted manifest until replacement. Heartbeat/session state determines liveness.

## Result Settlement

Each BeamCore `task_result` describes exactly one transfer part. The
orchestrator reads `sha256` and `etag` from that part's indexed execution
outputs and treats missing or multi-part evidence as a visible terminal local
failure.

The NATS `task_result_ack` reply is the authoritative disposition. `completed`
and `owned_processing` are accepted, `retry` is retried, and `failed`,
`rejected`, `late_expired`, and `late_superseded` are terminal. Do not call a
second HTTP acknowledgement endpoint; none is required for result settlement.

## NATS Connectivity and Build Provenance

The canonical Go runtime uses `github.com/nats-io/nats.go` and NATS protocol 1.
The production gateway does not accept client protocol 2. An
`-ERR invalid client protocol` response therefore indicates a non-canonical or
stale binary, not a requirement to downgrade the gateway.

Record the source revision and embedded Go module metadata before replacing a
binary:

```bash
git rev-parse HEAD
./bin/beam-orchestrator version
go version -m ./bin/beam-orchestrator
go list -m github.com/nats-io/nats.go
```

Rebuild the official source with the supported Go toolchain when provenance is
missing or differs:

```bash
git pull --ff-only
go clean -cache
go build -trimpath -o bin/beam-orchestrator ./cmd/beam-orchestrator
```

## Health

```bash
curl http://127.0.0.1:8781/v1/orchestrator/health
```
