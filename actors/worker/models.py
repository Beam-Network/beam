"""Stable worker runtime models and capability-manifest helpers."""

import asyncio
import time
from dataclasses import dataclass, field
from importlib.metadata import PackageNotFoundError, version as package_version
from typing import Any, Dict, Optional

import httpx


CAPABILITY_SCHEMA_VERSION = "room-transfer/v1"
WORKER_CAPABILITIES = ("transfer.multipart",)


def resolve_worker_version() -> str:
    try:
        return package_version("beam")
    except PackageNotFoundError:
        return "0.2.0"


WORKER_VERSION = resolve_worker_version()


def capability_timestamp(offset_seconds: float = 0.0) -> str:
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(time.time() + offset_seconds))


@dataclass
class WorkerState:
    """Worker runtime state."""

    wallet: Any
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
    pending_task_results: Dict[str, asyncio.Future] = field(default_factory=dict)
    active_ws_task_ids: set[str] = field(default_factory=set)
    ws_offer_queue: asyncio.Queue = field(default_factory=asyncio.Queue)
    ws_executor_task: Optional[asyncio.Task] = None
    active_websocket: Optional[Any] = None


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
    """BeamCore ownership fields returned for a worker task result."""

    received: bool = False
    status: Optional[str] = None
    reason: Optional[str] = None


def build_worker_capability_manifest(state: WorkerState) -> dict:
    available = 0 if state.active_tasks else 1
    capabilities = list(WORKER_CAPABILITIES)
    return {
        "schema_version": CAPABILITY_SCHEMA_VERSION,
        "actor_type": "worker",
        "actor_id": state.worker_id or "",
        "software_version": WORKER_VERSION,
        "protocols": [{"name": capability, "min": 1, "max": 1} for capability in capabilities],
        "capabilities": capabilities,
        "capacity": {"max_connections": 1, "available_connections": available},
        "observed_at": capability_timestamp(),
        "expires_at": capability_timestamp(45),
    }
