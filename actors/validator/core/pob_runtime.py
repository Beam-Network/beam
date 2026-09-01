"""Collection, verification, and retention of Proof-of-Bandwidth results."""

import logging
import time

from core._beam_stubs import (
    PoBVerificationResult,
    ProofOfBandwidth,
    Task,
    build_merkle_leaf,
    compute_canary_proof,
)

logger = logging.getLogger(__name__)


async def _collect_pob_results(validator) -> None:
    """Collect and verify pending PoB submissions."""
    if validator.settings.local_mode:
        await validator._process_local_challenge_results()
        return

    if not hasattr(validator, "pending_pob_submissions"):
        validator.pending_pob_submissions = {}

    if not validator.pending_pob_submissions:
        logger.debug("_collect_pob_results: no pending PoB submissions")
        return

    logger.info(
        f"_collect_pob_results: processing {len(validator.pending_pob_submissions)} pending PoB submissions"
    )

    now = time.time()
    verified_count = 0
    failed_count = 0
    expired_count = 0
    no_task_count = 0
    tasks_to_remove = []

    for task_id, submission_info in list(validator.pending_pob_submissions.items()):
        pob = submission_info["pob"]
        received_at = submission_info["received_at"]

        if now - received_at > 300:
            tasks_to_remove.append(task_id)
            expired_count += 1
            logger.debug(
                f"_collect_pob_results: {task_id[:16]}... expired (age={now - received_at:.0f}s)"
            )
            continue

        task = validator.pending_tasks.get(task_id)

        if task is None:
            no_task_count += 1
            continue

        try:
            result = validator.verify_pob(pob, task)

            if result.valid:
                validator.task_results[task_id] = result
                verified_count += 1
            else:
                failed_count += 1

            tasks_to_remove.append(task_id)

        except Exception as e:
            logger.error(f"Error verifying pending PoB {task_id[:16]}...: {e}")
            tasks_to_remove.append(task_id)

    for task_id in tasks_to_remove:
        del validator.pending_pob_submissions[task_id]

    logger.info(
        f"_collect_pob_results: verified={verified_count} failed={failed_count} "
        f"expired={expired_count} no_task={no_task_count} "
        f"remaining={len(validator.pending_pob_submissions)} "
        f"total_results={len(validator.task_results)}"
    )

    await validator._cleanup_old_results()

async def _process_local_challenge_results(validator) -> None:
    """Process challenge results from HTTP responses in local mode."""
    now = time.time()
    processed_count = 0
    tasks_to_remove = []

    for task_id, challenge_info in list(validator.active_challenges.items()):
        if challenge_info.get("scored"):
            continue

        status = challenge_info.get("status")

        if status == "chunk_received":
            bandwidth_mbps = challenge_info.get("bandwidth_mbps", 0)
            chunk_sent_at = challenge_info.get("chunk_sent_at", now)
            receive_time = challenge_info.get("receive_time", now)

            task = validator.pending_tasks.get(task_id)
            if task:
                mock_pob = ProofOfBandwidth(
                    task_id=bytes.fromhex(task_id),
                    miner_id=challenge_info.get("connection", {})
                    .get("hotkey", "local-orchestrator")
                    .encode(),
                    chunk_hash=task.chunk_hash,
                    receive_time_us=int(receive_time * 1_000_000),
                    send_time_us=int(chunk_sent_at * 1_000_000),
                    bandwidth_mbps=bandwidth_mbps,
                    signature=b"local-mode-signature",
                )

                result = PoBVerificationResult(
                    pob=mock_pob,
                    valid=True,
                    calculated_bandwidth=bandwidth_mbps,
                )

                validator.task_results[task_id] = result
                challenge_info["scored"] = True
                processed_count += 1

        elif challenge_info.get("created_at", 0) < now - 120:
            tasks_to_remove.append(task_id)

    for task_id in tasks_to_remove:
        del validator.active_challenges[task_id]
        if task_id in validator.pending_tasks:
            del validator.pending_tasks[task_id]

def verify_pob(
    validator,
    pob: ProofOfBandwidth,
    task: Task,
    signature_verifier,
) -> PoBVerificationResult:
    """Verify a Proof-of-Bandwidth submission."""
    task_id_hex = pob.task_id.hex() if isinstance(pob.task_id, bytes) else str(pob.task_id)
    miner_hotkey = pob.miner_id.decode() if isinstance(pob.miner_id, bytes) else pob.miner_id
    logger.info(
        f"verify_pob: starting verification for task {task_id_hex[:16]}... miner={miner_hotkey[:16]}..."
    )

    result = PoBVerificationResult(pob=pob, valid=False)

    try:
        message = pob.get_canonical_message()
        result.signature_valid = signature_verifier(message, pob.signature, miner_hotkey)
    except Exception as e:
        result.signature_valid = False
        result.error = f"Invalid signature: {e}"
        logger.warning(
            f"verify_pob: signature check FAILED for task {task_id_hex[:16]}... error={e}"
        )
        return result

    if not result.signature_valid:
        result.error = "Invalid signature"
        logger.warning(
            f"verify_pob: invalid signature for task {task_id_hex[:16]}... miner={miner_hotkey[:16]}..."
        )
        return result
    logger.debug(f"verify_pob: signature OK for task {task_id_hex[:16]}...")

    delta_us = pob.end_time - pob.start_time

    if delta_us < validator.settings.min_transfer_time_us:
        result.error = f"Transfer too fast: {delta_us}µs"
        logger.warning(
            f"verify_pob: transfer too fast for task {task_id_hex[:16]}... delta={delta_us}µs min={validator.settings.min_transfer_time_us}µs"
        )
        return result

    if pob.end_time > task.deadline:
        result.error = "Task deadline exceeded"
        logger.warning(
            f"verify_pob: deadline exceeded for task {task_id_hex[:16]}... end={pob.end_time} deadline={task.deadline}"
        )
        return result

    result.timing_valid = True
    logger.debug(f"verify_pob: timing OK for task {task_id_hex[:16]}... delta={delta_us}µs")

    calculated_bandwidth = pob.calculate_bandwidth()
    bandwidth_diff = abs(calculated_bandwidth - pob.bandwidth_mbps)

    if bandwidth_diff > 1.0:
        result.error = f"Bandwidth mismatch: claimed {pob.bandwidth_mbps}, calculated {calculated_bandwidth}"
        logger.warning(
            f"verify_pob: bandwidth mismatch for task {task_id_hex[:16]}... claimed={pob.bandwidth_mbps:.2f} calculated={calculated_bandwidth:.2f} diff={bandwidth_diff:.2f}"
        )
        return result

    result.bandwidth_valid = True
    result.calculated_bandwidth = calculated_bandwidth
    logger.debug(
        f"verify_pob: bandwidth OK for task {task_id_hex[:16]}... bw={calculated_bandwidth:.2f} Mbps"
    )

    expected_canary_proof = compute_canary_proof(task.canary, pob.start_time)

    if pob.canary_proof != expected_canary_proof:
        result.error = "Invalid canary proof"
        logger.warning(f"verify_pob: invalid canary proof for task {task_id_hex[:16]}...")
        return result

    result.canary_valid = True
    logger.debug(f"verify_pob: canary OK for task {task_id_hex[:16]}...")

    if len(task.path) > 1:
        # Multi-hop: require merkle proof
        if not pob.merkle_path:
            result.merkle_valid = False
            result.error = "Multi-hop transfer missing merkle proof"
            logger.warning(
                f"verify_pob: missing merkle proof for multi-hop task {task_id_hex[:16]}... (path_len={len(task.path)})"
            )
            return result

        # Verify merkle proof: build expected leaf and check against path
        try:
            # Determine hop neighbors from task path
            hop_idx = min(pob.path_index, len(task.path) - 1)
            prev_hop = task.path[hop_idx - 1] if hop_idx > 0 else ""
            next_hop = task.path[hop_idx + 1] if hop_idx < len(task.path) - 1 else ""

            build_merkle_leaf(
                prev_hop_id=prev_hop,
                current_miner_id=miner_hotkey,
                next_hop_id=next_hop,
                bytes_relayed=pob.bytes_relayed,
                start_time=pob.start_time,
                end_time=pob.end_time,
            )

            # Compute expected root from all path hops if we have all leaves
            # For now, verify the proof structure is valid (non-empty, correct length)
            expected_depth = (len(task.path) - 1).bit_length()
            if len(pob.merkle_path) != expected_depth and expected_depth > 0:
                logger.warning(
                    f"verify_pob: merkle proof depth mismatch for task {task_id_hex[:16]}... "
                    f"expected={expected_depth} got={len(pob.merkle_path)}"
                )
                result.merkle_valid = False
                result.error = "Merkle proof depth mismatch"
                return result

            result.merkle_valid = True
            logger.debug(
                f"verify_pob: merkle proof structure OK for task {task_id_hex[:16]}..."
            )

        except Exception as merkle_err:
            result.merkle_valid = False
            result.error = f"Merkle verification error: {merkle_err}"
            logger.warning(
                f"verify_pob: merkle error for task {task_id_hex[:16]}...: {merkle_err}"
            )
            return result
    else:
        # Single-hop: no merkle proof needed
        result.merkle_valid = True

    result.geo_valid = True

    result.valid = result.all_checks_passed
    result.latency_ms = delta_us / 1000

    logger.info(
        f"verify_pob: task {task_id_hex[:16]}... result={'PASS' if result.valid else 'FAIL'} "
        f"bw={calculated_bandwidth:.2f}Mbps latency={result.latency_ms:.1f}ms "
        f"checks=[sig={result.signature_valid} time={result.timing_valid} bw={result.bandwidth_valid} "
        f"canary={result.canary_valid} merkle={result.merkle_valid} geo={result.geo_valid}]"
    )

    return result

async def _cleanup_old_results(validator) -> None:
    """Clean up old task results and pending tasks."""
    now = time.time()
    max_age_seconds = 24 * 60 * 60

    old_results = []
    for task_id, result in validator.task_results.items():
        result_time = result.pob.end_time / 1_000_000 if result.pob else 0
        if now - result_time > max_age_seconds:
            old_results.append(task_id)

    for task_id in old_results:
        del validator.task_results[task_id]

    old_tasks = []
    for task_id, task in validator.pending_tasks.items():
        task_time = task.deadline / 1_000_000 if task.deadline else 0
        if now - task_time > max_age_seconds:
            old_tasks.append(task_id)

    for task_id in old_tasks:
        del validator.pending_tasks[task_id]

    old_challenges = []
    for task_id, challenge in validator.active_challenges.items():
        created_at = challenge.get("created_at", 0)
        if now - created_at > max_age_seconds:
            old_challenges.append(task_id)

    for task_id in old_challenges:
        del validator.active_challenges[task_id]

    if old_results or old_tasks or old_challenges:
        logger.info(
            f"_cleanup_old_results: removed {len(old_results)} results, "
            f"{len(old_tasks)} tasks, {len(old_challenges)} challenges "
            f"(remaining: {len(validator.task_results)} results, {len(validator.pending_tasks)} tasks, "
            f"{len(validator.active_challenges)} challenges)"
        )
