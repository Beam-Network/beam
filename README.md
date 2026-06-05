# BEAM

An open coordination layer for bandwidth and machine-to-machine data transfer.

## Why BEAM Exists

**The modern internet has developed large-scale markets for compute and storage, but not for bandwidth.**

Cloud platforms provide on-demand compute. Storage networks allow data to be stored and retrieved globally. However, the movement of data — bandwidth — remains controlled by centralized providers: cloud platforms, CDNs, and telecom operators.

As a result:

- Bandwidth pricing is opaque and expensive
- Unused global network capacity cannot participate in data delivery
- Routing decisions are controlled by centralized infrastructure

Despite the enormous scale of global networking infrastructure, there is no open coordination layer for bandwidth.

**BEAM is designed to address this gap.**

## The Missing Internet Layer

Compute has markets.
Storage has markets.
Bandwidth markets exist, but they are largely closed, centralized, and inaccessible to independent participants.

BEAM introduces an open coordination layer where bandwidth can be contributed, measured, and rewarded based on performance.

## What BEAM Does

BEAM aggregates distributed bandwidth and coordinates data transfers across a network of participants.

- **Nodes contribute bandwidth** and participate in executing transfers
- **Proof-of-Bandwidth** measures and validates bandwidth performance during transfers, allowing validators to assess delivery quality
- **Rewards are based on measurable performance**, not promises

## Transfer Patterns

BEAM supports flexible source-to-destination configurations:

| Pattern             | Description                                              |
| ------------------- | -------------------------------------------------------- |
| **Single → Single** | One source to one destination                            |
| **Single → Multi**  | One source replicated to multiple destinations (fan-out) |
| **Multi → Single**  | Multiple sources aggregated to one destination           |
| **Multi → Multi**   | Multiple sources to multiple destinations (mesh)         |

Each transfer is chunked and distributed across workers in parallel.

## Capabilities

- **Universal Storage Connectivity** — S3, GCS, Azure Blob, R2, IPFS, HTTP endpoints
- **Multi-Destination Fan-Out** — Replicate data to multiple destinations in a single transfer
- **Parallel Transfers** — Large files split into chunks and transferred in parallel
- **Webhook Callbacks** — Get notified when transfers complete
- **MCP Integration** — AI agents can invoke BEAM as a tool for data movement

## Built for the Machine Internet

Machine-to-machine communication is rapidly increasing:

- **AI-to-AI** — Autonomous agents exchanging data with AI services
- **Agentic workflows** — AI systems coordinating tasks and sharing outputs
- **AI training pipelines** — Datasets moving to compute, models distributing to inference
- **Data center synchronization** — Replication across clusters and regions
- **AI-to-IoT** — Sensor data flowing to ML systems

These workflows involve terabytes or petabytes moving across regions and independent systems. BEAM provides a decentralized coordination layer for this data movement.

## BeamCore (coordination stack)

This repository is **off-chain node software** (orchestrator, worker, validator) that connects to **BeamCore**.

### Services (typical ports)

```
Clients / Validators                Orchestrators
         │ HTTP                           │ WebSocket
  ┌──────▼────────┐                 ┌─────▼───────┐
  │  core-server  │  :8000          │orch-gateway │  :8002
  │ API + Control │◄─WS relay──────►│ Orch WS edge│
  └──────┬────────┘                 └─────────────┘
         │ HTTP (internal)
  ┌──────▼────────┐
  │worker-gateway │  :8001  Worker WebSocket edge
  └──────┬────────┘
         │ WebSocket
      Workers

   ops-scheduler   — background maintenance (no execution ownership)
```

| Piece | Role |
| ----- | ---- |
| **core-server** | Public HTTP API, control plane, transfer and task lifecycle (your `CORE_SERVER_URL` / `BEAM_VALIDATOR_*` URLs point here) |
| **orch-gateway** | Orchestrator WebSocket edge; upstream relay to core-server (`ORCH_GATEWAY_URL`) |
| **worker-gateway** | Worker WebSocket edge; task offers and sessions (`WORKER_GATEWAY_URL` — **worker-gateway origin**, not core-server) |
| **ops-scheduler** | PRISM refresh, payment verification, cleanup jobs |

**Invariants:** PostgreSQL holds authoritative execution state; control-plane loops advance task lifecycle; **object bytes do not flow through** BeamCore (workers read/write storage directly).

**Pipeline:** `transfers → transfer_assignments → tasks → task_results → proofs_of_bandwidth → payments`.

**PRISM:** orchestrators are tracked in **qualifying** vs **qualified** pools; production routing uses `prism_final_score` (see [Beam dashboard](https://data.b1m.ai/)).

### How this repo fits in

1. **Clients** create transfers via BeamCore HTTP API or SDK (core-server).
2. **Orchestrators** register over HTTP and keep a **persistent orch-gateway WebSocket** for assignments and control messages — not HTTP polling.
3. **Workers** register over HTTP, then receive chunk **offers** over **worker-gateway** WebSockets (Beam-hosted gateway and/or an orchestrator-owned gateway, depending on configuration).
4. **Workers** move data directly between source and destination connectors; they submit **payment / PoB evidence** to BeamCore HTTP.
5. **Validators** read scores and PRISM inputs from BeamCore HTTP and set weights on Bittensor.

> **Note:** `WORKER_GATEWAY_URL` / worker env must target the **worker-gateway** base URL. BeamCore HTTP (`CORE_SERVER_URL`) is orthogonal (registration, REST, evidence POSTs).

For operator-level connection maps and env naming, use the BeamCore operator documentation published for your target network.

## The Vision

BEAM's goal is to become the open bandwidth layer for the machine internet.

By coordinating distributed bandwidth and measuring performance through Proof-of-Bandwidth, BEAM enables data to move efficiently across a network that is:

- **Open to participation** — Contributors join via the Bittensor network
- **Performance-based** — Rewards tied to measured delivery metrics
- **Decentralized** — No single point of control

## Run a Node

This repository contains the code for running orchestrators, workers, and validators on the BEAM network.

- [Orchestrator Guide](docs/orchestrator.md) — Run a miner node that coordinates bandwidth work
- [Worker Guide](docs/worker.md) — Run a worker node that executes transfer tasks
- [Validator Guide](docs/validator.md) — Run a validator that verifies proofs and sets weights

### Network Information

| Network | Subnet UID | Subtensor | BeamCore URL |
|---------|------------|-----------|--------------|
| Mainnet | 105 | `finney` | https://beamcore.b1m.ai |

## Links

- BeamCore — API, control plane, gateways, PRISM pipeline
- [Bittensor](https://bittensor.com)

## License

MIT License — see [LICENSE](LICENSE)
