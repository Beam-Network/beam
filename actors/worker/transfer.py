"""HTTP transfer execution for the Beam worker.

Dependencies are injectable so ``worker.py`` can preserve its historical
public monkeypatch seams while the implementation lives here.
"""

import asyncio
import hashlib
import time
from typing import Any, Awaitable, Callable, Dict, Optional

import httpx

try:
    from .models import TaskExecutionResult, WorkerState
    from .transfer_context import (
        exception_detail,
        format_route_context,
        http_status_detail,
        is_canary_destination,
        is_object_storage_presigned_url,
        is_retryable,
        object_storage_route_context,
        remaining_deadline_seconds,
        task_label,
    )
except ImportError:  # Direct execution through worker.py
    from models import TaskExecutionResult, WorkerState
    from transfer_context import (
        exception_detail,
        format_route_context,
        http_status_detail,
        is_canary_destination,
        is_object_storage_presigned_url,
        is_retryable,
        object_storage_route_context,
        remaining_deadline_seconds,
        task_label,
    )


FETCH_TIMEOUT = 30
SEND_TIMEOUT = 30
MAX_RETRIES = 3
RETRY_BACKOFF = 1.0
FETCH_STREAM_CHUNK_SIZE = 64 * 1024

FetchChunk = Callable[..., Awaitable[bytes]]
SendChunk = Callable[..., Awaitable[tuple]]
ExecuteTransfer = Callable[..., Awaitable[tuple]]


async def execute_task_with_metrics(
    state: WorkerState,
    task_id: str,
    task: dict,
    transfer_context: dict,
    deadline_us: int,
    log_prefix: str = "[Worker]",
    *,
    execute_transfer_fn: Optional[ExecuteTransfer] = None,
) -> TaskExecutionResult:
    """Execute a transfer task and produce the metrics required by BeamCore."""
    transfer_executor = execute_transfer_fn or execute_transfer
    state.active_tasks += 1
    start_time = time.time()
    success = False
    bytes_transferred = 0
    error_msg: Optional[str] = None
    chunk_hash = ""
    etag: Optional[str] = None

    try:
        remaining_sec = remaining_deadline_seconds(deadline_us)
        if remaining_sec is not None and remaining_sec < 2:
            error_msg = f"Deadline expired while waiting ({remaining_sec:.1f}s)"
            print(f"{log_prefix} {error_msg}")
        else:
            bytes_transferred, success, error_msg, chunk_hash, etag = await transfer_executor(
                state, task_id, transfer_context, task, deadline_us
            )
    except Exception as error:
        error_msg = str(error)
        print(f"{log_prefix} Task error: {error}")
    finally:
        state.active_tasks = max(0, state.active_tasks - 1)

    duration_ms = (time.time() - start_time) * 1000
    return TaskExecutionResult(
        success=success,
        bytes_transferred=bytes_transferred,
        duration_ms=round(duration_ms, 1),
        chunk_hash=chunk_hash,
        etag=etag,
        error_msg=error_msg,
    )


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
            async with client.stream("GET", url, headers=headers, timeout=FETCH_TIMEOUT) as response:
                if response.status_code not in (200, 206):
                    response.raise_for_status()
                if expected_max_bytes and expected_max_bytes > 0:
                    content_length = response.headers.get("Content-Length")
                    if content_length and int(content_length) > expected_max_bytes:
                        raise ValueError(
                            f"response too large: {int(content_length)} bytes > expected {expected_max_bytes}"
                        )

                data = bytearray()
                async for chunk in response.aiter_bytes(chunk_size=FETCH_STREAM_CHUNK_SIZE):
                    data.extend(chunk)
                    if expected_max_bytes and expected_max_bytes > 0 and len(data) > expected_max_bytes:
                        raise ValueError(
                            "response exceeded expected size while streaming: "
                            f"{len(data)} bytes > expected {expected_max_bytes}"
                        )
                return bytes(data)
        except Exception as error:
            if not is_retryable(error) or attempt == MAX_RETRIES - 1:
                raise
            print(
                "[Worker] Fetch retry "
                f"task={task_label(task_id)} offer={task_label(offer_id)} "
                f"chunk={chunk_index if chunk_index is not None else 'unknown'} "
                f"attempt={attempt + 1}/{MAX_RETRIES} "
                f"error={exception_detail(error)}{http_status_detail(error)}"
            )
            await asyncio.sleep(RETRY_BACKOFF * (2**attempt))

    raise Exception("Max retries exceeded")


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
    """Send chunk data and return success, ETag, and response status."""
    is_object_storage = is_object_storage_presigned_url(destination_url)
    route_context = object_storage_route_context(destination_url, route_metadata) if is_object_storage else {}
    if is_object_storage and len(data) == 0:
        raise ValueError(f"[Worker] Refusing 0-byte staging PUT to object storage for chunk {chunk_index}")

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
                response = await client.put(destination_url, content=data, headers=headers, timeout=SEND_TIMEOUT)
            else:
                response = await client.post(destination_url, content=data, headers=headers, timeout=SEND_TIMEOUT)
            response.raise_for_status()
            etag = response.headers.get("ETag") or response.headers.get("etag")
            if is_object_storage:
                print(f"[Worker] Staging PUT ok chunk={chunk_index} bytes={len(data)} etag={etag!r}")
            return (True, etag, response.status_code)
        except Exception as error:
            transient_storage_404 = (
                is_object_storage
                and isinstance(error, httpx.HTTPStatusError)
                and error.response.status_code == 404
                and attempt < 2
            )
            can_retry = is_retryable(error) or transient_storage_404
            if is_object_storage and (not can_retry or attempt == MAX_RETRIES - 1):
                print(
                    "[Worker] Object storage upload failed "
                    f"task={task_label(task_id)} offer={task_label(offer_id)} "
                    f"chunk={chunk_index} error={exception_detail(error)}{http_status_detail(error)}"
                    f"{format_route_context(route_context)}"
                )
            if not can_retry or attempt == MAX_RETRIES - 1:
                raise
            print(
                "[Worker] Send retry "
                f"task={task_label(task_id)} offer={task_label(offer_id)} "
                f"chunk={chunk_index} attempt={attempt + 1}/{MAX_RETRIES} "
                f"error={exception_detail(error)}{http_status_detail(error)}"
            )
            await asyncio.sleep(RETRY_BACKOFF * (2**attempt))

    raise Exception("Max retries exceeded")


async def execute_transfer(
    state: WorkerState,
    task_id: str,
    transfer_context: dict,
    task_message: dict,
    deadline_us: int,
    *,
    fetch_chunk_fn: Optional[FetchChunk] = None,
    send_chunk_fn: Optional[SendChunk] = None,
) -> tuple:
    """Execute one signed-range transfer."""
    fetcher = fetch_chunk_fn or fetch_chunk
    sender = send_chunk_fn or send_chunk
    source_url = transfer_context["source_url"]
    destination_url = transfer_context["dest_url"]
    transfer_id = transfer_context.get("transfer_id", "")
    chunk_size = int(transfer_context["chunk_size"])
    range_start = int(transfer_context["range_start"])
    range_end = int(transfer_context["range_end"])
    source_headers = transfer_context.get("source_headers") or {}
    destination_headers = transfer_context.get("dest_headers") or {}
    chunk_index = 0
    chunk_hashes = _chunk_hashes(task_message, chunk_index)
    client = state.http_client
    total_bytes = 0
    canary = is_canary_destination(destination_url)
    computed_hash = ""
    last_etag: Optional[str] = None
    offer_id = task_message.get("offer_id") or task_id
    hotkey = getattr(getattr(state.wallet, "hotkey", None), "ss58_address", "unknown")
    print(
        f"[Worker] Transferring signed range bytes={range_start}-{range_end} "
        f"task={task_label(task_id)} offer={task_label(offer_id)} hotkey={hotkey[:16]}"
    )

    if deadline_us > 0 and deadline_us - time.time() * 1_000_000 <= 0:
        return (total_bytes, False, f"Deadline exceeded before chunk {chunk_index}", "", last_etag)

    try:
        total_bytes, computed_hash, last_etag = await _transfer_chunk(
            fetcher,
            sender,
            client,
            source_url,
            destination_url,
            transfer_id,
            chunk_size,
            range_start,
            source_headers,
            destination_headers,
            chunk_hashes,
            canary,
            task_id,
            offer_id,
            chunk_index,
        )
    except asyncio.TimeoutError as error:
        detail = exception_detail(error)
        print(f"[Worker] Chunk {chunk_index} timeout task={task_label(task_id)} offer={task_label(offer_id)} error={detail}")
        return (total_bytes, False, f"Deadline exceeded at chunk {chunk_index}: {detail}", "", last_etag)
    except httpx.HTTPStatusError as error:
        detail = exception_detail(error)
        print(
            f"[Worker] Chunk {chunk_index} HTTP failure task={task_label(task_id)} "
            f"offer={task_label(offer_id)} status={error.response.status_code} error={detail}"
        )
        return (total_bytes, False, f"HTTP {error.response.status_code} at chunk {chunk_index}: {detail}", "", last_etag)
    except TransferValidationError as error:
        return (total_bytes, False, str(error), error.chunk_hash, last_etag)
    except Exception as error:
        detail = exception_detail(error)
        print(
            f"[Worker] Chunk {chunk_index} failure task={task_label(task_id)} "
            f"offer={task_label(offer_id)} error={detail}{http_status_detail(error)}"
        )
        return (total_bytes, False, f"Error at chunk {chunk_index}: {detail}", "", last_etag)

    if transfer_context.get("etag_required") and not last_etag:
        return (total_bytes, False, "missing ETag from storage PUT response", computed_hash or "", last_etag)
    print(f"[Worker] Transfer complete: {total_bytes} bytes")
    return (total_bytes, True, None, computed_hash, last_etag)


class TransferValidationError(Exception):
    def __init__(self, message: str, chunk_hash: str = "") -> None:
        super().__init__(message)
        self.chunk_hash = chunk_hash


def _chunk_hashes(task_message: dict, chunk_index: int) -> dict:
    hashes = {}
    if "chunk_hashes" in task_message and isinstance(task_message["chunk_hashes"], dict):
        for key, value in task_message["chunk_hashes"].items():
            hashes[int(key)] = value
    elif "chunk_hash" in task_message and task_message["chunk_hash"]:
        hashes[chunk_index] = task_message["chunk_hash"]
    return hashes


async def _transfer_chunk(
    fetcher: FetchChunk,
    sender: SendChunk,
    client: httpx.AsyncClient,
    source_url: str,
    destination_url: str,
    transfer_id: str,
    chunk_size: int,
    range_start: int,
    source_headers: dict,
    destination_headers: dict,
    chunk_hashes: dict,
    canary: bool,
    task_id: str,
    offer_id: str,
    chunk_index: int,
) -> tuple[int, str, Optional[str]]:
    chunk_started = time.perf_counter()
    fetch_started = time.perf_counter()
    data = await fetcher(
        client,
        source_url,
        expected_max_bytes=chunk_size,
        task_id=task_id,
        offer_id=offer_id,
        chunk_index=chunk_index,
        offer_source_headers=source_headers or None,
    )
    fetch_ms = (time.perf_counter() - fetch_started) * 1000
    bytes_fetched = len(data)
    if bytes_fetched != chunk_size:
        raise TransferValidationError(f"source range returned {bytes_fetched} bytes, expected {chunk_size}")

    hash_started = time.perf_counter()
    computed_hash = hashlib.sha256(data).hexdigest()
    hash_ms = (time.perf_counter() - hash_started) * 1000
    expected_hash = chunk_hashes.get(chunk_index) or ""
    if expected_hash and computed_hash and expected_hash.lower() != computed_hash.lower():
        raise TransferValidationError(f"Chunk {chunk_index} hash mismatch", computed_hash)

    if canary:
        print(f"[Worker] Chunk {chunk_index}: CANARY mode, skipping upload")
        return bytes_fetched, computed_hash, None

    send_started = time.perf_counter()
    _success, etag, response_code = await sender(
        client,
        destination_url,
        data,
        transfer_id,
        chunk_index,
        chunk_offset=range_start,
        total_size=chunk_size,
        task_id=task_id,
        offer_id=offer_id,
        offer_dest_headers=destination_headers or None,
    )
    send_ms = (time.perf_counter() - send_started) * 1000
    total_ms = (time.perf_counter() - chunk_started) * 1000
    mbps = (bytes_fetched * 8 / 1_000_000) / (total_ms / 1000) if total_ms > 0 else 0
    print(
        f"[Worker] Chunk {chunk_index}: {bytes_fetched} bytes transferred "
        f"task={task_label(task_id)} offer={task_label(offer_id)} "
        f"fetch_ms={fetch_ms:.1f} hash_ms={hash_ms:.1f} send_ms={send_ms:.1f} "
        f"total_ms={total_ms:.1f} mbps={mbps:.1f} response={response_code}"
    )
    return bytes_fetched, computed_hash, etag
