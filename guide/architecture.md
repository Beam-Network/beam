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
  ↓  assign workload offers            ↓  set weights
Orchestrator ←── $TAO ←── Metagraph ←──┘
  ↓
BeamLink/WCP
  ↓  offer workloads
Worker ──────────────────────────→ Storage (S3 · R2 · GCS · HTTP) / Room transfer leases
```

---

## Component Roles

| Component          | Runs at             | Responsibility                                              |
| ------------------ | ------------------- | ----------------------------------------------------------- |
| **Core Server**    | Beam-operated       | API, task orchestration, transfer tracking, PRISM data      |
| **Orchestrator**   | Operator-run        | Worker pool management, task routing, task result reporting |
| **BeamLink/WCP**   | Orchestrator/worker | Authenticated worker session and workload transport         |
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

The Core Server and each orchestrator communicate over an authenticated **NATS control session**. Task assignments, Room offers, recovery offers, readiness, and task results travel on this channel in real time.

### Orchestrators → Workers

Orchestrators accept worker sessions over **BeamLink/WCP**.

### Workers to Orchestrators

After completing or failing a workload, workers send results through BeamLink/WCP.

---

## Control Plane

BeamCore keeps active transfers moving by watching task-offer batches and authoritative task results. When work stalls or fails, BeamCore issues replacement offers to eligible ready orchestrators in the transfer pool. Participants keep their sessions healthy and relay results promptly.

---

## Data Plane

Data never passes through the Core Server. Chunks are transferred directly from the origin or Room transfer lease to the worker, which writes to the destination storage or Room transfer lease. This keeps the Core Server lightweight and prevents it from becoming a bandwidth bottleneck.
