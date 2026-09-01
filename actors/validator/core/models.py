"""State and result models shared by validator services."""

from dataclasses import dataclass, field
from datetime import datetime
from typing import Dict, List, Optional


@dataclass
class OrchestratorInfo:
    """Information about an Orchestrator endpoint."""

    url: str
    hotkey: str
    uid: Optional[int] = None
    last_seen: datetime = field(default_factory=datetime.utcnow)
    registered_at: Optional[datetime] = None
    is_healthy: bool = True
    is_subnet_owned: bool = False
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
    uptime_percent: float = 100.0
    latency_p95_ms: float = 0.0
    worker_contributions: Optional[Dict[str, int]] = None
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
    signature_valid: bool = False
    timing_valid: bool = False
    bandwidth_valid: bool = False
    canary_valid: bool = False
    geo_valid: bool = False
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
    invalid_proof_ids: List[str] = field(default_factory=list)
    invalid_reasons: Dict[str, str] = field(default_factory=dict)
    fraud_detected: bool = False
    fraud_severity: float = 0.0
    timestamp: datetime = field(default_factory=datetime.utcnow)


@dataclass
class PoBVerificationStats:
    """Aggregated PoB verification stats for one orchestrator and epoch."""

    total_proofs: int = 0
    verified_count: int = 0
    rejected_count: int = 0
    total_bytes_verified: int = 0
