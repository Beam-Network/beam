"""WebSocket transport and sequential task execution for the Beam worker."""

import asyncio
import json
import time
from typing import Awaitable, Callable, Optional

try:
    from .models import TaskSummaryAck, WorkerState
    from .transfer_context import (
        build_transfer_context,
        claim_ws_task,
        remaining_deadline_seconds,
        task_label,
    )
    from .websocket_protocol import (
        get_ws_status_code,
        get_ws_url,
        parse_task_result_ack,
        ws_send_capability_update,
        ws_send_task_result,
    )
except ImportError:  # Direct execution through worker.py
    from models import TaskSummaryAck, WorkerState
    from transfer_context import (
        build_transfer_context,
        claim_ws_task,
        remaining_deadline_seconds,
        task_label,
    )
    from websocket_protocol import (
        get_ws_status_code,
        get_ws_url,
        parse_task_result_ack,
        ws_send_capability_update,
        ws_send_task_result,
    )

try:
    import websockets
    from websockets.exceptions import ConnectionClosed

    try:
        from websockets.exceptions import InvalidStatus
    except ImportError:
        from websockets.exceptions import InvalidStatusCode as InvalidStatus
    WEBSOCKETS_AVAILABLE = True
except ImportError:
    websockets = None
    ConnectionClosed = Exception
    InvalidStatus = Exception
    WEBSOCKETS_AVAILABLE = False


async def wait_for_result_websocket(
    state: WorkerState,
    fallback,
    reconnect_wait_seconds: float,
):
    """Return the current worker-gateway socket, waiting across reconnects."""
    if state.active_websocket is not None:
        return state.active_websocket
    if state.ws_connected and fallback is not None:
        return fallback
    if reconnect_wait_seconds <= 0:
        return fallback

    deadline = time.monotonic() + reconnect_wait_seconds
    while state.running and time.monotonic() < deadline:
        if state.active_websocket is not None:
            return state.active_websocket
        await asyncio.sleep(0.1)
    return state.active_websocket or fallback


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
    *,
    wait_for_websocket_fn: Callable[..., Awaitable] = wait_for_result_websocket,
    send_result_fn: Callable[..., Awaitable[bool]] = ws_send_task_result,
    ack_timeout: float = 3.0,
    send_attempts: int = 8,
    reconnect_wait_seconds: float = 2.0,
    terminal_statuses: set[str],
) -> TaskSummaryAck:
    """Send task_result until BeamCore assumes or rejects relay ownership."""
    result_key = offer_id or task_id
    empty = TaskSummaryAck()
    for attempt in range(send_attempts):
        ack_future = asyncio.get_event_loop().create_future()
        state.pending_task_results[result_key] = ack_future
        try:
            send_websocket = await wait_for_websocket_fn(
                state, websocket, reconnect_wait_seconds
            )
            sent = await send_result_fn(
                send_websocket,
                state,
                task_id,
                success,
                bytes_transferred,
                duration_ms=duration_ms,
                chunk_hash=chunk_hash,
                etag=etag,
                error=error,
                offer_id=offer_id,
            )
            if not sent:
                if attempt < send_attempts - 1:
                    await asyncio.sleep(min(reconnect_wait_seconds, 0.25 * (attempt + 1)))
                continue
            ack = await _wait_for_task_ack(
                ack_future,
                ack_timeout,
                attempt,
                send_attempts,
                task_id,
                offer_id,
                terminal_statuses,
            )
            if ack is not None:
                return ack
        finally:
            state.pending_task_results.pop(result_key, None)

    print(
        f"[Worker] [WS] Task result failed after websocket retries: "
        f"{task_label(task_id)} offer={task_label(offer_id)}"
    )
    return empty


async def _wait_for_task_ack(
    ack_future,
    ack_timeout: float,
    attempt: int,
    send_attempts: int,
    task_id: str,
    offer_id: str,
    terminal_statuses: set[str],
) -> Optional[TaskSummaryAck]:
    try:
        ack = await asyncio.wait_for(ack_future, timeout=ack_timeout)
        if ack.status in terminal_statuses:
            print(
                f"[Worker] [WS] Task result settled by BeamCore: "
                f"task={task_label(task_id)} offer={task_label(offer_id)} "
                f"status={ack.status or 'unknown'}"
            )
            return ack
        print(
            f"[Worker] [WS] Task result relay not terminal: "
            f"task={task_label(task_id)} offer={task_label(offer_id)} "
            f"status={ack.status or 'invalid'} reason={ack.reason or 'retry'}"
        )
    except asyncio.TimeoutError:
        print(
            f"[Worker] [WS] Task result ack timeout attempt={attempt + 1}/{send_attempts} "
            f"task={task_label(task_id)} offer={task_label(offer_id)}"
        )
    return None


def enqueue_ws_task(state: WorkerState, websocket, task: dict) -> None:
    """Queue one offered attempt for the worker's singular FIFO executor."""
    task_id = task.get("task_id") or task.get("offer_id")
    offer_id = task.get("offer_id") or task_id
    task_key = offer_id or task_id
    if not task_id:
        print("[Worker] [WS] Skipping task: missing task_id")
        return
    if not claim_ws_task(state, task_key):
        print(
            f"[Worker] [WS] Duplicate task offer ignored: "
            f"{task_label(task_id)} offer={task_label(offer_id)}"
        )
        return
    state.ws_offer_queue.put_nowait((websocket, task))
    print(
        f"[Worker] [WS] Task queued: {task_label(task_id)} offer={task_label(offer_id)} "
        f"queue_depth={state.ws_offer_queue.qsize()}"
    )


async def ws_task_executor(
    state: WorkerState,
    handle_task_fn: Callable[..., Awaitable[bool]],
) -> None:
    """Execute queued offers one at a time, in arrival order."""
    while True:
        item = await state.ws_offer_queue.get()
        try:
            if item is None:
                return
            websocket, task = item
            await handle_task_fn(state, websocket, task)
        except asyncio.CancelledError:
            raise
        except Exception as exc:
            task_id = task.get("task_id") or task.get("offer_id")
            offer_id = task.get("offer_id") or task_id
            print(
                f"[Worker] [WS] Sequential task executor error: "
                f"task={task_label(task_id)} offer={task_label(offer_id)} "
                f"error={type(exc).__name__}: {exc}"
            )
        finally:
            state.ws_offer_queue.task_done()


async def handle_ws_task(
    state: WorkerState,
    websocket,
    task: dict,
    *,
    execute_task_fn: Callable[..., Awaitable],
    finalize_result_fn: Callable[..., Awaitable[TaskSummaryAck]],
) -> bool:
    """Handle a task received via WebSocket push."""
    task_id = task.get("task_id") or task.get("offer_id")
    offer_id = task.get("offer_id") or task_id
    task_key = offer_id or task_id
    deadline_us = task.get("deadline_us", 0)
    print(f"[Worker] [WS] Task: {task_label(task_id)} offer={task_label(offer_id)}...")

    try:
        transfer_context, validation_error = build_transfer_context(task)
        if validation_error or transfer_context is None:
            reason = f"invalid_offer:{validation_error or 'unknown'}"
            await finalize_result_fn(
                websocket, state, task_id, False, 0, error=reason, offer_id=offer_id
            )
            print(
                f"[Worker] [WS] Failed task {task_label(task_id)} "
                f"offer={task_label(offer_id)}: {reason}"
            )
            return False

        remaining_sec = remaining_deadline_seconds(deadline_us)
        if remaining_sec is not None and remaining_sec < 5:
            reason = "deadline_too_close"
            await finalize_result_fn(
                websocket, state, task_id, False, 0, error=reason, offer_id=offer_id
            )
            print(
                f"[Worker] [WS] Failed task {task_label(task_id)} "
                f"offer={task_label(offer_id)}: {reason} ({remaining_sec:.1f}s)"
            )
            return False

        result = await execute_task_fn(
            state,
            task_id,
            task,
            transfer_context,
            deadline_us,
            log_prefix="[Worker] [WS]",
        )
        await finalize_result_fn(
            websocket,
            state,
            task_id,
            result.success,
            result.bytes_transferred,
            duration_ms=result.duration_ms,
            chunk_hash=result.chunk_hash,
            etag=result.etag,
            error=result.error_msg,
            offer_id=offer_id,
        )
        status = "OK" if result.success else f"FAIL: {result.error_msg}"
        print(
            f"[Worker] [WS] Task {task_label(task_id)} offer={task_label(offer_id)}: "
            f"{status} | {result.bytes_transferred} bytes"
        )
        return result.success
    finally:
        state.active_ws_task_ids.discard(task_key)


async def websocket_loop(
    state: WorkerState,
    *,
    websocket_api,
    connection_closed_type,
    invalid_status_type,
    shutdown_signal: asyncio.Event,
    send_capability_fn: Callable[..., Awaitable[None]],
    enqueue_task_fn: Callable[[WorkerState, object, dict], None],
    valid_ack_statuses: set[str],
    reconnect_min_delay: float,
    reconnect_max_delay: float,
    reconnect_multiplier: float,
    max_reconnect_attempts: Optional[int],
    ping_interval: float,
    ping_timeout: float,
) -> None:
    """Run the WebSocket connection and reconnection lifecycle."""
    if websocket_api is None:
        raise RuntimeError("websockets library is required for worker gateway transport")
    if not state.worker_gateway_url:
        raise RuntimeError("WORKER_GATEWAY_URL is required for worker gateway transport")

    ws_url = get_ws_url(state.worker_id, state.api_key, state.worker_gateway_url)
    print(f"[Worker] Connecting to WebSocket: {ws_url.split('?')[0]}")
    reconnect_delay = reconnect_min_delay
    while state.running and state.use_websocket:
        reconnect_delay = await _connection_attempt(
            state,
            ws_url,
            reconnect_delay,
            websocket_api,
            connection_closed_type,
            invalid_status_type,
            send_capability_fn,
            enqueue_task_fn,
            valid_ack_statuses,
            ping_interval,
            ping_timeout,
            reconnect_min_delay,
        )
        if max_reconnect_attempts is not None and state.ws_reconnect_attempts >= max_reconnect_attempts:
            raise RuntimeError("worker gateway websocket unavailable after maximum reconnect attempts")
        if state.running and not shutdown_signal.is_set():
            _log_reconnect(reconnect_delay, state.ws_reconnect_attempts, max_reconnect_attempts)
            try:
                await asyncio.wait_for(shutdown_signal.wait(), timeout=reconnect_delay)
                break
            except asyncio.TimeoutError:
                pass
            reconnect_delay = min(reconnect_delay * reconnect_multiplier, reconnect_max_delay)

    state.ws_connected = False
    print("[Worker] [WS] Loop stopped")


async def _connection_attempt(
    state,
    ws_url,
    reconnect_delay,
    websocket_api,
    connection_closed_type,
    invalid_status_type,
    send_capability_fn,
    enqueue_task_fn,
    valid_ack_statuses,
    ping_interval,
    ping_timeout,
    reconnect_min_delay,
) -> float:
    try:
        async with websocket_api.connect(
            ws_url,
            ping_interval=ping_interval,
            ping_timeout=ping_timeout,
            close_timeout=5,
        ) as websocket:
            state.ws_connected = True
            state.active_websocket = websocket
            state.ws_reconnect_attempts = 0
            reconnect_delay = reconnect_min_delay
            print("[Worker] [WS] Connected!")
            await send_capability_fn(websocket, state)
            await _receive_messages(
                state,
                websocket,
                connection_closed_type,
                send_capability_fn,
                enqueue_task_fn,
                valid_ack_statuses,
                ping_interval,
            )
    except invalid_status_type as exc:
        status_code = getattr(exc, "status_code", None)
        print(f"[Worker] [WS] Connection rejected: HTTP {status_code}")
        raise RuntimeError(
            f"worker gateway websocket rejected the connection with HTTP {status_code}"
        ) from exc
    except ConnectionRefusedError:
        print("[Worker] [WS] Connection refused")
    except Exception as exc:
        print(f"[Worker] [WS] Connection error: {type(exc).__name__}: {exc}")

    state.ws_connected = False
    state.active_websocket = None
    state.ws_reconnect_attempts += 1
    return reconnect_delay


async def _receive_messages(
    state,
    websocket,
    connection_closed_type,
    send_capability_fn,
    enqueue_task_fn,
    valid_ack_statuses,
    ping_interval,
) -> None:
    while state.running:
        try:
            try:
                raw_message = await asyncio.wait_for(websocket.recv(), timeout=ping_interval)
                await _dispatch_message(
                    json.loads(raw_message),
                    state,
                    websocket,
                    enqueue_task_fn,
                    valid_ack_statuses,
                )
            except asyncio.TimeoutError:
                await send_capability_fn(websocket, state)
        except connection_closed_type as exc:
            print(f"[Worker] [WS] Connection closed: {exc.code} {exc.reason}")
            break


async def _dispatch_message(message, state, websocket, enqueue_task_fn, valid_ack_statuses):
    msg_type = message.get("type")
    if msg_type == "connected":
        print("[Worker] [WS] Server confirmed connection")
    elif msg_type == "task_offer":
        enqueue_task_fn(state, websocket, message)
    elif msg_type == "task_result_ack":
        ack_task_id = message.get("task_id")
        ack_offer_id = message.get("offer_id") or ack_task_id
        ack = parse_task_result_ack(message, valid_ack_statuses)
        if ack_offer_id and ack_offer_id in state.pending_task_results:
            future = state.pending_task_results.pop(ack_offer_id)
            if not future.done():
                future.set_result(ack)
        if ack.status == "rejected":
            print(
                f"[Worker] [WS] BeamCore rejected task_result: "
                f"task={task_label(ack_task_id)} offer={task_label(ack_offer_id)} "
                f"status={ack.status or 'unknown'} reason={ack.reason or 'unknown'}"
            )
    elif msg_type == "error":
        print(f"[Worker] [WS] Server error: {message.get('message', 'unknown')}")


def _log_reconnect(delay: float, attempts: int, max_attempts: Optional[int]) -> None:
    if max_attempts is None:
        print(f"[Worker] [WS] Reconnecting in {delay:.1f}s (attempt {attempts})...")
    else:
        print(
            f"[Worker] [WS] Reconnecting in {delay:.1f}s "
            f"(attempt {attempts}/{max_attempts})..."
        )
