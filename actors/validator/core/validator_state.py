"""Construction of validator-owned mutable state."""

import asyncio
from typing import Dict, List, Optional

import aiohttp
import bittensor as bt
from chain import FiberChain, FiberNode
from clients import SubnetCoreClient
from core._beam_stubs import (
    PoBVerificationResult,
    ReassignmentManager,
    Task,
    WorkerRegistry,
    get_sybil_detector,
)
from core.config import Settings, get_settings
from core.models import (
    ChallengeResult,
    OrchestratorInfo,
    PoBVerificationStats,
    SpotCheckResult,
    WorkSummary,
)
from core.redundancy import CheckpointManager, HealthMonitor, RecoveryManager


def initialize_validator_state(validator, settings: Optional[Settings] = None) -> None:
    validator.settings = settings or get_settings()

    # Bittensor components
    validator.wallet: Optional[bt.Wallet] = None
    validator.subtensor: Optional[bt.Subtensor] = None
    validator.metagraph: Optional[bt.Metagraph] = None
    validator.dendrite: Optional[bt.Dendrite] = None

    # Fiber chain interface (for weight setting and node discovery)
    validator.fiber_chain: Optional[FiberChain] = None
    validator._fiber_nodes: Dict[str, FiberNode] = {}  # hotkey -> FiberNode cache

    # Validator state
    validator.uid: Optional[int] = None
    validator.hotkey: Optional[str] = None
    validator.is_registered: bool = False

    # Connection tracking (for dendrite mode)
    validator.connections: Dict[int, dict] = {}  # uid -> connection info
    validator.connection_scores: Dict[int, float] = {}  # uid -> score
    validator.connection_bandwidth: Dict[int, float] = {}  # uid -> bandwidth EMA

    # Orchestrator tracking (for HTTP mode)
    validator.orchestrators: Dict[str, OrchestratorInfo] = {}
    validator._beamcore_worker_counts: Dict[int, int] = {}  # uid -> worker_count from BeamCore

    # =====================================================================
    # Orchestrator and Worker Management
    # =====================================================================
    validator.worker_registry = WorkerRegistry()
    validator.reassignment_manager = ReassignmentManager(validator.worker_registry)

    # Local score cache; BeamCore Prism remains authoritative.
    validator.orchestrator_scores: Dict[str, float] = {}  # hotkey -> final score
    validator.payment_penalty_multipliers: Dict[str, float] = (
        {}
    )  # hotkey -> multiplier (1.0 = no penalty)

    # Work summaries (rolling 24h window)
    validator.work_summaries: Dict[str, WorkSummary] = {}  # hotkey -> last summary
    validator.work_summary_history: Dict[str, List[WorkSummary]] = {}  # hotkey -> history

    # Sybil Detection
    validator.sybil_detector = get_sybil_detector()

    # Orchestrator performance tracking (rolling window)
    validator.orchestrator_metrics: Dict[int, Dict] = {}  # uid -> metrics accumulator
    validator.worker_metrics: Dict[str, Dict] = {}  # worker_id -> metrics accumulator

    # Sybil tracking per orchestrator
    validator.sybil_penalties: Dict[int, float] = {}  # uid -> sybil penalty multiplier (0.0-1.0)

    # Redundancy and failover
    validator.health_monitor: Optional[HealthMonitor] = None
    validator.checkpoint_manager: Optional[CheckpointManager] = None
    validator.recovery_manager: Optional[RecoveryManager] = None

    # Penalty tracking
    validator.total_redirected_to_one: float = 0.0  # TAO redirected to #1 this epoch
    validator.fraud_penalties: Dict[str, float] = {}  # hotkey -> fraud penalty multiplier

    # Task tracking
    validator.pending_tasks: Dict[str, Task] = {}  # task_id -> Task
    validator.task_results: Dict[str, PoBVerificationResult] = {}  # task_id -> result

    # Challenge tracking
    validator.active_challenges: Dict[str, dict] = {}  # task_id -> challenge info
    validator.challenge_results: Dict[str, ChallengeResult] = {}  # task_id -> result

    # Proof spot-checking
    validator.spot_check_results: Dict[str, SpotCheckResult] = {}  # hotkey -> last result
    validator.spot_check_history: List[SpotCheckResult] = []

    # Local proof verification stats
    validator.pob_verification_results: Dict[str, PoBVerificationStats] = {}  # hotkey -> stats

    # Weight history
    validator.last_weight_block: int = 0
    validator.weights_history: List[Dict] = []
    validator._chain_weights_rate_limit: int = 0  # cached from subtensor at startup

    # Penalty history
    validator.penalty_history: List[dict] = []

    # Epoch tracking for emissions
    validator.current_epoch: int = 0
    validator.epoch_start_block: int = 0
    validator.tasks_this_epoch: int = 0
    validator.last_emission_check_block: int = 0

    # HTTP session for local mode
    validator._http_session: Optional[aiohttp.ClientSession] = None

    # SubnetCore API client for score submission (set by main.py)
    validator.subnet_core_client: Optional[SubnetCoreClient] = None

    # Async control
    validator._running: bool = False
    validator._main_loop_task: Optional[asyncio.Task] = None
