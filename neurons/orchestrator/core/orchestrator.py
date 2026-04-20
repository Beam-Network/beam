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
│  │   Worker    │  │    Task     │  │    Proof    │                  │
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
from collections import defaultdict
from dataclasses import dataclass, field
from datetime import datetime, timedelta
from typing import Any, Dict, List, Optional, Set, Tuple
from enum import Enum

import bittensor as bt

from .config import OrchestratorSettings, get_settings
from .worker_manager import WorkerManager
from .task_scheduler import TaskScheduler
from .proof_aggregator import ProofAggregator
from .reward_manager import RewardManager
from .epoch_manager import EpochManager
# BlindWorkerManager removed
# GatewayManager removed

# Database imports (optional - legacy, being replaced by SubnetCoreClient)
try:
    from db.database import Database, init_database, close_database
    from db.merkle import MerkleTree, create_payment_merkle_tree
    DB_AVAILABLE = True
except ImportError:
    DB_AVAILABLE = False


# SubnetCoreClient imports (for API-based data operations)
try:
    from clients import (
        SubnetCoreClient,
        PoBSubmission,
        WorkerRegistration,
        WorkerUpdate,
        WorkerPaymentData,
        EpochPaymentData,
        TaskCreate,
        TaskUpdate,
        ReceiverCodeData,
        init_subnet_core_client,
        get_subnet_core_client,
        close_subnet_core_client,
        # Blind worker data classes
        BlindSessionValidation,
        BlindTrustScore,
        BlindTrustReport,
        BlindPaymentRequest,
    )
    SUBNET_CORE_CLIENT_AVAILABLE = True
except ImportError:
    SUBNET_CORE_CLIENT_AVAILABLE = False
    SubnetCoreClient = None
    BlindTrustReport = None
    BlindPaymentRequest = None

logger = logging.getLogger(__name__)


# =============================================================================
# Data Models
# =============================================================================

class WorkerStatus(Enum):
    """Worker lifecycle status."""
    PENDING = "pending"           # Registered, awaiting verification
    ACTIVE = "active"             # Verified and accepting tasks
    SUSPENDED = "suspended"       # Temporarily disabled (failed checks)
    OFFLINE = "offline"           # Heartbeat timeout
    BANNED = "banned"             # Permanently banned (fraud detected)


@dataclass
class Worker:
    """
    Registered worker in the Orchestrator network.

    Workers are NOT on-chain entities - they register directly with the
    Orchestrator and earn rewards through verified bandwidth work.
    """
    worker_id: str
    hotkey: str
    ip: str
    port: int
    region: str

    # Geographic coordinates (set during verification)
    latitude: Optional[float] = None
    longitude: Optional[float] = None

    # Status
    status: WorkerStatus = WorkerStatus.PENDING
    registered_at: datetime = field(default_factory=datetime.utcnow)
    last_seen: datetime = field(default_factory=datetime.utcnow)

    # Performance metrics
    bandwidth_mbps: float = 0.0
    bandwidth_ema: float = 0.0
    latency_ms: float = 0.0
    success_rate: float = 1.0

    # Task tracking
    total_tasks: int = 0
    successful_tasks: int = 0
    failed_tasks: int = 0
    active_tasks: int = 0  # Local: tasks this orchestrator assigned
    global_pending_tasks: int = 0  # Global: tasks across ALL orchestrators (from BeamCore)
    max_concurrent_tasks: int = 10

    # Bytes relayed
    bytes_relayed_total: int = 0
    bytes_relayed_epoch: int = 0

    # Trust scoring
    trust_score: float = 0.5  # Starts at neutral
    fraud_score: float = 0.0  # Higher = more suspicious

    # Reward tracking
    rewards_earned_epoch: int = 0
    rewards_earned_total: int = 0

    @property
    def is_available(self) -> bool:
        """Check if worker can accept new tasks."""
        return (
            self.status == WorkerStatus.ACTIVE and
            self.active_tasks < self.max_concurrent_tasks
        )

    @property
    def load_factor(self) -> float:
        """Current load as fraction of capacity, using max of local and global counts."""
        if self.max_concurrent_tasks == 0:
            return 1.0
        # Use max of local active_tasks and global pending_tasks from BeamCore
        # This ensures we see tasks assigned by OTHER orchestrators too
        effective_tasks = max(self.active_tasks, self.global_pending_tasks)
        return effective_tasks / self.max_concurrent_tasks

    def update_bandwidth_ema(self, bandwidth: float, alpha: float = 0.3) -> None:
        """Update bandwidth EMA with new measurement."""
        if self.bandwidth_ema == 0:
            self.bandwidth_ema = bandwidth
        else:
            self.bandwidth_ema = alpha * bandwidth + (1 - alpha) * self.bandwidth_ema

    def update_success_rate(self) -> None:
        """Recalculate success rate."""
        if self.total_tasks > 0:
            self.success_rate = self.successful_tasks / self.total_tasks


@dataclass
class BandwidthTask:
    """A bandwidth relay task assigned to a worker."""
    task_id: str
    worker_id: str

    # Task details
    chunk_size: int
    chunk_hash: str
    source_region: str
    dest_region: str

    # Timing
    created_at: float  # Unix timestamp
    deadline_us: int   # Microsecond deadline
    started_at: Optional[float] = None
    completed_at: Optional[float] = None

    # Status
    status: str = "pending"  # pending, in_progress, completed, failed, timeout

    # Anti-cheat
    canary: bytes = field(default_factory=bytes)
    canary_offset: int = 0

    # Results
    bytes_relayed: int = 0
    bandwidth_mbps: float = 0.0
    latency_ms: float = 0.0


@dataclass
class PendingOffer:
    """
    Tracks a task offer broadcast to all workers.

    The broadcast offer model:
    1. Orchestrator broadcasts offer to ALL connected workers
    2. First worker to accept wins (atomic)
    3. Non-winners receive TASK_ASSIGNED notification
    4. If no one accepts within timeout, offer expires
    """
    offer_id: str
    task_id: str

    # Task preview info (sent to workers - no actual chunk data)
    chunk_size: int
    chunk_hash: str
    source_region: str
    dest_region: str
    estimated_reward: float = 0.0  # Estimated dTAO reward

    # The actual chunk data (only sent to winner)
    chunk_data: bytes = field(default_factory=bytes, repr=False)
    chunk_index: int = 0
    transfer_id: str = ""
    destination_url: Optional[str] = None
    sender_hotkey: Optional[str] = None
    filename: Optional[str] = None
    total_chunks: Optional[int] = None
    receiver_filename: Optional[str] = None

    # Timing
    created_at: float = field(default_factory=time.time)
    timeout_seconds: float = 5.0  # How long workers have to accept
    deadline_us: int = 0

    # Anti-cheat (for winner's task)
    canary: bytes = field(default_factory=bytes, repr=False)
    canary_offset: int = 0

    # State
    status: str = "pending"  # pending, accepted, expired, cancelled
    accepted_by: Optional[str] = None  # worker_id of winner
    accepted_at: Optional[float] = None

    # Track which workers received the offer
    workers_offered: Set[str] = field(default_factory=set)
    workers_rejected: Set[str] = field(default_factory=set)

    @property
    def is_expired(self) -> bool:
        """Check if offer has timed out."""
        return time.time() > (self.created_at + self.timeout_seconds)

    @property
    def is_available(self) -> bool:
        """Check if offer can still be accepted."""
        return self.status == "pending" and not self.is_expired


@dataclass
class BandwidthProof:
    """Proof of bandwidth work completed by a worker."""
    task_id: str
    worker_id: str
    worker_hotkey: str

    # Timing (microseconds)
    start_time_us: int
    end_time_us: int

    # Metrics
    bytes_relayed: int
    bandwidth_mbps: float

    # Verification
    chunk_hash: str
    canary_proof: str

    # Signatures
    worker_signature: str = ""
    orchestrator_signature: str = ""
    worker_coldkey: str = ""

    # Metadata
    source_region: str = ""
    dest_region: str = ""
    timestamp: datetime = field(default_factory=datetime.utcnow)

    @property
    def duration_ms(self) -> float:
        """Task duration in milliseconds."""
        return (self.end_time_us - self.start_time_us) / 1000


@dataclass
class EpochSummary:
    """Aggregated work summary for a validation epoch."""
    epoch: int
    start_time: datetime
    end_time: datetime

    # Aggregated metrics
    total_tasks: int = 0
    successful_tasks: int = 0
    failed_tasks: int = 0

    total_bytes_relayed: int = 0
    total_bandwidth_seconds: float = 0.0

    # Worker participation
    active_workers: int = 0
    worker_contributions: Dict[str, int] = field(default_factory=dict)  # worker_id -> bytes

    # Proof aggregation
    proof_count: int = 0
    merkle_root: str = ""

    # Scoring inputs for validators
    avg_bandwidth_mbps: float = 0.0
    avg_latency_ms: float = 0.0
    success_rate: float = 0.0


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
        self.settings = settings or get_settings()

        # Bittensor (for signing and validator communication)
        self.wallet: Optional[bt.wallet] = None
        self.subtensor: Optional[bt.subtensor] = None
        self.metagraph: Optional[bt.metagraph] = None
        self.hotkey: Optional[str] = None

        # --- Manager instances ---
        self._worker_mgr = WorkerManager(self.settings, lambda: self.subnet_core_client)
        self._task_sched = TaskScheduler(self.settings, self._worker_mgr, get_subnet_core_client=lambda: self.subnet_core_client)
        self._proof_agg = ProofAggregator(self.settings)
        self._reward_mgr = RewardManager(self.settings)
        self._epoch_mgr = EpochManager(self.settings)
        # BlindWorkerManager removed
        # GatewayManager removed

        # --- Payment self-limiting ---
        self._consecutive_payment_failures: int = 0
        self._max_payment_failures: int = 5
        self._accepting_tasks: bool = True

        # --- Expose manager state as public attributes (backward compat) ---
        # Worker state (from WorkerManager)
        self.workers = self._worker_mgr.workers
        self.workers_by_hotkey = self._worker_mgr.workers_by_hotkey
        self.workers_by_region = self._worker_mgr.workers_by_region
        self.worker_connections = self._worker_mgr.worker_connections

        # Task state (from TaskScheduler)
        self.active_tasks = self._task_sched.active_tasks
        self.completed_tasks = self._task_sched.completed_tasks
        self.pending_offers = self._task_sched.pending_offers
        self._offer_lock = self._task_sched._offer_lock

        # Proof state (from ProofAggregator)
        self.pending_proofs = self._proof_agg.pending_proofs
        self.epoch_proofs = self._proof_agg.epoch_proofs

        # Blind worker state (BlindWorkerManager removed - stubbed)
        self.blind_workers = {}
        self.blind_worker_connections = {}

        # Gateway state (GatewayManager removed - stubbed)
        self.gateway_connections = {}
        self.gateways = {}
        self.receiver_codes = {}
        self.worker_receiver_codes = {}

        # Epoch tracking
        self.current_epoch: int = 0
        self.epoch_start_time: datetime = datetime.utcnow()
        self.epoch_summaries: Dict[int, EpochSummary] = {}

        # Note: Validator tracking removed - BeamCore handles PoB centrally

        # Statistics
        self.total_bytes_relayed: int = 0
        self.total_tasks_completed: int = 0

        # Reward tracking (delegate to reward manager but expose)
        self.our_uid: Optional[int] = None

        # Database for payment proofs (initialized in initialize())
        self.db: Optional[Any] = None

        # SubnetCoreClient for API-based data operations
        self.subnet_core_client: Optional[Any] = None

        # Async control
        self._running: bool = False
        self._background_tasks: List[asyncio.Task] = []

        # Orchestrator manager for incentive mechanism
        self.orch_manager: Optional[Any] = None

    # --- Reward tracking properties (delegated to RewardManager) ---
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

    @property
    def _payment_retry_queue(self) -> list:
        return self._reward_mgr._payment_retry_queue

    # =========================================================================
    # Lifecycle
    # =========================================================================

    async def initialize(self) -> None:
        """Initialize the Orchestrator."""
        logger.info("Initializing BEAM Orchestrator...")

        # Initialize database for payment proofs
        if DB_AVAILABLE and self.settings.database_url:
            self.db = await init_database(self.settings.database_url)
            logger.info("Database initialized for payment proofs")
        else:
            if self.settings.database_url:
                logger.warning("Database module not available but database_url is set. Payment proofs will not be persisted.")
            self.db = None

        # Skip bittensor initialization in local mode
        if self.settings.local_mode:
            logger.info("Running in LOCAL MODE - skipping Bittensor wallet/subtensor initialization")
            self.wallet = None
            self.hotkey = self.settings.local_orchestrator_hotkey
            self.subtensor = None
            self.metagraph = None
            self.our_uid = 0
        else:
            # Load wallet (for signing reports)
            self.wallet = bt.Wallet(
                name=self.settings.wallet_name,
                hotkey=self.settings.wallet_hotkey,
                path=self.settings.wallet_path,
            )
            self.hotkey = self.wallet.hotkey.ss58_address
            logger.info(f"Orchestrator wallet: {self.hotkey}")

            # Connect to subtensor
            if self.settings.subtensor_address:
                self.subtensor = bt.Subtensor(network=self.settings.subtensor_address)
            else:
                self.subtensor = bt.Subtensor(network=self.settings.subtensor_network)
            logger.info(f"Connected to subtensor: {self.subtensor.network}")

            # Load metagraph (for validator discovery)
            self.metagraph = bt.Metagraph(
                netuid=self.settings.netuid,
                network=self.subtensor.network,
            )
            self.metagraph.sync(subtensor=self.subtensor)

            # Find our UID in the metagraph
            self._find_our_uid()

        # Initialize SubnetCoreClient for API-based data operations
        await self._init_subnet_core_client()

        # Skip chain-dependent initialization in local mode
        if not self.settings.local_mode:
            # Load paid task IDs to prevent double-payments after restart
            await self._reward_mgr.load_paid_tasks(self.subnet_core_client)

            # Sync epoch from chain block number so it matches the validator's numbering
            self._sync_epoch_from_chain()

            # Initialize epoch emission tracking
            self._reward_mgr.epoch_start_emission = self.get_our_emission()
            self._reward_mgr.last_emission_check = self._reward_mgr.epoch_start_emission
            logger.info(f"Initial emission: {self._reward_mgr.epoch_start_emission:.6f} ध")

            # Note: Validator discovery removed - BeamCore handles PoB centrally
        else:
            logger.info("LOCAL MODE: Skipping chain sync and emission tracking")

        # Initialize orchestrator manager for incentive mechanism
        await self._init_orch_manager()

        # Add mock worker if requested
        if self.settings.add_mock_worker:
            await self._add_local_mock_worker()
            logger.info("Added mock worker for testing")

        logger.info("Orchestrator initialized")

    async def start(self) -> None:
        """Start the Orchestrator background services."""
        self._running = True
        running = lambda: self._running

        self._background_tasks = [
            asyncio.create_task(self._metagraph_sync_loop()),
            asyncio.create_task(self._worker_mgr.worker_health_loop(running)),
            asyncio.create_task(self._worker_mgr.worker_sync_loop(running, interval_seconds=60)),
            asyncio.create_task(self._proof_agg.proof_aggregation_loop(
                running, subnet_core_client_ref=lambda: self.subnet_core_client,
            )),
            # Removed: validator_report_loop - BeamCore handles PoB centrally, validators read from BeamCore
            asyncio.create_task(self._epoch_management_loop()),
            # Removed: _stale_task_reassignment_loop - deprecated endpoint replaced by WebSocket push
        ]
        logger.info("Worker sync loop started (syncs from SubnetCore every 60s)")

        if self.settings.registry_enabled:
            self._background_tasks.append(asyncio.create_task(self._registry_loop()))

        # BlindWorkerManager removed - blind mode disabled
        # if self.settings.blind_mode_enabled:
        #     self._background_tasks.append(asyncio.create_task(self._blind_mgr.blind_trust_sync_loop(running)))
        #     logger.info("Blind worker trust sync loop started")

        logger.info("Orchestrator started")

    async def stop(self) -> None:
        """Stop the Orchestrator."""
        self._running = False

        for task in self._background_tasks:
            task.cancel()
            try:
                await task
            except asyncio.CancelledError:
                pass

        if DB_AVAILABLE and self.db:
            await close_database()
            logger.info("Database connection closed")

        if SUBNET_CORE_CLIENT_AVAILABLE and self.subnet_core_client:
            # Stop HTTP polling
            if hasattr(self.subnet_core_client, 'stop_polling'):
                await self.subnet_core_client.stop_polling()
            await close_subnet_core_client()
            logger.info("SubnetCoreClient closed")

        logger.info("Orchestrator stopped")

    # =========================================================================
    # Worker Management (delegate to WorkerManager)
    # =========================================================================

    async def register_worker(self, hotkey, ip, port, region, bandwidth_mbps=0.0):
        return await self._worker_mgr.register_worker(
            hotkey, ip, port, region, bandwidth_mbps,
            subnet_core_client=self.subnet_core_client,
        )

    async def deregister_worker(self, worker_id):
        return await self._worker_mgr.deregister_worker(worker_id)

    async def worker_heartbeat(self, worker_id, bandwidth_mbps, active_tasks, bytes_relayed=0):
        return await self._worker_mgr.worker_heartbeat(worker_id, bandwidth_mbps, active_tasks, bytes_relayed)

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

    async def assign_task(self, task_id, chunk_size, chunk_hash, source_region, dest_region, deadline_us, canary, canary_offset):
        return await self._task_sched.assign_task(task_id, chunk_size, chunk_hash, source_region, dest_region, deadline_us, canary, canary_offset)

    async def send_task_to_worker(self, worker_id, task_id, chunk_data, chunk_index, chunk_hash, transfer_id, destination_url=None, sender_hotkey=None, filename=None, total_chunks=None, receiver_filename=None):
        return await self._task_sched.send_task_to_worker(worker_id, task_id, chunk_data, chunk_index, chunk_hash, transfer_id, destination_url, sender_hotkey, filename, total_chunks, receiver_filename)

    async def send_pull_task_to_worker(self, worker_id, task_id, chunk_index, chunk_hash, transfer_id, gateway_address, gateway_port, sender_hotkey=None, filename=None, total_chunks=None, destination_url=None, receiver_filename=None):
        return await self._task_sched.send_pull_task_to_worker(worker_id, task_id, chunk_index, chunk_hash, transfer_id, gateway_address, gateway_port, sender_hotkey, filename, total_chunks, destination_url, receiver_filename)

    async def broadcast_task_offer(self, task_id, chunk_data, chunk_index, chunk_hash, transfer_id, source_region="", dest_region="", estimated_reward=0.0, timeout_seconds=5.0, deadline_us=0, canary=b"", canary_offset=0, destination_url=None, sender_hotkey=None, filename=None, total_chunks=None, receiver_filename=None):
        return await self._task_sched.broadcast_task_offer(task_id, chunk_data, chunk_index, chunk_hash, transfer_id, source_region, dest_region, estimated_reward, timeout_seconds, deadline_us, canary, canary_offset, destination_url, sender_hotkey, filename, total_chunks, receiver_filename)

    async def accept_task_offer(self, offer_id, worker_id):
        return await self._task_sched.accept_task_offer(offer_id, worker_id)

    async def reject_task_offer(self, offer_id, worker_id, reason=""):
        return await self._task_sched.reject_task_offer(offer_id, worker_id, reason)

    async def _notify_offer_result(self, offer_id, winner_id, status):
        return await self._task_sched._notify_offer_result(offer_id, winner_id, status)

    # =========================================================================
    # Gateway & Receiver Codes (GatewayManager removed - stubbed)
    # =========================================================================

    def register_gateway(self, gateway_id, address, port, hotkey=None, websocket=None):
        pass  # GatewayManager removed

    def unregister_gateway(self, gateway_id):
        pass  # GatewayManager removed

    def get_gateway(self, gateway_id):
        return None  # GatewayManager removed

    def register_receiver_code(self, receiver_code, worker_id, hotkey, ip, port):
        return None  # GatewayManager removed

    def unregister_receiver_code(self, receiver_code):
        return None  # GatewayManager removed

    def get_receiver_info(self, receiver_code):
        return None  # GatewayManager removed

    def get_worker_receiver_code(self, worker_id):
        return None  # GatewayManager removed

    def get_workers_excluding(self, exclude_worker_id):
        return []  # GatewayManager removed

    # =========================================================================
    # Blind Worker Management (BlindWorkerManager removed - stubbed)
    # =========================================================================

    async def register_blind_worker(self, blind_worker_id, session_token, trust_score=0.5, tier="probation", success_rate=0.0, total_tasks=0, avg_bandwidth_mbps=0.0, claimed_region=None):
        return None  # BlindWorkerManager removed

    def get_blind_worker(self, blind_worker_id):
        return None  # BlindWorkerManager removed

    def register_blind_worker_connection(self, blind_worker_id, websocket):
        pass  # BlindWorkerManager removed

    def unregister_blind_worker_connection(self, blind_worker_id):
        pass  # BlindWorkerManager removed

    async def record_blind_relay_result(self, blind_worker_id, task_id, success, bytes_relayed, bandwidth_mbps, start_time_us=0, end_time_us=0):
        return None  # BlindWorkerManager removed

    async def fail_blind_task(self, blind_worker_id, task_id, reason="unknown"):
        return None  # BlindWorkerManager removed

    async def accept_blind_task_offer(self, offer_id, blind_worker_id):
        return None  # BlindWorkerManager removed

    async def reject_blind_task_offer(self, offer_id, blind_worker_id, reason=""):
        return None  # BlindWorkerManager removed

    def get_connected_blind_workers(self):
        return []  # BlindWorkerManager removed

    async def broadcast_task_offer_to_blind_workers(self, offer_id, task_id, transfer_id, chunk_size, chunk_hash, source_region="", dest_region="", estimated_reward=0.0, deadline_us=0, destination_url=None, filename=None, total_chunks=None):
        return []  # BlindWorkerManager removed

    # =========================================================================
    # Cross-cutting: Task Completion & Relay Results
    # =========================================================================

    async def complete_task(
        self,
        task_id: str,
        bytes_relayed: int,
        bandwidth_mbps: float,
        start_time_us: int,
        end_time_us: int,
        canary_proof: str,
        worker_signature: str,
    ) -> Optional[BandwidthProof]:
        """Record task completion and generate proof."""
        logger.info(f"complete_task called: task_id={task_id[:20]}..., bytes={bytes_relayed}")
        task = self.active_tasks.get(task_id)
        if not task:
            logger.warning(f"Unknown task: {task_id[:16]}...")
            return None

        worker = self.workers.get(task.worker_id)
        worker_hotkey = None
        worker_region = ""
        logger.info(f"complete_task: worker_id={task.worker_id}, in_memory={worker is not None}, client={self.subnet_core_client is not None}")
        if worker:
            worker_hotkey = worker.hotkey
            worker_region = getattr(worker, 'region', '') or task.source_region or ""
            logger.info(f"complete_task: using in-memory worker hotkey={worker_hotkey[:16]}...")
        elif self.subnet_core_client:
            # Fall back to database lookup
            try:
                logger.info(f"complete_task: looking up worker {task.worker_id} from SubnetCore...")
                worker_data = await self.subnet_core_client.get_worker(task.worker_id)
                worker_hotkey = worker_data.get("hotkey", "")
                worker_region = worker_data.get("region", "") or task.source_region or ""
                logger.info(f"complete_task: got worker_hotkey={worker_hotkey[:16] if worker_hotkey else 'None'}...")
            except Exception as e:
                logger.warning(f"Worker lookup failed for task {task_id[:16]}...: {e}")

        if not worker_hotkey:
            logger.warning(f"Worker not found for task: {task_id[:16]}... (no hotkey)")
            return None

        # Update task
        task.status = "completed"
        task.completed_at = time.time()
        task.bytes_relayed = bytes_relayed
        task.bandwidth_mbps = bandwidth_mbps
        task.latency_ms = (end_time_us - start_time_us) / 1000

        # Update worker stats (only if worker is in memory)
        if worker:
            worker.active_tasks = max(0, worker.active_tasks - 1)
            worker.successful_tasks += 1
            worker.bytes_relayed_total += bytes_relayed
            worker.bytes_relayed_epoch += bytes_relayed
            worker.update_bandwidth_ema(bandwidth_mbps)
            worker.update_success_rate()
            worker.trust_score = min(1.0, worker.trust_score + 0.001)

        # Move to completed
        del self.active_tasks[task_id]
        self.completed_tasks[task_id] = task

        # Update global stats
        self.total_bytes_relayed += bytes_relayed
        self.total_tasks_completed += 1

        # Generate proof
        proof = BandwidthProof(
            task_id=task_id,
            worker_id=task.worker_id,
            worker_hotkey=worker_hotkey,
            start_time_us=start_time_us,
            end_time_us=end_time_us,
            bytes_relayed=bytes_relayed,
            bandwidth_mbps=bandwidth_mbps,
            chunk_hash=task.chunk_hash,
            canary_proof=canary_proof,
            worker_signature=worker_signature,
            source_region=task.source_region or worker_region,
            dest_region=task.dest_region or worker_region,
        )

        proof.orchestrator_signature = self._sign_proof(proof)

        # Ensure task exists in SubnetCore DB for payment address resolution
        if self.subnet_core_client:
            try:
                from clients.subnet_core_client import TaskCreate, TaskUpdate
                # Create the task first (idempotent - will succeed or already exist)
                await self.subnet_core_client.create_task(TaskCreate(
                    task_id=task_id,
                    worker_id=task.worker_id,
                    chunk_size=bytes_relayed,
                    chunk_hash=task.chunk_hash,
                    deadline_us=end_time_us,
                    source_region=task.source_region,
                    dest_region=task.dest_region,
                ))
                logger.debug(f"Created task {task_id[:16]}... in SubnetCore for payment resolution")
            except Exception as e:
                # Task may already exist (409 Conflict) - that's fine
                if "409" not in str(e) and "already exists" not in str(e).lower():
                    logger.warning(f"Failed to create task {task_id[:16]}... in SubnetCore: {e}")

            # Now update with completion data
            try:
                await self.subnet_core_client.update_task(task_id, TaskUpdate(
                    status="completed",
                    bytes_relayed=bytes_relayed,
                    bandwidth_mbps=bandwidth_mbps,
                    latency_ms=task.latency_ms,
                ))
            except Exception as e:
                logger.warning(f"Failed to update task {task_id[:16]}... in SubnetCore: {e}")

        # Add to aggregation queue
        self.pending_proofs.append(proof)
        self.epoch_proofs[self.current_epoch].append(proof)

        # Persist proof
        await self._proof_agg.persist_bandwidth_proof(proof, self.current_epoch, self.db, self.subnet_core_client)

        # Pay worker immediately (ALPHA payment - mandatory)
        payment = await self._reward_mgr.pay_worker_immediately(
            worker, proof, self.current_epoch, self.get_our_emission,
            self.total_bytes_relayed, self.wallet, self.subtensor,
            self.hotkey or "", self.db, self.subnet_core_client,
            fee_percentage=self.settings.fee_percentage,
            netuid=self.settings.netuid,
            alpha_per_chunk=self.settings.alpha_per_chunk,
        )
        self._track_payment_result(payment)

        logger.info(
            f"Task {task_id[:16]}... completed: "
            f"{bytes_relayed} bytes @ {bandwidth_mbps:.2f} Mbps"
        )
        return proof

    async def record_relay_result(
        self,
        worker_id: str,
        task_id: str,
        success: bool,
        bytes_relayed: int,
        bandwidth_mbps: float,
        chunks_relayed: int,
        latency_ms: float,
        proof_of_bandwidth: dict,
    ) -> Optional[BandwidthProof]:
        """Record a relay result from a worker (P2P transfer)."""
        worker = self.workers.get(worker_id)
        worker_hotkey = None
        worker_region = ""
        if worker:
            worker_hotkey = worker.hotkey
            worker_region = getattr(worker, 'region', '')
        elif self.subnet_core_client:
            try:
                worker_data = await self.subnet_core_client.get_worker(worker_id)
                worker_hotkey = worker_data.get("hotkey", "")
                worker_region = worker_data.get("region", "")
            except Exception as e:
                logger.warning(f"Worker lookup failed for relay result {worker_id}: {e}")

        if not worker_hotkey:
            logger.warning(f"Unknown worker {worker_id} sent relay result (no hotkey)")
            return None

        if not success:
            if worker:
                worker.failed_tasks += 1
                worker.update_success_rate()
            return None

        # Update worker stats (only if worker is in memory)
        if worker:
            worker.successful_tasks += 1
            worker.bytes_relayed_total += bytes_relayed
            worker.bytes_relayed_epoch += bytes_relayed
            if bandwidth_mbps > 0:
                worker.update_bandwidth_ema(bandwidth_mbps)
            worker.update_success_rate()
            worker.trust_score = min(1.0, worker.trust_score + 0.001)

        # Update global stats
        self.total_bytes_relayed += bytes_relayed
        self.total_tasks_completed += 1

        # Generate proof with realistic timestamps
        current_time_us = int(time.time() * 1_000_000)
        if latency_ms > 0:
            estimated_duration_us = int(latency_ms * 1000)
        elif bandwidth_mbps > 0 and bytes_relayed > 0:
            estimated_duration_us = int((bytes_relayed * 8) / (bandwidth_mbps * 1_000_000) * 1_000_000)
        else:
            estimated_duration_us = 100_000
        estimated_duration_us = max(estimated_duration_us, 10_000)

        start_time_us = proof_of_bandwidth.get("start_time_us", current_time_us - estimated_duration_us)
        end_time_us = proof_of_bandwidth.get("end_time_us", current_time_us)

        proof = BandwidthProof(
            task_id=task_id or f"relay-{worker_id}-{int(time.time())}",
            worker_id=worker_id,
            worker_hotkey=worker_hotkey,
            start_time_us=start_time_us,
            end_time_us=end_time_us,
            bytes_relayed=bytes_relayed,
            bandwidth_mbps=bandwidth_mbps,
            chunk_hash=proof_of_bandwidth.get("transfer_id", ""),
            canary_proof="",
            worker_signature=proof_of_bandwidth.get("signature", ""),
            source_region=worker_region,
            dest_region="local",
        )

        if hasattr(self, '_sign_proof'):
            proof.orchestrator_signature = self._sign_proof(proof)

        # Ensure task exists in SubnetCore DB for payment address resolution
        if self.subnet_core_client and task_id:
            try:
                from clients.subnet_core_client import TaskCreate, TaskUpdate
                # Create the task first (idempotent - will succeed or already exist)
                await self.subnet_core_client.create_task(TaskCreate(
                    task_id=task_id,
                    worker_id=worker_id,
                    chunk_size=bytes_relayed,
                    chunk_hash=proof_of_bandwidth.get("transfer_id", ""),
                    deadline_us=end_time_us,
                    source_region=worker_region,
                    dest_region="local",
                ))
                logger.debug(f"Created task {task_id[:16]}... in SubnetCore for payment resolution")
            except Exception as e:
                # Task may already exist (409 Conflict) - that's fine
                if "409" not in str(e) and "already exists" not in str(e).lower():
                    logger.warning(f"Failed to create task {task_id[:16]}... in SubnetCore: {e}")

            # Now update with completion data
            try:
                actual_latency = latency_ms if latency_ms > 0 else (end_time_us - start_time_us) / 1000
                await self.subnet_core_client.update_task(task_id, TaskUpdate(
                    status="completed",
                    bytes_relayed=bytes_relayed,
                    bandwidth_mbps=bandwidth_mbps,
                    latency_ms=actual_latency,
                ))
            except Exception as e:
                logger.warning(f"Failed to update task {task_id[:16]}... in SubnetCore: {e}")

        self.pending_proofs.append(proof)
        self.epoch_proofs[self.current_epoch].append(proof)

        await self._proof_agg.persist_bandwidth_proof(proof, self.current_epoch, self.db, self.subnet_core_client)

        # Pay worker immediately (ALPHA payment - mandatory)
        payment = await self._reward_mgr.pay_worker_immediately(
            worker, proof, self.current_epoch, self.get_our_emission,
            self.total_bytes_relayed, self.wallet, self.subtensor,
            self.hotkey or "", self.db, self.subnet_core_client,
            fee_percentage=self.settings.fee_percentage,
            netuid=self.settings.netuid,
            alpha_per_chunk=self.settings.alpha_per_chunk,
        )
        self._track_payment_result(payment)

        logger.info(
            f"Recorded relay result from worker {worker_id}: "
            f"{bytes_relayed} bytes at {bandwidth_mbps:.1f} Mbps"
        )
        return proof

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
    # Payment Self-Limiting
    # =========================================================================

    def _track_payment_result(self, payment_result: Optional[float]) -> None:
        """Track consecutive payment failures and self-limit when broke."""
        if payment_result is not None:
            # Payment succeeded — reset counter
            if self._consecutive_payment_failures > 0:
                logger.info(
                    f"Payment succeeded after {self._consecutive_payment_failures} consecutive failures. "
                    f"Resuming task acceptance."
                )
            self._consecutive_payment_failures = 0
            self._accepting_tasks = True
        else:
            # Payment failed
            self._consecutive_payment_failures += 1
            if self._consecutive_payment_failures >= self._max_payment_failures and self._accepting_tasks:
                self._accepting_tasks = False
                logger.warning(
                    f"Orchestrator cannot pay workers — {self._consecutive_payment_failures} consecutive "
                    f"payment failures (insufficient balance). Stopping task acceptance."
                )

    # =========================================================================
    # Reward Distribution (delegate to RewardManager)
    # =========================================================================

    def distribute_rewards_at_epoch_end(self) -> Dict[str, float]:
        return self._reward_mgr.distribute_rewards_at_epoch_end(self.workers.values(), self.get_our_emission)

    def distribute_rewards_to_workers(self) -> Dict[str, float]:
        return self._reward_mgr.distribute_rewards_to_workers(self.get_our_emission)

    # =========================================================================
    # Metagraph & Validators
    # =========================================================================

    def _find_our_uid(self) -> None:
        if self.metagraph is None or self.hotkey is None:
            return
        for uid in range(len(self.metagraph.hotkeys)):
            if self.metagraph.hotkeys[uid] == self.hotkey:
                logger.info(f"Found our UID: {uid}")
                self.our_uid = uid
                return
        logger.warning(f"Hotkey {self.hotkey[:16]}... not found in metagraph")

    def get_our_emission(self) -> float:
        """Get our emission converted from alpha to TAO.

        Caches the subnet price for 60s to avoid hammering the subtensor
        websocket (which is not safe for concurrent recv calls).
        """
        if self.metagraph is None or self.our_uid is None:
            return 0.0
        try:
            emission_alpha = float(self.metagraph.E[self.our_uid])
        except Exception as e:
            logger.error(f"Error getting emission: {e}")
            return 0.0
        if emission_alpha <= 0 or not self.subtensor:
            return emission_alpha

        # metagraph.E returns emission in alpha (ध), not TAO.
        # Convert using cached subnet price (refreshed every 5 min).
        import time as _time
        now = _time.time()
        cache_ttl = 300  # 5 minutes
        if not hasattr(self, '_cached_alpha_per_tao') or (now - getattr(self, '_cached_price_at', 0)) > cache_ttl:
            try:
                price = self.subtensor.get_subnet_price(self.settings.netuid)
                self._cached_alpha_per_tao = float(price)
                self._cached_price_at = now
            except Exception as e:
                logger.warning(f"Could not convert emission alpha→TAO: {e}")
                # Use stale cache if available
                if not hasattr(self, '_cached_alpha_per_tao'):
                    return emission_alpha

        alpha_per_tao = getattr(self, '_cached_alpha_per_tao', 0)
        if alpha_per_tao > 0:
            emission_tao = emission_alpha / alpha_per_tao
            logger.debug(
                f"Emission: {emission_alpha:.4f} ध → {emission_tao:.9f} TAO "
                f"(rate: {alpha_per_tao:.2f} ध/τ)"
            )
            return emission_tao

        return emission_alpha

    # Note: _discover_validators removed - BeamCore handles PoB centrally

    # =========================================================================
    # Background Loops
    # =========================================================================

    async def _metagraph_sync_loop(self) -> None:
        """Background loop for syncing metagraph."""
        sync_interval = 300  # Sync every 5 minutes (validators/stake change infrequently)

        while self._running:
            try:
                await asyncio.sleep(sync_interval)
                if self.metagraph and self.subtensor:
                    self.metagraph.sync(subtensor=self.subtensor)

                # Always re-check UID after sync — hotkey may have moved to a
                # different UID slot after re-registration (stale UID causes
                # PoB proofs to be filtered by BeamCore).
                old_uid = self.our_uid
                self._find_our_uid()
                if self.our_uid != old_uid and self.subnet_core_client is not None:
                    logger.info(f"UID changed {old_uid} → {self.our_uid}, updating SubnetCoreClient")
                    self.subnet_core_client.orchestrator_uid = self.our_uid

                self.distribute_rewards_to_workers()
                # Note: Validator discovery removed - BeamCore handles PoB centrally

            except asyncio.CancelledError:
                break
            except Exception as e:
                logger.error(f"Error syncing metagraph: {e}")

    def _sync_epoch_from_chain(self) -> None:
        """Set current_epoch from the chain block number to match validator epoch numbering."""
        if self.subtensor:
            try:
                block = self.subtensor.block
                # Use 360 blocks per epoch to match SubnetCore's epoch calculation
                epoch_length_blocks = 360
                chain_epoch = block // epoch_length_blocks

                # Always sync if epochs differ significantly or chain_epoch is newer
                # This handles the case where old epoch calculation (block//25) was used
                # and produced epochs like 258007 which are > correct epoch ~17925
                should_sync = (
                    chain_epoch > self.current_epoch  # Normal case: new epoch
                    or self.current_epoch > 100_000   # Old epoch calculation was used
                    or chain_epoch != self.current_epoch  # Any mismatch (first sync)
                )

                if should_sync and chain_epoch != self.current_epoch:
                    logger.info(
                        f"Synced epoch from chain: {self.current_epoch} -> {chain_epoch} "
                        f"(block={block})"
                    )
                    self.current_epoch = chain_epoch
            except Exception as e:
                logger.warning(f"Failed to sync epoch from chain: {e}")

    async def _epoch_management_loop(self) -> None:
        """Background loop for managing epochs."""
        epoch_duration = timedelta(minutes=5)
        chain_sync_interval = 600  # Re-sync with chain every 10 minutes
        backfill_interval = 300  # Backfill unpaid tasks every 5 minutes
        last_chain_sync = time.time()
        last_backfill = time.time()

        while self._running:
            try:
                await asyncio.sleep(60)

                # Process queued payment retries
                queue_before = len(self._reward_mgr._payment_retry_queue)
                await self._reward_mgr.process_payment_retry_queue(
                    self.current_epoch, self.wallet, self.subtensor,
                    self.hotkey or "", self.db, self.subnet_core_client,
                )
                queue_after = len(self._reward_mgr._payment_retry_queue)
                # If some retries succeeded, reset payment failure counter
                if queue_after < queue_before:
                    self._consecutive_payment_failures = 0
                    if not self._accepting_tasks:
                        self._accepting_tasks = True
                        logger.info("Retry payments succeeded — resuming task acceptance.")

                # Re-sync epoch from chain periodically (every 10 min) to correct drift
                now = time.time()
                if now - last_chain_sync >= chain_sync_interval:
                    self._sync_epoch_from_chain()
                    last_chain_sync = now

                # Periodically backfill unpaid acknowledged tasks
                if self.subnet_core_client and now - last_backfill >= backfill_interval:
                    try:
                        result = await self.subnet_core_client.backfill_unpaid_tasks(max_tasks=100)
                        if result.get("backfilled_count", 0) > 0:
                            logger.info(f"Backfill: created payments for {result['backfilled_count']} tasks")
                    except Exception as e:
                        logger.debug(f"Backfill check failed: {e}")
                    last_backfill = now

                # Check if epoch should change (time-based fallback)
                if datetime.utcnow() - self.epoch_start_time >= epoch_duration:
                    await self._advance_epoch()

            except asyncio.CancelledError:
                break
            except Exception as e:
                logger.error(f"Error in epoch management: {e}")

    async def _advance_epoch(self) -> None:
        """Advance to next epoch.  Must always increment the counter."""
        prev_epoch = self.current_epoch
        summary = None

        try:
            summary = self._build_epoch_summary()
            self.epoch_summaries[self.current_epoch] = summary
        except Exception as e:
            logger.error(f"Error building epoch summary: {e}")

        try:
            self.distribute_rewards_at_epoch_end()
        except Exception as e:
            logger.error(f"Error distributing epoch rewards: {e}")

        try:
            # Submit payment proofs to BeamCore (central PoB authority)
            await self._epoch_mgr.generate_payment_proofs(
                self.current_epoch, self.workers.values(),
                self.hotkey or "", self.our_uid, self.db,
                self.wallet,
                subnet_core=self.subnet_core_client,
            )
        except Exception as e:
            logger.error(f"Error generating payment proofs: {e}")

        # BlindWorkerManager removed - blind mode disabled
        # if self.settings.blind_mode_enabled and self.blind_workers:
        #     try:
        #         await self._blind_mgr.submit_blind_trust_reports(self.current_epoch)
        #         await self._blind_mgr.submit_blind_payments(self.current_epoch)
        #         await self._blind_mgr.reset_blind_worker_epoch_stats()
        #     except Exception as e:
        #         logger.error(f"Error in blind mode epoch tasks: {e}")

        for worker in self.workers.values():
            worker.bytes_relayed_epoch = 0
            worker.rewards_earned_epoch = 0

        self._reward_mgr.epoch_start_emission = self.get_our_emission()

        # Always advance — never let a sub-step failure block epoch progression
        self.current_epoch += 1
        self.epoch_start_time = datetime.utcnow()

        tasks = summary.total_tasks if summary else "?"
        bytes_relayed = summary.total_bytes_relayed if summary else "?"
        logger.info(
            f"Advanced to epoch {self.current_epoch} "
            f"(previous epoch {prev_epoch}: {tasks} tasks, {bytes_relayed} bytes)"
        )

    async def _registry_loop(self) -> None:
        """
        Background loop for registry registration and heartbeat.

        NOTE: Registration and heartbeat are now handled via WebSocket connection
        in main.py (_connect_and_register_ws). This loop is kept for backward
        compatibility but does nothing.
        """
        # Registration is now handled via WebSocket in main.py
        logger.info("Registry loop: registration/heartbeat handled via WebSocket, skipping HTTP")

        # Just keep the task alive to avoid breaking background task management
        while self._running:
            try:
                await asyncio.sleep(60)
            except asyncio.CancelledError:
                break

    async def _stale_task_reassignment_loop(self) -> None:
        """
        Background loop for reassigning stale tasks to new workers.

        Periodically polls SubnetCore for stale tasks (acknowledged but not accepted)
        and reassigns them to different active workers.
        """
        poll_interval = 15.0  # Check every 15 seconds (was 2s, reduced to lower SubnetCore load)
        stale_timeout = 10    # Tasks stale after 10 seconds without completion

        logger.info("Stale task reassignment loop started")

        while self._running:
            try:
                await asyncio.sleep(poll_interval)

                if not self.subnet_core_client:
                    logger.debug("Stale task loop: no subnet_core_client, skipping")
                    continue

                # Get stale tasks from SubnetCore
                stale_tasks = await self.subnet_core_client.get_stale_tasks(
                    timeout_seconds=stale_timeout
                )

                logger.debug(f"Stale task loop: got {len(stale_tasks)} stale tasks")

                if not stale_tasks:
                    continue

                logger.info(f"Found {len(stale_tasks)} stale tasks to reassign")

                # Get available workers from BeamCore (source of truth for worker_id)
                # Then filter to those with recent heartbeat
                workers_response = await self.subnet_core_client.list_workers(status="active")
                all_workers = workers_response.get("workers", [])
                logger.debug(f"Stale task loop: got {len(all_workers)} workers from BeamCore")

                available_workers = self._filter_workers_with_recent_heartbeat(
                    all_workers, max_age_seconds=60
                )
                logger.debug(f"Stale task loop: {len(available_workers)} workers with recent heartbeat")

                if not available_workers:
                    logger.warning(f"No workers with recent heartbeat for reassignment (had {len(all_workers)} from BeamCore)")
                    continue

                # Reassign each stale task
                for stale_task in stale_tasks:
                    task_id = stale_task.get("task_id")
                    original_worker_id = stale_task.get("worker_id")
                    # If task was previously reassigned, exclude that worker too
                    current_worker_id = stale_task.get("reassigned_worker_id") or original_worker_id

                    if not task_id:
                        continue

                    # Select a new worker (different from the current/failed one)
                    new_worker = self._select_worker_for_reassignment(
                        available_workers, exclude_worker_id=current_worker_id
                    )

                    if not new_worker:
                        logger.warning(
                            f"No alternative worker available for task {task_id[:16]}..."
                        )
                        continue

                    new_worker_id = new_worker.get("worker_id")

                    try:
                        await self.subnet_core_client.reassign_task(task_id, new_worker_id)
                        logger.info(
                            f"Reassigned stale task {task_id[:16]}... "
                            f"from {current_worker_id[:16] if current_worker_id else 'unknown'}... "
                            f"to {new_worker_id[:16]}..."
                        )
                    except Exception as e:
                        logger.error(f"Failed to reassign task {task_id[:16]}...: {e}")

            except asyncio.CancelledError:
                break
            except Exception as e:
                logger.error(f"Error in stale task reassignment loop: {e}")

    def _filter_workers_with_recent_heartbeat(
        self,
        workers_from_beamcore: List[Dict[str, Any]],
        max_age_seconds: int = 60,
    ) -> List[Dict[str, Any]]:
        """
        Filter workers from BeamCore to those with a recent heartbeat.

        Args:
            workers_from_beamcore: Workers from BeamCore (affiliated with this orchestrator)
            max_age_seconds: Maximum time since last heartbeat (default 60s)

        Returns:
            Filtered list of worker dicts with recent heartbeats
        """
        from datetime import datetime as dt

        now = dt.utcnow()
        recent_workers = []

        for w in workers_from_beamcore:
            worker_id = w.get("worker_id")
            if not worker_id:
                continue

            # Check last_seen from BeamCore response
            last_seen_str = w.get("last_seen")
            if not last_seen_str:
                continue

            try:
                # Parse ISO format timestamp from BeamCore (e.g., "2026-02-13T22:40:00")
                last_seen = dt.fromisoformat(last_seen_str.rstrip("Z"))
                time_since_heartbeat = (now - last_seen).total_seconds()
                if time_since_heartbeat <= max_age_seconds:
                    recent_workers.append(w)
            except (ValueError, TypeError):
                # Skip workers with unparseable timestamps
                continue

        return recent_workers

    def _select_worker_for_reassignment(
        self,
        workers: List[Dict[str, Any]],
        exclude_worker_id: Optional[str] = None,
    ) -> Optional[Dict[str, Any]]:
        """
        Select a worker for task reassignment.

        Uses simple round-robin/random selection, excluding the failed worker.
        Could be enhanced with load balancing or trust-based selection.

        Args:
            workers: List of available worker dicts
            exclude_worker_id: Worker to exclude (the one that failed)

        Returns:
            Selected worker dict or None if no workers available
        """
        import random

        # Filter out the excluded worker
        candidates = [
            w for w in workers
            if w.get("worker_id") != exclude_worker_id
        ]

        if not candidates:
            return None

        # Simple random selection (could be enhanced with load balancing)
        return random.choice(candidates)

    # =========================================================================
    # State & Metrics
    # =========================================================================

    def _build_epoch_summary(self) -> EpochSummary:
        return self._proof_agg.build_epoch_summary(self.current_epoch, self.epoch_start_time)

    def get_state(self) -> dict:
        """Get current Orchestrator state."""
        active_workers = [w for w in self.workers.values() if w.is_available]

        return {
            "hotkey": self.hotkey,
            "current_epoch": self.current_epoch,
            "epoch_start": self.epoch_start_time.isoformat(),
            "total_workers": len(self.workers),
            "active_workers": len(active_workers),
            "workers_by_status": {
                status.value: len([w for w in self.workers.values() if w.status == status])
                for status in WorkerStatus
            },
            "active_tasks": len(self.active_tasks),
            "pending_proofs": len(self.pending_proofs),
            "total_bytes_relayed": self.total_bytes_relayed,
            "total_tasks_completed": self.total_tasks_completed,
            # validators_known removed - BeamCore handles PoB centrally
        }

    def get_worker_stats(self) -> List[dict]:
        """Get statistics for all workers."""
        return [
            {
                "worker_id": w.worker_id,
                "region": w.region,
                "status": w.status.value,
                "trust_score": round(w.trust_score, 4),
                "bandwidth_mbps": round(w.bandwidth_ema, 2),
                "success_rate": round(w.success_rate, 4),
                "total_tasks": w.total_tasks,
                "bytes_relayed": w.bytes_relayed_total,
                "load_factor": round(w.load_factor, 2),
            }
            for w in self.workers.values()
        ]

    def get_epoch_stats(self, epoch: Optional[int] = None) -> Optional[dict]:
        """Get statistics for an epoch."""
        if epoch is None:
            epoch = self.current_epoch

        summary = self.epoch_summaries.get(epoch)
        if not summary:
            if epoch == self.current_epoch:
                summary = self._build_epoch_summary()
            else:
                return None

        return {
            "epoch": summary.epoch,
            "start_time": summary.start_time.isoformat(),
            "end_time": summary.end_time.isoformat(),
            "total_tasks": summary.total_tasks,
            "total_bytes_relayed": summary.total_bytes_relayed,
            "active_workers": summary.active_workers,
            "avg_bandwidth_mbps": round(summary.avg_bandwidth_mbps, 2),
            "avg_latency_ms": round(summary.avg_latency_ms, 2),
            "success_rate": round(summary.success_rate, 4),
        }

    # =========================================================================
    # Utilities
    # =========================================================================

    def _generate_worker_id(self, hotkey: str, ip: str, port: int) -> str:
        """Generate unique worker ID."""
        import hashlib
        data = f"{hotkey}:{ip}:{port}:{time.time()}"
        return hashlib.sha256(data.encode()).hexdigest()[:16]

    def _sign_proof(self, proof: BandwidthProof) -> str:
        """Sign a proof with orchestrator key."""
        message = f"{proof.task_id}:{proof.worker_id}:{proof.bytes_relayed}"

        if self.wallet is None:
            import hashlib
            return hashlib.sha256(message.encode()).hexdigest()

        signature = self.wallet.hotkey.sign(message.encode())
        return signature.hex() if isinstance(signature, bytes) else str(signature)

    # =========================================================================
    # Initialization Helpers
    # =========================================================================

    async def _init_subnet_core_client(self) -> None:
        """Initialize SubnetCoreClient for API-based data operations."""
        if not SUBNET_CORE_CLIENT_AVAILABLE:
            logger.warning("SubnetCoreClient not available - data will not be persisted to BeamCore")
            return

        if not self.settings.subnet_core_url:
            logger.warning("SUBNET_CORE_URL not configured - data will not be persisted to BeamCore")
            return

        try:
            signer = None
            if self.wallet and hasattr(self.wallet, 'hotkey'):
                signer = self.wallet.hotkey
            self.subnet_core_client = init_subnet_core_client(
                base_url=self.settings.subnet_core_url,
                orchestrator_hotkey=self.hotkey or "unknown",
                orchestrator_uid=self.our_uid or 0,
                signer=signer,
            )
            logger.info(f"SubnetCoreClient initialized: {self.settings.subnet_core_url}")

            if await self.subnet_core_client.health_check():
                logger.info("SubnetCoreClient health check passed")
            else:
                logger.warning("SubnetCoreClient health check failed - service may be unavailable")

            # Set up handlers for HTTP polling
            self.subnet_core_client.set_task_completion_handler(self._handle_task_completion_notification)
            self.subnet_core_client.set_transfer_handler(self._handle_transfer_notification)
            self.subnet_core_client.set_stats_provider(self._get_heartbeat_stats)
            self.subnet_core_client.set_worker_update_handler(self._worker_mgr.handle_worker_update)

            # Start WebSocket connection for real-time notifications
            # WebSocket replaces HTTP polling for transfers and task results
            # Heartbeat is still sent via HTTP for compatibility
            await self.subnet_core_client.start_polling(
                heartbeat_interval=30.0,
            )
            logger.info("SubnetCore WebSocket connection started")

        except Exception as e:
            logger.warning(f"Failed to initialize SubnetCoreClient: {e}")
            self.subnet_core_client = None

    async def _handle_task_completion_notification(self, message: dict) -> bool:
        """
        Handle task completion notifications from SubnetCore.

        Called when workers report task completion via SubnetCore.
        Orchestrator verifies the completion, triggers payment, and returns True to acknowledge.

        Args:
            message: Task completion message with task_id, worker_id, bytes, etc.

        Returns:
            True if verified and should be acknowledged, False otherwise
        """
        task_id = message.get("task_id")
        worker_id = message.get("worker_id")
        worker_hotkey = message.get("worker_hotkey", "")
        worker_coldkey = message.get("worker_coldkey", "")
        bytes_transferred = message.get("bytes_relayed", 0) or message.get("bytes_transferred", 0)
        bandwidth_mbps = message.get("bandwidth_mbps", 0.0)

        logger.info(
            f"Verifying task completion: task={task_id[:16] if task_id else 'none'}... "
            f"worker={worker_id[:16] if worker_id else 'none'}... bytes={bytes_transferred}"
        )

        # Check if task exists in our active tasks (may not exist if created by SubnetCore)
        task = self.active_tasks.get(task_id) or self.completed_tasks.get(task_id)
        already_completed = task_id in self.completed_tasks

        # Get reassigned_worker_id (set by BeamCore when task was reassigned)
        # worker_id = original assignee, reassigned_worker_id = new worker after reassignment
        reassigned_worker_id = message.get("reassigned_worker_id")

        if task:
            # Verify completing worker matches either original or reassigned worker
            worker_matches = (
                task.worker_id == worker_id or
                (reassigned_worker_id and reassigned_worker_id == worker_id)
            )
            if not worker_matches:
                logger.warning(f"Worker mismatch for task {task_id}: expected {task.worker_id} or reassigned={reassigned_worker_id}, got {worker_id}")
                return False

            # Verify bytes are reasonable (within 20% of expected)
            if task.chunk_size:
                expected_bytes = task.chunk_size
                tolerance = expected_bytes * 0.2
                if abs(bytes_transferred - expected_bytes) > tolerance:
                    logger.warning(
                        f"Bytes mismatch for task {task_id}: expected ~{expected_bytes}, got {bytes_transferred}"
                    )
                    # Still acknowledge but log warning

            # Update task status if not already completed
            if task_id in self.active_tasks:
                task.status = "completed"
                task.completed_at = time.time()
                task.bytes_relayed = bytes_transferred
                task.bandwidth_mbps = bandwidth_mbps
                del self.active_tasks[task_id]
                self.completed_tasks[task_id] = task
                self.total_bytes_relayed += bytes_transferred
                self.total_tasks_completed += 1
                logger.info(f"Task {task_id[:16]}... marked completed via SubnetCore notification")
        else:
            # Task not in memory (created by SubnetCore) - still process payment
            logger.info(f"Task {task_id[:16]}... not in memory, processing payment from SubnetCore data")
            self.total_bytes_relayed += bytes_transferred
            self.total_tasks_completed += 1

        # Trigger payment if not already completed
        if not already_completed and bytes_transferred > 0:
            # Get worker hotkey from message or lookup
            if not worker_hotkey:
                worker = self.workers.get(worker_id)
                if worker and worker.hotkey:
                    worker_hotkey = worker.hotkey
                if not worker_hotkey and self.subnet_core_client:
                    try:
                        # Use unscoped hotkey endpoint — works for workers
                        # completing tasks cross-orchestrator (recovery/speculative)
                        worker_hotkey = await self.subnet_core_client.get_worker_hotkey(worker_id) or ""
                    except Exception as e:
                        logger.warning(f"Worker hotkey lookup failed for {worker_id}: {e}")

            if worker_hotkey:
                # Use timing from message if available, otherwise estimate
                start_time_us = message.get("start_time_us", 0)
                end_time_us = message.get("end_time_us", 0)
                chunk_hash = message.get("chunk_hash", "")

                if not end_time_us:
                    end_time_us = int(time.time() * 1_000_000)
                if not start_time_us:
                    # Estimate start time based on bandwidth (bytes / Mbps = seconds)
                    if bandwidth_mbps > 0:
                        duration_us = int((bytes_transferred * 8) / (bandwidth_mbps * 1_000_000) * 1_000_000)
                    else:
                        duration_us = 1_000_000  # Default 1 second
                    start_time_us = end_time_us - duration_us

                proof = BandwidthProof(
                    task_id=task_id,
                    worker_id=worker_id,
                    worker_hotkey=worker_hotkey,
                    worker_coldkey=worker_coldkey,
                    start_time_us=start_time_us,
                    end_time_us=end_time_us,
                    bytes_relayed=bytes_transferred,
                    bandwidth_mbps=bandwidth_mbps,
                    chunk_hash=chunk_hash or (task.chunk_hash if task else ""),
                    canary_proof="",
                    worker_signature="",
                    orchestrator_signature="",
                )

                # Get worker object for quality multiplier calculation
                worker = self.workers.get(worker_id)

                # Trigger ALPHA payment (mandatory)
                try:
                    payment = await self._reward_mgr.pay_worker_immediately(
                        worker, proof, self.current_epoch, self.get_our_emission,
                        self.total_bytes_relayed, self.wallet, self.subtensor,
                        self.hotkey or "", self.db, self.subnet_core_client,
                        fee_percentage=self.settings.fee_percentage,
                        netuid=self.settings.netuid,
                        alpha_per_chunk=self.settings.alpha_per_chunk,
                    )
                    if payment:
                        logger.info(
                            f"Payment triggered for task {task_id[:16]}...: "
                            f"{payment:.9f} TAO to worker {worker_id[:16]}..."
                        )
                        self._track_payment_result(payment)
                    else:
                        logger.warning(f"Payment queued/failed for task {task_id[:16]}...")
                except Exception as e:
                    logger.error(f"Failed to trigger payment for task {task_id[:16]}...: {e}")
            else:
                logger.warning(f"Cannot trigger payment - no hotkey for worker {worker_id[:16]}...")

        return True

    async def _handle_transfer_notification(self, transfer: dict) -> None:
        """
        Handle transfer notification from SubnetCore HTTP polling.

        When a new transfer is registered, the orchestrator:
        1. Extracts transfer info (gateway_url, destination_url, chunks)
        2. Lists available workers from its pool
        3. Assigns chunks to workers (round-robin or load-based)
        4. POSTs assignments to SubnetCore

        SubnetCore then notifies workers of their task assignments.
        """
        transfer_id = transfer.get("transfer_id", "")
        gateway_url = transfer.get("gateway_url", "")
        object_id = transfer.get("object_id", "")
        destination_url = transfer.get("destination_url", "")
        total_chunks = transfer.get("total_chunks", 0)
        total_size = transfer.get("total_size", 0)
        auth_token = transfer.get("auth_token")
        chunks = transfer.get("chunks", [])

        # Per-chunk URLs for S3 presigned URL transfers
        source_urls = transfer.get("source_urls")  # Dict[str, str]: chunk_idx -> presigned GET URL
        dest_urls = transfer.get("dest_urls")  # Dict[str, str]: chunk_idx -> presigned PUT URL
        is_s3_transfer = bool(source_urls and dest_urls)

        # Get this orchestrator's assigned chunk range from transfer assignment
        chunk_start = transfer.get("chunk_start", 0)
        chunk_end = transfer.get("chunk_end", total_chunks - 1 if total_chunks > 0 else 0)
        assigned_chunk_count = chunk_end - chunk_start + 1

        logger.info(
            f"Transfer registered: {transfer_id[:16]}... "
            f"chunks={assigned_chunk_count} (range {chunk_start}-{chunk_end}) "
            f"total={total_chunks} gateway={gateway_url[:30] if gateway_url else 'none'}... "
            f"s3_transfer={is_s3_transfer}"
        )

        # For regular transfers, require gateway_url and destination_url
        # For S3 transfers, require source_urls and dest_urls instead
        if not is_s3_transfer and (not gateway_url or not destination_url):
            logger.warning(f"Transfer {transfer_id[:16]}... missing gateway_url or destination_url")
            return

        # Get available workers from SubnetCore
        try:
            workers_response = await self.subnet_core_client.list_workers(status="active")
            workers = workers_response.get("workers", [])
            if workers:
                worker_ids = [w.get("worker_id", "?") for w in workers[:5]]
                logger.info(f"Got {len(workers)} workers from SubnetCore: {worker_ids}...")
        except Exception as e:
            logger.error(f"Failed to list workers for transfer {transfer_id[:16]}...: {e}")
            return

        if not workers:
            logger.warning(f"No active workers available for transfer {transfer_id[:16]}...")
            return

        # Assign chunks to workers (round-robin) - only for this orchestrator's range
        assignments = self._assign_chunks_to_workers(
            workers=workers,
            chunks=chunks,
            chunk_start=chunk_start,
            chunk_end=chunk_end,
        )

        logger.info(
            f"Assigned {assigned_chunk_count} chunks (range {chunk_start}-{chunk_end}) "
            f"to {len(assignments)} workers for transfer {transfer_id[:16]}..."
        )

        # Use total_size from message, or calculate from chunks
        if not total_size:
            total_size = sum(c.get("size", 0) for c in chunks) if chunks else 0
        chunk_size = chunks[0].get("size", 4 * 1024 * 1024) if chunks else 4 * 1024 * 1024

        # POST assignments to SubnetCore
        try:
            result = await self.subnet_core_client.post_chunk_assignments(
                transfer_id=transfer_id,
                gateway_url=gateway_url,
                object_id=object_id,
                destination_url=destination_url,
                total_size=total_size,
                chunk_size=chunk_size,
                assignments=assignments,
                auth_token=auth_token,
                source_urls=source_urls,
                dest_urls=dest_urls,
            )

            created = result.get('tasks_created', result.get('created_count', 0))
            pushed = result.get('tasks_pushed', 0)
            already_assigned = result.get('already_assigned_count', 0)

            logger.info(
                f"Posted assignments for transfer {transfer_id[:16]}...: "
                f"created={created} pushed={pushed}"
            )

            # Log any lost races (chunks already assigned to other orchestrators)
            if already_assigned > 0:
                logger.info(
                    f"Transfer {transfer_id[:16]}...: {already_assigned} chunks "
                    f"already assigned to other orchestrators (race lost, moving on)"
                )

        except Exception as e:
            logger.error(f"Failed to post assignments for transfer {transfer_id[:16]}...: {e}")

    def _assign_chunks_to_workers(
        self,
        workers: List[Dict[str, Any]],
        chunks: List[Dict[str, Any]],
        chunk_start: int,
        chunk_end: int,
    ) -> List[Dict[str, Any]]:
        """
        Assign chunks to workers based on SLA metrics, spreading load evenly.

        Only assigns chunks in the range [chunk_start, chunk_end] (inclusive).
        This ensures each orchestrator only assigns its designated chunk range.

        Workers are ranked by SLA score (trust_score * success_rate * bandwidth_factor).
        Chunks are assigned round-robin across all workers, ensuring each worker gets
        at most 1 chunk before any worker gets a second chunk.

        Returns list of assignments: [{"worker_id": "...", "chunk_indices": [17, 18, 19]}, ...]
        """
        if not workers:
            return []

        # Build chunk indices from this orchestrator's assigned range
        chunk_indices = list(range(chunk_start, chunk_end + 1))

        # Score workers by SLA metrics (higher is better)
        def worker_sla_score(w: Dict[str, Any]) -> float:
            trust = float(w.get("trust_score", 0.5))
            success = float(w.get("success_rate", 0.5))
            bandwidth = float(w.get("bandwidth_mbps", 100.0))
            # Normalize bandwidth (100 Mbps = 1.0, cap at 2.0)
            bandwidth_factor = min(2.0, bandwidth / 100.0)
            return trust * success * bandwidth_factor

        # Sort workers by SLA score (best first)
        sorted_workers = sorted(workers, key=worker_sla_score, reverse=True)
        worker_ids = [w["worker_id"] for w in sorted_workers]

        # Initialize assignments for each worker
        worker_assignments = {wid: [] for wid in worker_ids}

        # Assign chunks round-robin across workers (spreads load evenly)
        # Each worker gets 1 chunk before any gets 2
        for i, chunk_idx in enumerate(chunk_indices):
            worker_id = worker_ids[i % len(worker_ids)]
            worker_assignments[worker_id].append(chunk_idx)

        # Log SLA-based assignment
        if chunk_indices:
            top_worker = sorted_workers[0] if sorted_workers else {}
            logger.debug(
                f"SLA-based assignment: {len(chunk_indices)} chunks to {len(worker_ids)} workers, "
                f"top worker={top_worker.get('worker_id', '?')[:16]} "
                f"(trust={top_worker.get('trust_score', 0):.2f}, "
                f"success={top_worker.get('success_rate', 0):.2f})"
            )

        # Convert to list format
        return [
            {"worker_id": wid, "chunk_indices": indices}
            for wid, indices in worker_assignments.items()
            if indices  # Only include workers with assigned chunks
        ]

    async def _init_orch_manager(self) -> None:
        """Initialize the orchestrator manager for incentive mechanism."""
        try:
            from beam.orchestrator import OrchestratorManager
            self.orch_manager = OrchestratorManager()
            logger.info("Orchestrator manager initialized (in-memory mode)")
        except ImportError:
            # OrchestratorManager is optional - not needed for normal operation
            self.orch_manager = None
        except Exception as e:
            logger.error(f"Failed to initialize orchestrator manager: {e}")
            self.orch_manager = None

    def _get_heartbeat_stats(self) -> Dict[str, Any]:
        """
        Get stats for heartbeat.

        Returns dict with:
            - current_workers: int
            - avg_bandwidth_mbps: float
            - total_bytes_relayed: int
            - fee_percentage: float
            - balance_tao: float
            - coldkey_balance_tao: float
            - pending_payments: int
        """
        # Count active workers
        active_workers = sum(
            1 for w in self.workers.values()
            if w.status == WorkerStatus.ACTIVE
        )

        # Calculate average bandwidth
        bandwidths = [w.bandwidth_mbps for w in self.workers.values() if w.bandwidth_mbps > 0]
        avg_bandwidth = sum(bandwidths) / len(bandwidths) if bandwidths else 0.0

        # Get pending payments count from reward manager
        pending_payments = 0
        if self._reward_mgr:
            pending_payments = getattr(self._reward_mgr, 'pending_payment_count', 0)

        return {
            "current_workers": active_workers,
            "avg_bandwidth_mbps": avg_bandwidth,
            "total_bytes_relayed": self.total_bytes_relayed,
            "fee_percentage": self.settings.fee_percentage,
            "balance_tao": -1.0,  # Would need async call to subtensor
            "coldkey_balance_tao": -1.0,  # Would need async call to subtensor
            "pending_payments": pending_payments,
        }

    async def _add_local_mock_worker(self) -> None:
        """Add a mock worker for local mode bandwidth challenges."""
        worker_hotkey = self.settings.mock_worker_hotkey or "5LocalMockWorkerHotkey0000000000000000000000"
        worker_id = f"worker-{worker_hotkey[:8]}" if self.settings.mock_worker_hotkey else "local-mock-worker"

        mock_worker = Worker(
            worker_id=worker_id,
            hotkey=worker_hotkey,
            ip="127.0.0.1",
            port=9100,
            region="local",
            status=WorkerStatus.ACTIVE,
            bandwidth_mbps=1000.0,
            bandwidth_ema=1000.0,
            latency_ms=1.0,
            success_rate=1.0,
            trust_score=1.0,
            max_concurrent_tasks=100,
        )

        self.workers[mock_worker.worker_id] = mock_worker
        self.workers_by_region["local"].add(mock_worker.worker_id)

        logger.info(f"Added mock worker for local mode: {mock_worker.worker_id} (hotkey: {worker_hotkey})")


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
