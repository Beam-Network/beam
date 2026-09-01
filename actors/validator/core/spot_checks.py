"""Proof spot-check orchestration and fraud classification."""

import logging
import random
from typing import List

from core.models import OrchestratorInfo, SpotCheckResult, WorkSummary

logger = logging.getLogger(__name__)


async def spot_check_proofs(validator) -> None:
    eligible = [
        (hotkey, summary)
        for hotkey, summary in validator.work_summaries.items()
        if summary.proof_count > 0
        and validator.orchestrators.get(hotkey)
        and validator.orchestrators[hotkey].is_healthy
    ]
    if eligible:
        logger.info(f"_spot_check_proofs: checking {len(eligible)} eligible orchestrators")
    for hotkey, summary in validator.work_summaries.items():
        if summary.proof_count == 0:
            continue
        orchestrator = validator.orchestrators.get(hotkey)
        if not orchestrator or not orchestrator.is_healthy:
            continue
        try:
            result = await validator._spot_check_orchestrator(hotkey, orchestrator, summary)
            validator.spot_check_results[hotkey] = result
            validator.spot_check_history.append(result)
            if len(validator.spot_check_history) > 1000:
                validator.spot_check_history = validator.spot_check_history[-1000:]
            if result.fraud_detected:
                validator.fraud_penalties[hotkey] = result.fraud_severity
                logger.warning(
                    f"Fraud detected for orchestrator {hotkey[:16]}...: "
                    f"{result.proofs_invalid}/{result.proofs_received} invalid proofs"
                )
            else:
                validator.fraud_penalties.pop(hotkey, None)
        except Exception as exc:
            logger.error(f"Error spot-checking orchestrator {hotkey[:16]}...: {exc}")


async def spot_check_orchestrator(
    validator,
    hotkey: str,
    orchestrator: OrchestratorInfo,
    summary: WorkSummary,
) -> SpotCheckResult:
    sample_percent = 0.05 + random.random() * 0.05
    sample_size = max(5, min(50, int(summary.proof_count * sample_percent)))
    logger.info(
        f"_spot_check_orchestrator: {hotkey[:16]}... "
        f"sampling {sample_size} of {summary.proof_count} proofs ({sample_percent:.1%})"
    )
    proof_ids = await validator._get_random_proof_ids(orchestrator, sample_size)
    if not proof_ids:
        logger.warning(f"_spot_check_orchestrator: {hotkey[:16]}... no proof IDs returned")
        return SpotCheckResult(
            orchestrator_hotkey=hotkey,
            proofs_requested=sample_size,
            proofs_received=0,
            proofs_valid=0,
            proofs_invalid=0,
        )

    logger.debug(
        f"_spot_check_orchestrator: {hotkey[:16]}... got {len(proof_ids)} proof IDs, "
        "requesting full proofs"
    )
    proofs = await validator._request_proofs(orchestrator, proof_ids)
    if not proofs:
        logger.warning(
            f"_spot_check_orchestrator: {hotkey[:16]}... returned 0 proofs for "
            f"{len(proof_ids)} IDs - FRAUD"
        )
        return SpotCheckResult(
            orchestrator_hotkey=hotkey,
            proofs_requested=sample_size,
            proofs_received=0,
            proofs_valid=0,
            proofs_invalid=0,
            fraud_detected=True,
            fraud_severity=1.0,
        )

    valid_count, invalid_ids, invalid_reasons = await _verify_sample(validator, proofs)
    verification_rate = valid_count / len(proofs) if proofs else 0
    invalid_rate = 1.0 - verification_rate
    fraud_detected = invalid_rate > 0.05
    fraud_severity = min(1.0, invalid_rate * 2)
    _log_result(
        hotkey,
        proofs,
        valid_count,
        invalid_ids,
        invalid_reasons,
        verification_rate,
        fraud_detected,
        fraud_severity,
    )
    return SpotCheckResult(
        orchestrator_hotkey=hotkey,
        proofs_requested=sample_size,
        proofs_received=len(proofs),
        proofs_valid=valid_count,
        proofs_invalid=len(invalid_ids),
        verification_rate=verification_rate,
        invalid_proof_ids=invalid_ids,
        invalid_reasons=invalid_reasons,
        fraud_detected=fraud_detected,
        fraud_severity=fraud_severity,
    )


async def _verify_sample(validator, proofs: List[dict]) -> tuple:
    valid_count = 0
    invalid_ids = []
    invalid_reasons = {}
    for proof_data in proofs:
        verification = await validator._verify_single_subnet_proof(proof_data)
        if verification.valid:
            valid_count += 1
        else:
            invalid_ids.append(verification.task_id)
            invalid_reasons[verification.task_id] = verification.error or "Unknown error"
    return valid_count, invalid_ids, invalid_reasons


def _log_result(
    hotkey,
    proofs,
    valid_count,
    invalid_ids,
    invalid_reasons,
    verification_rate,
    fraud_detected,
    fraud_severity,
) -> None:
    log_fn = logger.warning if fraud_detected else logger.info
    log_fn(
        f"_spot_check_orchestrator: {hotkey[:16]}... "
        f"valid={valid_count}/{len(proofs)} invalid={len(invalid_ids)} "
        f"rate={verification_rate:.2%} fraud={'YES' if fraud_detected else 'no'}"
        + (f" severity={fraud_severity:.4f}" if fraud_detected else "")
        + (
            f" invalid_reasons={dict(list(invalid_reasons.items())[:3])}"
            if invalid_reasons
            else ""
        )
    )


async def get_random_proof_ids(
    _validator,
    _orchestrator: OrchestratorInfo,
    _sample_size: int,
) -> List[str]:
    return []


async def request_proofs(
    _validator,
    _orchestrator: OrchestratorInfo,
    _proof_ids: List[str],
) -> List[dict]:
    return []
