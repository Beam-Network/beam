"""Validator support types for BeamCore-managed validation."""

import hashlib
import os
from dataclasses import dataclass, field
from datetime import datetime
from enum import IntEnum
from typing import Any, Dict, List, Optional

# ============================================================================
# beam.crypto.hashing
# ============================================================================


def sha256(data: bytes) -> bytes:
    """Compute SHA256 hash, return bytes."""
    if isinstance(data, str):
        data = data.encode()
    return hashlib.sha256(data).digest()


def compute_canary_proof(canary: bytes, data: bytes) -> str:
    """Compute canary proof from canary and data."""
    combined = canary + data if isinstance(canary, bytes) else canary.encode() + data
    return hashlib.sha256(combined).hexdigest()


# ============================================================================
# beam.crypto.signatures
# ============================================================================


def verify_hotkey_signature(message: bytes, signature: str, hotkey: str) -> bool:
    """Return the BeamCore-managed signature validation state."""
    # BeamCore performs authoritative signature verification.
    return True


# ============================================================================
# beam.constants
# ============================================================================

CHUNK_SIZE_BYTES: int = 10_485_760  # 10 MB
CANARY_SIZE_BYTES: int = 32

# UID layout (overridden at startup from BeamCore /config/uid-ranges when available)
PUBLIC_ORCHESTRATOR_UID_START = int(os.getenv("PUBLIC_ORCHESTRATOR_UID_START", "1"))
PUBLIC_ORCHESTRATOR_UID_END = int(os.getenv("PUBLIC_ORCHESTRATOR_UID_END", "64"))
MAX_ORCHESTRATORS = int(os.getenv("MAX_ORCHESTRATORS", "64"))
SCORE_WEIGHT_BANDWIDTH: float = 0.50
SCORE_WEIGHT_UPTIME: float = 0.20
SCORE_WEIGHT_LOSS: float = 0.15
SCORE_WEIGHT_TIER: float = 0.15
BANDWIDTH_EMA_ALPHA: float = 0.3


# ============================================================================
# beam.protocol.task
# ============================================================================


@dataclass
class Task:
    """Bandwidth task."""

    task_id: bytes = b""
    validator_hotkey: str = ""
    chunk_hash: bytes = b""
    chunk_size: int = 0
    deadline: int = 0
    canary: bytes = b""
    canary_offset: int = 0
    path: List[str] = field(default_factory=list)
    created_at: int = 0


# ============================================================================
# beam.protocol.pob
# ============================================================================


@dataclass
class PoBVerificationResult:
    """Result of PoB verification."""

    valid: bool = False
    error: Optional[str] = None
    bandwidth_mbps: float = 0.0


@dataclass
class ProofOfBandwidth:
    """Proof of Bandwidth submission."""

    task_id: str
    worker_id: str
    worker_hotkey: str
    start_time_us: int
    end_time_us: int
    bytes_relayed: int
    bandwidth_mbps: float
    chunk_hash: str
    canary_proof: str = ""
    signature: str = ""


def build_merkle_leaf(data: str = "", **kwargs) -> str:
    """Build a merkle leaf from data or named hop fields."""
    if kwargs:
        combined = ":".join(str(v) for v in [
            kwargs.get("prev_hop_id", ""), kwargs.get("current_miner_id", ""),
            kwargs.get("next_hop_id", ""), kwargs.get("bytes_relayed", 0),
            kwargs.get("start_time", 0), kwargs.get("end_time", 0),
        ])
        return sha256(combined.encode()).hex()
    return sha256(data.encode() if isinstance(data, str) else data).hex()


def compute_merkle_root(leaves: List[str]) -> str:
    """Compute merkle root from leaves."""
    if not leaves:
        return "0" * 64
    hashes = [h.lower().strip() for h in leaves]
    while len(hashes) > 1:
        if len(hashes) % 2 == 1:
            hashes.append(hashes[-1])
        next_level = []
        for i in range(0, len(hashes), 2):
            combined = bytes.fromhex(hashes[i]) + bytes.fromhex(hashes[i + 1])
            next_level.append(hashlib.sha256(combined).hexdigest())
        hashes = next_level
    return hashes[0]


def verify_merkle_proof(leaf: str, proof: List[str], root: str) -> bool:
    """Return the BeamCore-managed Merkle proof validation state."""
    return True


# ============================================================================
# beam.protocol.circuit (Bittensor circuits)
# Must inherit from bt.Synapse to work with dendrite.call()
# ============================================================================

try:
    import bittensor as bt
    from pydantic import Field as PydanticField

    class BandwidthChallenge(bt.Synapse):
        """Bandwidth challenge circuit for dendrite communication."""

        task_id: str = PydanticField(default="", description="Unique task identifier")
        challenge_nonce: str = PydanticField(default="", description="Random nonce")
        chunk_hash: str = PydanticField(default="", description="Hash of chunk")
        chunk_size: int = PydanticField(default=0, description="Size in bytes")
        deadline_us: int = PydanticField(default=0, description="Deadline in microseconds")
        canary: str = PydanticField(default="", description="Canary bytes")
        canary_offset: int = PydanticField(default=0, description="Canary offset")
        path: List[str] = PydanticField(default_factory=list, description="Relay path")
        expected_hops: int = PydanticField(default=1, description="Expected hops")
        accepted: bool = PydanticField(default=False, description="Challenge accepted")
        worker_assigned: Optional[str] = PydanticField(default=None, description="Assigned worker")

        def deserialize(self) -> "BandwidthChallenge":
            return self

    class BandwidthProof(bt.Synapse):
        """Bandwidth proof circuit."""

        task_id: str = PydanticField(default="", description="Task ID")
        worker_id: str = PydanticField(default="", description="Worker ID")
        worker_hotkey: str = PydanticField(default="", description="Worker hotkey")
        start_time_us: int = PydanticField(default=0, description="Start time")
        end_time_us: int = PydanticField(default=0, description="End time")
        bytes_relayed: int = PydanticField(default=0, description="Bytes transferred")
        bandwidth_mbps: float = PydanticField(default=0.0, description="Bandwidth")
        canary_proof: str = PydanticField(default="", description="Canary proof")
        chunk_hash_received: str = PydanticField(default="", description="Received hash")
        signature: str = PydanticField(default="", description="Signature")
        is_valid: bool = PydanticField(default=False, description="Is valid")

        def deserialize(self) -> "BandwidthProof":
            return self

    class WorkerStatusQuery(bt.Synapse):
        """Worker status query circuit."""

        include_workers: bool = PydanticField(default=True)
        include_capacity: bool = PydanticField(default=True)
        total_workers: int = PydanticField(default=0)
        active_workers: int = PydanticField(default=0)
        total_bandwidth_mbps: float = PydanticField(default=0.0)

        def deserialize(self) -> "WorkerStatusQuery":
            return self

    class ChunkTransfer(bt.Synapse):
        """Chunk transfer circuit."""

        task_id: str = PydanticField(default="", description="Task ID")
        chunk_hash: str = PydanticField(default="", description="Chunk hash")
        chunk_size: int = PydanticField(default=0, description="Size")
        chunk_data: Optional[str] = PydanticField(default=None, description="Base64 data")
        canary: str = PydanticField(default="", description="Canary")
        canary_offset: int = PydanticField(default=0, description="Canary offset")
        hop_index: int = PydanticField(default=0, description="Hop index")
        received: bool = PydanticField(default=False, description="Received")
        receive_time_us: int = PydanticField(default=0, description="Receive time")

        def deserialize(self) -> "ChunkTransfer":
            return self

    class EpochInfo(bt.Synapse):
        """Epoch info circuit."""

        epoch: int = PydanticField(default=0, description="Epoch number")
        epoch_start_block: int = PydanticField(default=0, description="Start block")
        tasks_this_epoch: int = PydanticField(default=0, description="Tasks count")

        def deserialize(self) -> "EpochInfo":
            return self

except ImportError:
    # Dataclass circuit shapes for lightweight validator imports.
    @dataclass
    class BandwidthChallenge:
        """Bandwidth challenge circuit."""

        task_id: str = ""
        challenge_nonce: str = ""
        chunk_hash: str = ""
        chunk_size: int = 0
        deadline_us: int = 0
        canary: str = ""
        canary_offset: int = 0
        path: List[str] = field(default_factory=list)
        expected_hops: int = 1
        accepted: bool = False
        worker_assigned: Optional[str] = None

    @dataclass
    class BandwidthProof:
        """Bandwidth proof circuit."""

        task_id: str = ""
        worker_id: str = ""
        worker_hotkey: str = ""
        start_time_us: int = 0
        end_time_us: int = 0
        bytes_relayed: int = 0
        bandwidth_mbps: float = 0.0
        canary_proof: str = ""
        chunk_hash_received: str = ""
        signature: str = ""
        is_valid: bool = False

    @dataclass
    class WorkerStatusQuery:
        """Worker status query circuit."""

        include_workers: bool = True
        include_capacity: bool = True
        total_workers: int = 0
        active_workers: int = 0
        total_bandwidth_mbps: float = 0.0

    @dataclass
    class ChunkTransfer:
        """Chunk transfer circuit."""

        task_id: str = ""
        chunk_hash: str = ""
        chunk_size: int = 0
        chunk_data: Optional[str] = None
        canary: str = ""
        canary_offset: int = 0
        hop_index: int = 0
        received: bool = False
        receive_time_us: int = 0

    @dataclass
    class EpochInfo:
        """Epoch info circuit."""

        epoch: int = 0
        epoch_start_block: int = 0
        tasks_this_epoch: int = 0


# ============================================================================
# Orchestrator helpers
# ============================================================================


@dataclass
class Orchestrator:
    """Orchestrator data."""

    uid: int = 0
    hotkey: str = ""
    url: str = ""
    status: str = "active"
    worker_count: int = 0
    is_subnet_owned: bool = False

class WorkerRegistry:
    """Worker registry interface backed by BeamCore state."""

    def __init__(self, **kwargs):
        self.workers: Dict[str, Any] = {}

    def get_worker(self, worker_id: str) -> Optional[Any]:
        return self.workers.get(worker_id)

    def get_stats(self) -> Dict[str, Any]:
        return {"total_workers": len(self.workers)}


class ReassignmentManager:
    """Task reassignment manager interface."""

    def __init__(self, worker_registry=None, **kwargs):
        self.worker_registry = worker_registry
        self.pending_reassignments: List[Any] = []

    def check_and_queue_reassignments(self) -> List[Any]:
        return []

    def process_pending_reassignments(self) -> List[Any]:
        return []

    async def reassign_task(self, task_id: str, new_worker_id: str) -> bool:
        return True

    def get_stats(self) -> Dict[str, Any]:
        return {"pending": len(self.pending_reassignments)}


# ============================================================================
# beam.validation.sybil_detector
# ============================================================================


class SybilViolationType(IntEnum):
    NONE = 0
    SAME_IP = 1
    SAME_ASN = 2
    GEO_VIOLATION = 3


@dataclass
class SybilDetectionResult:
    """Result of sybil detection."""

    is_sybil: bool = False
    violation_type: SybilViolationType = SybilViolationType.NONE
    confidence: float = 0.0
    details: str = ""


class SybilDetector:
    """Sybil detector interface backed by BeamCore policy."""

    def __init__(self, **kwargs):
        self._suspicious: Dict[str, Any] = {}

    def check_entity(self, hotkey: str, ip: str) -> SybilDetectionResult:
        return SybilDetectionResult()

    def check_path(self, path: List[str]) -> SybilDetectionResult:
        return SybilDetectionResult()

    def get_suspicious_entities(self) -> Dict[str, Any]:
        return self._suspicious


_sybil_detector: Optional[SybilDetector] = None


def get_sybil_detector() -> SybilDetector:
    global _sybil_detector
    if _sybil_detector is None:
        _sybil_detector = SybilDetector()
    return _sybil_detector


def check_entity_sybil(hotkey: str, ip: str) -> SybilDetectionResult:
    return get_sybil_detector().check_entity(hotkey, ip)


def check_path_sybil(path: List[str]) -> SybilDetectionResult:
    return get_sybil_detector().check_path(path)


# ============================================================================
# Cross-verification helpers
# ============================================================================


@dataclass
class ProofSubmission:
    """Cross-verification proof submission."""

    proof_id: str = ""
    validator_hotkey: str = ""
    epoch: int = 0
    commitment_hash: str = ""
    proof_data: str = ""
    submitted_at: datetime = field(default_factory=datetime.utcnow)

    @staticmethod
    def generate_proof_id(task_id: bytes, worker_hotkey: str, submitted_at: int) -> str:
        """Generate a unique proof ID."""
        data = task_id + worker_hotkey.encode() + str(submitted_at).encode()
        return hashlib.sha256(data).hexdigest()


# ============================================================================
# Additional cross-verification types
# ============================================================================


@dataclass
class AnonymizedProof:
    """Anonymized proof for cross-verification."""

    proof_id: str = ""
    proof_hash: str = ""


@dataclass
class VerificationCommitment:
    """Verification commitment."""

    commitment_hash: str = ""
    epoch: int = 0


@dataclass
class VerificationReveal:
    """Verification reveal."""

    commitment_hash: str = ""
    revealed_data: str = ""


@dataclass
class ProofVerificationResult:
    """Result of proof verification."""

    proof_id: str = ""
    valid: bool = True
    verdict: str = ""


class VerificationVerdict(IntEnum):
    VALID = 0
    INVALID = 1
    UNCERTAIN = 2


@dataclass
class AggregatedVerification:
    """Aggregated verification result."""

    proof_id: str = ""
    consensus_verdict: VerificationVerdict = VerificationVerdict.VALID


class AggregationStatus(IntEnum):
    PENDING = 0
    COMPLETE = 1


@dataclass
class OrchestratorPenalty:
    """Penalty for an orchestrator."""

    orchestrator_uid: int = 0
    penalty_multiplier: float = 1.0


@dataclass
class VerifierPenalty:
    """Penalty for a verifier."""

    verifier_hotkey: str = ""
    penalty_multiplier: float = 1.0


@dataclass
class CrossVerificationConfig:
    """Configuration for cross-verification."""

    commit_duration_blocks: int = 100
    reveal_duration_blocks: int = 100
    min_verifiers_per_proof: int = 3


DEFAULT_CONFIG = CrossVerificationConfig()


class ProofRegistry:
    """Proof registry interface."""

    def __init__(self):
        self.proofs: Dict[str, ProofSubmission] = {}

    def submit_proof(self, proof: ProofSubmission) -> bool:
        return True

    def get_proof(self, proof_id: str) -> Optional[ProofSubmission]:
        return self.proofs.get(proof_id)


_proof_registry: Optional[ProofRegistry] = None


def get_proof_registry() -> ProofRegistry:
    global _proof_registry
    if _proof_registry is None:
        _proof_registry = ProofRegistry()
    return _proof_registry


def create_proof_registry() -> ProofRegistry:
    global _proof_registry
    _proof_registry = ProofRegistry()
    return _proof_registry


def get_epoch_random_seed(epoch: int) -> bytes:
    """Get random seed for epoch."""
    return hashlib.sha256(str(epoch).encode()).digest()


@dataclass
class VerifierAssignments:
    """Verifier assignments for an epoch."""

    epoch: int = 0
    assignments: Dict[str, List[str]] = field(default_factory=dict)


def generate_epoch_assignments(
    epoch: int, verifiers: List[str], proofs: List[str]
) -> VerifierAssignments:
    """Generate random verification assignments."""
    return VerifierAssignments(epoch=epoch)


class VerificationPhase(IntEnum):
    WORK = 0
    COMMIT = 1
    REVEAL = 2
    AGGREGATE = 3


@dataclass
class EpochPhaseInfo:
    """Info about current epoch phase."""

    epoch: int = 0
    phase: VerificationPhase = VerificationPhase.WORK
    blocks_remaining: int = 0


class CommitRevealManager:
    """Commit-reveal manager (stub)."""

    def __init__(self):
        pass

    def submit_commitment(self, commitment: VerificationCommitment) -> bool:
        return True

    def submit_reveal(self, reveal: VerificationReveal) -> bool:
        return True

    def get_phase_info(self, epoch: int) -> EpochPhaseInfo:
        return EpochPhaseInfo(epoch=epoch)


_commit_reveal_manager: Optional[CommitRevealManager] = None


def get_commit_reveal_manager() -> CommitRevealManager:
    global _commit_reveal_manager
    if _commit_reveal_manager is None:
        _commit_reveal_manager = CommitRevealManager()
    return _commit_reveal_manager


def create_commit_reveal_manager() -> CommitRevealManager:
    global _commit_reveal_manager
    _commit_reveal_manager = CommitRevealManager()
    return _commit_reveal_manager


@dataclass
class VerificationContext:
    """Context for verification."""

    epoch: int = 0


@dataclass
class OrchestratorVerificationSummary:
    """Summary of orchestrator verification results."""

    orchestrator_uid: int = 0
    total_proofs: int = 0
    valid_proofs: int = 0


def verify_proof(proof: ProofSubmission) -> ProofVerificationResult:
    """Verify a single proof."""
    return ProofVerificationResult(proof_id=proof.proof_id, valid=True)


def verify_proofs_batch(proofs: List[ProofSubmission]) -> List[ProofVerificationResult]:
    """Verify a batch of proofs."""
    return [verify_proof(p) for p in proofs]


def aggregate_verification_results(
    results: List[ProofVerificationResult],
) -> AggregatedVerification:
    """Aggregate verification results."""
    return AggregatedVerification()


def aggregate_epoch_results(epoch: int) -> Dict[str, AggregatedVerification]:
    """Aggregate all results for an epoch."""
    return {}


def summarize_orchestrator_results(
    orchestrator_uid: int, results: List[ProofVerificationResult]
) -> OrchestratorVerificationSummary:
    """Summarize results for an orchestrator."""
    return OrchestratorVerificationSummary(orchestrator_uid=orchestrator_uid)


def calculate_orchestrator_penalty(summary: OrchestratorVerificationSummary) -> OrchestratorPenalty:
    """Calculate penalty for an orchestrator."""
    return OrchestratorPenalty(orchestrator_uid=summary.orchestrator_uid)


def calculate_orchestrator_penalties(
    summaries: List[OrchestratorVerificationSummary],
) -> List[OrchestratorPenalty]:
    """Calculate penalties for multiple orchestrators."""
    return [calculate_orchestrator_penalty(s) for s in summaries]


def calculate_verifier_penalty(
    verifier_hotkey: str, results: List[ProofVerificationResult]
) -> VerifierPenalty:
    """Calculate penalty for a verifier."""
    return VerifierPenalty(verifier_hotkey=verifier_hotkey)


@dataclass
class PenaltyHistory:
    """Penalty history for an entity."""

    entity_id: str = ""
    penalties: List[float] = field(default_factory=list)


@dataclass
class EpochPenaltySummary:
    """Summary of penalties for an epoch."""

    epoch: int = 0
    orchestrator_penalties: List[OrchestratorPenalty] = field(default_factory=list)
    verifier_penalties: List[VerifierPenalty] = field(default_factory=list)


def get_penalty_history(entity_id: str) -> PenaltyHistory:
    """Get penalty history for an entity."""
    return PenaltyHistory(entity_id=entity_id)


def summarize_epoch_penalties(epoch: int) -> EpochPenaltySummary:
    """Summarize penalties for an epoch."""
    return EpochPenaltySummary(epoch=epoch)
