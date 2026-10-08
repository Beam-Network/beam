# BEAM Worker Guide

Run a Go worker on BEAM mainnet for `transfer.multipart`, room transfer, and
worker-hosted room workloads.

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
  -H "Authorization: Bearer $(cat data/orchestrator/control-token)" \
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
  --capabilities transfer.multipart,transfer.multipart.fanout.v1,room.transfer,room.transfer.direct.v1,room.transfer.e2ee.v2,storage.probe.relay.v2 \
  --room-transfer-addr 0.0.0.0:9470 \
  --room-transfer-advertise-url https://worker.example.com:9470
```

Agent-only room transfers are end-to-end encrypted. Encryption keys and
plaintext remain in the room agents.

Publications combining agents and object storage require
`room.transfer.storage.v2`. All legs of a storage publication use TLS with
worker-visible plaintext.
No worker receives storage credentials or room keys.

Hybrid workers require explicit HTTPS listener configuration before advertising
support:

```bash
export BEAM_ROOM_STORAGE_LISTEN_ADDR=0.0.0.0:9443
export BEAM_ROOM_STORAGE_ADVERTISE_URL=https://worker.example.com:9443
```

Include `room.transfer.storage.v2` and `room.transfer` in the worker capabilities.
The listener binds before registration. Agents authenticate its TLS 1.3
certificate through the coordinator-authorized assignment; agents remain
outbound clients. See [hybrid room execution](room-storage-hybrid.md) for
listener requirements and transport security.

Add `BEAM_ORCHESTRATOR_DELEGATION=<base64url-value>` when the membership response includes a delegation.

## Room Endpoint Capabilities

Enable each versioned room endpoint together with its base capability. Startup
fails closed when a pair is incomplete or its HTTPS advertise URL is missing:

| Capabilities | Flags (environment) |
| --- | --- |
| `room.media` + `room.media.webrtc.v1` | `--media-addr` (`BEAM_MEDIA_LISTEN_ADDR`), `--media-advertise-url` (`BEAM_MEDIA_ADVERTISE_URL`), `--media-public-ip` (`BEAM_MEDIA_PUBLIC_IP`), `--media-udp-port-min` / `--media-udp-port-max` (`BEAM_MEDIA_UDP_PORT_MIN` / `BEAM_MEDIA_UDP_PORT_MAX`) |
| `room.message` + `room.message.direct.v1` | `--room-message-addr` (`BEAM_ROOM_MESSAGE_LISTEN_ADDR`), `--room-message-advertise-url` (`BEAM_ROOM_MESSAGE_ADVERTISE_URL`) |
| `room.transfer` + `room.transfer.storage.v2` | `--room-storage-addr` (`BEAM_ROOM_STORAGE_LISTEN_ADDR`), `--room-storage-advertise-url` (`BEAM_ROOM_STORAGE_ADVERTISE_URL`) |

Room messages are placed only on workers that enable `room.message.direct.v1`.

Advertise URLs use `https://` without credentials, query, or fragment; `{port}`
expands to the bound listener port. These listeners serve short-lived
self-signed TLS certificates that agents pin by fingerprint, so each port must
reach the worker directly through TCP passthrough; a TLS-terminating proxy or
load balancer breaks the pin. Open the media UDP port range to clients and set
the media public IP to the address they reach. For clients behind restrictive
networks, optionally list STUN/TURN URLs in `--media-ice-servers`
(`BEAM_MEDIA_ICE_SERVERS`) and set `--media-turn-secret`
(`BEAM_MEDIA_TURN_SECRET`) to issue temporary TURN credentials.

## Storage Probe Relay

`storage.probe.relay.v2` lets BeamCore check a storage host from the worker's
network: for each BeamCore-signed request, the worker opens one outbound TCP
connection to the named host and port and forwards bytes. TLS runs end to end
between BeamCore and the host, so the worker carries only ciphertext.

## Health

```bash
./bin/beam-worker doctor --worker-id "$BEAM_WORKER_ID"
curl http://127.0.0.1:8780/healthz
```

Other control API routes need the token in `data/worker/control-token`, created on first start
(or set `BEAM_WORKER_CONTROL_TOKEN`).
