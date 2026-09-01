"""Construction of orchestrator-owned mutable state and manager façades."""

import asyncio
from datetime import datetime
from typing import Any, Dict, List, Optional

from .config import OrchestratorSettings, get_settings
from .epoch_manager import EpochManager
from .models import EpochSummary
from .reward_manager import RewardManager
from .task_scheduler import TaskScheduler
from .worker_gateway import WorkerGateway
from .worker_manager import WorkerManager


def initialize_orchestrator_state(orchestrator, settings: Optional[OrchestratorSettings] = None) -> None:
    orchestrator.settings = settings or get_settings()

    # Bittensor (for signing and validator communication)
    orchestrator.wallet: Optional[bt.wallet] = None
    orchestrator.subtensor: Optional[bt.subtensor] = None
    orchestrator.metagraph: Optional[bt.metagraph] = None
    orchestrator.hotkey: Optional[str] = None

    # --- Manager instances ---
    orchestrator._worker_mgr = WorkerManager(orchestrator.settings, lambda: orchestrator.subnet_core_client)
    orchestrator._task_sched = TaskScheduler(
        orchestrator.settings, orchestrator._worker_mgr, get_subnet_core_client=lambda: orchestrator.subnet_core_client
    )
    orchestrator._reward_mgr = RewardManager(orchestrator.settings)
    orchestrator._epoch_mgr = EpochManager(orchestrator.settings)
    # BlindWorkerManager removed
    # GatewayManager removed

    # --- Expose manager state as public attributes (backward compat) ---
    # Worker state (from WorkerManager)
    orchestrator.workers = orchestrator._worker_mgr.workers
    orchestrator.workers_by_hotkey = orchestrator._worker_mgr.workers_by_hotkey
    orchestrator.workers_by_region = orchestrator._worker_mgr.workers_by_region
    orchestrator.worker_connections = orchestrator._worker_mgr.worker_connections

    # Task state (from TaskScheduler)
    orchestrator.active_tasks = orchestrator._task_sched.active_tasks
    orchestrator.completed_tasks = orchestrator._task_sched.completed_tasks

    # Epoch tracking
    orchestrator.current_epoch: int = 0
    orchestrator.epoch_start_time: datetime = datetime.utcnow()
    orchestrator.epoch_summaries: Dict[int, EpochSummary] = {}

    # Note: Validator tracking removed - BeamCore handles PRISM evidence centrally

    # Statistics
    orchestrator.total_bytes_relayed: int = 0
    orchestrator.total_tasks_completed: int = 0

    # Reward tracking (delegate to reward manager but expose)
    orchestrator.our_uid: Optional[int] = None

    # SubnetCoreClient for API-based data operations
    orchestrator.subnet_core_client: Optional[Any] = None

    # In-process worker gateway (workers dial /ws/{worker_id})
    orchestrator.worker_gateway: WorkerGateway = WorkerGateway(
        on_ready_change=orchestrator._on_worker_gateway_ready_change,
    )

    # Async control
    orchestrator._running: bool = False
    orchestrator._background_tasks: List[asyncio.Task] = []
    orchestrator._chain_rpc_lock = asyncio.Lock()

    # Orchestrator manager for incentive mechanism
    orchestrator.orch_manager: Optional[Any] = None
