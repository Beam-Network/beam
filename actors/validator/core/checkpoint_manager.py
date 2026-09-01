"""Durable validator checkpoint management."""

import asyncio
import json
import logging
from datetime import datetime
from pathlib import Path
from typing import Any, Optional

from core.redundancy_config import (
    CHECKPOINT_INTERVAL_SECONDS,
    DEFAULT_CHECKPOINT_DIR,
    MAX_CHECKPOINT_AGE_HOURS,
    MAX_CHECKPOINTS_KEPT,
)
from core.redundancy_models import ValidatorCheckpoint

logger = logging.getLogger("core.redundancy")


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
