"""Bandwidth challenge creation and delivery transports."""

import base64
import logging
import os
import time
from typing import Optional

from core._beam_stubs import (
    CANARY_SIZE_BYTES,
    BandwidthChallenge,
    ChunkTransfer,
    Task,
    sha256,
)
from core.models import ChallengeResult, OrchestratorInfo

logger = logging.getLogger(__name__)


async def _generate_and_send_challenges(validator) -> None:
    """Generate bandwidth challenges and send to connections via dendrite"""
    for uid, conn in validator.connections.items():
        active_for_uid = [
            c
            for c in validator.active_challenges.values()
            if c.get("uid") == uid and c.get("status") == "pending"
        ]
        if len(active_for_uid) >= 2:
            continue

        challenge = await validator._create_and_send_challenge(uid, conn)
        if challenge:
            validator.tasks_this_epoch += 1

async def _issue_challenges(validator) -> None:
    """Issue bandwidth challenges to verify Orchestrator capacity."""
    for hotkey, info in validator.orchestrators.items():
        if not info.is_healthy:
            continue

        active_for_orchestrator = [
            c
            for c in validator.active_challenges.values()
            if c.get("orchestrator") == hotkey and c.get("status") == "pending"
        ]

        if len(active_for_orchestrator) >= 2:
            continue

        try:
            result = await validator._issue_orchestrator_challenge(info)
            if result:
                logger.info(
                    f"Challenge {result.challenge_id[:16]}... "
                    f"to Orchestrator: {result.bandwidth_mbps:.2f} Mbps"
                )
        except Exception as e:
            logger.error(f"Failed to issue challenge to {hotkey[:16]}: {e}")

async def _create_and_send_challenge(
    validator,
    uid: int,
    connection: dict,
) -> Optional[BandwidthChallenge]:
    """Create a bandwidth challenge and send to connection"""
    nonce = os.urandom(32)
    task_id = sha256(nonce + validator.hotkey.encode()).hex()

    chunk_data = os.urandom(validator.settings.chunk_size_bytes)
    chunk_hash = sha256(chunk_data).hex()

    canary = os.urandom(CANARY_SIZE_BYTES)
    canary_offset = int.from_bytes(os.urandom(4), "big") % (
        validator.settings.chunk_size_bytes - CANARY_SIZE_BYTES
    )

    chunk_with_canary = bytearray(chunk_data)
    chunk_with_canary[canary_offset : canary_offset + len(canary)] = canary
    chunk_data = bytes(chunk_with_canary)
    chunk_hash = sha256(chunk_data).hex()

    timeout_us = validator.settings.job_timeout_seconds * 1_000_000
    deadline_us = int(time.time() * 1_000_000) + timeout_us

    challenge = BandwidthChallenge(
        task_id=task_id,
        challenge_nonce=nonce.hex(),
        chunk_hash=chunk_hash,
        chunk_size=validator.settings.chunk_size_bytes,
        deadline_us=deadline_us,
        canary=canary.hex(),
        canary_offset=canary_offset,
        path=[connection["hotkey"]],
        expected_hops=1,
    )

    validator.active_challenges[task_id] = {
        "uid": uid,
        "connection": connection,
        "challenge": challenge,
        "chunk_data": chunk_data,
        "canary": canary,
        "canary_offset": canary_offset,
        "created_at": time.time(),
        "status": "pending",
    }

    task = Task(
        task_id=bytes.fromhex(task_id),
        validator_hotkey=validator.hotkey,
        chunk_hash=bytes.fromhex(chunk_hash),
        chunk_size=validator.settings.chunk_size_bytes,
        deadline=deadline_us,
        canary=canary,
        canary_offset=canary_offset,
        path=[connection["hotkey"]],
        created_at=int(time.time() * 1_000_000),
    )
    validator.pending_tasks[task_id] = task

    if connection.get("is_local") and validator._http_session:
        return await validator._send_challenge_http(uid, task_id, chunk_data, challenge, connection)
    elif validator.dendrite:
        return await validator._send_challenge_dendrite(uid, task_id, chunk_data, challenge)
    return None

async def _issue_orchestrator_challenge(
    validator,
    orchestrator: OrchestratorInfo,
) -> Optional[ChallengeResult]:
    """Bandwidth challenge flow has been removed from the BeamCore validator path."""
    logger.debug("Challenge flow is disabled for orchestrator %s", orchestrator.hotkey[:16])
    return None

async def _send_challenge_http(
    validator,
    uid: int,
    task_id: str,
    chunk_data: bytes,
    challenge: BandwidthChallenge,
    connection: dict,
) -> Optional[BandwidthChallenge]:
    """Legacy HTTP challenge path removed."""
    return None

async def _send_chunk_data_http(
    validator,
    uid: int,
    task_id: str,
    chunk_data: bytes,
    challenge: BandwidthChallenge,
    connection: dict,
) -> bool:
    """Legacy HTTP challenge data path removed."""
    return False

async def _send_challenge_dendrite(
    validator,
    uid: int,
    task_id: str,
    chunk_data: bytes,
    challenge: BandwidthChallenge,
) -> Optional[BandwidthChallenge]:
    """Send challenge via Bittensor dendrite (for mainnet/testnet)"""
    if validator.dendrite is None or validator.metagraph is None:
        return None

    try:
        axon = validator.metagraph.axons[uid]
        response = await validator.dendrite.call(
            target_axon=axon,
            circuit=challenge,
            timeout=10.0,
        )

        if response and response.accepted:
            logger.info(f"Challenge {task_id[:16]}... accepted by UID {uid}")
            validator.active_challenges[task_id]["status"] = "accepted"
            validator.active_challenges[task_id]["worker_assigned"] = response.worker_assigned

            await validator._send_chunk_data(uid, task_id, chunk_data, challenge)

            return response
        else:
            validator.active_challenges[task_id]["status"] = "rejected"

    except Exception as e:
        logger.error(f"Failed to send challenge to UID {uid}: {e}")
        validator.active_challenges[task_id]["status"] = "error"

    return None

async def _send_chunk_data(
    validator,
    uid: int,
    task_id: str,
    chunk_data: bytes,
    challenge: BandwidthChallenge,
) -> bool:
    """Send actual chunk data to connection for bandwidth proof"""
    if validator.dendrite is None or validator.metagraph is None:
        return False

    chunk_transfer = ChunkTransfer(
        task_id=task_id,
        chunk_hash=challenge.chunk_hash,
        chunk_size=challenge.chunk_size,
        chunk_data=base64.b64encode(chunk_data).decode(),
        canary=challenge.canary,
        canary_offset=challenge.canary_offset,
        hop_index=0,
    )

    try:
        axon = validator.metagraph.axons[uid]
        send_time = time.time()
        validator.active_challenges[task_id]["chunk_sent_at"] = send_time

        response = await validator.dendrite.call(
            target_axon=axon,
            circuit=chunk_transfer,
            timeout=validator.settings.job_timeout_seconds,
        )

        if response and response.received:
            receive_time = response.receive_time_us / 1_000_000
            validator.active_challenges[task_id]["status"] = "chunk_received"
            validator.active_challenges[task_id]["receive_time"] = receive_time
            return True
        else:
            validator.active_challenges[task_id]["status"] = "chunk_failed"

    except Exception as e:
        logger.error(f"Failed to send chunk for task {task_id[:16]}...: {e}")
        validator.active_challenges[task_id]["status"] = "error"

    return False
