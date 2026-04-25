#!/usr/bin/env python3
"""
Beam Network Worker

Connects to BeamCore and handles data transfer tasks.
Uses bittensor wallet for authentication.

Minimum Requirements:
    - CPU: 2 cores
    - RAM: 4 GB
    - Storage: 20 GB SSD
    - Network: 100 Mbps symmetric (upload/download)
    - OS: Ubuntu 22.04+ / Debian 12+ / macOS 13+

Tech Stack:
    - Python 3.10+
    - bittensor >= 6.0.0
    - httpx >= 0.24.0

Installation:
    pip install bittensor httpx websockets

Usage:
    # Using default wallet (~/.bittensor/wallets/default/hotkeys/default):
    python3 worker.py

    # Using custom wallet:
    python3 worker.py --wallet.name my_wallet --wallet.hotkey my_hotkey

    # Testnet:
    python3 worker.py --subtensor.network test
"""

import argparse
import asyncio
import hashlib
import json
import os
import signal
import sys
import time
from dataclasses import dataclass, field
from typing import Any, Dict, List, Optional

import httpx

try:
    import websockets
    from websockets.exceptions import ConnectionClosed, InvalidStatusCode
    WEBSOCKETS_AVAILABLE = True
except ImportError:
    WEBSOCKETS_AVAILABLE = False

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
TESTNET_URL = "https://beamcore-dev.b1m.ai"

# Connection mode: "websocket", "http", or "auto" (try WS first, fallback to HTTP)
CONNECTION_MODE = os.environ.get("CONNECTION_MODE", "auto")

# Intervals
HEARTBEAT_INTERVAL = 30  # seconds
TASK_POLL_INTERVAL = 5   # seconds

# WebSocket settings
WS_RECONNECT_MIN_DELAY = 12.0   # must exceed server's 10s cooldown
WS_RECONNECT_MAX_DELAY = 60.0
WS_RECONNECT_MULTIPLIER = 2.0
WS_MAX_RECONNECT_ATTEMPTS = 10
WS_HEARTBEAT_INTERVAL = 25      # seconds

# Transfer settings
MAX_CONCURRENT_TASKS = 8
FETCH_TIMEOUT = 30  # seconds
SEND_TIMEOUT = 30   # seconds
MAX_RETRIES = 3
RETRY_BACKOFF = 1.0  # Base backoff in seconds

# Global semaphore for task concurrency
task_semaphore = asyncio.Semaphore(MAX_CONCURRENT_TASKS)


@dataclass
class WorkerState:
    """Worker runtime state."""
    wallet: Any  # bittensor.wallet
    api_url: str
    worker_id: Optional[str] = None
    api_key: Optional[str] = None
    orchestrator_hotkey: Optional[str] = None
    active_tasks: int = 0
    bytes_relayed: int = 0
    running: bool = True
    http_client: Optional[httpx.AsyncClient] = None
    ws_connected: bool = False
    ws_reconnect_attempts: int = 0
    use_websocket: bool = True
    pending_probe_id: Optional[str] = None
    pending_task_accepts: Dict[str, asyncio.Future] = field(default_factory=dict)


# =============================================================================
# Worker Registration with SubnetCore
# =============================================================================

_public_ip: Optional[str] = None


async def get_public_ip() -> str:
    """Get public IP address using external services."""
    global _public_ip
    if _public_ip:
        return _public_ip

    services = [
        "https://api.ipify.org",
        "https://ifconfig.me/ip",
        "https://icanhazip.com",
    ]

    async with httpx.AsyncClient(timeout=10.0) as client:
        for url in services:
            try:
                resp = await client.get(url)
                if resp.status_code == 200:
                    _public_ip = resp.text.strip()
                    print(f"[Worker] Detected public IP: {_public_ip}")
                    return _public_ip
            except Exception:
                continue

    raise RuntimeError("Failed to detect public IP from any service")


def sign_message(wallet: Any, message: str) -> str:
    """Sign a message with the wallet's hotkey. Returns hex signature."""
    signature = wallet.hotkey.sign(message.encode())
    return "0x" + signature.hex()


async def register_worker(client: httpx.AsyncClient, state: WorkerState) -> Dict[str, Any]:
    """Register as a worker with SubnetCore.

    Requires signing the message "{hotkey}:{ip}:{port}" with the wallet's keypair.
    """
    wallet = state.wallet
    hotkey = wallet.hotkey.ss58_address
    ip = await get_public_ip()
    port = 9000

    # Generate a payment pubkey
    payment_pubkey = hashlib.sha256(f"payment:{hotkey}".encode()).hexdigest()

    # Sign the registration message: "{hotkey}:{ip}:{port}"
    message = f"{hotkey}:{ip}:{port}"
    try:
        signature = sign_message(wallet, message)
        print(f"[Worker] Signed registration message")
    except Exception as e:
        raise Exception(f"Failed to sign registration: {e}")

    payload = {
        "hotkey": hotkey,
        "ip": ip,
        "port": port,
        "claimed_bandwidth_mbps": 100,
        "region": "auto",
        "coldkey": wallet.coldkeypub.ss58_address if wallet.coldkeypub else hotkey,
        "payment_pubkey": payment_pubkey,
        "signature": signature,
    }

    # Retry registration up to 3 times
    for attempt in range(3):
        try:
            timeout = 15.0 + (attempt * 10)
            print(f"[Worker] Registration attempt {attempt + 1}/3, timeout={timeout}s")

            response = await client.post(
                f"{state.api_url}/workers/register",
                json=payload,
                timeout=timeout,
            )

            if response.status_code != 200:
                raise Exception(f"HTTP {response.status_code}: {response.text[:200]}")

            data = response.json()

            if not data.get("success"):
                error = data.get("error") or data.get("detail") or data.get("message") or f"Registration failed: {data}"
                raise Exception(error)

            return data

        except httpx.TimeoutException:
            print(f"[Worker] Timeout on attempt {attempt + 1}")
            if attempt == 2:
                raise Exception(f"Timeout connecting to {state.api_url} after 3 attempts")
            await asyncio.sleep(2)
        except httpx.ConnectError:
            print(f"[Worker] Connection error on attempt {attempt + 1}")
            if attempt == 2:
                raise Exception(f"Connection error to {state.api_url} after 3 attempts")
            await asyncio.sleep(2)


async def send_heartbeat(client: httpx.AsyncClient, state: WorkerState) -> bool:
    """Send worker heartbeat to BeamCore."""
    try:
        headers = {"X-Api-Key": state.api_key} if state.api_key else {}
        response = await client.post(
            f"{state.api_url}/workers/heartbeat",
            json={
                "worker_id": state.worker_id,
                "hotkey": state.wallet.hotkey.ss58_address,
                "bandwidth_mbps": 100.0,
                "active_tasks": state.active_tasks,
                "bytes_relayed": state.bytes_relayed,
            },
            headers=headers,
            timeout=10.0,
        )
        if response.status_code != 200:
            return False
        data = response.json()
        return data.get("success", False)
    except Exception as e:
        print(f"[Worker] Heartbeat error: {e}")
        return False


async def poll_worker_tasks(client: httpx.AsyncClient, state: WorkerState) -> List[Dict[str, Any]]:
    """Poll for pending tasks assigned to this worker."""
    try:
        headers = {"X-Api-Key": state.api_key} if state.api_key else {}
        response = await client.get(
            f"{state.api_url}/workers/tasks/pending",
            params={"worker_id": state.worker_id, "max_tasks": 3},
            headers=headers,
            timeout=10.0,
        )
        if response.status_code != 200:
            return []
        if not response.text.strip():
            return []
        data = response.json()
        return data.get("tasks", [])
    except Exception as e:
        print(f"[Worker] Poll error: {e}")
        return []


async def accept_task(client: httpx.AsyncClient, state: WorkerState, task_id: str) -> bool:
    """Accept a task via HTTP."""
    try:
        headers = {"X-Api-Key": state.api_key} if state.api_key else {}
        response = await client.post(
            f"{state.api_url}/workers/tasks/accept",
            params={"task_id": task_id, "worker_id": state.worker_id},
            headers=headers,
            timeout=10.0,
        )
        if response.status_code == 200:
            data = response.json()
            return data.get("accepted", False)
        return False
    except Exception as e:
        print(f"[Worker] Accept task error: {e}")
        return False


async def complete_task(
    client: httpx.AsyncClient,
    state: WorkerState,
    task_id: str,
    success: bool,
    bytes_transferred: int,
    bandwidth_mbps: float,
    duration_ms: float,
    chunk_hash: str = "",
    error: str = None,
) -> bool:
    """Report task completion via HTTP."""
    try:
        payload = {
            "task_id": task_id,
            "worker_id": state.worker_id,
            "success": success,
            "bytes_transferred": bytes_transferred,
            "bandwidth_mbps": bandwidth_mbps,
            "latency_ms": duration_ms,
            "duration_ms": int(duration_ms),
        }
        if chunk_hash:
            payload["chunk_hash"] = chunk_hash
        if error:
            payload["error"] = error

        response = await client.post(
            f"{state.api_url}/workers/tasks/complete",
            json=payload,
            headers={"X-Worker-Hotkey": state.wallet.hotkey.ss58_address},
            timeout=10.0,
        )
        data = response.json()
        return data.get("success", False)
    except Exception as e:
        print(f"[Worker] Complete task error: {e}")
        return False


async def report_chunk_complete(
    client: httpx.AsyncClient,
    state: WorkerState,
    transfer_id: str,
    chunk_index: int,
    etag: Optional[str] = None,
    response_code: Optional[int] = None,
) -> bool:
    """Report chunk completion to SubnetCore."""
    try:
        payload = {}
        if etag:
            payload["etag"] = etag.strip('"')
        if response_code:
            payload["response_code"] = response_code

        headers = {"X-Api-Key": state.api_key} if state.api_key else {}

        response = await client.post(
            f"{state.api_url}/transfers/{transfer_id}/chunks/{chunk_index}/complete",
            json=payload,
            headers=headers,
            timeout=10.0,
        )
        data = response.json()
        if data.get("success"):
            print(f"[Worker] Chunk {chunk_index}: {data.get('chunks_completed')}/{data.get('total_chunks')}")
            return True
        return False
    except Exception as e:
        print(f"[Worker] Failed to report chunk completion: {e}")
        return False


# =============================================================================
# Transfer Helpers
# =============================================================================


def is_retryable(error: Exception) -> bool:
    """Check if an error is retryable."""
    if isinstance(error, (asyncio.TimeoutError, httpx.TimeoutException)):
        return True
    if isinstance(error, httpx.HTTPStatusError):
        return error.response.status_code >= 500
    return False


async def fetch_chunk(
    client: httpx.AsyncClient,
    url: str,
    chunk_offset: int = None,
    chunk_size: int = None,
    total_size: int = None,
) -> bytes:
    """Fetch chunk data from source URL."""
    headers = {"ngrok-skip-browser-warning": "true"}

    if chunk_offset is not None and chunk_size is not None:
        if total_size is not None:
            range_end = min(chunk_offset + chunk_size - 1, total_size - 1)
        else:
            range_end = chunk_offset + chunk_size - 1
        headers["Range"] = f"bytes={chunk_offset}-{range_end}"

    for attempt in range(MAX_RETRIES):
        try:
            response = await client.get(url, headers=headers, timeout=FETCH_TIMEOUT)
            if response.status_code not in (200, 206):
                response.raise_for_status()
            return response.content

        except Exception as e:
            if not is_retryable(e) or attempt == MAX_RETRIES - 1:
                raise
            await asyncio.sleep(RETRY_BACKOFF * (2 ** attempt))

    raise Exception("Max retries exceeded")


def is_s3_presigned_url(url: str) -> bool:
    """Check if URL is an S3 presigned URL."""
    return "s3.amazonaws.com" in url or ("s3." in url and "X-Amz-Signature" in url)


def is_canary_destination(url: str) -> bool:
    """Check if URL is a canary/null destination."""
    if not url:
        return False
    return url.startswith(("null://", "canary://", "skip://"))


async def send_chunk(
    client: httpx.AsyncClient,
    destination_url: str,
    data: bytes,
    transfer_id: str,
    chunk_index: int,
    chunk_offset: int = 0,
    total_size: int = 0,
    auth_token: str = None,
) -> tuple:
    """Send chunk data to destination URL.

    Returns: (success, etag, response_code)
    """
    is_s3 = is_s3_presigned_url(destination_url)

    if is_s3:
        headers = {"Content-Type": "application/octet-stream"}
    else:
        chunk_sha256 = hashlib.sha256(data).hexdigest()
        headers = {
            "Content-Type": "application/octet-stream",
            "X-Transfer-ID": transfer_id,
            "X-Chunk-ID": f"chunk_{chunk_index}",
            "X-Offset": str(chunk_offset),
            "X-Length": str(len(data)),
            "X-Total-Size": str(total_size),
            "X-Chunk-SHA256": chunk_sha256,
        }
        if auth_token:
            headers["Authorization"] = f"Bearer {auth_token}"

    for attempt in range(MAX_RETRIES):
        try:
            if is_s3:
                response = await client.put(
                    destination_url, content=data, headers=headers, timeout=SEND_TIMEOUT
                )
            else:
                response = await client.post(
                    destination_url, content=data, headers=headers, timeout=SEND_TIMEOUT
                )

            response.raise_for_status()
            etag = response.headers.get("ETag") or response.headers.get("etag")
            return (True, etag, response.status_code)

        except Exception as e:
            if not is_retryable(e) or attempt == MAX_RETRIES - 1:
                raise
            await asyncio.sleep(RETRY_BACKOFF * (2 ** attempt))

    raise Exception("Max retries exceeded")


async def execute_transfer(
    state: WorkerState,
    task_id: str,
    execution_context: dict,
    task_message: dict,
    deadline_us: int,
) -> tuple:
    """Execute real data transfer: fetch from source, send to destination.

    Returns: (bytes_transferred, success, error_message, chunk_hash)
    """
    gateway_url = execution_context.get("gateway_url", "")
    destination_url = execution_context.get("destination_url", "")
    transfer_id = execution_context.get("transfer_id", "")
    chunk_indices = execution_context.get("chunk_indices", [0])
    object_id = execution_context.get("object_id")
    chunk_offset = execution_context.get("chunk_offset")
    chunk_size_ctx = execution_context.get("chunk_size")
    total_size = execution_context.get("total_size", 0)
    auth_token = execution_context.get("auth_token")
    source_urls = execution_context.get("source_urls")
    dest_urls = execution_context.get("dest_urls")

    if not chunk_indices:
        chunk_indices = [0]

    # Build per-chunk hash map
    chunk_hashes: dict = {}
    if "chunk_hashes" in task_message and isinstance(task_message["chunk_hashes"], dict):
        for k, v in task_message["chunk_hashes"].items():
            chunk_hashes[int(k)] = v
    elif "chunk_hash" in task_message and task_message["chunk_hash"]:
        if len(chunk_indices) == 1:
            chunk_hashes[chunk_indices[0]] = task_message["chunk_hash"]

    client = state.http_client
    total_bytes = 0
    is_s3_transfer = bool(source_urls and dest_urls)
    is_canary = is_canary_destination(destination_url)
    computed_chunk_hash = ""

    print(f"[Worker] Transferring {len(chunk_indices)} chunk(s)")

    for chunk_index in chunk_indices:
        chunk_key = str(chunk_index)

        # Resolve source URL
        if source_urls and chunk_key in source_urls:
            final_url = source_urls[chunk_key]
        elif object_id:
            base = gateway_url.rstrip("/")
            final_url = f"{base}/objects/{object_id}"
        else:
            base = gateway_url.rstrip("/")
            final_url = f"{base}/chunks/{transfer_id}/{chunk_index}"

        # Resolve destination URL
        if dest_urls and chunk_key in dest_urls:
            chunk_dest_url = dest_urls[chunk_key]
        else:
            chunk_dest_url = destination_url

        use_range = object_id is not None and chunk_offset is not None and chunk_size_ctx is not None

        # Check deadline
        if deadline_us > 0:
            now_us = time.time() * 1_000_000
            remaining_us = deadline_us - now_us
            if remaining_us <= 0:
                return (total_bytes, False, f"Deadline exceeded before chunk {chunk_index}", "")

        try:
            # Fetch chunk
            data = await fetch_chunk(
                client, final_url,
                chunk_offset=chunk_offset if use_range else None,
                chunk_size=chunk_size_ctx if use_range else None,
                total_size=total_size if use_range else None,
            )

            bytes_fetched = len(data)
            computed_chunk_hash = hashlib.sha256(data).hexdigest()

            # Send chunk (or skip for canary)
            if is_canary:
                print(f"[Worker] Chunk {chunk_index}: CANARY mode, skipping upload")
                total_bytes += bytes_fetched
                continue

            send_success, etag, response_code = await send_chunk(
                client, chunk_dest_url, data, transfer_id, chunk_index,
                chunk_offset=chunk_offset if use_range else 0,
                total_size=total_size,
                auth_token=auth_token,
            )

            # Report chunk completion
            await report_chunk_complete(
                client, state, transfer_id, chunk_index,
                etag=etag, response_code=response_code,
            )

            total_bytes += bytes_fetched
            print(f"[Worker] Chunk {chunk_index}: {bytes_fetched} bytes transferred")

        except asyncio.TimeoutError:
            return (total_bytes, False, f"Deadline exceeded at chunk {chunk_index}", "")
        except httpx.HTTPStatusError as e:
            return (total_bytes, False, f"HTTP {e.response.status_code} at chunk {chunk_index}", "")
        except Exception as e:
            return (total_bytes, False, f"Error at chunk {chunk_index}: {e}", "")

    print(f"[Worker] Transfer complete: {total_bytes} bytes")
    return (total_bytes, True, None, computed_chunk_hash)


# =============================================================================
# Task Handler
# =============================================================================


async def handle_task(state: WorkerState, task: dict) -> bool:
    """Handle a task received via HTTP polling."""
    task_id = task.get("task_id") or task.get("offer_id")
    chunk_size = task.get("chunk_size", 0)
    deadline_us = task.get("deadline_us", 0)
    execution_context = task.get("execution_context", {})

    print(f"[Worker] Processing task: {task_id[:16] if task_id else 'unknown'}...")

    # Validate execution_context
    gateway_url = execution_context.get("gateway_url")
    destination_url = execution_context.get("destination_url")
    source_urls = execution_context.get("source_urls")
    dest_urls = execution_context.get("dest_urls")

    if not (gateway_url and destination_url) and not (source_urls and dest_urls):
        print(f"[Worker] Skipping task: missing gateway_url/destination_url and no presigned URLs")
        return False

    # Check deadline
    if deadline_us > 0:
        now_us = time.time() * 1_000_000
        remaining_sec = (deadline_us - now_us) / 1_000_000
        if remaining_sec < 5:
            print(f"[Worker] Skipping task: deadline too close ({remaining_sec:.1f}s)")
            return False

    # Accept task
    accepted = await accept_task(state.http_client, state, task_id)
    if not accepted:
        print(f"[Worker] Failed to accept task")
        return False

    state.active_tasks += 1
    start_time = time.time()
    success = False
    bytes_transferred = 0
    error_msg = None
    chunk_hash = ""

    try:
        async with task_semaphore:
            if deadline_us > 0:
                now_us = time.time() * 1_000_000
                remaining_sec = (deadline_us - now_us) / 1_000_000
                if remaining_sec < 2:
                    error_msg = f"Deadline expired while waiting ({remaining_sec:.1f}s)"
                    print(f"[Worker] {error_msg}")
                else:
                    bytes_transferred, success, error_msg, chunk_hash = await execute_transfer(
                        state, task_id, execution_context, task, deadline_us
                    )
            else:
                bytes_transferred, success, error_msg, chunk_hash = await execute_transfer(
                    state, task_id, execution_context, task, deadline_us
                )
    except Exception as e:
        error_msg = str(e)
        print(f"[Worker] Task error: {e}")

    # Calculate metrics
    duration_ms = (time.time() - start_time) * 1000
    duration_sec = duration_ms / 1000
    bandwidth_mbps = (bytes_transferred * 8 / 1_000_000) / duration_sec if duration_sec > 0 else 0

    # Report completion
    await complete_task(
        client=state.http_client,
        state=state,
        task_id=task_id,
        success=success,
        bytes_transferred=bytes_transferred,
        bandwidth_mbps=round(bandwidth_mbps, 2),
        duration_ms=round(duration_ms, 1),
        chunk_hash=chunk_hash,
        error=error_msg,
    )

    if success:
        state.bytes_relayed += bytes_transferred

    state.active_tasks -= 1

    status = "completed" if success else f"failed: {error_msg}"
    print(f"[Worker] Task {task_id[:16]}: {status} | {bytes_transferred} bytes | {bandwidth_mbps:.1f} Mbps")

    return success


# =============================================================================
# WebSocket Communication
# =============================================================================


def get_ws_url(worker_id: str, api_key: str, api_url: str) -> str:
    """Convert API URL to WebSocket URL for worker connection."""
    base = api_url.rstrip("/")
    if base.startswith("https://"):
        ws_base = "wss://" + base[8:]
    elif base.startswith("http://"):
        ws_base = "ws://" + base[7:]
    else:
        ws_base = "ws://" + base
    url = f"{ws_base}/ws/{worker_id}"
    if api_key:
        url = f"{url}?api_key={api_key}"
    return url


async def ws_send_heartbeat(websocket, state: WorkerState) -> bool:
    """Send heartbeat message over WebSocket."""
    try:
        msg = {
            "type": "heartbeat",
            "worker_id": state.worker_id,
            "hotkey": state.wallet.hotkey.ss58_address,
            "bandwidth_mbps": 100.0,
            "tasks_active": state.active_tasks,
            "bytes_relayed": state.bytes_relayed,
        }
        if state.pending_probe_id:
            msg["echo_probe_id"] = state.pending_probe_id
            state.pending_probe_id = None
        await websocket.send(json.dumps(msg))
        return True
    except Exception as e:
        print(f"[Worker] WS heartbeat error: {e}")
        return False


async def ws_send_task_accept(websocket, state: WorkerState, task_id: str) -> bool:
    """Send task acceptance over WebSocket."""
    try:
        msg = {
            "type": "task_accept",
            "offer_id": task_id,
            "task_id": task_id,
            "worker_id": state.worker_id,
        }
        await websocket.send(json.dumps(msg))
        return True
    except Exception as e:
        print(f"[Worker] WS task_accept error: {e}")
        return False


async def ws_send_task_result(
    websocket,
    state: WorkerState,
    task_id: str,
    success: bool,
    bytes_transferred: int,
    bandwidth_mbps: float,
    duration_ms: float,
    chunk_hash: str = "",
    error: str = None,
) -> bool:
    """Send task result over WebSocket."""
    try:
        msg = {
            "type": "task_result",
            "task_id": task_id,
            "worker_id": state.worker_id,
            "success": success,
            "bytes_transferred": bytes_transferred,
            "bandwidth_mbps": bandwidth_mbps,
            "latency_ms": duration_ms,
            "duration_ms": int(duration_ms),
        }
        if chunk_hash:
            msg["chunk_hash"] = chunk_hash
        if error:
            msg["error"] = error
        await websocket.send(json.dumps(msg))
        return True
    except Exception as e:
        print(f"[Worker] WS task_result error: {e}")
        return False


async def handle_ws_task(state: WorkerState, websocket, task: dict) -> bool:
    """Handle a task received via WebSocket push."""
    task_id = task.get("task_id") or task.get("offer_id")
    chunk_size = task.get("chunk_size", 0)
    deadline_us = task.get("deadline_us", 0)
    execution_context = task.get("execution_context", {})

    print(f"[Worker] [WS] Task: {task_id[:16] if task_id else 'unknown'}...")

    gateway_url = execution_context.get("gateway_url")
    destination_url = execution_context.get("destination_url")
    source_urls = execution_context.get("source_urls")
    dest_urls = execution_context.get("dest_urls")

    if not (gateway_url and destination_url) and not (source_urls and dest_urls):
        print(f"[Worker] [WS] Skipping task: missing gateway_url/destination_url and no presigned URLs")
        return False

    if deadline_us > 0:
        now_us = time.time() * 1_000_000
        remaining_sec = (deadline_us - now_us) / 1_000_000
        if remaining_sec < 5:
            print(f"[Worker] [WS] Skipping task: deadline too close ({remaining_sec:.1f}s)")
            return False

    # Register a future to wait for the server's accept ack before executing
    accept_future: asyncio.Future = asyncio.get_event_loop().create_future()
    state.pending_task_accepts[task_id] = accept_future

    accepted = await ws_send_task_accept(websocket, state, task_id)
    if not accepted:
        state.pending_task_accepts.pop(task_id, None)
        print(f"[Worker] [WS] Failed to send task accept")
        return False

    # Wait for server confirmation (5s timeout — proceed on timeout to avoid stalling)
    try:
        server_accepted = await asyncio.wait_for(accept_future, timeout=5.0)
        if not server_accepted:
            print(f"[Worker] [WS] Task accept rejected by server")
            return False
    except asyncio.TimeoutError:
        state.pending_task_accepts.pop(task_id, None)
        print(f"[Worker] [WS] Accept ack timeout, proceeding")

    state.active_tasks += 1
    start_time = time.time()
    success = False
    bytes_transferred = 0
    error_msg = None
    chunk_hash = ""

    try:
        async with task_semaphore:
            if deadline_us > 0:
                now_us = time.time() * 1_000_000
                remaining_sec = (deadline_us - now_us) / 1_000_000
                if remaining_sec < 2:
                    error_msg = f"Deadline expired while waiting ({remaining_sec:.1f}s)"
                    print(f"[Worker] [WS] {error_msg}")
                else:
                    bytes_transferred, success, error_msg, chunk_hash = await execute_transfer(
                        state, task_id, execution_context, task, deadline_us
                    )
            else:
                bytes_transferred, success, error_msg, chunk_hash = await execute_transfer(
                    state, task_id, execution_context, task, deadline_us
                )
    except Exception as e:
        error_msg = str(e)
        print(f"[Worker] [WS] Task error: {e}")

    duration_ms = (time.time() - start_time) * 1000
    duration_sec = duration_ms / 1000
    bandwidth_mbps = (bytes_transferred * 8 / 1_000_000) / duration_sec if duration_sec > 0 else 0

    await ws_send_task_result(
        websocket, state, task_id, success, bytes_transferred,
        round(bandwidth_mbps, 2), round(duration_ms, 1),
        chunk_hash=chunk_hash, error=error_msg,
    )

    if success:
        state.bytes_relayed += bytes_transferred

    state.active_tasks -= 1

    status = "OK" if success else f"FAIL: {error_msg}"
    print(f"[Worker] [WS] Task {task_id[:16]}: {status} | {bytes_transferred} bytes | {bandwidth_mbps:.1f} Mbps")

    return success


async def websocket_loop(state: WorkerState):
    """WebSocket communication loop with automatic reconnection."""
    if not WEBSOCKETS_AVAILABLE:
        print(f"[Worker] websockets library not available, using HTTP polling")
        state.use_websocket = False
        return

    ws_url = get_ws_url(state.worker_id, state.api_key, state.api_url)
    print(f"[Worker] Connecting to WebSocket: {ws_url.split('?')[0]}")
    reconnect_delay = WS_RECONNECT_MIN_DELAY

    while state.running and state.use_websocket:
        try:
            async with websockets.connect(
                ws_url,
                ping_interval=WS_HEARTBEAT_INTERVAL,
                ping_timeout=10,
                close_timeout=5,
            ) as websocket:
                state.ws_connected = True
                state.ws_reconnect_attempts = 0
                reconnect_delay = WS_RECONNECT_MIN_DELAY
                print(f"[Worker] [WS] Connected!")

                last_heartbeat = time.time()

                while state.running:
                    try:
                        try:
                            msg_str = await asyncio.wait_for(
                                websocket.recv(),
                                timeout=WS_HEARTBEAT_INTERVAL,
                            )
                            message = json.loads(msg_str)
                            msg_type = message.get("type")

                            if msg_type == "connected":
                                print(f"[Worker] [WS] Server confirmed connection")

                            elif msg_type == "heartbeat_ack":
                                probe_id = message.get("probe_id")
                                if probe_id:
                                    state.pending_probe_id = probe_id

                                bw_challenge = message.get("bw_challenge")
                                if bw_challenge:
                                    challenge_id = bw_challenge.get("challenge_id")
                                    if challenge_id:
                                        bw_response = {
                                            "type": "bw_challenge_response",
                                            "challenge_id": challenge_id,
                                            "worker_id": state.worker_id,
                                        }
                                        await websocket.send(json.dumps(bw_response))

                            elif msg_type == "task_offer":
                                asyncio.create_task(handle_ws_task(state, websocket, message))

                            elif msg_type == "task_accept_ack":
                                ack_task_id = message.get("task_id") or message.get("offer_id")
                                server_accepted = message.get("accepted", True)
                                if ack_task_id and ack_task_id in state.pending_task_accepts:
                                    future = state.pending_task_accepts.pop(ack_task_id)
                                    if not future.done():
                                        future.set_result(server_accepted)
                                if not server_accepted:
                                    print(f"[Worker] [WS] Task accept rejected: {message.get('reason', 'unknown')}")

                            elif msg_type == "task_result_ack":
                                pass

                            elif msg_type == "error":
                                print(f"[Worker] [WS] Server error: {message.get('message', 'unknown')}")

                        except asyncio.TimeoutError:
                            pass

                        now = time.time()
                        if now - last_heartbeat >= WS_HEARTBEAT_INTERVAL:
                            await ws_send_heartbeat(websocket, state)
                            last_heartbeat = now

                    except ConnectionClosed as e:
                        print(f"[Worker] [WS] Connection closed: {e.code} {e.reason}")
                        break

        except InvalidStatusCode as e:
            print(f"[Worker] [WS] Connection rejected: HTTP {e.status_code}")
            if e.status_code == 4001:
                print(f"[Worker] [WS] Server has WebSocket disabled, falling back to HTTP polling")
                state.use_websocket = False
                return

        except ConnectionRefusedError:
            print(f"[Worker] [WS] Connection refused")

        except Exception as e:
            print(f"[Worker] [WS] Connection error: {type(e).__name__}: {e}")

        state.ws_connected = False
        state.ws_reconnect_attempts += 1

        if state.ws_reconnect_attempts >= WS_MAX_RECONNECT_ATTEMPTS:
            print(f"[Worker] [WS] Max reconnect attempts reached, falling back to HTTP polling")
            state.use_websocket = False
            return

        if state.running and not shutdown_event.is_set():
            print(f"[Worker] [WS] Reconnecting in {reconnect_delay:.1f}s (attempt {state.ws_reconnect_attempts}/{WS_MAX_RECONNECT_ATTEMPTS})...")
            try:
                await asyncio.wait_for(shutdown_event.wait(), timeout=reconnect_delay)
                break
            except asyncio.TimeoutError:
                pass
            reconnect_delay = min(reconnect_delay * WS_RECONNECT_MULTIPLIER, WS_RECONNECT_MAX_DELAY)

    state.ws_connected = False
    print(f"[Worker] [WS] Loop stopped")


# =============================================================================
# Main Polling Loop
# =============================================================================


async def polling_loop(state: WorkerState):
    """HTTP polling loop for receiving and processing tasks."""
    poll_count = 0
    heartbeat_count = 0
    last_heartbeat = 0

    print(f"[Worker] Starting polling loop...")

    while state.running:
        try:
            # Send heartbeat periodically
            now = time.time()
            if now - last_heartbeat >= HEARTBEAT_INTERVAL:
                await send_heartbeat(state.http_client, state)
                last_heartbeat = now
                heartbeat_count += 1
                if heartbeat_count % 10 == 1:
                    print(f"[Worker] Heartbeat #{heartbeat_count} | Tasks: {state.active_tasks} | Bytes: {state.bytes_relayed}")

            # Poll for tasks
            tasks = await poll_worker_tasks(state.http_client, state)
            poll_count += 1

            if tasks:
                print(f"[Worker] Found {len(tasks)} pending task(s)")
                # Process tasks concurrently
                task_coros = [handle_task(state, task) for task in tasks if state.running]
                if task_coros:
                    await asyncio.gather(*task_coros, return_exceptions=True)
                continue  # Poll immediately for more

            await asyncio.sleep(TASK_POLL_INTERVAL)

        except asyncio.CancelledError:
            break
        except Exception as e:
            if state.running:
                print(f"[Worker] Polling error: {e}")
            await asyncio.sleep(TASK_POLL_INTERVAL)

    print(f"[Worker] Polling loop stopped")


# =============================================================================
# Main
# =============================================================================


shutdown_event = asyncio.Event()


async def run_worker(state: WorkerState):
    """Run the worker."""
    wallet = state.wallet
    hotkey = wallet.hotkey.ss58_address

    # Create HTTP client
    state.http_client = httpx.AsyncClient(
        timeout=httpx.Timeout(connect=10.0, read=60.0, write=60.0, pool=5.0),
        limits=httpx.Limits(max_connections=20, max_keepalive_connections=10)
    )

    try:
        async with httpx.AsyncClient() as client:
            # Register with SubnetCore
            print(f"[Worker] Registering with SubnetCore...")
            print(f"[Worker] Hotkey: {hotkey}")
            print(f"[Worker] API URL: {state.api_url}")

            result = await register_worker(client, state)
            state.worker_id = result.get("worker_id")
            state.api_key = result.get("api_key")
            print(f"[Worker] Registered: {state.worker_id}")

        # Start connection loop
        if CONNECTION_MODE == "http":
            state.use_websocket = False
        elif CONNECTION_MODE == "websocket":
            state.use_websocket = WEBSOCKETS_AVAILABLE
        else:  # "auto" — try WS first, fallback to HTTP
            state.use_websocket = WEBSOCKETS_AVAILABLE

        if state.use_websocket:
            print(f"[Worker] Starting WebSocket connection (instant task push)")
            await websocket_loop(state)
            if not state.use_websocket and state.running:
                print(f"[Worker] Falling back to HTTP polling")
                await polling_loop(state)
        else:
            print(f"[Worker] Starting HTTP polling (interval: {TASK_POLL_INTERVAL}s)")
            await polling_loop(state)

    except asyncio.CancelledError:
        print(f"[Worker] Cancelled")
    except Exception as e:
        print(f"[Worker] Error: {e}")
        raise
    finally:
        if state.http_client:
            await state.http_client.aclose()
            state.http_client = None

    print(f"[Worker] Stopped")


def get_config():
    """Get configuration from command line arguments."""
    parser = argparse.ArgumentParser(description="Beam Network Worker")

    # Bittensor wallet arguments
    bt.wallet.add_args(parser)
    bt.subtensor.add_args(parser)

    # Parse arguments
    config = bt.config(parser)
    return config


async def main():
    """Main entry point."""
    print("Beam Network Worker")
    print("=" * 40)

    # Parse configuration
    config = get_config()

    # Load bittensor wallet
    wallet = bt.wallet(config=config)
    print(f"Wallet name: {wallet.name}")
    print(f"Hotkey name: {wallet.hotkey_str}")

    # Unlock hotkey (will prompt for password if encrypted)
    try:
        _ = wallet.hotkey
        print(f"Hotkey address: {wallet.hotkey.ss58_address}")
    except Exception as e:
        print(f"Failed to load hotkey: {e}")
        sys.exit(1)

    # Determine API URL based on network
    network = config.subtensor.get("network", "finney")
    if network in ("test", "testnet"):
        api_url = os.environ.get("SUBNET_CORE_URL", TESTNET_URL)
        print(f"Network: testnet")
    else:
        api_url = os.environ.get("SUBNET_CORE_URL", MAINNET_URL)
        print(f"Network: mainnet")

    print(f"API URL: {api_url}")
    print()

    # Create worker state
    state = WorkerState(wallet=wallet, api_url=api_url)

    # Setup signal handlers
    loop = asyncio.get_running_loop()

    def handle_shutdown():
        print(f"\nShutting down worker...")
        state.running = False
        shutdown_event.set()

    for sig in (signal.SIGINT, signal.SIGTERM):
        loop.add_signal_handler(sig, handle_shutdown)

    # Run worker
    try:
        await run_worker(state)
    finally:
        for sig in (signal.SIGINT, signal.SIGTERM):
            loop.remove_signal_handler(sig)

    print("Worker stopped")


if __name__ == "__main__":
    try:
        asyncio.run(main())
    except KeyboardInterrupt:
        print("\nExited")
