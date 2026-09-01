"""Value objects shared by validator redundancy services."""

from dataclasses import dataclass, field
from datetime import datetime
from enum import Enum
from typing import Any, Dict, List, Optional

from core.redundancy_config import MAX_CHECKPOINTS_KEPT


class HealthStatus(Enum):
    """Validator health status levels"""

    HEALTHY = "healthy"
    DEGRADED = "degraded"  # Some issues but functional
    UNHEALTHY = "unhealthy"  # Significant issues
    CRITICAL = "critical"  # Unable to function properly
    RECOVERING = "recovering"  # Attempting recovery


class ComponentHealth(Enum):
    """Health status of individual components"""

    OK = "ok"
    WARNING = "warning"
    ERROR = "error"
    UNKNOWN = "unknown"


@dataclass
class HealthCheck:
    """Result of a single health check"""

    component: str
    status: ComponentHealth
    message: str = ""
    latency_ms: float = 0.0
    checked_at: datetime = field(default_factory=datetime.utcnow)

    def to_dict(self) -> Dict[str, Any]:
        return {
            "component": self.component,
            "status": self.status.value,
            "message": self.message,
            "latency_ms": self.latency_ms,
            "checked_at": self.checked_at.isoformat(),
        }


@dataclass
class HealthReport:
    """Complete health report for the validator"""

    status: HealthStatus
    checks: List[HealthCheck] = field(default_factory=list)
    consecutive_failures: int = 0
    last_healthy: Optional[datetime] = None
    uptime_seconds: float = 0.0
    report_time: datetime = field(default_factory=datetime.utcnow)

    def to_dict(self) -> Dict[str, Any]:
        return {
            "status": self.status.value,
            "checks": [c.to_dict() for c in self.checks],
            "consecutive_failures": self.consecutive_failures,
            "last_healthy": self.last_healthy.isoformat() if self.last_healthy else None,
            "uptime_seconds": self.uptime_seconds,
            "report_time": self.report_time.isoformat(),
        }


@dataclass
class ValidatorCheckpoint:
    """Checkpoint of validator state for recovery"""

    version: str = "1.0"
    created_at: datetime = field(default_factory=datetime.utcnow)

    # Validator identity
    hotkey: str = ""
    uid: Optional[int] = None

    # Weight history
    last_weight_block: int = 0
    weights_history: List[Dict] = field(default_factory=list)

    # Scoring state
    orchestrator_metrics: Dict[int, Dict] = field(default_factory=dict)
    worker_metrics: Dict[str, Dict] = field(default_factory=dict)

    # Task state (minimal - tasks are ephemeral)
    pending_task_count: int = 0
    completed_task_count: int = 0

    # Sybil detection state
    sybil_suspicious_entities: List[str] = field(default_factory=list)

    # Health state
    last_health_status: str = "unknown"
    recovery_count: int = 0

    def to_dict(self) -> Dict[str, Any]:
        return {
            "version": self.version,
            "created_at": self.created_at.isoformat(),
            "hotkey": self.hotkey,
            "uid": self.uid,
            "last_weight_block": self.last_weight_block,
            "weights_history": self.weights_history[-MAX_CHECKPOINTS_KEPT:],  # Limit size
            "orchestrator_metrics": {str(k): v for k, v in self.orchestrator_metrics.items()},
            "worker_metrics": self.worker_metrics,
            "pending_task_count": self.pending_task_count,
            "completed_task_count": self.completed_task_count,
            "sybil_suspicious_entities": self.sybil_suspicious_entities,
            "last_health_status": self.last_health_status,
            "recovery_count": self.recovery_count,
        }

    @classmethod
    def from_dict(cls, data: Dict[str, Any]) -> "ValidatorCheckpoint":
        """Create checkpoint from dictionary"""
        checkpoint = cls()
        checkpoint.version = data.get("version", "1.0")
        checkpoint.created_at = (
            datetime.fromisoformat(data["created_at"])
            if "created_at" in data
            else datetime.utcnow()
        )
        checkpoint.hotkey = data.get("hotkey", "")
        checkpoint.uid = data.get("uid")
        checkpoint.last_weight_block = data.get("last_weight_block", 0)
        checkpoint.weights_history = data.get("weights_history", [])
        checkpoint.orchestrator_metrics = {
            int(k): v for k, v in data.get("orchestrator_metrics", {}).items()
        }
        checkpoint.worker_metrics = data.get("worker_metrics", {})
        checkpoint.pending_task_count = data.get("pending_task_count", 0)
        checkpoint.completed_task_count = data.get("completed_task_count", 0)
        checkpoint.sybil_suspicious_entities = data.get("sybil_suspicious_entities", [])
        checkpoint.last_health_status = data.get("last_health_status", "unknown")
        checkpoint.recovery_count = data.get("recovery_count", 0)
        return checkpoint


# =============================================================================
