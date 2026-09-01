"""Compatibility facade for validator health, checkpoint, and recovery services."""

import logging
from typing import Any

from core.checkpoint_manager import CheckpointManager
from core.health_monitor import HealthMonitor
from core.recovery_manager import RecoveryManager
from core.redundancy_config import (
    CHECKPOINT_INTERVAL_SECONDS,
    CRITICAL_THRESHOLD_FAILURES,
    DEFAULT_CHECKPOINT_DIR,
    HEALTH_CHECK_INTERVAL_SECONDS,
    MAX_CHECKPOINT_AGE_HOURS,
    MAX_CHECKPOINTS_KEPT,
    MAX_RECOVERY_ATTEMPTS,
    RECOVERY_BACKOFF_SECONDS,
    UNHEALTHY_THRESHOLD_FAILURES,
)
from core.redundancy_models import (
    ComponentHealth,
    HealthCheck,
    HealthReport,
    HealthStatus,
    ValidatorCheckpoint,
)

logger = logging.getLogger(__name__)


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
