# BEAM Orchestrator Guide

Run a Go orchestrator on BEAM mainnet for transfer and room workloads.

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

Run it under a supervisor that always restarts it, such as systemd with `Restart=always`: the orchestrator exits with an error when its BeamCore connection fails.

## 5. Register Worker Membership

```bash
curl -X POST http://127.0.0.1:8781/v1/orchestrator/memberships \
  -H "Authorization: Bearer $(cat data/orchestrator/control-token)" \
  -H 'Content-Type: application/json' \
  -d '{"OrchestratorID":"orchestrator-id","WorkerID":"worker-id","NodeID":"worker-node-id","Status":"active"}'
```

## Capabilities

`transfer.multipart` handles normal transfer chunks. A transfer offer is one
source range delivered to every listed destination; `offer_id` is its only
identity.

A destination with `protocol` `authenticated_part_v1` or
`authenticated_form_v1` (Box is the first `profile`) declares its `method`,
`digest`, `receipt`, `max_attempts` and, for forms, `form`. Its offer requires
`transfer.authenticated_part.v1` and reserves the chunk size plus 4 MiB on the
worker. The orchestrator advertises `transfer.authenticated_part.v1` while a
connected worker that advertises it can reserve a 32 MiB part plus 4 MiB.
Destinations without a protocol keep the presigned path and its URL-derived
method. See [Authenticated Part Uploads](worker.md#authenticated-part-uploads).

An offer with `kind` `source_digest` carries `source` `{url, headers, size}`
and `digest` `{algorithms}`, and no `destinations`. The orchestrator accepts it
only with an HTTPS source without a `Range` header, a positive `size` and
exactly the `sha1` and `sha256` algorithms. It becomes one
`transfer.source_digest` workload that requires `transfer.source_digest.v1`
and reserves 4 MiB and one connection on the worker, whatever the object size.
The orchestrator advertises `transfer.source_digest.v1` (protocol version 1)
while a connected worker that advertises it can reserve that. See
[Source Digest](worker.md#source-digest).

A transfer offer has no `kind`. A `task_offer_batch` with an invalid offer, an
unknown destination `protocol` or any other offer `kind` is rejected before any
of its offers is dispatched.

`room.transfer` identifies Room data-transfer lanes.
For current Room transfers, the orchestrator advertises eligibility only when
connected workers also expose `room.transfer.direct.v1` and
`room.transfer.e2ee.v2`.

Room workloads (`room.datagram`, `room.message`, `room.command`, `room.stream`,
`room.media`) require the room tunnel coordinator. The orchestrator advertises
each room capability only while a connected worker can take it. Versioned
endpoint capabilities come in worker pairs: `room.media` +
`room.media.webrtc.v1`, `room.message` + `room.message.direct.v1`,
`room.stream` + `room.stream.direct.v1`, and `room.transfer` +
`room.transfer.storage.v2`. Room streams are placed only on workers that
advertise `room.stream.direct.v1`. A worker reports the private endpoint of a
direct message or stream attempt in its first progress report; the orchestrator
publishes it to BeamCore as `room_workload_runtime` instead of as progress.
Configure those workers as described in
[Room Endpoint Capabilities](worker.md#room-endpoint-capabilities).

`storage.probe.relay.v2` is advertised while a connected worker advertises it.
See [Storage Probe Relay](worker.md#storage-probe-relay).

## Task Results

The orchestrator sends one `task_offer_result` per finished offer. A transfer
offer's result has one outcome per destination in offer order: `{etag}` for a
delivered presigned destination, `{error}` for a failed one, plus the range
`chunk_hash`. Missing required evidence is a terminal local failure.

An authenticated destination reports
`{sha1, request_count, provider_part_id}` when delivered and
`{error, request_count, sha1}` when it failed; `sha1` is omitted when the range
was never read. `sha1` is the hash of the bytes the worker sent and
`request_count` the number of provider HTTP requests it made. A delivered
authenticated destination without a 40-character lowercase hex `sha1`, a
positive `request_count` and a `provider_part_id` is a terminal local failure.
A `provider_throttled` failure also reports `retry_after_ms`, the provider's
`Retry-After` in milliseconds when it is at most one hour, so BeamCore can
pause that provider account for every participant.

A source digest reports `{offer_id, worker_id, digest: {sha1, sha256, bytes}}`
when it completed and `{offer_id, worker_id, error}` when it failed. A
completed digest without a 40-character lowercase hex `sha1`, a 64-character
lowercase hex `sha256` and `bytes` equal to the offered `size` is a terminal
local failure. A cancelled or expired digest that reported no failure class,
such as one whose subsystem process was stopped, reports `delivery_cancelled`.

## NATS Connectivity and Build Provenance

The canonical Go runtime uses `github.com/nats-io/nats.go` and NATS protocol 1.
The production gateway does not accept client protocol 2. An
`-ERR invalid client protocol` response therefore indicates a non-canonical or
stale binary, not a requirement to downgrade the gateway.

Run only one orchestrator process per hotkey. If a process receives
`duplicate_control_session`, stop the duplicate or assign it another identity.

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
curl http://127.0.0.1:8781/healthz
```

Other API routes need the token in `data/orchestrator/control-token`, created on first start
(or set `BEAM_ORCHESTRATOR_CONTROL_TOKEN`).
