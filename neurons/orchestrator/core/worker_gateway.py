"""
In-process worker gateway.

Workers connect via WebSocket to /ws/{worker_id}?api_key=...
The orchestrator forwards task offer batch items as task_offer messages,
and relays task_accept / task_reject / task_result upstream.
"""

import asyncio
import json
import logging
import os
from collections import deque
from typing import Callable, Dict, Optional, Set

logger = logging.getLogger(__name__)

MAX_WORKERS = 10
RESULT_FORWARD_CONCURRENCY = max(1, int(os.environ.get("ORCH_RESULT_FORWARD_CONCURRENCY", "8")))
RESULT_FORWARD_MAX_ATTEMPTS = max(1, int(os.environ.get("ORCH_RESULT_FORWARD_MAX_ATTEMPTS", "8")))
RESULT_FORWARD_RETRY_BASE_SECONDS = max(0.0, float(os.environ.get("ORCH_RESULT_FORWARD_RETRY_BASE_SECONDS", "0.25")))
RESULT_FORWARD_RETRY_MAX_SECONDS = max(
    RESULT_FORWARD_RETRY_BASE_SECONDS,
    float(os.environ.get("ORCH_RESULT_FORWARD_RETRY_MAX_SECONDS", "2.0")),
)
RESULT_SEEN_CACHE_SIZE = max(1, int(os.environ.get("ORCH_RESULT_SEEN_CACHE_SIZE", "100000")))


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
        self._result_forward_semaphore = asyncio.Semaphore(RESULT_FORWARD_CONCURRENCY)
        self._result_forward_tasks: Set[asyncio.Task] = set()
        self._result_forward_seen_offers: Set[str] = set()
        self._result_forward_seen_order = deque()

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
        await asyncio.gather(*list(self._result_forward_tasks), return_exceptions=True)
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
        if msg_type in ("task_accept", "task_reject"):
            await self._relay_task_decision(worker_id, msg)
        elif msg_type == "task_result":
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

    async def _relay_task_decision(self, worker_id: str, msg: dict) -> None:
        ack_type = "task_accept_ack" if msg.get("type") == "task_accept" else "task_reject_ack"
        if self._upstream is None:
            await self._send_to_worker(
                worker_id,
                {"type": ack_type, "task_id": msg.get("task_id"), "offer_id": msg.get("offer_id"), "accepted": False, "reason": "beamcore_unavailable"},
            )
            return
        task_id = msg.get("task_id") or msg.get("offer_id")
        offer_id = msg.get("offer_id") or task_id
        reason = msg.get("reason")
        try:
            if msg.get("type") == "task_accept":
                ack = await self._upstream.send_task_accept(
                    task_id=task_id,
                    worker_id=worker_id,
                    offer_id=offer_id,
                    worker_version=msg.get("worker_version"),
                )
            else:
                ack = await self._upstream.send_task_reject(
                    task_id=task_id,
                    worker_id=worker_id,
                    offer_id=offer_id,
                    reason=reason,
                )
        except Exception as exc:
            logger.warning("relay task decision failed: %s", exc)
            ack = {
                "type": ack_type,
                "task_id": task_id,
                "offer_id": offer_id,
                "accepted": False,
                "reason": "beamcore_decision_forward_failed",
            }
        ack_payload = {
            **(ack if isinstance(ack, dict) else {}),
            "type": ack_type,
            "task_id": task_id,
            "offer_id": offer_id,
        }
        await self._send_to_worker(worker_id, ack_payload)

    def _remember_result_offer(self, offer_id: str) -> bool:
        if offer_id in self._result_forward_seen_offers:
            return False
        self._result_forward_seen_offers.add(offer_id)
        self._result_forward_seen_order.append(offer_id)
        while len(self._result_forward_seen_order) > RESULT_SEEN_CACHE_SIZE:
            expired = self._result_forward_seen_order.popleft()
            self._result_forward_seen_offers.discard(expired)
        return True

    def _schedule_result_forward(self, payload: dict) -> None:
        task = asyncio.create_task(self._forward_task_result_limited(payload))
        self._result_forward_tasks.add(task)

        def _done(done_task: asyncio.Task) -> None:
            self._result_forward_tasks.discard(done_task)
            try:
                exc = done_task.exception()
            except asyncio.CancelledError:
                return
            if exc is not None:
                logger.error("task_result forward crashed: %s", exc)

        task.add_done_callback(_done)

    async def _forward_task_result_limited(self, payload: dict) -> None:
        async with self._result_forward_semaphore:
            await self._forward_task_result_to_beamcore(payload)

    async def _forward_task_result_to_beamcore(self, payload: dict) -> None:
        task_id = payload.get("task_id")
        offer_id = payload.get("offer_id") or task_id
        worker_id = payload.get("worker_id")
        last_error: Exception | None = None
        for attempt in range(1, RESULT_FORWARD_MAX_ATTEMPTS + 1):
            try:
                if self._upstream is None:
                    raise RuntimeError("beamcore_unavailable")
                sender = getattr(self._upstream, "send_task_result_strict", self._upstream.send_task_result)
                ack = await sender(payload)
                received = bool(ack.get("received", True)) if isinstance(ack, dict) else False
                completed = ack.get("completed") if isinstance(ack, dict) else None
                reason = ack.get("reason") if isinstance(ack, dict) else "invalid_beamcore_ack"
                if received and completed is not False:
                    logger.info(
                        "task_result forwarded: task=%s offer=%s worker=%s completed=%s",
                        task_id,
                        offer_id,
                        worker_id,
                        completed,
                    )
                    return
                logger.warning(
                    "task_result rejected by BeamCore: task=%s offer=%s worker=%s reason=%s",
                    task_id,
                    offer_id,
                    worker_id,
                    reason,
                )
                return
            except Exception as exc:
                last_error = exc
                if attempt >= RESULT_FORWARD_MAX_ATTEMPTS:
                    break
                delay = min(
                    RESULT_FORWARD_RETRY_MAX_SECONDS,
                    RESULT_FORWARD_RETRY_BASE_SECONDS * (2 ** (attempt - 1)),
                )
                logger.info(
                    "task_result forward retry: task=%s offer=%s worker=%s attempt=%s/%s delay_s=%.3f error=%s",
                    task_id,
                    offer_id,
                    worker_id,
                    attempt + 1,
                    RESULT_FORWARD_MAX_ATTEMPTS,
                    delay,
                    type(exc).__name__,
                )
                if delay > 0:
                    await asyncio.sleep(delay)
        logger.error(
            "task_result forward exhausted: task=%s offer=%s worker=%s error=%s",
            task_id,
            offer_id,
            worker_id,
            last_error,
        )

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
                    "completed": False,
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

        first_seen = self._remember_result_offer(str(offer_id))
        if first_seen:
            self._schedule_result_forward(payload)
        else:
            logger.info("duplicate task_result received locally: task=%s offer=%s worker=%s", task_id, offer_id, worker_id)

        await self._send_to_worker(
            worker_id,
            {
                "type": "task_result_ack",
                "task_id": task_id,
                "offer_id": offer_id,
                "received": True,
                "forwarding": first_seen,
                **({"duplicate": True} if not first_seen else {}),
            },
        )
