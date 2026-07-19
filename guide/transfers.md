---
id: transfers
title: How Transfers Work
sidebar_label: How Transfers Work
sidebar_position: 3
---

import ZoomableMermaid from '@site/src/components/ZoomableMermaid';

# How Transfers Work

A Beam transfer moves data from a source to a destination by splitting it into chunks and distributing those chunks across multiple workers. This page walks through the full lifecycle from request to completion.

---

## Transfer Lifecycle

<ZoomableMermaid chart={`sequenceDiagram
    participant Client
    participant CoreServer as Core Server
    participant Orchestrator
    participant Worker
    participant Storage

    Client->>CoreServer: POST /transfers/create
    CoreServer-->>Client: { transfer_id, status: "pending" }
    Client->>CoreServer: POST /transfers/distribute

    CoreServer->>Orchestrator: Assign transfer task
    Orchestrator->>Worker: Offer chunk task

    alt Worker completes assigned work
        Worker->>Storage: Upload chunk
        Worker-->>Orchestrator: task_result
    else Task times out
        CoreServer->>Orchestrator: Reassign chunk
    end

    Note over CoreServer: All chunks completed
    CoreServer-->>Client: status: "completed"
`} />

---

## Step-by-Step

### 1. Create a Transfer

The client submits a transfer request to the Core Server specifying one or more sources, one or more destinations, and the total byte size.

```http
POST /transfers/create
Content-Type: application/json
X-Api-Key: b1m_...

{
  "sources": [
    { "type": "s3", "bucket": "my-data", "key": "dataset.tar.gz" }
  ],
  "destinations": [
    { "type": "s3", "bucket": "beam-output", "key": "dataset.tar.gz" }
  ],
  "total_size": 1073741824,
  "name": "dataset-transfer"
}
```

The Core Server:
1. Validates the request and API key
2. Computes the chunking plan from the total size
3. Creates one **task** per chunk
4. Returns `transfer_id`, `transfer_key`, `total_chunks`, and the selected `chunk_size`

Call `POST /transfers/distribute` with the returned `transfer_id` to begin assignment:

```http
POST /transfers/distribute
Content-Type: application/json
X-Api-Key: b1m_...

{
  "transfer_id": "uuid"
}
```

### 2. Task Assignment

The Core Server selects an orchestrator from the **active pool** using PRISM scores as routing weights. Orchestrators in the `qualified` pool are preferred; `qualifying` orchestrators serve as overflow capacity.

Tasks are dispatched to the orchestrator's control plane connection.

### 3. Worker Execution

The orchestrator assigns each task to an available worker via the Worker Gateway WebSocket:

1. Download the source chunk
2. Write it to the destination backend
3. Compute a cryptographic hash of the transferred bytes
4. Report the completed chunk with `task_result`

### 4. Completion

Once all chunks are verified, the transfer status transitions to `completed`. The Core Server records:

- Total bytes transferred per orchestrator
- Task completion counts per worker

Failed or missing verified task evidence can reduce the orchestrator's reliability inputs and PRISM routing weight.

---

## Signed URL Multipart (`signed_url_v1`)

For client-supplied destination storage, Beam prepares task-scoped signed access for each chunk. A worker receives only the short-lived source and destination access needed for its assigned chunk, uploads the part, and reports the storage `ETag` in `task_result`.

BeamCore verifies the uploaded parts with object storage and completes the multipart object after all chunks are done. Workers do not receive bucket credentials or unrestricted access to the full source or destination object.

---

## Transfer Statuses

| Status | Meaning |
|---|---|
| `pending` | Created, awaiting task assignment |
| `in_progress` | One or more chunks being transferred |
| `completed` | All chunks verified |
| `failed` | Terminal error — the transfer or a chunk could not complete |
| `cancelled` | Cancelled by client or timeout |

Public Transfer Detail responses convert operational transfer and task failures into fixed, audience-safe messages. Database errors, provider responses, object paths, identifiers, and other diagnostic details are not returned to the browser, and the interface does not provide a raw-error expansion.

---

## Chunking

Files are split server-side using BeamCore chunking configuration. The current minimum is **40 MiB**. Chunk size grows for large transfers.

Chunk boundaries are deterministic given a file size. Each chunk becomes an independent task that can be executed by a different worker on a different orchestrator, enabling parallelism for large transfers.

---

## Fault Tolerance

When an active task-offer batch has a **5 second** gap between valid task results for current in-progress tasks, BeamCore reassigns the affected active chunk indices. See **[Recovery Timeouts](./transfer-overseer)** for the recovery windows.

Recovery follows the same orchestrator flow as initial delivery:

1. Eligible orchestrators receive `worker_task_offer_batch` with executable task offers.
2. Each orchestrator selects connected local workers and forwards individual `task_offer` messages.
3. Workers report success or failure with `task_result`; orchestrators relay each result immediately until BeamCore returns a terminal acknowledgement.

Qualifying transfers draw recovery candidates from the qualifying pool. Qualified transfers draw from the qualified pool.

Orchestrators should keep the NATS control connection healthy and route `worker_task_offer_batch` messages promptly during recovery. Repeated stalls on the same orchestrator reduce its PRISM routing weight until reliability improves.

---

