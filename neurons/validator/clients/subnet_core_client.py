"""
BeamCore API Client

Client for validators to submit scores and data to the private BeamCore service.
This replaces direct database access and direct orchestrator communication.
"""

import logging
import secrets
import time
from typing import Optional, List, Dict, Any
from dataclasses import dataclass, asdict

import httpx

logger = logging.getLogger(__name__)


def build_signed_auth_headers(
    wallet,
    hotkey: str,
    action: str = "request",
) -> Dict[str, str]:
    """
    Build signed authentication headers for validator requests.

    Args:
        wallet: Bittensor wallet with hotkey for signing
        hotkey: Validator's SS58 hotkey address
        action: Action being performed (e.g., "get_orchestrators", "submit_scores")

    Returns:
        Dict with X-Validator-* headers including signature
    """
    timestamp = int(time.time())
    nonce = secrets.token_hex(16)

    # Build canonical message (must match server's build_auth_message)
    message = f"validator_auth:{hotkey}:{timestamp}:{action}:{nonce}"

    # Sign with hotkey
    try:
        signature = wallet.hotkey.sign(message.encode("utf-8"))
        signature_hex = signature.hex()
    except Exception as e:
        logger.error(f"Failed to sign auth message: {e}")
        raise

    return {
        "X-Validator-Hotkey": hotkey,
        "X-Validator-Signature": signature_hex,
        "X-Validator-Timestamp": str(timestamp),
        "X-Validator-Nonce": nonce,
        "X-Validator-Action": action,
    }


@dataclass
class UIDRanges:
    """Orchestrator UID range configuration from BeamCore."""

    subnet_orchestrator_uid: int
    public_orchestrator_uid_start: int
    public_orchestrator_uid_end: int
    reserved_orchestrator_uid_start: int
    reserved_orchestrator_uid_end: int
    max_orchestrators: int

    def is_valid_public_uid(self, uid: int) -> bool:
        """Check if a UID is in the public orchestrator range."""
        return self.public_orchestrator_uid_start <= uid <= self.public_orchestrator_uid_end

    def is_valid_reserved_uid(self, uid: int) -> bool:
        """Check if a UID is in the reserved orchestrator range."""
        return self.reserved_orchestrator_uid_start <= uid <= self.reserved_orchestrator_uid_end

    def is_subnet_orchestrator_uid(self, uid: int) -> bool:
        """Check if a UID is the subnet orchestrator."""
        return uid == self.subnet_orchestrator_uid

    def is_valid_orchestrator_uid(self, uid: int) -> bool:
        """Check if a UID is valid for any orchestrator type."""
        return (
            self.is_subnet_orchestrator_uid(uid) or
            self.is_valid_public_uid(uid) or
            self.is_valid_reserved_uid(uid)
        )


@dataclass
class ScoreSubmission:
    """Score submission for an orchestrator."""

    epoch: int
    orchestrator_hotkey: str
    orchestrator_uid: int

    # Scores
    bandwidth_score: float = 0.0
    compliance_score: float = 1.0
    final_score: float = 0.0

    # Metrics
    total_proofs_published: int = 0
    total_bytes_claimed: int = 0
    total_workers_active: int = 0
    avg_bandwidth_mbps: float = 0.0

    # Verification results
    proofs_verified: int = 0
    proofs_rejected: int = 0
    proofs_challenged: int = 0
    verification_rate: float = 0.0

    # Spot check results
    spot_checks_attempted: int = 0
    spot_checks_passed: int = 0
    spot_check_rate: float = 0.0


@dataclass
class SpotCheckSubmission:
    """Spot check result submission."""

    epoch: int
    orchestrator_hotkey: str
    proof_task_id: str
    worker_id: str
    worker_hotkey: str
    claimed_bytes: int
    claimed_bandwidth_mbps: float
    response_valid: bool
    verification_passed: bool
    orchestrator_uid: Optional[int] = None
    challenge_chunk_hash: Optional[str] = None
    challenge_offset: Optional[int] = None
    verification_notes: Optional[str] = None


@dataclass
class FraudPenaltySubmission:
    """Fraud penalty submission."""

    orchestrator_hotkey: str
    epoch_detected: int
    fraud_type: str
    severity: float = 1.0
    penalty_multiplier: float = 0.0
    penalty_duration_epochs: int = 10
    orchestrator_uid: Optional[int] = None
    worker_hotkey: Optional[str] = None
    evidence_task_id: Optional[str] = None
    evidence_description: Optional[str] = None
    evidence_data: Optional[str] = None


@dataclass
class ChallengeResultSubmission:
    """Challenge result submission."""

    challenge_id: str
    orchestrator_hotkey: str
    epoch: int
    challenge_type: str
    passed: bool
    expected_bandwidth_mbps: Optional[float] = None
    measured_bandwidth_mbps: Optional[float] = None
    bytes_transferred: Optional[int] = None
    latency_ms: Optional[float] = None
    failure_reason: Optional[str] = None
    score_impact: float = 0.0


class SubnetCoreClient:
    """
    Client for communicating with BeamCore.

    Validators use this client to:
    - Submit scores instead of direct DB access
    - Query work summaries (replaces direct orchestrator queries)
    - Coordinate bandwidth challenges (replaces direct orchestrator challenges)
    """

    def __init__(
        self,
        base_url: str,
        validator_hotkey: str,
        wallet=None,  # bittensor.wallet for signing
        timeout: float = 30.0,
    ):
        """
        Initialize the client.

        Args:
            base_url: Base URL of BeamCore (e.g., http://localhost:8090)
            validator_hotkey: This validator's hotkey for authentication
            wallet: Bittensor wallet for signing requests (optional, enables signed auth)
            timeout: Request timeout in seconds
        """
        self.base_url = base_url.rstrip("/")
        self.validator_hotkey = validator_hotkey
        self.wallet = wallet
        self.timeout = timeout
        self._client: Optional[httpx.AsyncClient] = None

    def _get_auth_headers(self, action: str = "request") -> Dict[str, str]:
        """Get authentication headers for requests."""
        if self.wallet:
            try:
                headers = build_signed_auth_headers(
                    wallet=self.wallet,
                    hotkey=self.validator_hotkey,
                    action=action,
                )
                logger.debug(f"Using signed auth headers for action={action}")
                return headers
            except Exception as e:
                logger.warning(f"Failed to build signed auth headers: {e}, falling back to simple header")

        # Fallback to simple hotkey header (only works with AUTH_LOCAL_MODE=true)
        if not self.validator_hotkey:
            logger.error("CRITICAL: validator_hotkey is empty! Auth will fail.")
        logger.warning("Using unsigned auth - will fail if server requires signatures")
        return {"X-Validator-Hotkey": self.validator_hotkey}

    async def _get_client(self) -> httpx.AsyncClient:
        """Get or create HTTP client."""
        if self._client is None:
            self._client = httpx.AsyncClient(timeout=self.timeout)
        return self._client

    async def _request(
        self,
        method: str,
        path: str,
        action: str = "request",
        timeout: Optional[float] = None,
        **kwargs,
    ) -> httpx.Response:
        """Make an authenticated request.

        Args:
            method: HTTP method
            path: Request path
            action: Action name for auth headers
            timeout: Optional timeout override (uses client default if None)
            **kwargs: Additional arguments passed to httpx.request
        """
        client = await self._get_client()
        headers = self._get_auth_headers(action)

        # Merge with any existing headers
        if "headers" in kwargs:
            headers.update(kwargs.pop("headers"))

        logger.info(f"_request {method} {path} headers={headers}")

        # Apply timeout override if specified
        request_kwargs = kwargs.copy()
        if timeout is not None:
            request_kwargs["timeout"] = timeout

        return await client.request(
            method,
            f"{self.base_url}{path}",
            headers=headers,
            **request_kwargs,
        )

    async def close(self):
        """Close the HTTP client."""
        if self._client:
            await self._client.aclose()
            self._client = None

    # =========================================================================
    # Score Submission
    # =========================================================================

    async def submit_scores(
        self,
        epoch: int,
        scores: List[ScoreSubmission],
    ) -> Dict[str, Any]:
        """
        Submit batch of orchestrator scores for an epoch.

        Args:
            epoch: The epoch number
            scores: List of ScoreSubmission objects

        Returns:
            Response dict with status and count
        """
        payload = {
            "validator_hotkey": self.validator_hotkey,
            "epoch": epoch,
            "scores": [asdict(s) for s in scores],
        }

        try:
            response = await self._request(
                "POST",
                "/validators/scores/submit",
                action="submit_scores",
                json=payload,
            )
            response.raise_for_status()
            result = response.json()
            logger.info(f"Submitted {len(scores)} scores for epoch {epoch}")
            return result

        except httpx.HTTPStatusError as e:
            logger.error(f"HTTP error submitting scores: {e.response.status_code} - {e.response.text}")
            raise
        except Exception as e:
            logger.error(f"Error submitting scores: {e}")
            raise

    async def submit_single_score(self, score: ScoreSubmission) -> Dict[str, Any]:
        """Submit a single orchestrator score."""
        return await self.submit_scores(score.epoch, [score])

    # =========================================================================
    # Spot Check Submission
    # =========================================================================

    async def submit_spot_check(self, check: SpotCheckSubmission) -> Dict[str, Any]:
        """
        Submit a spot check verification result.

        Args:
            check: SpotCheckSubmission object

        Returns:
            Response dict with status
        """
        try:
            response = await self._request(
                "POST",
                "/validators/spot-checks/submit",
                action="submit_spot_check",
                json=asdict(check),
            )
            response.raise_for_status()
            result = response.json()
            logger.debug(f"Submitted spot check for {check.orchestrator_hotkey[:16]}...")
            return result

        except httpx.HTTPStatusError as e:
            logger.error(f"HTTP error submitting spot check: {e.response.status_code}")
            raise
        except Exception as e:
            logger.error(f"Error submitting spot check: {e}")
            raise

    # =========================================================================
    # Fraud Penalty Submission
    # =========================================================================

    async def submit_fraud_penalty(self, penalty: FraudPenaltySubmission) -> Dict[str, Any]:
        """
        Submit a fraud penalty for an orchestrator.

        Args:
            penalty: FraudPenaltySubmission object

        Returns:
            Response dict with status
        """
        try:
            response = await self._request(
                "POST",
                "/validators/penalties/submit",
                action="submit_fraud_penalty",
                json=asdict(penalty),
            )
            response.raise_for_status()
            result = response.json()
            logger.warning(
                f"Submitted fraud penalty: type={penalty.fraud_type} "
                f"orchestrator={penalty.orchestrator_hotkey[:16]}..."
            )
            return result

        except httpx.HTTPStatusError as e:
            logger.error(f"HTTP error submitting fraud penalty: {e.response.status_code}")
            raise
        except Exception as e:
            logger.error(f"Error submitting fraud penalty: {e}")
            raise

    # =========================================================================
    # Challenge Result Submission
    # =========================================================================

    async def submit_challenge_result(self, result: ChallengeResultSubmission) -> Dict[str, Any]:
        """
        Submit a bandwidth challenge result.

        Args:
            result: ChallengeResultSubmission object

        Returns:
            Response dict with status
        """
        try:
            response = await self._request(
                "POST",
                "/validators/challenges/submit",
                action="submit_challenge_result",
                json=asdict(result),
            )
            response.raise_for_status()
            resp_data = response.json()
            logger.debug(f"Submitted challenge result: {result.challenge_id}")
            return resp_data

        except httpx.HTTPStatusError as e:
            logger.error(f"HTTP error submitting challenge result: {e.response.status_code}")
            raise
        except Exception as e:
            logger.error(f"Error submitting challenge result: {e}")
            raise

    # =========================================================================
    # Weight Proof Submission
    # =========================================================================

    async def submit_weight_proof(
        self,
        epoch: int,
        block_number: int,
        tx_hash: str,
        netuid: int,
        uids: List[int],
        weights: List[float],
        formula_version: Optional[str] = None,
        params_hash: Optional[str] = None,
    ) -> Dict[str, Any]:
        """
        Submit proof that weights were set on-chain.

        Args:
            epoch: Epoch the weights are for
            block_number: Chain block where weights were set
            tx_hash: Extrinsic hash (or block-based identifier)
            netuid: Subnet UID
            uids: UIDs that received weights
            weights: Normalized weights
            formula_version: Weight formula version (e.g. 'exposure_v1', 'legacy_v0')
            params_hash: SHA-256 of formula tunables

        Returns:
            Response dict with status
        """
        try:
            payload = {
                "epoch": epoch,
                "block_number": block_number,
                "tx_hash": tx_hash,
                "netuid": netuid,
                "uids": uids,
                "weights": [round(w, 6) for w in weights],
            }
            if formula_version is not None:
                payload["formula_version"] = formula_version
            if params_hash is not None:
                payload["params_hash"] = params_hash
            response = await self._request(
                "POST",
                "/validators/weights/proof",
                action="submit_weight_proof",
                json=payload,
            )
            response.raise_for_status()
            resp_data = response.json()
            logger.info(
                f"Submitted weight proof for epoch {epoch} "
                f"(block={block_number}, uids={len(uids)}, "
                f"formula={formula_version or 'unknown'})"
            )
            return resp_data

        except Exception as e:
            logger.error(f"Error submitting weight proof: {e}")
            raise

    # =========================================================================
    # Query Operations
    # =========================================================================

    async def get_epoch_scores(self, epoch: int) -> Dict[str, Any]:
        """
        Get all scores for an epoch.

        Args:
            epoch: The epoch number

        Returns:
            Dict with epoch, count, and scores list
        """
        try:
            response = await self._request(
                "GET",
                f"/validators/scores/{epoch}",
                action="get_epoch_scores",
            )
            response.raise_for_status()
            return response.json()

        except httpx.HTTPStatusError as e:
            logger.error(f"HTTP error fetching epoch scores: {e.response.status_code}")
            raise
        except Exception as e:
            logger.error(f"Error fetching epoch scores: {e}")
            raise

    async def get_orchestrator_penalties(
        self,
        orchestrator_hotkey: str,
        active_only: bool = True,
    ) -> Dict[str, Any]:
        """
        Get penalties for an orchestrator.

        Args:
            orchestrator_hotkey: The orchestrator's hotkey
            active_only: If True, only return active penalties

        Returns:
            Dict with orchestrator_hotkey, count, and penalties list
        """
        try:
            response = await self._request(
                "GET",
                f"/validators/penalties/{orchestrator_hotkey}",
                action="get_orchestrator_penalties",
                params={"active_only": active_only},
            )
            response.raise_for_status()
            return response.json()

        except httpx.HTTPStatusError as e:
            logger.error(f"HTTP error fetching penalties: {e.response.status_code}")
            raise
        except Exception as e:
            logger.error(f"Error fetching penalties: {e}")
            raise

    # =========================================================================
    # PoB Query Operations (for verification)
    # =========================================================================

    async def get_unverified_proofs(
        self,
        epoch: Optional[int] = None,
        orchestrator_hotkey: Optional[str] = None,
        limit: int = 100,
    ) -> Dict[str, Any]:
        """
        Get proofs that haven't been verified yet.

        Args:
            epoch: Filter by epoch (optional)
            orchestrator_hotkey: Filter by orchestrator (optional)
            limit: Max proofs to return

        Returns:
            Dict with count and proofs list
        """
        params = {"limit": limit}
        if epoch is not None:
            params["epoch"] = epoch
        if orchestrator_hotkey:
            params["orchestrator_hotkey"] = orchestrator_hotkey

        try:
            response = await self._request(
                "GET",
                "/pob/unverified",
                action="get_unverified_proofs",
                params=params,
            )
            response.raise_for_status()
            return response.json()

        except httpx.HTTPStatusError as e:
            logger.error(f"HTTP error fetching unverified proofs: {e.response.status_code}")
            raise
        except Exception as e:
            logger.error(f"Error fetching unverified proofs: {e}")
            raise

    async def verify_proof(
        self,
        task_id: str,
        passed: bool,
        signature_valid: bool = False,
        timing_valid: bool = False,
        bandwidth_valid: bool = False,
        canary_valid: bool = False,
        geo_valid: bool = False,
        verification_notes: Optional[str] = None,
        measured_latency_ms: Optional[float] = None,
    ) -> Dict[str, Any]:
        """
        Submit verification result for a proof.

        Args:
            task_id: Task ID of the proof
            passed: Overall verification passed
            signature_valid: Signature check passed
            timing_valid: Timing check passed
            bandwidth_valid: Bandwidth check passed
            canary_valid: Canary check passed
            geo_valid: Geographic check passed
            verification_notes: Optional notes
            measured_latency_ms: Measured latency

        Returns:
            Response dict with status
        """
        payload = {
            "task_id": task_id,
            "passed": passed,
            "signature_valid": signature_valid,
            "timing_valid": timing_valid,
            "bandwidth_valid": bandwidth_valid,
            "canary_valid": canary_valid,
            "geo_valid": geo_valid,
        }
        if verification_notes:
            payload["verification_notes"] = verification_notes
        if measured_latency_ms is not None:
            payload["measured_latency_ms"] = measured_latency_ms

        try:
            response = await self._request(
                "POST",
                "/pob/verify",
                action="verify_proof",
                json=payload,
            )
            response.raise_for_status()
            result = response.json()
            logger.debug(f"Verified proof {task_id[:16]}... passed={passed}")
            return result

        except httpx.HTTPStatusError as e:
            logger.error(f"HTTP error verifying proof: {e.response.status_code}")
            raise
        except Exception as e:
            logger.error(f"Error verifying proof: {e}")
            raise

    async def get_proof(self, task_id: str) -> Dict[str, Any]:
        """
        Get a specific proof by task ID.

        Args:
            task_id: The task ID

        Returns:
            Dict with proof data
        """
        try:
            response = await self._request(
                "GET",
                f"/pob/proof/{task_id}",
                action="get_proof",
            )
            response.raise_for_status()
            return response.json()

        except httpx.HTTPStatusError as e:
            logger.error(f"HTTP error fetching proof: {e.response.status_code}")
            raise
        except Exception as e:
            logger.error(f"Error fetching proof: {e}")
            raise

    async def get_orchestrator_pob_stats(
        self,
        orchestrator_hotkey: str,
        epoch: int,
    ) -> Dict[str, Any]:
        """
        Get aggregated PoB stats for an orchestrator in an epoch.

        Args:
            orchestrator_hotkey: Orchestrator's hotkey
            epoch: Epoch number

        Returns:
            Dict with stats
        """
        try:
            response = await self._request(
                "GET",
                f"/pob/stats/{orchestrator_hotkey}",
                action="get_orchestrator_pob_stats",
                params={"epoch": epoch},
            )
            response.raise_for_status()
            return response.json()

        except httpx.HTTPStatusError as e:
            logger.error(f"HTTP error fetching PoB stats: {e.response.status_code}")
            raise
        except Exception as e:
            logger.error(f"Error fetching PoB stats: {e}")
            raise

    async def get_latest_data_epoch(self) -> Optional[int]:
        """Get the latest epoch that has non-zero summary data."""
        try:
            response = await self._request(
                "GET",
                "/pob/latest-epoch",
                action="get_latest_data_epoch",
            )
            response.raise_for_status()
            data = response.json()
            return data.get("epoch")
        except Exception as e:
            logger.warning(f"Failed to get latest data epoch: {e}")
            return None

    async def get_epoch_summaries(self, epoch: int) -> Dict[str, Any]:
        """
        Get all orchestrator summaries for an epoch.

        Args:
            epoch: Epoch number

        Returns:
            Dict with epoch, count, and summaries list
        """
        try:
            response = await self._request(
                "GET",
                f"/pob/epoch/{epoch}/summaries",
                action="get_epoch_summaries",
            )
            response.raise_for_status()
            return response.json()

        except httpx.HTTPStatusError as e:
            logger.error(f"HTTP error fetching epoch summaries: {e.response.status_code}")
            raise
        except Exception as e:
            logger.error(f"Error fetching epoch summaries: {e}")
            raise

    # =========================================================================
    # Payment Verification
    # =========================================================================

    async def get_paid_proofs_for_pop_verification(
        self,
        epoch: Optional[int] = None,
        limit: int = 100,
    ) -> Dict[str, Any]:
        """
        Get paid PoB proofs with data needed for independent PoP verification.

        Returns proofs that have payment_status='paid' along with their
        tx_hash, pop_verified flag, transfer_id, and worker_coldkey.

        Validators can use this to independently verify ALPHA payments on-chain
        using AlphaPaymentVerifier from chain/tx_verifier.py.

        Args:
            epoch: Filter by epoch (optional)
            limit: Max proofs to return

        Returns:
            Dict with count and proofs list containing:
            - task_id: PoB task ID
            - tx_hash: On-chain transaction hash (extrinsic_hash:block_hash)
            - payment_status: Should be 'paid'
            - payment_amount: Amount in RAO
            - pop_verified: BeamCore's verification result (True/False/None)
            - pop_error: Error message if BeamCore verification failed
            - transfer_id: For memo verification
            - worker_hotkey: Worker's hotkey
            - worker_coldkey: Worker's coldkey (for recipient verification)
            - orchestrator_hotkey: Orchestrator who made payment
        """
        params = {"limit": limit}
        if epoch is not None:
            params["epoch"] = epoch

        try:
            response = await self._request(
                "GET",
                "/pob/paid",
                action="get_paid_proofs",
                params=params,
            )
            response.raise_for_status()
            return response.json()

        except httpx.HTTPStatusError as e:
            logger.error(f"HTTP error fetching paid proofs: {e.response.status_code}")
            return {"proofs": [], "count": 0}
        except Exception as e:
            logger.error(f"Error fetching paid proofs: {e}")
            return {"proofs": [], "count": 0}

    async def get_verified_proofs(
        self,
        limit: int = 100,
        seconds_ago: int = 300,
    ) -> Dict[str, Any]:
        """
        Get recently verified proofs.

        Args:
            limit: Max proofs to return
            seconds_ago: Look back time in seconds

        Returns:
            Dict with count and proofs list
        """
        try:
            response = await self._request(
                "GET",
                "/pob/verified",
                action="get_verified_proofs",
                params={"limit": limit, "seconds_ago": seconds_ago},
            )
            response.raise_for_status()
            return response.json()

        except httpx.HTTPStatusError as e:
            logger.error(f"HTTP error fetching verified proofs: {e.response.status_code}")
            return {"proofs": [], "count": 0}
        except Exception as e:
            logger.error(f"Error fetching verified proofs: {e}")
            return {"proofs": [], "count": 0}

    async def get_worker_payments(
        self,
        limit: int = 200,
        seconds_ago: int = 300,
    ) -> Dict[str, Any]:
        """
        Get recent worker payments.

        Args:
            limit: Max payments to return
            seconds_ago: Look back time in seconds

        Returns:
            Dict with count and payments list
        """
        try:
            response = await self._request(
                "GET",
                "/orchestrators/payments/recent",
                action="get_worker_payments",
                params={"limit": limit, "seconds_ago": seconds_ago},
            )
            response.raise_for_status()
            return response.json()

        except httpx.HTTPStatusError as e:
            logger.error(f"HTTP error fetching worker payments: {e.response.status_code}")
            return {"payments": [], "count": 0}
        except Exception as e:
            logger.error(f"Error fetching worker payments: {e}")
            return {"payments": [], "count": 0}

    # =========================================================================
    # Epoch Payment Verification
    # =========================================================================

    async def get_unverified_epoch_payments(
        self,
        limit: int = 50,
    ) -> Dict[str, Any]:
        """
        Get epoch payments that are submitted but not yet verified.

        Returns:
            Dict with count and payments list
        """
        try:
            response = await self._request(
                "GET",
                "/validators/payments/unverified",
                action="get_unverified_epoch_payments",
                params={"limit": limit},
            )
            response.raise_for_status()
            return response.json()

        except httpx.HTTPStatusError as e:
            logger.error(f"HTTP error fetching unverified epoch payments: {e.response.status_code}")
            return {"payments": [], "count": 0}
        except Exception as e:
            logger.error(f"Error fetching unverified epoch payments: {e}")
            return {"payments": [], "count": 0}

    async def verify_epoch_payment(
        self,
        orchestrator_hotkey: str,
        epoch: int,
    ) -> Dict[str, Any]:
        """
        Mark an epoch payment as verified.

        Args:
            orchestrator_hotkey: The orchestrator's hotkey
            epoch: The epoch number to verify

        Returns:
            Response dict with status
        """
        try:
            response = await self._request(
                "POST",
                "/validators/payments/verify",
                action="verify_epoch_payment",
                json={
                    "orchestrator_hotkey": orchestrator_hotkey,
                    "epoch": epoch,
                },
            )
            response.raise_for_status()
            return response.json()

        except httpx.HTTPStatusError as e:
            logger.error(
                f"HTTP error verifying epoch payment: {e.response.status_code} - {e.response.text}"
            )
            raise
        except Exception as e:
            logger.error(f"Error verifying epoch payment: {e}")
            raise

    # =========================================================================
    # Health Check
    # =========================================================================

    async def health_check(self) -> bool:
        """
        Check if BeamCore is healthy.

        Returns:
            True if healthy, False otherwise
        """
        client = await self._get_client()

        try:
            response = await client.get(f"{self.base_url}/health")
            return response.status_code == 200

        except Exception as e:
            logger.warning(f"Health check failed: {e}")
            return False

    # =========================================================================
    # Work Summaries (replaces direct orchestrator queries)
    # =========================================================================

    async def get_work_summaries(
        self,
        epoch: Optional[int] = None,
        orchestrator_hotkey: Optional[str] = None,
    ) -> Dict[str, Any]:
        """
        Get work summaries for scoring.

        This replaces direct orchestrator `/validators/summary` queries.
        SubnetCore aggregates data from all orchestrators.

        Args:
            epoch: Specific epoch (default: current)
            orchestrator_hotkey: Filter by orchestrator (optional)

        Returns:
            Dict with epoch, summaries list, and total_orchestrators
        """
        try:
            params = {}
            if epoch is not None:
                params["epoch"] = epoch
            if orchestrator_hotkey:
                params["orchestrator_hotkey"] = orchestrator_hotkey

            response = await self._request(
                "GET",
                "/validators/summaries",
                action="get_summaries",
                params=params,
            )
            response.raise_for_status()
            return response.json()

        except httpx.HTTPStatusError as e:
            logger.error(f"HTTP error fetching work summaries: {e.response.status_code}")
            return {"epoch": epoch or 0, "summaries": [], "total_orchestrators": 0}
        except Exception as e:
            logger.error(f"Error fetching work summaries: {e}")
            return {"epoch": epoch or 0, "summaries": [], "total_orchestrators": 0}

    async def get_orchestrators(
        self,
        active_only: bool = True,
        limit: int = 256,
    ) -> Dict[str, Any]:
        """
        Get list of orchestrators from SubnetCore.

        This provides orchestrator discovery without direct metagraph access.

        Args:
            active_only: Only return active orchestrators
            limit: Max results

        Returns:
            Dict with orchestrators list and total count
        """
        try:
            response = await self._request(
                "GET",
                "/validators/orchestrators",
                action="get_orchestrators",
                params={
                    "active_only": active_only,
                    "limit": limit,
                },
            )
            response.raise_for_status()
            return response.json()

        except httpx.HTTPStatusError as e:
            logger.error(f"HTTP error fetching orchestrators: {e.response.status_code}")
            return {"orchestrators": [], "total": 0}
        except Exception as e:
            logger.error(f"Error fetching orchestrators: {e}")
            return {"orchestrators": [], "total": 0}

    async def get_orchestrator_workers(
        self,
        orchestrator_hotkey: str,
        status: Optional[str] = None,
        limit: int = 100,
    ) -> Dict[str, Any]:
        """
        Get workers for a specific orchestrator.

        API: GET /validators/orchestrators/{hotkey}/workers
        Table: public.registered_workers

        Args:
            orchestrator_hotkey: The orchestrator's hotkey
            status: Filter by status (active, pending, suspended, offline, banned)
            limit: Max results

        Returns:
            Dict with workers list and count
        """
        try:
            params = {"limit": limit}
            if status:
                params["status"] = status

            response = await self._request(
                "GET",
                f"/validators/orchestrators/{orchestrator_hotkey}/workers",
                action="get_orchestrator_workers",
                params=params,
            )
            response.raise_for_status()
            return response.json()

        except httpx.HTTPStatusError as e:
            logger.error(f"HTTP error fetching orchestrator workers: {e.response.status_code}")
            return {"workers": [], "count": 0}
        except Exception as e:
            logger.error(f"Error fetching orchestrator workers: {e}")
            return {"workers": [], "count": 0}

    async def get_epoch_payments(
        self,
        epoch: int,
        orchestrator_hotkey: Optional[str] = None,
    ) -> Dict[str, Any]:
        """
        Get payment records for an epoch.

        API: GET /validators/epoch/{epoch}/payments
        Table: public.epoch_payments

        Args:
            epoch: The epoch number
            orchestrator_hotkey: Filter by orchestrator (optional)

        Returns:
            Dict with payments list and count
        """
        try:
            params = {}
            if orchestrator_hotkey:
                params["orchestrator_hotkey"] = orchestrator_hotkey

            response = await self._request(
                "GET",
                f"/validators/epoch/{epoch}/payments",
                action="get_epoch_payments",
                params=params if params else None,
            )
            response.raise_for_status()
            return response.json()

        except httpx.HTTPStatusError as e:
            logger.error(f"HTTP error fetching epoch payments: {e.response.status_code}")
            return {"payments": [], "count": 0}
        except Exception as e:
            logger.error(f"Error fetching epoch payments: {e}")
            return {"payments": [], "count": 0}

    async def get_available_epochs(self) -> Dict[str, Any]:
        """
        Get list of all epochs that have data in BeamCore.

        Returns epochs from epoch_summaries, epoch_payments, and pob_proofs tables,
        with counts for each data type.

        API: GET /validators/epochs

        Returns:
            Dict with total_epochs and epochs list containing:
            - epoch: epoch number
            - has_summaries: bool
            - summary_count: int
            - has_payments: bool
            - payment_count: int
            - has_proofs: bool
            - proof_count: int
        """
        try:
            response = await self._request(
                "GET",
                "/validators/epochs",
                action="get_available_epochs",
            )
            response.raise_for_status()
            return response.json()

        except httpx.HTTPStatusError as e:
            logger.error(f"HTTP error fetching available epochs: {e.response.status_code}")
            return {"epochs": [], "total_epochs": 0}
        except Exception as e:
            logger.error(f"Error fetching available epochs: {e}")
            return {"epochs": [], "total_epochs": 0}

    async def get_latest_epoch_with_summaries(self) -> Optional[int]:
        """
        Get the latest epoch that has summary data (for weight calculation).

        Returns:
            Latest epoch number with summaries, or None if none found
        """
        try:
            data = await self.get_available_epochs()
            epochs = data.get("epochs", [])

            # Find first epoch with summaries (list is sorted by epoch DESC)
            for epoch_info in epochs:
                if epoch_info.get("has_summaries"):
                    return epoch_info["epoch"]

            return None

        except Exception as e:
            logger.warning(f"Error getting latest epoch with summaries: {e}")
            return None

    async def get_proofs_from_subnetcore(
        self,
        epoch: Optional[int] = None,
        orchestrator_hotkey: Optional[str] = None,
        worker_id: Optional[str] = None,
        limit: int = 100,
        offset: int = 0,
    ) -> Dict[str, Any]:
        """
        Get proofs from SubnetCore.

        This replaces direct orchestrator `/validators/proofs` queries.

        Args:
            epoch: Filter by epoch
            orchestrator_hotkey: Filter by orchestrator
            worker_id: Filter by worker
            limit: Max results
            offset: Pagination offset

        Returns:
            Dict with proofs list and total count
        """
        try:
            params = {"limit": limit, "offset": offset}
            if epoch is not None:
                params["epoch"] = epoch
            if orchestrator_hotkey:
                params["orchestrator_hotkey"] = orchestrator_hotkey
            if worker_id:
                params["worker_id"] = worker_id

            response = await self._request(
                "GET",
                "/validators/proofs",
                action="get_proofs",
                params=params,
            )
            response.raise_for_status()
            return response.json()

        except httpx.HTTPStatusError as e:
            logger.error(f"HTTP error fetching proofs: {e.response.status_code}")
            return {"proofs": [], "total": 0}
        except Exception as e:
            logger.error(f"Error fetching proofs: {e}")
            return {"proofs": [], "total": 0}

    # =========================================================================
    # Challenge Coordination (replaces direct orchestrator challenges)
    # =========================================================================

    async def submit_challenge(
        self,
        orchestrator_uid: int,
        challenge_id: str,
        chunk_size: int,
        chunk_hash: str,
        canary: str,
        canary_offset: int,
        deadline_us: int,
        source_region: Optional[str] = None,
        dest_region: Optional[str] = None,
    ) -> Dict[str, Any]:
        """
        Submit a bandwidth challenge via SubnetCore.

        SubnetCore routes the challenge to the target orchestrator.

        Args:
            orchestrator_uid: Target orchestrator UID
            challenge_id: Unique challenge ID
            chunk_size: Challenge chunk size in bytes
            chunk_hash: SHA256 hash of chunk data
            canary: Hex-encoded canary bytes
            canary_offset: Canary position in chunk
            deadline_us: Challenge deadline (unix microseconds)
            source_region: Validator region
            dest_region: Target region

        Returns:
            Dict with success, challenge_id, accepted, worker_assigned, error
        """
        try:
            payload = {
                "orchestrator_uid": orchestrator_uid,
                "challenge_id": challenge_id,
                "chunk_size": chunk_size,
                "chunk_hash": chunk_hash,
                "canary": canary,
                "canary_offset": canary_offset,
                "deadline_us": deadline_us,
            }
            if source_region:
                payload["source_region"] = source_region
            if dest_region:
                payload["dest_region"] = dest_region

            # Use 45s timeout to cover BeamCore's 30s orchestrator forwarding timeout
            response = await self._request(
                "POST",
                "/validators/challenge",
                action="challenge",
                timeout=45.0,
                json=payload,
            )
            response.raise_for_status()
            return response.json()

        except httpx.HTTPStatusError as e:
            logger.error(f"HTTP error submitting challenge: {e.response.status_code} - {e.response.text[:200]}")
            logger.error(f"Challenge payload was: orchestrator_uid={orchestrator_uid}, challenge_id={challenge_id[:16]}...")
            return {
                "success": False,
                "challenge_id": challenge_id,
                "error": f"HTTP {e.response.status_code}: {e.response.text[:100]}",
            }
        except Exception as e:
            logger.error(f"Error submitting challenge: {type(e).__name__}: {e!r}")
            return {
                "success": False,
                "challenge_id": challenge_id,
                "error": f"{type(e).__name__}: {e}",
            }

    async def submit_challenge_data(
        self,
        challenge_id: str,
        chunk_data: str,  # Base64-encoded
    ) -> Dict[str, Any]:
        """
        Submit chunk data for an active challenge.

        SubnetCore forwards the data to the orchestrator.

        Args:
            challenge_id: Challenge ID
            chunk_data: Base64-encoded chunk data

        Returns:
            Dict with success, receive_time_us, bandwidth_mbps, canary_proof, error
        """
        logger.debug(f"submit_challenge_data called: challenge_id={challenge_id[:16]}... chunk_size={len(chunk_data)}")
        try:
            # Use 90s timeout to cover BeamCore's 60s orchestrator forwarding timeout
            response = await self._request(
                "POST",
                "/validators/challenge/data",
                action="challenge_data",
                timeout=90.0,
                json={
                    "challenge_id": challenge_id,
                    "chunk_data": chunk_data,
                },
            )
            response.raise_for_status()
            return response.json()

        except httpx.HTTPStatusError as e:
            logger.error(f"HTTP error submitting challenge data: {e.response.status_code} - {e.response.text[:200]}")
            return {
                "success": False,
                "challenge_id": challenge_id,
                "error": f"HTTP {e.response.status_code}: {e.response.text[:100]}",
            }
        except Exception as e:
            logger.error(f"Error submitting challenge data: {type(e).__name__}: {e!r}")
            return {
                "success": False,
                "challenge_id": challenge_id,
                "error": f"{type(e).__name__}: {e}",
            }

    async def get_challenge_status(self, challenge_id: str) -> Dict[str, Any]:
        """
        Get status of a challenge.

        Args:
            challenge_id: Challenge ID

        Returns:
            Dict with challenge status details
        """
        try:
            response = await self._request(
                "GET",
                f"/validators/challenge/{challenge_id}",
                action="get_challenge",
            )
            response.raise_for_status()
            return response.json()

        except httpx.HTTPStatusError as e:
            if e.response.status_code == 404:
                return {"error": "Challenge not found"}
            logger.error(f"HTTP error getting challenge status: {e.response.status_code}")
            return {"error": f"HTTP {e.response.status_code}"}
        except Exception as e:
            logger.error(f"Error getting challenge status: {e}")
            return {"error": str(e)}

    # =========================================================================
    # Weight Audit Polling & Response
    # =========================================================================

    async def get_pending_audits(self) -> Dict[str, Any]:
        """
        Poll for weight audits assigned to this validator.

        Returns:
            Dict with count and audits list
        """
        try:
            response = await self._request(
                "GET",
                "/validators/audits/pending",
                action="get_pending_audits",
            )
            response.raise_for_status()
            return response.json()

        except httpx.HTTPStatusError as e:
            logger.error(f"HTTP error fetching pending audits: {e.response.status_code}")
            return {"audits": [], "count": 0}
        except Exception as e:
            logger.error(f"Error fetching pending audits: {e}")
            return {"audits": [], "count": 0}

    async def submit_audit_response(
        self,
        audit_id: int,
        uids: List[int],
        weights: List[float],
    ) -> Dict[str, Any]:
        """
        Submit weight audit response.

        Args:
            audit_id: The audit ID to respond to
            uids: UIDs for the weight vector
            weights: Corresponding weight values

        Returns:
            Response dict with status
        """
        try:
            response = await self._request(
                "POST",
                "/validators/audits/submit",
                action="submit_audit_response",
                json={
                    "audit_id": audit_id,
                    "uids": uids,
                    "weights": [round(w, 6) for w in weights],
                },
            )
            response.raise_for_status()
            return response.json()

        except httpx.HTTPStatusError as e:
            logger.error(f"HTTP error submitting audit response: {e.response.status_code} - {e.response.text}")
            raise
        except Exception as e:
            logger.error(f"Error submitting audit response: {e}")
            raise

    async def get_recommended_weights(self, epoch: int) -> Dict[str, Any]:
        """
        Get recommended weights for an epoch from BeamCore.

        Args:
            epoch: The epoch number

        Returns:
            Dict with epoch, uids, weights, and scoring details
        """
        try:
            response = await self._request(
                "GET",
                f"/validators/weights/{epoch}",
                action="get_recommended_weights",
            )
            response.raise_for_status()
            return response.json()

        except httpx.HTTPStatusError as e:
            logger.error(f"HTTP error fetching recommended weights: {e.response.status_code}")
            raise
        except Exception as e:
            logger.error(f"Error fetching recommended weights: {e}")
            raise

    # =========================================================================
    # Configuration
    # =========================================================================

    async def get_uid_ranges(self) -> Optional[UIDRanges]:
        """
        Get orchestrator UID range configuration from BeamCore.

        This is the authoritative source for UID ranges. The validator should
        use these ranges to determine which orchestrator UIDs are valid.

        Returns:
            UIDRanges object or None if fetch failed
        """
        client = await self._get_client()

        try:
            response = await client.get(f"{self.base_url}/config/uid-ranges")
            response.raise_for_status()
            data = response.json()

            uid_ranges = UIDRanges(
                subnet_orchestrator_uid=data["subnet_orchestrator_uid"],
                public_orchestrator_uid_start=data["public_orchestrator_uid_start"],
                public_orchestrator_uid_end=data["public_orchestrator_uid_end"],
                reserved_orchestrator_uid_start=data["reserved_orchestrator_uid_start"],
                reserved_orchestrator_uid_end=data["reserved_orchestrator_uid_end"],
                max_orchestrators=data["max_orchestrators"],
            )

            logger.info(
                f"Fetched UID ranges: public={uid_ranges.public_orchestrator_uid_start}-"
                f"{uid_ranges.public_orchestrator_uid_end}, "
                f"reserved={uid_ranges.reserved_orchestrator_uid_start}-"
                f"{uid_ranges.reserved_orchestrator_uid_end}"
            )

            return uid_ranges

        except httpx.HTTPStatusError as e:
            logger.error(f"HTTP error fetching UID ranges: {e.response.status_code}")
            return None
        except Exception as e:
            logger.error(f"Error fetching UID ranges: {e}")
            return None

    async def get_network_config(self) -> Optional[dict]:
        """Fetch network config from SubnetCore (authoritative source for scoring params)."""
        client = await self._get_client()
        try:
            response = await client.get(f"{self.base_url}/config/network")
            response.raise_for_status()
            return response.json()
        except Exception as e:
            logger.warning(f"Error fetching network config: {e}")
            return None

    # =========================================================================
    # Heartbeat / Offline Beacon
    # =========================================================================

    async def submit_heartbeat(
        self,
        validator_uid: int,
        status: str = "online",
        last_epoch_scored: Optional[int] = None,
        health_info: Optional[dict] = None,
        external_url: Optional[str] = None,
    ) -> Dict[str, Any]:
        """
        Submit a heartbeat to indicate the validator is online.

        Should be called every 60 seconds while the validator is running.

        Args:
            validator_uid: This validator's UID
            status: Current status (online, degraded)
            last_epoch_scored: Last epoch this validator scored
            health_info: Optional health details
            external_url: Public-facing URL if behind proxy

        Returns:
            Response dict with status
        """
        import time

        payload = {
            "validator_hotkey": self.validator_hotkey,
            "validator_uid": validator_uid,
            "status": status,
            "timestamp": int(time.time() * 1e6),  # microseconds
            "last_epoch_scored": last_epoch_scored,
            "health_info": health_info,
            "external_url": external_url,
        }

        try:
            response = await self._request(
                "POST",
                "/validators/heartbeat",
                action="heartbeat",
                json=payload,
            )
            response.raise_for_status()
            return response.json()

        except httpx.HTTPStatusError as e:
            logger.warning(f"Heartbeat failed: {e.response.status_code}")
            raise
        except Exception as e:
            logger.warning(f"Heartbeat error: {e}")
            raise

    async def submit_offline_beacon(
        self,
        reason: str = "graceful_shutdown",
    ) -> Dict[str, Any]:
        """
        Submit an offline beacon during graceful shutdown.

        Should be called in the validator's stop() method before cleanup.

        Args:
            reason: Reason for going offline (graceful_shutdown, error, etc.)

        Returns:
            Response dict with status
        """
        payload = {
            "validator_hotkey": self.validator_hotkey,
            "reason": reason,
        }

        try:
            response = await self._request(
                "POST",
                "/validators/offline",
                action="offline_beacon",
                json=payload,
            )
            response.raise_for_status()
            logger.info(f"Offline beacon sent: {reason}")
            return response.json()

        except httpx.HTTPStatusError as e:
            logger.warning(f"Offline beacon failed: {e.response.status_code}")
            raise
        except Exception as e:
            logger.warning(f"Offline beacon error: {e}")
            raise


# =============================================================================
# Global Client Instance
# =============================================================================

_client: Optional[SubnetCoreClient] = None


def get_subnet_core_client() -> Optional[SubnetCoreClient]:
    """Get the global SubnetCoreClient instance."""
    return _client


def init_subnet_core_client(
    base_url: str,
    validator_hotkey: str,
    wallet=None,
    timeout: float = 30.0,
) -> SubnetCoreClient:
    """
    Initialize the global SubnetCoreClient instance.

    Args:
        base_url: Base URL of BeamCore
        validator_hotkey: This validator's hotkey
        wallet: Bittensor wallet for signed auth (optional)
        timeout: Request timeout

    Returns:
        The initialized client
    """
    global _client
    _client = SubnetCoreClient(base_url, validator_hotkey, wallet, timeout)
    logger.info(f"SubnetCoreClient initialized: {base_url} (signed_auth={'enabled' if wallet else 'disabled'}, hotkey={validator_hotkey[:16] if validator_hotkey else 'EMPTY'}...)")
    return _client


async def close_subnet_core_client():
    """Close the global client."""
    global _client
    if _client:
        await _client.close()
        _client = None
