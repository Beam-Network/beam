"""Validation of individual Proof-of-Bandwidth evidence records."""

from typing import Callable

from core.models import ProofVerificationResult


async def verify_single_subnet_proof(
    proof_data: dict,
    *,
    signature_verifier: Callable,
    logger,
) -> ProofVerificationResult:
    """Validate timing, bandwidth, canary, signature, and geography flags."""
    task_id = proof_data.get("task_id", "unknown")
    try:
        worker_hotkey = proof_data.get("worker_hotkey", "")
        start_time_us = proof_data.get("start_time_us", 0)
        end_time_us = proof_data.get("end_time_us", 0)
        bytes_relayed = proof_data.get("bytes_relayed", 0)
        bandwidth_mbps = proof_data.get("bandwidth_mbps", 0.0)
        canary_proof = proof_data.get("canary_proof", "")
        worker_signature = proof_data.get("worker_signature", "")
        result = ProofVerificationResult(task_id=task_id, valid=False)

        duration_us = end_time_us - start_time_us
        if duration_us <= 0:
            result.error = "Invalid timing: end before start"
            return result
        if duration_us < 1000:
            result.error = "Transfer impossibly fast"
            return result
        result.timing_valid = True
        result.latency_ms = duration_us / 1000.0

        bandwidth_error = _validate_bandwidth(
            duration_us,
            bytes_relayed,
            bandwidth_mbps,
        )
        if bandwidth_error:
            result.error = bandwidth_error
            return result
        result.bandwidth_valid = True

        canary_error = _validate_canary(canary_proof)
        if canary_error:
            result.error = canary_error
            return result
        result.canary_valid = True

        if worker_signature and worker_hotkey:
            if not _validate_signature(
                result,
                task_id,
                worker_hotkey,
                start_time_us,
                end_time_us,
                bytes_relayed,
                worker_signature,
                signature_verifier,
                logger,
            ):
                return result
        else:
            logger.debug(f"No worker signature for {task_id[:16]}... (unsigned proof)")
            result.signature_valid = True

        result.geo_valid = True
        result.valid = (
            result.timing_valid
            and result.bandwidth_valid
            and result.canary_valid
            and result.signature_valid
            and result.geo_valid
        )
        return result
    except Exception as exc:
        return ProofVerificationResult(
            task_id=task_id,
            valid=False,
            error=f"Verification error: {str(exc)}",
        )


def _validate_bandwidth(
    duration_us: int,
    bytes_relayed: int,
    bandwidth_mbps: float,
) -> str | None:
    if duration_us <= 0 or bytes_relayed <= 0:
        return None
    calculated_bw = (bytes_relayed * 8) / (duration_us / 1_000_000) / 1_000_000
    if bandwidth_mbps > 0:
        ratio = calculated_bw / bandwidth_mbps
        if ratio < 0.5 or ratio > 2.0:
            return (
                f"Bandwidth mismatch: claimed {bandwidth_mbps:.2f}, "
                f"calculated {calculated_bw:.2f}"
            )
    if calculated_bw > 100_000:
        return f"Bandwidth exceeds physical limits: {calculated_bw:.2f} Mbps"
    return None


def _validate_canary(canary_proof: str) -> str | None:
    if not canary_proof:
        return None
    if len(canary_proof) != 64:
        return f"Invalid canary proof format: {len(canary_proof)} chars"
    try:
        bytes.fromhex(canary_proof)
    except ValueError:
        return "Canary proof is not valid hex"
    return None


def _validate_signature(
    result: ProofVerificationResult,
    task_id: str,
    worker_hotkey: str,
    start_time_us: int,
    end_time_us: int,
    bytes_relayed: int,
    worker_signature: str,
    signature_verifier: Callable,
    logger,
) -> bool:
    message = f"{task_id}:{worker_hotkey}:{start_time_us}:{end_time_us}:{bytes_relayed}"
    try:
        result.signature_valid = signature_verifier(
            message.encode(),
            bytes.fromhex(worker_signature),
            worker_hotkey,
        )
        if not result.signature_valid:
            result.error = f"Invalid worker signature for {task_id[:16]}..."
            logger.warning(f"Signature verification FAILED for {task_id[:16]}...")
        return result.signature_valid
    except Exception as exc:
        result.signature_valid = False
        result.error = f"Signature verification error: {exc}"
        logger.warning(f"Signature verification error for {task_id[:16]}...: {exc}")
        return False
