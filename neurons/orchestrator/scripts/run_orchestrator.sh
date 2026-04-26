#!/bin/bash
# BEAM Orchestrator Startup Script
# Usage: ./scripts/run_orchestrator.sh [local|testnet|mainnet]

set -e

# Default mode and port
MODE=${1:-testnet}
PORT=${2:-8001}

# Get script directory, orchestrator root, and repository root
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ORCHESTRATOR_ROOT="$(dirname "$SCRIPT_DIR")"
REPO_ROOT="$(cd "$ORCHESTRATOR_ROOT/../.." && pwd)"

# Create logs directory
LOGS_DIR="$ORCHESTRATOR_ROOT/logs"
mkdir -p "$LOGS_DIR"
LOG_FILE="$LOGS_DIR/orchestrator_${PORT}.log"

# Kill any process running on the port
echo "Checking for processes on port $PORT..."
PIDS=$(lsof -ti:$PORT 2>/dev/null || true)
if [ -n "$PIDS" ]; then
    echo "Killing processes on port $PORT: $PIDS"
    echo "$PIDS" | xargs kill -9 2>/dev/null || true
    sleep 2
fi

# Change to orchestrator directory
cd "$ORCHESTRATOR_ROOT"

# Activate virtual environment
source "$REPO_ROOT/.venv/bin/activate"

# Ensure repo packages are importable when running from the orchestrator di
export PYTHONPATH="$REPO_ROOT${PYTHONPATH:+:$PYTHONPATH}"

# Enable verbose logging
export LOG_LEVEL=${LOG_LEVEL:-DEBUG}

# =============================================================================
# Wallet Configuration (edit these or set as environment variables)
# =============================================================================
export WALLET_NAME=${WALLET_NAME:-terapipe_orchestrator1_cold}
export WALLET_HOTKEY=${WALLET_HOTKEY:-terapipe_orchestrator1_hot}
export WALLET_PATH=${WALLET_PATH:-~/.bittensor/wallets}
export SUBNET_CORE_URL=${SUBNET_CORE_URL:-http://157.245.126.211:8000}
export REGISTRY_URL=${REGISTRY_URL:-http://157.245.126.211:8000}

# =============================================================================
# Run based on mode
# =============================================================================
case $MODE in
  local)
    echo "Starting Orchestrator in LOCAL mode..."
    echo "Logging to: $LOG_FILE"
    LOCAL_MODE=true \
    CLIENT_AUTH_ENABLED=false \
    SUBNET_AUTH_ENABLED=false \
    API_PORT=$PORT \
    SUBNET_CORE_URL="http://127.0.0.1:8080" \
    python main.py 2>&1 | tee -a "$LOG_FILE"
    ;;

  testnet)
    echo "Starting Orchestrator in TESTNET mode on port $PORT..."
    echo "Wallet: $WALLET_NAME / $WALLET_HOTKEY"
    echo "Logging to: $LOG_FILE"

    SUBTENSOR_NETWORK=test \
    NETUID=304 \
    API_PORT=$PORT \
    CLIENT_AUTH_ENABLED=false \
    SUBNET_AUTH_ENABLED=false \
    PUBLIC_ORCHESTRATOR_UID_START=2 \
    PUBLIC_ORCHESTRATOR_UID_END=150 \
    RESERVED_ORCHESTRATOR_UID_START=0 \
    RESERVED_ORCHESTRATOR_UID_END=1 \
    SUBNET_CORE_URL="${SUBNET_CORE_URL:-${SUBNET_CORE_URL_TESTNET:-https://beamcore-dev.b1m.ai}}" \
    REGISTRY_URL="${REGISTRY_URL:-${SUBNET_CORE_URL_TESTNET:-https://beamcore-dev.b1m.ai}}" \
    python main.py 2>&1 | tee -a "$LOG_FILE"
    ;;

  mainnet)
    echo "Starting Orchestrator in MAINNET mode on port $PORT..."
    echo "Wallet: $WALLET_NAME / $WALLET_HOTKEY"
    echo "Logging to: $LOG_FILE"

    SUBTENSOR_NETWORK=finney \
    NETUID=75 \
    API_PORT=$PORT \
    CLIENT_AUTH_ENABLED=false \
    SUBNET_CORE_URL="${SUBNET_CORE_URL:-https://beamcore.b1m.ai}" \
    python main.py 2>&1 | tee -a "$LOG_FILE"
    ;;

  *)
    echo "Usage: $0 [local|testnet|mainnet] [port]"
    echo "  local   - Development mode without Bittensor"
    echo "  testnet - Connect to Bittensor testnet (netuid=304)"
    echo "  mainnet - Connect to Bittensor mainnet (netuid=75)"
    echo "  port    - API port (default: 8001)"
    echo ""
    echo "Example: Run two orchestrators on different ports:"
    echo "  WALLET_HOTKEY=hotkey1 $0 testnet 8001"
    echo "  WALLET_HOTKEY=hotkey2 $0 testnet 8002"
    exit 1
    ;;
esac
