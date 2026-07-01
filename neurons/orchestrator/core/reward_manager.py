"""
Reward Manager - Local emission/reward-share accounting.

Operators are responsible for any external worker compensation policy.

This module maintains per-worker emission-share data for dashboards, exports,
and operator tooling.
"""

import logging
from typing import Dict

from .config import OrchestratorSettings

logger = logging.getLogger(__name__)


class RewardManager:
    """Tracks emissions and computes per-worker reward shares for local accounting."""

    def __init__(self, settings: OrchestratorSettings):
        self.settings = settings

        # Emission tracking
        self.total_rewards_distributed: float = 0.0
        self.last_emission_check: float = 0.0
        self.epoch_start_emission: float = 0.0

    def _calculate_quality_multiplier(self, worker) -> float:
        """
        Calculate quality multiplier for worker rewards.

        Range: 0.5 to 1.5
        """
        if worker is None:
            return 1.0

        multiplier = 1.0
        multiplier += getattr(worker, "success_rate", 0.0) * 0.25

        latency_ms = getattr(worker, "latency_ms", 0)
        if latency_ms > 0:
            latency_score = max(0, 1 - (latency_ms / 1000))
            multiplier += latency_score * 0.15

        multiplier += getattr(worker, "trust_score", 0.5) * 0.10

        return max(0.5, min(1.5, multiplier))

    def _calculate_worker_reward_score(self, worker) -> float:
        """Calculate a worker's reward score based on multiple factors."""
        if worker is None:
            return 0.0

        w_success = self.settings.reward_weight_success_rate
        w_latency = self.settings.reward_weight_latency
        w_trust = self.settings.reward_weight_trust

        bytes_score = float(getattr(worker, "bytes_relayed_epoch", 0))
        success_score = getattr(worker, "success_rate", 0.0)

        max_latency_ms = 1000.0
        latency_ms = getattr(worker, "latency_ms", 0)
        latency_score = max(0.0, 1.0 - (latency_ms / max_latency_ms))
        trust_score = getattr(worker, "trust_score", 0.5)

        quality_multiplier = (
            (w_success * success_score + w_latency * latency_score + w_trust * trust_score)
            / (w_success + w_latency + w_trust)
            if (w_success + w_latency + w_trust) > 0
            else 1.0
        )

        final_score = bytes_score * (0.5 + 0.5 * quality_multiplier)
        return final_score

    def distribute_rewards_at_epoch_end(self, workers_values, get_our_emission) -> Dict[str, float]:
        """
        Distribute accumulated epoch emissions to all workers proportionally.

        This only updates in-memory `worker.rewards_earned_*` counters for
        reporting/export; no payout is executed by this process.

        Returns dict of worker_id -> reward amount in TAO.
        """
        current_emission = get_our_emission()
        epoch_rewards = current_emission - self.epoch_start_emission

        if epoch_rewards <= 0:
            logger.debug("No new emissions this epoch, no rewards to distribute")
            return {}

        contributing_workers = [w for w in workers_values if w.bytes_relayed_epoch > 0]

        if not contributing_workers:
            logger.debug("No workers contributed this epoch, no rewards to distribute")
            return {}

        worker_scores: Dict[str, float] = {}
        for worker in contributing_workers:
            score = self._calculate_worker_reward_score(worker)
            worker_scores[worker.worker_id] = score

        total_score = sum(worker_scores.values())
        if total_score <= 0:
            logger.warning("Total worker score is 0, distributing equally")
            total_score = len(contributing_workers)
            worker_scores = {w.worker_id: 1.0 for w in contributing_workers}

        rewards_distributed: Dict[str, float] = {}
        for worker in contributing_workers:
            share = worker_scores[worker.worker_id] / total_score
            reward = epoch_rewards * share

            reward_nano = int(reward * 1e9)
            worker.rewards_earned_epoch = reward_nano
            worker.rewards_earned_total += reward_nano

            rewards_distributed[worker.worker_id] = reward

            logger.info(
                f"Worker {worker.worker_id[:8]} ({worker.hotkey[:12]}...) earned {reward:.6f} ध "
                f"({share*100:.2f}% share, {worker.bytes_relayed_epoch:,} bytes, "
                f"success={worker.success_rate:.2f}, latency={worker.latency_ms:.0f}ms)"
            )

        self.total_rewards_distributed += epoch_rewards

        logger.info(
            f"Epoch reward distribution complete: {epoch_rewards:.6f} ध "
            f"to {len(rewards_distributed)} workers "
            f"(total all-time: {self.total_rewards_distributed:.6f} ध) "
            "(local accounting only - operator-defined worker compensation)"
        )

        return rewards_distributed

    def distribute_rewards_to_workers(self, get_our_emission) -> Dict[str, float]:
        """
        Track emission deltas during the epoch.

        Actual share allocation happens at epoch end via
        ``distribute_rewards_at_epoch_end``.
        """
        current_emission = get_our_emission()
        if current_emission > self.last_emission_check:
            new_emissions = current_emission - self.last_emission_check
            logger.debug(
                f"New emissions detected: {new_emissions:.6f} ध (accumulated for epoch end)"
            )
            self.last_emission_check = current_emission

        return {}
