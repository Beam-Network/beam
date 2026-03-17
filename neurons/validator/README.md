# Beam Validator

Validators verify bandwidth proofs and set weights on the Bittensor chain.

## Prerequisites

- Python 3.10+
- Bittensor wallet with registered hotkey

## Installation

```bash
# From repository root
pip install -e ".[validator]"
```

## Quick Start

```bash
# Run validator on testnet (single command)
cd neurons/validator && \
BEAM_VALIDATOR_WALLET_NAME=your_coldkey \
BEAM_VALIDATOR_WALLET_HOTKEY=your_hotkey \
BEAM_VALIDATOR_SUBNET_CORE_URL=https://beamcore-dev.b1m.ai \
python main.py
```

## Configuration

| Variable                 | Description                  | Default   |
| ------------------------ | ---------------------------- | --------- |
| `BEAM_WALLET_NAME`       | Bittensor wallet name        | `default` |
| `BEAM_WALLET_HOTKEY`     | Bittensor hotkey name        | `default` |
| `BEAM_SUBTENSOR_NETWORK` | Network (`test` or `finney`) | `test`    |
| `BEAM_NETUID`            | Subnet UID                   | `304`     |
| `BEAM_SUBNET_CORE_URL`   | SubnetCore API endpoint      | -         |

## Running

### Direct Python Command

```bash
# From neurons/validator directory
cd neurons/validator

# Testnet
BEAM_VALIDATOR_WALLET_NAME=your_coldkey \
BEAM_VALIDATOR_WALLET_HOTKEY=your_hotkey \
BEAM_VALIDATOR_SUBNET_CORE_URL=https://beamcore-dev.b1m.ai \
python main.py

# Mainnet
BEAM_VALIDATOR_WALLET_NAME=your_coldkey \
BEAM_VALIDATOR_WALLET_HOTKEY=your_hotkey \
BEAM_VALIDATOR_SUBNET_CORE_URL=https://beamcore.b1m.ai \
BEAM_SUBTENSOR_NETWORK=finney \
BEAM_NETUID=105 \
python main.py
```

### Using .env File

```bash
# Copy and edit .env file
cp ../../.env.example .env
# Edit .env with your wallet and network settings

# Run
cd neurons/validator
python main.py
```

### Using Helper Script (if available)

```bash
./scripts/run_validator.sh [testnet|mainnet]
```

## Network Endpoints

| Network | SubnetCore URL              |
| ------- | --------------------------- |
| Testnet | https://beamcore-dev.b1m.ai |
| Mainnet | https://beamcore.b1m.ai     |

---

# Validator Scoring Guide

## Overview

Validators score orchestrators based on metrics fetched from BeamCore API.
All operational data flows through BeamCore - validators do NOT maintain independent state.

```
┌─────────────────────────────────────────────────────────────────────────┐
│                           BEAMCORE                                       │
│                    (Source of Truth)                                     │
│                                                                          │
│  Tables: orchestrators, registered_workers, bandwidth_tasks,             │
│          epoch_payments, work_summaries                                  │
│                                                                          │
│  API: GET /validators/orchestrators                                      │
│       GET /validators/work-summaries                                     │
│       GET /validators/epoch/{epoch}/payments                             │
│       GET /validators/weights/{epoch}                                    │
└─────────────────────────────────────────────────────────────────────────┘
                                    │
                                    │ HTTP API
                                    ▼
┌─────────────────────────────────────────────────────────────────────────┐
│                          VALIDATORS                                      │
│                                                                          │
│   1. Fetch metrics from BeamCore API                                    │
│   2. Compute SLA scores using standard formula                          │
│   3. (Optional) Apply local modifiers                                   │
│   4. Set weights on Bittensor chain                                     │
└─────────────────────────────────────────────────────────────────────────┘
```

## Metrics from BeamCore

### 1. Orchestrator Metrics

**API:** `GET /validators/orchestrators`

| Metric                            | Description                | Used For         |
| --------------------------------- | -------------------------- | ---------------- |
| `stake_tao`                       | TAO staked by orchestrator | Stake weight     |
| `latest_uptime_percent`           | % of time online (0-100)   | Uptime score     |
| `latest_bandwidth_mbps`           | Aggregate pool bandwidth   | Bandwidth score  |
| `latest_latency_p95_ms`           | 95th percentile latency    | Latency score    |
| `latest_acceptance_rate`          | % of tasks accepted (0-1)  | Acceptance score |
| `latest_success_rate`             | % of tasks completed (0-1) | Success score    |
| `latest_penalty_redirect_percent` | BeamCore-assigned penalty  | Penalty          |

### 2. Worker Pool Metrics

**API:** `GET /validators/orchestrators/{hotkey}/workers`

| Metric           | Description             |
| ---------------- | ----------------------- |
| `bandwidth_mbps` | Per-worker bandwidth    |
| `latency_ms`     | Per-worker latency      |
| `success_rate`   | Per-worker task success |
| `trust_score`    | Worker trust (0-1)      |
| `fraud_score`    | Fraud detection score   |

### 3. Work Summary Metrics

**API:** `GET /validators/work-summaries`

| Metric                  | Description                |
| ----------------------- | -------------------------- |
| `tasks_completed`       | Tasks completed this epoch |
| `tasks_failed`          | Tasks failed this epoch    |
| `total_bytes_relayed`   | Bytes transferred          |
| `merkle_verified_count` | Verified chunks            |

### 4. Epoch Payment Metrics

**API:** `GET /validators/epoch/{epoch}/payments`

| Metric              | Description                   |
| ------------------- | ----------------------------- |
| `total_distributed` | Payments to workers           |
| `worker_count`      | Workers paid                  |
| `merkle_root`       | Verifiable proof hash         |
| `verified`          | Validator verification status |

---

## Scoring Formula

### Component Weights (must sum to 1.0)

| Component    | Weight | Description                 |
| ------------ | ------ | --------------------------- |
| Bandwidth    | 0.30   | Throughput capability       |
| Success Rate | 0.25   | Task completion reliability |
| Uptime       | 0.20   | Availability                |
| Latency      | 0.15   | Speed (inverted)            |
| Acceptance   | 0.10   | Task acceptance rate        |

### Normalization

```python
# Bandwidth: 0 Mbps = 0.0, 10 Gbps = 1.0
bandwidth_score = min(bandwidth_mbps / 10_000, 1.0)

# Latency: 0 ms = 1.0, 500+ ms = 0.0 (inverted)
latency_score = max(0, 1.0 - (latency_ms / 500))

# Stake: sqrt scaling, capped at 10000 TAO
stake_weight = sqrt(min(stake_tao, 10000)) / sqrt(10000)
```

### Final Weight Calculation

```
SLA Score = (
    0.30 × bandwidth_score +
    0.25 × success_rate +
    0.20 × (uptime_percent / 100) +
    0.15 × latency_score +
    0.10 × acceptance_rate
)

Final Weight = SLA Score × Stake Weight × (1 - Penalty)

Normalized Weight = Final Weight / sum(all Final Weights)
```

---

## Scoring Options

### Option 1: Standard Scoring (Maximum Consensus)

Use BeamCore data with the standard formula. All validators using this will compute **identical weights**.

```python
from beam.scoring import (
    OrchestratorMetrics,
    compute_uid_weights,
)

# Fetch from BeamCore
response = await beamcore.get("/validators/orchestrators")
orchestrators = [OrchestratorMetrics(**o) for o in response]

# Compute weights (all validators get same result)
uid_weights = compute_uid_weights(orchestrators)

# Set on Bittensor
subtensor.set_weights(
    netuid=NETUID,
    uids=list(uid_weights.keys()),
    weights=list(uid_weights.values()),
)
```

### Option 2: Custom Scoring (Validator Autonomy)

Apply validator-specific modifiers for independent scoring.

```python
from beam.scoring import (
    OrchestratorMetrics,
    ValidatorModifiers,
    compute_all_weights_with_modifiers,
)

# Fetch from BeamCore
response = await beamcore.get("/validators/orchestrators")
orchestrators = [OrchestratorMetrics(**o) for o in response]

# Create custom modifiers
modifiers = ValidatorModifiers(
    # Custom weights (prioritize what matters to you)
    weight_bandwidth=0.40,  # Increased from 0.30
    weight_success=0.20,    # Decreased from 0.25

    # Penalties from your own observations
    local_penalties={
        "hotkey_abc": 0.3,  # 30% penalty - slow responses
        "hotkey_xyz": 0.5,  # 50% penalty - detected fraud
    },

    # Boosts for verified orchestrators
    local_boosts={
        "hotkey_trusted": 0.2,  # 20% boost
    },

    # Zero weight for bad actors
    blacklist={"malicious_hotkey"},

    # Skip penalties for trusted operators
    whitelist={"subnet_owned_hotkey"},
)

# Compute with modifiers
weights = compute_all_weights_with_modifiers(orchestrators, modifiers)
```

---

## ValidatorModifiers Reference

| Modifier            | Type             | Default | Description                |
| ------------------- | ---------------- | ------- | -------------------------- |
| `weight_bandwidth`  | float            | 0.30    | Custom bandwidth weight    |
| `weight_success`    | float            | 0.25    | Custom success rate weight |
| `weight_uptime`     | float            | 0.20    | Custom uptime weight       |
| `weight_latency`    | float            | 0.15    | Custom latency weight      |
| `weight_acceptance` | float            | 0.10    | Custom acceptance weight   |
| `local_penalties`   | Dict[str, float] | {}      | Hotkey → penalty (0-1)     |
| `local_boosts`      | Dict[str, float] | {}      | Hotkey → boost (0-0.5)     |
| `blacklist`         | Set[str]         | {}      | Hotkeys to zero out        |
| `whitelist`         | Set[str]         | {}      | Hotkeys to skip penalties  |

---

## Active Validation (Canary Transfers)

Validators can issue **canary transfers** through BeamCore to verify orchestrator/worker bandwidth claims.

### How Canary Transfers Work

```
┌───────────┐     POST /canary/transfer      ┌───────────┐
│ VALIDATOR │ ─────────────────────────────► │ BEAMCORE  │
│           │   {orchestrator_uid, size}     │           │
└───────────┘                                └─────┬─────┘
                                                   │
                                                   │ Creates tasks for
                                                   │ target orchestrator
                                                   ▼
                                            ┌─────────────┐
                                            │ORCHESTRATOR │
                                            │             │
                                            └─────┬───────┘
                                                  │
                                                  │ Assigns to workers
                                                  ▼
                                            ┌───────────┐
                                            │  WORKERS  │
                                            │           │
                                            │ Execute   │
                                            │ with null │
                                            │ dest      │
                                            └─────┬─────┘
                                                  │
                                                  │ Report results
                                                  ▼
┌───────────┐     GET /canary/transfer/{id}  ┌───────────┐
│ VALIDATOR │ ◄───────────────────────────── │ BEAMCORE  │
│           │   {bandwidth_mbps, status}     │           │
└───────────┘                                └───────────┘
```

### Key Points

1. **Challenges go TO BeamCore** (not directly to orchestrators)
2. **Target by orchestrator UID** - BeamCore routes to the right orchestrator
3. **Workers execute** - Orchestrator assigns to its worker pool
4. **Null destination** - Measures bandwidth without sending actual data
5. **Results via BeamCore** - Validator polls for completion

### Canary Transfer API

```python
# Create canary transfer
POST /validators/canary/transfer
{
    "orchestrator_uid": 5,        # Target orchestrator to test
    "chunk_size": 1048576,        # 1MB chunks
    "chunk_count": 3              # Number of test chunks
}

# Response
{
    "success": true,
    "transfer_id": "canary-abc123",
    "orchestrator_hotkey": "5xxx...",
    "task_ids": ["canary-abc123-chunk0", ...]
}

# Poll for results
GET /validators/canary/transfer/{transfer_id}

# Response
{
    "status": "completed",
    "total_tasks": 3,
    "completed_tasks": 3,
    "aggregate_bandwidth_mbps": 850.5,
    "results": [
        {"task_id": "...", "worker_id": "w1", "bandwidth_mbps": 900},
        {"task_id": "...", "worker_id": "w2", "bandwidth_mbps": 800},
        ...
    ]
}
```

### Example: Challenge and Penalize

```python
async def challenge_orchestrators(orchestrators, beamcore, modifiers):
    """Issue canary transfers and apply penalties based on results."""

    for orch in orchestrators:
        # Issue canary transfer via BeamCore
        result = await beamcore.post("/validators/canary/transfer", {
            "orchestrator_uid": orch.uid,
            "chunk_size": 1_048_576,  # 1MB
            "chunk_count": 3,
        })

        if not result.get("success"):
            continue

        transfer_id = result["transfer_id"]

        # Poll for completion (with timeout)
        for _ in range(30):  # 30 attempts, 2 sec each = 60 sec max
            await asyncio.sleep(2)
            status = await beamcore.get(f"/validators/canary/transfer/{transfer_id}")

            if status["status"] in ("completed", "failed"):
                break

        # Apply penalties based on results
        if status["status"] == "failed":
            # Complete failure - heavy penalty
            modifiers.local_penalties[orch.hotkey] = 0.5

        elif status["aggregate_bandwidth_mbps"] < orch.latest_bandwidth_mbps * 0.5:
            # Measured <50% of claimed - moderate penalty
            modifiers.local_penalties[orch.hotkey] = 0.3

        elif status["aggregate_bandwidth_mbps"] > orch.latest_bandwidth_mbps * 0.9:
            # Measured >90% of claimed - small boost
            modifiers.local_boosts[orch.hotkey] = 0.1

    return modifiers
```

### What Canary Transfers Test

| Aspect              | Verified? | How                                |
| ------------------- | --------- | ---------------------------------- |
| Orchestrator online | Yes       | Task pickup                        |
| Worker pool active  | Yes       | Tasks assigned                     |
| Actual bandwidth    | Yes       | Measured during transfer           |
| Worker availability | Yes       | Multiple chunks test pool          |
| Claimed vs actual   | Yes       | Compare to `latest_bandwidth_mbps` |

---

## Scoring Strategy Guide

| Strategy         | When to Use            | How                                           |
| ---------------- | ---------------------- | --------------------------------------------- |
| **Passive**      | Trust BeamCore data    | Use `compute_uid_weights()`                   |
| **Active**       | Verify claims yourself | Issue challenges, apply penalties             |
| **Conservative** | Prioritize reliability | Increase `weight_success`                     |
| **Performance**  | Prioritize speed       | Increase `weight_bandwidth`, `weight_latency` |
| **Defensive**    | Protect network        | Liberal use of `blacklist`                    |

---

## BeamCore API Endpoints

| Endpoint                                     | Method | Description               |
| -------------------------------------------- | ------ | ------------------------- |
| `/validators/orchestrators`                  | GET    | All orchestrator metrics  |
| `/validators/orchestrators/{hotkey}/workers` | GET    | Workers in pool           |
| `/validators/work-summaries`                 | GET    | Epoch work stats          |
| `/validators/epoch/{epoch}/payments`         | GET    | Payment proofs            |
| `/validators/weights/{epoch}`                | GET    | Recommended weights       |
| `/validators/canary/transfer`                | POST   | Issue bandwidth challenge |
| `/validators/scores`                         | POST   | Submit computed scores    |
| `/validators/payments/verify`                | POST   | Verify epoch payment      |

---

---

## On-Chain Payment Verification

Validators verify that orchestrators actually paid workers on-chain by checking transaction hashes against blockchain records.

### How It Works

```
┌─────────────────────────────────────────────────────────────────────────┐
│                    PAYMENT VERIFICATION FLOW                             │
│                                                                          │
│   1. Validator fetches payment records from BeamCore API                │
│   2. For each payment, extract tx_hash (format: extrinsic:block)        │
│   3. Query Bittensor chain to verify transfer details                   │
│   4. Check: recipient matches worker, amount matches payment            │
│   5. Apply 50% SLASH for any invalid/missing tx_hash                    │
└─────────────────────────────────────────────────────────────────────────┘
```

### tx_hash Format

Orchestrators store payment tx_hash in the format:

```
{extrinsic_hash}:{block_hash}
```

Example:

```
0xa989b4c3589bcba3574a21795ba7ffd9...:0x5c7c5471b7c70f61cb09bcc8800c7a41...
```

### Verification Checks

| Check              | What's Verified              | Failure Action |
| ------------------ | ---------------------------- | -------------- |
| tx_hash present    | Payment has on-chain proof   | 50% slash      |
| Transaction exists | Extrinsic found in block     | 50% slash      |
| Is transfer        | Call is `Balances.transfer*` | 50% slash      |
| Recipient match    | Worker received payment      | 50% slash      |
| Amount match       | Within 5% tolerance          | 50% slash      |

### Penalty Application

```python
# In validator scoring:
payment_multiplier = self.payment_penalty_multipliers.get(hotkey, 1.0)

# Normal orchestrator: payment_multiplier = 1.0 (full emissions)
# Slashed orchestrator: payment_multiplier = 0.5 (50% emissions)

final_score = sla_score × stake_weight × payment_multiplier
```

### Code Location

| File                          | Purpose                                    |
| ----------------------------- | ------------------------------------------ |
| `chain/tx_verifier.py`        | TxVerifier class for on-chain verification |
| `chain/__init__.py`           | Chain module exports                       |
| `core/validator.py:1180`      | TxVerifier initialization                  |
| `core/validator.py:1207-1235` | Payment verification logic                 |
| `core/validator.py:1284-1295` | 50% slash application                      |
| `core/validator.py:2822`      | Penalty applied to final score             |

---

## Code Location

| File                                              | Purpose                                  |
| ------------------------------------------------- | ---------------------------------------- |
| `beam/scoring/metrics.py`                         | Metric dataclasses and scoring functions |
| `beam/scoring/sla.py`                             | Legacy SLA scoring (may be deprecated)   |
| `neurons/validator/core/validator.py`             | Main validator loop                      |
| `neurons/validator/clients/subnet_core_client.py` | BeamCore API client                      |
| `neurons/validator/chain/tx_verifier.py`          | On-chain transaction verifier            |

---

## Optional: Local Database for Analytics

By default, validators are **stateless** and rely entirely on BeamCore API for all data.

However, if you want local persistence for analytics, caching, or custom tracking, you can set up your own database:

### Supported Databases

| Provider                       | Connection String                                        |
| ------------------------------ | -------------------------------------------------------- |
| **Neon** (Serverless Postgres) | `postgresql+asyncpg://user:pass@host/db?sslmode=require` |
| **Supabase**                   | `postgresql+asyncpg://user:pass@host:5432/postgres`      |
| **Local Postgres**             | `postgresql+asyncpg://localhost/validator`               |

### Configuration

Set the `DATABASE_URL` environment variable:

```bash
# Neon (recommended for serverless)
export DATABASE_URL=postgresql+asyncpg://user:password@localhost:5432/beam

# Local Postgres
export DATABASE_URL="postgresql+asyncpg://localhost/validator"
```

### Use Cases

| Use Case                 | Description                         |
| ------------------------ | ----------------------------------- |
| **Historical Analytics** | Track orchestrator scores over time |
| **Challenge Results**    | Persist canary transfer outcomes    |
| **Custom Penalties**     | Store local penalty decisions       |
| **Audit Trail**          | Log all weight-setting decisions    |

### Note

Local database is **optional** and does not affect consensus. All validators compute identical scores from BeamCore data regardless of local storage.
