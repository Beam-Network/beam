# Room Storage Transfers

Room publications involving agents and object storage require
`room.transfer.storage.v2` and `room.transfer`.

These transfers use TLS with plaintext visible to the executing worker.
They do not provide end-to-end encryption. Workers do not receive storage
credentials or room keys. [Agent-only room transfers](room-workload-foundation.md)
remain end-to-end encrypted.

## Worker configuration

Complete the [worker setup](worker.md) and configure a reachable HTTPS endpoint:

```bash
export BEAM_ROOM_STORAGE_LISTEN_ADDR=0.0.0.0:9443
export BEAM_ROOM_STORAGE_ADVERTISE_URL=https://worker.example.com:9443

./bin/beam-worker serve \
  --node-key data/worker/node.key \
  --capabilities transfer.multipart,room.transfer,room.transfer.storage.v2
```

The worker manages this listener's TLS certificate. Allow TLS 1.3 connections
to the advertised endpoint; preserve the worker's TLS connection through any
network forwarding. Invalid listener configuration or a port conflict prevents
startup.

The orchestrator requires the [room coordinator configuration](orchestrator.md#3-configure)
and connected workers with available capacity. Advertise support only when
using compatible participant software and a Beam environment supporting this capability.

## Transfer failures

Workers reject changed or truncated source data. Check source availability and
provider errors when delivery fails; do not disable integrity checks.
