"""Domain and public response models owned by the orchestrator."""

from dataclasses import dataclass, field
from datetime import datetime
from enum import Enum
from typing import Dict, Optional


class WorkerStatus(Enum):
    """Worker lifecycle status."""

    PENDING = "pending"
    ACTIVE = "active"
    SUSPENDED = "suspended"
    OFFLINE = "offline"
    BANNED = "banned"


@dataclass
class Worker:
    """Registered off-chain worker and its performance counters."""

    worker_id: str
    hotkey: str
    ip: str
    port: int
    region: str
    latitude: Optional[float] = None
    longitude: Optional[float] = None
    status: WorkerStatus = WorkerStatus.PENDING
    registered_at: datetime = field(default_factory=datetime.utcnow)
    last_seen: datetime = field(default_factory=datetime.utcnow)
    bandwidth_mbps: float = 0.0
    bandwidth_ema: float = 0.0
    latency_ms: float = 0.0
    success_rate: float = 1.0
    total_tasks: int = 0
    successful_tasks: int = 0
    failed_tasks: int = 0
    active_tasks: int = 0
    global_pending_tasks: int = 0
    max_concurrent_tasks: int = 10
    bytes_relayed_total: int = 0
    bytes_relayed_epoch: int = 0
    trust_score: float = 0.5
    fraud_score: float = 0.0
    rewards_earned_epoch: int = 0
    rewards_earned_total: int = 0

    @property
    def is_available(self) -> bool:
        return (
            self.status == WorkerStatus.ACTIVE
            and self.active_tasks < self.max_concurrent_tasks
        )

    @property
    def load_factor(self) -> float:
        if self.max_concurrent_tasks == 0:
            return 1.0
        effective_tasks = max(self.active_tasks, self.global_pending_tasks)
        return effective_tasks / self.max_concurrent_tasks

    def update_bandwidth_ema(self, bandwidth: float, alpha: float = 0.3) -> None:
        if self.bandwidth_ema == 0:
            self.bandwidth_ema = bandwidth
        else:
            self.bandwidth_ema = (
                alpha * bandwidth + (1 - alpha) * self.bandwidth_ema
            )

    def update_success_rate(self) -> None:
        if self.total_tasks > 0:
            self.success_rate = self.successful_tasks / self.total_tasks


@dataclass
class BandwidthTask:
    """A bandwidth relay task assigned to a worker."""

    task_id: str
    worker_id: str
    chunk_size: int
    chunk_hash: str
    source_region: str
    dest_region: str
    created_at: float
    deadline_us: int
    started_at: Optional[float] = None
    completed_at: Optional[float] = None
    status: str = "pending"
    canary: bytes = field(default_factory=bytes)
    canary_offset: int = 0
    bytes_relayed: int = 0
    bandwidth_mbps: float = 0.0
    latency_ms: float = 0.0


@dataclass
class EpochSummary:
    """Aggregated work summary for a validation epoch."""

    epoch: int
    start_time: datetime
    end_time: datetime
    total_tasks: int = 0
    successful_tasks: int = 0
    failed_tasks: int = 0
    total_bytes_relayed: int = 0
    total_bandwidth_seconds: float = 0.0
    active_workers: int = 0
    worker_contributions: Dict[str, int] = field(default_factory=dict)
    proof_count: int = 0
    merkle_root: str = ""
    avg_bandwidth_mbps: float = 0.0
    avg_latency_ms: float = 0.0
    success_rate: float = 0.0
