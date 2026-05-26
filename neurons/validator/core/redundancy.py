"""
Validator Redundancy and Failover System

Provides high-availability features for validators:
1. State Checkpointing - Regular snapshots for recovery
2. Health Monitoring - Self-health checks and alerting
3. Graceful Degradation - Continue operation with reduced functionality
4. Recovery Procedures - Automatic state restoration after failures

Design Principles:
- Validators should be able to restart without losing scoring history
- Health issues should be detected proactively
- Failures should not result in loss of stake or incorrect weight setting
"""

import asyncio
import json
import logging
import os
import time
from dataclasses import dataclass, field
from datetime import datetime
from enum import Enum
from pathlib import Path
from typing import Any, Callable, Dict, List, Optional

logger = logging.getLogger(__name__)


# =============================================================================
# CONFIGURATION
# =============================================================================

# Checkpoint configuration
DEFAULT_CHECKPOINT_DIR = os.getenv("VALIDATOR_CHECKPOINT_DIR", "/tmp/beam-validator")
CHECKPOINT_INTERVAL_SECONDS = int(os.getenv("CHECKPOINT_INTERVAL_SECONDS", "300"))  # 5 minutes
MAX_CHECKPOINT_AGE_HOURS = int(os.getenv("MAX_CHECKPOINT_AGE_HOURS", "24"))
MAX_CHECKPOINTS_KEPT = int(os.getenv("MAX_CHECKPOINTS_KEPT", "10"))

# Health monitoring
HEALTH_CHECK_INTERVAL_SECONDS = int(os.getenv("HEALTH_CHECK_INTERVAL_SECONDS", "30"))
UNHEALTHY_THRESHOLD_FAILURES = int(os.getenv("UNHEALTHY_THRESHOLD_FAILURES", "3"))
CRITICAL_THRESHOLD_FAILURES = int(os.getenv("CRITICAL_THRESHOLD_FAILURES", "5"))

# Recovery
MAX_RECOVERY_ATTEMPTS = int(os.getenv("MAX_RECOVERY_ATTEMPTS", "3"))
RECOVERY_BACKOFF_SECONDS = int(os.getenv("RECOVERY_BACKOFF_SECONDS", "60"))


# =============================================================================
# DATA STRUCTURES
# =============================================================================


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
# HEALTH MONITOR
# =============================================================================


class HealthMonitor:
    """
    Monitors validator health and detects issues proactively.

    Runs periodic health checks on critical components:
    - Bittensor connection
    - Metagraph sync
    - Memory usage
    - Task processing
    - Weight setting
    """

    def __init__(self, validator: Any):
        self.validator = validator
        self.start_time = datetime.utcnow()
        self.consecutive_failures = 0
        self.last_healthy = datetime.utcnow()
        self.checks: List[HealthCheck] = []
        self._running = False
        self._task: Optional[asyncio.Task] = None

        # Custom health check functions
        self._custom_checks: Dict[str, Callable] = {}

    async def start(self) -> None:
        """Start the health monitoring loop"""
        if self._running:
            return

        self._running = True
        self._task = asyncio.create_task(self._health_loop())
        logger.info("Health monitor started")

    async def stop(self) -> None:
        """Stop the health monitoring loop"""
        self._running = False
        if self._task:
            self._task.cancel()
            try:
                await self._task
            except asyncio.CancelledError:
                pass
        logger.info("Health monitor stopped")

    def register_check(self, name: str, check_fn: Callable) -> None:
        """Register a custom health check function"""
        self._custom_checks[name] = check_fn

    async def _health_loop(self) -> None:
        """Main health checking loop"""
        await asyncio.sleep(HEALTH_CHECK_INTERVAL_SECONDS)
        while self._running:
            try:
                report = await self.run_health_checks()

                if report.status == HealthStatus.HEALTHY:
                    self.consecutive_failures = 0
                    self.last_healthy = datetime.utcnow()
                else:
                    self.consecutive_failures += 1
                    logger.warning(
                        f"Health check failed: {report.status.value}, "
                        f"consecutive failures: {self.consecutive_failures}"
                    )

                    if self.consecutive_failures >= CRITICAL_THRESHOLD_FAILURES:
                        logger.error("Validator health is CRITICAL - intervention may be needed")
                        # Could trigger alerts here

            except Exception as e:
                logger.error(f"Error in health check loop: {e}")
                self.consecutive_failures += 1

            await asyncio.sleep(HEALTH_CHECK_INTERVAL_SECONDS)

    async def run_health_checks(self) -> HealthReport:
        """Run all health checks and return a report"""
        checks = []

        # Check 1: Bittensor connection
        checks.append(await self._check_bittensor_connection())

        # Check 2: Metagraph sync
        checks.append(await self._check_metagraph())

        # Check 3: Memory usage
        checks.append(self._check_memory())

        # Check 4: Task processing
        checks.append(self._check_task_processing())

        # Check 5: Weight setting recency
        checks.append(self._check_weight_setting())

        # Run custom checks
        for name, check_fn in self._custom_checks.items():
            try:
                result = await check_fn() if asyncio.iscoroutinefunction(check_fn) else check_fn()
                checks.append(result)
            except Exception as e:
                checks.append(
                    HealthCheck(
                        component=name,
                        status=ComponentHealth.ERROR,
                        message=f"Check failed: {e}",
                    )
                )

        self.checks = checks

        # Determine overall status
        status = self._determine_status(checks)

        return HealthReport(
            status=status,
            checks=checks,
            consecutive_failures=self.consecutive_failures,
            last_healthy=self.last_healthy,
            uptime_seconds=(datetime.utcnow() - self.start_time).total_seconds(),
        )

    async def _check_bittensor_connection(self) -> HealthCheck:
        """Check Bittensor subtensor connection"""
        start = time.time()

        if self.validator.subtensor is None:
            return HealthCheck(
                component="bittensor",
                status=ComponentHealth.ERROR,
                message="Subtensor not initialized",
            )

        try:
            block = self.validator.subtensor.block
            latency = (time.time() - start) * 1000

            if block is None or block <= 0:
                return HealthCheck(
                    component="bittensor",
                    status=ComponentHealth.WARNING,
                    message="Unable to get block number",
                    latency_ms=latency,
                )

            return HealthCheck(
                component="bittensor",
                status=ComponentHealth.OK,
                message=f"Connected, block {block}",
                latency_ms=latency,
            )

        except Exception as e:
            return HealthCheck(
                component="bittensor",
                status=ComponentHealth.ERROR,
                message=f"Connection error: {e}",
                latency_ms=(time.time() - start) * 1000,
            )

    async def _check_metagraph(self) -> HealthCheck:
        """Check metagraph sync status"""
        if self.validator.metagraph is None:
            return HealthCheck(
                component="metagraph",
                status=ComponentHealth.ERROR,
                message="Metagraph not initialized",
            )

        try:
            n = self.validator.metagraph.n.item()

            if n <= 0:
                return HealthCheck(
                    component="metagraph",
                    status=ComponentHealth.WARNING,
                    message="Metagraph empty",
                )

            return HealthCheck(
                component="metagraph",
                status=ComponentHealth.OK,
                message=f"Synced, {n} neurons",
            )

        except Exception as e:
            return HealthCheck(
                component="metagraph",
                status=ComponentHealth.ERROR,
                message=f"Sync error: {e}",
            )

    def _check_memory(self) -> HealthCheck:
        """Check memory usage"""
        try:
            import psutil

            process = psutil.Process()
            memory_mb = process.memory_info().rss / 1024 / 1024
            memory_percent = process.memory_percent()

            if memory_percent > 90:
                return HealthCheck(
                    component="memory",
                    status=ComponentHealth.ERROR,
                    message=f"Critical: {memory_mb:.0f}MB ({memory_percent:.1f}%)",
                )
            elif memory_percent > 75:
                return HealthCheck(
                    component="memory",
                    status=ComponentHealth.WARNING,
                    message=f"High: {memory_mb:.0f}MB ({memory_percent:.1f}%)",
                )

            return HealthCheck(
                component="memory",
                status=ComponentHealth.OK,
                message=f"{memory_mb:.0f}MB ({memory_percent:.1f}%)",
            )

        except ImportError:
            return HealthCheck(
                component="memory",
                status=ComponentHealth.UNKNOWN,
                message="psutil not available",
            )
        except Exception as e:
            return HealthCheck(
                component="memory",
                status=ComponentHealth.ERROR,
                message=f"Check failed: {e}",
            )

    def _check_task_processing(self) -> HealthCheck:
        """Check task processing health"""
        pending = len(getattr(self.validator, "pending_tasks", {}))
        results = len(getattr(self.validator, "task_results", {}))

        if pending > 1000:
            return HealthCheck(
                component="tasks",
                status=ComponentHealth.WARNING,
                message=f"High pending: {pending} pending, {results} completed",
            )

        return HealthCheck(
            component="tasks",
            status=ComponentHealth.OK,
            message=f"{pending} pending, {results} completed",
        )

    def _check_weight_setting(self) -> HealthCheck:
        """Check weight setting recency"""
        last_block = getattr(self.validator, "last_weight_block", 0)

        if self.validator.subtensor is None:
            return HealthCheck(
                component="weights",
                status=ComponentHealth.UNKNOWN,
                message="Subtensor not available",
            )

        try:
            current_block = self.validator.subtensor.block
            blocks_since = current_block - last_block

            if last_block == 0:
                return HealthCheck(
                    component="weights",
                    status=ComponentHealth.WARNING,
                    message="No weights set yet",
                )

            if blocks_since > 500:
                return HealthCheck(
                    component="weights",
                    status=ComponentHealth.WARNING,
                    message=f"Stale: {blocks_since} blocks since last set",
                )

            return HealthCheck(
                component="weights",
                status=ComponentHealth.OK,
                message=f"Set at block {last_block} ({blocks_since} blocks ago)",
            )

        except Exception as e:
            return HealthCheck(
                component="weights",
                status=ComponentHealth.ERROR,
                message=f"Check failed: {e}",
            )

    def _determine_status(self, checks: List[HealthCheck]) -> HealthStatus:
        """Determine overall health status from individual checks"""
        error_count = sum(1 for c in checks if c.status == ComponentHealth.ERROR)
        warning_count = sum(1 for c in checks if c.status == ComponentHealth.WARNING)

        if error_count >= 2:
            return HealthStatus.CRITICAL
        elif error_count == 1:
            return HealthStatus.UNHEALTHY
        elif warning_count >= 2:
            return HealthStatus.DEGRADED
        elif warning_count == 1:
            return HealthStatus.DEGRADED

        return HealthStatus.HEALTHY

    def get_status(self) -> HealthStatus:
        """Get current health status"""
        if self.consecutive_failures >= CRITICAL_THRESHOLD_FAILURES:
            return HealthStatus.CRITICAL
        elif self.consecutive_failures >= UNHEALTHY_THRESHOLD_FAILURES:
            return HealthStatus.UNHEALTHY
        elif self.consecutive_failures > 0:
            return HealthStatus.DEGRADED
        return HealthStatus.HEALTHY


# =============================================================================
# CHECKPOINT MANAGER
# =============================================================================


class CheckpointManager:
    """
    Manages validator state checkpoints for recovery.

    Periodically saves validator state to disk and can restore
    from checkpoints after crashes or restarts.
    """

    def __init__(
        self,
        validator: Any,
        checkpoint_dir: str = DEFAULT_CHECKPOINT_DIR,
    ):
        self.validator = validator
        self.checkpoint_dir = Path(checkpoint_dir)
        self.checkpoint_dir.mkdir(parents=True, exist_ok=True)

        self._running = False
        self._task: Optional[asyncio.Task] = None
        self.last_checkpoint: Optional[ValidatorCheckpoint] = None
        self.recovery_count = 0

    async def start(self) -> None:
        """Start the checkpointing loop"""
        if self._running:
            return

        self._running = True
        self._task = asyncio.create_task(self._checkpoint_loop())
        logger.info(f"Checkpoint manager started, dir: {self.checkpoint_dir}")

    async def stop(self) -> None:
        """Stop the checkpointing loop and save final checkpoint"""
        self._running = False
        if self._task:
            self._task.cancel()
            try:
                await self._task
            except asyncio.CancelledError:
                pass

        # Save final checkpoint
        await self.save_checkpoint()
        logger.info("Checkpoint manager stopped")

    async def _checkpoint_loop(self) -> None:
        """Main checkpointing loop"""
        while self._running:
            try:
                await self.save_checkpoint()
            except Exception as e:
                logger.error(f"Error saving checkpoint: {e}")

            await asyncio.sleep(CHECKPOINT_INTERVAL_SECONDS)

    async def save_checkpoint(self) -> Optional[Path]:
        """Save current validator state to checkpoint"""
        try:
            checkpoint = self._create_checkpoint()
            self.last_checkpoint = checkpoint

            # Create filename with timestamp
            timestamp = datetime.utcnow().strftime("%Y%m%d_%H%M%S")
            filename = f"checkpoint_{timestamp}.json"
            filepath = self.checkpoint_dir / filename

            # Write checkpoint
            with open(filepath, "w") as f:
                json.dump(checkpoint.to_dict(), f, indent=2, default=str)

            logger.debug(f"Saved checkpoint: {filepath}")

            # Cleanup old checkpoints
            self._cleanup_old_checkpoints()

            return filepath

        except Exception as e:
            logger.error(f"Failed to save checkpoint: {e}")
            return None

    def _create_checkpoint(self) -> ValidatorCheckpoint:
        """Create checkpoint from current validator state"""
        # Get Sybil suspicious entities
        suspicious = []
        if hasattr(self.validator, "sybil_detector"):
            suspicious = [h for h, _ in self.validator.sybil_detector.get_suspicious_entities()]

        return ValidatorCheckpoint(
            hotkey=getattr(self.validator, "hotkey", ""),
            uid=getattr(self.validator, "uid", None),
            last_weight_block=getattr(self.validator, "last_weight_block", 0),
            weights_history=getattr(self.validator, "weights_history", [])[-MAX_CHECKPOINTS_KEPT:],
            orchestrator_metrics=dict(getattr(self.validator, "orchestrator_metrics", {})),
            worker_metrics=dict(getattr(self.validator, "worker_metrics", {})),
            pending_task_count=len(getattr(self.validator, "pending_tasks", {})),
            completed_task_count=len(getattr(self.validator, "task_results", {})),
            sybil_suspicious_entities=suspicious,
            last_health_status="healthy",  # Will be updated by health monitor
            recovery_count=self.recovery_count,
        )

    def load_latest_checkpoint(self) -> Optional[ValidatorCheckpoint]:
        """Load the most recent checkpoint"""
        checkpoints = sorted(self.checkpoint_dir.glob("checkpoint_*.json"), reverse=True)

        if not checkpoints:
            logger.info("No checkpoints found")
            return None

        latest = checkpoints[0]

        try:
            with open(latest, "r") as f:
                data = json.load(f)

            checkpoint = ValidatorCheckpoint.from_dict(data)

            # Check age
            age = datetime.utcnow() - checkpoint.created_at
            if age.total_seconds() > MAX_CHECKPOINT_AGE_HOURS * 3600:
                logger.warning(
                    f"Latest checkpoint is {age.total_seconds()/3600:.1f}h old, may be stale"
                )

            logger.info(f"Loaded checkpoint from {latest}")
            return checkpoint

        except Exception as e:
            logger.error(f"Failed to load checkpoint {latest}: {e}")
            return None

    def restore_from_checkpoint(self, checkpoint: ValidatorCheckpoint) -> bool:
        """Restore validator state from checkpoint"""
        try:
            self.recovery_count += 1

            # Restore weight history
            if checkpoint.weights_history:
                self.validator.weights_history = checkpoint.weights_history
                self.validator.last_weight_block = checkpoint.last_weight_block

            # Restore orchestrator metrics
            if checkpoint.orchestrator_metrics:
                self.validator.orchestrator_metrics = checkpoint.orchestrator_metrics

            # Restore worker metrics
            if checkpoint.worker_metrics:
                self.validator.worker_metrics = checkpoint.worker_metrics

            logger.info(
                f"Restored from checkpoint: "
                f"last_weight_block={checkpoint.last_weight_block}, "
                f"orchestrators={len(checkpoint.orchestrator_metrics)}, "
                f"workers={len(checkpoint.worker_metrics)}"
            )

            return True

        except Exception as e:
            logger.error(f"Failed to restore from checkpoint: {e}")
            return False

    def _cleanup_old_checkpoints(self) -> None:
        """Remove old checkpoints beyond the limit"""
        checkpoints = sorted(self.checkpoint_dir.glob("checkpoint_*.json"), reverse=True)

        if len(checkpoints) > MAX_CHECKPOINTS_KEPT:
            for old_checkpoint in checkpoints[MAX_CHECKPOINTS_KEPT:]:
                try:
                    old_checkpoint.unlink()
                    logger.debug(f"Removed old checkpoint: {old_checkpoint}")
                except Exception as e:
                    logger.warning(f"Failed to remove old checkpoint: {e}")


# =============================================================================
# RECOVERY MANAGER
# =============================================================================


class RecoveryManager:
    """
    Handles validator recovery from failures.

    Coordinates checkpoint restoration and graceful degradation.
    """

    def __init__(
        self,
        validator: Any,
        checkpoint_manager: CheckpointManager,
        health_monitor: HealthMonitor,
    ):
        self.validator = validator
        self.checkpoint_manager = checkpoint_manager
        self.health_monitor = health_monitor
        self.recovery_attempts = 0
        self.last_recovery: Optional[datetime] = None
        self.in_recovery = False

    async def attempt_recovery(self) -> bool:
        """Attempt to recover from a failure state"""
        if self.in_recovery:
            logger.warning("Recovery already in progress")
            return False

        self.in_recovery = True
        self.recovery_attempts += 1
        self.last_recovery = datetime.utcnow()

        logger.info(f"Starting recovery attempt {self.recovery_attempts}")

        try:
            # Step 1: Load latest checkpoint
            checkpoint = self.checkpoint_manager.load_latest_checkpoint()

            if checkpoint:
                # Step 2: Restore state
                success = self.checkpoint_manager.restore_from_checkpoint(checkpoint)

                if success:
                    logger.info("Recovery successful from checkpoint")
                    self.in_recovery = False
                    return True

            # Step 3: If checkpoint restoration failed, try fresh start
            logger.warning("Checkpoint restoration failed, attempting fresh start")

            # Clear problematic state
            self.validator.pending_tasks = {}
            self.validator.task_results = {}

            # Reset health monitor
            self.health_monitor.consecutive_failures = 0

            logger.info("Recovery completed with fresh state")
            self.in_recovery = False
            return True

        except Exception as e:
            logger.error(f"Recovery attempt failed: {e}")
            self.in_recovery = False

            # Backoff before next attempt
            if self.recovery_attempts < MAX_RECOVERY_ATTEMPTS:
                await asyncio.sleep(RECOVERY_BACKOFF_SECONDS * self.recovery_attempts)
                return await self.attempt_recovery()

            logger.critical(f"Max recovery attempts ({MAX_RECOVERY_ATTEMPTS}) reached")
            return False

    def should_attempt_recovery(self) -> bool:
        """Check if recovery should be attempted"""
        if self.in_recovery:
            return False

        if self.recovery_attempts >= MAX_RECOVERY_ATTEMPTS:
            # Check if enough time has passed for reset
            if self.last_recovery:
                time_since = (datetime.utcnow() - self.last_recovery).total_seconds()
                if time_since > RECOVERY_BACKOFF_SECONDS * MAX_RECOVERY_ATTEMPTS * 2:
                    self.recovery_attempts = 0  # Reset attempts
                    return True
            return False

        status = self.health_monitor.get_status()
        return status in (HealthStatus.CRITICAL, HealthStatus.UNHEALTHY)


# =============================================================================
# CONVENIENCE FUNCTIONS
# =============================================================================


def create_redundancy_system(validator: Any, checkpoint_dir: str = DEFAULT_CHECKPOINT_DIR):
    """
    Create a complete redundancy system for a validator.

    Returns:
        Tuple of (HealthMonitor, CheckpointManager, RecoveryManager)
    """
    health_monitor = HealthMonitor(validator)
    checkpoint_manager = CheckpointManager(validator, checkpoint_dir)
    recovery_manager = RecoveryManager(validator, checkpoint_manager, health_monitor)

    return health_monitor, checkpoint_manager, recovery_manager


async def initialize_with_recovery(
    validator: Any,
    checkpoint_dir: str = DEFAULT_CHECKPOINT_DIR,
) -> bool:
    """
    Initialize validator with checkpoint recovery if available.

    Returns:
        True if recovery was performed, False if fresh start
    """
    checkpoint_manager = CheckpointManager(validator, checkpoint_dir)

    checkpoint = checkpoint_manager.load_latest_checkpoint()
    if checkpoint:
        success = checkpoint_manager.restore_from_checkpoint(checkpoint)
        if success:
            logger.info("Validator initialized from checkpoint")
            return True

    logger.info("Validator initialized with fresh state")
    return False
