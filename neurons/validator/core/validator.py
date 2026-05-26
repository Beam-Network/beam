"""
Validator Core Logic - Unified Version

Validates Proof-of-Bandwidth submissions and sets weights on the Bittensor network.
Supports both local mode (HTTP) and mainnet (Bittensor dendrite).

"""

import asyncio
import base64
import logging
import os
import random
import time
from dataclasses import dataclass, field
from datetime import datetime, timedelta
from typing import Dict, List, Optional, Tuple

import aiohttp
import bittensor as bt
from chain import FiberChain, FiberNode

# Import shared validator protocol/scoring helpers.
from core._beam_stubs import (
    BANDWIDTH_EMA_ALPHA,
    CANARY_SIZE_BYTES,
    # beam.constants
    SUBNET_ORCHESTRATOR_UID,
    # beam.protocol.synapse
    BandwidthChallenge,
    ChunkTransfer,
    OrchestratorManager,
    PoBVerificationResult,
    # beam.protocol.pob
    ProofOfBandwidth,
    ReassignmentManager,
    # Scoring helpers
    Task,
    WorkerRegistry,
    build_merkle_leaf,
    compute_canary_proof,
    get_sybil_detector,
    # beam.crypto.hashing
    sha256,
    # beam.crypto.signatures
    verify_hotkey_signature,
)
from core.config import Settings, get_settings

# Redundancy and failover imports
from core.redundancy import (
    CheckpointManager,
    HealthMonitor,
    RecoveryManager,
    create_redundancy_system,
    initialize_with_recovery,
)

# SubnetCore API client for score submission (replaces direct DB access)
try:
    from clients import SubnetCoreClient

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
    last_seen: datetime = field(default_factory=datetime.utcnow)
    registered_at: Optional[datetime] = None
    is_healthy: bool = True
    is_subnet_owned: bool = False  # True for UID #1
    last_score: float = 0.0


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

    # Extended work metrics
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
    - Track orchestrator work and payment/fraud signals
    - Set weights on Bittensor network

    Supports both:
    - Local mode: HTTP-based communication with orchestrators
    - Mainnet: Bittensor dendrite-based communication
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

        # Local score cache; BeamCore PRISM remains authoritative.
        self.orchestrator_scores: Dict[str, float] = {}  # hotkey -> final score
        self.payment_penalty_multipliers: Dict[str, float] = (
            {}
        )  # hotkey -> multiplier (1.0 = no penalty)

        # Work summaries (rolling 24h window)
        self.work_summaries: Dict[str, WorkSummary] = {}  # hotkey -> last summary
        self.work_summary_history: Dict[str, List[WorkSummary]] = {}  # hotkey -> history

        # Sybil Detection
        self.sybil_detector = get_sybil_detector()

        # Orchestrator performance tracking (rolling window)
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

        # PoB verification stats (populated by _verify_subnet_core_proofs)
        self.pob_verification_results: Dict[str, PoBVerificationStats] = {}  # hotkey -> stats

        # Weight history
        self.last_weight_block: int = 0
        self.weights_history: List[Dict] = []
        self._chain_weights_rate_limit: int = 0  # cached from subtensor at startup

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
        logger.debug("Initializing Validator node...")

        if self.settings.local_mode:
            # Local development mode - skip Bittensor network connection
            logger.debug("Running in LOCAL MODE - skipping Bittensor network connection")
            await self._initialize_local_mode()
        else:
            # Mainnet mode with Bittensor
            await self._initialize_bittensor_mode()

        logger.debug("Validator node initialized")

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

        # Connect to subtensor for mainnet
        if self.settings.subtensor_address:
            self.subtensor = bt.Subtensor(network=self.settings.subtensor_address)
        else:
            self.subtensor = bt.Subtensor(network=self.settings.subtensor_network)
        logger.debug(f"Connected to subtensor: {self.subtensor.network}")

        # Load metagraph for mainnet data
        try:
            self.metagraph = bt.Metagraph(
                netuid=self.settings.netuid,
                network=self.subtensor.network,
            )
            self.metagraph.sync(subtensor=self.subtensor)
            logger.debug(f"Metagraph loaded with {len(self.metagraph.hotkeys)} neurons")
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
            logger.debug(f"Fiber chain initialized with {len(self._fiber_nodes)} nodes")
        except Exception as e:
            logger.warning("Fiber chain init failed: %s", e, exc_info=True)
            self.fiber_chain = None

        # Create HTTP session for local mode communication
        self._http_session = aiohttp.ClientSession(timeout=aiohttp.ClientTimeout(total=60))

        # Add local orchestrator as a connection
        self.connections[1] = {
            "uid": 1,
            "hotkey": "local-orchestrator",
            "ip": "127.0.0.1",
            "port": 8000,
            "url": self.settings.orchestrator_url,
            "last_seen": datetime.utcnow(),
            "is_local": True,
        }

        # Discover orchestrators
        await self._discover_orchestrators()

        logger.debug(f"Local mode initialized with hotkey: {self.hotkey}")

    async def _initialize_bittensor_mode(self) -> None:
        """Initialize validator in Bittensor network mode"""
        # Load wallet
        self.wallet = bt.Wallet(
            name=self.settings.wallet_name,
            hotkey=self.settings.wallet_hotkey,
            path=self.settings.wallet_path,
        )
        self.hotkey = self.wallet.hotkey.ss58_address
        logger.debug(f"Wallet loaded: {self.hotkey}")

        # Connect to subtensor
        if self.settings.subtensor_address:
            self.subtensor = bt.Subtensor(network=self.settings.subtensor_address)
        else:
            self.subtensor = bt.Subtensor(network=self.settings.subtensor_network)
        logger.debug(f"Connected to subtensor: {self.subtensor.network}")

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
            logger.debug(f"Fiber chain initialized with {len(self._fiber_nodes)} nodes")
        except Exception as e:
            logger.warning("Fiber chain init failed: %s", e, exc_info=True)
            self.fiber_chain = None

        # Check registration
        await self._check_registration()

        # Seed last_weight_block from chain so a restart doesn't immediately retry set_weights
        if self.uid is not None and self.subtensor is not None:
            try:
                last_update_vec = self.subtensor.query_module(
                    "SubtensorModule", "LastUpdate", [self.settings.netuid]
                )
                vec = (last_update_vec.value or []) if last_update_vec else []
                if self.uid < len(vec):
                    self.last_weight_block = int(vec[self.uid])
                    _sw_rows = [
                        ("UID",        str(self.uid)),
                        ("Last Block", str(self.last_weight_block)),
                    ]
                    _sw_kw = max(len(k) for k, _ in _sw_rows)
                    _sw_vw = max(len(v) for _, v in _sw_rows)
                    _sw_in = _sw_kw + _sw_vw + 5
                    print("\n".join([
                        f"┌{'─' * _sw_in}┐",
                        f"│{'Chain Weight Block':^{_sw_in}}│",
                        f"├{'─' * (_sw_kw + 2)}┬{'─' * (_sw_vw + 2)}┤",
                        *[f"│ {k:<{_sw_kw}} │ {v:<{_sw_vw}} │" for k, v in _sw_rows],
                        f"└{'─' * (_sw_kw + 2)}┴{'─' * (_sw_vw + 2)}┘",
                    ]), flush=True)
            except Exception as _exc:
                logger.debug("Could not read LastUpdate from chain: %s", _exc)

        # Cache the chain's weights_rate_limit so we can compute wait times without hitting chain
        if self.subtensor is not None:
            try:
                self._chain_weights_rate_limit = self.subtensor.weights_rate_limit(self.settings.netuid) or 0
                logger.info("Chain weights_rate_limit for netuid %s: %s blocks", self.settings.netuid, self._chain_weights_rate_limit)
            except Exception as _exc:
                logger.debug("Could not read weights_rate_limit: %s", _exc)

        # Setup dendrite for querying connections
        self.dendrite = bt.Dendrite(wallet=self.wallet)

        # Create HTTP session
        self._http_session = aiohttp.ClientSession(timeout=aiohttp.ClientTimeout(total=60))

    async def _check_registration(self) -> None:
        """Check if this hotkey is registered on the subnet"""
        # Try Fiber first if available
        uid = self._get_uid_for_hotkey(self.hotkey)

        if uid is not None:
            self.uid = uid
            self.is_registered = True
            logger.debug(f"Registered on subnet {self.settings.netuid} with UID {self.uid}")
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

        logger.warning(
            f"_get_uid_for_hotkey: {hotkey[:16]}... -> NOT FOUND (fiber_nodes={len(self._fiber_nodes)}, metagraph={'yes' if self.metagraph else 'no'})"
        )
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
            self.health_monitor, self.checkpoint_manager, self.recovery_manager = (
                create_redundancy_system(self)
            )

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

        # Cancel heartbeat task
        if hasattr(self, "_heartbeat_task") and self._heartbeat_task:
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

    async def _submit_beamcore_heartbeat(self) -> None:
        """POST /validators/heartbeat to BeamCore."""
        if not SUBNET_CORE_AVAILABLE or not self.subnet_core_client:
            return

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

        status = "online"
        if health_info and health_info.get("checks_failed", 0) > 0:
            status = "degraded"

        result = await self.subnet_core_client.submit_heartbeat(
            validator_uid=self.uid,
            status=status,
            last_epoch_scored=self.current_epoch or None,
            health_info=health_info,
            external_url=self.settings.external_url,
        )
        api_key = result.get("api_key")
        if api_key:
            self.subnet_core_client._api_key = api_key
        _hb_items = [
            ("Status", status),
            ("UID",    str(self.uid)),
            ("Epoch",  str(self.current_epoch) if self.current_epoch else "—"),
        ]
        if api_key:
            _hb_items.append(("API Key", "received"))
        _hk, _hv = 7, max(len(v) for _, v in _hb_items)
        _hv = max(_hv, 8)
        _top   = f"┌{'─' * (_hk + _hv + 5)}┐"
        _title = f"│{'Heartbeat':^{_hk + _hv + 5}}│"
        _sep   = f"├{'─' * (_hk + 2)}┬{'─' * (_hv + 2)}┤"
        _body  = "\n".join(f"│ {k:<{_hk}} │ {v:<{_hv}} │" for k, v in _hb_items)
        _bot   = f"└{'─' * (_hk + 2)}┴{'─' * (_hv + 2)}┘"
        print("\n".join([_top, _title, _sep, _body, _bot]), flush=True)

    async def _heartbeat_loop(self) -> None:
        """Send periodic heartbeats to SubnetCore while running."""
        heartbeat_interval = 60  # seconds

        while self._running:
            try:
                await asyncio.sleep(heartbeat_interval)

                if not self._running:
                    break

                await self._submit_beamcore_heartbeat()

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

                # Update scores
                await self._update_scores()

                # Check for epoch change and broadcast (must run before weight-setting
                # so self.current_epoch is correct on the first main-loop iteration)
                await self._check_epoch()

                # Set weights if needed
                await self._maybe_set_weights()

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
                        proof_id=proof_data.get("proof_id"),
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
                    total_bytes_verified=prev.total_bytes_verified
                    + sum(p.get("bytes_relayed", 0) for p in verified_list),
                )

            # Build work summaries from verified proofs
            await self._build_work_summaries_from_proofs(verified_proofs_by_orch)

        except Exception as e:
            logger.error(f"Error fetching/verifying SubnetCore proofs: {e}")

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
            bandwidths = [
                p.get("bandwidth_mbps", 0.0) for p in proofs if p.get("bandwidth_mbps", 0) > 0
            ]
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
                    url="http://unknown",  # Not needed for SubnetCore flow
                    hotkey=orch_hotkey,
                    uid=orch_uid,
                    is_healthy=True,
                )
                logger.info(
                    f"Auto-registered orchestrator {orch_hotkey[:16]}... from SubnetCore proofs"
                )

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
            start_time_us = proof_data.get("start_time_us", 0)
            end_time_us = proof_data.get("end_time_us", 0)
            bytes_relayed = proof_data.get("bytes_relayed", 0)
            bandwidth_mbps = proof_data.get("bandwidth_mbps", 0.0)
            canary_proof = proof_data.get("canary_proof", "")
            worker_signature = proof_data.get("worker_signature", "")

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
                    logger.warning(
                        f"Signature verification error for {task_id[:16]}...: {sig_error}"
                    )
                    return result
            else:
                # No signature provided - log warning but allow for now
                # TODO: Make signatures required once orchestrator signing is implemented
                logger.debug(f"No worker signature for {task_id[:16]}... (unsigned proof)")
                result.signature_valid = True

            # 5. Geographic check (simplified)
            result.geo_valid = True

            # All checks passed
            result.valid = (
                result.timing_valid
                and result.bandwidth_valid
                and result.canary_valid
                and result.signature_valid
                and result.geo_valid
            )

            return result

        except Exception as e:
            return ProofVerificationResult(
                task_id=task_id,
                valid=False,
                error=f"Verification error: {str(e)}",
            )

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
            hotkey = self.metagraph.hotkeys[uid]

            # Only track orchestrators registered with BeamCore
            if hotkey not in registered_hotkeys:
                skipped_unregistered += 1
                continue

            axon = self.metagraph.axons[uid]

            self.connections[uid] = {
                "uid": uid,
                "hotkey": hotkey,
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
        """Discover orchestrators from BeamCore epoch summary (materialized weights + metrics)."""
        if SUBNET_CORE_AVAILABLE and self.subnet_core_client and self.wallet:
            try:
                result = await self.subnet_core_client.get_latest_epoch_summary()
                orch_list = result.get("orchestrators", [])
                if orch_list:
                    added = 0
                    for o in orch_list:
                        hotkey = o.get("hotkey", "")
                        if not hotkey:
                            continue
                        uid = o.get("uid")
                        url = "via-subnetcore"

                        if hotkey not in self.orchestrators:
                            self.orchestrators[hotkey] = OrchestratorInfo(
                                url=url,
                                hotkey=hotkey,
                                uid=uid,
                                is_healthy=True,
                            )
                            added += 1
                        else:
                            existing = self.orchestrators[hotkey]
                            if uid is not None and existing.uid is None:
                                existing.uid = uid

                        if uid is not None:
                            self._beamcore_worker_counts[uid] = 0

                    if added:
                        logger.info(
                            f"SubnetCore discovery (epoch summary): added {added} new orchestrator(s), "
                            f"total={len(self.orchestrators)}"
                        )
                    return
            except Exception as e:
                logger.warning(f"BeamCore orchestrator discovery failed: {e}")

    async def _query_orchestrators(self) -> None:
        """Work summaries are derived from verified BeamCore proofs in-memory."""
        return

    async def _sync_orchestrators(self) -> None:
        """Sync orchestrators from metagraph."""
        if self.metagraph is None:
            return

        for uid in range(len(self.metagraph.hotkeys)):
            hotkey = self.metagraph.hotkeys[uid]
            existing = self.orchestrator_manager.get_orchestrator(uid)

            if existing is None and uid != SUBNET_ORCHESTRATOR_UID:
                try:
                    self.orchestrator_manager.register_orchestrator(
                        uid=uid,
                        hotkey=hotkey,
                    )
                    logger.info(f"Registered new orchestrator UID {uid}")
                except ValueError as e:
                    logger.warning(f"Could not register orchestrator UID {uid}: {e}")

        self.orchestrator_manager.update_all_statuses()

    # =========================================================================
    # Challenge Generation and Sending
    # =========================================================================

    async def _generate_and_send_challenges(self) -> None:
        """Generate bandwidth challenges and send to connections via dendrite"""
        for uid, conn in self.connections.items():
            active_for_uid = [
                c
                for c in self.active_challenges.values()
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
                c
                for c in self.active_challenges.values()
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
        canary_offset = int.from_bytes(os.urandom(4), "big") % (
            self.settings.chunk_size_bytes - CANARY_SIZE_BYTES
        )

        chunk_with_canary = bytearray(chunk_data)
        chunk_with_canary[canary_offset : canary_offset + len(canary)] = canary
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
            canary=canary,
            canary_offset=canary_offset,
            path=[connection["hotkey"]],
            created_at=int(time.time() * 1_000_000),
        )
        self.pending_tasks[task_id] = task

        if connection.get("is_local") and self._http_session:
            return await self._send_challenge_http(uid, task_id, chunk_data, challenge, connection)
        elif self.dendrite:
            return await self._send_challenge_dendrite(uid, task_id, chunk_data, challenge)
        return None

    async def _issue_orchestrator_challenge(
        self,
        orchestrator: OrchestratorInfo,
    ) -> Optional[ChallengeResult]:
        """Bandwidth challenge flow has been removed from the BeamCore validator path."""
        logger.debug("Challenge flow is disabled for orchestrator %s", orchestrator.hotkey[:16])
        return None

    async def _send_challenge_http(
        self,
        uid: int,
        task_id: str,
        chunk_data: bytes,
        challenge: BandwidthChallenge,
        connection: dict,
    ) -> Optional[BandwidthChallenge]:
        """Legacy HTTP challenge path removed."""
        return None

    async def _send_chunk_data_http(
        self,
        uid: int,
        task_id: str,
        chunk_data: bytes,
        challenge: BandwidthChallenge,
        connection: dict,
    ) -> bool:
        """Legacy HTTP challenge data path removed."""
        return False

    async def _send_challenge_dendrite(
        self,
        uid: int,
        task_id: str,
        chunk_data: bytes,
        challenge: BandwidthChallenge,
    ) -> Optional[BandwidthChallenge]:
        """Send challenge via Bittensor dendrite (for mainnet)"""
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
                logger.info(f"Challenge {task_id[:16]}... accepted by UID {uid}")
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

        logger.info(
            f"_collect_pob_results: processing {len(self.pending_pob_submissions)} pending PoB submissions"
        )

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
                logger.debug(
                    f"_collect_pob_results: {task_id[:16]}... expired (age={now - received_at:.0f}s)"
                )
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
                bandwidth_mbps = challenge_info.get("bandwidth_mbps", 0)
                chunk_sent_at = challenge_info.get("chunk_sent_at", now)
                receive_time = challenge_info.get("receive_time", now)

                task = self.pending_tasks.get(task_id)
                if task:
                    mock_pob = ProofOfBandwidth(
                        task_id=bytes.fromhex(task_id),
                        miner_id=challenge_info.get("connection", {})
                        .get("hotkey", "local-orchestrator")
                        .encode(),
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
        logger.info(
            f"verify_pob: starting verification for task {task_id_hex[:16]}... miner={miner_hotkey[:16]}..."
        )

        result = PoBVerificationResult(pob=pob, valid=False)

        try:
            message = pob.get_canonical_message()
            result.signature_valid = verify_hotkey_signature(message, pob.signature, miner_hotkey)
        except Exception as e:
            result.signature_valid = False
            result.error = f"Invalid signature: {e}"
            logger.warning(
                f"verify_pob: signature check FAILED for task {task_id_hex[:16]}... error={e}"
            )
            return result

        if not result.signature_valid:
            result.error = "Invalid signature"
            logger.warning(
                f"verify_pob: invalid signature for task {task_id_hex[:16]}... miner={miner_hotkey[:16]}..."
            )
            return result
        logger.debug(f"verify_pob: signature OK for task {task_id_hex[:16]}...")

        delta_us = pob.end_time - pob.start_time

        if delta_us < self.settings.min_transfer_time_us:
            result.error = f"Transfer too fast: {delta_us}µs"
            logger.warning(
                f"verify_pob: transfer too fast for task {task_id_hex[:16]}... delta={delta_us}µs min={self.settings.min_transfer_time_us}µs"
            )
            return result

        if pob.end_time > task.deadline:
            result.error = "Task deadline exceeded"
            logger.warning(
                f"verify_pob: deadline exceeded for task {task_id_hex[:16]}... end={pob.end_time} deadline={task.deadline}"
            )
            return result

        result.timing_valid = True
        logger.debug(f"verify_pob: timing OK for task {task_id_hex[:16]}... delta={delta_us}µs")

        calculated_bandwidth = pob.calculate_bandwidth()
        bandwidth_diff = abs(calculated_bandwidth - pob.bandwidth_mbps)

        if bandwidth_diff > 1.0:
            result.error = f"Bandwidth mismatch: claimed {pob.bandwidth_mbps}, calculated {calculated_bandwidth}"
            logger.warning(
                f"verify_pob: bandwidth mismatch for task {task_id_hex[:16]}... claimed={pob.bandwidth_mbps:.2f} calculated={calculated_bandwidth:.2f} diff={bandwidth_diff:.2f}"
            )
            return result

        result.bandwidth_valid = True
        result.calculated_bandwidth = calculated_bandwidth
        logger.debug(
            f"verify_pob: bandwidth OK for task {task_id_hex[:16]}... bw={calculated_bandwidth:.2f} Mbps"
        )

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
                logger.warning(
                    f"verify_pob: missing merkle proof for multi-hop task {task_id_hex[:16]}... (path_len={len(task.path)})"
                )
                return result

            # Verify merkle proof: build expected leaf and check against path
            try:
                # Determine hop neighbors from task path
                hop_idx = min(pob.path_index, len(task.path) - 1)
                prev_hop = task.path[hop_idx - 1] if hop_idx > 0 else ""
                next_hop = task.path[hop_idx + 1] if hop_idx < len(task.path) - 1 else ""

                build_merkle_leaf(
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
                logger.debug(
                    f"verify_pob: merkle proof structure OK for task {task_id_hex[:16]}..."
                )

            except Exception as merkle_err:
                result.merkle_valid = False
                result.error = f"Merkle verification error: {merkle_err}"
                logger.warning(
                    f"verify_pob: merkle error for task {task_id_hex[:16]}...: {merkle_err}"
                )
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
            (hk, s)
            for hk, s in self.work_summaries.items()
            if s.proof_count > 0
            and self.orchestrators.get(hk)
            and self.orchestrators[hk].is_healthy
        ]
        if eligible:
            logger.info(f"_spot_check_proofs: checking {len(eligible)} eligible orchestrators")

        # Prefetch all epoch proofs in one request to avoid per-orchestrator 429 bursts
        proof_ids_by_hotkey: Dict[str, List[str]] = {}
        if SUBNET_CORE_AVAILABLE and self.subnet_core_client:
            try:
                result = await self.subnet_core_client.get_proofs_from_subnetcore(
                    epoch=self.current_epoch,
                    limit=1000,
                )
                for p in result.get("proofs", []):
                    orch_hk = p.get("orchestrator_hotkey", "")
                    task_id = p.get("task_id", "")
                    if orch_hk and task_id:
                        proof_ids_by_hotkey.setdefault(orch_hk, []).append(task_id)
                logger.info(f"_spot_check_proofs: prefetched proofs for {len(proof_ids_by_hotkey)} orchestrators")
            except Exception as e:
                logger.warning(f"_spot_check_proofs: epoch proof prefetch failed: {e}")

        for hotkey, summary in self.work_summaries.items():
            if summary.proof_count == 0:
                continue

            orchestrator = self.orchestrators.get(hotkey)
            if not orchestrator or not orchestrator.is_healthy:
                continue

            try:
                result = await self._spot_check_orchestrator(
                    hotkey, orchestrator, summary,
                    prefetched_ids=proof_ids_by_hotkey.get(hotkey),
                )
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
        prefetched_ids: List[str] = None,
    ) -> SpotCheckResult:
        """Perform spot-check verification for a single orchestrator."""
        sample_percent = 0.05 + random.random() * 0.05
        sample_size = max(5, min(50, int(summary.proof_count * sample_percent)))
        logger.info(
            f"_spot_check_orchestrator: {hotkey[:16]}... "
            f"sampling {sample_size} of {summary.proof_count} proofs ({sample_percent:.1%})"
        )

        proof_ids = await self._get_random_proof_ids(orchestrator, sample_size, prefetched_ids=prefetched_ids)

        if not proof_ids:
            logger.warning(f"_spot_check_orchestrator: {hotkey[:16]}... no proof IDs returned")
            return SpotCheckResult(
                orchestrator_hotkey=hotkey,
                proofs_requested=sample_size,
                proofs_received=0,
                proofs_valid=0,
                proofs_invalid=0,
            )

        logger.debug(
            f"_spot_check_orchestrator: {hotkey[:16]}... got {len(proof_ids)} proof IDs, requesting full proofs"
        )
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
            + (
                f" invalid_reasons={dict(list(invalid_reasons.items())[:3])}"
                if invalid_reasons
                else ""
            )
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
        prefetched_ids: List[str] = None,
    ) -> List[str]:
        """Get random proof IDs for spot-checking."""
        # Use prefetched batch if available (avoids per-orchestrator API calls)
        if prefetched_ids is not None:
            if not prefetched_ids:
                return []
            n = min(sample_size, len(prefetched_ids))
            return random.sample(prefetched_ids, n)

        # Fallback: per-orchestrator fetch
        if SUBNET_CORE_AVAILABLE and self.subnet_core_client:
            try:
                result = await self.subnet_core_client.get_proofs_from_subnetcore(
                    epoch=self.current_epoch,
                    orchestrator_hotkey=orchestrator.hotkey,
                    limit=sample_size * 2,
                )
                proofs = result.get("proofs", [])
                if proofs:
                    all_proof_ids = [p.get("task_id", "") for p in proofs if p.get("task_id")]
                    n = min(sample_size, len(all_proof_ids))
                    return random.sample(all_proof_ids, n) if all_proof_ids else []
                return []
            except Exception as e:
                logger.warning(
                    f"BeamCore proof query failed for {orchestrator.hotkey[:16]}...: {e}"
                )
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
    # Scoring
    # =========================================================================

    async def _update_scores(self) -> None:
        """Update local orchestrator scores from work, challenge, fraud, and payment signals."""
        logger.info(
            f"_update_scores: scoring {len(self.orchestrators)} orchestrators, {len(self.work_summaries)} with summaries"
        )

        _baseline_multiplier = 0.1

        for hotkey, info in self.orchestrators.items():
            summary = self.work_summaries.get(hotkey)

            if not summary:
                final_score = 1.0 if info.is_subnet_owned else _baseline_multiplier
                self.orchestrator_scores[hotkey] = final_score
                info.last_score = final_score
                logger.info(
                    f"_update_scores: {hotkey[:16]}... UID={info.uid} "
                    f"no work summary - baseline score={final_score:.4f}"
                )
                continue

            throughput_score = min(max(summary.avg_bandwidth_mbps / 1000.0, 0.0), 1.0)
            reliability_score = min(max(summary.success_rate, 0.0), 1.0)
            work_score = (throughput_score * 0.6) + (reliability_score * 0.4)
            challenge_multiplier = self._calculate_challenge_multiplier(hotkey)
            fraud_multiplier = self._calculate_fraud_multiplier(hotkey)
            payment_multiplier = self.payment_penalty_multipliers.get(hotkey, 1.0)
            final_score = work_score * challenge_multiplier * fraud_multiplier * payment_multiplier

            self.orchestrator_scores[hotkey] = final_score
            info.last_score = final_score

            logger.info(
                f"_update_scores: {hotkey[:16]}... UID={info.uid} "
                f"work_score={work_score:.4f} "
                f"challenge_mult={challenge_multiplier:.4f} "
                f"fraud_mult={fraud_multiplier:.4f} "
                f"payment_mult={payment_multiplier:.4f} "
                f"final_score={final_score:.6f}"
            )

        # Also update connection scores for legacy compatibility
        for uid in self.connections:
            conn_results = [
                r
                for r in self.task_results.values()
                if r.valid and self._get_uid_for_miner(r.pob.miner_id) == uid
            ]

            if not conn_results:
                continue

            bandwidth_scores = [r.calculated_bandwidth for r in conn_results]
            avg_bandwidth = sum(bandwidth_scores) / len(bandwidth_scores)

            bandwidth_normalized = min(avg_bandwidth / self.settings.max_bandwidth_mbps, 1.0)

            total_tasks = len(
                [t for t in self.pending_tasks.values() if self._get_uid_for_task(t) == uid]
            )
            success_rate = len(conn_results) / total_tasks if total_tasks > 0 else 0

            score = (
                self.settings.score_weight_bandwidth * bandwidth_normalized
                + self.settings.score_weight_uptime * success_rate
                + self.settings.score_weight_loss * 1.0
                + self.settings.score_weight_tier * 0.5
            )

            if uid in self.connection_scores:
                old_score = self.connection_scores[uid]
                score = BANDWIDTH_EMA_ALPHA * score + (1 - BANDWIDTH_EMA_ALPHA) * old_score

            self.connection_scores[uid] = score
            self.connection_bandwidth[uid] = avg_bandwidth

    def _calculate_challenge_multiplier(self, hotkey: str) -> float:
        """Calculate challenge verification multiplier."""
        challenge_results = [
            r
            for cid, r in self.challenge_results.items()
            if self.active_challenges.get(cid, {}).get("orchestrator") == hotkey
        ]

        if not challenge_results:
            logger.debug(
                f"_calculate_challenge_multiplier: {hotkey[:16]}... no challenge results, using default 0.9"
            )
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
        logger.debug(
            f"_get_uid_for_miner: {hotkey[:16]}... -> NOT FOUND in {len(self.connections)} connections"
        )
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
        blocks_since = current_block - self.last_weight_block

        def _fmt_wait(blocks: int) -> str:
            secs = blocks * 12
            return f"~{secs // 60}m {secs % 60}s" if secs >= 60 else f"~{secs}s"

        # Chain uses strict `blocks_since > rate_limit`, so we must wait for blocks_since >= rate_limit + 1
        effective_limit = max(self.settings.blocks_between_weights, self._chain_weights_rate_limit)
        if blocks_since <= effective_limit:
            blocks_remaining = effective_limit - blocks_since + 1
            logger.info("Next weight set window in %s (%d blocks)", _fmt_wait(blocks_remaining), blocks_remaining)
            return

        await self._set_weights()

    async def _set_weights(self) -> None:
        """Set weights using BeamCore's persisted epoch snapshot."""
        if not SUBNET_CORE_AVAILABLE or not self.subnet_core_client:
            logger.warning("BeamCore client not available, skipping weight setting")
            return

        weight_snapshot = await self._get_persisted_weight_snapshot()
        if not weight_snapshot:
            logger.warning("No persisted BeamCore weight snapshot available")
            return

        uids, weights, formula_version, params_hash, data_epoch, _no_weight_period, _burn_reason = weight_snapshot

        # Set weights on chain
        try:
            # Check if commit-reveal is enabled on this subnet
            commit_reveal_enabled = False
            if self.subtensor is not None:
                try:
                    cr_result = self.subtensor.query_module(
                        "SubtensorModule", "CommitRevealWeightsEnabled", [self.settings.netuid]
                    )
                    commit_reveal_enabled = bool(cr_result.value) if cr_result else False
                except Exception as e:
                    logger.debug(f"Could not check commit-reveal status: {e}")

            if commit_reveal_enabled and self.subtensor is not None:
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
                _kw, _uw, _ww = 9, 4, 10
                _info_items = [
                    ("Block",   str(self.last_weight_block)),
                    ("Epoch",   str(data_epoch)),
                    ("Method",  weight_method),
                    ("Formula", formula_version),
                ]
                if _no_weight_period:
                    _info_items.append(("Period", f"BURN — {_burn_reason}"))
                _vw = max(len(v) for _, v in _info_items)
                _hw = _kw + _vw - _uw - _ww - 3  # derived: header_outer == uid_outer
                _hw = max(_hw, 20)                # ensure hotkey col is readable
                _vw = _uw + _hw + _ww + 3 - _kw  # recompute in case _hw was clamped
                _hotkeys = getattr(self.metagraph, "hotkeys", []) if self.metagraph else []
                _sorted_w = sorted(zip(uids, weights), key=lambda x: -x[1])
                _uid_rows = []
                for _uid, _w in _sorted_w:
                    if _uid == 0 and _w > 0:
                        _hk = "(burn)"
                    elif 0 <= _uid < len(_hotkeys):
                        _hk = (_hotkeys[_uid][:_hw - 3] + "...") if len(_hotkeys[_uid]) > _hw else _hotkeys[_uid]
                    else:
                        _hk = "unknown"
                    _uid_rows.append(f"│ {_uid:>{_uw}} │ {_hk:<{_hw}} │ {_w:>{_ww}.4f} │")
                _inner = _kw + _vw + 5
                _top    = f"┌{'─' * _inner}┐"
                _title  = f"│{'Weight Set Successfully':^{_inner}}│"
                _hsep1  = f"├{'─' * (_kw + 2)}┬{'─' * (_vw + 2)}┤"
                _info   = "\n".join(f"│ {k:<{_kw}} │ {v:<{_vw}} │" for k, v in _info_items)
                _hsep2  = f"├{'─' * (_uw + 2)}┬{'─' * (_hw + 2)}┬{'─' * (_ww + 2)}┤"
                _uidhdr = f"│ {'UID':>{_uw}} │ {'Hotkey':<{_hw}} │ {'Weight':>{_ww}} │"
                _hsep3  = f"├{'─' * (_uw + 2)}┼{'─' * (_hw + 2)}┼{'─' * (_ww + 2)}┤"
                _bot    = f"└{'─' * (_uw + 2)}┴{'─' * (_hw + 2)}┴{'─' * (_ww + 2)}┘"
                print("\n".join([_top, _title, _hsep1, _info, _hsep2, _uidhdr, _hsep3] + _uid_rows + [_bot]), flush=True)

                self.weights_history.append(
                    {
                        "block": self.last_weight_block,
                        "timestamp": datetime.utcnow().isoformat(),
                        "weights": {uid: round(w, 6) for uid, w in zip(uids, weights)},
                        "weight_method": weight_method,
                        "formula_version": formula_version,
                    }
                )

                if len(self.weights_history) > 100:
                    self.weights_history = self.weights_history[-100:]

                if self.subnet_core_client:
                    try:
                        await self.subnet_core_client.submit_weight_proof(
                            epoch=data_epoch,
                            block_number=self.last_weight_block,
                            netuid=self.settings.netuid,
                            uids=list(uids),
                            weights=list(weights),
                            formula_version=formula_version,
                            params_hash=params_hash,
                        )
                        logger.debug("Weight proof submitted to BeamCore")
                    except Exception as _e:
                        _body = getattr(getattr(_e, "response", None), "text", None)
                        logger.warning("Failed to submit weight proof: %s%s", _e, f" — {_body}" if _body else "")

            else:
                _cur_block = self.subtensor.block if self.subtensor else "?"
                _msg = message or "(empty — likely chain rate limit or rejection)"
                logger.error(
                    "Failed to set weights: method=%s block=%s last_weight_block=%s message=%r",
                    weight_method, _cur_block, self.last_weight_block, _msg,
                )
                # Back off: treat this attempt as the new baseline so we don't
                # retry every block after a chain rejection.
                self.last_weight_block = self.subtensor.block if self.subtensor else self.last_weight_block

        except Exception as e:
            logger.error(f"Error setting weights: {e}", exc_info=True)

    async def _get_persisted_weight_snapshot(
        self,
    ) -> Optional[Tuple[List[int], List[float], str, Optional[str], int, bool, str]]:
        """Fetch recommended weights from BeamCore epoch summary (ops-materialized)."""
        if not self.subnet_core_client:
            return None
        try:
            snapshot = await self.subnet_core_client.get_latest_epoch_summary()
        except Exception as exc:
            logger.warning("BeamCore epoch summary unavailable: %s", exc)
            return None

        uids = snapshot.get("uids") or []
        weights = snapshot.get("weights") or []
        if not uids or len(uids) != len(weights):
            logger.warning("BeamCore epoch summary missing uids/weights vectors")
            return None
        fv = str(snapshot.get("formula_version") or "prism_final_x_task_done_count_v2")
        ph = snapshot.get("params_hash")
        if isinstance(ph, str):
            params_hash: Optional[str] = ph
        else:
            params_hash = None
        data_epoch = int(snapshot.get("epoch", self.current_epoch))

        _no_weight_period = bool(snapshot.get("no_weight_period"))
        _burn_reason = snapshot.get("reason", "") if _no_weight_period else ""
        return (list(uids), list(weights), fv, params_hash, data_epoch, _no_weight_period, _burn_reason)

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
            or self.current_epoch > 100_000  # Old epoch calculation was used
        )

        if should_sync and current_epoch != self.current_epoch:
            previous_epoch = self.current_epoch
            self.current_epoch = current_epoch
            self.epoch_start_block = current_epoch * epoch_length_blocks

            logger.info("══════════════ EPOCH %s ══════════════ (prev=%s)", self.current_epoch, previous_epoch)
            self.tasks_this_epoch = 0

            # Reset PoB verification stats for the new epoch
            self.pob_verification_results.clear()

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
            }
            for uid, conn in self.connections.items()
        }

    def get_orchestrator_scores(self) -> Dict[str, dict]:
        """Get all local orchestrator scores."""
        result = {}

        for hotkey, info in self.orchestrators.items():
            uid = self._get_uid_for_hotkey(hotkey)
            result[hotkey] = {
                "uid": uid,
                "score": round(self.orchestrator_scores.get(hotkey, 0), 4),
                "url": info.url,
                "is_healthy": info.is_healthy,
                "is_subnet_owned": info.is_subnet_owned,
                "last_seen": info.last_seen.isoformat(),
                "registered": uid is not None,
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
