"""Pure WebSocket message and URL contracts for the Beam worker."""

import json
from typing import Callable, Optional

try:
    from .models import TaskSummaryAck, WorkerState, build_worker_capability_manifest
except ImportError:  # Direct execution through worker.py
    from models import TaskSummaryAck, WorkerState, build_worker_capability_manifest


def get_ws_url(worker_id: str, api_key: str, gateway_url: str) -> str:
    """Convert a worker gateway URL to its worker WebSocket URL."""
    base = gateway_url.rstrip("/")
    if base.startswith("https://"):
        ws_base = "wss://" + base[8:]
    elif base.startswith("http://"):
        ws_base = "ws://" + base[7:]
    else:
        ws_base = "ws://" + base
    url = f"{ws_base}/ws/{worker_id}"
    return f"{url}?api_key={api_key}" if api_key else url


def get_ws_status_code(exc: Exception) -> Optional[int]:
    """Extract an HTTP status code from websocket handshake failures."""
    status_code = getattr(exc, "status_code", None)
    if isinstance(status_code, int):
        return status_code
    response_status = getattr(getattr(exc, "response", None), "status_code", None)
    if isinstance(response_status, int):
        return response_status
    for token in str(exc).split():
        stripped = token.rstrip(":,)")
        if stripped.isdigit() and 100 <= int(stripped) <= 599:
            return int(stripped)
    return None


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
    """Send one task completion receipt over WebSocket."""
    try:
        message = {
            "type": "task_result",
            "task_id": task_id,
            "offer_id": offer_id or task_id,
            "worker_id": state.worker_id,
            "success": success,
            "bytes_transferred": bytes_transferred,
        }
        if duration_ms is not None:
            message["duration_ms"] = duration_ms
        if chunk_hash:
            message["chunk_hash"] = chunk_hash
        if etag:
            message["etag"] = etag
        if error:
            message["error"] = error
        await websocket.send(json.dumps(message))
        return True
    except Exception as exc:
        print(f"[Worker] WS task_result error: {exc}")
        return False


async def ws_send_capability_update(
    websocket,
    state: WorkerState,
    manifest_builder: Callable[[WorkerState], dict] = build_worker_capability_manifest,
) -> None:
    if not state.worker_id:
        return
    try:
        await websocket.send(
            json.dumps(
                {
                    "type": "worker_capability_update",
                    "capability_manifest": manifest_builder(state),
                }
            )
        )
    except Exception as exc:
        print(f"[Worker] WS capability_update error: {exc}")


def parse_task_result_ack(message: dict, valid_statuses: set[str]) -> TaskSummaryAck:
    received_value = message.get("received")
    received = received_value if isinstance(received_value, bool) else False
    status_value = message.get("status")
    status = status_value if isinstance(status_value, str) and status_value in valid_statuses else None
    if status is not None and received != (status not in {"retry", "rejected"}):
        status = None
    reason = message.get("reason")
    return TaskSummaryAck(
        received=received,
        status=status,
        reason=str(reason) if reason else None,
    )
