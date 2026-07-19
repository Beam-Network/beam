---
id: architecture
title: Architecture
sidebar_label: Architecture
sidebar_position: 2
---

# Architecture

Beam is composed of four distinct layers: the client-facing **Core Server**, the network's **Orchestrators** and **Workers**, and the metagraph-level **Validators**. This page describes how they connect and communicate.

---

## Network Topology

```
Client
  ↓  transfer request
Core Server ─────────────────────→ Validator
  ↓  assign tasks                      ↓  set weights
Orchestrator ←── $TAO ←── Metagraph ←──┘
  ↓
Worker Gateway
  ↓  offer tasks
Worker ──────────────────────────→ Storage (S3 · R2 · GCS · HTTP)
```

---

## Component Roles

| Component          | Runs at             | Responsibility                                              |
| ------------------ | ------------------- | ----------------------------------------------------------- |
| **Core Server**    | Beam-operated       | API, task orchestration, transfer tracking, PRISM data      |
| **Orchestrator**   | Operator-run        | Worker pool management, task routing, task result reporting |
| **Worker Gateway** | Orchestrator-run    | WebSocket session hub for workers                           |
| **Worker**         | Operator-run        | Data movement, chunk execution, task result reporting       |
| **Validator**      | Bittensor validator | Reads BeamCore epoch summaries and sets metagraph weights   |

---

## Communication Paths

### Client → Core Server

Clients interact exclusively via the REST API. They submit transfer requests, poll status, and retrieve metadata. Authentication uses API keys.

```
POST /transfers/create
POST /transfers/distribute
GET  /transfers/:transfer_id/status
```

### Core Server → Orchestrators

The Core Server and each orchestrator communicate over an authenticated **NATS control session**. Task assignments, recovery offers, readiness, and task results travel on this channel in real time.

### Orchestrators → Workers

Orchestrators operate a **Worker Gateway** — a WebSocket server that workers connect to.

### Workers to Orchestrators

After completing or failing a chunk, workers send `task_result` through the orchestrator-owned worker gateway.

---

## Control Plane

BeamCore keeps active transfers moving by watching task-offer batches and authoritative task results. When work stalls or fails, BeamCore issues replacement offers to eligible ready orchestrators in the transfer pool. Participants keep their sessions healthy and relay results promptly.

---

## Data Plane

Data never passes through the Core Server. Chunks are transferred directly from the origin (or client) to the worker, which writes to the destination storage. This keeps the Core Server lightweight and prevents it from becoming a bandwidth bottleneck.
