# Beam Orchestrator

Orchestrators coordinate data transfers and manage worker pools on the Beam subnet.

## Prerequisites

- Python 3.10+
- Bittensor wallet with registered hotkey on the subnet
- TAO stake (see Staking Requirements below)

## Installation

```bash
# From repository root
pip install -e ".[orchestrator]"
```

## Quick Start

```bash
# Run orchestrator on testnet (single command)
cd neurons/orchestrator && \
WALLET_NAME=your_coldkey \
WALLET_HOTKEY=your_hotkey \
SUBTENSOR_NETWORK=test \
NETUID=304 \
SUBNET_CORE_URL=https://beamcore-dev.b1m.ai \
REGISTRY_URL=https://beamcore-dev.b1m.ai \
python main.py
```

## Staking Requirements

Orchestrators must stake TAO to participate in the subnet (minimum 2 TAO required).

**Note:** Slot assignment is handled automatically during registration. No separate slot claiming step is needed.

## Configuration

| Variable | Description | Default |
|----------|-------------|---------|
| `WALLET_NAME` | Bittensor coldkey wallet name | `default` |
| `WALLET_HOTKEY` | Bittensor hotkey name | `default` |
| `SUBTENSOR_NETWORK` | Network (`test` or `finney`) | `test` |
| `NETUID` | Subnet UID | `304` |
| `SUBNET_CORE_URL` | BeamCore API endpoint | - |
| `LOCAL_MODE` | Skip chain verification (dev only) | `false` |

## Running

### Direct Python Command

```bash
# From neurons/orchestrator directory
cd neurons/orchestrator

# Testnet
WALLET_NAME=your_coldkey \
WALLET_HOTKEY=your_hotkey \
SUBTENSOR_NETWORK=test \
NETUID=304 \
SUBNET_CORE_URL=https://beamcore-dev.b1m.ai \
REGISTRY_URL=https://beamcore-dev.b1m.ai \
python main.py

# Mainnet
WALLET_NAME=your_coldkey \
WALLET_HOTKEY=your_hotkey \
SUBTENSOR_NETWORK=finney \
NETUID=105 \
SUBNET_CORE_URL=https://beamcore.b1m.ai \
REGISTRY_URL=https://beamcore.b1m.ai \
python main.py
```

### Using .env File

```bash
# Copy and edit .env file
cp ../../.env.example .env
# Edit .env with your wallet and network settings

# Run
cd neurons/orchestrator
python main.py
```

### Using Helper Script (if available)

```bash
./scripts/run_orchestrator.sh [local|testnet|mainnet] [port]
```

## Network Endpoints

| Network | SubnetCore URL |
|---------|----------------|
| Local | http://localhost:8080 |
| Testnet | https://beamcore-dev.b1m.ai |
| Mainnet | https://beamcore.b1m.ai |

## How It Works

1. **Bittensor Registration**: Register as a miner on the subnet (`btcli subnet register`)
2. **Stake TAO**: Stake at least 2 TAO on the subnet
3. **Start Orchestrator**: Run the orchestrator software (auto-registers with BeamCore and claims a slot)
4. **Worker Management**: Workers connect and join the orchestrator's pool
5. **Task Assignment**: BeamCore assigns transfer chunks based on orchestrator weight
6. **Proof Aggregation**: Orchestrator collects worker delivery proofs
7. **On-Chain Payment**: Workers are paid TAO directly on Bittensor chain
8. **Epoch Submission**: Proofs with tx_hash are submitted to validators each epoch

---

## On-Chain Worker Payments

Orchestrators pay workers directly on the Bittensor chain. Validators verify these payments.

### Payment Flow

```
Worker completes task
         │
         ▼
Orchestrator calculates reward
         │
         ▼
Bittensor transfer (on-chain)
         │
         ▼
Extract extrinsic_hash + block_hash
         │
         ▼
Store tx_hash in BeamCore
         │
         ▼
Validators verify on-chain
```

### tx_hash Format

The orchestrator stores payment proof as a combined hash:

```
{extrinsic_hash}:{block_hash}
```

Example:
```
0xa989b4c3589bcba3574a21795ba7ffd9b64eb186...:0x5c7c5471b7c70f61cb09bcc8800c7a41ec9f6c76...
```

This format allows validators to query the exact transaction on-chain.

### Validator Verification

Validators check each payment tx_hash against the blockchain:

| Check | Description |
|-------|-------------|
| Transaction exists | Extrinsic found in the specified block |
| Is transfer | Call is `Balances.transfer` or `transfer_keep_alive` |
| Recipient match | Worker received the payment |
| Amount match | Amount within 5% of expected |

### Slashing

**50% emission slash** if:
- Missing tx_hash (no on-chain proof)
- Invalid/fake tx_hash (transaction not found)
- Wrong recipient (payment to wrong address)
- Wrong amount (>5% deviation)

### Requirements

1. **Wallet Required**: Orchestrator must have a funded coldkey wallet
2. **Sufficient Balance**: Must have TAO to pay workers
3. **Chain Connection**: Must connect to subtensor for transfers

### Configuration

```bash
# Required for on-chain payments
export WALLET_NAME="your_coldkey"
export WALLET_HOTKEY="your_hotkey"
export SUBTENSOR_NETWORK="test"  # or "finney" for mainnet
```

### Code Location

| File | Purpose |
|------|---------|
| `src/core/reward_manager.py:265-277` | Extract extrinsic_hash + block_hash |
| `src/core/reward_manager.py:331` | Store tx_hash in payment record |
| `src/clients/subnet_core_client.py` | Send payment to BeamCore |

---

## Monitoring

Check your orchestrator status on the BeamCore dashboard:
- Testnet: https://beamcore-dev.b1m.ai/dashboard
- Mainnet: https://beamcore.b1m.ai/dashboard

See [Orchestrator Guide](../../docs/orchestrator.md) for detailed documentation.
