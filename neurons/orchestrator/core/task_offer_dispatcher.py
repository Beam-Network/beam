"""
Dedicated FIFO queue for BeamCore NATS task-offer delivery.

This queue keeps worker websocket delivery asynchronous so NATS message handling stays independent
from local worker I/O.
"""

from __future__ import annotations

import asyncio
import inspect
import logging
import time
from typing import Any, Awaitable, Callable, Optional

logger = logging.getLogger(__name__)

OfferHandler = Callable[[dict[str, Any]], Awaitable[None] | None]


class TaskOfferDispatcher:
    """Single-consumer FIFO dispatcher for worker_task_offer_batch payloads."""

    def __init__(self, handler: OfferHandler) -> None:
        self._handler = handler
        self._queue: asyncio.Queue[dict[str, Any] | None] = asyncio.Queue()
        self._worker_task: Optional[asyncio.Task] = None
        self._running = False

    async def start(self) -> None:
        if self._worker_task and not self._worker_task.done():
            return
        if self._worker_task and self._worker_task.done():
            logger.warning("TaskOfferDispatcher worker exited; restarting")
        self._running = True
        self._worker_task = asyncio.create_task(self._run())

    async def stop(self) -> None:
        self._running = False
        if self._worker_task:
            self._worker_task.cancel()
            try:
                await self._worker_task
            except asyncio.CancelledError:
                pass
            self._worker_task = None

    def enqueue_offer(self, payload: dict[str, Any]) -> bool:
        if not self._running or not self._worker_task or self._worker_task.done():
            logger.warning("TaskOfferDispatcher not running; dropping worker_task_offer_batch")
            return False
        self._queue.put_nowait(payload)
        return True

    async def _run(self) -> None:
        try:
            while self._running:
                try:
                    item = await asyncio.wait_for(self._queue.get(), timeout=1.0)
                except asyncio.TimeoutError:
                    continue
                if item is None:
                    continue
                await self._dispatch_one(item)
        except Exception:
            logger.exception("TaskOfferDispatcher worker crashed")
        finally:
            self._running = False

    async def _dispatch_one(self, payload: dict[str, Any]) -> None:
        offers = payload.get("offers") if isinstance(payload.get("offers"), list) else []
        batch_id = str(payload.get("batch_id") or "")
        task_id = ""
        if offers and isinstance(offers[0], dict):
            task_id = str(offers[0].get("task_id") or "")
        started_at = time.monotonic()
        try:
            result = self._handler(payload)
            if inspect.isawaitable(result):
                await result
        except Exception as exc:
            logger.error("Error handling worker_task_offer_batch batch=%s task=%s: %s", batch_id, task_id[:8], exc)
        finally:
            handler_ms = (time.monotonic() - started_at) * 1000.0
            logger.info(
                "worker_task_offer_batch handled batch=%s task=%s offer_handler_ms=%.1f",
                batch_id,
                task_id[:8] if task_id else "unknown",
                handler_ms,
            )
