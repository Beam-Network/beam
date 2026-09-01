---
id: rooms
title: Rooms
sidebar_label: Rooms
sidebar_position: 4
---

# Rooms

Rooms give applications a live place to coordinate work for a shared session. A room can carry messages, commands, streams, media, and room-scoped transfer work through participant worker pools.

Participants support Rooms by keeping an orchestrator ready, workers connected, a worker gateway reachable, and capability listings accurate.

---

## Participant Setup

Room work uses the same participant path as transfers:

```text
application -> Beam -> orchestrator -> worker gateway -> worker
```

1. Your orchestrator stays registered, connected, and ready.
2. Your worker gateway stays reachable by your workers.
3. Your workers advertise the room capabilities they support.
4. Your workers accept assignments, run workloads, and report progress.
5. Beam keeps room sessions moving through active assignments and recovery paths.

---

## Room Workloads

| Type | Participant intent |
| --- | --- |
| Room transfer | Move room-scoped data between members or destinations. |
| Datagram | Carry best-effort packet traffic. |
| Message | Deliver a finite message to selected members. |
| Command | Run a request and return acknowledgement or response data. |
| Stream | Carry ordered session traffic from the latest safe point. |
| Media | Carry real-time audio, video, or data sessions. |

---

## Capability Readiness

An orchestrator is ready for room work when:

- it is registered, connected, and ready
- its worker gateway is reachable by workers
- connected workers advertise the required room capability
- the worker pool has available capacity

Advertise a room capability after the worker can run that workload end to end. Accurate capabilities help Beam route work to healthy participant paths.

---

## Recovery

Room work supports live recovery across participant paths.

| Workload | Recovery behavior |
| --- | --- |
| Datagram | Beam records delivered packet progress and continues with fresh traffic. |
| Message or command | Beam can place the work on another eligible path. |
| Stream or media | Beam resumes from the latest safe sequence. |

Fast acknowledgement, steady worker sessions, and accurate progress reporting help recovery stay smooth.

---

## Media

Some room media can use worker-hosted WebRTC. A worker is ready for this capability when it has:

- a reachable media endpoint
- network capacity for the session
- support for the required protected media path

Media keys stay with the source and destination agents. Workers forward protected media traffic as part of the participant path.

---

## Related Pages

- [Architecture](./architecture)
- [How Transfers Work](./transfers)
- [Orchestrators](./orchestrators)
- [Workers](./workers)
