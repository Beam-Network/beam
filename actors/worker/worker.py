#!/usr/bin/env python3
"""
Beam Network Worker

Registers with BeamCore, connects to an orchestrator-owned worker gateway, and handles data transfer tasks.
Uses bittensor wallet for authentication.

Minimum Requirements:
    - CPU: 2 cores
    - RAM: 4 GB
    - Storage: 20 GB SSD
    - Network: 100 Mbps symmetric (upload/download)
    - OS: Ubuntu 22.04+ / Debian 12+ / macOS 13+

Tech Stack:
    - Python 3.10+
    - bittensor >= 10.3.1,<11.0.0
    - httpx >= 0.25.0
    - websockets >= 12.0

Installation:
    pip install bittensor httpx websockets

Usage:
    # Using default wallet (~/.bittensor/wallets/default/hotkeys/default):
    python3 worker.py

    # Using custom wallet:
    python3 worker.py --wallet.name my_wallet --wallet.hotkey my_hotkey

    # Mainnet:
    python3 worker.py --subtensor.network finney
"""

import asyncio
import json
import os
import sys
import time
from typing import Any, Dict, Optional

import httpx

try:
    from .models import (
        CAPABILITY_SCHEMA_VERSION,
        WORKER_CAPABILITIES,
        WORKER_VERSION,
        TaskExecutionResult,
        TaskSummaryAck,
        WorkerState,
        build_worker_capability_manifest,
        capability_timestamp,
        resolve_worker_version,
    )
except ImportError:  # Direct execution: python actors/worker/worker.py
    from models import (
        CAPABILITY_SCHEMA_VERSION,
        WORKER_CAPABILITIES,
        WORKER_VERSION,
        TaskExecutionResult,
        TaskSummaryAck,
        WorkerState,
        build_worker_capability_manifest,
        capability_timestamp,
        resolve_worker_version,
    )

try:
    from .transfer_context import (
        RANGE_HEADER_RE,
        api_key_headers,
        build_transfer_context,
        claim_ws_task,
        exception_detail,
        format_route_context,
        http_status_detail,
        is_canary_destination,
        is_object_storage_presigned_url,
        is_retryable,
        object_storage_route_context,
        offer_headers,
        parse_offer_range,
        redact_url,
        remaining_deadline_seconds,
        task_label,
    )

except ImportError:  # Direct execution: python actors/worker/worker.py
    from transfer_context import (
        RANGE_HEADER_RE,
        api_key_headers,
        build_transfer_context,
        claim_ws_task,
        exception_detail,
        format_route_context,
        http_status_detail,
        is_canary_destination,
        is_object_storage_presigned_url,
        is_retryable,
        object_storage_route_context,
        offer_headers,
        parse_offer_range,
        redact_url,
        remaining_deadline_seconds,
        task_label,
    )

try:
    from . import registration as _registration
except ImportError:  # Direct execution: python actors/worker/worker.py
    import registration as _registration

try:
    from . import transfer as _transfer
except ImportError:  # Direct execution: python actors/worker/worker.py
    import transfer as _transfer

try:
    from . import websocket_runtime as _websocket_runtime
except ImportError:  # Direct execution: python actors/worker/worker.py
    import websocket_runtime as _websocket_runtime

try:
    from . import cli_config as _cli_config
    from . import runtime as _runtime
except ImportError:  # Direct execution: python actors/worker/worker.py
    import cli_config as _cli_config
    import runtime as _runtime

websockets = _websocket_runtime.websockets
ConnectionClosed = _websocket_runtime.ConnectionClosed
InvalidStatus = _websocket_runtime.InvalidStatus
WEBSOCKETS_AVAILABLE = _websocket_runtime.WEBSOCKETS_AVAILABLE

try:
    import bittensor as bt

    BITTENSOR_AVAILABLE = True
except ImportError:
    BITTENSOR_AVAILABLE = False
    print("Error: bittensor library not installed.")
    print("Install with: pip install bittensor")
    sys.exit(1)

# =============================================================================
# Configuration
# =============================================================================

# Network endpoints
MAINNET_URL = "https://beamcore.b1m.ai"
TESTNET_URL = ""

# Connection mode: worker transport is websocket-only after registration.
CONNECTION_MODE = os.environ.get("CONNECTION_MODE", "websocket").lower()


# WebSocket settings
WS_RECONNECT_MIN_DELAY = 12.0  # must exceed server's 10s cooldown
WS_RECONNECT_MAX_DELAY = 60.0
WS_RECONNECT_MULTIPLIER = 2.0
_ws_max_reconnect_attempts = os.environ.get("WS_MAX_RECONNECT_ATTEMPTS", "0").strip()
WS_MAX_RECONNECT_ATTEMPTS = (
    None if not _ws_max_reconnect_attempts or int(_ws_max_reconnect_attempts) <= 0 else int(_ws_max_reconnect_attempts)
)

WS_PING_INTERVAL = 25  # seconds
WS_PING_TIMEOUT = float(
    os.environ.get("WORKER_WS_PING_TIMEOUT", os.environ.get("WS_PING_TIMEOUT", "120"))
)

# Compatibility exports for callers that historically imported these here.
FETCH_TIMEOUT = _transfer.FETCH_TIMEOUT
SEND_TIMEOUT = _transfer.SEND_TIMEOUT
MAX_RETRIES = _transfer.MAX_RETRIES
RETRY_BACKOFF = _transfer.RETRY_BACKOFF
FETCH_STREAM_CHUNK_SIZE = _transfer.FETCH_STREAM_CHUNK_SIZE
WS_TASK_RESULT_ACK_TIMEOUT = float(os.environ.get("WORKER_TASK_RESULT_ACK_TIMEOUT", "3.0"))
WS_TASK_RESULT_SEND_ATTEMPTS = max(3, int(os.environ.get("WORKER_TASK_RESULT_SEND_ATTEMPTS", "8")))
WS_TASK_RESULT_RECONNECT_WAIT_SECONDS = max(0.0, float(os.environ.get("WORKER_TASK_RESULT_RECONNECT_WAIT_SECONDS", "2.0")))
TASK_RESULT_ACK_STATUSES = {
    "owned_processing",
    "completed",
    "failed",
    "late_superseded",
    "late_expired",
    "retry",
    "rejected",
}
TASK_RESULT_TERMINAL_STATUSES = TASK_RESULT_ACK_STATUSES - {"retry"}


async def execute_task_with_metrics(
    state: WorkerState,
    task_id: str,
    task: dict,
    transfer_context: dict,
    deadline_us: int,
    log_prefix: str = "[Worker]",
) -> TaskExecutionResult:
    return await _transfer.execute_task_with_metrics(
        state,
        task_id,
        task,
        transfer_context,
        deadline_us,
        log_prefix,
        execute_transfer_fn=execute_transfer,
    )


async def get_public_ip() -> str:
    return await _registration.get_public_ip()


def sign_message(wallet: Any, message: str) -> str:
    return _registration.sign_message(wallet, message)


async def register_worker(client: httpx.AsyncClient, state: WorkerState) -> Dict[str, Any]:
    return await _registration.register_worker(
        client,
        state,
        public_ip_resolver=get_public_ip,
        signer=sign_message,
    )


# =============================================================================
# Transfer Helpers
# =============================================================================


async def fetch_chunk(
    client: httpx.AsyncClient,
    url: str,
    expected_max_bytes: int = None,
    task_id: str = None,
    offer_id: str = None,
    chunk_index: int = None,
    offer_source_headers: dict = None,
) -> bytes:
    return await _transfer.fetch_chunk(
        client,
        url,
        expected_max_bytes,
        task_id,
        offer_id,
        chunk_index,
        offer_source_headers,
    )


async def send_chunk(
    client: httpx.AsyncClient,
    destination_url: str,
    data: bytes,
    transfer_id: str,
    chunk_index: int,
    chunk_offset: int = 0,
    total_size: int = 0,
    auth_token: str = None,
    task_id: str = None,
    offer_id: str = None,
    route_metadata: Optional[Dict[str, Any]] = None,
    offer_dest_headers: dict = None,
) -> tuple:
    return await _transfer.send_chunk(
        client,
        destination_url,
        data,
        transfer_id,
        chunk_index,
        chunk_offset,
        total_size,
        auth_token,
        task_id,
        offer_id,
        route_metadata,
        offer_dest_headers,
    )


async def execute_transfer(
    state: WorkerState,
    task_id: str,
    transfer_context: dict,
    task_message: dict,
    deadline_us: int,
) -> tuple:
    return await _transfer.execute_transfer(
        state,
        task_id,
        transfer_context,
        task_message,
        deadline_us,
        fetch_chunk_fn=fetch_chunk,
        send_chunk_fn=send_chunk,
    )

# =============================================================================
# WebSocket Communication
# =============================================================================


def get_ws_url(worker_id: str, api_key: str, gateway_url: str) -> str:
    return _websocket_runtime.get_ws_url(worker_id, api_key, gateway_url)


def get_ws_status_code(exc: Exception) -> Optional[int]:
    return _websocket_runtime.get_ws_status_code(exc)


async def wait_for_result_websocket(state: WorkerState, fallback):
    return await _websocket_runtime.wait_for_result_websocket(
        state,
        fallback,
        WS_TASK_RESULT_RECONNECT_WAIT_SECONDS,
    )


async def ws_send_task_result(
    websocket,
    state: WorkerState,
    task_id: str,
    success: bool,
    bytes_transferred: int,
    duration_ms: Optional[float] = None,
    chunk_hash: str = "",
    etag: str = None,
    error: str = None,
    offer_id: str = None,
) -> bool:
    return await _websocket_runtime.ws_send_task_result(
        websocket,
        state,
        task_id,
        success,
        bytes_transferred,
        duration_ms,
        chunk_hash,
        etag,
        error,
        offer_id,
    )


async def ws_send_capability_update(websocket, state: WorkerState) -> None:
    await _websocket_runtime.ws_send_capability_update(
        websocket,
        state,
        manifest_builder=build_worker_capability_manifest,
    )


async def finalize_ws_task_result(
    websocket,
    state: WorkerState,
    task_id: str,
    success: bool,
    bytes_transferred: int,
    duration_ms: Optional[float] = None,
    chunk_hash: str = "",
    etag: str = None,
    error: str = None,
    offer_id: str = None,
) -> TaskSummaryAck:
    async def wait_adapter(current_state, fallback, _reconnect_wait_seconds):
        return await wait_for_result_websocket(current_state, fallback)

    return await _websocket_runtime.finalize_ws_task_result(
        websocket,
        state,
        task_id,
        success,
        bytes_transferred,
        duration_ms,
        chunk_hash,
        etag,
        error,
        offer_id,
        wait_for_websocket_fn=wait_adapter,
        send_result_fn=ws_send_task_result,
        ack_timeout=WS_TASK_RESULT_ACK_TIMEOUT,
        send_attempts=WS_TASK_RESULT_SEND_ATTEMPTS,
        reconnect_wait_seconds=WS_TASK_RESULT_RECONNECT_WAIT_SECONDS,
        terminal_statuses=TASK_RESULT_TERMINAL_STATUSES,
    )


def enqueue_ws_task(state: WorkerState, websocket, task: dict) -> None:
    _websocket_runtime.enqueue_ws_task(state, websocket, task)


async def ws_task_executor(state: WorkerState) -> None:
    await _websocket_runtime.ws_task_executor(state, handle_task_fn=handle_ws_task)


async def handle_ws_task(state: WorkerState, websocket, task: dict) -> bool:
    return await _websocket_runtime.handle_ws_task(
        state,
        websocket,
        task,
        execute_task_fn=execute_task_with_metrics,
        finalize_result_fn=finalize_ws_task_result,
    )


async def websocket_loop(state: WorkerState):
    if not WEBSOCKETS_AVAILABLE:
        raise RuntimeError("websockets library is required for worker gateway transport")
    await _websocket_runtime.websocket_loop(
        state,
        websocket_api=websockets,
        connection_closed_type=ConnectionClosed,
        invalid_status_type=InvalidStatus,
        shutdown_signal=shutdown_event,
        send_capability_fn=ws_send_capability_update,
        enqueue_task_fn=enqueue_ws_task,
        valid_ack_statuses=TASK_RESULT_ACK_STATUSES,
        reconnect_min_delay=WS_RECONNECT_MIN_DELAY,
        reconnect_max_delay=WS_RECONNECT_MAX_DELAY,
        reconnect_multiplier=WS_RECONNECT_MULTIPLIER,
        max_reconnect_attempts=WS_MAX_RECONNECT_ATTEMPTS,
        ping_interval=WS_PING_INTERVAL,
        ping_timeout=WS_PING_TIMEOUT,
    )

# =============================================================================
# Main
# =============================================================================


shutdown_event = asyncio.Event()


async def run_worker(state: WorkerState):
    await _runtime.run_worker(
        state,
        httpx_module=httpx,
        register_worker_fn=register_worker,
        websocket_loop_fn=websocket_loop,
        task_executor_fn=ws_task_executor,
        connection_mode=CONNECTION_MODE,
        websockets_available=WEBSOCKETS_AVAILABLE,
    )


def get_config():
    return _cli_config.get_config(bt)


def apply_env_config_overrides(config):
    _cli_config.apply_env_config_overrides(config, os.environ)


async def main():
    await _runtime.main(
        bt_module=bt,
        environ=os.environ,
        get_config_fn=get_config,
        apply_config_overrides_fn=apply_env_config_overrides,
        run_worker_fn=run_worker,
        shutdown_signal=shutdown_event,
        mainnet_url=MAINNET_URL,
        testnet_url=TESTNET_URL,
        exit_fn=sys.exit,
    )

if __name__ == "__main__":
    try:
        asyncio.run(main())
    except KeyboardInterrupt:
        print("\nExited")
