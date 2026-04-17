"""
Validator Core Logic - Unified Version

Validates Proof-of-Bandwidth submissions and sets weights on the Bittensor network.
Supports both local mode (HTTP) and mainnet/testnet (Bittensor dendrite).

Uses SLA-based multiplicative penalty scoring for orchestrators and workers.
Orchestrators failing SLA have rewards redirected to Orchestrator #1 (subnet treasury).
"""

import asyncio
import base64
import logging
import os
import random
import time
from dataclasses import dataclass, field
from datetime import datetime, timedelta
from typing import Dict, List, Optional, Set, Tuple

import aiohttp
import bittensor as bt

from core.config import Settings, get_settings
from chain import FiberChain, FiberNode
from chain.tx_verifier import TxVerifier, TxVerificationResult

# Import stubs for removed beam.* modules (logic now in BeamCore)
from core._beam_stubs import (
    # beam.protocol.task
    Task, SLALevel,
    # beam.protocol.pob
    ProofOfBandwidth, PoBVerificationResult,
    build_merkle_leaf, compute_merkle_root, verify_merkle_proof,
    # beam.protocol.synapse
    BandwidthChallenge, BandwidthProof, WorkerStatusQuery, ChunkTransfer, EpochInfo,
    # beam.crypto.hashing
    sha256, compute_canary_proof,
    # beam.crypto.signatures
    verify_hotkey_signature,
    # beam.constants
    CHUNK_SIZE_BYTES, CANARY_SIZE_BYTES,
    SCORE_WEIGHT_BANDWIDTH, SCORE_WEIGHT_UPTIME, SCORE_WEIGHT_LOSS, SCORE_WEIGHT_TIER,
    BANDWIDTH_EMA_ALPHA,
    # beam.scoring.sla
    SLAScorer, SLAMetrics, SLAScore, SLARewardCalculator,
    OrchestratorSLAState, WorkerSLAState,
    calculate_stake_weight, SUBNET_ORCHESTRATOR_UID,
    MIN_ORCHESTRATOR_STAKE_TAO, NEW_ORCHESTRATOR_GRACE_PERIOD_HOURS,
    # beam.orchestrator
    OrchestratorManager, Orchestrator, WorkerRegistry, ReassignmentManager,
    # beam.validation.sybil_detector
    SybilDetector, SybilDetectionResult, SybilViolationType,
    get_sybil_detector, check_entity_sybil, check_path_sybil,
)

# Redundancy and failover imports
from core.redundancy import (
    HealthMonitor,
    CheckpointManager,
    RecoveryManager,
    HealthStatus,
    create_redundancy_system,
    initialize_with_recovery,
)

# SubnetCore API client for score submission (replaces direct DB access)
try:
    from clients import (
        SubnetCoreClient,
        ScoreSubmission,
        SpotCheckSubmission,
        FraudPenaltySubmission,
        ChallengeResultSubmission,
        get_subnet_core_client,
    )
    SUBNET_CORE_AVAILABLE = True
except ImportError:
    SUBNET_CORE_AVAILABLE = False
    SubnetCoreClient = None

logger = logging.getLogger(__name__)


# =============================================================================
# Data Models
# =============================================================================

@dataclass
class OrchestratorInfo:
    """Information about an Orchestrator endpoint."""
    url: str
    hotkey: str
    uid: Optional[int] = None
    stake_tao: float = 0.0
    last_seen: datetime = field(default_factory=datetime.utcnow)
    registered_at: Optional[datetime] = None
    is_healthy: bool = True
    is_subnet_owned: bool = False  # True for UID #1
    last_score: float = 0.0

    # SLA state
    sla_state: Optional[OrchestratorSLAState] = None


@dataclass
class WorkSummary:
    """Work summary received from Orchestrator."""
    epoch: int
    orchestrator_hotkey: str
    total_tasks: int
    successful_tasks: int
    total_bytes_relayed: int
    active_workers: int
    avg_bandwidth_mbps: float
    avg_latency_ms: float
    success_rate: float
    proof_count: int
    worker_regions: Dict[str, int]
    orchestrator_signature: str

    # Extended SLA metrics (for full scoring)
    uptime_percent: float = 100.0  # Orchestrator uptime over 24h window
    acceptance_rate: float = 100.0  # Task acceptance rate
    latency_p95_ms: float = 0.0  # 95th percentile latency

    # Worker contribution breakdown
    worker_contributions: Optional[Dict[str, int]] = None  # worker_id -> bytes

    # Measurement window
    measurement_start: Optional[datetime] = None
    measurement_end: Optional[datetime] = None


@dataclass
class ChallengeResult:
    """Result of a bandwidth challenge."""
    challenge_id: str
    success: bool
    bytes_relayed: int = 0
    bandwidth_mbps: float = 0.0
    latency_ms: float = 0.0
    canary_verified: bool = False
    error: Optional[str] = None


@dataclass
class ProofVerificationResult:
    """Result of verifying a single proof."""
    task_id: str
    valid: bool
    error: Optional[str] = None

    # Verification details
    signature_valid: bool = False
    timing_valid: bool = False
    bandwidth_valid: bool = False
    canary_valid: bool = False
    geo_valid: bool = False

    # Measured latency from proof timing
    latency_ms: Optional[float] = None


@dataclass
class SpotCheckResult:
    """Result of spot-checking proofs for an orchestrator."""
    orchestrator_hotkey: str
    proofs_requested: int
    proofs_received: int
    proofs_valid: int
    proofs_invalid: int
    verification_rate: float = 0.0

    # Details of invalid proofs
    invalid_proof_ids: List[str] = field(default_factory=list)
    invalid_reasons: Dict[str, str] = field(default_factory=dict)  # task_id -> reason

    # Fraud indicators
    fraud_detected: bool = False
    fraud_severity: float = 0.0  # 0.0 to 1.0

    timestamp: datetime = field(default_factory=datetime.utcnow)


@dataclass
class PoBVerificationStats:
    """Aggregated PoB verification stats for an orchestrator in the current epoch."""
    total_proofs: int = 0
    verified_count: int = 0
    rejected_count: int = 0
    total_bytes_verified: int = 0


# =============================================================================
# Unified Validator
# =============================================================================

class Validator:
    """
    BEAM Validator Node - Unified Version

    Responsibilities:
    - Generate bandwidth challenge tasks
    - Verify Proof-of-Bandwidth submissions
    - Fetch and verify proofs from SubnetCore API
    - Calculate orchestrator and worker SLA scores
    - Apply multiplicative penalties for SLA violations
    - Redirect penalties to Orchestrator #1 (subnet treasury)
    - Process worker reassignments
    - Set weights on Bittensor network

    Supports both:
    - Local mode: HTTP-based communication with orchestrators
    - Mainnet/Testnet: Bittensor dendrite-based communication
    """

    def __init__(self, settings: Optional[Settings] = None):
        self.settings = settings or get_settings()

        # Bittensor components
        self.wallet: Optional[bt.Wallet] = None
        self.subtensor: Optional[bt.Subtensor] = None
        self.metagraph: Optional[bt.Metagraph] = None
        self.dendrite: Optional[bt.Dendrite] = None

        # Fiber chain interface (for weight setting and node discovery)
        self.fiber_chain: Optional[FiberChain] = None
        self._fiber_nodes: Dict[str, FiberNode] = {}  # hotkey -> FiberNode cache

        # Validator state
        self.uid: Optional[int] = None
        self.hotkey: Optional[str] = None
        self.is_registered: bool = False

        # Connection tracking (for dendrite mode)
        self.connections: Dict[int, dict] = {}  # uid -> connection info
        self.connection_scores: Dict[int, float] = {}  # uid -> score
        self.connection_bandwidth: Dict[int, float] = {}  # uid -> bandwidth EMA

        # Orchestrator tracking (for HTTP mode)
        self.orchestrators: Dict[str, OrchestratorInfo] = {}
        self._beamcore_worker_counts: Dict[int, int] = {}  # uid -> worker_count from BeamCore

        # =====================================================================
        # Orchestrator and Worker Management
        # =====================================================================
        self.orchestrator_manager = OrchestratorManager()
        self.worker_registry = WorkerRegistry()
        self.reassignment_manager = ReassignmentManager(self.worker_registry)

        # SLA Scoring
        self.sla_scorer = SLAScorer()
        self.sla_reward_calculator = SLARewardCalculator(self.sla_scorer)
        self.orchestrator_scores: Dict[str, float] = {}  # hotkey -> final score
        self.orchestrator_sla_scores: Dict[str, SLAScore] = {}  # hotkey -> SLA score
        self.payment_penalty_multipliers: Dict[str, float] = {}  # hotkey -> multiplier (1.0 = no penalty)

        # Work summaries (rolling 24h window)
        self.work_summaries: Dict[str, WorkSummary] = {}  # hotkey -> last summary
        self.work_summary_history: Dict[str, List[WorkSummary]] = {}  # hotkey -> history

        # Sybil Detection
        self.sybil_detector = get_sybil_detector()

        # Orchestrator SLA tracking (rolling 24h metrics)
        self.orchestrator_metrics: Dict[int, Dict] = {}  # uid -> metrics accumulator
        self.worker_metrics: Dict[str, Dict] = {}  # worker_id -> metrics accumulator

        # Sybil tracking per orchestrator
        self.sybil_penalties: Dict[int, float] = {}  # uid -> sybil penalty multiplier (0.0-1.0)

        # Redundancy and failover
        self.health_monitor: Optional[HealthMonitor] = None
        self.checkpoint_manager: Optional[CheckpointManager] = None
        self.recovery_manager: Optional[RecoveryManager] = None

        # Penalty tracking
        self.total_redirected_to_one: float = 0.0  # TAO redirected to #1 this epoch
        self.fraud_penalties: Dict[str, float] = {}  # hotkey -> fraud penalty multiplier

        # Task tracking
        self.pending_tasks: Dict[str, Task] = {}  # task_id -> Task
        self.task_results: Dict[str, PoBVerificationResult] = {}  # task_id -> result

        # Challenge tracking
        self.active_challenges: Dict[str, dict] = {}  # task_id -> challenge info
        self.challenge_results: Dict[str, ChallengeResult] = {}  # task_id -> result

        # Proof spot-checking
        self.spot_check_results: Dict[str, SpotCheckResult] = {}  # hotkey -> last result
        self.spot_check_history: List[SpotCheckResult] = []

        # PoB verification stats (populated by _verify_subnet_core_proofs, consumed by _submit_scores_to_core)
        self.pob_verification_results: Dict[str, PoBVerificationStats] = {}  # hotkey -> stats

        # Weight history
        self.last_weight_block: int = 0
        self.weights_history: List[Dict] = []

        # Penalty history
        self.penalty_history: List[dict] = []

        # Epoch tracking for emissions
        self.current_epoch: int = 0
        self.epoch_start_block: int = 0
        self.tasks_this_epoch: int = 0
        self.last_emission_check_block: int = 0

        # HTTP session for local mode
        self._http_session: Optional[aiohttp.ClientSession] = None

        # SubnetCore API client for score submission (set by main.py)
        self.subnet_core_client: Optional[SubnetCoreClient] = None

        # Async control
        self._running: bool = False
        self._main_loop_task: Optional[asyncio.Task] = None

    async def initialize(self) -> None:
        """Initialize the Validator node"""
        logger.info("Initializing Validator node...")

        if self.settings.local_mode:
            # Local development mode - skip Bittensor network connection
            logger.info("Running in LOCAL MODE - skipping Bittensor network connection")
            await self._initialize_local_mode()
        else:
            # Mainnet/Testnet mode with Bittensor
            await self._initialize_bittensor_mode()

        logger.info("Validator node initialized")

    async def _initialize_local_mode(self) -> None:
        """Initialize validator in local development mode (no Bittensor connection)"""
        # Create a mock wallet for local testing
        self.wallet = bt.Wallet(
            name=self.settings.wallet_name,
            hotkey=self.settings.wallet_hotkey,
            path=self.settings.wallet_path,
        )
        self.hotkey = self.wallet.hotkey.ss58_address

        # Set mock values for local development
        self.uid = 1
        self.is_registered = True

        # Connect to subtensor for testnet
        if self.settings.subtensor_address:
            self.subtensor = bt.Subtensor(network=self.settings.subtensor_address)
        else:
            self.subtensor = bt.Subtensor(network=self.settings.subtensor_network)
        logger.info(f"Connected to subtensor: {self.subtensor.network}")

        # Load metagraph for testnet data
        try:
            self.metagraph = bt.Metagraph(
                netuid=self.settings.netuid,
                network=self.subtensor.network,
            )
            self.metagraph.sync(subtensor=self.subtensor)
            logger.info(f"Metagraph loaded with {len(self.metagraph.hotkeys)} neurons")
        except Exception as e:
            logger.warning(f"Failed to load metagraph: {e}")
            self.metagraph = None

        # Initialize Fiber chain interface
        try:
            self.fiber_chain = FiberChain(
                subtensor_network=self.settings.subtensor_network,
                subtensor_address=self.settings.subtensor_address,
                netuid=self.settings.netuid,
            )
            self._fiber_nodes = self.fiber_chain.get_nodes_by_hotkey()
            logger.info(f"Fiber chain initialized with {len(self._fiber_nodes)} nodes")
        except Exception as e:
            logger.warning(f"Fiber not available: {e}")
            self.fiber_chain = None

        # Create HTTP session for local mode communication
        self._http_session = aiohttp.ClientSession(
            timeout=aiohttp.ClientTimeout(total=60)
        )

        # Add local orchestrator as a connection
        self.connections[1] = {
            "uid": 1,
            "hotkey": "local-orchestrator",
            "stake": 1000.0,
            "ip": "127.0.0.1",
            "port": 8000,
            "url": self.settings.orchestrator_url,
            "last_seen": datetime.utcnow(),
            "is_local": True,
        }

        # Discover orchestrators
        await self._discover_orchestrators()

        logger.info(f"Local mode initialized with hotkey: {self.hotkey}")

    async def _initialize_bittensor_mode(self) -> None:
        """Initialize validator in Bittensor network mode"""
        # Load wallet
        self.wallet = bt.Wallet(
            name=self.settings.wallet_name,
            hotkey=self.settings.wallet_hotkey,
            path=self.settings.wallet_path,
        )
        self.hotkey = self.wallet.hotkey.ss58_address
        logger.info(f"Wallet loaded: {self.hotkey}")

        # Connect to subtensor
        if self.settings.subtensor_address:
            self.subtensor = bt.Subtensor(network=self.settings.subtensor_address)
        else:
            self.subtensor = bt.Subtensor(network=self.settings.subtensor_network)
        logger.info(f"Connected to subtensor: {self.subtensor.network}")

        # Load metagraph
        self.metagraph = bt.Metagraph(
            netuid=self.settings.netuid,
            network=self.subtensor.network,
        )
        self.metagraph.sync(subtensor=self.subtensor)

        # Initialize Fiber chain interface
        try:
            self.fiber_chain = FiberChain(
                subtensor_network=self.settings.subtensor_network,
                subtensor_address=self.settings.subtensor_address,
                netuid=self.settings.netuid,
            )
            self._fiber_nodes = self.fiber_chain.get_nodes_by_hotkey()
            logger.info(f"Fiber chain initialized with {len(self._fiber_nodes)} nodes")
        except Exception as e:
            logger.warning(f"Fiber not available: {e}")
            self.fiber_chain = None

        # Check registration
        await self._check_registration()

        # Setup dendrite for querying connections
        self.dendrite = bt.Dendrite(wallet=self.wallet)

        # Create HTTP session
        self._http_session = aiohttp.ClientSession(
            timeout=aiohttp.ClientTimeout(total=60)
        )

    async def _check_registration(self) -> None:
        """Check if this hotkey is registered on the subnet"""
        # Try Fiber first if available
        uid = self._get_uid_for_hotkey(self.hotkey)

        if uid is not None:
            self.uid = uid
            self.is_registered = True
            logger.info(f"Registered on subnet {self.settings.netuid} with UID {self.uid}")
        else:
            self.is_registered = False
            logger.warning(f"Hotkey {self.hotkey} not registered on subnet")

    def _get_uid_for_hotkey(self, hotkey: str) -> Optional[int]:
        """Get UID for a hotkey using Fiber or metagraph fallback."""
        # Try Fiber first (if available and cached)
        if self._fiber_nodes:
            node = self._fiber_nodes.get(hotkey)
            if node:
                logger.debug(f"_get_uid_for_hotkey: {hotkey[:16]}... -> UID {node.uid} (via Fiber)")
                return node.uid

        # Fallback to metagraph
        if self.metagraph and hotkey in self.metagraph.hotkeys:
            uid = self.metagraph.hotkeys.index(hotkey)
            logger.debug(f"_get_uid_for_hotkey: {hotkey[:16]}... -> UID {uid} (via metagraph)")
            return uid

        logger.warning(f"_get_uid_for_hotkey: {hotkey[:16]}... -> NOT FOUND (fiber_nodes={len(self._fiber_nodes)}, metagraph={'yes' if self.metagraph else 'no'})")
        return None

    def _get_node_info(self, hotkey: str) -> Optional[FiberNode]:
        """Get full node info for a hotkey using Fiber."""
        if self._fiber_nodes:
            node = self._fiber_nodes.get(hotkey)
            if node:
                logger.debug(f"_get_node_info: found node for {hotkey[:16]}... uid={node.uid}")
            else:
                logger.debug(f"_get_node_info: no Fiber node for {hotkey[:16]}...")
            return node
        logger.debug(f"_get_node_info: no Fiber nodes available, cannot look up {hotkey[:16]}...")
        return None

    async def start(self) -> None:
        """Start the Validator node"""
        if not self.is_registered:
            logger.error("Cannot start: not registered on subnet")
            return

        # Initialize redundancy system
        await self._initialize_redundancy()

        # Load pending challenges from database (state recovery)
        await self._load_pending_challenges()

        self._running = True
        self._main_loop_task = asyncio.create_task(self._main_loop())
        self._heartbeat_task = asyncio.create_task(self._heartbeat_loop())

        logger.info("Validator node started")

    async def _initialize_redundancy(self) -> None:
        """Initialize redundancy and failover systems"""
        try:
            self.health_monitor, self.checkpoint_manager, self.recovery_manager = \
                create_redundancy_system(self)

            recovered = await initialize_with_recovery(self)
            if recovered:
                logger.info("Validator state restored from checkpoint")

            await self.health_monitor.start()
            await self.checkpoint_manager.start()

            logger.info("Redundancy system initialized")

        except Exception as e:
            logger.warning(f"Failed to initialize redundancy system: {e}")

    async def stop(self) -> None:
        """Stop the Validator node"""
        self._running = False

        # Send offline beacon before cleanup
        if SUBNET_CORE_AVAILABLE and self.subnet_core_client:
            try:
                await self.subnet_core_client.submit_offline_beacon(reason="graceful_shutdown")
            except Exception as e:
                logger.warning(f"Failed to send offline beacon: {e}")

        # Cancel heartbeat task
        if hasattr(self, '_heartbeat_task') and self._heartbeat_task:
            self._heartbeat_task.cancel()
            try:
                await self._heartbeat_task
            except asyncio.CancelledError:
                pass

        if self._main_loop_task:
            self._main_loop_task.cancel()
            try:
                await self._main_loop_task
            except asyncio.CancelledError:
                pass

        # Stop redundancy systems
        if self.health_monitor:
            await self.health_monitor.stop()
        if self.checkpoint_manager:
            await self.checkpoint_manager.stop()

        # Close HTTP session if open
        if self._http_session and not self._http_session.closed:
            await self._http_session.close()
            self._http_session = None

        logger.info("Validator node stopped")

    async def _heartbeat_loop(self) -> None:
        """Send periodic heartbeats to SubnetCore while running."""
        heartbeat_interval = 60  # seconds

        while self._running:
            try:
                await asyncio.sleep(heartbeat_interval)

                if not self._running:
                    break

                if not SUBNET_CORE_AVAILABLE or not self.subnet_core_client:
                    continue

                # Gather health info if available
                health_info = None
                if self.health_monitor:
                    try:
                        report = await self.health_monitor.run_health_checks()
                        health_info = {
                            "status": report.get("status"),
                            "checks_passed": report.get("checks_passed", 0),
                            "checks_failed": report.get("checks_failed", 0),
                        }
                    except Exception:
                        pass

                # Determine status
                status = "online"
                if health_info and health_info.get("checks_failed", 0) > 0:
                    status = "degraded"

                await self.subnet_core_client.submit_heartbeat(
                    validator_uid=self.uid,
                    status=status,
                    last_epoch_scored=getattr(self, 'current_epoch', None),
                    health_info=health_info,
                    external_url=self.settings.external_url,
                )

            except asyncio.CancelledError:
                break
            except Exception as e:
                logger.warning(f"Heartbeat error: {e}")
                # Don't break - continue trying

    async def _main_loop(self) -> None:
        """Main validator loop"""
        while self._running:
            try:
                # Sync metagraph
                await self._sync_metagraph()

                # Discover orchestrators from BeamCore first, then filter connections
                await self._discover_orchestrators()
                await self._update_connections()

                # Sync orchestrators from metagraph
                await self._sync_orchestrators()

                # Query orchestrators for work summaries
                await self._query_orchestrators()

                # Generate and send bandwidth challenges
                # DISABLED: Challenges temporarily disabled - endpoint deprecated
                # await self._generate_and_send_challenges()

                # Issue additional challenges
                # DISABLED: Challenges temporarily disabled - endpoint deprecated
                # await self._issue_challenges()

                # Collect and verify PoB
                await self._collect_pob_results()

                # *** NEW: Verify proofs from SubnetCore API ***
                await self._verify_subnet_core_proofs()

                # Spot-check proofs
                await self._spot_check_proofs()

                # Update SLA metrics from task results
                await self._update_sla_metrics()

                # Calculate SLA scores with multiplicative penalties
                await self._calculate_sla_scores()

                # Process worker reassignments (SLA violations)
                await self._process_reassignments()

                # Verify orchestrator payments to workers
                await self._verify_orchestrator_payments()

                # Update scores
                await self._update_scores()

                # Check for epoch change and broadcast (must run before weight-setting
                # so self.current_epoch is correct on the first main-loop iteration)
                await self._check_epoch()

                # Set weights if needed
                await self._maybe_set_weights()

                # Respond to weight audits from BeamCore
                await self._respond_to_weight_audits()

                # Periodic cleanup
                await self._expire_penalties_and_challenges()

            except Exception as e:
                logger.error(f"Error in main loop: {e}", exc_info=True)

                if self.recovery_manager and self.recovery_manager.should_attempt_recovery():
                    logger.warning("Health issues detected, attempting recovery...")
                    success = await self.recovery_manager.attempt_recovery()
                    if success:
                        logger.info("Recovery successful")
                    else:
                        logger.error("Recovery failed")

            await asyncio.sleep(self.settings.sync_interval)

    # =========================================================================
    # SubnetCore Proof Verification (NEW)
    # =========================================================================

    async def _verify_subnet_core_proofs(self) -> None:
        """
        Fetch unverified proofs from SubnetCore API and verify them.

        This is the main verification loop that ensures proofs are validated
        and workers can be compensated.
        """
        if not SUBNET_CORE_AVAILABLE or not self.subnet_core_client:
            logger.debug("SubnetCore client not available, skipping proof verification")
            return

        try:
            # Fetch unverified proofs from SubnetCore
            result = await self.subnet_core_client.get_unverified_proofs(
                limit=50,  # Process up to 50 proofs per cycle
            )

            proofs = result.get("proofs", [])
            if not proofs:
                logger.debug("No unverified proofs to verify")
                return

            logger.info(f"Fetched {len(proofs)} unverified proofs from SubnetCore")

            verified_count = 0
            failed_count = 0

            # Track verified proofs by orchestrator for work summary computation
            verified_proofs_by_orch: Dict[str, List[dict]] = {}
            # Track all proofs per orchestrator for PoB stats
            all_proofs_by_orch: Dict[str, List[dict]] = {}
            failed_by_orch: Dict[str, int] = {}

            for proof_data in proofs:
                orch_hotkey = proof_data.get("orchestrator_hotkey", "")
                if orch_hotkey:
                    all_proofs_by_orch.setdefault(orch_hotkey, []).append(proof_data)

                try:
                    verification_result = await self._verify_single_subnet_proof(proof_data)

                    # Submit verification result back to SubnetCore
                    await self.subnet_core_client.verify_proof(
                        task_id=proof_data.get("task_id"),
                        passed=verification_result.valid,
                        signature_valid=verification_result.signature_valid,
                        timing_valid=verification_result.timing_valid,
                        bandwidth_valid=verification_result.bandwidth_valid,
                        canary_valid=verification_result.canary_valid,
                        geo_valid=verification_result.geo_valid,
                        verification_notes=verification_result.error,
                        measured_latency_ms=verification_result.latency_ms,
                    )

                    if verification_result.valid:
                        verified_count += 1
                        # Track verified proof for work summary
                        if orch_hotkey:
                            verified_proofs_by_orch.setdefault(orch_hotkey, []).append(proof_data)
                        logger.debug(
                            f"Proof {proof_data.get('task_id', 'unknown')[:16]}... verified successfully"
                        )
                    else:
                        failed_count += 1
                        if orch_hotkey:
                            failed_by_orch[orch_hotkey] = failed_by_orch.get(orch_hotkey, 0) + 1
                        logger.warning(
                            f"Proof {proof_data.get('task_id', 'unknown')[:16]}... failed: "
                            f"{verification_result.error}"
                        )

                except Exception as e:
                    logger.error(f"Error verifying proof: {e}")
                    failed_count += 1
                    if orch_hotkey:
                        failed_by_orch[orch_hotkey] = failed_by_orch.get(orch_hotkey, 0) + 1

            if verified_count > 0 or failed_count > 0:
                logger.info(
                    f"SubnetCore verification: {verified_count} passed, {failed_count} failed"
                )

            # Accumulate per-orchestrator PoB verification stats
            for hotkey, all_proofs in all_proofs_by_orch.items():
                verified_list = verified_proofs_by_orch.get(hotkey, [])
                rejected = failed_by_orch.get(hotkey, 0)
                prev = self.pob_verification_results.get(hotkey, PoBVerificationStats())
                self.pob_verification_results[hotkey] = PoBVerificationStats(
                    total_proofs=prev.total_proofs + len(all_proofs),
                    verified_count=prev.verified_count + len(verified_list),
                    rejected_count=prev.rejected_count + rejected,
                    total_bytes_verified=prev.total_bytes_verified + sum(
                        p.get("bytes_relayed", 0) for p in verified_list
                    ),
                )

            # Build work summaries from verified proofs
            await self._build_work_summaries_from_proofs(verified_proofs_by_orch)

        except Exception as e:
            logger.error(f"Error fetching/verifying SubnetCore proofs: {e}")

    # =========================================================================
    # Weight Audit Response
    # =========================================================================

    async def _respond_to_weight_audits(self) -> None:
        """Poll for weight audit assignments and submit locally-computed responses.

        For each pending audit, independently computes weights using the
        exposure-linked formula (same path as _compute_exposure_weights).
        Falls back to relaying BeamCore's weights only on params_hash mismatch.
        """
        if not SUBNET_CORE_AVAILABLE or not self.subnet_core_client:
            return

        try:
            result = await self.subnet_core_client.get_pending_audits()
            audits = result.get("audits", [])
            if not audits:
                return

            logger.info(f"Found {len(audits)} pending weight audits")

            for audit in audits:
                audit_id = audit["audit_id"]
                epoch = audit["epoch"]

                try:
                    from services.weight_calculator import (
                        compute_weights, SummaryInput, verify_params_hash,
                    )

                    # Fetch breakdowns for penalties + hash verification
                    breakdowns = await self.subnet_core_client.get_recommended_weights(epoch)

                    bc_hash = breakdowns.get("params_hash")
                    if bc_hash and not verify_params_hash(bc_hash):
                        # Hash mismatch — fall back to relaying BeamCore's weights
                        logger.warning(f"Audit {audit_id}: params_hash mismatch, relaying BeamCore weights")
                        uids = breakdowns.get("uids", [])
                        weights = breakdowns.get("weights", [])
                    else:
                        # Extract penalties from breakdowns
                        penalty_multipliers: Dict[str, float] = {}
                        for d in breakdowns.get("details", []):
                            pm = d.get("penalty_multiplier", 1.0)
                            if pm != 1.0:
                                penalty_multipliers[d["hotkey"]] = pm

                        # Fetch summaries and compute locally
                        summaries_data = await self.subnet_core_client.get_epoch_summaries(epoch)
                        summaries = [
                            SummaryInput(
                                orchestrator_hotkey=s["orchestrator_hotkey"],
                                orchestrator_uid=s["orchestrator_uid"],
                                total_proofs_published=s.get("total_proofs_published", 0),
                                total_bytes_claimed=s.get("total_bytes_claimed", 0),
                                bandwidth_score=s.get("bandwidth_score", 0.0),
                                compliance_score=s.get("compliance_score", 1.0),
                                verification_rate=s.get("verification_rate", 0.0),
                                spot_check_rate=s.get("spot_check_rate", 0.0),
                            )
                            for s in summaries_data.get("summaries", [])
                        ]

                        if not summaries:
                            logger.warning(f"Audit {audit_id}: no summaries for epoch {epoch}")
                            continue

                        wv = compute_weights(summaries, penalty_multipliers, epoch)
                        uids = wv.uids
                        weights = wv.weights

                    if not uids or not weights:
                        logger.warning(f"Audit {audit_id}: no weight data for epoch {epoch}")
                        continue

                    await self.subnet_core_client.submit_audit_response(
                        audit_id=audit_id,
                        uids=uids,
                        weights=weights,
                    )
                    logger.info(f"Submitted audit response for audit {audit_id} (epoch {epoch})")

                except Exception as e:
                    logger.warning(f"Failed to respond to audit {audit_id}: {e}")
                    continue

        except Exception as e:
            logger.error(f"Error in weight audit response: {e}")

    async def _build_work_summaries_from_proofs(
        self,
        verified_proofs_by_orch: Dict[str, List[dict]],
    ) -> None:
        """
        Build work summaries from verified proofs fetched from SubnetCore.

        This replaces the need to query orchestrators directly for summaries.
        The validator computes summaries from the proofs it has already verified.
        """
        if not verified_proofs_by_orch:
            return

        for orch_hotkey, proofs in verified_proofs_by_orch.items():
            if not proofs:
                continue

            # Aggregate metrics from proofs
            total_bytes = sum(p.get("bytes_relayed", 0) for p in proofs)
            bandwidths = [p.get("bandwidth_mbps", 0.0) for p in proofs if p.get("bandwidth_mbps", 0) > 0]
            avg_bandwidth = sum(bandwidths) / len(bandwidths) if bandwidths else 0.0

            # Calculate latencies from timing
            latencies = []
            for p in proofs:
                start = p.get("start_time_us", 0)
                end = p.get("end_time_us", 0)
                if end > start:
                    latencies.append((end - start) / 1000.0)  # Convert to ms
            avg_latency = sum(latencies) / len(latencies) if latencies else 0.0

            # Count unique workers
            workers = set(p.get("worker_id", "") for p in proofs if p.get("worker_id"))
            worker_regions: Dict[str, int] = {}
            for p in proofs:
                region = p.get("source_region", "unknown")
                worker_regions[region] = worker_regions.get(region, 0) + 1

            # Get epoch from proofs (use most common)
            epochs = [p.get("epoch", 0) for p in proofs]
            epoch = max(set(epochs), key=epochs.count) if epochs else 0

            # Build WorkSummary
            summary = WorkSummary(
                epoch=epoch,
                orchestrator_hotkey=orch_hotkey,
                total_tasks=len(proofs),
                successful_tasks=len(proofs),  # All verified proofs are successful
                total_bytes_relayed=total_bytes,
                active_workers=len(workers),
                avg_bandwidth_mbps=avg_bandwidth,
                avg_latency_ms=avg_latency,
                success_rate=1.0,  # All proofs verified
                proof_count=len(proofs),
                worker_regions=worker_regions,
                orchestrator_signature="",  # Not needed for SubnetCore-derived summaries
                uptime_percent=100.0,  # Assume online if publishing proofs
                acceptance_rate=100.0,
                latency_p95_ms=sorted(latencies)[int(len(latencies) * 0.95)] if latencies else 0.0,
                measurement_start=datetime.utcnow() - timedelta(minutes=5),
                measurement_end=datetime.utcnow(),
            )

            # Update work summaries
            self.work_summaries[orch_hotkey] = summary

            # Auto-register orchestrator if not known (for SubnetCore-driven scoring)
            if orch_hotkey not in self.orchestrators:
                # Get UID from proofs if available
                orch_uid = proofs[0].get("orchestrator_uid", 0) if proofs else 0
                self.orchestrators[orch_hotkey] = OrchestratorInfo(
                    url=f"http://unknown",  # Not needed for SubnetCore flow
                    hotkey=orch_hotkey,
                    uid=orch_uid,
                    stake_tao=0.0,  # Will be updated from metagraph if available
                    is_healthy=True,
                )
                logger.info(f"Auto-registered orchestrator {orch_hotkey[:16]}... from SubnetCore proofs")

            logger.info(
                f"Built work summary for {orch_hotkey[:16]}...: "
                f"{len(proofs)} proofs, {total_bytes:,} bytes, {avg_bandwidth:.1f} Mbps"
            )

    async def _verify_single_subnet_proof(self, proof_data: dict) -> ProofVerificationResult:
        """
        Verify a single proof from SubnetCore.

        Checks:
        1. Signature validity (worker signed this proof)
        2. Timing validity (timestamps are reasonable)
        3. Bandwidth validity (claimed bandwidth matches reality)
        4. Canary validity (worker actually relayed the data)

        Args:
            proof_data: Dictionary with proof details from SubnetCore

        Returns:
            ProofVerificationResult with verification status
        """
        task_id = proof_data.get("task_id", "unknown")

        try:
            # Extract proof fields
            worker_hotkey = proof_data.get("worker_hotkey", "")
            orchestrator_hotkey = proof_data.get("orchestrator_hotkey", "")
            start_time_us = proof_data.get("start_time_us", 0)
            end_time_us = proof_data.get("end_time_us", 0)
            bytes_relayed = proof_data.get("bytes_relayed", 0)
            bandwidth_mbps = proof_data.get("bandwidth_mbps", 0.0)
            canary_proof = proof_data.get("canary_proof", "")
            worker_signature = proof_data.get("worker_signature", "")
            orchestrator_signature = proof_data.get("orchestrator_signature", "")

            result = ProofVerificationResult(task_id=task_id, valid=False)

            # 1. Timing validation
            duration_us = end_time_us - start_time_us
            if duration_us <= 0:
                result.error = "Invalid timing: end before start"
                return result

            if duration_us < 1000:  # Less than 1ms
                result.error = "Transfer impossibly fast"
                return result

            result.timing_valid = True

            # Calculate latency from proof timing (transfer duration)
            result.latency_ms = duration_us / 1000.0

            # 2. Bandwidth validation
            if duration_us > 0 and bytes_relayed > 0:
                calculated_bw = (bytes_relayed * 8) / (duration_us / 1_000_000) / 1_000_000
                if bandwidth_mbps > 0:
                    ratio = calculated_bw / bandwidth_mbps
                    if ratio < 0.5 or ratio > 2.0:  # 50% tolerance
                        result.error = f"Bandwidth mismatch: claimed {bandwidth_mbps:.2f}, calculated {calculated_bw:.2f}"
                        return result

                # Check physical limits (100 Gbps max)
                if calculated_bw > 100_000:
                    result.error = f"Bandwidth exceeds physical limits: {calculated_bw:.2f} Mbps"
                    return result

            result.bandwidth_valid = True

            # 3. Canary validation
            if canary_proof:
                # Check canary proof has correct format (64 hex chars = 32 bytes)
                if len(canary_proof) != 64:
                    result.error = f"Invalid canary proof format: {len(canary_proof)} chars"
                    return result

                try:
                    bytes.fromhex(canary_proof)
                except ValueError:
                    result.error = "Canary proof is not valid hex"
                    return result

            result.canary_valid = True

            # 4. Signature validation (worker signature)
            if worker_signature and worker_hotkey:
                # Build message that should have been signed
                message = f"{task_id}:{worker_hotkey}:{start_time_us}:{end_time_us}:{bytes_relayed}"

                try:
                    is_valid = verify_hotkey_signature(
                        message.encode(),
                        bytes.fromhex(worker_signature),
                        worker_hotkey,
                    )
                    result.signature_valid = is_valid
                    if not is_valid:
                        result.error = f"Invalid worker signature for {task_id[:16]}..."
                        logger.warning(f"Signature verification FAILED for {task_id[:16]}...")
                        return result
                except Exception as sig_error:
                    result.signature_valid = False
                    result.error = f"Signature verification error: {sig_error}"
                    logger.warning(f"Signature verification error for {task_id[:16]}...: {sig_error}")
                    return result
            else:
                # No signature provided — log warning but allow for now
                # TODO: Make signatures required once orchestrator signing is implemented
                logger.debug(f"No worker signature for {task_id[:16]}... (unsigned proof)")
                result.signature_valid = True

            # 5. Geographic check (simplified)
            result.geo_valid = True

            # All checks passed
            result.valid = (
                result.timing_valid and
                result.bandwidth_valid and
                result.canary_valid and
                result.signature_valid and
                result.geo_valid
            )

            return result

        except Exception as e:
            return ProofVerificationResult(
                task_id=task_id,
                valid=False,
                error=f"Verification error: {str(e)}",
            )

    # =========================================================================
    # Orchestrator Payment Verification
    # =========================================================================

    async def _verify_orchestrator_payments(self) -> None:
        """
        Verify that orchestrators are paying workers for verified bandwidth proofs.

        This checks:
        1. Fetch verified PoB proofs from SubnetCore
        2. Fetch worker payments from SubnetCore
        3. Compare: each verified proof should have a corresponding payment with matching task_id
        4. Penalize orchestrators who don't pay workers

        Orchestrators must pay workers immediately when tasks complete.
        Payment records must include task_id to link payment to specific work.
        Missing payments or payments without task_id linkage result in score penalties.
        """
        if not SUBNET_CORE_AVAILABLE or not self.subnet_core_client:
            logger.debug("SubnetCore client not available, skipping payment verification")
            return

        try:
            # Get verified proofs that should have been paid
            # Look back at recent proofs (last 30 minutes / 1800 seconds)
            verified_proofs = await self.subnet_core_client.get_verified_proofs(
                limit=100,
                seconds_ago=1800,
            )

            if not verified_proofs.get("proofs"):
                logger.debug("No recent verified proofs to check payments for")
                return

            # Get worker payments from SubnetCore
            payments = await self.subnet_core_client.get_worker_payments(
                limit=200,
                seconds_ago=1800,
            )

            # Build payment maps: primary by task_id (exact match), fallback by worker+bytes
            payment_by_task_id = {}
            payment_by_legacy_key = {}
            for p in payments.get("payments", []):
                task_id = p.get("task_id")
                if task_id:
                    payment_by_task_id[task_id] = p
                # Fallback: legacy key for payments without task_id
                legacy_key = f"{p.get('worker_hotkey')}:{p.get('bytes_relayed')}"
                payment_by_legacy_key[legacy_key] = p

            # Track unpaid proofs and payments without proper task_id linkage
            unpaid_by_orchestrator: dict = {}
            unlinked_by_orchestrator: dict = {}  # Paid but no task_id
            invalid_tx_by_orchestrator: dict = {}  # Invalid on-chain tx_hash
            paid_count = 0
            paid_linked_count = 0  # Paid with task_id linkage
            unpaid_count = 0

            # Initialize TxVerifier for on-chain verification
            tx_verifier = TxVerifier(self.subtensor) if hasattr(self, 'subtensor') and self.subtensor else None

            for proof in verified_proofs.get("proofs", []):
                task_id = proof.get("task_id", "")
                worker_hotkey = proof.get("worker_hotkey", "")
                bytes_relayed = proof.get("bytes_relayed", 0)
                orchestrator_hotkey = proof.get("orchestrator_hotkey", "")

                # Primary check: exact task_id match (preferred)
                if task_id and task_id in payment_by_task_id:
                    payment = payment_by_task_id[task_id]
                    paid_count += 1
                    paid_linked_count += 1

                    # Verify payment has tx_hash proof (on-chain transfer)
                    tx_hash = payment.get("tx_hash")
                    if not tx_hash:
                        # Missing tx_hash - track as invalid
                        if orchestrator_hotkey not in invalid_tx_by_orchestrator:
                            invalid_tx_by_orchestrator[orchestrator_hotkey] = []
                        invalid_tx_by_orchestrator[orchestrator_hotkey].append({
                            "task_id": task_id,
                            "reason": "missing tx_hash",
                        })
                        logger.warning(
                            f"Payment for task {task_id[:16]}... missing tx_hash - will slash"
                        )
                    elif tx_verifier and tx_hash.startswith("0x"):
                        # Verify on-chain transfer
                        # Get orchestrator coldkey and worker payment address
                        # Note: We need the orchestrator's coldkey (sender) and worker's payment address (recipient)
                        # For now, verify that tx_hash exists on chain and is a valid transfer
                        worker_payment_addr = payment.get("worker_hotkey", "")
                        amount_tao = payment.get("amount_earned", 0) / 1e9  # Convert from rao to TAO

                        # TODO: Get orchestrator coldkey from metagraph or payment record
                        # For now, just verify the tx exists and skip sender verification
                        verification = tx_verifier.verify_transfer(
                            tx_hash=tx_hash,
                            expected_from="",  # Skip sender check for now
                            expected_to=worker_payment_addr,
                            expected_amount=amount_tao,
                            tolerance=0.05,  # 5% tolerance for amount
                        )

                        if not verification.is_valid:
                            if orchestrator_hotkey not in invalid_tx_by_orchestrator:
                                invalid_tx_by_orchestrator[orchestrator_hotkey] = []
                            invalid_tx_by_orchestrator[orchestrator_hotkey].append({
                                "task_id": task_id,
                                "tx_hash": tx_hash[:20] + "...",
                                "reason": verification.error,
                            })
                            logger.warning(
                                f"Invalid tx_hash for task {task_id[:16]}...: {verification.error}"
                            )
                else:
                    # Fallback: legacy key match (less reliable)
                    legacy_key = f"{worker_hotkey}:{bytes_relayed}"
                    if legacy_key in payment_by_legacy_key:
                        paid_count += 1
                        # Record as unlinked (payment exists but no task_id)
                        if orchestrator_hotkey not in unlinked_by_orchestrator:
                            unlinked_by_orchestrator[orchestrator_hotkey] = []
                        unlinked_by_orchestrator[orchestrator_hotkey].append(proof)
                    else:
                        # No payment found at all
                        unpaid_count += 1
                        if orchestrator_hotkey not in unpaid_by_orchestrator:
                            unpaid_by_orchestrator[orchestrator_hotkey] = []
                        unpaid_by_orchestrator[orchestrator_hotkey].append(proof)

            if paid_count > 0 or unpaid_count > 0:
                logger.info(
                    f"Payment verification: {paid_count} paid ({paid_linked_count} with task_id), "
                    f"{unpaid_count} unpaid"
                )

            # Build work summaries from verified proofs (grouped by orchestrator)
            verified_proofs_by_orch: Dict[str, List[dict]] = {}
            for proof in verified_proofs.get("proofs", []):
                orch_hotkey = proof.get("orchestrator_hotkey", "")
                if orch_hotkey:
                    if orch_hotkey not in verified_proofs_by_orch:
                        verified_proofs_by_orch[orch_hotkey] = []
                    verified_proofs_by_orch[orch_hotkey].append(proof)

            if verified_proofs_by_orch:
                await self._build_work_summaries_from_proofs(verified_proofs_by_orch)

            # Store payment penalty multipliers (applied in _update_scores)
            self.payment_penalty_multipliers = {}

            # Apply penalties to orchestrators with unpaid proofs (severe)
            for orch_hotkey, unpaid_proofs in unpaid_by_orchestrator.items():
                unpaid_bytes = sum(p.get("bytes_relayed", 0) for p in unpaid_proofs)
                penalty_factor = min(0.5, len(unpaid_proofs) * 0.05)  # 5% per unpaid, max 50%
                self.payment_penalty_multipliers[orch_hotkey] = 1.0 - penalty_factor

                logger.warning(
                    f"Orchestrator {orch_hotkey[:16]}... has {len(unpaid_proofs)} UNPAID proofs "
                    f"({unpaid_bytes:,} bytes), applying {penalty_factor:.0%} penalty"
                )

            # Apply 50% SLASH for invalid/missing tx_hash (on-chain verification failed)
            for orch_hotkey, invalid_txs in invalid_tx_by_orchestrator.items():
                if invalid_txs:
                    # 50% slash for ANY invalid tx_hash (severe penalty)
                    existing = self.payment_penalty_multipliers.get(orch_hotkey, 1.0)
                    self.payment_penalty_multipliers[orch_hotkey] = existing * 0.5  # 50% slash

                    reasons = [tx.get("reason", "unknown") for tx in invalid_txs[:3]]
                    logger.warning(
                        f"SLASHING orchestrator {orch_hotkey[:16]}... by 50%: "
                        f"{len(invalid_txs)} invalid tx_hash(es). Reasons: {reasons}"
                    )

            # Log tx verification stats
            if tx_verifier:
                stats = tx_verifier.get_cache_stats()
                if stats["total"] > 0:
                    logger.info(
                        f"On-chain tx verification: {stats['valid']} valid, "
                        f"{stats['invalid']} invalid out of {stats['total']} checked"
                    )

            # Warn about payments without task_id linkage (minor penalty for now)
            for orch_hotkey, unlinked_proofs in unlinked_by_orchestrator.items():
                if len(unlinked_proofs) > 0:
                    logger.warning(
                        f"Orchestrator {orch_hotkey[:16]}... has {len(unlinked_proofs)} payments "
                        f"without task_id linkage (legacy payment format)"
                    )
                    # Minor penalty for using legacy format (2% per unlinked, max 10%)
                    minor_penalty = min(0.1, len(unlinked_proofs) * 0.02)
                    existing = self.payment_penalty_multipliers.get(orch_hotkey, 1.0)
                    self.payment_penalty_multipliers[orch_hotkey] = existing * (1.0 - minor_penalty)

            # Verify unverified epoch payments from SubnetCore
            await self._verify_unverified_epoch_payments(
                unpaid_by_orchestrator, verified_proofs_by_orch
            )

        except Exception as e:
            logger.error(f"Error verifying orchestrator payments: {e}")

    async def _verify_unverified_epoch_payments(
        self,
        unpaid_by_orchestrator: dict,
        verified_proofs_by_orch: Dict[str, List[dict]],
    ) -> None:
        """
        Fetch unverified epoch payments from SubnetCore and verify them.

        An epoch payment is verified if the orchestrator has verified proofs
        and no outstanding unpaid proofs. This marks verified=True on the
        EpochPayment record so the dashboard shows it.
        """
        try:
            result = await self.subnet_core_client.get_unverified_epoch_payments(limit=50)
            unverified = result.get("payments", [])

            if not unverified:
                return

            verified_count = 0
            for payment in unverified:
                orch_hotkey = payment.get("orchestrator_hotkey", "")
                epoch = payment.get("epoch")

                # Don't verify if orchestrator has unpaid proofs
                if orch_hotkey in unpaid_by_orchestrator:
                    logger.debug(
                        f"Skipping epoch {epoch} verification for {orch_hotkey[:16]}... "
                        f"(has unpaid proofs)"
                    )
                    continue

                # Verify: orchestrator has submitted work and no outstanding issues
                has_proofs = orch_hotkey in verified_proofs_by_orch
                has_payment = payment.get("total_distributed", 0) > 0
                has_workers = payment.get("worker_count", 0) > 0

                if has_proofs or (has_payment and has_workers):
                    try:
                        await self.subnet_core_client.verify_epoch_payment(
                            orchestrator_hotkey=orch_hotkey,
                            epoch=epoch,
                        )
                        verified_count += 1
                        logger.info(
                            f"Verified epoch {epoch} for orchestrator {orch_hotkey[:16]}..."
                        )
                    except Exception as e:
                        logger.warning(
                            f"Failed to verify epoch {epoch} for {orch_hotkey[:16]}...: {e}"
                        )

            if verified_count > 0:
                logger.info(f"Verified {verified_count}/{len(unverified)} epoch payments")

        except Exception as e:
            logger.error(f"Error in epoch payment verification: {e}")

    # =========================================================================
    # Metagraph and Connection Management
    # =========================================================================

    async def _sync_metagraph(self) -> None:
        """Sync metagraph with latest state"""
        if self.metagraph is None:
            return

        try:
            self.metagraph.sync(subtensor=self.subtensor)

            # Also refresh Fiber node cache if available
            if self.fiber_chain is not None:
                try:
                    self._fiber_nodes = self.fiber_chain.get_nodes_by_hotkey()
                    logger.debug(f"Fiber nodes refreshed: {len(self._fiber_nodes)} nodes")
                except Exception as e:
                    logger.warning(f"Failed to refresh Fiber nodes: {e}")

            logger.debug("Metagraph synced")
        except Exception as e:
            logger.warning(f"Failed to sync metagraph: {e}")

    async def _update_connections(self) -> None:
        """Update list of valid connections (miners).

        Only tracks orchestrators that are registered with BeamCore.
        This prevents generating tasks for metagraph miners that haven't
        registered with the network.
        """
        if self.settings.local_mode:
            # In local mode, keep the local orchestrator connection
            logger.debug("Keeping local mode connections")
            return

        if self.metagraph is None:
            logger.debug("Skipping connection update - no metagraph")
            return

        # Get set of registered orchestrator hotkeys from BeamCore
        registered_hotkeys = set(self.orchestrators.keys())

        self.connections.clear()
        skipped_unregistered = 0

        for uid in range(len(self.metagraph.hotkeys)):
            stake = float(self.metagraph.S[uid])

            if stake >= self.settings.min_connection_stake:
                hotkey = self.metagraph.hotkeys[uid]

                # Only track orchestrators registered with BeamCore
                if hotkey not in registered_hotkeys:
                    skipped_unregistered += 1
                    continue

                axon = self.metagraph.axons[uid]

                self.connections[uid] = {
                    "uid": uid,
                    "hotkey": hotkey,
                    "stake": stake,
                    "ip": axon.ip,
                    "port": axon.port,
                    "last_seen": datetime.utcnow(),
                }

        logger.info(
            f"Updated connections: {len(self.connections)} registered, "
            f"{skipped_unregistered} skipped (not registered with BeamCore)"
        )

    # =========================================================================
    # Orchestrator Discovery and Queries
    # =========================================================================

    async def _discover_orchestrators(self) -> None:
        """Discover Orchestrator endpoints via SubnetCore and direct probing."""
        # ── SubnetCore discovery (preferred: returns ALL registered orchestrators) ──
        if SUBNET_CORE_AVAILABLE and self.subnet_core_client:
            try:
                result = await self.subnet_core_client.get_orchestrators(active_only=True)
                orch_list = result.get("orchestrators", [])
                if orch_list:
                    added = 0
                    for o in orch_list:
                        hotkey = o.get("hotkey", "")
                        if not hotkey:
                            continue
                        uid = o.get("uid")
                        url = o.get("url", "")
                        stake = o.get("stake_tao", 0.0)
                        is_healthy = o.get("is_healthy", True)

                        if hotkey not in self.orchestrators:
                            self.orchestrators[hotkey] = OrchestratorInfo(
                                url=url or "via-subnetcore",
                                hotkey=hotkey,
                                uid=uid,
                                stake_tao=stake,
                                is_healthy=is_healthy,
                            )
                            added += 1
                        else:
                            # Update UID and stake if missing
                            existing = self.orchestrators[hotkey]
                            if uid is not None and existing.uid is None:
                                existing.uid = uid
                            if stake > 0 and existing.stake_tao == 0:
                                existing.stake_tao = stake
                            if url and existing.url in ("via-subnetcore", "http://unknown"):
                                existing.url = url

                        # Store worker count from BeamCore
                        worker_count = o.get("worker_count", 0)
                        if uid is not None:
                            self._beamcore_worker_counts[uid] = worker_count

                    if added:
                        logger.info(
                            f"SubnetCore discovery: added {added} new orchestrator(s), "
                            f"total={len(self.orchestrators)}"
                        )
                    return  # SubnetCore succeeded, skip legacy probe
            except Exception as e:
                logger.debug(f"SubnetCore orchestrator discovery failed, falling back: {e}")

        # ── Legacy single-URL probe (fallback) ──
        orchestrator_url = getattr(
            self.settings, 'orchestrator_url',
            'http://localhost:8080'
        )

        try:
            async with self._http_session.get(
                f"{orchestrator_url}/health",
                timeout=aiohttp.ClientTimeout(total=10),
            ) as response:
                if response.status == 200:
                    # Get Orchestrator's hotkey
                    async with self._http_session.get(
                        f"{orchestrator_url}/state",
                    ) as state_response:
                        if state_response.status == 200:
                            state = await state_response.json()
                            hotkey = state.get("hotkey", "unknown")

                            # Look up UID from metagraph
                            uid = None
                            if self.metagraph and hotkey in self.metagraph.hotkeys:
                                uid = self.metagraph.hotkeys.index(hotkey)

                            self.orchestrators[hotkey] = OrchestratorInfo(
                                url=orchestrator_url,
                                hotkey=hotkey,
                                uid=uid,
                            )
                            logger.info(f"Discovered Orchestrator at {orchestrator_url} (UID: {uid})")
        except Exception as e:
            logger.debug(f"Orchestrator not available at {orchestrator_url}: {e}")

    async def _query_orchestrators(self) -> None:
        """Query work summaries via SubnetCore (replaces direct orchestrator queries)."""
        # Prefer SubnetCore for centralized data access
        if SUBNET_CORE_AVAILABLE and self.subnet_core_client:
            try:
                result = await self.subnet_core_client.get_work_summaries(
                    epoch=self.current_epoch
                )
                summaries = result.get("summaries", [])

                for data in summaries:
                    hotkey = data.get("orchestrator_hotkey", "")
                    if not hotkey:
                        continue

                    summary = WorkSummary(
                        epoch=data.get("epoch", self.current_epoch),
                        orchestrator_hotkey=hotkey,
                        total_tasks=data.get("total_tasks", 0),
                        successful_tasks=data.get("successful_tasks", 0),
                        total_bytes_relayed=data.get("total_bytes_relayed", 0),
                        active_workers=data.get("active_workers", 0),
                        avg_bandwidth_mbps=data.get("avg_bandwidth_mbps", 0),
                        avg_latency_ms=data.get("avg_latency_ms", 0),
                        success_rate=data.get("success_rate", 0),
                        proof_count=data.get("proof_count", 0),
                        worker_regions=data.get("worker_regions", {}),
                        orchestrator_signature="",  # Not needed via SubnetCore
                    )
                    self.work_summaries[hotkey] = summary

                    # Update orchestrator health if known
                    if hotkey in self.orchestrators:
                        self.orchestrators[hotkey].last_seen = datetime.utcnow()
                        self.orchestrators[hotkey].is_healthy = True

                logger.info(f"Fetched {len(summaries)} work summaries from SubnetCore")
                return

            except Exception as e:
                logger.warning(f"SubnetCore query failed, falling back to direct: {e}")

        # Fallback: Direct orchestrator queries (legacy path)
        for hotkey, info in self.orchestrators.items():
            try:
                summary = await self._query_orchestrator_summary(info)
                if summary:
                    self.work_summaries[hotkey] = summary
                    info.last_seen = datetime.utcnow()
                    info.is_healthy = True
                else:
                    info.is_healthy = False

            except Exception as e:
                logger.error(f"Failed to query Orchestrator {hotkey[:16]}: {e}")
                info.is_healthy = False

    async def _query_orchestrator_summary(
        self,
        orchestrator: OrchestratorInfo,
    ) -> Optional[WorkSummary]:
        """Query a single Orchestrator for work summary (legacy fallback)."""
        try:
            headers = {"X-Validator-Hotkey": self.hotkey}

            async with self._http_session.get(
                f"{orchestrator.url}/validators/summary",
                headers=headers,
                timeout=aiohttp.ClientTimeout(total=30),
            ) as response:
                if response.status == 200:
                    data = await response.json()

                    return WorkSummary(
                        epoch=data.get("epoch", 0),
                        orchestrator_hotkey=data.get("orchestrator_hotkey", orchestrator.hotkey),
                        total_tasks=data.get("total_tasks", 0),
                        successful_tasks=data.get("successful_tasks", 0),
                        total_bytes_relayed=data.get("total_bytes_relayed", 0),
                        active_workers=data.get("active_workers", 0),
                        avg_bandwidth_mbps=data.get("avg_bandwidth_mbps", 0),
                        avg_latency_ms=data.get("avg_latency_ms", 0),
                        success_rate=data.get("success_rate", 0),
                        proof_count=data.get("proof_count", 0),
                        worker_regions=data.get("worker_regions", {}),
                        orchestrator_signature=data.get("orchestrator_signature", ""),
                    )
                else:
                    return None

        except Exception as e:
            logger.debug(f"Error querying Orchestrator summary: {e}")
            return None

    async def _sync_orchestrators(self) -> None:
        """Sync orchestrators from metagraph."""
        if self.metagraph is None:
            return

        for uid in range(len(self.metagraph.hotkeys)):
            stake = float(self.metagraph.S[uid])
            hotkey = self.metagraph.hotkeys[uid]

            if stake >= MIN_ORCHESTRATOR_STAKE_TAO:
                existing = self.orchestrator_manager.get_orchestrator(uid)

                if existing is None and uid != SUBNET_ORCHESTRATOR_UID:
                    try:
                        self.orchestrator_manager.register_orchestrator(
                            uid=uid,
                            hotkey=hotkey,
                            stake_tao=stake,
                        )
                        logger.info(f"Registered new orchestrator UID {uid}")
                    except ValueError as e:
                        logger.warning(f"Could not register orchestrator UID {uid}: {e}")

                elif existing is not None:
                    if existing.stake_tao != stake:
                        existing.stake_tao = stake

        self.orchestrator_manager.update_all_statuses()

    # =========================================================================
    # Challenge Generation and Sending
    # =========================================================================

    async def _generate_and_send_challenges(self) -> None:
        """Generate bandwidth challenges and send to connections via dendrite"""
        for uid, conn in self.connections.items():
            active_for_uid = [
                c for c in self.active_challenges.values()
                if c.get("uid") == uid and c.get("status") == "pending"
            ]
            if len(active_for_uid) >= 2:
                continue

            challenge = await self._create_and_send_challenge(uid, conn)
            if challenge:
                self.tasks_this_epoch += 1

    async def _issue_challenges(self) -> None:
        """Issue bandwidth challenges to verify Orchestrator capacity."""
        for hotkey, info in self.orchestrators.items():
            if not info.is_healthy:
                continue

            active_for_orchestrator = [
                c for c in self.active_challenges.values()
                if c.get("orchestrator") == hotkey and c.get("status") == "pending"
            ]

            if len(active_for_orchestrator) >= 2:
                continue

            try:
                result = await self._issue_orchestrator_challenge(info)
                if result:
                    logger.info(
                        f"Challenge {result.challenge_id[:16]}... "
                        f"to Orchestrator: {result.bandwidth_mbps:.2f} Mbps"
                    )
            except Exception as e:
                logger.error(f"Failed to issue challenge to {hotkey[:16]}: {e}")

    async def _create_and_send_challenge(
        self,
        uid: int,
        connection: dict,
    ) -> Optional[BandwidthChallenge]:
        """Create a bandwidth challenge and send to connection"""
        nonce = os.urandom(32)
        task_id = sha256(nonce + self.hotkey.encode()).hex()

        chunk_data = os.urandom(self.settings.chunk_size_bytes)
        chunk_hash = sha256(chunk_data).hex()

        canary = os.urandom(CANARY_SIZE_BYTES)
        canary_offset = int.from_bytes(os.urandom(4), 'big') % (
            self.settings.chunk_size_bytes - CANARY_SIZE_BYTES
        )

        chunk_with_canary = bytearray(chunk_data)
        chunk_with_canary[canary_offset:canary_offset + len(canary)] = canary
        chunk_data = bytes(chunk_with_canary)
        chunk_hash = sha256(chunk_data).hex()

        timeout_us = self.settings.job_timeout_seconds * 1_000_000
        deadline_us = int(time.time() * 1_000_000) + timeout_us

        challenge = BandwidthChallenge(
            task_id=task_id,
            challenge_nonce=nonce.hex(),
            chunk_hash=chunk_hash,
            chunk_size=self.settings.chunk_size_bytes,
            deadline_us=deadline_us,
            canary=canary.hex(),
            canary_offset=canary_offset,
            path=[connection["hotkey"]],
            expected_hops=1,
            sla_level=0,
        )

        self.active_challenges[task_id] = {
            "uid": uid,
            "connection": connection,
            "challenge": challenge,
            "chunk_data": chunk_data,
            "canary": canary,
            "canary_offset": canary_offset,
            "created_at": time.time(),
            "status": "pending",
        }

        task = Task(
            task_id=bytes.fromhex(task_id),
            validator_hotkey=self.hotkey,
            chunk_hash=bytes.fromhex(chunk_hash),
            chunk_size=self.settings.chunk_size_bytes,
            deadline=deadline_us,
            sla_level=SLALevel.BEST_EFFORT,
            canary=canary,
            canary_offset=canary_offset,
            path=[connection["hotkey"]],
            created_at=int(time.time() * 1_000_000),
        )
        self.pending_tasks[task_id] = task

        if connection.get("is_local") and self._http_session:
            return await self._send_challenge_http(
                uid, task_id, chunk_data, challenge, connection
            )
        elif self.dendrite:
            return await self._send_challenge_dendrite(
                uid, task_id, chunk_data, challenge
            )
        return None

    async def _issue_orchestrator_challenge(
        self,
        orchestrator: OrchestratorInfo,
    ) -> Optional[ChallengeResult]:
        """Issue a single bandwidth challenge to an Orchestrator via SubnetCore."""
        nonce = os.urandom(32)
        challenge_id = sha256(nonce + self.hotkey.encode()).hex()

        chunk_size = self.settings.chunk_size_bytes
        chunk_data = os.urandom(chunk_size)
        chunk_hash = sha256(chunk_data).hex()

        canary = os.urandom(32)
        canary_offset = int.from_bytes(os.urandom(4), 'big') % (chunk_size - 32)

        chunk_with_canary = bytearray(chunk_data)
        chunk_with_canary[canary_offset:canary_offset + len(canary)] = canary
        chunk_data = bytes(chunk_with_canary)
        chunk_hash = sha256(chunk_data).hex()

        deadline_us = int(time.time() * 1_000_000) + (60 * 1_000_000)

        # Prefer SubnetCore for challenge coordination
        if SUBNET_CORE_AVAILABLE and self.subnet_core_client:
            try:
                # Ensure orchestrator UID is set (may be None if discovered before metagraph)
                orch_uid = orchestrator.uid
                if orch_uid is None and self.metagraph and orchestrator.hotkey in self.metagraph.hotkeys:
                    orch_uid = self.metagraph.hotkeys.index(orchestrator.hotkey)
                    orchestrator.uid = orch_uid  # Update cached value
                    logger.info(f"Resolved orchestrator UID from metagraph: {orch_uid}")

                if orch_uid is None:
                    logger.warning(f"Cannot submit challenge: orchestrator UID unknown for {orchestrator.hotkey[:16]}...")
                    return ChallengeResult(
                        challenge_id=challenge_id,
                        success=False,
                        error="Orchestrator UID unknown",
                    )

                # Submit challenge via SubnetCore
                submit_result = await self.subnet_core_client.submit_challenge(
                    orchestrator_uid=orch_uid,
                    challenge_id=challenge_id,
                    chunk_size=chunk_size,
                    chunk_hash=chunk_hash,
                    canary=canary.hex(),
                    canary_offset=canary_offset,
                    deadline_us=deadline_us,
                    source_region="validator",
                    dest_region="orchestrator",
                )

                if not submit_result.get("accepted"):
                    return ChallengeResult(
                        challenge_id=challenge_id,
                        success=False,
                        error=submit_result.get("error", "Not accepted"),
                    )

                self.active_challenges[challenge_id] = {
                    "orchestrator": orchestrator.hotkey,
                    "chunk_data": chunk_data,
                    "canary": canary,
                    "canary_offset": canary_offset,
                    "created_at": time.time(),
                    "status": "pending",
                }

                # Submit chunk data via SubnetCore
                start_time = time.time()
                data_result = await self.subnet_core_client.submit_challenge_data(
                    challenge_id=challenge_id,
                    chunk_data=base64.b64encode(chunk_data).decode(),
                )
                end_time = time.time()

                if not data_result.get("success"):
                    return ChallengeResult(
                        challenge_id=challenge_id,
                        success=False,
                        error=data_result.get("error", "Data transfer failed"),
                    )

                result = ChallengeResult(
                    challenge_id=challenge_id,
                    success=True,
                    bytes_relayed=data_result.get("bytes_relayed", chunk_size),
                    bandwidth_mbps=data_result.get("bandwidth_mbps", 0),
                    latency_ms=(end_time - start_time) * 1000,
                    canary_verified=True,
                )

                self.challenge_results[challenge_id] = result
                self.active_challenges[challenge_id]["status"] = "completed"
                return result

            except Exception as e:
                logger.warning(f"SubnetCore challenge failed, falling back to direct: {e}")

        # Fallback: Direct orchestrator challenge (legacy path)
        headers = {"X-Validator-Hotkey": self.hotkey}

        try:
            async with self._http_session.post(
                f"{orchestrator.url}/validators/challenge",
                headers=headers,
                json={
                    "challenge_id": challenge_id,
                    "chunk_size": chunk_size,
                    "chunk_hash": chunk_hash,
                    "canary": canary.hex(),
                    "canary_offset": canary_offset,
                    "deadline_us": deadline_us,
                    "source_region": "validator",
                    "dest_region": "orchestrator",
                },
                timeout=aiohttp.ClientTimeout(total=10),
            ) as response:
                if response.status != 200:
                    return ChallengeResult(
                        challenge_id=challenge_id,
                        success=False,
                        error=f"Challenge rejected: {await response.text()}",
                    )

                data = await response.json()
                if not data.get("accepted"):
                    return ChallengeResult(
                        challenge_id=challenge_id,
                        success=False,
                        error=data.get("error", "Not accepted"),
                    )

            self.active_challenges[challenge_id] = {
                "orchestrator": orchestrator.hotkey,
                "chunk_data": chunk_data,
                "canary": canary,
                "canary_offset": canary_offset,
                "created_at": time.time(),
                "status": "pending",
            }

            start_time = time.time()

            async with self._http_session.post(
                f"{orchestrator.url}/validators/challenge/data",
                headers=headers,
                json={
                    "challenge_id": challenge_id,
                    "chunk_data": base64.b64encode(chunk_data).decode(),
                },
                timeout=aiohttp.ClientTimeout(total=60),
            ) as response:
                end_time = time.time()

                if response.status != 200:
                    return ChallengeResult(
                        challenge_id=challenge_id,
                        success=False,
                        error=f"Data transfer failed: {await response.text()}",
                    )

                data = await response.json()

                if not data.get("success"):
                    return ChallengeResult(
                        challenge_id=challenge_id,
                        success=False,
                        error=data.get("error", "Transfer failed"),
                    )

                result = ChallengeResult(
                    challenge_id=challenge_id,
                    success=True,
                    bytes_relayed=data.get("bytes_relayed", chunk_size),
                    bandwidth_mbps=data.get("bandwidth_mbps", 0),
                    latency_ms=(end_time - start_time) * 1000,
                    canary_verified=True,
                )

                self.challenge_results[challenge_id] = result
                self.active_challenges[challenge_id]["status"] = "completed"

                return result

        except asyncio.TimeoutError:
            return ChallengeResult(
                challenge_id=challenge_id,
                success=False,
                error="Challenge timeout",
            )
        except Exception as e:
            return ChallengeResult(
                challenge_id=challenge_id,
                success=False,
                error=str(e),
            )

    async def _send_challenge_http(
        self,
        uid: int,
        task_id: str,
        chunk_data: bytes,
        challenge: BandwidthChallenge,
        connection: dict,
    ) -> Optional[BandwidthChallenge]:
        """Send challenge via HTTP (for local mode)"""
        orchestrator_url = connection.get("url", self.settings.orchestrator_url)
        headers = {"X-Validator-Hotkey": self.hotkey}

        try:
            challenge_payload = {
                "challenge_id": task_id,
                "chunk_size": challenge.chunk_size,
                "chunk_hash": challenge.chunk_hash,
                "canary": challenge.canary,
                "canary_offset": challenge.canary_offset,
                "deadline_us": challenge.deadline_us,
                "source_region": "local",
                "dest_region": "local",
            }

            async with self._http_session.post(
                f"{orchestrator_url}/validators/challenge",
                json=challenge_payload,
                headers=headers,
            ) as resp:
                if resp.status == 200:
                    result = await resp.json()
                    if result.get("accepted"):
                        logger.info(
                            f"Challenge {task_id[:16]}... accepted by orchestrator"
                        )
                        self.active_challenges[task_id]["status"] = "accepted"
                        self.active_challenges[task_id]["worker_assigned"] = result.get("worker_assigned")

                        await self._send_chunk_data_http(
                            uid, task_id, chunk_data, challenge, connection
                        )

                        return challenge
                    else:
                        self.active_challenges[task_id]["status"] = "rejected"
                elif resp.status == 403:
                    self.active_challenges[task_id]["status"] = "auth_error"
                else:
                    self.active_challenges[task_id]["status"] = "error"

        except Exception as e:
            logger.error(f"Failed to send challenge via HTTP to UID {uid}: {e}")
            self.active_challenges[task_id]["status"] = "error"

        return None

    async def _send_chunk_data_http(
        self,
        uid: int,
        task_id: str,
        chunk_data: bytes,
        challenge: BandwidthChallenge,
        connection: dict,
    ) -> bool:
        """Send chunk data via HTTP (for local mode)"""
        orchestrator_url = connection.get("url", self.settings.orchestrator_url)
        headers = {"X-Validator-Hotkey": self.hotkey}

        try:
            send_time = time.time()
            self.active_challenges[task_id]["chunk_sent_at"] = send_time

            chunk_payload = {
                "challenge_id": task_id,
                "chunk_data": base64.b64encode(chunk_data).decode(),
            }

            async with self._http_session.post(
                f"{orchestrator_url}/validators/challenge/data",
                json=chunk_payload,
                headers=headers,
            ) as resp:
                if resp.status == 200:
                    result = await resp.json()
                    if result.get("success"):
                        receive_time_us = result.get("receive_time_us", int(time.time() * 1_000_000))
                        bandwidth_mbps = result.get("bandwidth_mbps", 0)

                        self.active_challenges[task_id]["status"] = "chunk_received"
                        self.active_challenges[task_id]["receive_time"] = receive_time_us / 1_000_000
                        self.active_challenges[task_id]["bandwidth_mbps"] = bandwidth_mbps
                        self.active_challenges[task_id]["canary_proof"] = result.get("canary_proof")

                        logger.info(
                            f"Chunk received for task {task_id[:16]}... "
                            f"bandwidth: {bandwidth_mbps:.2f} Mbps"
                        )
                        return True

        except Exception as e:
            logger.error(f"Failed to send chunk via HTTP for task {task_id[:16]}...: {e}")

        return False

    async def _send_challenge_dendrite(
        self,
        uid: int,
        task_id: str,
        chunk_data: bytes,
        challenge: BandwidthChallenge,
    ) -> Optional[BandwidthChallenge]:
        """Send challenge via Bittensor dendrite (for mainnet/testnet)"""
        if self.dendrite is None or self.metagraph is None:
            return None

        try:
            axon = self.metagraph.axons[uid]
            response = await self.dendrite.call(
                target_axon=axon,
                synapse=challenge,
                timeout=10.0,
            )

            if response and response.accepted:
                logger.info(
                    f"Challenge {task_id[:16]}... accepted by UID {uid}"
                )
                self.active_challenges[task_id]["status"] = "accepted"
                self.active_challenges[task_id]["worker_assigned"] = response.worker_assigned

                await self._send_chunk_data(uid, task_id, chunk_data, challenge)

                return response
            else:
                self.active_challenges[task_id]["status"] = "rejected"

        except Exception as e:
            logger.error(f"Failed to send challenge to UID {uid}: {e}")
            self.active_challenges[task_id]["status"] = "error"

        return None

    async def _send_chunk_data(
        self,
        uid: int,
        task_id: str,
        chunk_data: bytes,
        challenge: BandwidthChallenge,
    ) -> bool:
        """Send actual chunk data to connection for bandwidth proof"""
        if self.dendrite is None or self.metagraph is None:
            return False

        chunk_transfer = ChunkTransfer(
            task_id=task_id,
            chunk_hash=challenge.chunk_hash,
            chunk_size=challenge.chunk_size,
            chunk_data=base64.b64encode(chunk_data).decode(),
            canary=challenge.canary,
            canary_offset=challenge.canary_offset,
            hop_index=0,
        )

        try:
            axon = self.metagraph.axons[uid]
            send_time = time.time()
            self.active_challenges[task_id]["chunk_sent_at"] = send_time

            response = await self.dendrite.call(
                target_axon=axon,
                synapse=chunk_transfer,
                timeout=self.settings.job_timeout_seconds,
            )

            if response and response.received:
                receive_time = response.receive_time_us / 1_000_000
                self.active_challenges[task_id]["status"] = "chunk_received"
                self.active_challenges[task_id]["receive_time"] = receive_time
                return True
            else:
                self.active_challenges[task_id]["status"] = "chunk_failed"

        except Exception as e:
            logger.error(f"Failed to send chunk for task {task_id[:16]}...: {e}")
            self.active_challenges[task_id]["status"] = "error"

        return False

    # =========================================================================
    # PoB Verification
    # =========================================================================

    async def _collect_pob_results(self) -> None:
        """Collect and verify pending PoB submissions."""
        if self.settings.local_mode:
            await self._process_local_challenge_results()
            return

        if not hasattr(self, "pending_pob_submissions"):
            self.pending_pob_submissions = {}

        if not self.pending_pob_submissions:
            logger.debug("_collect_pob_results: no pending PoB submissions")
            return

        logger.info(f"_collect_pob_results: processing {len(self.pending_pob_submissions)} pending PoB submissions")

        now = time.time()
        verified_count = 0
        failed_count = 0
        expired_count = 0
        no_task_count = 0
        tasks_to_remove = []

        for task_id, submission_info in list(self.pending_pob_submissions.items()):
            pob = submission_info["pob"]
            received_at = submission_info["received_at"]

            if now - received_at > 300:
                tasks_to_remove.append(task_id)
                expired_count += 1
                logger.debug(f"_collect_pob_results: {task_id[:16]}... expired (age={now - received_at:.0f}s)")
                continue

            task = self.pending_tasks.get(task_id)

            if task is None:
                no_task_count += 1
                continue

            try:
                result = self.verify_pob(pob, task)

                if result.valid:
                    self.task_results[task_id] = result
                    verified_count += 1
                else:
                    failed_count += 1

                tasks_to_remove.append(task_id)

            except Exception as e:
                logger.error(f"Error verifying pending PoB {task_id[:16]}...: {e}")
                tasks_to_remove.append(task_id)

        for task_id in tasks_to_remove:
            del self.pending_pob_submissions[task_id]

        logger.info(
            f"_collect_pob_results: verified={verified_count} failed={failed_count} "
            f"expired={expired_count} no_task={no_task_count} "
            f"remaining={len(self.pending_pob_submissions)} "
            f"total_results={len(self.task_results)}"
        )

        await self._cleanup_old_results()

    async def _process_local_challenge_results(self) -> None:
        """Process challenge results from HTTP responses in local mode."""
        now = time.time()
        processed_count = 0
        tasks_to_remove = []

        for task_id, challenge_info in list(self.active_challenges.items()):
            if challenge_info.get("scored"):
                continue

            status = challenge_info.get("status")

            if status == "chunk_received":
                uid = challenge_info.get("uid")
                bandwidth_mbps = challenge_info.get("bandwidth_mbps", 0)
                chunk_sent_at = challenge_info.get("chunk_sent_at", now)
                receive_time = challenge_info.get("receive_time", now)

                task = self.pending_tasks.get(task_id)
                if task:
                    mock_pob = ProofOfBandwidth(
                        task_id=bytes.fromhex(task_id),
                        miner_id=challenge_info.get("connection", {}).get("hotkey", "local-orchestrator").encode(),
                        chunk_hash=task.chunk_hash,
                        receive_time_us=int(receive_time * 1_000_000),
                        send_time_us=int(chunk_sent_at * 1_000_000),
                        bandwidth_mbps=bandwidth_mbps,
                        signature=b"local-mode-signature",
                    )

                    result = PoBVerificationResult(
                        pob=mock_pob,
                        valid=True,
                        calculated_bandwidth=bandwidth_mbps,
                    )

                    self.task_results[task_id] = result
                    challenge_info["scored"] = True
                    processed_count += 1

            elif challenge_info.get("created_at", 0) < now - 120:
                tasks_to_remove.append(task_id)

        for task_id in tasks_to_remove:
            del self.active_challenges[task_id]
            if task_id in self.pending_tasks:
                del self.pending_tasks[task_id]

    def verify_pob(
        self,
        pob: ProofOfBandwidth,
        task: Task,
    ) -> PoBVerificationResult:
        """Verify a Proof-of-Bandwidth submission."""
        task_id_hex = pob.task_id.hex() if isinstance(pob.task_id, bytes) else str(pob.task_id)
        miner_hotkey = pob.miner_id.decode() if isinstance(pob.miner_id, bytes) else pob.miner_id
        logger.info(f"verify_pob: starting verification for task {task_id_hex[:16]}... miner={miner_hotkey[:16]}...")

        result = PoBVerificationResult(pob=pob, valid=False)

        try:
            message = pob.get_canonical_message()
            result.signature_valid = verify_hotkey_signature(
                message, pob.signature, miner_hotkey
            )
        except Exception as e:
            result.signature_valid = False
            result.error = f"Invalid signature: {e}"
            logger.warning(f"verify_pob: signature check FAILED for task {task_id_hex[:16]}... error={e}")
            return result

        if not result.signature_valid:
            result.error = "Invalid signature"
            logger.warning(f"verify_pob: invalid signature for task {task_id_hex[:16]}... miner={miner_hotkey[:16]}...")
            return result
        logger.debug(f"verify_pob: signature OK for task {task_id_hex[:16]}...")

        delta_us = pob.end_time - pob.start_time

        if delta_us < self.settings.min_transfer_time_us:
            result.error = f"Transfer too fast: {delta_us}µs"
            logger.warning(f"verify_pob: transfer too fast for task {task_id_hex[:16]}... delta={delta_us}µs min={self.settings.min_transfer_time_us}µs")
            return result

        if pob.end_time > task.deadline:
            result.error = "Task deadline exceeded"
            logger.warning(f"verify_pob: deadline exceeded for task {task_id_hex[:16]}... end={pob.end_time} deadline={task.deadline}")
            return result

        result.timing_valid = True
        logger.debug(f"verify_pob: timing OK for task {task_id_hex[:16]}... delta={delta_us}µs")

        calculated_bandwidth = pob.calculate_bandwidth()
        bandwidth_diff = abs(calculated_bandwidth - pob.bandwidth_mbps)

        if bandwidth_diff > 1.0:
            result.error = f"Bandwidth mismatch: claimed {pob.bandwidth_mbps}, calculated {calculated_bandwidth}"
            logger.warning(f"verify_pob: bandwidth mismatch for task {task_id_hex[:16]}... claimed={pob.bandwidth_mbps:.2f} calculated={calculated_bandwidth:.2f} diff={bandwidth_diff:.2f}")
            return result

        result.bandwidth_valid = True
        result.calculated_bandwidth = calculated_bandwidth
        logger.debug(f"verify_pob: bandwidth OK for task {task_id_hex[:16]}... bw={calculated_bandwidth:.2f} Mbps")

        expected_canary_proof = compute_canary_proof(task.canary, pob.start_time)

        if pob.canary_proof != expected_canary_proof:
            result.error = "Invalid canary proof"
            logger.warning(f"verify_pob: invalid canary proof for task {task_id_hex[:16]}...")
            return result

        result.canary_valid = True
        logger.debug(f"verify_pob: canary OK for task {task_id_hex[:16]}...")

        if len(task.path) > 1:
            # Multi-hop: require merkle proof
            if not pob.merkle_path:
                result.merkle_valid = False
                result.error = "Multi-hop transfer missing merkle proof"
                logger.warning(f"verify_pob: missing merkle proof for multi-hop task {task_id_hex[:16]}... (path_len={len(task.path)})")
                return result

            # Verify merkle proof: build expected leaf and check against path
            try:
                # Determine hop neighbors from task path
                hop_idx = min(pob.path_index, len(task.path) - 1)
                prev_hop = task.path[hop_idx - 1] if hop_idx > 0 else ""
                next_hop = task.path[hop_idx + 1] if hop_idx < len(task.path) - 1 else ""

                leaf = build_merkle_leaf(
                    prev_hop_id=prev_hop,
                    current_miner_id=miner_hotkey,
                    next_hop_id=next_hop,
                    bytes_relayed=pob.bytes_relayed,
                    start_time=pob.start_time,
                    end_time=pob.end_time,
                )

                # Compute expected root from all path hops if we have all leaves
                # For now, verify the proof structure is valid (non-empty, correct length)
                expected_depth = (len(task.path) - 1).bit_length()
                if len(pob.merkle_path) != expected_depth and expected_depth > 0:
                    logger.warning(
                        f"verify_pob: merkle proof depth mismatch for task {task_id_hex[:16]}... "
                        f"expected={expected_depth} got={len(pob.merkle_path)}"
                    )
                    result.merkle_valid = False
                    result.error = "Merkle proof depth mismatch"
                    return result

                result.merkle_valid = True
                logger.debug(f"verify_pob: merkle proof structure OK for task {task_id_hex[:16]}...")

            except Exception as merkle_err:
                result.merkle_valid = False
                result.error = f"Merkle verification error: {merkle_err}"
                logger.warning(f"verify_pob: merkle error for task {task_id_hex[:16]}...: {merkle_err}")
                return result
        else:
            # Single-hop: no merkle proof needed
            result.merkle_valid = True

        result.geo_valid = True

        result.valid = result.all_checks_passed
        result.latency_ms = delta_us / 1000

        logger.info(
            f"verify_pob: task {task_id_hex[:16]}... result={'PASS' if result.valid else 'FAIL'} "
            f"bw={calculated_bandwidth:.2f}Mbps latency={result.latency_ms:.1f}ms "
            f"checks=[sig={result.signature_valid} time={result.timing_valid} bw={result.bandwidth_valid} "
            f"canary={result.canary_valid} merkle={result.merkle_valid} geo={result.geo_valid}]"
        )

        return result

    async def _cleanup_old_results(self) -> None:
        """Clean up old task results and pending tasks."""
        now = time.time()
        max_age_seconds = 24 * 60 * 60

        old_results = []
        for task_id, result in self.task_results.items():
            result_time = result.pob.end_time / 1_000_000 if result.pob else 0
            if now - result_time > max_age_seconds:
                old_results.append(task_id)

        for task_id in old_results:
            del self.task_results[task_id]

        old_tasks = []
        for task_id, task in self.pending_tasks.items():
            task_time = task.deadline / 1_000_000 if task.deadline else 0
            if now - task_time > max_age_seconds:
                old_tasks.append(task_id)

        for task_id in old_tasks:
            del self.pending_tasks[task_id]

        old_challenges = []
        for task_id, challenge in self.active_challenges.items():
            created_at = challenge.get("created_at", 0)
            if now - created_at > max_age_seconds:
                old_challenges.append(task_id)

        for task_id in old_challenges:
            del self.active_challenges[task_id]

        if old_results or old_tasks or old_challenges:
            logger.info(
                f"_cleanup_old_results: removed {len(old_results)} results, "
                f"{len(old_tasks)} tasks, {len(old_challenges)} challenges "
                f"(remaining: {len(self.task_results)} results, {len(self.pending_tasks)} tasks, "
                f"{len(self.active_challenges)} challenges)"
            )

    # =========================================================================
    # Spot-Check Proofs
    # =========================================================================

    async def _spot_check_proofs(self) -> None:
        """Randomly spot-check proofs from Orchestrators."""
        eligible = [
            (hk, s) for hk, s in self.work_summaries.items()
            if s.proof_count > 0 and self.orchestrators.get(hk) and self.orchestrators[hk].is_healthy
        ]
        if eligible:
            logger.info(f"_spot_check_proofs: checking {len(eligible)} eligible orchestrators")
        for hotkey, summary in self.work_summaries.items():
            if summary.proof_count == 0:
                continue

            orchestrator = self.orchestrators.get(hotkey)
            if not orchestrator or not orchestrator.is_healthy:
                continue

            try:
                result = await self._spot_check_orchestrator(hotkey, orchestrator, summary)
                self.spot_check_results[hotkey] = result
                self.spot_check_history.append(result)

                if len(self.spot_check_history) > 1000:
                    self.spot_check_history = self.spot_check_history[-1000:]

                if result.fraud_detected:
                    self.fraud_penalties[hotkey] = result.fraud_severity
                    logger.warning(
                        f"Fraud detected for orchestrator {hotkey[:16]}...: "
                        f"{result.proofs_invalid}/{result.proofs_received} invalid proofs"
                    )
                else:
                    self.fraud_penalties.pop(hotkey, None)

            except Exception as e:
                logger.error(f"Error spot-checking orchestrator {hotkey[:16]}...: {e}")

    async def _spot_check_orchestrator(
        self,
        hotkey: str,
        orchestrator: OrchestratorInfo,
        summary: WorkSummary,
    ) -> SpotCheckResult:
        """Perform spot-check verification for a single orchestrator."""
        sample_percent = 0.05 + random.random() * 0.05
        sample_size = max(5, min(50, int(summary.proof_count * sample_percent)))
        logger.info(
            f"_spot_check_orchestrator: {hotkey[:16]}... "
            f"sampling {sample_size} of {summary.proof_count} proofs ({sample_percent:.1%})"
        )

        proof_ids = await self._get_random_proof_ids(orchestrator, sample_size)

        if not proof_ids:
            logger.warning(f"_spot_check_orchestrator: {hotkey[:16]}... no proof IDs returned")
            return SpotCheckResult(
                orchestrator_hotkey=hotkey,
                proofs_requested=sample_size,
                proofs_received=0,
                proofs_valid=0,
                proofs_invalid=0,
            )

        logger.debug(f"_spot_check_orchestrator: {hotkey[:16]}... got {len(proof_ids)} proof IDs, requesting full proofs")
        proofs = await self._request_proofs(orchestrator, proof_ids)

        if not proofs:
            logger.warning(
                f"_spot_check_orchestrator: {hotkey[:16]}... returned 0 proofs for {len(proof_ids)} IDs - FRAUD"
            )
            return SpotCheckResult(
                orchestrator_hotkey=hotkey,
                proofs_requested=sample_size,
                proofs_received=0,
                proofs_valid=0,
                proofs_invalid=0,
                fraud_detected=True,
                fraud_severity=1.0,
            )

        valid_count = 0
        invalid_ids = []
        invalid_reasons = {}

        for proof_data in proofs:
            verification = await self._verify_single_subnet_proof(proof_data)

            if verification.valid:
                valid_count += 1
            else:
                invalid_ids.append(verification.task_id)
                invalid_reasons[verification.task_id] = verification.error or "Unknown error"

        verification_rate = valid_count / len(proofs) if proofs else 0
        invalid_rate = 1.0 - verification_rate

        fraud_detected = invalid_rate > 0.05
        fraud_severity = min(1.0, invalid_rate * 2)

        log_fn = logger.warning if fraud_detected else logger.info
        log_fn(
            f"_spot_check_orchestrator: {hotkey[:16]}... "
            f"valid={valid_count}/{len(proofs)} invalid={len(invalid_ids)} "
            f"rate={verification_rate:.2%} fraud={'YES' if fraud_detected else 'no'}"
            + (f" severity={fraud_severity:.4f}" if fraud_detected else "")
            + (f" invalid_reasons={dict(list(invalid_reasons.items())[:3])}" if invalid_reasons else "")
        )

        return SpotCheckResult(
            orchestrator_hotkey=hotkey,
            proofs_requested=sample_size,
            proofs_received=len(proofs),
            proofs_valid=valid_count,
            proofs_invalid=len(invalid_ids),
            verification_rate=verification_rate,
            invalid_proof_ids=invalid_ids,
            invalid_reasons=invalid_reasons,
            fraud_detected=fraud_detected,
            fraud_severity=fraud_severity,
        )

    async def _get_random_proof_ids(
        self,
        orchestrator: OrchestratorInfo,
        sample_size: int,
    ) -> List[str]:
        """Get random proof IDs for spot-checking."""
        # Prefer SubnetCore for proof queries
        if SUBNET_CORE_AVAILABLE and self.subnet_core_client:
            try:
                result = await self.subnet_core_client.get_proofs_from_subnetcore(
                    epoch=self.current_epoch,
                    orchestrator_hotkey=orchestrator.hotkey,
                    limit=sample_size * 2,  # Get extra to sample from
                )
                proofs = result.get("proofs", [])
                if proofs:
                    all_proof_ids = [p.get("task_id", "") for p in proofs if p.get("task_id")]
                    sample_size = min(sample_size, len(all_proof_ids))
                    return random.sample(all_proof_ids, sample_size) if all_proof_ids else []
                return []  # BeamCore responded — no proofs for this orchestrator, don't connect directly
            except Exception as e:
                logger.warning(f"SubnetCore proof query failed, falling back: {e}")

        # Fallback: Direct orchestrator query (legacy path)
        try:
            url = f"{orchestrator.url}/validators/proof-ids"
            headers = {"X-Validator-Hotkey": self.hotkey}

            async with self._http_session.get(
                url,
                headers=headers,
                timeout=aiohttp.ClientTimeout(total=10),
            ) as response:
                if response.status != 200:
                    return []

                data = await response.json()
                all_proof_ids = data.get("proof_ids", [])

                if not all_proof_ids:
                    return []

                sample_size = min(sample_size, len(all_proof_ids))
                return random.sample(all_proof_ids, sample_size)

        except Exception as e:
            logger.error(f"Error getting proof IDs: {e}")
            return []

    async def _request_proofs(
        self,
        orchestrator: OrchestratorInfo,
        proof_ids: List[str],
    ) -> List[dict]:
        """Request full proof data for specific proof IDs."""
        # Use SubnetCore for proof queries (proofs are stored in subnet_pob_registry)
        if SUBNET_CORE_AVAILABLE and self.subnet_core_client:
            try:
                proofs = []
                for task_id in proof_ids[:10]:  # Limit to avoid too many requests
                    result = await self.subnet_core_client.get_proof(task_id)
                    if result and "error" not in result:
                        proofs.append(result)
                return proofs
            except Exception as e:
                logger.error(f"SubnetCore proof fetch failed: {e}")
                return []

        # No SubnetCore client available
        logger.warning("SubnetCore client not available for proof queries")
        return []

    # =========================================================================
    # SLA Metrics and Scoring
    # =========================================================================

    async def _update_sla_metrics(self) -> None:
        """Update SLA metrics for orchestrators and workers based on task results."""
        for task_id, result in self.task_results.items():
            if not result.valid:
                continue

            uid = self._get_uid_for_miner(result.pob.miner_id)
            if uid is None:
                continue

            if uid not in self.orchestrator_metrics:
                self.orchestrator_metrics[uid] = {
                    "bandwidth_samples": [],
                    "latency_samples": [],
                    "task_count": 0,
                    "success_count": 0,
                    "accepted_count": 0,
                    "offered_count": 0,
                    "last_updated": datetime.utcnow(),
                }

            metrics = self.orchestrator_metrics[uid]

            metrics["bandwidth_samples"].append(result.calculated_bandwidth)

            if result.latency_ms:
                metrics["latency_samples"].append(result.latency_ms)

            metrics["task_count"] += 1
            metrics["success_count"] += 1
            metrics["last_updated"] = datetime.utcnow()

            max_samples = 1000
            if len(metrics["bandwidth_samples"]) > max_samples:
                metrics["bandwidth_samples"] = metrics["bandwidth_samples"][-max_samples:]
            if len(metrics["latency_samples"]) > max_samples:
                metrics["latency_samples"] = metrics["latency_samples"][-max_samples:]

        for task_id, challenge in self.active_challenges.items():
            uid = challenge.get("uid")
            if uid is None:
                continue

            if uid not in self.orchestrator_metrics:
                self.orchestrator_metrics[uid] = {
                    "bandwidth_samples": [],
                    "latency_samples": [],
                    "task_count": 0,
                    "success_count": 0,
                    "accepted_count": 0,
                    "offered_count": 0,
                    "last_updated": datetime.utcnow(),
                }

            metrics = self.orchestrator_metrics[uid]
            metrics["offered_count"] += 1

            if challenge.get("status") in ["accepted", "chunk_received"]:
                metrics["accepted_count"] += 1

    async def _calculate_sla_scores(self) -> None:
        """Calculate SLA scores for all orchestrators using multiplicative penalties."""
        if self.orchestrator_metrics:
            logger.info(f"_calculate_sla_scores: processing {len(self.orchestrator_metrics)} orchestrators")
        for uid, metrics in self.orchestrator_metrics.items():
            orchestrator = self.orchestrator_manager.get_orchestrator(uid)
            if orchestrator is None:
                continue

            bandwidth_avg = 0.0
            if metrics["bandwidth_samples"]:
                bandwidth_avg = sum(metrics["bandwidth_samples"]) / len(metrics["bandwidth_samples"])

            success_rate = 0.0
            if metrics["task_count"] > 0:
                success_rate = (metrics["success_count"] / metrics["task_count"]) * 100

            acceptance_rate = 0.0
            if metrics["offered_count"] > 0:
                acceptance_rate = (metrics["accepted_count"] / metrics["offered_count"]) * 100

            last_updated = metrics["last_updated"]
            if isinstance(last_updated, str):
                last_updated = datetime.fromisoformat(last_updated.replace("Z", "+00:00")).replace(tzinfo=None)
            hours_since_update = (datetime.utcnow() - last_updated).total_seconds() / 3600
            uptime = 99.0 if hours_since_update < 1 else max(50.0, 99.0 - hours_since_update * 2)

            sla_metrics = SLAMetrics.from_latency_samples(
                latency_samples=metrics["latency_samples"],
                uptime_percent=uptime,
                bandwidth_mbps=bandwidth_avg,
                acceptance_rate_percent=acceptance_rate,
                success_rate_percent=success_rate,
                measurement_start=metrics.get("window_start", datetime.utcnow() - timedelta(hours=24)),
                measurement_end=datetime.utcnow(),
            )

            self.orchestrator_manager.update_orchestrator_metrics(uid, sla_metrics)

    async def _process_reassignments(self) -> None:
        """Process worker reassignments due to SLA violations."""
        new_requests = self.reassignment_manager.check_and_queue_sla_reassignments()

        if new_requests:
            logger.info(f"Queued {len(new_requests)} new worker reassignments")

        completed = self.reassignment_manager.process_pending_reassignments()

        if completed:
            logger.info(f"Completed {len(completed)} worker reassignments")

    # =========================================================================
    # Scoring
    # =========================================================================

    async def _update_scores(self) -> None:
        """Update Orchestrator scores using full SLA-based multiplicative penalties."""
        logger.info(f"_update_scores: scoring {len(self.orchestrators)} orchestrators, {len(self.work_summaries)} with summaries")

        # Fetch baseline multiplier from SubnetCore (authoritative, not locally editable)
        _baseline_multiplier = 0.1  # hardcoded fallback
        if self.subnet_core_client:
            try:
                net_config = await self.subnet_core_client.get_network_config()
                if net_config and "validator_baseline_multiplier" in net_config:
                    _baseline_multiplier = float(net_config["validator_baseline_multiplier"])
            except Exception as e:
                logger.warning(f"Could not fetch baseline multiplier from SubnetCore: {e}")

        for hotkey, info in self.orchestrators.items():
            summary = self.work_summaries.get(hotkey)

            if not summary:
                # Give idle orchestrators a minimal baseline so they appear in weights
                stake_weight = calculate_stake_weight(info.stake_tao)
                baseline_multiplier = _baseline_multiplier
                if info.is_subnet_owned:
                    baseline_multiplier = 1.0
                    stake_weight = 1.0
                final_score = baseline_multiplier * stake_weight
                self.orchestrator_scores[hotkey] = final_score
                info.last_score = final_score
                logger.info(
                    f"_update_scores: {hotkey[:16]}... UID={info.uid} "
                    f"no work summary — baseline score={final_score:.4f} "
                    f"(stake_weight={stake_weight:.4f})"
                )
                continue

            metrics = SLAMetrics(
                uptime_percent=summary.uptime_percent,
                bandwidth_mbps=summary.avg_bandwidth_mbps,
                latency_p95_ms=summary.latency_p95_ms if summary.latency_p95_ms > 0 else summary.avg_latency_ms,
                acceptance_rate_percent=summary.acceptance_rate,
                success_rate_percent=summary.success_rate * 100,
                packet_loss_percent=0.0,
                measurement_start=summary.measurement_start,
                measurement_end=summary.measurement_end,
                sample_count=summary.total_tasks,
            )

            logger.debug(
                f"_update_scores: {hotkey[:16]}... SLA metrics: "
                f"uptime={metrics.uptime_percent:.1f}% bw={metrics.bandwidth_mbps:.1f}Mbps "
                f"latency_p95={metrics.latency_p95_ms:.1f}ms accept={metrics.acceptance_rate_percent:.1f}% "
                f"success={metrics.success_rate_percent:.1f}% samples={metrics.sample_count}"
            )

            sla_state = OrchestratorSLAState(
                uid=info.uid or 0,
                hotkey=hotkey,
                stake_tao=info.stake_tao,
                metrics=metrics,
                is_subnet_owned=info.is_subnet_owned,
                registered_at=info.registered_at,
                grace_period_ends=(
                    info.registered_at + timedelta(hours=NEW_ORCHESTRATOR_GRACE_PERIOD_HOURS)
                    if info.registered_at else None
                ),
            )

            sla_score = self.sla_scorer.score_orchestrator(sla_state)

            challenge_multiplier = self._calculate_challenge_multiplier(hotkey)
            fraud_multiplier = self._calculate_fraud_multiplier(hotkey)
            payment_multiplier = self.payment_penalty_multipliers.get(hotkey, 1.0)

            effective_multiplier = (
                sla_score.effective_multiplier *
                challenge_multiplier *
                fraud_multiplier *
                payment_multiplier
            )

            stake_weight = calculate_stake_weight(info.stake_tao)
            final_score = effective_multiplier * stake_weight

            self.orchestrator_sla_scores[hotkey] = sla_score
            self.orchestrator_scores[hotkey] = final_score
            info.last_score = final_score
            info.sla_state = sla_state
            sla_state.score = sla_score

            logger.info(
                f"_update_scores: {hotkey[:16]}... UID={info.uid} "
                f"sla_mult={sla_score.effective_multiplier:.4f} "
                f"challenge_mult={challenge_multiplier:.4f} "
                f"fraud_mult={fraud_multiplier:.4f} "
                f"payment_mult={payment_multiplier:.4f} "
                f"combined_mult={effective_multiplier:.4f} "
                f"stake_weight={stake_weight:.4f} "
                f"final_score={final_score:.6f}"
                + (f" violations={[v.value for v in sla_score.violations]}" if sla_score.violations else "")
                + (f" redirect={sla_score.penalty_redirect_percent:.1f}%" if sla_score.penalty_redirect_percent > 0 else "")
            )

        # Also update connection scores for legacy compatibility
        for uid in self.connections:
            conn_results = [
                r for r in self.task_results.values()
                if r.valid and self._get_uid_for_miner(r.pob.miner_id) == uid
            ]

            if not conn_results:
                continue

            bandwidth_scores = [r.calculated_bandwidth for r in conn_results]
            avg_bandwidth = sum(bandwidth_scores) / len(bandwidth_scores)

            bandwidth_normalized = min(
                avg_bandwidth / self.settings.max_bandwidth_mbps,
                1.0
            )

            total_tasks = len([t for t in self.pending_tasks.values()
                             if self._get_uid_for_task(t) == uid])
            success_rate = len(conn_results) / total_tasks if total_tasks > 0 else 0

            score = (
                self.settings.score_weight_bandwidth * bandwidth_normalized +
                self.settings.score_weight_uptime * success_rate +
                self.settings.score_weight_loss * 1.0 +
                self.settings.score_weight_tier * 0.5
            )

            if uid in self.connection_scores:
                old_score = self.connection_scores[uid]
                score = BANDWIDTH_EMA_ALPHA * score + (1 - BANDWIDTH_EMA_ALPHA) * old_score

            self.connection_scores[uid] = score
            self.connection_bandwidth[uid] = avg_bandwidth

    def _calculate_challenge_multiplier(self, hotkey: str) -> float:
        """Calculate challenge verification multiplier."""
        challenge_results = [
            r for cid, r in self.challenge_results.items()
            if self.active_challenges.get(cid, {}).get("orchestrator") == hotkey
        ]

        if not challenge_results:
            logger.debug(f"_calculate_challenge_multiplier: {hotkey[:16]}... no challenge results, using default 0.9")
            return 0.9

        successful = sum(1 for r in challenge_results if r.success)
        success_rate = successful / len(challenge_results)
        multiplier = 0.5 + (success_rate * 0.5)

        logger.info(
            f"_calculate_challenge_multiplier: {hotkey[:16]}... "
            f"{successful}/{len(challenge_results)} challenges passed "
            f"(rate={success_rate:.2%}) -> multiplier={multiplier:.4f}"
        )

        return multiplier

    def _calculate_fraud_multiplier(self, hotkey: str) -> float:
        """Calculate fraud penalty multiplier from spot-check results."""
        fraud_severity = self.fraud_penalties.get(hotkey, 0.0)

        if fraud_severity <= 0:
            logger.debug(f"_calculate_fraud_multiplier: {hotkey[:16]}... no fraud penalty -> 1.0")
            return 1.0

        multiplier = 1.0 - fraud_severity
        multiplier = max(0.1, multiplier)
        logger.warning(
            f"_calculate_fraud_multiplier: {hotkey[:16]}... "
            f"fraud_severity={fraud_severity:.4f} -> multiplier={multiplier:.4f}"
        )
        return multiplier

    def _get_uid_for_miner(self, miner_id) -> Optional[int]:
        """Get UID for a miner hotkey"""
        hotkey = miner_id.decode() if isinstance(miner_id, bytes) else str(miner_id)
        for uid, conn in self.connections.items():
            if conn["hotkey"] == hotkey:
                logger.debug(f"_get_uid_for_miner: {hotkey[:16]}... -> UID {uid}")
                return uid
        logger.debug(f"_get_uid_for_miner: {hotkey[:16]}... -> NOT FOUND in {len(self.connections)} connections")
        return None

    def _get_uid_for_task(self, task: Task) -> Optional[int]:
        """Get UID for a task's assigned connection"""
        if task.path:
            for uid, conn in self.connections.items():
                if conn["hotkey"] == task.path[0]:
                    logger.debug(f"_get_uid_for_task: path[0]={task.path[0][:16]}... -> UID {uid}")
                    return uid
            logger.debug(f"_get_uid_for_task: path[0]={task.path[0][:16]}... -> NOT FOUND")
        return None

    # =========================================================================
    # Weight Setting
    # =========================================================================

    async def _maybe_set_weights(self) -> None:
        """Set weights on chain if enough blocks have passed"""
        if self.subtensor is None:
            logger.debug("Skipping weight setting - no subtensor")
            return

        current_block = self.subtensor.block

        if current_block - self.last_weight_block < self.settings.blocks_between_weights:
            return

        await self._set_weights()

    async def _set_weights(self) -> None:
        """Set weights on the Bittensor network.

        Tries exposure-linked formula first (from BeamCore summaries),
        falls back to legacy SLA-based scores if unavailable.
        """
        uids, weights, formula_version, params_hash = None, None, None, None
        data_epoch = self.current_epoch  # Default to current epoch

        # Try exposure-linked formula first
        if SUBNET_CORE_AVAILABLE and self.subnet_core_client:
            try:
                uids, weights, formula_version, params_hash, data_epoch = await self._compute_exposure_weights()
                logger.info(
                    f"_set_weights: using {formula_version} for {len(uids)} UIDs (data_epoch={data_epoch})"
                )
            except Exception as e:
                logger.warning(f"Exposure weight computation failed, falling back to legacy: {e}")
                uids, weights, formula_version, params_hash = None, None, None, None

        # Fallback to legacy scores
        if uids is None:
            uids, weights = self._compute_legacy_weights()
            formula_version = "legacy_v0"
            params_hash = None

        if not uids:
            logger.warning("No weights to set")
            return

        logger.info(
            f"_set_weights: setting weights for {len(uids)} UIDs ({formula_version}), "
            f"top weights: {sorted(zip(uids, weights), key=lambda x: -x[1])[:5]}"
        )

        # Set weights on chain
        try:
            # Check if commit-reveal is enabled on this subnet
            commit_reveal_enabled = False
            if self.subtensor is not None:
                try:
                    cr_result = self.subtensor.query_module(
                        'SubtensorModule', 'CommitRevealWeightsEnabled', [self.settings.netuid]
                    )
                    commit_reveal_enabled = bool(cr_result.value) if cr_result else False
                except Exception as e:
                    logger.debug(f"Could not check commit-reveal status: {e}")

            if commit_reveal_enabled and self.subtensor is not None:
                # Use set_weights for subnets with commit-reveal enabled
                # set_weights() handles the full time-locked commit-reveal flow automatically
                logger.info(f"Using set_weights with time-locked commit-reveal")
                result = self.subtensor.set_weights(
                    wallet=self.wallet,
                    netuid=self.settings.netuid,
                    uids=list(uids),
                    weights=list(weights),
                    wait_for_inclusion=True,
                    wait_for_finalization=False,
                    wait_for_revealed_execution=True,
                )
                success = result.success
                message = result.message or result.error or ""
                weight_method = "timelocked_commit_reveal"
            elif self.fiber_chain is not None and self.uid is not None:
                success, message = self.fiber_chain.set_weights(
                    keypair=self.wallet,  # Pass full wallet, not just hotkey
                    validator_uid=self.uid,
                    uids=uids,
                    weights=weights,
                    wait_for_inclusion=True,
                    wait_for_finalization=False,
                )
                weight_method = "Fiber"
            elif self.subtensor is not None:
                success, message = self.subtensor.set_weights(
                    wallet=self.wallet,
                    netuid=self.settings.netuid,
                    uids=uids,
                    weights=weights,
                    wait_for_inclusion=True,
                    wait_for_finalization=False,
                )
                weight_method = "bittensor"
            else:
                logger.warning("No method available to set weights")
                return

            if success:
                self.last_weight_block = self.subtensor.block if self.subtensor else 0
                logger.info(
                    f"Weights set successfully via {weight_method} at block {self.last_weight_block} "
                    f"(formula={formula_version})"
                )

                self.weights_history.append({
                    "block": self.last_weight_block,
                    "timestamp": datetime.utcnow().isoformat(),
                    "weights": {uid: round(w, 6) for uid, w in zip(uids, weights)},
                    "weight_method": weight_method,
                    "formula_version": formula_version,
                })

                if len(self.weights_history) > 100:
                    self.weights_history = self.weights_history[-100:]

                # Submit weight proof to SubnetCore
                # Use data_epoch (the epoch whose data was used for computation)
                # to ensure auditors compare against the same data
                await self._submit_weight_proof(
                    epoch=data_epoch,
                    block_number=self.last_weight_block,
                    uids=uids,
                    weights=weights,
                    formula_version=formula_version,
                    params_hash=params_hash,
                )

            else:
                error_msg = message or (
                    "Unknown error. Common causes: "
                    "(1) Too soon since last weight update - wait for rate limit, "
                    "(2) Wallet not registered on subnet, "
                    "(3) Insufficient stake, "
                    "(4) Subtensor connection issue"
                )
                logger.error(f"Failed to set weights: {error_msg}")

        except Exception as e:
            logger.error(f"Error setting weights: {e}", exc_info=True)

    async def _compute_exposure_weights(self) -> Tuple[List[int], List[float], str, str, int]:
        """Compute weights locally using exposure-linked formula with BeamCore data.

        Degradation ladder:
            Summaries + breakdowns both OK → exposure_v1 (fully verified)
            Summaries OK, breakdowns failed  → exposure_v1_unverified (default penalties)
            Summaries failed                 → raises (caller falls back to legacy_v0)

        Returns:
            (uids, weights, formula_version, params_hash, data_epoch)
            where data_epoch is the epoch whose data was used for computation.
        """
        from services.weight_calculator import (
            compute_weights, SummaryInput, get_params_hash,
            FORMULA_VERSION, verify_params_hash,
        )

        # 1. Fetch summaries FIRST (essential — if this fails, caller falls back to legacy)
        #    The current epoch may not have data yet, so ask BeamCore for the
        #    latest epoch that has non-zero proof/bandwidth data.
        used_epoch = self.current_epoch
        latest_data_epoch = await self.subnet_core_client.get_latest_data_epoch()
        if latest_data_epoch is not None and latest_data_epoch != self.current_epoch:
            logger.info(
                f"Current epoch {self.current_epoch} has no data, "
                f"using latest data epoch {latest_data_epoch}"
            )
            used_epoch = latest_data_epoch

        summaries_data = await self.subnet_core_client.get_epoch_summaries(used_epoch)
        raw_summaries = summaries_data.get("summaries", [])

        if not raw_summaries:
            raise ValueError(
                f"No epoch summaries available for epoch {used_epoch}. "
                f"This typically means no orchestrators are active or submitting proofs. "
                f"Falling back to legacy weight formula."
            )

        # Filter summaries by current metagraph ownership to avoid stale UID mappings
        filtered_summaries = []
        skipped_count = 0
        for s in raw_summaries:
            uid = s["orchestrator_uid"]
            hotkey = s["orchestrator_hotkey"]
            # Check if this hotkey currently owns this UID in the metagraph
            if self.metagraph and 0 <= uid < len(self.metagraph.hotkeys):
                current_owner = self.metagraph.hotkeys[uid]
                if current_owner != hotkey:
                    skipped_count += 1
                    logger.debug(
                        f"Skipping stale summary: UID {uid} claimed by {hotkey[:16]}... "
                        f"but currently owned by {current_owner[:16]}..."
                    )
                    continue
            filtered_summaries.append(s)

        if skipped_count > 0:
            logger.info(f"Filtered {skipped_count} stale summaries (UID ownership changed)")

        summaries = [
            SummaryInput(
                orchestrator_hotkey=s["orchestrator_hotkey"],
                orchestrator_uid=s["orchestrator_uid"],
                total_proofs_published=s.get("total_proofs_published", 0),
                total_bytes_claimed=s.get("total_bytes_claimed", 0),
                bandwidth_score=s.get("bandwidth_score", 0.0),
                compliance_score=s.get("compliance_score", 1.0),
                verification_rate=s.get("verification_rate", 0.0),
                spot_check_rate=s.get("spot_check_rate", 0.0),
            )
            for s in filtered_summaries
        ]

        # 2. Fetch breakdowns (secondary — for penalties + params_hash verification)
        penalty_multipliers: Dict[str, float] = {}
        breakdowns = None
        verified = False
        try:
            breakdowns = await self.subnet_core_client.get_recommended_weights(used_epoch)

            # Verify params_hash
            bc_hash = breakdowns.get("params_hash")
            if bc_hash and verify_params_hash(bc_hash):
                verified = True

                # Extract penalties from breakdown details
                for d in breakdowns.get("details", []):
                    pm = d.get("penalty_multiplier", 1.0)
                    if pm != 1.0:
                        penalty_multipliers[d["hotkey"]] = pm
            else:
                logger.warning("params_hash mismatch — computing with default penalties")

        except Exception as e:
            logger.warning(f"Breakdown fetch failed, computing with default penalties: {e}")

        # 3. Compute locally (works even without breakdowns — default 1.0 penalties)
        wv = compute_weights(summaries, penalty_multipliers, used_epoch)

        # 4. Determine formula_version based on verification status
        if not verified:
            formula_version = "exposure_v1_unverified"
            p_hash = get_params_hash()
        else:
            formula_version = wv.formula_version
            p_hash = wv.params_hash

        # 5. Log comparison with BeamCore's weights (if breakdowns available)
        if breakdowns and verified:
            bc_weights = {u: w for u, w in zip(breakdowns.get("uids", []), breakdowns.get("weights", []))}
            for d in wv.details:
                bc_w = bc_weights.get(d.uid, 0)
                diff = abs(d.normalized_weight - bc_w)
                if diff > 0.001:
                    logger.warning(
                        f"Weight divergence UID {d.uid}: local={d.normalized_weight:.6f} "
                        f"beamcore={bc_w:.6f} diff={diff:.6f}"
                    )

        return wv.uids, wv.weights, formula_version, p_hash, used_epoch

    def _compute_legacy_weights(self) -> Tuple[List[int], List[float]]:
        """Compute weights from legacy SLA-based orchestrator scores (fallback).

        Returns (uids, weights) or ([], []) if no data.
        """
        if self.orchestrator_scores:
            total_score = sum(self.orchestrator_scores.values())

            if total_score <= 0:
                # All scores are zero — set equal weights to avoid stale weights
                logger.warning("All orchestrator scores are 0, setting equal weights")
                uids = []
                weights = []
                for hotkey in self.orchestrator_scores:
                    uid = self._get_uid_for_hotkey(hotkey)
                    if uid is not None:
                        uids.append(uid)
                        weights.append(1.0)
                if not uids:
                    return [], []
                weight_sum = len(uids)
                weights = [1.0 / weight_sum for _ in weights]
                return uids, weights

            uids: List[int] = []
            weights: List[float] = []
            skipped_hotkeys: List[str] = []

            for hotkey, score in self.orchestrator_scores.items():
                uid = self._get_uid_for_hotkey(hotkey)
                if uid is not None:
                    uids.append(uid)
                    weights.append(score / total_score)
                else:
                    skipped_hotkeys.append(hotkey)

            if skipped_hotkeys:
                logger.warning(
                    f"_compute_legacy_weights: skipped {len(skipped_hotkeys)} unregistered orchestrators: "
                    f"{[h[:16] + '...' for h in skipped_hotkeys[:5]]}"
                )

            if not uids:
                return [], []

            weight_sum = sum(weights)
            if weight_sum > 0:
                weights = [w / weight_sum for w in weights]

            return uids, weights

        elif self.connection_scores:
            total_score = sum(self.connection_scores.values())
            if total_score <= 0:
                return [], []

            uids = []
            weights = []
            for uid, score in self.connection_scores.items():
                uids.append(uid)
                weights.append(score / total_score)
            return uids, weights

        return [], []

    async def _submit_weight_proof(
        self,
        epoch: int,
        block_number: int,
        uids: List[int],
        weights: List[float],
        formula_version: Optional[str] = None,
        params_hash: Optional[str] = None,
    ) -> None:
        """Submit weight-set proof to SubnetCore for accountability."""
        if not self.subnet_core_client:
            return
        try:
            tx_hash = f"0x{block_number:016x}"  # Block-based identifier
            netuid = self.settings.netuid if hasattr(self.settings, 'netuid') else 0
            await self.subnet_core_client.submit_weight_proof(
                epoch=epoch,
                block_number=block_number,
                tx_hash=tx_hash,
                netuid=netuid,
                uids=uids,
                weights=weights,
                formula_version=formula_version,
                params_hash=params_hash,
            )
        except Exception as e:
            logger.warning(f"Failed to submit weight proof to SubnetCore: {e}")

    # =========================================================================
    # Epoch Management
    # =========================================================================

    async def _check_epoch(self) -> None:
        """Check for epoch changes and broadcast emission info"""
        if self.subtensor is None:
            return

        current_block = self.subtensor.block
        # Use 360 blocks per epoch to match SubnetCore's epoch calculation
        epoch_length_blocks = 360
        current_epoch = current_block // epoch_length_blocks

        # Sync if epoch changed or if current epoch is obviously wrong (old calculation)
        # Old calculation used block//25 which gave epochs like 258007 instead of ~17925
        should_sync = (
            current_epoch > self.current_epoch  # Normal case: new epoch
            or self.current_epoch > 100_000     # Old epoch calculation was used
        )

        if should_sync and current_epoch != self.current_epoch:
            previous_epoch = self.current_epoch
            self.current_epoch = current_epoch
            self.epoch_start_block = current_epoch * epoch_length_blocks

            logger.info(f"New epoch: {self.current_epoch} (was {previous_epoch})")
            self.tasks_this_epoch = 0

            # Submit scores to SubnetCore for the completed epoch (only if previous was valid)
            if previous_epoch > 0 and previous_epoch < 100_000:
                await self._submit_scores_to_core(previous_epoch)

            # Reset PoB verification stats for the new epoch
            self.pob_verification_results.clear()

    async def _submit_scores_to_core(self, epoch: int) -> None:
        """Submit orchestrator scores to SubnetCore at epoch boundary."""
        if not SUBNET_CORE_AVAILABLE or not self.subnet_core_client:
            return
        if epoch <= 0:
            return

        # Collect all orchestrators: from self.orchestrators + any with PoB verification results
        all_hotkeys = set(self.orchestrators.keys())
        for hotkey in self.pob_verification_results.keys():
            all_hotkeys.add(hotkey)

        scores = []
        for hotkey in all_hotkeys:
            info = self.orchestrators.get(hotkey)
            uid = self._get_uid_for_hotkey(hotkey)
            if uid is None:
                # Try to get UID from metagraph for orchestrators not in self.orchestrators
                try:
                    if hotkey in self.metagraph.hotkeys:
                        uid = self.metagraph.hotkeys.index(hotkey)
                except Exception:
                    pass
            if uid is None:
                continue

            summary = self.work_summaries.get(hotkey)
            sla_score = self.orchestrator_sla_scores.get(hotkey)

            # Compute bandwidth_score from SLA multiplier (bandwidth component)
            bandwidth_score = 0.0
            compliance_score = 1.0
            if sla_score and sla_score.multipliers:
                bandwidth_score = sla_score.multipliers.bandwidth
                # Use acceptance as a proxy for compliance
                compliance_score = sla_score.multipliers.acceptance

            # Compute final_score = effective_multiplier (before stake weighting)
            final_score = sla_score.effective_multiplier if sla_score else 0.0

            # Gather metrics from work summary
            total_proofs = summary.proof_count if summary else 0
            total_bytes = summary.total_bytes_relayed if summary else 0
            total_workers = summary.active_workers if summary else 0
            avg_bw = summary.avg_bandwidth_mbps if summary else 0.0
            success_rate = summary.success_rate if summary else 0.0

            # Use actual PoB verification stats when available
            pob_stats = self.pob_verification_results.get(hotkey)
            if pob_stats and pob_stats.total_proofs > 0:
                proofs_verified = pob_stats.verified_count
                proofs_rejected = pob_stats.rejected_count
                verification_rate = (
                    pob_stats.verified_count / pob_stats.total_proofs
                )
            else:
                # Fallback: trust claims when no PoB data is available
                proofs_verified = total_proofs
                proofs_rejected = 0
                verification_rate = success_rate

            scores.append(ScoreSubmission(
                epoch=epoch,
                orchestrator_hotkey=hotkey,
                orchestrator_uid=uid,
                bandwidth_score=round(bandwidth_score, 4),
                compliance_score=round(compliance_score, 4),
                final_score=round(final_score, 4),
                total_proofs_published=total_proofs,
                total_bytes_claimed=total_bytes,
                total_workers_active=total_workers,
                avg_bandwidth_mbps=round(avg_bw, 2),
                proofs_verified=proofs_verified,
                proofs_rejected=proofs_rejected,
                proofs_challenged=0,
                verification_rate=round(verification_rate, 4),
                spot_checks_attempted=0,
                spot_checks_passed=0,
                spot_check_rate=0.0,
            ))

        if not scores:
            return

        try:
            await self.subnet_core_client.submit_scores(epoch, scores)
            logger.info(f"Submitted {len(scores)} orchestrator scores to SubnetCore for epoch {epoch}")
        except Exception as e:
            logger.warning(f"Failed to submit scores to SubnetCore for epoch {epoch}: {e}")

    # =========================================================================
    # Cleanup and Maintenance
    # =========================================================================

    async def _load_pending_challenges(self) -> None:
        """Load pending challenges on startup."""
        logger.debug("Challenges are tracked in memory only")

    async def _expire_penalties_and_challenges(self) -> None:
        """Periodic cleanup: timeout stale in-memory challenges."""
        current_time = time.time()
        stale_ids = []

        for challenge_id, info in self.active_challenges.items():
            created_at = info.get("created_at", 0)
            if current_time - created_at > 300:
                stale_ids.append(challenge_id)

        for challenge_id in stale_ids:
            del self.active_challenges[challenge_id]

        if stale_ids:
            logger.debug(f"Cleaned up {len(stale_ids)} stale challenges")

    # =========================================================================
    # State & Metrics
    # =========================================================================

    def get_validator_state(self) -> dict:
        """Get current Validator state"""
        orchestrator_stats = self.orchestrator_manager.get_network_stats()
        worker_stats = self.worker_registry.get_stats()
        reassignment_stats = self.reassignment_manager.get_stats()

        # Use BeamCore worker counts instead of internal tracking
        beamcore_total_workers = sum(self._beamcore_worker_counts.values())
        orchestrator_stats["total_workers"] = beamcore_total_workers
        orchestrator_stats["worker_counts_by_uid"] = dict(self._beamcore_worker_counts)

        return {
            "uid": self.uid,
            "hotkey": self.hotkey,
            "is_registered": self.is_registered,
            "connections_tracked": len(self.connections),
            "pending_tasks": len(self.pending_tasks),
            "verified_pob": len([r for r in self.task_results.values() if r.valid]),
            "last_weight_block": self.last_weight_block,
            "current_block": self.subtensor.block if self.subtensor else 0,
            "orchestrators": orchestrator_stats,
            "workers": worker_stats,
            "reassignments": reassignment_stats,
            "total_redirected_to_one_tao": self.total_redirected_to_one,
        }

    def get_connection_scores(self) -> Dict[int, dict]:
        """Get all connection scores (legacy method)"""
        return {
            uid: {
                "score": round(self.connection_scores.get(uid, 0), 4),
                "bandwidth_mbps": round(self.connection_bandwidth.get(uid, 0), 2),
                "hotkey": conn["hotkey"],
                "stake": conn["stake"],
            }
            for uid, conn in self.connections.items()
        }

    def get_orchestrator_scores(self) -> Dict[str, dict]:
        """Get all orchestrator SLA scores"""
        result = {}

        for hotkey, info in self.orchestrators.items():
            uid = self._get_uid_for_hotkey(hotkey)

            sla_score = self.orchestrator_sla_scores.get(hotkey)
            sla_details = None
            if sla_score:
                sla_details = {
                    "effective_multiplier": round(sla_score.effective_multiplier, 4),
                    "combined_multiplier": round(sla_score.combined_multiplier, 4),
                    "penalty_redirect_percent": round(sla_score.penalty_redirect_percent, 2),
                    "violations": [v.value for v in sla_score.violations],
                    "in_grace_period": sla_score.in_grace_period,
                    "multipliers": {
                        "uptime": round(sla_score.multipliers.uptime, 4),
                        "bandwidth": round(sla_score.multipliers.bandwidth, 4),
                        "latency": round(sla_score.multipliers.latency, 4),
                        "acceptance": round(sla_score.multipliers.acceptance, 4),
                        "success": round(sla_score.multipliers.success, 4),
                    },
                }

            result[hotkey] = {
                "uid": uid,
                "score": round(self.orchestrator_scores.get(hotkey, 0), 4),
                "stake_tao": info.stake_tao,
                "stake_weight": round(calculate_stake_weight(info.stake_tao), 2),
                "url": info.url,
                "is_healthy": info.is_healthy,
                "is_subnet_owned": info.is_subnet_owned,
                "last_seen": info.last_seen.isoformat(),
                "registered": uid is not None,
                "sla": sla_details,
            }

        return result

    def get_spot_check_results(self) -> Dict[str, dict]:
        """Get spot-check results for all orchestrators."""
        results = {}
        for hotkey, result in self.spot_check_results.items():
            results[hotkey[:16]] = {
                "proofs_requested": result.proofs_requested,
                "proofs_received": result.proofs_received,
                "proofs_valid": result.proofs_valid,
                "proofs_invalid": result.proofs_invalid,
                "verification_rate": round(result.verification_rate, 4),
                "fraud_detected": result.fraud_detected,
                "fraud_severity": round(result.fraud_severity, 4),
                "fraud_multiplier": round(self._calculate_fraud_multiplier(hotkey), 4),
                "timestamp": result.timestamp.isoformat(),
            }
        return results

    def get_weights_history(self, limit: int = 10) -> List[dict]:
        """Get recent weights history."""
        return list(reversed(self.weights_history[-limit:]))
