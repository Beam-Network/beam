"""
BEAM Orchestrator Core

Central coordinator for the BEAM decentralized bandwidth network.
Facade that delegates to specialized manager classes.

Architecture:
┌─────────────────────────────────────────────────────────────────────┐
│                        ORCHESTRATOR                                  │
│                  (Subnet-operated, NOT a miner)                      │
│                                                                      │
│  ┌─────────────┐  ┌─────────────┐  ┌─────────────┐                  │
│  │   Worker    │  │    Task     │  │   Result    │                  │
│  │  Registry   │  │  Scheduler  │  │ Aggregator  │                  │
│  └─────────────┘  └─────────────┘  └─────────────┘                  │
│         │                │                │                          │
│         └────────────────┴────────────────┘                          │
│                          │                                           │
│  ┌───────────────────────┴───────────────────────┐                  │
│  │              Work Coordinator                  │                  │
│  └───────────────────────────────────────────────┘                  │
│                          │                                           │
└──────────────────────────┼──────────────────────────────────────────┘
                           │
           ┌───────────────┼───────────────┐
           ▼               ▼               ▼
    ┌──────────┐    ┌──────────┐    ┌──────────┐
    │ Worker 1 │    │ Worker 2 │    │ Worker N │  (Off-chain, unlimited)
    └──────────┘    └──────────┘    └──────────┘
           │               │               │
           └───────────────┴───────────────┘
                           │
                           ▼
                    ┌──────────────┐
                    │  Validators  │  (On-chain, ~64 UIDs)
                    │  (verify &   │
                    │  set weights)│
                    └──────────────┘

Key differences from Connection model:
1. Workers register directly with Orchestrator (no miner slot needed)
2. Orchestrator aggregates ALL work and reports to validators
3. Validators verify aggregated proofs and score the Orchestrator
4. Single emission distribution path vs multiple competing miners
"""

import asyncio
import logging
import time
from datetime import datetime, timedelta
from typing import Any, Dict, List, Optional

import bittensor as bt

from .config import OrchestratorSettings, get_settings
from .epoch_manager import EpochManager
from .models import BandwidthTask, EpochSummary, Worker, WorkerStatus
from . import bootstrap, chain_runtime, lifecycle, orchestrator_state, orchestrator_views
from .reward_manager import RewardManager
from .task_scheduler import TaskScheduler
from .worker_gateway import WorkerGateway
from .worker_manager import WorkerManager

# BlindWorkerManager removed
# GatewayManager removed


# SubnetCoreClient imports (BeamCore contract)
try:
    from clients import (
        SubnetCoreClient,
        close_subnet_core_client,
        init_subnet_core_client,
    )

    SUBNET_CORE_CLIENT_AVAILABLE = True
except ImportError:
    SUBNET_CORE_CLIENT_AVAILABLE = False
    SubnetCoreClient = None

logger = logging.getLogger(__name__)

SUBTENSOR_INIT_MAX_ATTEMPTS = 5
SUBTENSOR_INIT_BASE_DELAY_SECONDS = 2.0


# =============================================================================
# Orchestrator Core (Facade)
# =============================================================================


class Orchestrator:
    """
    BEAM Orchestrator - Central coordinator for bandwidth mining.

    Facade that delegates to specialized manager classes while preserving
    the public interface used by route handlers.
    """

    def __init__(self, settings: Optional[OrchestratorSettings] = None):
        orchestrator_state.initialize_orchestrator_state(self, settings)
    # --- Reward-share tracking properties (delegated to RewardManager) ---
    @property
    def last_emission_check(self) -> float:
        return self._reward_mgr.last_emission_check

    @last_emission_check.setter
    def last_emission_check(self, value: float):
        self._reward_mgr.last_emission_check = value

    @property
    def epoch_start_emission(self) -> float:
        return self._reward_mgr.epoch_start_emission

    @epoch_start_emission.setter
    def epoch_start_emission(self, value: float):
        self._reward_mgr.epoch_start_emission = value

    @property
    def total_rewards_distributed(self) -> float:
        return self._reward_mgr.total_rewards_distributed

    @total_rewards_distributed.setter
    def total_rewards_distributed(self, value: float):
        self._reward_mgr.total_rewards_distributed = value

    def _on_worker_gateway_ready_change(self, ready: bool) -> None:
        """Toggle orchestrator readiness when the first/last worker connects."""
        if self.subnet_core_client is None:
            return
        import asyncio
        try:
            loop = asyncio.get_event_loop()
            if loop.is_running():
                asyncio.create_task(self.subnet_core_client.set_ready(ready))
        except Exception as exc:
            logger.warning("ready-change signal failed: %s", exc)

    # =========================================================================
    # Lifecycle
    # =========================================================================

    async def initialize(self) -> None:
        await lifecycle.initialize(self)

    def _initialize_subtensor_and_metagraph_with_retry(self) -> None:
        lifecycle._initialize_subtensor_and_metagraph_with_retry(self)

    async def start(self) -> None:
        await lifecycle.start(self)

    async def stop(self) -> None:
        await lifecycle.stop(self)
    # =========================================================================
    # Worker Management (delegate to WorkerManager)
    # =========================================================================

    async def register_worker(self, hotkey, ip, port, region, bandwidth_mbps=0.0):
        return await self._worker_mgr.register_worker(
            hotkey,
            ip,
            port,
            region,
            bandwidth_mbps,
            subnet_core_client=self.subnet_core_client,
        )

    async def deregister_worker(self, worker_id):
        return await self._worker_mgr.deregister_worker(worker_id)

    def get_worker(self, worker_id):
        return self._worker_mgr.get_worker(worker_id)

    async def get_available_workers(self, region=None, min_bandwidth=0.0):
        return await self._worker_mgr.get_available_workers(region, min_bandwidth)

    def register_worker_connection(self, worker_id, websocket):
        self._worker_mgr.register_worker_connection(worker_id, websocket)

    def unregister_worker_connection(self, worker_id):
        self._worker_mgr.unregister_worker_connection(worker_id)

    # =========================================================================
    # Task Management (delegate to TaskScheduler)
    # =========================================================================

    async def assign_task(
        self,
        task_id,
        chunk_size,
        chunk_hash,
        source_region,
        dest_region,
        deadline_us,
        canary,
        canary_offset,
    ):
        return await self._task_sched.assign_task(
            task_id,
            chunk_size,
            chunk_hash,
            source_region,
            dest_region,
            deadline_us,
            canary,
            canary_offset,
        )

    async def send_task_to_worker(
        self,
        worker_id,
        task_id,
        chunk_data,
        chunk_index,
        chunk_hash,
        transfer_id,
        destination_url=None,
        sender_hotkey=None,
        filename=None,
        total_chunks=None,
        receiver_filename=None,
    ):
        return await self._task_sched.send_task_to_worker(
            worker_id,
            task_id,
            chunk_data,
            chunk_index,
            chunk_hash,
            transfer_id,
            destination_url,
            sender_hotkey,
            filename,
            total_chunks,
            receiver_filename,
        )

    async def send_pull_task_to_worker(
        self,
        worker_id,
        task_id,
        chunk_index,
        chunk_hash,
        transfer_id,
        gateway_address,
        gateway_port,
        sender_hotkey=None,
        filename=None,
        total_chunks=None,
        destination_url=None,
        receiver_filename=None,
    ):
        return await self._task_sched.send_pull_task_to_worker(
            worker_id,
            task_id,
            chunk_index,
            chunk_hash,
            transfer_id,
            gateway_address,
            gateway_port,
            sender_hotkey,
            filename,
            total_chunks,
            destination_url,
            receiver_filename,
        )

    async def fail_task(self, task_id: str, reason: str) -> None:
        """Record task failure."""
        task = self.active_tasks.get(task_id)
        if not task:
            return

        worker = self.workers.get(task.worker_id)
        if worker:
            worker.active_tasks = max(0, worker.active_tasks - 1)
            worker.failed_tasks += 1
            worker.update_success_rate()
            worker.trust_score = max(0.0, worker.trust_score - 0.01)

        task.status = "failed"
        del self.active_tasks[task_id]

        logger.warning(f"Task {task_id[:16]}... failed: {reason}")

    # =========================================================================
    # Local reward-share accounting (delegate to RewardManager)
    # =========================================================================

    def distribute_rewards_at_epoch_end(self) -> Dict[str, float]:
        return self._reward_mgr.distribute_rewards_at_epoch_end(
            self.workers.values(), self.get_our_emission
        )

    def distribute_rewards_to_workers(self) -> Dict[str, float]:
        return self._reward_mgr.distribute_rewards_to_workers(self.get_our_emission)

    # =========================================================================
    # Metagraph & Validators
    # =========================================================================

    def _find_our_uid(self) -> None:
        chain_runtime._find_our_uid(self)

    def get_our_emission(self) -> float:
        return chain_runtime.get_our_emission(self)

    async def _run_chain_rpc(self, label: str, func):
        return await chain_runtime._run_chain_rpc(self, label, func)

    def _sync_metagraph_from_chain(self) -> None:
        chain_runtime._sync_metagraph_from_chain(self)

    async def _sync_metagraph_from_chain_async(self) -> None:
        await chain_runtime._sync_metagraph_from_chain_async(self)

    def _refresh_subnet_price_cache(self) -> None:
        chain_runtime._refresh_subnet_price_cache(self)

    async def _refresh_subnet_price_cache_async(self) -> None:
        await chain_runtime._refresh_subnet_price_cache_async(self)

    async def _sync_epoch_from_chain_async(self) -> None:
        await chain_runtime._sync_epoch_from_chain_async(self)

    async def _metagraph_sync_loop(self) -> None:
        await chain_runtime._metagraph_sync_loop(self)

    def _sync_epoch_from_chain(self) -> None:
        chain_runtime._sync_epoch_from_chain(self)

    async def _epoch_management_loop(self) -> None:
        await chain_runtime._epoch_management_loop(self)

    async def _advance_epoch(self) -> None:
        await chain_runtime._advance_epoch(self)
    # BeamCore owns stalled chunk recovery server-side.

    # =========================================================================
    # State & Metrics
    # =========================================================================

    def _build_epoch_summary(self) -> EpochSummary:
        return orchestrator_views._build_epoch_summary(self)

    def get_state(self) -> dict:
        return orchestrator_views.get_state(self)

    def get_worker_stats(self) -> List[dict]:
        return orchestrator_views.get_worker_stats(self)

    def get_epoch_stats(self, epoch: Optional[int] = None) -> Optional[dict]:
        return orchestrator_views.get_epoch_stats(self, epoch)
    # =========================================================================
    # Utilities
    # =========================================================================

    def _generate_worker_id(self, hotkey: str, ip: str, port: int) -> str:
        return bootstrap._generate_worker_id(self, hotkey, ip, port)

    async def _init_subnet_core_client(self) -> None:
        await bootstrap._init_subnet_core_client(
            self,
            SUBNET_CORE_CLIENT_AVAILABLE,
            init_subnet_core_client,
        )

    async def _init_orch_manager(self) -> None:
        await bootstrap._init_orch_manager(self)

    async def _add_local_mock_worker(self) -> None:
        await bootstrap._add_local_mock_worker(self)

# =============================================================================
# Singleton
# =============================================================================

_orchestrator: Optional[Orchestrator] = None


def get_orchestrator() -> Orchestrator:
    """Get the global Orchestrator instance."""
    global _orchestrator
    if _orchestrator is None:
        _orchestrator = Orchestrator()
    return _orchestrator
