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

import argparse
import asyncio
import hashlib
import json
import os
import re
import signal
import sys
import time
from dataclasses import dataclass, field
from importlib.metadata import PackageNotFoundError, version as package_version
from typing import Any, Dict, Optional
from urllib.parse import parse_qs, urlsplit, urlunsplit

import httpx

try:
    import websockets
    from websockets.exceptions import ConnectionClosed

    try:
        from websockets.exceptions import InvalidStatus
    except ImportError:
        from websockets.exceptions import InvalidStatusCode as InvalidStatus
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

# Connection mode: worker transport is websocket-only after registration.
CONNECTION_MODE = os.environ.get("CONNECTION_MODE", "websocket").lower()


def resolve_worker_version() -> str:
    try:
        return package_version("beam")
    except PackageNotFoundError:
        return "0.2.0"


def parse_strict_semver(value: str) -> Optional[tuple[int, int, int]]:
    parts = str(value or "").split(".")
    if len(parts) != 3:
        return None
    parsed: list[int] = []
    for part in parts:
        if not part.isdigit():
            return None
        if len(part) > 1 and part.startswith("0"):
            return None
        parsed.append(int(part))
    return parsed[0], parsed[1], parsed[2]


def worker_version_satisfies(minimum_version: str) -> bool:
    current = parse_strict_semver(WORKER_VERSION)
    minimum = parse_strict_semver(minimum_version)
    return bool(current and minimum and current >= minimum)


WORKER_VERSION = resolve_worker_version()

# WebSocket settings
WS_RECONNECT_MIN_DELAY = 12.0  # must exceed server's 10s cooldown
WS_RECONNECT_MAX_DELAY = 60.0
WS_RECONNECT_MULTIPLIER = 2.0
_ws_max_reconnect_attempts = os.environ.get("WS_MAX_RECONNECT_ATTEMPTS", "0").strip()
WS_MAX_RECONNECT_ATTEMPTS = (
    None if not _ws_max_reconnect_attempts or int(_ws_max_reconnect_attempts) <= 0 else int(_ws_max_reconnect_attempts)
)

WS_PING_INTERVAL = 25  # seconds

# Transfer settings
DEFAULT_CHUNK_SIZE_BYTES = 4 * 1024 * 1024
MAX_CONCURRENT_TASKS = max(1, int(os.environ.get("WORKER_MAX_CONCURRENT_TASKS", "4")))
MAX_QUEUED_WS_TASKS = max(
    1, int(os.environ.get("WORKER_MAX_QUEUED_WS_TASKS", str(MAX_CONCURRENT_TASKS)))
)
MAX_IN_FLIGHT_BYTES = max(
    DEFAULT_CHUNK_SIZE_BYTES,
    int(os.environ.get("WORKER_MAX_IN_FLIGHT_BYTES", str(256 * 1024 * 1024))),
)
FETCH_TIMEOUT = 30  # seconds
SEND_TIMEOUT = 30  # seconds
MAX_RETRIES = 3
RETRY_BACKOFF = 1.0  # Base backoff in seconds
FETCH_STREAM_CHUNK_SIZE = 64 * 1024
WS_TASK_RESULT_ACK_TIMEOUT = float(os.environ.get("WORKER_TASK_RESULT_ACK_TIMEOUT", "3.0"))


def _env_bool(name: str, default: bool) -> bool:
    raw = os.environ.get(name)
    if raw is None or not str(raw).strip():
        return default
    return str(raw).strip().lower() in ("1", "true", "yes")


# Participant workers default to recording a payment obligation unless opted out.
WORKER_REQUIRED_PAYMENT = _env_bool("WORKER_REQUIRED_PAYMENT", True)

# Global semaphore for task concurrency
task_semaphore = asyncio.Semaphore(MAX_CONCURRENT_TASKS)


@dataclass
class WorkerState:
    """Worker runtime state."""

    wallet: Any  # bittensor.wallet
    api_url: str
    worker_gateway_url: Optional[str] = None
    worker_id: Optional[str] = None
    api_key: Optional[str] = None
    orchestrator_hotkey: Optional[str] = None
    active_tasks: int = 0
    running: bool = True
    http_client: Optional[httpx.AsyncClient] = None
    ws_connected: bool = False
    ws_reconnect_attempts: int = 0
    use_websocket: bool = True
    pending_task_accepts: Dict[str, asyncio.Future] = field(default_factory=dict)
    pending_task_results: Dict[str, asyncio.Future] = field(default_factory=dict)
    active_ws_task_ids: set[str] = field(default_factory=set)
    ws_task_handles: set[asyncio.Task] = field(default_factory=set)
    reserved_ws_slots: int = 0
    reserved_bytes: int = 0
    ws_send_lock: Optional[asyncio.Lock] = None


@dataclass
class TaskExecutionResult:
    """Normalized task execution metrics used by HTTP and WebSocket paths."""

    success: bool
    bytes_transferred: int
    duration_ms: float
    chunk_hash: str = ""
    etag: Optional[str] = None
    error_msg: Optional[str] = None


@dataclass
class TaskSummaryAck:
    """BeamCore task_result_ack fields used for payment gating."""

    received: bool = False
    completed: bool = False
    reason: Optional[str] = None


def task_label(task_id: Optional[str]) -> str:
    """Short task label for logs."""
    return task_id[:16] if task_id else "unknown"


def exception_detail(error: Exception) -> str:
    """Return an exception string that is useful even when str(error) is empty."""
    if isinstance(error, httpx.HTTPStatusError):
        request_url = str(error.request.url)
        redacted_url = redact_url(request_url)
        try:
            body = error.response.text[:500].strip()
        except httpx.ResponseNotRead:
            body = ""
        body_detail = f" body={body!r}" if body else ""
        return (
            f"{type(error).__name__}: HTTP {error.response.status_code} "
            f"for {redacted_url}{body_detail}"
        )
    message = str(error)
    if message:
        return f"{type(error).__name__}: {message}"
    return f"{type(error).__name__}: {repr(error)}"


def redact_url(url: str) -> str:
    """Drop query parameters from capability URLs before logging errors."""
    try:
        parts = urlsplit(url)
    except ValueError:
        return url.split("?", 1)[0]
    return urlunsplit((parts.scheme, parts.netloc, parts.path, "", ""))


def object_storage_route_context(
    destination_url: str,
    route_metadata: Optional[Dict[str, Any]] = None,
) -> Dict[str, Any]:
    """Return safe multipart route fields for logs without exposing signatures."""
    context: Dict[str, Any] = {}
    if route_metadata:
        for key in (
            "transfer_id",
            "source_id",
            "destination_id",
            "chunk_index",
            "upload_id",
            "part_number",
            "final_object_key",
            "multipart_group_id",
            "multipart_created_at",
            "urls_expires_at",
        ):
            value = route_metadata.get(key)
            if value is not None:
                context[key] = value

    try:
        parts = urlsplit(destination_url)
        query = parse_qs(parts.query)
    except ValueError:
        return context

    if "upload_id" not in context and query.get("uploadId"):
        context["upload_id"] = query["uploadId"][0]
    if "part_number" not in context and query.get("partNumber"):
        context["part_number"] = query["partNumber"][0]
    if "final_object_key" not in context:
        context["final_object_key"] = parts.path.lstrip("/")
    context["destination_host"] = parts.netloc
    return context


def format_route_context(context: Dict[str, Any]) -> str:
    """Format safe route fields in stable order for grep-friendly logs."""
    if not context:
        return ""
    ordered_keys = (
        "transfer_id",
        "source_id",
        "destination_id",
        "chunk_index",
        "upload_id",
        "part_number",
        "final_object_key",
        "multipart_group_id",
        "multipart_created_at",
        "urls_expires_at",
        "destination_host",
    )
    parts = [f"{key}={context[key]}" for key in ordered_keys if context.get(key) is not None]
    return " " + " ".join(parts) if parts else ""


def http_status_detail(error: Exception) -> str:
    """Return HTTP status context for httpx exceptions when available."""
    if isinstance(error, httpx.HTTPStatusError):
        return f" status={error.response.status_code}"
    response = getattr(error, "response", None)
    status_code = getattr(response, "status_code", None)
    return f" status={status_code}" if status_code else ""


def api_key_headers(state: WorkerState) -> Dict[str, str]:
    """Build BeamCore API key headers when the worker has an issued key."""
    return {"X-Api-Key": state.api_key} if state.api_key else {}


RANGE_HEADER_RE = re.compile(r"^bytes=(\d+)-(\d+)$")


def offer_headers(value: Any) -> Dict[str, str]:
    """Return string-only offer headers."""
    if not isinstance(value, dict):
        return {}
    return {str(k): v for k, v in value.items() if isinstance(v, str)}


def parse_offer_range(headers: Dict[str, str]) -> Optional[tuple[int, int, int]]:
    """Parse the signed source Range header as start, end, length."""
    range_header = headers.get("Range") or headers.get("range")
    if not range_header:
        return None
    match = RANGE_HEADER_RE.fullmatch(range_header.strip())
    if not match:
        raise ValueError(f"invalid source Range header: {range_header!r}")
    start = int(match.group(1))
    end = int(match.group(2))
    if end < start:
        raise ValueError(f"invalid source Range header: {range_header!r}")
    return start, end, end - start + 1


def build_transfer_context(task: dict) -> tuple[Optional[dict], Optional[str]]:
    """Validate and normalize the flat worker task offer."""
    source_url = task.get("source_url")
    dest_url = task.get("dest_url")
    if not isinstance(source_url, str) or not source_url.strip():
        return None, "missing_source_url"
    if not isinstance(dest_url, str) or not dest_url.strip():
        return None, "missing_dest_url"

    try:
        chunk_size = int(task.get("chunk_size"))
    except (TypeError, ValueError):
        return None, "invalid_chunk_size"
    if chunk_size <= 0:
        return None, "invalid_chunk_size"

    source_headers = offer_headers(task.get("source_headers"))
    dest_headers = offer_headers(task.get("dest_headers"))
    minimum_worker_version = str(task.get("minimum_worker_version") or "").strip()
    if minimum_worker_version and not worker_version_satisfies(minimum_worker_version):
        return None, "unsupported_worker_version"
    signed_url_flow = str(task.get("signed_url_flow") or "").strip()
    if signed_url_flow == "signed_url_v1" and is_object_storage_presigned_url(dest_url):
        if not (dest_headers.get("Content-MD5") or dest_headers.get("content-md5")):
            return None, "missing_content_md5"
    try:
        parsed_range = parse_offer_range(source_headers)
    except ValueError as exc:
        return None, str(exc)
    if parsed_range is None:
        return None, "missing_source_range"
    range_start, range_end, range_size = parsed_range
    if range_size != chunk_size:
        return None, f"range_size_mismatch:{range_size}!={chunk_size}"

    return {
        "source_url": source_url.strip(),
        "dest_url": dest_url.strip(),
        "chunk_size": chunk_size,
        "range_start": range_start,
        "range_end": range_end,
        "source_headers": source_headers,
        "dest_headers": dest_headers,
        "signed_url_flow": signed_url_flow,
        "minimum_worker_version": minimum_worker_version,
        "transfer_id": str(task.get("transfer_id") or task.get("task_id") or ""),
        "etag_required": bool(task.get("etag_required")),
    }, None


def remaining_deadline_seconds(deadline_us: int) -> Optional[float]:
    """Return seconds until task deadline, or None when no deadline is set."""
    if deadline_us <= 0:
        return None
    return (deadline_us - time.time() * 1_000_000) / 1_000_000


async def execute_task_with_metrics(
    state: WorkerState,
    task_id: str,
    task: dict,
    transfer_context: dict,
    deadline_us: int,
    log_prefix: str = "[Worker]",
) -> TaskExecutionResult:
    """Execute a transfer task and produce the metrics required by BeamCore."""
    state.active_tasks += 1
    start_time = time.time()
    success = False
    bytes_transferred = 0
    error_msg: Optional[str] = None
    chunk_hash = ""
    etag: Optional[str] = None

    try:
        async with task_semaphore:
            remaining_sec = remaining_deadline_seconds(deadline_us)
            if remaining_sec is not None and remaining_sec < 2:
                error_msg = f"Deadline expired while waiting ({remaining_sec:.1f}s)"
                print(f"{log_prefix} {error_msg}")
            else:
                bytes_transferred, success, error_msg, chunk_hash, etag = await execute_transfer(
                    state,
                    task_id,
                    transfer_context,
                    task,
                    deadline_us,
                )
    except Exception as e:
        error_msg = str(e)
        print(f"{log_prefix} Task error: {e}")
    finally:
        state.active_tasks = max(0, state.active_tasks - 1)

    end_time = time.time()
    duration_ms = (end_time - start_time) * 1000
    return TaskExecutionResult(
        success=success,
        bytes_transferred=bytes_transferred,
        duration_ms=round(duration_ms, 1),
        chunk_hash=chunk_hash,
        etag=etag,
        error_msg=error_msg,
    )


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


def payment_evidence_message(
    worker_id: str,
    task_id: str,
    offer_id: str,
    chunk_hash: str = "",
) -> str:
    """Canonical message BeamCore verifies for worker payment evidence."""
    return ":".join(
        [
            "beam-worker-payment-evidence",
            worker_id,
            task_id,
            offer_id,
            chunk_hash or "",
        ]
    )


async def submit_worker_payment_evidence(
    state: WorkerState,
    task_id: str,
    offer_id: str,
    chunk_hash: str = "",
) -> bool:
    """Submit durable worker-signed payment evidence directly to BeamCore HTTP."""
    if not state.worker_id or not state.api_key:
        print("[Worker] Payment evidence skipped: missing worker_id or api_key")
        return False

    effective_offer = (offer_id or "").strip()
    if not effective_offer:
        print(
            "[Worker] Payment evidence skipped: missing offer_id "
            f"(task={task_label(task_id)}) — never substitute task_id for attempt UUID"
        )
        return False

    message = payment_evidence_message(
        state.worker_id,
        task_id,
        effective_offer,
        chunk_hash,
    )
    try:
        worker_signature = sign_message(state.wallet, message)
    except Exception as e:
        print(f"[Worker] Payment evidence signing failed: {e}")
        return False

    payload = {
        "offer_id": effective_offer,
        "success": True,
        "chunk_hash": chunk_hash or "",
        "worker_signature": worker_signature,
        "required_payment": WORKER_REQUIRED_PAYMENT,
    }
    url = f"{state.api_url.rstrip('/')}/workers/{state.worker_id}/tasks/{task_id}/payment-evidence"

    for attempt in range(3):
        try:
            async with httpx.AsyncClient(timeout=10.0) as client:
                response = await client.post(url, json=payload, headers=api_key_headers(state))
            if 200 <= response.status_code < 300:
                print(
                    f"[Worker] Payment evidence OK task={task_label(task_id)} "
                    f"offer={task_label(effective_offer)}"
                )
                return True
            print(
                f"[Worker] Payment evidence rejected attempt={attempt + 1}/3 "
                f"status={response.status_code} task={task_label(task_id)} "
                f"offer={task_label(effective_offer)}"
            )
        except Exception as e:
            print(f"[Worker] Payment evidence submit error attempt={attempt + 1}/3: {e}")
        await asyncio.sleep(1 + attempt)

    print(
        f"[Worker] Payment evidence FAILED after retries task_id={task_id} "
        f"offer_id={effective_offer} worker_id={state.worker_id}"
    )
    return False


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
        print("[Worker] Signed registration message")
    except Exception as e:
        raise Exception(f"Failed to sign registration: {e}")

    payload = {
        "hotkey": hotkey,
        "ip": ip,
        "port": port,
        "claimed_bandwidth_mbps": 100,
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
                error = (
                    data.get("error")
                    or data.get("detail")
                    or data.get("message")
                    or f"Registration failed: {data}"
                )
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
    expected_max_bytes: int = None,
    task_id: str = None,
    offer_id: str = None,
    chunk_index: int = None,
    offer_source_headers: dict = None,
) -> bytes:
    """Fetch chunk data from source URL."""
    headers = {"ngrok-skip-browser-warning": "true"}

    if offer_source_headers:
        headers.update(offer_source_headers)

    for attempt in range(MAX_RETRIES):
        try:
            async with client.stream(
                "GET", url, headers=headers, timeout=FETCH_TIMEOUT
            ) as response:
                if response.status_code not in (200, 206):
                    response.raise_for_status()

                if expected_max_bytes and expected_max_bytes > 0:
                    content_length = response.headers.get("Content-Length")
                    if content_length:
                        response_size = int(content_length)
                        if response_size > expected_max_bytes:
                            raise ValueError(
                                f"response too large: {response_size} bytes > expected {expected_max_bytes}"
                            )

                data = bytearray()
                async for chunk in response.aiter_bytes(chunk_size=FETCH_STREAM_CHUNK_SIZE):
                    data.extend(chunk)
                    if (
                        expected_max_bytes
                        and expected_max_bytes > 0
                        and len(data) > expected_max_bytes
                    ):
                        raise ValueError(
                            f"response exceeded expected size while streaming: "
                            f"{len(data)} bytes > expected {expected_max_bytes}"
                        )

                return bytes(data)

        except Exception as e:
            if not is_retryable(e) or attempt == MAX_RETRIES - 1:
                raise
            print(
                "[Worker] Fetch retry "
                f"task={task_label(task_id)} offer={task_label(offer_id)} "
                f"chunk={chunk_index if chunk_index is not None else 'unknown'} "
                f"attempt={attempt + 1}/{MAX_RETRIES} "
                f"error={exception_detail(e)}{http_status_detail(e)}"
            )
            await asyncio.sleep(RETRY_BACKOFF * (2**attempt))

    raise Exception("Max retries exceeded")


def is_object_storage_presigned_url(url: str) -> bool:
    """Check if URL is an object-storage pre-signed upload URL."""
    if not url:
        return False
    return (
        "X-Amz-Signature" in url
        or "X-Goog-Signature" in url
        or "r2.cloudflarestorage.com" in url
        or "storage.googleapis.com" in url
    )


def is_canary_destination(url: str) -> bool:
    """Check if URL is a canary/null destination."""
    if not url:
        return False
    return url.startswith(("null://", "canary://", "skip://"))


def estimate_task_bytes(task: dict) -> int:
    """Estimate how many bytes this task can hold in memory."""
    raw_chunk_size = task.get("chunk_size") or DEFAULT_CHUNK_SIZE_BYTES

    try:
        chunk_size = max(1, int(raw_chunk_size))
    except (TypeError, ValueError):
        chunk_size = DEFAULT_CHUNK_SIZE_BYTES

    return chunk_size


async def ws_send_task_reject(
    websocket,
    state: WorkerState,
    task_id: str,
    reason: str,
    offer_id: str = None,
) -> bool:
    """Reject a WebSocket task offer so BeamCore can reassign it quickly."""
    try:
        msg = {
            "type": "task_reject",
            "offer_id": offer_id or task_id,
            "task_id": task_id,
            "worker_id": state.worker_id,
            "reason": reason,
        }
        await ws_send_json(websocket, state, msg)
        return True
    except Exception as e:
        print(f"[Worker] WS task_reject error: {e}")
        return False


def try_reserve_ws_capacity(
    state: WorkerState, task_id: str, estimated_bytes: int
) -> Optional[str]:
    """Reserve local capacity for a pushed task before accepting it."""
    if task_id in state.active_ws_task_ids:
        return "duplicate"

    if estimated_bytes > MAX_IN_FLIGHT_BYTES:
        return f"task_too_large:{estimated_bytes}"

    if state.reserved_ws_slots >= MAX_QUEUED_WS_TASKS:
        return f"queue_full:{state.reserved_ws_slots}"

    if state.reserved_bytes + estimated_bytes > MAX_IN_FLIGHT_BYTES:
        return f"memory_budget:{state.reserved_bytes + estimated_bytes}"

    state.active_ws_task_ids.add(task_id)
    state.reserved_ws_slots += 1
    state.reserved_bytes += estimated_bytes
    return None


def release_ws_capacity(
    state: WorkerState, task_id: str, estimated_bytes: int, reserved: bool
) -> None:
    """Release capacity reserved for a pushed task."""
    if reserved:
        state.reserved_ws_slots = max(0, state.reserved_ws_slots - 1)
        state.reserved_bytes = max(0, state.reserved_bytes - max(0, estimated_bytes))
    state.active_ws_task_ids.discard(task_id)


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
    """Send chunk data to destination URL.

    Returns: (success, etag, response_code)
    """
    is_object_storage = is_object_storage_presigned_url(destination_url)
    route_context = object_storage_route_context(destination_url, route_metadata) if is_object_storage else {}

    if is_object_storage and len(data) == 0:
        raise ValueError(
            f"[Worker] Refusing 0-byte staging PUT to object storage for chunk {chunk_index}"
        )

    if is_object_storage:
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

    if offer_dest_headers:
        headers.update(offer_dest_headers)

    for attempt in range(MAX_RETRIES):
        try:
            if is_object_storage:
                response = await client.put(
                    destination_url, content=data, headers=headers, timeout=SEND_TIMEOUT
                )
            else:
                response = await client.post(
                    destination_url, content=data, headers=headers, timeout=SEND_TIMEOUT
                )

            response.raise_for_status()
            etag = response.headers.get("ETag") or response.headers.get("etag")
            if is_object_storage:
                print(
                    f"[Worker] Staging PUT ok chunk={chunk_index} "
                    f"bytes={len(data)} etag={etag!r}"
                )
            return (True, etag, response.status_code)

        except Exception as e:
            # Presigned object-storage 404s may be transient Cloudflare routing issues.
            # Allow up to 2 retries (2 s, 4 s backoff) before giving up.
            is_transient_storage_404 = (
                is_object_storage
                and isinstance(e, httpx.HTTPStatusError)
                and e.response.status_code == 404
                and attempt < 2
            )
            can_retry = is_retryable(e) or is_transient_storage_404
            if is_object_storage and (not can_retry or attempt == MAX_RETRIES - 1):
                print(
                    "[Worker] Object storage upload failed "
                    f"task={task_label(task_id)} offer={task_label(offer_id)} "
                    f"chunk={chunk_index} error={exception_detail(e)}{http_status_detail(e)}"
                    f"{format_route_context(route_context)}"
                )
            if not can_retry or attempt == MAX_RETRIES - 1:
                raise
            print(
                "[Worker] Send retry "
                f"task={task_label(task_id)} offer={task_label(offer_id)} "
                f"chunk={chunk_index} attempt={attempt + 1}/{MAX_RETRIES} "
                f"error={exception_detail(e)}{http_status_detail(e)}"
            )
            await asyncio.sleep(RETRY_BACKOFF * (2**attempt))

    raise Exception("Max retries exceeded")


async def execute_transfer(
    state: WorkerState,
    task_id: str,
    transfer_context: dict,
    task_message: dict,
    deadline_us: int,
) -> tuple:
    """Execute real data transfer: fetch from source, send to destination.

    Returns: (bytes_transferred, success, error_message, chunk_hash, etag)
    """
    source_url = transfer_context["source_url"]
    destination_url = transfer_context["dest_url"]
    transfer_id = transfer_context.get("transfer_id", "")
    chunk_size = int(transfer_context["chunk_size"])
    range_start = int(transfer_context["range_start"])
    range_end = int(transfer_context["range_end"])
    source_headers_offer = transfer_context.get("source_headers") or {}
    dest_headers_offer = transfer_context.get("dest_headers") or {}
    chunk_index = 0

    # Build per-chunk hash map
    chunk_hashes: dict = {}
    if "chunk_hashes" in task_message and isinstance(task_message["chunk_hashes"], dict):
        for k, v in task_message["chunk_hashes"].items():
            chunk_hashes[int(k)] = v
    elif "chunk_hash" in task_message and task_message["chunk_hash"]:
        chunk_hashes[chunk_index] = task_message["chunk_hash"]

    client = state.http_client
    total_bytes = 0
    is_canary = is_canary_destination(destination_url)
    computed_chunk_hash = ""
    last_etag: Optional[str] = None
    offer_id = task_message.get("offer_id") or task_id
    hotkey = getattr(getattr(state.wallet, "hotkey", None), "ss58_address", "unknown")

    print(
        f"[Worker] Transferring signed range bytes={range_start}-{range_end} "
        f"task={task_label(task_id)} offer={task_label(offer_id)} hotkey={hotkey[:16]}"
    )

    try:
        # Check deadline
        if deadline_us > 0:
            now_us = time.time() * 1_000_000
            remaining_us = deadline_us - now_us
            if remaining_us <= 0:
                return (
                    total_bytes,
                    False,
                    f"Deadline exceeded before chunk {chunk_index}",
                    "",
                    last_etag,
                )

        chunk_started = time.perf_counter()
        fetch_started = time.perf_counter()
        data = await fetch_chunk(
            client,
            source_url,
            expected_max_bytes=chunk_size,
            task_id=task_id,
            offer_id=offer_id,
            chunk_index=chunk_index,
            offer_source_headers=source_headers_offer or None,
        )
        fetch_ms = (time.perf_counter() - fetch_started) * 1000

        bytes_fetched = len(data)
        if bytes_fetched != chunk_size:
            return (
                total_bytes,
                False,
                f"source range returned {bytes_fetched} bytes, expected {chunk_size}",
                "",
                last_etag,
            )

        hash_started = time.perf_counter()
        computed_chunk_hash = hashlib.sha256(data).hexdigest()
        hash_ms = (time.perf_counter() - hash_started) * 1000

        expected_chunk_hash = chunk_hashes.get(chunk_index) or ""
        if (
            expected_chunk_hash
            and computed_chunk_hash
            and expected_chunk_hash.lower() != computed_chunk_hash.lower()
        ):
            return (
                total_bytes,
                False,
                f"Chunk {chunk_index} hash mismatch",
                computed_chunk_hash,
                last_etag,
            )

        if is_canary:
            print(f"[Worker] Chunk {chunk_index}: CANARY mode, skipping upload")
            total_bytes += bytes_fetched
        else:
            send_started = time.perf_counter()
            _send_success, etag, response_code = await send_chunk(
                client,
                destination_url,
                data,
                transfer_id,
                chunk_index,
                chunk_offset=range_start,
                total_size=chunk_size,
                task_id=task_id,
                offer_id=offer_id,
                offer_dest_headers=dest_headers_offer or None,
            )
            send_ms = (time.perf_counter() - send_started) * 1000
            if etag:
                last_etag = etag

            total_bytes += bytes_fetched
            total_ms = (time.perf_counter() - chunk_started) * 1000
            mbps = (bytes_fetched * 8 / 1_000_000) / (total_ms / 1000) if total_ms > 0 else 0
            print(
                f"[Worker] Chunk {chunk_index}: {bytes_fetched} bytes transferred "
                f"task={task_label(task_id)} offer={task_label(offer_id)} "
                f"fetch_ms={fetch_ms:.1f} hash_ms={hash_ms:.1f} send_ms={send_ms:.1f} "
                f"total_ms={total_ms:.1f} mbps={mbps:.1f} response={response_code}"
            )

    except asyncio.TimeoutError as e:
        detail = exception_detail(e)
        print(
            f"[Worker] Chunk {chunk_index} timeout "
            f"task={task_label(task_id)} offer={task_label(offer_id)} error={detail}"
        )
        return (
            total_bytes,
            False,
            f"Deadline exceeded at chunk {chunk_index}: {detail}",
            "",
            last_etag,
        )
    except httpx.HTTPStatusError as e:
        detail = exception_detail(e)
        print(
            f"[Worker] Chunk {chunk_index} HTTP failure "
            f"task={task_label(task_id)} offer={task_label(offer_id)} "
            f"status={e.response.status_code} error={detail}"
        )
        return (
            total_bytes,
            False,
            f"HTTP {e.response.status_code} at chunk {chunk_index}: {detail}",
            "",
            last_etag,
        )
    except Exception as e:
        detail = exception_detail(e)
        print(
            f"[Worker] Chunk {chunk_index} failure "
            f"task={task_label(task_id)} offer={task_label(offer_id)} error={detail}{http_status_detail(e)}"
        )
        return (total_bytes, False, f"Error at chunk {chunk_index}: {detail}", "", last_etag)

    if transfer_context.get("etag_required") and not last_etag:
        return (
            total_bytes,
            False,
            "missing ETag from storage PUT response",
            computed_chunk_hash or "",
            last_etag,
        )

    print(f"[Worker] Transfer complete: {total_bytes} bytes")
    return (total_bytes, True, None, computed_chunk_hash, last_etag)


# =============================================================================
# WebSocket Communication
# =============================================================================


def get_ws_url(worker_id: str, api_key: str, gateway_url: str) -> str:
    """Convert worker gateway URL to the worker WebSocket URL."""
    base = gateway_url.rstrip("/")
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


def get_ws_status_code(exc: Exception) -> Optional[int]:
    """Extract an HTTP status code from websocket handshake failures."""
    status_code = getattr(exc, "status_code", None)
    if isinstance(status_code, int):
        return status_code

    response = getattr(exc, "response", None)
    response_status = getattr(response, "status_code", None)
    if isinstance(response_status, int):
        return response_status

    message = str(exc)
    for token in message.split():
        stripped = token.rstrip(":,)")
        if stripped.isdigit():
            value = int(stripped)
            if 100 <= value <= 599:
                return value
    return None


async def ws_send_task_accept(
    websocket, state: WorkerState, task_id: str, offer_id: str = None
) -> bool:
    """Send task acceptance over WebSocket."""
    try:
        msg = {
            "type": "task_accept",
            "offer_id": offer_id or task_id,
            "task_id": task_id,
            "worker_id": state.worker_id,
            "worker_version": WORKER_VERSION,
        }
        await ws_send_json(websocket, state, msg)
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
    chunk_hash: str = "",
    etag: str = None,
    error: str = None,
    offer_id: str = None,
) -> bool:
    """Send task completion receipt over WebSocket."""
    try:
        msg = {
            "type": "task_result",
            "task_id": task_id,
            "offer_id": offer_id or task_id,
            "worker_id": state.worker_id,
            "success": success,
        }
        if chunk_hash:
            msg["chunk_hash"] = chunk_hash
        if etag:
            msg["etag"] = etag
        if error:
            msg["error"] = error
        await ws_send_json(websocket, state, msg)
        return True
    except Exception as e:
        print(f"[Worker] WS task_result error: {e}")
        return False


async def finalize_ws_task_result(
    websocket,
    state: WorkerState,
    task_id: str,
    success: bool,
    bytes_transferred: int,
    chunk_hash: str = "",
    etag: str = None,
    error: str = None,
    offer_id: str = None,
) -> TaskSummaryAck:
    """Send task_result and wait for BeamCore ack (received / completed)."""
    result_key = offer_id or task_id
    empty = TaskSummaryAck()

    for attempt in range(3):
        ack_future: asyncio.Future = asyncio.get_event_loop().create_future()
        state.pending_task_results[result_key] = ack_future

        try:
            sent = await ws_send_task_result(
                websocket,
                state,
                task_id,
                success,
                bytes_transferred,
                chunk_hash=chunk_hash,
                etag=etag,
                error=error,
                offer_id=offer_id,
            )
            if not sent:
                continue

            try:
                ack = await asyncio.wait_for(ack_future, timeout=WS_TASK_RESULT_ACK_TIMEOUT)
                if ack.completed:
                    print(
                        f"[Worker] [WS] Task completed on BeamCore: {task_label(task_id)} offer={task_label(offer_id)}"
                    )
                    return ack
                if ack.received:
                    reason = ack.reason or "not_completed"
                    print(
                        f"[Worker] [WS] Task result received but not completed: "
                        f"task={task_label(task_id)} offer={task_label(offer_id)} reason={reason}"
                    )
                    return ack
                print(
                    f"[Worker] [WS] Task result nack from gateway: {task_label(task_id)} offer={task_label(offer_id)}"
                )
            except asyncio.TimeoutError:
                print(
                    f"[Worker] [WS] Task result ack timeout "
                    f"attempt={attempt + 1}/3 task={task_label(task_id)} offer={task_label(offer_id)}"
                )
        finally:
            state.pending_task_results.pop(result_key, None)

    print(
        f"[Worker] [WS] Task result failed after websocket retries: {task_label(task_id)} offer={task_label(offer_id)}"
    )
    return empty


async def ws_send_json(websocket, state: WorkerState, payload: dict) -> None:
    """Serialize worker websocket sends to avoid concurrent-send races."""
    if state.ws_send_lock is None:
        state.ws_send_lock = asyncio.Lock()

    async with state.ws_send_lock:
        await websocket.send(json.dumps(payload))


def track_ws_task(state: WorkerState, coro: asyncio.coroutines) -> None:
    """Track spawned WS task handlers so they are not dropped on loop exit."""
    task = asyncio.create_task(coro)
    state.ws_task_handles.add(task)

    def _on_done(done_task: asyncio.Task) -> None:
        state.ws_task_handles.discard(done_task)
        try:
            exc = done_task.exception()
        except asyncio.CancelledError:
            return
        if exc is not None:
            print(f"[Worker] [WS] Task handler crashed: {type(exc).__name__}: {exc}")

    task.add_done_callback(_on_done)


async def handle_ws_task(state: WorkerState, websocket, task: dict) -> bool:
    """Handle a task received via WebSocket push."""
    task_id = task.get("task_id") or task.get("offer_id")
    offer_id = task.get("offer_id") or task_id
    task_key = offer_id or task_id
    deadline_us = task.get("deadline_us", 0)
    transfer_context, validation_error = build_transfer_context(task)
    estimated_bytes = estimate_task_bytes(task)
    reserved_capacity = False

    print(f"[Worker] [WS] Task: {task_label(task_id)} offer={task_label(offer_id)}...")
    if not task_id:
        print("[Worker] [WS] Skipping task: missing task_id")
        return False
    if validation_error or transfer_context is None:
        reason = validation_error if validation_error == "unsupported_worker_version" else f"invalid_offer:{validation_error or 'unknown'}"
        await ws_send_task_reject(websocket, state, task_id, reason, offer_id=offer_id)
        print(
            f"[Worker] [WS] Rejected task {task_label(task_id)} offer={task_label(offer_id)}: "
            f"{reason}"
        )
        return False

    capacity_error = try_reserve_ws_capacity(state, task_key, estimated_bytes)
    if capacity_error == "duplicate":
        print(
            f"[Worker] [WS] Duplicate task offer ignored: {task_label(task_id)} offer={task_label(offer_id)}"
        )
        return False
    if capacity_error:
        await ws_send_task_reject(websocket, state, task_id, capacity_error, offer_id=offer_id)
        print(
            f"[Worker] [WS] Rejected task {task_label(task_id)} offer={task_label(offer_id)} "
            f"due to capacity guard: {capacity_error} (budget={MAX_IN_FLIGHT_BYTES} bytes)"
        )
        return False
    reserved_capacity = True

    try:
        remaining_sec = remaining_deadline_seconds(deadline_us)
        if remaining_sec is not None and remaining_sec < 5:
            print(f"[Worker] [WS] Skipping task: deadline too close ({remaining_sec:.1f}s)")
            return False

        # Register a future to wait for the server's accept ack before executing
        accept_future: asyncio.Future = asyncio.get_event_loop().create_future()
        state.pending_task_accepts[task_key] = accept_future

        accepted = await ws_send_task_accept(websocket, state, task_id, offer_id=offer_id)
        if not accepted:
            state.pending_task_accepts.pop(task_key, None)
            print("[Worker] [WS] Failed to send task accept")
            return False

        # Execute only after BeamCore confirms this exact attempt lease.
        try:
            server_accepted = await asyncio.wait_for(accept_future, timeout=5.0)
            if not server_accepted:
                print(
                    f"[Worker] [WS] Task accept rejected by server: task={task_label(task_id)} offer={task_label(offer_id)}"
                )
                return False
        except asyncio.TimeoutError:
            state.pending_task_accepts.pop(task_key, None)
            print(
                f"[Worker] [WS] Accept ack timeout, aborting: task={task_label(task_id)} offer={task_label(offer_id)}"
            )
            return False

        result = await execute_task_with_metrics(
            state,
            task_id,
            task,
            transfer_context,
            deadline_us,
            log_prefix="[Worker] [WS]",
        )

        summary_ack = await finalize_ws_task_result(
            websocket,
            state,
            task_id,
            result.success,
            result.bytes_transferred,
            chunk_hash=result.chunk_hash,
            etag=result.etag,
            error=result.error_msg,
            offer_id=offer_id,
        )

        if result.success and summary_ack.completed:
            await submit_worker_payment_evidence(
                state,
                task_id,
                offer_id,
                chunk_hash=result.chunk_hash,
            )

        status = "OK" if result.success else f"FAIL: {result.error_msg}"
        print(
            f"[Worker] [WS] Task {task_label(task_id)} offer={task_label(offer_id)}: {status} | "
            f"{result.bytes_transferred} bytes"
        )

        return result.success
    finally:
        state.pending_task_accepts.pop(task_key, None)
        release_ws_capacity(state, task_key, estimated_bytes, reserved_capacity)


async def websocket_loop(state: WorkerState):
    """WebSocket communication loop with automatic reconnection."""
    if not WEBSOCKETS_AVAILABLE:
        raise RuntimeError("websockets library is required for worker gateway transport")

    if not state.worker_gateway_url:
        raise RuntimeError("WORKER_GATEWAY_URL is required for worker gateway transport")

    ws_url = get_ws_url(state.worker_id, state.api_key, state.worker_gateway_url)
    print(f"[Worker] Connecting to WebSocket: {ws_url.split('?')[0]}")
    reconnect_delay = WS_RECONNECT_MIN_DELAY

    while state.running and state.use_websocket:
        try:
            async with websockets.connect(
                ws_url,
                ping_interval=WS_PING_INTERVAL,
                ping_timeout=10,
                close_timeout=5,
            ) as websocket:
                state.ws_connected = True
                state.ws_reconnect_attempts = 0
                reconnect_delay = WS_RECONNECT_MIN_DELAY
                print("[Worker] [WS] Connected!")

                while state.running:
                    try:
                        try:
                            msg_str = await asyncio.wait_for(
                                websocket.recv(),
                                timeout=WS_PING_INTERVAL,
                            )
                            message = json.loads(msg_str)
                            msg_type = message.get("type")

                            if msg_type == "connected":
                                print("[Worker] [WS] Server confirmed connection")

                            elif msg_type == "task_offer":
                                track_ws_task(state, handle_ws_task(state, websocket, message))

                            elif msg_type == "task_accept_ack":
                                ack_task_id = message.get("task_id")
                                ack_offer_id = message.get("offer_id") or ack_task_id
                                server_accepted = message.get("accepted", True)
                                if ack_offer_id and ack_offer_id in state.pending_task_accepts:
                                    future = state.pending_task_accepts.pop(ack_offer_id)
                                    if not future.done():
                                        future.set_result(server_accepted)
                                if not server_accepted:
                                    print(
                                        f"[Worker] [WS] Task accept rejected: "
                                        f"task={task_label(ack_task_id)} offer={task_label(ack_offer_id)} "
                                        f"reason={message.get('reason', 'unknown')}"
                                    )

                            elif msg_type == "task_result_ack":
                                ack_task_id = message.get("task_id")
                                ack_offer_id = message.get("offer_id") or ack_task_id
                                received = bool(message.get("received", False))
                                completed_raw = message.get("completed")
                                completed = (
                                    bool(completed_raw)
                                    if completed_raw is not None
                                    else received
                                )
                                reason = message.get("reason")
                                ack = TaskSummaryAck(
                                    received=received,
                                    completed=completed,
                                    reason=str(reason) if reason else None,
                                )
                                if ack_offer_id and ack_offer_id in state.pending_task_results:
                                    future = state.pending_task_results.pop(ack_offer_id)
                                    if not future.done():
                                        future.set_result(ack)
                                if not received:
                                    print(
                                        f"[Worker] [WS] Gateway rejected task_result: "
                                        f"task={task_label(ack_task_id)} offer={task_label(ack_offer_id)}"
                                    )

                            elif msg_type == "error":
                                print(
                                    f"[Worker] [WS] Server error: {message.get('message', 'unknown')}"
                                )

                        except asyncio.TimeoutError:
                            pass

                    except ConnectionClosed as e:
                        print(f"[Worker] [WS] Connection closed: {e.code} {e.reason}")
                        break

        except InvalidStatus as e:
            print(f"[Worker] [WS] Connection rejected: HTTP {e.status_code}")
            raise RuntimeError(
                f"worker gateway websocket rejected the connection with HTTP {e.status_code}"
            ) from e

        except ConnectionRefusedError:
            print("[Worker] [WS] Connection refused")

        except Exception as e:
            print(f"[Worker] [WS] Connection error: {type(e).__name__}: {e}")

        state.ws_connected = False
        state.ws_reconnect_attempts += 1

        if (
            WS_MAX_RECONNECT_ATTEMPTS is not None
            and state.ws_reconnect_attempts >= WS_MAX_RECONNECT_ATTEMPTS
        ):
            raise RuntimeError(
                "worker gateway websocket unavailable after maximum reconnect attempts"
            )

        if state.running and not shutdown_event.is_set():
            if WS_MAX_RECONNECT_ATTEMPTS is None:
                print(
                    f"[Worker] [WS] Reconnecting in {reconnect_delay:.1f}s (attempt {state.ws_reconnect_attempts})..."
                )
            else:
                print(
                    f"[Worker] [WS] Reconnecting in {reconnect_delay:.1f}s (attempt {state.ws_reconnect_attempts}/{WS_MAX_RECONNECT_ATTEMPTS})..."
                )
            try:
                await asyncio.wait_for(shutdown_event.wait(), timeout=reconnect_delay)
                break
            except asyncio.TimeoutError:
                pass
            reconnect_delay = min(reconnect_delay * WS_RECONNECT_MULTIPLIER, WS_RECONNECT_MAX_DELAY)

    state.ws_connected = False
    print("[Worker] [WS] Loop stopped")


# =============================================================================
# Main
# =============================================================================


shutdown_event = asyncio.Event()


async def run_worker(state: WorkerState):
    """Run the worker."""
    wallet = state.wallet
    hotkey = wallet.hotkey.ss58_address
    if state.ws_send_lock is None:
        state.ws_send_lock = asyncio.Lock()

    # Create HTTP client
    state.http_client = httpx.AsyncClient(
        timeout=httpx.Timeout(connect=10.0, read=60.0, write=60.0, pool=5.0),
        limits=httpx.Limits(
            max_connections=max(8, MAX_CONCURRENT_TASKS * 4),
            max_keepalive_connections=max(4, MAX_CONCURRENT_TASKS * 2),
        ),
    )

    try:
        async with httpx.AsyncClient() as client:
            # Register with SubnetCore
            print("[Worker] Registering with SubnetCore...")
            print(f"[Worker] Hotkey: {hotkey}")
            print(f"[Worker] API URL: {state.api_url}")

            result = await register_worker(client, state)
            state.worker_id = result.get("worker_id")
            state.api_key = result.get("api_key")
            print(f"[Worker] Registered: {state.worker_id}")

        if CONNECTION_MODE not in {"websocket", "auto"}:
            raise RuntimeError("Worker transport is websocket-only; remove CONNECTION_MODE=http")
        if not WEBSOCKETS_AVAILABLE:
            raise RuntimeError("websockets library is required for worker gateway transport")
        if not state.worker_gateway_url:
            raise RuntimeError("WORKER_GATEWAY_URL must point to an orchestrator-owned worker gateway")

        print("[Worker] Starting WebSocket connection (worker gateway transport)")
        await websocket_loop(state)

    except asyncio.CancelledError:
        print("[Worker] Cancelled")
    except Exception as e:
        print(f"[Worker] Error: {e}")
        raise
    finally:
        if state.ws_task_handles:
            print(f"[Worker] Waiting for {len(state.ws_task_handles)} active WS task(s) to finish")
            await asyncio.gather(*list(state.ws_task_handles), return_exceptions=True)
        if state.http_client:
            await state.http_client.aclose()
            state.http_client = None

    print("[Worker] Stopped")


def get_config():
    """Get configuration from command line arguments."""
    parser = argparse.ArgumentParser(description="Beam Network Worker")

    # Bittensor wallet arguments
    bt.Wallet.add_args(parser)
    bt.Subtensor.add_args(parser)

    # Parse arguments
    config = bt.Config(parser)
    return config


def apply_env_config_overrides(config):
    """Apply deployment env overrides after Bittensor parses its defaults."""
    overrides = (
        ("WALLET_NAME", "wallet", "name"),
        ("WALLET_HOTKEY", "wallet", "hotkey"),
        ("WALLET_PATH", "wallet", "path"),
        ("SUBTENSOR_NETWORK", "subtensor", "network"),
    )
    for env_name, section_name, key in overrides:
        value = os.environ.get(env_name)
        if not value:
            continue
        section = getattr(config, section_name, None)
        if section is None:
            section = {}
            setattr(config, section_name, section)
        if isinstance(section, dict):
            section[key] = value
        else:
            setattr(section, key, value)


async def main():
    """Main entry point."""
    print("Beam Network Worker")
    print("=" * 40)

    # Parse configuration
    config = get_config()
    apply_env_config_overrides(config)

    # Load bittensor wallet
    wallet = bt.Wallet(config=config)
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
    if network not in ("finney", "mainnet"):
        api_url = os.environ.get("CORE_SERVER_URL")
        if not api_url:
            raise RuntimeError("CORE_SERVER_URL is required when running the public worker on a non-mainnet network")
        print(f"Network: {network}")
    else:
        api_url = os.environ.get("CORE_SERVER_URL", MAINNET_URL)
        print("Network: mainnet")
    worker_gateway_url = os.environ.get("WORKER_GATEWAY_URL")

    print(f"API URL: {api_url}")
    if worker_gateway_url:
        print(f"Worker gateway URL: {worker_gateway_url}")
    else:
        print("Worker gateway URL: MISSING")
    print(
        f"Worker limits: concurrency={MAX_CONCURRENT_TASKS}, "
        f"ws_queue={MAX_QUEUED_WS_TASKS}, "
        f"in_flight={MAX_IN_FLIGHT_BYTES // (1024 * 1024)} MiB"
    )
    print()

    # Create worker state
    state = WorkerState(wallet=wallet, api_url=api_url, worker_gateway_url=worker_gateway_url)

    # Setup signal handlers
    loop = asyncio.get_running_loop()

    def handle_shutdown():
        print("\nShutting down worker...")
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
