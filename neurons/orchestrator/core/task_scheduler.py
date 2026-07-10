"""
Task Scheduler - Task assignment, offer broadcasting, and worker selection.
"""

import asyncio
import logging
import time
import uuid
from typing import Any, Dict, List, Optional, Tuple

from .config import OrchestratorSettings

logger = logging.getLogger(__name__)


class TaskScheduler:
    """Manages task assignment, broadcast offers, and worker selection."""

    def __init__(self, settings: OrchestratorSettings, worker_manager, get_subnet_core_client=None):
        self.settings = settings
        self.worker_manager = worker_manager
        self._get_subnet_core_client = get_subnet_core_client or (lambda: None)

        # Task management
        self.active_tasks: Dict[str, Any] = {}  # task_id -> BandwidthTask
        self.completed_tasks: Dict[str, Any] = {}  # task_id -> BandwidthTask

        # Pending task offers (broadcast model)
        self.pending_offers: Dict[str, Any] = {}  # offer_id -> PendingOffer
        self._offer_lock = asyncio.Lock()

    async def _save_task_to_core(
        self,
        task_id: str,
        worker_id: str,
        chunk_size: int = 0,
        chunk_hash: str = "",
        deadline_us: int = 0,
        source_region: str = "",
        dest_region: str = "",
        execution_context: Optional[Dict[str, Any]] = None,
    ) -> None:
        """Task records are owned by BeamCore task-offer batches."""
        return None

    async def assign_task(
        self,
        task_id: str,
        chunk_size: int,
        chunk_hash: str,
        source_region: str,
        dest_region: str,
        deadline_us: int,
        canary: bytes,
        canary_offset: int,
    ) -> Optional[str]:
        """Assign a bandwidth task to the best available worker."""
        from .orchestrator import BandwidthTask

        candidates = await self.worker_manager.get_available_workers()
        if not candidates:
            logger.warning("No available workers for task")
            return None

        worker = self._select_best_worker(candidates, source_region, dest_region)
        if not worker:
            return None

        task = BandwidthTask(
            task_id=task_id,
            worker_id=worker.worker_id,
            chunk_size=chunk_size,
            chunk_hash=chunk_hash,
            source_region=source_region,
            dest_region=dest_region,
            created_at=time.time(),
            deadline_us=deadline_us,
            canary=canary,
            canary_offset=canary_offset,
            status="assigned",
        )

        self.active_tasks[task_id] = task
        worker.active_tasks += 1
        worker.total_tasks += 1

        logger.info(
            f"Task {task_id[:16]}... assigned to worker {worker.worker_id} "
            f"(region: {worker.region}, trust: {worker.trust_score:.2f})"
        )

        await self._save_task_to_core(
            task_id=task_id,
            worker_id=worker.worker_id,
            chunk_size=chunk_size,
            chunk_hash=chunk_hash,
            deadline_us=deadline_us,
            source_region=source_region,
            dest_region=dest_region,
        )

        return worker.worker_id

    async def send_task_to_worker(
        self,
        worker_id: str,
        task_id: str,
        chunk_data: bytes,
        chunk_index: int,
        chunk_hash: str,
        transfer_id: str,
        destination_url: Optional[str] = None,
        sender_hotkey: Optional[str] = None,
        filename: Optional[str] = None,
        total_chunks: Optional[int] = None,
        receiver_filename: Optional[str] = None,
    ) -> bool:
        """Send a task (chunk data) to a worker via WebSocket."""
        websocket = self.worker_manager.worker_connections.get(worker_id)
        if not websocket:
            logger.warning(f"Worker {worker_id} not connected via WebSocket")
            return False

        try:
            import base64

            await websocket.send_json(
                {
                    "type": "task_offer",
                    "task_id": task_id,
                    "transfer_id": transfer_id,
                    "chunk_index": chunk_index,
                    "chunk_hash": chunk_hash,
                    "chunk_data": base64.b64encode(chunk_data).decode(),
                    "chunk_size": len(chunk_data),
                    "destination_url": destination_url,
                    "sender_hotkey": sender_hotkey,
                    "filename": filename,
                    "total_chunks": total_chunks,
                    "receiver_filename": receiver_filename,
                }
            )
            logger.info(
                f"Sent task {task_id[:16]}... to worker {worker_id} ({len(chunk_data)} bytes)"
            )

            # Build execution context for real data transfer (push model - no gateway URL)
            execution_context = {
                "transfer_id": transfer_id,
                "stream_id": transfer_id,
                "gateway_url": "",  # Push model - data is embedded in message
                "destination_url": destination_url or "",
                "chunk_indices": [chunk_index] if chunk_index is not None else [],
                "source_type": "push",
            }
            await self._save_task_to_core(
                task_id=task_id,
                worker_id=worker_id,
                chunk_size=len(chunk_data),
                chunk_hash=chunk_hash,
                execution_context=execution_context,
            )
            return True
        except Exception as e:
            logger.error(f"Failed to send task to worker {worker_id}: {e}")
            return False

    async def send_pull_task_to_worker(
        self,
        worker_id: str,
        task_id: str,
        chunk_index: int,
        chunk_hash: str,
        transfer_id: str,
        gateway_address: str,
        gateway_port: int,
        sender_hotkey: Optional[str] = None,
        filename: Optional[str] = None,
        total_chunks: Optional[int] = None,
        destination_url: Optional[str] = None,
        receiver_filename: Optional[str] = None,
    ) -> bool:
        """Send a pull-based task to a worker via WebSocket."""
        if not gateway_address:
            logger.warning(f"Cannot send pull task {task_id} - gateway_address is not set")
            return False

        websocket = self.worker_manager.worker_connections.get(worker_id)
        if not websocket:
            logger.warning(f"Worker {worker_id} not connected via WebSocket")
            return False

        try:
            await websocket.send_json(
                {
                    "type": "pull_task",
                    "task_id": task_id,
                    "transfer_id": transfer_id,
                    "chunk_index": chunk_index,
                    "chunk_hash": chunk_hash,
                    "gateway_address": gateway_address,
                    "gateway_port": gateway_port,
                    "sender_hotkey": sender_hotkey,
                    "filename": filename,
                    "total_chunks": total_chunks,
                    "destination_url": destination_url,
                    "receiver_filename": receiver_filename,
                }
            )
            logger.info(
                f"Sent pull task {task_id[:16]}... to worker {worker_id} (gateway {gateway_address}:{gateway_port}, dest={destination_url or 'none'})"
            )

            # Build execution context for real data transfer
            gateway_url = f"http://{gateway_address}:{gateway_port}"
            execution_context = {
                "transfer_id": transfer_id,
                "stream_id": transfer_id,  # Use transfer_id as stream_id
                "gateway_url": gateway_url,
                "destination_url": destination_url or "",
                "chunk_indices": [chunk_index] if chunk_index is not None else [],
                "source_type": "http",
            }
            await self._save_task_to_core(
                task_id=task_id,
                worker_id=worker_id,
                chunk_hash=chunk_hash,
                execution_context=execution_context,
            )
            return True
        except Exception as e:
            logger.error(f"Failed to send pull task to worker {worker_id}: {e}")
            return False

    async def broadcast_task_offer(
        self,
        task_id: str,
        chunk_data: bytes,
        chunk_index: int,
        chunk_hash: str,
        transfer_id: str,
        source_region: str = "",
        dest_region: str = "",
        estimated_reward: float = 0.0,
        timeout_seconds: float = 5.0,
        deadline_us: int = 0,
        canary: bytes = b"",
        canary_offset: int = 0,
        destination_url: Optional[str] = None,
        sender_hotkey: Optional[str] = None,
        filename: Optional[str] = None,
        total_chunks: Optional[int] = None,
        receiver_filename: Optional[str] = None,
    ) -> Optional[str]:
        """Broadcast a task offer to ALL connected workers."""
        from .orchestrator import PendingOffer

        connected_workers = list(self.worker_manager.worker_connections.keys())
        if not connected_workers:
            logger.warning("No connected workers to broadcast offer")
            return None

        offer_id = str(uuid.uuid4())[:16]
        offer = PendingOffer(
            offer_id=offer_id,
            task_id=task_id,
            chunk_size=len(chunk_data),
            chunk_hash=chunk_hash,
            source_region=source_region,
            dest_region=dest_region,
            estimated_reward=estimated_reward,
            chunk_data=chunk_data,
            chunk_index=chunk_index,
            transfer_id=transfer_id,
            destination_url=destination_url,
            sender_hotkey=sender_hotkey,
            filename=filename,
            total_chunks=total_chunks,
            receiver_filename=receiver_filename,
            timeout_seconds=timeout_seconds,
            deadline_us=deadline_us,
            canary=canary,
            canary_offset=canary_offset,
        )

        self.pending_offers[offer_id] = offer

        broadcast_count = 0
        for worker_id in connected_workers:
            websocket = self.worker_manager.worker_connections.get(worker_id)
            if not websocket:
                continue
            try:
                await websocket.send_json(
                    {
                        "type": "task_offer",
                        "offer_id": offer_id,
                        "task_id": task_id,
                        "transfer_id": transfer_id,
                        "chunk_index": chunk_index,
                        "chunk_hash": chunk_hash,
                        "chunk_size": len(chunk_data),
                        "source_region": source_region,
                        "dest_region": dest_region,
                        "estimated_reward": estimated_reward,
                        "timeout_seconds": timeout_seconds,
                        "filename": filename,
                        "total_chunks": total_chunks,
                    }
                )
                offer.workers_offered.add(worker_id)
                broadcast_count += 1
            except Exception as e:
                logger.warning(f"Failed to send offer to worker {worker_id}: {e}")

        if broadcast_count == 0:
            del self.pending_offers[offer_id]
            logger.warning("Failed to broadcast offer to any worker")
            return None

        logger.info(
            f"Broadcast offer {offer_id} for task {task_id[:16]}... "
            f"to {broadcast_count} workers ({len(chunk_data)} bytes)"
        )

        asyncio.create_task(self._offer_timeout_handler(offer_id, timeout_seconds))
        return offer_id

    async def accept_task_offer(
        self,
        offer_id: str,
        worker_id: str,
    ) -> Tuple[bool, Optional[bytes], Optional[dict]]:
        """Handle a worker accepting a task offer."""
        from .orchestrator import BandwidthTask

        async with self._offer_lock:
            offer = self.pending_offers.get(offer_id)
            if not offer:
                logger.warning(f"Worker {worker_id} tried to accept unknown offer {offer_id}")
                return (False, None, None)

            if not offer.is_available:
                reason = (
                    "expired" if offer.is_expired else f"already accepted by {offer.accepted_by}"
                )
                logger.info(f"Worker {worker_id} too late for offer {offer_id} ({reason})")
                return (False, None, None)

            offer.status = "accepted"
            offer.accepted_by = worker_id
            offer.accepted_at = time.time()

            logger.info(f"Worker {worker_id} won offer {offer_id} for task {offer.task_id[:16]}...")

            task = BandwidthTask(
                task_id=offer.task_id,
                worker_id=worker_id,
                chunk_size=offer.chunk_size,
                chunk_hash=offer.chunk_hash,
                source_region=offer.source_region,
                dest_region=offer.dest_region,
                created_at=offer.created_at,
                deadline_us=offer.deadline_us,
                canary=offer.canary,
                canary_offset=offer.canary_offset,
                status="in_progress",
                started_at=time.time(),
            )
            self.active_tasks[offer.task_id] = task

            worker = self.worker_manager.workers.get(worker_id)
            if worker:
                worker.active_tasks += 1
                worker.total_tasks += 1

            await self._save_task_to_core(
                task_id=offer.task_id,
                worker_id=worker_id,
                chunk_size=offer.chunk_size,
                chunk_hash=offer.chunk_hash,
                deadline_us=offer.deadline_us,
                source_region=offer.source_region,
                dest_region=offer.dest_region,
            )

            metadata = {
                "task_id": offer.task_id,
                "transfer_id": offer.transfer_id,
                "chunk_index": offer.chunk_index,
                "chunk_hash": offer.chunk_hash,
                "destination_url": offer.destination_url,
                "sender_hotkey": offer.sender_hotkey,
                "filename": offer.filename,
                "total_chunks": offer.total_chunks,
                "receiver_filename": offer.receiver_filename,
            }

            return (True, offer.chunk_data, metadata)

    async def reject_task_offer(self, offer_id: str, worker_id: str, reason: str = "") -> None:
        """Handle a worker rejecting a task offer."""
        offer = self.pending_offers.get(offer_id)
        if not offer:
            return
        offer.workers_rejected.add(worker_id)
        logger.debug(f"Worker {worker_id} rejected offer {offer_id}: {reason or 'no reason'}")

    async def _offer_timeout_handler(self, offer_id: str, timeout_seconds: float) -> None:
        """Handle offer expiration after timeout."""
        await asyncio.sleep(timeout_seconds)

        async with self._offer_lock:
            offer = self.pending_offers.get(offer_id)
            if not offer:
                return
            if offer.status == "pending":
                offer.status = "expired"
                logger.warning(
                    f"Offer {offer_id} expired - no workers accepted "
                    f"(offered to {len(offer.workers_offered)}, "
                    f"rejected by {len(offer.workers_rejected)})"
                )
                await self._notify_offer_result(offer_id, None, "expired")
            del self.pending_offers[offer_id]

    async def _notify_offer_result(
        self,
        offer_id: str,
        winner_id: Optional[str],
        status: str,
    ) -> None:
        """Notify all workers about offer result."""
        offer = self.pending_offers.get(offer_id)
        if not offer:
            return

        for worker_id in offer.workers_offered:
            if worker_id == winner_id:
                continue
            websocket = self.worker_manager.worker_connections.get(worker_id)
            if not websocket:
                continue
            try:
                await websocket.send_json(
                    {
                        "type": "task_assigned",
                        "offer_id": offer_id,
                        "task_id": offer.task_id,
                        "status": status,
                    }
                )
            except Exception:
                pass

    def _select_best_worker(
        self,
        candidates: List[Any],
        source_region: str,
        dest_region: str,
    ) -> Optional[Any]:
        """Select the best worker for a task using multi-factor scoring.

        Worker selection is based on available performance metrics from SubnetCore:
        - trust_score: Worker trust score
        - success_rate: Historical task success rate
        - bandwidth_mbps: Current bandwidth from the latest worker stats snapshot
        - load_factor: Current task load

        Note: Region is not available (worker anonymity) so geo_score is neutral.
        """
        if not candidates:
            return None

        scored = []
        for worker in candidates:
            trust_score = worker.trust_score
            load_score = 1.0 - worker.load_factor
            # Use bandwidth_mbps directly if bandwidth_ema not set
            bandwidth = worker.bandwidth_ema if worker.bandwidth_ema > 0 else worker.bandwidth_mbps
            bandwidth_score = min(1.0, bandwidth / 1000.0)
            success_score = worker.success_rate

            # Geo scoring disabled - worker region is anonymous
            # Use neutral score of 0.5 for all workers
            geo_score = 0.5

            final_score = (
                self.settings.weight_trust * trust_score
                + self.settings.weight_latency * geo_score
                + self.settings.weight_load * load_score
                + self.settings.weight_bandwidth * bandwidth_score
                + self.settings.weight_success * success_score
            )
            scored.append((worker, final_score))

        scored.sort(key=lambda x: x[1], reverse=True)
        return scored[0][0] if scored else None
