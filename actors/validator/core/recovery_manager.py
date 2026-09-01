"""Validator recovery orchestration."""

import asyncio
import logging
from datetime import datetime
from typing import Any, Optional

from core.checkpoint_manager import CheckpointManager
from core.health_monitor import HealthMonitor
from core.redundancy_config import MAX_RECOVERY_ATTEMPTS, RECOVERY_BACKOFF_SECONDS
from core.redundancy_models import HealthStatus

logger = logging.getLogger("core.redundancy")


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
