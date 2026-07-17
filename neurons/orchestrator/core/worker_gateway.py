"""
In-process worker gateway.

Workers connect via WebSocket to /ws/{worker_id}?api_key=...
The orchestrator forwards task offer batch items as task_offer messages,
and relays task_result messages upstream.
"""

import asyncio
import json
import logging
import os
from collections import deque
from typing import Callable, Dict, Optional

logger = logging.getLogger(__name__)

MAX_WORKERS = 10
RESULT_FORWARD_RETRY_BASE_SECONDS = max(0.0, float(os.environ.get("ORCH_RESULT_FORWARD_RETRY_BASE_SECONDS", "0.25")))
RESULT_FORWARD_RETRY_MAX_SECONDS = max(
    RESULT_FORWARD_RETRY_BASE_SECONDS,
    float(os.environ.get("ORCH_RESULT_FORWARD_RETRY_MAX_SECONDS", "2.0")),
)
RESULT_TERMINAL_CACHE_SIZE = max(1, int(os.environ.get("ORCH_RESULT_TERMINAL_CACHE_SIZE", "100000")))
RESULT_TERMINAL_STATUSES = {
    "owned_processing",
    "completed",
    "failed",
    "late_superseded",
    "late_expired",
    "rejected",
}


class WorkerGateway:
    """Manages WebSocket sessions for locally-connected workers."""

    def __init__(
        self,
        on_ready_change: Optional[Callable[[bool], None]] = None,
    ) -> None:
        self._sessions: Dict[str, object] = {}
        self._cursor = 0
        self._on_ready_change = on_ready_change
        self._upstream: Optional[object] = None
        self._result_forward_tasks: Dict[str, asyncio.Task] = {}
        self._terminal_result_acks: Dict[str, dict] = {}
        self._terminal_result_order = deque()

    def set_upstream(self, upstream: object) -> None:
        self._upstream = upstream

    @property
    def connected_count(self) -> int:
        return len(self._sessions)

    @property
    def worker_ids(self) -> list:
        return list(self._sessions.keys())

    def is_full(self) -> bool:
        return len(self._sessions) >= MAX_WORKERS

    def connect(self, worker_id: str, ws: object) -> bool:
        if self.is_full() and worker_id not in self._sessions:
            logger.warning("Worker cap reached (%d); rejecting %s", MAX_WORKERS, worker_id)
            return False
        was_empty = len(self._sessions) == 0
        self._sessions[worker_id] = ws
        logger.info("Worker connected: %s (%d/%d)", worker_id, len(self._sessions), MAX_WORKERS)
        if was_empty and self._on_ready_change:
            self._on_ready_change(True)
        return True

    def disconnect(self, worker_id: str) -> None:
        self._sessions.pop(worker_id, None)
        logger.info("Worker disconnected: %s (%d/%d)", worker_id, len(self._sessions), MAX_WORKERS)
        if len(self._sessions) == 0 and self._on_ready_change:
            self._on_ready_change(False)

    async def stop(self) -> None:
        if not self._result_forward_tasks:
            return
        tasks = list(self._result_forward_tasks.values())
        for task in tasks:
            task.cancel()
        await asyncio.gather(*tasks, return_exceptions=True)
        self._result_forward_tasks.clear()

    async def deliver_task_offer(self, worker_id: str, offer: dict) -> bool:
        ws = self._sessions.get(worker_id)
        if ws is None:
            logger.warning("deliver_task_offer: worker %s not connected", worker_id)
            return False
        try:
            await ws.send_text(json.dumps({"type": "task_offer", **offer}))
            return True
        except Exception as exc:
            logger.warning("deliver_task_offer send failed for %s: %s", worker_id, exc)
            self._sessions.pop(worker_id, None)
            return False

    def get_workers_round_robin(self, n: int = 1) -> list:
        """Return up to n worker_ids in round-robin order."""
        ids = list(self._sessions.keys())
        if not ids:
            return []
        selected = []
        for _ in range(min(n, len(ids))):
            selected.append(ids[self._cursor % len(ids)])
            self._cursor += 1
        return selected

    async def handle_worker_message(self, worker_id: str, raw: str) -> None:
        """Process an inbound message from a connected worker."""
        try:
            msg = json.loads(raw)
        except json.JSONDecodeError:
            logger.warning("Non-JSON from worker %s", worker_id)
            return

        msg_type = msg.get("type")
        if msg_type == "task_result":
            await self._relay_task_result(worker_id, msg)
        else:
            logger.debug("Unhandled worker message type %s from %s", msg_type, worker_id)

    async def _send_to_worker(self, worker_id: str, payload: dict) -> None:
        ws = self._sessions.get(worker_id)
        if ws is None:
            return
        try:
            await ws.send_text(json.dumps(payload))
        except Exception as exc:
            logger.warning("worker ack send failed for %s: %s", worker_id, exc)
            self._sessions.pop(worker_id, None)

    def _cache_terminal_result_ack(self, result_key: str, ack: dict) -> None:
        if result_key not in self._terminal_result_acks:
            self._terminal_result_order.append(result_key)
        self._terminal_result_acks[result_key] = ack
        while len(self._terminal_result_order) > RESULT_TERMINAL_CACHE_SIZE:
            expired = self._terminal_result_order.popleft()
            self._terminal_result_acks.pop(expired, None)

    def _schedule_result_forward(self, worker_id: str, payload: dict) -> None:
        offer_id = str(payload.get("offer_id") or payload.get("task_id"))
        result_key = f"{offer_id}:{worker_id}"
        if result_key in self._result_forward_tasks:
            return

        task = asyncio.create_task(self._forward_task_result_to_beamcore(worker_id, payload))
        self._result_forward_tasks[result_key] = task

        def _done(done_task: asyncio.Task) -> None:
            if self._result_forward_tasks.get(result_key) is done_task:
                self._result_forward_tasks.pop(result_key, None)
            try:
                exc = done_task.exception()
            except asyncio.CancelledError:
                return
            if exc is not None:
                logger.error("task_result forward crashed: %s", exc)

        task.add_done_callback(_done)

    async def _forward_task_result_to_beamcore(self, worker_id: str, payload: dict) -> None:
        task_id = payload.get("task_id")
        offer_id = str(payload.get("offer_id") or task_id)
        result_key = f"{offer_id}:{worker_id}"
        attempt = 0
        while True:
            attempt += 1
            try:
                if self._upstream is None:
                    raise RuntimeError("beamcore_unavailable")
                sender = getattr(self._upstream, "send_task_result_strict", self._upstream.send_task_result)
                ack = await sender(payload)
                if not isinstance(ack, dict):
                    raise RuntimeError("invalid_beamcore_ack")
                status = str(ack.get("status") or "")
                if status in RESULT_TERMINAL_STATUSES:
                    terminal_ack = {
                        **ack,
                        "type": "task_result_ack",
                        "task_id": task_id,
                        "offer_id": offer_id,
                    }
                    self._cache_terminal_result_ack(result_key, terminal_ack)
                    await self._send_to_worker(worker_id, terminal_ack)
                    logger.info(
                        "task_result relay terminal: task=%s offer=%s worker=%s status=%s",
                        task_id,
                        offer_id,
                        worker_id,
                        status,
                    )
                    return
                retry_reason = ack.get("reason") or status or "invalid_ack_status"
            except Exception as exc:
                retry_reason = type(exc).__name__

            delay = min(
                RESULT_FORWARD_RETRY_MAX_SECONDS,
                RESULT_FORWARD_RETRY_BASE_SECONDS * (2 ** min(attempt - 1, 16)),
            )
            if attempt == 1 or attempt & (attempt - 1) == 0:
                logger.info(
                    "task_result relay retry: task=%s offer=%s worker=%s attempt=%s delay_s=%.3f reason=%s",
                    task_id,
                    offer_id,
                    worker_id,
                    attempt,
                    delay,
                    retry_reason,
                )
            await asyncio.sleep(delay)

    async def _relay_task_result(self, worker_id: str, msg: dict) -> None:
        task_id = msg.get("task_id")
        offer_id = msg.get("offer_id") or task_id
        if not task_id or not offer_id:
            logger.warning("dropping task_result missing task_id/offer_id from worker=%s", worker_id)
            await self._send_to_worker(
                worker_id,
                {
                    "type": "task_result_ack",
                    "task_id": task_id,
                    "offer_id": offer_id,
                    "received": False,
                    "status": "rejected",
                    "reason": "missing_task_or_offer_id",
                },
            )
            return

        payload = {
            "type": "task_result",
            "task_id": task_id,
            "offer_id": offer_id,
            "worker_id": worker_id,
            "success": bool(msg.get("success")),
        }
        for key in ("etag", "chunk_hash", "error"):
            if msg.get(key) is not None:
                payload[key] = msg[key]

        result_key = f"{offer_id}:{worker_id}"
        terminal_ack = self._terminal_result_acks.get(result_key)
        if terminal_ack is not None:
            await self._send_to_worker(worker_id, terminal_ack)
            return

        if result_key in self._result_forward_tasks:
            logger.debug("task_result relay already active: task=%s offer=%s worker=%s", task_id, offer_id, worker_id)
            return

        self._schedule_result_forward(worker_id, payload)
