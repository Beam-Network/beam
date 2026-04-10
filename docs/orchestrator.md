# BEAM Orchestrator (Miner) Onboarding Guide

Complete guide for setting up and running a BEAM Network orchestrator on Bittensor Subnet 304 (testnet) or Subnet 105 (mainnet).

## Table of Contents

1. [Overview](#overview)
2. [Hardware Requirements](#hardware-requirements)
3. [Software Stack](#software-stack)
4. [Prerequisites](#prerequisites)
5. [Installation](#installation)
6. [Configuration](#configuration)
7. [Running the Orchestrator](#running-the-orchestrator)
8. [Worker Management](#worker-management)
9. [Monitoring and Maintenance](#monitoring-and-maintenance)
10. [Troubleshooting](#troubleshooting)
11. [Economics and Rewards](#economics-and-rewards)

---

## Overview

### What is an Orchestrator?

In the BEAM Network, orchestrators are the "miners" on Bittensor. Unlike traditional mining, orchestrators coordinate bandwidth work rather than computing hashes.

### Architecture Overview

```
                                    BITTENSOR NETWORK
                                           │
                                    ┌──────┴──────┐
                                    │  Emissions  │
                                    └──────┬──────┘
                                           │
    ┌──────────────────────────────────────┼──────────────────────────────────────┐
    │                                      ▼                                      │
    │                              ┌──────────────┐                               │
    │                              │  VALIDATORS  │                               │
    │                              │ (Set Weights)│                               │
    │                              └───────┬──────┘                               │
    │                                      │ read proofs                          │
    │                                      ▼                                      │
    │  ┌─────────┐   transfer    ┌─────────────────┐   tasks      ┌───────────┐  │
    │  │ CLIENT  │──────────────▶│    BEAMCORE     │─────────────▶│  WORKERS  │  │
    │  └─────────┘               │  (Coordinator)  │◀─────────────│           │  │
    │                            └────────┬────────┘   results    └─────┬─────┘  │
    │                                     │                             │        │
    │                      assignments    │                             │        │
    │                                     ▼                             │        │
    │                            ┌─────────────────┐                    │        │
    │                            │  ORCHESTRATOR   │                    │        │
    │                            │    (You)        │                    │        │
    │                            └────────┬────────┘             delivery        │
    │                                     │                             │        │
    │                              proofs │                             ▼        │
    │                                     └───────────────▶   ┌─────────────────┐│
    │                                                         │  DESTINATIONS   ││
    │                                                         │ (S3, GCS, etc.) ││
    │                                                         └─────────────────┘│
    │                              BEAM NETWORK                                   │
    └─────────────────────────────────────────────────────────────────────────────┘
```

**Flow:**
1. Client creates transfer via BeamCore API
2. BeamCore assigns chunks to orchestrators by stake-weighted allocation
3. Orchestrator polls BeamCore for chunk assignments
4. Orchestrator creates tasks and submits to BeamCore
5. BeamCore pushes tasks to workers via WebSocket (Buffer service)
6. Workers fetch data from source and deliver to destinations
7. Workers submit Proof-of-Bandwidth (PoB) results to BeamCore
8. Orchestrator submits aggregated proofs to BeamCore
9. Validators read proofs from BeamCore and set weights on Bittensor
10. Orchestrator receives emissions based on weights

Orchestrators are responsible for:

1. **Polling Assignments** - Fetch chunk assignments from BeamCore based on stake weight
2. **Task Creation** - Create tasks for assigned chunks and submit to BeamCore
3. **Proof Aggregation** - Collect and sign Proof-of-Bandwidth (PoB) from workers
4. **Worker Payments** - Pay workers for completed work using dTAO

> **Note:** Workers connect to BeamCore (via Buffer service), not directly to orchestrators. Orchestrators coordinate work through the BeamCore API.

### Network Information

| Network | Subnet UID | Subtensor | BeamCore URL |
|---------|------------|-----------|--------------|
| Testnet | 304 | `test` | https://beamcore-dev.b1m.ai |
| Mainnet | 105 | `finney` | https://beamcore.b1m.ai |

---

## Hardware Requirements

### Minimum Requirements

| Component | Minimum | Recommended |
|-----------|---------|-------------|
| CPU | 4 cores | 8+ cores |
| RAM | 8 GB | 16 GB |
| Storage | 100 GB SSD | 250 GB NVMe SSD |
| Network | 100 Mbps | 1 Gbps+ |
| Public IP | Required | Static IP preferred |

### Why These Requirements?

- **CPU**: Task scheduling, proof verification, and API handling
- **RAM**: Worker state management, proof aggregation, metagraph sync
- **Storage**: Proof storage, logs, database (if local)
- **Network**: Must handle API requests and BeamCore communication
- **Public IP**: BeamCore needs to reach your orchestrator for health checks

---

## Software Stack

### Required

| Software | Version | Purpose |
|----------|---------|---------|
| Python | 3.10+ | Runtime |
| Bittensor | 8.0+ | Blockchain interaction |
| Git | 2.0+ | Code deployment |

### Optional

| Software | Version | Purpose |
|----------|---------|---------|
| Redis | 7+ | Caching (recommended for production) |
| Prometheus + Grafana | - | Monitoring |
| systemd | - | Process management |
| nginx | - | Reverse proxy with SSL |

### Operating System

- Ubuntu 22.04 LTS (recommended)
- Debian 12
- Ubuntu 24.04 LTS

---

## Prerequisites

### 1. Bittensor Wallet Setup

You need a Bittensor wallet with:
- A coldkey (keeps your TAO safe)
- A hotkey (used for signing, stays on server)

```bash
# Install bittensor CLI
pip install bittensor

# Create wallet
btcli wallet new_coldkey --wallet.name orchestrator
btcli wallet new_hotkey --wallet.name orchestrator --wallet.hotkey default
```

### 2. Register on Subnet as Miner

Orchestrators register as "miners" on the Bittensor subnet:

**Testnet:**
```bash
btcli subnet register --netuid 304 --subtensor.network test \
    --wallet.name orchestrator --wallet.hotkey default
```

**Mainnet:**
```bash
btcli subnet register --netuid 105 --subtensor.network finney \
    --wallet.name orchestrator --wallet.hotkey default
```

### 3. Verify Registration and Get Your UID

```bash
btcli wallet overview --wallet.name orchestrator --subtensor.network test
```

You should see your UID (2-151 for public orchestrators).

### 4. Stake TAO

Orchestrators must stake TAO to participate in the subnet:

```bash
# Add stake (minimum 2 TAO required)
btcli stake add --wallet.name orchestrator --wallet.hotkey default \
    --subtensor.network test --amount 10
```

### 5. Prepare Hotkey for Server Use

For automated deployments, use an unencrypted hotkey:

```bash
btcli wallet regen-hotkey \
  --wallet.name orchestrator \
  --wallet.hotkey default \
  --no-use-password \
  --overwrite

# Secure file permissions
chmod 600 ~/.bittensor/wallets/orchestrator/hotkeys/default
```

---

## Installation

### Option A: Direct Installation

```bash
# 1. Clone repository
git clone https://github.com/Beam-Network/beam.git
cd beam

# 2. Create virtual environment
python3 -m venv .venv
source .venv/bin/activate

# 3. Install dependencies
pip install -e .

# 4. Navigate to orchestrator
cd neurons/orchestrator
```

### Option B: systemd Service (Production)

Create `/etc/systemd/system/beam-orchestrator.service`:

```ini
[Unit]
Description=BEAM Orchestrator
After=network.target redis.service

[Service]
Type=simple
User=beam
WorkingDirectory=/home/beam/beam/neurons/orchestrator
Environment="PATH=/home/beam/beam/.venv/bin:/usr/local/bin:/usr/bin:/bin"
EnvironmentFile=/home/beam/beam/neurons/orchestrator/.env
ExecStart=/home/beam/beam/.venv/bin/python main.py
Restart=always
RestartSec=10

# Security
NoNewPrivileges=true
ProtectSystem=strict
ReadWritePaths=/home/beam /var/log/beam /tmp/beam_logs

[Install]
WantedBy=multi-user.target
```

Enable and start:
```bash
sudo systemctl daemon-reload
sudo systemctl enable beam-orchestrator
sudo systemctl start beam-orchestrator
```

---

## Configuration

### Environment Variables

Create `.env` in `neurons/orchestrator/`:

```bash
# =============================================================================
# API SERVER
# =============================================================================
ORCHESTRATOR_HOST=0.0.0.0
API_PORT=8000
LOG_LEVEL=INFO

# =============================================================================
# BITTENSOR WALLET (Required)
# =============================================================================
WALLET_NAME=orchestrator
WALLET_HOTKEY=default

# =============================================================================
# SUBNET CONFIGURATION
# =============================================================================
# Testnet
NETUID=304
SUBTENSOR_NETWORK=test

# Mainnet (uncomment for production)
# NETUID=105
# SUBTENSOR_NETWORK=finney

# =============================================================================
# BEAMCORE API (Required)
# =============================================================================
# Testnet
SUBNET_CORE_URL=https://beamcore-dev.b1m.ai

# Mainnet (uncomment for production)
# SUBNET_CORE_URL=https://beamcore.b1m.ai

# =============================================================================
# WORKER MANAGEMENT
# =============================================================================
MAX_WORKERS=10000
WORKER_TIMEOUT=300
MIN_WORKER_BANDWIDTH=10.0

# =============================================================================
# TASK SETTINGS
# =============================================================================
CHUNK_SIZE=1048576
MAX_CONCURRENT_TASKS=1000
TASK_TIMEOUT=120

# =============================================================================
# AUTHENTICATION
# =============================================================================
# Subnet auth (validators/workers)
SUBNET_AUTH_ENABLED=true
SUBNET_AUTH_REQUIRE_METAGRAPH=true

# Client auth
CLIENT_AUTH_ENABLED=true
CLIENT_STAKE_GATED_ENABLED=true

# =============================================================================
# REDIS (Required for production)
# =============================================================================
REDIS_URL=redis://localhost:6379

# =============================================================================
# METAGRAPH
# =============================================================================
METAGRAPH_SYNC_INTERVAL=300

# =============================================================================
# OPERATOR FEE (Your profit margin)
# =============================================================================
# Percentage of emissions you keep (0-100, default 0)
# Rest goes to workers
FEE_PERCENTAGE=0
```

### Configuration Reference

| Variable | Default | Description |
|----------|---------|-------------|
| `API_PORT` | `8000` | API port |
| `WALLET_NAME` | `orchestrator` | Bittensor wallet name |
| `NETUID` | `304` | Subnet UID |
| `SUBNET_CORE_URL` | Required | BeamCore API endpoint |
| `MAX_WORKERS` | `10000` | Max workers to accept |
| `WORKER_TIMEOUT` | `300` | Worker heartbeat timeout (seconds) |
| `FEE_PERCENTAGE` | `0` | Your fee (0-100%) |
| `METAGRAPH_SYNC_INTERVAL` | `300` | Metagraph sync interval (seconds) |

### Firewall Configuration

```bash
# Orchestrator API
sudo ufw allow 8000/tcp

# Redis (if external)
sudo ufw allow 6379/tcp

# Optional: Prometheus metrics
sudo ufw allow 9090/tcp
```

---

## Running the Orchestrator

### Start the Orchestrator

```bash
cd neurons/orchestrator
source ../.venv/bin/activate

python main.py
```

### Verify It's Running

1. **Health check:**
```bash
curl http://localhost:8000/health
```

Expected:
```json
{
  "status": "healthy",
  "uid": 5,
  "hotkey": "5Gk...",
  "workers": 0,
  "active_tasks": 0
}
```

2. **Check state:**
```bash
curl http://localhost:8000/state | jq
```

3. **View logs:**
```bash
tail -f /tmp/beam_logs/orchestrator.log
```

### Expected Startup Sequence

```
INFO | Starting BEAM Orchestrator...
INFO | Wallet: orchestrator/default
INFO | Network: test (netuid: 304)
INFO | Syncing metagraph...
INFO | Metagraph synced: 10 neurons
INFO | Orchestrator UID: 5
INFO | Stake: 500.0 TAO
INFO | BeamCore connection: https://beamcore-dev.b1m.ai
INFO | Starting API server on 0.0.0.0:8000
INFO | Ready to accept workers
```

---

## Worker Management

### How Workers Connect

Workers connect to BeamCore (via Buffer service), not directly to orchestrators:

```
Worker → WebSocket → Buffer Service → BeamCore → Task from Orchestrator
Worker completes → Buffer Service → BeamCore → PoB recorded
```

### Attracting Workers

Workers affiliate with orchestrators based on:

1. **Operator Fee** - Lower fee = more worker earnings
2. **Uptime** - Reliable orchestrators get more tasks assigned
3. **Task Volume** - Higher stake = more chunk assignments from BeamCore
4. **Geographic Coverage** - Workers in diverse regions improve performance

### Setting Your Operator Fee

```bash
# Update fee via API
curl -X PATCH https://beamcore-dev.b1m.ai/orchestrators/{your_hotkey}/fee \
  -H "Content-Type: application/json" \
  -H "X-Orchestrator-Hotkey: {your_hotkey}" \
  -H "X-Orchestrator-Signature: {signature}" \
  -d '{"fee_percentage": 10.0}'
```

**Fee strategies:**

| Fee | Strategy | Use Case |
|-----|----------|----------|
| 0% | Maximum worker attraction | Building initial pool |
| 5-10% | Balanced | Standard operation |
| 15-20% | Higher profit | Established with loyal workers |

### Monitoring Workers

```bash
# Worker stats
curl http://localhost:8000/workers/stats | jq

# Full orchestrator state
curl http://localhost:8000/state | jq
```

---

## Monitoring and Maintenance

### Health Endpoints

| Endpoint | Description |
|----------|-------------|
| `GET /health` | Basic health status |
| `GET /state` | Full orchestrator state |
| `GET /metrics` | Prometheus metrics |
| `GET /metrics/json` | JSON metrics |
| `GET /workers/stats` | Worker statistics |

### Key Metrics to Monitor

1. **Worker Count** - Should grow over time
2. **Task Success Rate** - Target 95%+
3. **Payment Success** - All workers should be paid
4. **Wallet Balance** - Keep funded for worker payments

### Log Locations

| Log | Location |
|-----|----------|
| Orchestrator | `/tmp/beam_logs/orchestrator.log` |
| systemd | `journalctl -u beam-orchestrator -f` |

### Wallet Balance Management

Your orchestrator pays workers from its wallet. Keep it funded:

```bash
# Check balance
btcli wallet balance --wallet.name orchestrator

# Transfer TAO to orchestrator hotkey
btcli wallet transfer --dest {orchestrator_hotkey} --amount 10
```

**Payment retry system:**
- Failed payments are queued and retried every 60 seconds
- Up to 5 retry attempts per payment
- After 5 failures, payment is dropped (validator penalty applies)

### Updating the Orchestrator

```bash
cd beam
git pull origin main

# Restart
sudo systemctl restart beam-orchestrator
```

---

## Troubleshooting

### Common Issues

#### 1. "Hotkey not registered"

```
ERROR | Hotkey not registered on subnet 304
```

**Solution:** Register as a miner:
```bash
btcli subnet register --netuid 304 --subtensor.network test \
    --wallet.name orchestrator --wallet.hotkey default
```

#### 2. "Enter your password" at startup

The hotkey is encrypted.

**Solution:** Regenerate without encryption:
```bash
btcli wallet regen-hotkey --wallet.name orchestrator \
    --wallet.hotkey default --no-use-password --overwrite
```

#### 3. "BeamCore connection failed"

```
ERROR | Failed to connect to BeamCore
```

**Solutions:**
- Verify `SUBNET_CORE_URL` is correct
- Check network connectivity: `curl https://beamcore-dev.b1m.ai/health`

#### 4. No tasks being assigned

**Solutions:**
- Verify BeamCore connection is working
- Ensure orchestrator is registered and staked
- Check logs for assignment polling errors

#### 5. Worker payments failing

```
WARNING | Payment failed: insufficient balance
```

**Solutions:**
- Add TAO to orchestrator wallet
- Check payment retry queue
- Verify wallet hotkey is accessible

### Debug Mode

```bash
LOG_LEVEL=DEBUG python main.py
```

---

## Economics and Rewards

### How Orchestrators Earn

```
Bittensor Emissions
        │
        ▼
Validator sets weights based on SLA score
        │
        ▼
Orchestrator receives emissions
        │
        ├─► Operator Fee (your %) → Your wallet
        │
        └─► Worker Pool (remainder) → Workers
```

### SLA Scoring

Validators score orchestrators on:

| Metric | Weight | Description |
|--------|--------|-------------|
| Uptime | 20% | 24h rolling availability |
| Bandwidth | 35% | Throughput of your workers |
| Latency | 25% | Response time |
| Success Rate | 15% | Task completion rate |
| Worker Payments | 5% | Paying workers on time |

**SLA score formula:**
```
SLA = uptime × bandwidth × latency × success × payment_compliance
```

Scores are multiplicative - poor performance in any area significantly impacts total score.

### Maximizing Rewards

1. **Maintain high uptime** (99%+)
2. **Attract quality workers** with competitive fees
3. **Keep wallet funded** for worker payments
4. **Use reliable infrastructure** (low latency, good bandwidth)
5. **Monitor and fix issues quickly**
6. **Stake more TAO** for higher traffic allocation

### Penalty Redirects

When your SLA drops, a portion of your rewards redirects to Orchestrator #1:

```
penalty_percent = (1 - SLA_multiplier) × 100

Example: SLA = 0.70 → 30% of rewards redirected
```

### Subnet-Owned Orchestrator (#1)

| Feature | Description |
|---------|-------------|
| 0% operator fee | Workers get 100% |
| Always available | Fallback for all workers |
| Receives penalties | Gets redirected rewards |
| No stake required | Subnet-maintained |

Workers default to #1 until they choose to affiliate with another orchestrator.

---

## Quick Start Checklist

- [ ] Server meets hardware requirements
- [ ] Python 3.10+, Redis installed
- [ ] Bittensor wallet created
- [ ] Registered as miner on subnet (304 or 105)
- [ ] TAO staked (minimum 2 TAO required)
- [ ] Hotkey unencrypted for server use
- [ ] Repository cloned and dependencies installed
- [ ] `.env` configured correctly
- [ ] Orchestrator started and health check passes
- [ ] BeamCore connection working (check logs)
- [ ] Wallet funded for worker payments

---

## Support

- **Documentation**: https://github.com/Beam-Network/beam
- **Discord**: [BEAM Network Discord](https://discord.gg/beam-network)
- **GitHub Issues**: For bug reports and feature requests

---

*Last updated: March 2026*
