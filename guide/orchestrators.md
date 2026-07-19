---
id: orchestrators
title: Orchestrators
sidebar_label: Orchestrators
sidebar_position: 4
---

# Orchestrators

Orchestrators operate worker pools, connect to BeamCore over NATS, route executable task offers to workers, and report worker outcomes back to BeamCore. PRISM uses BeamCore-verified throughput and reliability to determine routing share.

## Role

An orchestrator is responsible for:

1. Maintaining an orchestrator-owned worker gateway and worker session pool.
2. Receiving `worker_task_offer_batch` messages from BeamCore over NATS.
3. Selecting a connected local worker for each offer.
4. Relaying worker results to BeamCore immediately.
5. Staying connected and ready so BeamCore can route work.

## Pools

| Pool       | Routing                     |
| ---------- | --------------------------- |
| Qualifying | Calibration transfers       |
| Qualified  | Production client transfers |

## Worker Gateway

Workers connect to the orchestrator-owned worker gateway at `/ws/<worker_id>?api_key=<worker-api-key>`. The worker derives this WebSocket URL from `WORKER_GATEWAY_URL`; the orchestrator advertises the externally reachable gateway origin with `ORCHESTRATOR_WORKER_GATEWAY_URL`, or derives it from its own HTTP address when no override is set. The gateway forwards each task offer to a selected worker and relays worker results back through the orchestrator.

```mermaid
sequenceDiagram
    participant BC as BeamCore
    participant NATS
    participant O as Orchestrator
    participant WG as Worker gateway
    participant W as Worker

    W->>WG: connect /ws/<worker_id>?api_key=...
    O->>NATS: register { url, gateway_url, ready }
    BC->>NATS: worker_task_offer_batch
    NATS->>O: worker_task_offer_batch
    O->>WG: task_offer
    WG->>W: task_offer
    W->>WG: task_result
    WG->>O: task_result
    O->>NATS: task_result
    NATS->>BC: task_result
```

## Batch Offer Message

BeamCore sends executable offers directly:

```json
{
	"type": "worker_task_offer_batch",
	"batch_id": "uuid",
	"offers": [
		{
			"task_id": "uuid",
			"offer_id": "uuid",
			"chunk_size": 8388608,
			"source_url": "https://source-presigned-url",
			"dest_url": "https://dest-presigned-url",
			"urls_expires_at": "2026-06-13T12:00:00.000Z",
			"etag_required": true,
			"source_headers": {},
			"dest_headers": {},
			"minimum_worker_version": "0.2.0"
		}
	]
}
```

Each offer is assigned work for one chunk. The orchestrator keeps worker assignment local and forwards every offer to a connected worker. Local validation or execution failures are reported as failed `task_result` messages.

## Task Results

Workers report task outcomes with canonical `task_result`:

```json
{
	"type": "task_result",
	"task_id": "uuid",
	"offer_id": "uuid",
	"worker_id": "worker-uuid",
	"success": true,
	"bytes_transferred": 8388608,
	"duration_ms": 1234,
	"etag": "\"abc123\"",
	"error": null
}
```

BeamCore derives verified bytes from trusted task metadata and computes bandwidth from offer send time to completion time.

## Setup

Set `CORE_SERVER_URL`, `ORCH_GATEWAY_URL`, wallet settings, and `READY=true` when the orchestrator should receive routed work. Set production `ORCH_GATEWAY_URL` to `tls://orch-gateway.b1m.ai:4222`. If workers connect through a public or reverse-proxied gateway origin, set `ORCHESTRATOR_WORKER_GATEWAY_URL` to that origin and set each worker's `WORKER_GATEWAY_URL` to the same gateway origin. Keep the NATS control connection and worker gateway sessions healthy so BeamCore can deliver batches.

## Dashboard

The dashboard shows orchestrator readiness, NATS control connection state, PRISM score, transfer batches, task results, and BeamCore-verified throughput. Each recent transfer summarizes winning results as `completed/assigned`, together with worker, failure, superseded-attempt, recovery, and batch-status context.

## History Reset

Orchestrators can wipe their entire transfer history, task records, PRISM scoring evidence, and penalty history in a single call. The result depends on the orchestrator's current pool:

| Current pool | After reset |
|---|---|
| Qualified | Demoted to qualifying pool; must re-accumulate confidence and evidence to graduate again |
| Qualifying | Stays in qualifying; reset to newly-registered state (age clock and scores zeroed) |

**What is deleted:**
- All transfers and their associated tasks, task results, task events, and task attempts
- Fraud penalties attributed to this orchestrator
- All PRISM evidence (hourly buckets, lifetime totals) and all PRISM metric snapshots (qualifying and qualified history)

**What is preserved:**
- Epoch weight history — past per-epoch weight and score records remain visible on the dashboard
- Identity fields (hotkey, UID, name, region)
- Worker registrations
- On-chain weight submissions

**Endpoint:**

```
DELETE /orchestrators/history
Authorization: Bearer <orchestrator-api-key>
Content-Type: application/json

{ "confirm": true }
```

The `confirm: true` body field is required to prevent accidental deletion. The call returns `409` if any transfers are currently active (`pending`, `planning`, or `in_progress`) — wait for active work to complete before wiping.

**Example response:**

```json
{
  "success": true,
  "orchestrator_id": "...",
  "previous_pool": "qualified",
  "new_pool": "qualifying",
  "demoted": true,
  "history_deleted_at": "2026-06-19T12:00:00.000Z",
  "deleted": {
    "transfers": 42,
    "tasks": 187
  }
}
```

This operation is **irreversible**. The `history_deleted_at` field on the orchestrator row is updated each time this endpoint is called.
