# Hybrid room execution foundation

`room-storage-transfer/v2` assigns a source range and a complete destination
snapshot to one worker. `room.transfer.storage.v2` identifies the replacement
capability. Agent-only `room-transfer/v1` workloads retain MLS E2EE; every leg
of a storage publication uses transport protection with worker-visible
plaintext. No participant receives storage credentials or room keys.

The room handler combines signed provider range reads and writes with existing
agent sessions. It reads each source chunk once, retains that buffer while up
to eight destination operations execute concurrently, and retries destination
routes using the same payload. Buffer memory depends on the assigned chunk
size, not on recipient count. The Runtime remains responsible for disjoint
source allocation and missing-coverage recovery; this adds no scheduler.

Storage leases identify the resource and frozen source ETag/version. They
resolve transient HTTPS provider routes through a scoped adapter endpoint.
Provider URLs, headers, and tokens are excluded from result/checkpoint data.
Conditional range reads reject changed source identities before delivery.
Provider responses produce part identity and hash evidence; they do not become
synthetic agent receipts or prove multipart finalization.

The orchestrator checks worker, lease, range, member, schema, and protection
bindings, forwards storage evidence separately from signed agent receipts,
and retains successful destination coverage. Core must verify provider results
and multipart finalization before declaring the publication complete.

Hybrid agent sessions have a separate HTTPS listener. The existing agent-only
listener is unchanged. The hybrid listener generates in-memory certificates,
advertises their SHA-256 identity through the authenticated assignment, and
requires TLS 1.3. Certificate selection uses the assigned pin in SNI; rotation
keeps unexpired pins usable and rejects unknown or expired ones. Agents remain
outbound clients with no public ingress port.

## Capability readiness

Standard signed multipart fanout uses `transfer.multipart.fanout.v1`. A complete
logical source group is dispatched to one capable worker, with the original
task and offer identities retained for every destination. The worker reserves
memory for one source chunk, reads it once, and reuses it across bounded
destination batches and retries. Remote routes require HTTPS. Source conditions,
range length, and hashes are checked before uploading. Successful destination
results remain independently valid when another destination fails. This
composition retains standard multipart finalization and missing-delivery recovery.

Enable this capability only with a coordinator, transfer runtime, credential
adapter, and orchestrator that support the complete v2 contract. A worker
listener alone does not establish end-to-end capability readiness. Do not
advertise the capability before cancellation, route renewal, provider
finalization, and recovery pass integration verification. The previous direct
agent-to-provider execution is not a fallback.

Local tests cover source-once fanout, scoped routes, destination retries,
receipt collection, and certificate rotation. Deployment verification must
also reconcile source bytes, destination coverage, finalization evidence,
and the actual orchestrator/worker identities.

Source HTTP admission validates the signed receipt and readiness before reading any payload. Agent clients use `Expect: 100-continue`; early retry/backpressure responses close the connection without consuming a chunk. Accepted source buffers are reused across destination batches.

Workers persist per-chunk source read counts, payload bytes, and wire bytes in checkpoints and results. Completed destination coverage restored from a checkpoint skips the corresponding source read; HTTP measurements in the fanout test are compared with the emitted counters.

Standard source groups checkpoint each destination result as it completes. The orchestrator retains and forwards that evidence independently of the final group result and replays unacknowledged checkpoints after reconnect. A worker process restart returns the retained successes and fails the unfinished cells to Runtime; it does not reread a lost buffer under the old attempt. Runtime creates the new attempt for missing coverage only.

Runtime cancellation may fence a single v2 lane attempt. Orchestrators cancel only that matching workload; another lane or its replacement attempt continues. Whole-publication cancellation remains supported.

The worker hashes each chunk once for provider Content-MD5 and reuses the existing source SHA-256 receipt across destinations. Route resolution must return the exact signed Content-MD5 header; a missing or changed checksum binding fails closed. Destination retries reuse both the payload and its checksums.

Workers enable hybrid execution with `room.transfer.storage.v2` and `room.transfer`, `BEAM_ROOM_STORAGE_LISTEN_ADDR`, and an HTTPS `BEAM_ROOM_STORAGE_ADVERTISE_URL`. The TLS listener binds before worker registration; invalid configuration or a port conflict stops startup. The coordinator-bound certificate authenticates this endpoint to agents. Orchestrators advertise hybrid support only when a live worker with that capability can be placed. Storage routes and results must use the standard three attempt slots per logical chunk.

Agent receipt waiting does not occupy provider upload slots. One session watcher collects agent acknowledgements, including bursts represented by a single notification. Bucket writes retain their bounded concurrency even when agents are unavailable. Outbound agent responses reference the immutable source buffer without creating a per-recipient payload copy.

Standard `worker_transfer_cancel` messages are authenticated against the Runtime
control envelope and scoped to one transfer. The orchestrator cancels its matching
WCP workloads, retains their cancelled records, and rejects late offers. Other
transfers continue. Disconnected workers remain bounded by assignment expiry;
provider cleanup is verified by the transfer owner separately.

Orchestrator hybrid capability follows available, connected workers with both the
base room protocol and the hybrid capability. MLS-only workers and exhausted
capacity do not enable hybrid routing. Selection uses the existing worker catalog.
