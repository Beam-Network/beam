"""Local validator scoring and UID lookup behavior."""

import logging
from typing import Optional

from core._beam_stubs import BANDWIDTH_EMA_ALPHA, Task

logger = logging.getLogger(__name__)


async def _update_scores(validator) -> None:
    """Update local orchestrator scores from work, challenge, fraud, and payment signals."""
    logger.debug(
        f"_update_scores: scoring {len(validator.orchestrators)} orchestrators, {len(validator.work_summaries)} with summaries"
    )

    _baseline_multiplier = 0.1
    if validator.subnet_core_client:
        try:
            net_config = await validator.subnet_core_client.get_network_config()
            if net_config and "validator_baseline_multiplier" in net_config:
                _baseline_multiplier = float(net_config["validator_baseline_multiplier"])
        except Exception as e:
            logger.warning(f"Could not fetch baseline multiplier from SubnetCore: {e}")

    for hotkey, info in validator.orchestrators.items():
        summary = validator.work_summaries.get(hotkey)

        if not summary:
            final_score = 1.0 if info.is_subnet_owned else _baseline_multiplier
            validator.orchestrator_scores[hotkey] = final_score
            info.last_score = final_score
            logger.debug(
                f"_update_scores: {hotkey[:16]}... UID={info.uid} "
                f"no work summary - baseline score={final_score:.4f}"
            )
            continue

        throughput_score = min(max(summary.avg_bandwidth_mbps / 1000.0, 0.0), 1.0)
        reliability_score = min(max(summary.success_rate, 0.0), 1.0)
        work_score = (throughput_score * 0.6) + (reliability_score * 0.4)
        payment_multiplier = validator.payment_penalty_multipliers.get(hotkey, 1.0)
        challenge_multiplier = validator._calculate_challenge_multiplier(hotkey)
        fraud_multiplier = validator._calculate_fraud_multiplier(hotkey)
        final_score = work_score * challenge_multiplier * fraud_multiplier * payment_multiplier

        validator.orchestrator_scores[hotkey] = final_score
        info.last_score = final_score

        logger.info(
            f"_update_scores: {hotkey[:16]}... UID={info.uid} "
            f"work_score={work_score:.4f} "
            f"challenge_mult={challenge_multiplier:.4f} "
            f"fraud_mult={fraud_multiplier:.4f} "
            f"payment_mult={payment_multiplier:.4f} "
            f"final_score={final_score:.6f}"
        )

    # Also update connection scores for legacy compatibility
    for uid in validator.connections:
        conn_results = [
            r
            for r in validator.task_results.values()
            if r.valid and validator._get_uid_for_miner(r.pob.miner_id) == uid
        ]

        if not conn_results:
            continue

        bandwidth_scores = [r.calculated_bandwidth for r in conn_results]
        avg_bandwidth = sum(bandwidth_scores) / len(bandwidth_scores)

        bandwidth_normalized = min(avg_bandwidth / validator.settings.max_bandwidth_mbps, 1.0)

        total_tasks = len(
            [t for t in validator.pending_tasks.values() if validator._get_uid_for_task(t) == uid]
        )
        success_rate = len(conn_results) / total_tasks if total_tasks > 0 else 0

        score = (
            validator.settings.score_weight_bandwidth * bandwidth_normalized
            + validator.settings.score_weight_uptime * success_rate
            + validator.settings.score_weight_loss * 1.0
            + validator.settings.score_weight_tier * 0.5
        )

        if uid in validator.connection_scores:
            old_score = validator.connection_scores[uid]
            score = BANDWIDTH_EMA_ALPHA * score + (1 - BANDWIDTH_EMA_ALPHA) * old_score

        validator.connection_scores[uid] = score
        validator.connection_bandwidth[uid] = avg_bandwidth

def _calculate_challenge_multiplier(validator, hotkey: str) -> float:
    """Calculate challenge verification multiplier."""
    challenge_results = [
        r
        for cid, r in validator.challenge_results.items()
        if validator.active_challenges.get(cid, {}).get("orchestrator") == hotkey
    ]

    if not challenge_results:
        logger.debug(
            f"_calculate_challenge_multiplier: {hotkey[:16]}... no challenge results, using default 0.9"
        )
        return 0.9

    successful = sum(1 for r in challenge_results if r.success)
    success_rate = successful / len(challenge_results)
    multiplier = 0.5 + (success_rate * 0.5)

    logger.info(
        f"_calculate_challenge_multiplier: {hotkey[:16]}... "
        f"{successful}/{len(challenge_results)} challenges passed "
        f"(rate={success_rate:.2%}) -> multiplier={multiplier:.4f}"
    )

    return multiplier

def _calculate_fraud_multiplier(validator, hotkey: str) -> float:
    """Calculate fraud penalty multiplier from spot-check results."""
    fraud_severity = validator.fraud_penalties.get(hotkey, 0.0)

    if fraud_severity <= 0:
        logger.debug(f"_calculate_fraud_multiplier: {hotkey[:16]}... no fraud penalty -> 1.0")
        return 1.0

    multiplier = 1.0 - fraud_severity
    multiplier = max(0.1, multiplier)
    logger.warning(
        f"_calculate_fraud_multiplier: {hotkey[:16]}... "
        f"fraud_severity={fraud_severity:.4f} -> multiplier={multiplier:.4f}"
    )
    return multiplier

def _get_uid_for_miner(validator, miner_id) -> Optional[int]:
    """Get UID for a miner hotkey"""
    hotkey = miner_id.decode() if isinstance(miner_id, bytes) else str(miner_id)
    for uid, conn in validator.connections.items():
        if conn["hotkey"] == hotkey:
            logger.debug(f"_get_uid_for_miner: {hotkey[:16]}... -> UID {uid}")
            return uid
    logger.debug(
        f"_get_uid_for_miner: {hotkey[:16]}... -> NOT FOUND in {len(validator.connections)} connections"
    )
    return None

def _get_uid_for_task(validator, task: Task) -> Optional[int]:
    """Get UID for a task's assigned connection"""
    if task.path:
        for uid, conn in validator.connections.items():
            if conn["hotkey"] == task.path[0]:
                logger.debug(f"_get_uid_for_task: path[0]={task.path[0][:16]}... -> UID {uid}")
                return uid
        logger.debug(f"_get_uid_for_task: path[0]={task.path[0][:16]}... -> NOT FOUND")
    return None
