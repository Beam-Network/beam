# BEAM Worker Guide

Run a Go worker on BEAM mainnet for `transfer.multipart`, source digests, room
transfer, and worker-hosted room workloads.

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
  --capabilities transfer.multipart,transfer.multipart.fanout.v1,transfer.authenticated_part.v1,transfer.source_digest.v1,room.transfer,room.transfer.direct.v1,room.transfer.e2ee.v2,storage.probe.relay.v2 \
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
| `room.stream` + `room.stream.direct.v1` | `--room-message-addr` (`BEAM_ROOM_MESSAGE_LISTEN_ADDR`), `--room-message-advertise-url` (`BEAM_ROOM_MESSAGE_ADVERTISE_URL`) |
| `room.transfer` + `room.transfer.storage.v2` | `--room-storage-addr` (`BEAM_ROOM_STORAGE_LISTEN_ADDR`), `--room-storage-advertise-url` (`BEAM_ROOM_STORAGE_ADVERTISE_URL`) |

Room messages are placed only on workers that enable `room.message.direct.v1`,
and room streams only on workers that enable `room.stream.direct.v1`.

Direct room messages and streams share one listener: `--room-message-addr`
serves both `/v1/room-messages/` and `/v1/room-streams/` under one pinned
certificate. Workers serve `room.stream` only directly, so the Worker refuses to
start with `room.stream` or `room.stream.direct.v1` alone.

A stream session runs for one workload attempt. The source agent posts numbered
frames of its end-to-end encrypted `beam-mls-stream-v1` byte stream and names
the attempt's first sequence in `X-Beam-Stream-Base-Sequence`; each target agent
long-polls frame batches and acknowledges what it has delivered. The Worker
keeps a frame until every live target has acknowledged it, holding at most the
workload's `max_buffer_bytes`. Under `block` the slowest live target paces the
source; under `drop_oldest` a target that holds a full window back for more than
a second is dropped. A target that makes no request within the workload's
`heartbeat_timeout_ms` is dropped; a source that makes none before ending the
stream fails the session. The Worker never sees plaintext.

Advertise URLs use `https://` without credentials, query, or fragment; `{port}`
expands to the bound listener port. These listeners serve short-lived
self-signed TLS certificates that agents pin by fingerprint, so each port must
reach the worker directly through TCP passthrough; a TLS-terminating proxy or
load balancer breaks the pin. Open the media UDP port range to clients and set
the media public IP to the address they reach. For clients behind restrictive
networks, optionally list STUN/TURN URLs in `--media-ice-servers`
(`BEAM_MEDIA_ICE_SERVERS`) and set `--media-turn-secret`
(`BEAM_MEDIA_TURN_SECRET`) to issue temporary TURN credentials.

## Authenticated Part Uploads

`transfer.authenticated_part.v1` lets the worker write `transfer.multipart`
ranges to provider APIs that take authenticated uploads instead of presigned
URLs. It is on by default and needs `transfer.multipart`. Box is the first
profile (`profile: "box"`). A destination declares one of two protocols:

| Protocol | Request | Digest header | Receipt |
| --- | --- | --- | --- |
| `authenticated_part_v1` | the declared method (Box: `PUT` to the upload-session part URL) with the offer's `Authorization` and `Content-Range` | `Digest: sha=<base64 SHA-1>` | `part` → `part_id` |
| `authenticated_form_v1` | `POST` of a multipart form: the `attributes` JSON part first, then the file part | `Content-MD5: <hex SHA-1>` | `entries.0` → `id` |

The offer names the digest header and its format (`sha=base64` or `hex`), the
receipt path and its required fields; the table shows the Box values. The
bearer credential arrives in the offer's destination headers, and the worker
sends it only with that destination's upload requests.

Destinations without a protocol keep the presigned path (`presigned_put`),
which streams the range with the method derived from the presigned URL; an
offered `method` applies only to the authenticated protocols.

For an authenticated destination the worker:

1. Reserves the chunk size plus 4 MiB, reads exactly the assigned range into
   one buffer, and computes SHA-1 and SHA-256 in the same pass. A short or long
   source read fails with `source_length_mismatch` before any provider request.
2. Sends the declared digest header with the declared method (`PUT` or `POST`)
   over HTTPS. Redirects are never followed.
3. Retries with the same buffer, up to the offer's `max_attempts`, after
   transport errors, 408, 429 and 5xx. It honours `Retry-After` and gives up
   when the wait would outlast the assignment lease. Other responses are never
   retried.
4. Requires every declared receipt field. A receipt `sha1` must equal the
   SHA-1 of the sent bytes.

Each destination reports `sha1`, `request_count` (every provider HTTP request,
including failed and retried ones) and, on success, `provider_part_id`. These
outputs are commitments in the signed receipt. A `provider_throttled` failure
with a provider `Retry-After` also reports it in milliseconds as
`retry_after_ms`. Failures carry one class and never include a URL, header or
response body:

| Class | Cause |
| --- | --- |
| `provider_throttled` | 429 after the attempts or the lease ran out |
| `authorization_lost` | 401 or 403 |
| `provider_digest_rejected` | 400 `digest_mismatch` (part) or `bad_digest` (form) |
| `provider_connection_dropped` | the connection failed on the last attempt (Box closes it for a missing digest or a misaligned `Content-Range`) |
| `provider_receipt_mismatch` | the receipt `sha1` differs from the sent bytes |
| `provider_receipt_invalid` | the success response lacks the receipt or a declared field |
| `provider_error` | any other 4xx, 3xx, or 408/5xx after the attempts |
| `source_read_failed`, `source_length_mismatch` | the source range could not be read exactly |
| `delivery_cancelled` | the assignment was cancelled or its lease expired |

## Source Digest

`transfer.source_digest.v1` lets BeamCore learn the whole-file SHA-1 and
SHA-256 of a source that carries no trusted checksum, which a Box commit
requires. It is on by default and needs no other capability. A source digest
reads one object and writes nothing; the worker never receives a destination
or its credential.

For a source digest the worker:

1. Sends one `GET` of the whole object to the HTTPS source with the offer's
   headers, without `Range`, and with `Accept-Encoding: identity` unless the
   offer sets one, so the hashes cover the stored bytes. Redirects are never
   followed.
2. Streams the body through SHA-1 and SHA-256 in one pass. The object is never
   buffered: the workload reserves 4 MiB and one connection whatever its size.
   The assignment lease, not a client timeout, bounds the read.
3. Counts the bytes. A declared `Content-Length` or a byte count that differs
   from the offered `size` fails with `source_length_mismatch`.

A completed digest reports `sha1`, `sha256` (lowercase hex) and `bytes`. These
outputs are commitments in the signed receipt. Failures carry one class:

| Class | Cause |
| --- | --- |
| `source_length_mismatch` | the object is shorter or longer than `size` |
| `source_read_failed` | any status other than 200 (a redirect included), a transport error, or a dropped connection |
| `delivery_cancelled` | the assignment was cancelled or its lease expired |

To opt out of either capability, leave it out of `--capabilities`.

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
