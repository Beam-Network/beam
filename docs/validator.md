# BEAM Validator Onboarding Guide

Complete guide for setting up and running a BEAM Network validator on Bittensor Subnet 304 (testnet) or Subnet 105 (mainnet).

## Table of Contents

1. [Overview](#overview)
2. [Hardware Requirements](#hardware-requirements)
3. [Software Stack](#software-stack)
4. [Prerequisites](#prerequisites)
5. [Installation](#installation)
6. [Configuration](#configuration)
7. [Running the Validator](#running-the-validator)
8. [Monitoring & Maintenance](#monitoring--maintenance)
9. [Troubleshooting](#troubleshooting)
10. [Rewards & Economics](#rewards--economics)

---

## Overview

### What Does a BEAM Validator Do?

BEAM validators are responsible for:

1. **Verifying Proof-of-Bandwidth (PoB)** - Read and verify proofs submitted to BeamCore
2. **Setting Weights** - Determine how emissions are distributed to orchestrators based on their performance
3. **SLA Scoring** - Calculate orchestrator scores based on uptime, bandwidth, latency, and success rates
4. **Cross-Verification** - Participate in peer verification to prevent score manipulation

> **Note:** Validators read all data from BeamCore API. They do not connect directly to workers or orchestrators.

### Architecture Overview

```
                                    BITTENSOR NETWORK
                                           │
                                    ┌──────┴──────┐
                                    │  Subtensor  │
                                    └──────┬──────┘
                                           │
    ┌──────────────────────────────────────┼──────────────────────────────────────┐
    │                                      ▼                                      │
    │                              ┌──────────────┐                               │
    │                              │  VALIDATOR   │                               │
    │                              │    (You)     │                               │
    │                              └───────┬──────┘                               │
    │                                      │                                      │
    │               ┌──────────────────────┼──────────────────────┐               │
    │               │                      │                      │               │
    │               ▼                      ▼                      ▼               │
    │      ┌────────────────┐    ┌────────────────┐    ┌────────────────┐         │
    │      │ 1. Fetch PoB   │    │ 2. Calculate   │    │ 3. Set Weights │         │
    │      │    Proofs      │    │   SLA Scores   │    │  on Bittensor  │         │
    │      └───────┬────────┘    └───────┬────────┘    └───────┬────────┘         │
    │              │                     │                     │                  │
    │              └─────────────────────┼─────────────────────┘                  │
    │                                    ▼                                        │
    │                            ┌──────────────┐      ┌──────────────┐           │
    │                            │   BEAMCORE   │      │  SUBTENSOR   │           │
    │                            │    (API)     │      │   (Chain)    │           │
    │                            └──────────────┘      └──────────────┘           │
    │                                                                             │
    │                              BEAM NETWORK                                   │
    └─────────────────────────────────────────────────────────────────────────────┘

    VALIDATION CYCLE (every ~20 minutes):
    ┌─────────────────────────────────────────────────────────────────────────────┐
    │                                                                             │
    │   ┌─────────┐    ┌─────────┐    ┌─────────┐    ┌─────────┐    ┌─────────┐  │
    │   │  Fetch  │───▶│ Verify  │───▶│  Score  │───▶│  Set    │───▶│ Receive │  │
    │   │  Proofs │    │  PoB    │    │  SLAs   │    │ Weights │    │ Rewards │  │
    │   └─────────┘    └─────────┘    └─────────┘    └─────────┘    └─────────┘  │
    │                                                                             │
    └─────────────────────────────────────────────────────────────────────────────┘
```

**Flow:**
1. Validator fetches Proof-of-Bandwidth proofs from BeamCore API
2. Verifies cryptographic signatures and timing data
3. Calculates SLA scores for each orchestrator (uptime, bandwidth, latency, success)
4. Sets weights on Bittensor blockchain based on scores
5. Receives validator emissions for participation

### Network Information

| Network | Subnet UID | Subtensor | BeamCore URL |
|---------|------------|-----------|--------------|
| **Testnet** | 304 | `test` | https://beamcore-dev.b1m.ai |
| **Mainnet** | 105 | `finney` | https://beamcore.b1m.ai |

---

## Hardware Requirements

### Minimum Requirements

| Component | Minimum | Recommended |
|-----------|---------|-------------|
| **CPU** | 4 cores | 8+ cores |
| **RAM** | 8 GB | 16 GB |
| **Storage** | 50 GB SSD | 100 GB NVMe SSD |
| **Network** | 100 Mbps | 1 Gbps+ |
| **Public IP** | Required | Static IP preferred |

### Why These Requirements?

- **CPU**: PoB verification involves cryptographic operations (signature verification, hash checks)
- **RAM**: Metagraph sync and state management require memory
- **Storage**: Checkpoints, logs, and verification data
- **Network**: API communication with BeamCore
- **Public IP**: Required for validator API endpoints

---

## Software Stack

### Required

| Software | Version | Purpose |
|----------|---------|---------|
| **Python** | 3.10+ | Runtime |
| **Bittensor** | 8.0+ | Blockchain interaction |
| **Fiber** | Latest | Weight setting & chain ops |
| **Git** | 2.0+ | Code deployment |

### Optional (Recommended)

| Software | Purpose |
|----------|---------|
| **Redis** | Caching (improves performance) |
| **Prometheus + Grafana** | Monitoring |
| **systemd** | Process management |

### Operating System

- **Ubuntu 22.04 LTS** (recommended)
- **Debian 12**
- **Ubuntu 24.04 LTS**

macOS and Windows are supported for development but not recommended for production.

---

## Prerequisites

### 1. Bittensor Wallet Setup

You need a Bittensor wallet with:
- A **coldkey** (keeps your TAO safe, stays offline)
- A **hotkey** (used for signing, can be on the validator server)

```bash
# Install bittensor CLI
pip install bittensor-cli

# Create a new wallet (if you don't have one)
btcli wallet new_coldkey --wallet.name validator
btcli wallet new_hotkey --wallet.name validator --wallet.hotkey default

# Or import existing wallet
btcli wallet regen_coldkey --wallet.name validator --mnemonic "your 12 words here"
```

### 2. Register on Subnet

**Testnet (recommended for first-time validators):**
```bash
# Check registration cost
btcli subnet register --netuid 304 --subtensor.network test --wallet.name validator

# Register (requires TAO for registration fee)
btcli subnet register --netuid 304 --subtensor.network test \
    --wallet.name validator --wallet.hotkey default
```

**Mainnet:**
```bash
btcli subnet register --netuid 105 --subtensor.network finney \
    --wallet.name validator --wallet.hotkey default
```

### 3. Verify Registration

```bash
# Check your UID
btcli wallet overview --wallet.name validator --subtensor.network test

# Should show your UID on subnet 304 (testnet) or 105 (mainnet)
```

### 4. Stake TAO (Required for Weight Setting)

Validators need stake to set weights. More stake = more influence on consensus.

```bash
# Add stake to your validator hotkey
btcli stake add --wallet.name validator --wallet.hotkey default \
    --subtensor.network test --amount 100
```

**Minimum recommended stake**: 100 TAO (testnet), 1000+ TAO (mainnet)

---

## Installation

### Option A: Direct Installation

```bash
# 1. Clone the repository
git clone https://github.com/Beam-Network/beam.git
cd beam

# 2. Create virtual environment
python3 -m venv .venv
source .venv/bin/activate

# 3. Install dependencies
pip install -e .

# 4. Navigate to validator
cd neurons/validator
```

### Option B: systemd Service (Production)

Create `/etc/systemd/system/beam-validator.service`:

```ini
[Unit]
Description=BEAM Validator
After=network.target

[Service]
Type=simple
User=beam
WorkingDirectory=/home/beam/beam/neurons/validator
Environment="PATH=/home/beam/beam/.venv/bin:/usr/local/bin:/usr/bin:/bin"
EnvironmentFile=/home/beam/beam/neurons/validator/.env
ExecStart=/home/beam/beam/.venv/bin/python main.py
Restart=always
RestartSec=10

# Security hardening
NoNewPrivileges=true
ProtectSystem=strict
ReadWritePaths=/home/beam /var/log/beam /tmp/beam_logs

[Install]
WantedBy=multi-user.target
```

Enable and start:
```bash
sudo systemctl daemon-reload
sudo systemctl enable beam-validator
sudo systemctl start beam-validator
```

---

## Configuration

### Environment Variables

Create `.env` in `neurons/validator/`:

```bash
# =============================================================================
# BITTENSOR WALLET (BEAM_VALIDATOR_ prefix)
# =============================================================================
BEAM_VALIDATOR_WALLET_NAME=validator
BEAM_VALIDATOR_WALLET_HOTKEY=default
# BEAM_VALIDATOR_WALLET_PATH=~/.bittensor/wallets

# =============================================================================
# SUBNET CONFIGURATION (BEAM_ prefix - shared across components)
# =============================================================================
# Testnet
BEAM_NETUID=304
BEAM_SUBTENSOR_NETWORK=test

# Mainnet (uncomment for production)
# BEAM_NETUID=105
# BEAM_SUBTENSOR_NETWORK=finney

# =============================================================================
# BEAMCORE API (BEAM_VALIDATOR_ prefix)
# =============================================================================
# Testnet
BEAM_VALIDATOR_SUBNET_CORE_URL=https://beamcore-dev.b1m.ai

# Mainnet (uncomment for production)
# BEAM_VALIDATOR_SUBNET_CORE_URL=https://beamcore.b1m.ai

# =============================================================================
# NETWORK (BEAM_VALIDATOR_ prefix)
# =============================================================================
# Your server's public IP (required for spot checks)
BEAM_VALIDATOR_EXTERNAL_IP=YOUR_PUBLIC_IP_HERE
BEAM_VALIDATOR_PORT=8093

# =============================================================================
# VALIDATION SETTINGS (BEAM_VALIDATOR_ prefix)
# =============================================================================
BEAM_VALIDATOR_TASK_INTERVAL_SECONDS=12
BEAM_VALIDATOR_TASKS_PER_EPOCH=10
BEAM_VALIDATOR_CHUNK_SIZE_BYTES=10485760

# PoB Verification
BEAM_VALIDATOR_MAX_CLOCK_DRIFT_US=5000000
BEAM_VALIDATOR_MIN_TRANSFER_TIME_US=50000

# =============================================================================
# WEIGHT SETTING (BEAM_VALIDATOR_ prefix)
# =============================================================================
BEAM_VALIDATOR_BLOCKS_BETWEEN_WEIGHTS=100
BEAM_VALIDATOR_WEIGHT_ALPHA=0.3
BEAM_VALIDATOR_MIN_CONNECTION_STAKE=10.0

# =============================================================================
# REDIS (Optional - BEAM_VALIDATOR_ prefix)
# =============================================================================
BEAM_VALIDATOR_REDIS_HOST=localhost
BEAM_VALIDATOR_REDIS_PORT=6379
BEAM_VALIDATOR_REDIS_DB=1
# BEAM_VALIDATOR_REDIS_PASSWORD=your_redis_password

# =============================================================================
# LOGGING (BEAM_VALIDATOR_ prefix)
# =============================================================================
BEAM_VALIDATOR_LOG_LEVEL=INFO
BEAM_VALIDATOR_DEBUG=false
```

### Configuration Reference

**Note:** Validator uses `BEAM_VALIDATOR_` prefix for most settings. Shared subnet settings use `BEAM_` prefix.

| Variable | Default | Description |
|----------|---------|-------------|
| `BEAM_VALIDATOR_WALLET_NAME` | `default` | Bittensor wallet name |
| `BEAM_VALIDATOR_WALLET_HOTKEY` | `default` | Hotkey name within wallet |
| `BEAM_NETUID` | `304` | Subnet UID (304=testnet, 105=mainnet) |
| `BEAM_SUBTENSOR_NETWORK` | `test` | Network (`test` or `finney`) |
| `BEAM_VALIDATOR_SUBNET_CORE_URL` | `http://localhost:8080` | BeamCore API endpoint |
| `BEAM_VALIDATOR_EXTERNAL_IP` | Auto-detect | Your public IP for spot checks |
| `BEAM_VALIDATOR_PORT` | `8093` | Validator API port |
| `BEAM_VALIDATOR_BLOCKS_BETWEEN_WEIGHTS` | `100` | Blocks between weight updates (~20 min) |
| `BEAM_VALIDATOR_LOG_LEVEL` | `INFO` | Logging verbosity |

### Firewall Configuration

Open required ports:

```bash
# Validator API (for spot check data reception)
sudo ufw allow 8093/tcp

# Optional: Prometheus metrics
sudo ufw allow 9090/tcp
```

---

## Running the Validator

### Start the Validator

```bash
cd neurons/validator
source ../.venv/bin/activate

# Run
python main.py
```

### Verify It's Working

1. **Check logs**:
```bash
tail -f /tmp/beam_logs/validator.log
```

2. **Check health endpoint**:
```bash
curl http://localhost:8093/health
```

Expected response:
```json
{
  "status": "healthy",
  "uid": 5,
  "hotkey": "5Gk...",
  "is_registered": true,
  "current_block": 12345678
}
```

3. **Check scores**:
```bash
curl http://localhost:8093/scores | jq
```

4. **Verify weight setting**:
```bash
# Check when last weights were set
curl http://localhost:8093/state | jq '.last_weight_block'
```

### Expected Startup Sequence

```
INFO | Initializing validator...
INFO | Wallet: validator/default
INFO | Network: test (netuid: 304)
INFO | Syncing metagraph...
INFO | Metagraph synced: 10 neurons
INFO | Validator UID: 5
INFO | Stake: 500.0 TAO
INFO | Starting validation loop...
INFO | BeamCore connection: https://beamcore-dev.b1m.ai
INFO | Ready to validate
```

---

## Monitoring & Maintenance

### Health Checks

The validator exposes several endpoints:

| Endpoint | Description |
|----------|-------------|
| `GET /health` | Basic health status |
| `GET /health/detailed` | Full component status |
| `GET /state` | Validator state and statistics |
| `GET /scores` | Current orchestrator scores |
| `GET /weights` | Weight setting history |

### Log Locations

| Log | Location |
|-----|----------|
| Validator logs | `/tmp/beam_logs/validator.log` |
| systemd logs | `journalctl -u beam-validator -f` |

### Key Metrics to Monitor

1. **Weight Setting Success**
   - Weights should be set every ~100 blocks (~20 minutes)
   - Check: `curl http://localhost:8093/state | jq '.weights_set_count'`

2. **Orchestrator Coverage**
   - Should see scores for all active orchestrators
   - Check: `curl http://localhost:8093/scores | jq 'keys | length'`

3. **PoB Verification Rate**
   - Proofs should be verified regularly
   - Check logs for verification stats

4. **Memory Usage**
   - Should stay under 4GB normally
   - Monitor: `htop` or Prometheus

### Checkpointing

The validator automatically saves state checkpoints:

```bash
# Default checkpoint directory
ls /var/lib/beam/checkpoints/

# Configure via environment
VALIDATOR_CHECKPOINT_DIR=/var/lib/beam/checkpoints
CHECKPOINT_INTERVAL_SECONDS=300
MAX_CHECKPOINTS_KEPT=10
```

### Updating the Validator

```bash
cd beam
git pull origin main

# Restart
sudo systemctl restart beam-validator

# Or if running directly
# Ctrl+C and restart
```

---

## Troubleshooting

### Common Issues

#### 1. "Not registered on subnet"

```
ERROR | Hotkey not registered on subnet 304
```

**Solution**: Register your hotkey on the subnet:
```bash
btcli subnet register --netuid 304 --subtensor.network test \
    --wallet.name validator --wallet.hotkey default
```

#### 2. "Insufficient stake for weight setting"

```
ERROR | Cannot set weights: insufficient stake
```

**Solution**: Add more stake:
```bash
btcli stake add --wallet.name validator --amount 100 --subtensor.network test
```

#### 3. "BeamCore connection failed"

```
ERROR | Failed to connect to BeamCore: Connection refused
```

**Solution**:
- Verify `BEAM_VALIDATOR_SUBNET_CORE_URL` is correct
- Check if BeamCore is reachable: `curl https://beamcore-dev.b1m.ai/health`

#### 4. "Metagraph sync timeout"

```
WARNING | Metagraph sync timed out after 30s
```

**Solution**:
- This is usually temporary network congestion
- Validator will retry automatically
- If persistent, check your subtensor network setting

#### 5. "Weight setting failed"

```
ERROR | Failed to set weights: ...
```

**Solutions**:
- Ensure you have enough stake
- Verify wallet permissions: hotkey must be accessible
- Check network connectivity to subtensor

### Debug Mode

For detailed logging:

```bash
BEAM_VALIDATOR_LOG_LEVEL=DEBUG BEAM_VALIDATOR_DEBUG=true python main.py
```

### Getting Help

1. **Check logs**: `/tmp/beam_logs/validator.log`
2. **Join Discord**: [BEAM Network Discord](https://discord.gg/beam-network)
3. **GitHub Issues**: Report bugs at the repository

---

## Rewards & Economics

### How Validators Earn

Validators earn TAO emissions from the Bittensor network for:

1. **Setting weights** - Participating in consensus
2. **Accurate scoring** - Correct orchestrator evaluations
3. **Cross-verification** - Peer verification participation

### Emission Distribution

```
Bittensor Emissions (per epoch)
        │
        ▼
    Subnet 304/105
        │
        ├─► Validators (18%)
        │       └─► Based on stake & activity
        │
        └─► Orchestrators (82%)
                └─► Based on validator weights
```

### Maximizing Returns

1. **Maintain high uptime** (99%+)
2. **Keep validator software updated**
3. **Stake more TAO** (increases weight influence)
4. **Fast, reliable network** (better spot check success)
5. **Monitor and respond quickly** to issues

### Penalties

Validators can have reduced rewards for:
- Extended downtime
- Incorrect weight setting
- Failed cross-verification consensus
- Manipulation attempts

**Note**: Stake is NEVER slashed - only rewards are reduced.

---

## Quick Start Checklist

- [ ] Server meets hardware requirements
- [ ] Python 3.10+ installed
- [ ] Bittensor wallet created
- [ ] Hotkey registered on subnet (304 or 105 for mainnet)
- [ ] TAO staked to hotkey (100+ recommended)
- [ ] Repository cloned and dependencies installed
- [ ] `.env` configured with correct values
- [ ] `BEAM_VALIDATOR_EXTERNAL_IP` set to your public IP
- [ ] Firewall port 8093 open
- [ ] Validator started and health check passes
- [ ] Weights being set (check `/state` endpoint)

---

## Support

- **Documentation**: https://github.com/Beam-Network/beam
- **Discord**: [BEAM Network Discord](https://discord.gg/beam-network)
- **GitHub Issues**: For bug reports and feature requests

---

*Last updated: March 2026*
