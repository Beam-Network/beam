"""
Validator Core Logic - Unified Version

Validates Proof-of-Bandwidth submissions and sets weights on the Bittensor network.
Supports both local mode (HTTP) and mainnet/testnet (Bittensor dendrite).

"""

import asyncio
import base64
import logging
import os
import random
import time
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
    # beam.protocol.circuit
    BandwidthChallenge,
    ChunkTransfer,
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
from core.models import (
    ChallengeResult,
    OrchestratorInfo,
    PoBVerificationStats,
    ProofVerificationResult,
    SpotCheckResult,
    WorkSummary,
)
from core.proof_verifier import verify_single_subnet_proof
from core import (
    challenge_delivery,
    lifecycle,
    network_state,
    pob_runtime,
    scoring,
    spot_checks,
    state_views,
    validator_state,
    weight_setting,
)

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
    - Redirect penalties to Orchestrator #1 (subnet treasury)
    - Process worker reassignments
    - Set weights on Bittensor network

    Supports both:
    - Local mode: HTTP-based communication with orchestrators
    - Mainnet/Testnet: Bittensor dendrite-based communication
    """

    def __init__(self, settings: Optional[Settings] = None):
        validator_state.initialize_validator_state(self, settings)
    async def initialize(self) -> None:
        await lifecycle.initialize(self)

    async def _initialize_local_mode(self) -> None:
        await lifecycle._initialize_local_mode(self)

    async def _initialize_bittensor_mode(self) -> None:
        await lifecycle._initialize_bittensor_mode(self)

    async def _check_registration(self) -> None:
        await lifecycle._check_registration(self)

    def _get_uid_for_hotkey(self, hotkey: str) -> Optional[int]:
        return lifecycle._get_uid_for_hotkey(self, hotkey)

    def _get_node_info(self, hotkey: str) -> Optional[FiberNode]:
        return lifecycle._get_node_info(self, hotkey)

    async def start(self) -> None:
        await lifecycle.start(self)

    async def _initialize_redundancy(self) -> None:
        await lifecycle._initialize_redundancy(self)

    async def stop(self) -> None:
        await lifecycle.stop(self)

    async def _submit_beamcore_heartbeat(self) -> None:
        await lifecycle._submit_beamcore_heartbeat(self, SUBNET_CORE_AVAILABLE)

    async def _heartbeat_loop(self) -> None:
        await lifecycle._heartbeat_loop(self)

    async def _main_loop(self) -> None:
        await lifecycle._main_loop(self)
    async def _verify_single_subnet_proof(
        self,
        proof_data: dict,
    ) -> ProofVerificationResult:
        return await verify_single_subnet_proof(
            proof_data,
            signature_verifier=verify_hotkey_signature,
            logger=logger,
        )
    # =========================================================================
    # Metagraph and Connection Management
    # =========================================================================

    async def _sync_metagraph(self) -> None:
        await network_state.sync_metagraph(self)

    async def _update_connections(self) -> None:
        await network_state.update_connections(self)

    async def _discover_orchestrators(self) -> None:
        await network_state.discover_orchestrators(self, SUBNET_CORE_AVAILABLE)
    # =========================================================================
    # Challenge Generation and Sending
    # =========================================================================

    async def _generate_and_send_challenges(self) -> None:
        await challenge_delivery._generate_and_send_challenges(self)

    async def _issue_challenges(self) -> None:
        await challenge_delivery._issue_challenges(self)

    async def _create_and_send_challenge(
        self,
        uid: int,
        connection: dict,
    ) -> Optional[BandwidthChallenge]:
        return await challenge_delivery._create_and_send_challenge(self, uid, connection)

    async def _issue_orchestrator_challenge(
        self,
        orchestrator: OrchestratorInfo,
    ) -> Optional[ChallengeResult]:
        return await challenge_delivery._issue_orchestrator_challenge(self, orchestrator)

    async def _send_challenge_http(
        self,
        uid: int,
        task_id: str,
        chunk_data: bytes,
        challenge: BandwidthChallenge,
        connection: dict,
    ) -> Optional[BandwidthChallenge]:
        return await challenge_delivery._send_challenge_http(
            self, uid, task_id, chunk_data, challenge, connection
        )

    async def _send_chunk_data_http(
        self,
        uid: int,
        task_id: str,
        chunk_data: bytes,
        challenge: BandwidthChallenge,
        connection: dict,
    ) -> bool:
        return await challenge_delivery._send_chunk_data_http(
            self, uid, task_id, chunk_data, challenge, connection
        )

    async def _send_challenge_dendrite(
        self,
        uid: int,
        task_id: str,
        chunk_data: bytes,
        challenge: BandwidthChallenge,
    ) -> Optional[BandwidthChallenge]:
        return await challenge_delivery._send_challenge_dendrite(
            self, uid, task_id, chunk_data, challenge
        )

    async def _send_chunk_data(
        self,
        uid: int,
        task_id: str,
        chunk_data: bytes,
        challenge: BandwidthChallenge,
    ) -> bool:
        return await challenge_delivery._send_chunk_data(
            self, uid, task_id, chunk_data, challenge
        )
    # =========================================================================
    # PoB Verification
    # =========================================================================

    async def _collect_pob_results(self) -> None:
        await pob_runtime._collect_pob_results(self)

    async def _process_local_challenge_results(self) -> None:
        await pob_runtime._process_local_challenge_results(self)

    def verify_pob(
        self,
        pob: ProofOfBandwidth,
        task: Task,
    ) -> PoBVerificationResult:
        return pob_runtime.verify_pob(
            self,
            pob,
            task,
            verify_hotkey_signature,
        )

    async def _cleanup_old_results(self) -> None:
        await pob_runtime._cleanup_old_results(self)
    # =========================================================================
    # Spot-Check Proofs
    # =========================================================================

    async def _spot_check_proofs(self) -> None:
        await spot_checks.spot_check_proofs(self)

    async def _spot_check_orchestrator(
        self,
        hotkey: str,
        orchestrator: OrchestratorInfo,
        summary: WorkSummary,
    ) -> SpotCheckResult:
        return await spot_checks.spot_check_orchestrator(
            self,
            hotkey,
            orchestrator,
            summary,
        )

    async def _get_random_proof_ids(
        self,
        orchestrator: OrchestratorInfo,
        sample_size: int,
    ) -> List[str]:
        return await spot_checks.get_random_proof_ids(self, orchestrator, sample_size)

    async def _request_proofs(
        self,
        orchestrator: OrchestratorInfo,
        proof_ids: List[str],
    ) -> List[dict]:
        return await spot_checks.request_proofs(self, orchestrator, proof_ids)
    # =========================================================================
    # Scoring
    # =========================================================================

    async def _update_scores(self) -> None:
        await scoring._update_scores(self)

    def _calculate_challenge_multiplier(self, hotkey: str) -> float:
        return scoring._calculate_challenge_multiplier(self, hotkey)

    def _calculate_fraud_multiplier(self, hotkey: str) -> float:
        return scoring._calculate_fraud_multiplier(self, hotkey)

    def _get_uid_for_miner(self, miner_id) -> Optional[int]:
        return scoring._get_uid_for_miner(self, miner_id)

    def _get_uid_for_task(self, task: Task) -> Optional[int]:
        return scoring._get_uid_for_task(self, task)
    # =========================================================================
    # Weight Setting
    # =========================================================================

    async def _maybe_set_weights(self) -> None:
        await weight_setting._maybe_set_weights(self)

    async def _set_weights(self) -> None:
        await weight_setting._set_weights(self, SUBNET_CORE_AVAILABLE)

    async def _get_persisted_weight_snapshot(
        self,
    ) -> Optional[Tuple[List[int], List[float], str, Optional[str], int]]:
        return await weight_setting._get_persisted_weight_snapshot(self)
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

        # Sync when the epoch advances or when a stale local epoch marker is present.
        should_sync = (
            current_epoch > self.current_epoch  # Normal case: new epoch
            or self.current_epoch > 100_000
        )

        if should_sync and current_epoch != self.current_epoch:
            prior_epoch = self.current_epoch
            self.current_epoch = current_epoch
            self.epoch_start_block = current_epoch * epoch_length_blocks

            logger.info("══════════════ EPOCH %s ══════════════ (prior=%s)", self.current_epoch, prior_epoch)
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
        return state_views.get_validator_state(self)

    def get_connection_scores(self) -> Dict[int, dict]:
        return state_views.get_connection_scores(self)

    def get_orchestrator_scores(self) -> Dict[str, dict]:
        return state_views.get_orchestrator_scores(self)

    def get_spot_check_results(self) -> Dict[str, dict]:
        return state_views.get_spot_check_results(self)

    def get_weights_history(self, limit: int = 10) -> List[dict]:
        return state_views.get_weights_history(self, limit)
