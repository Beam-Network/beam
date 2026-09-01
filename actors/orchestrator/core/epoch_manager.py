"""Epoch lifecycle helpers for the orchestrator."""

import asyncio
import logging
from datetime import datetime, timedelta

from .config import OrchestratorSettings

logger = logging.getLogger(__name__)


class EpochManager:
    """Manages epoch lifecycle helpers for the orchestrator."""

    def __init__(self, settings: OrchestratorSettings):
        self.settings = settings

    async def epoch_management_loop(
        self,
        running_flag,
        current_epoch_ref,
        epoch_start_time_ref,
        advance_epoch_fn,
    ) -> None:
        """Background loop for epoch advancement."""
        epoch_duration = timedelta(minutes=5)

        while running_flag():
            try:
                await asyncio.sleep(60)
                if datetime.utcnow() - epoch_start_time_ref() >= epoch_duration:
                    await advance_epoch_fn()
            except asyncio.CancelledError:
                break
            except Exception as exc:
                logger.error(f"Error in epoch management: {exc}")