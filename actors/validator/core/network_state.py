"""Metagraph synchronization and BeamCore orchestrator discovery."""

import logging
from datetime import datetime

from core.models import OrchestratorInfo

logger = logging.getLogger(__name__)


async def sync_metagraph(validator) -> None:
    if validator.metagraph is None:
        return
    try:
        validator.metagraph.sync(subtensor=validator.subtensor)
        if validator.fiber_chain is not None:
            try:
                validator._fiber_nodes = validator.fiber_chain.get_nodes_by_hotkey()
                logger.debug(f"Fiber nodes refreshed: {len(validator._fiber_nodes)} nodes")
            except Exception as exc:
                logger.warning(f"Failed to refresh Fiber nodes: {exc}")
        logger.debug("Metagraph synced")
    except Exception as exc:
        logger.warning(f"Failed to sync metagraph: {exc}")


async def update_connections(validator) -> None:
    if validator.settings.local_mode:
        logger.debug("Keeping local mode connections")
        return
    if validator.metagraph is None:
        logger.debug("Skipping connection update - no metagraph")
        return

    registered_hotkeys = set(validator.orchestrators.keys())
    validator.connections.clear()
    skipped_unregistered = 0
    for uid in range(len(validator.metagraph.hotkeys)):
        hotkey = validator.metagraph.hotkeys[uid]
        if hotkey not in registered_hotkeys:
            skipped_unregistered += 1
            continue
        axon = validator.metagraph.axons[uid]
        validator.connections[uid] = {
            "uid": uid,
            "hotkey": hotkey,
            "ip": axon.ip,
            "port": axon.port,
            "last_seen": datetime.utcnow(),
        }
    logger.debug(
        f"Updated connections: {len(validator.connections)} registered, "
        f"{skipped_unregistered} skipped (not registered with BeamCore)"
    )


async def discover_orchestrators(validator, subnet_core_available: bool) -> None:
    if not (subnet_core_available and validator.subnet_core_client and validator.wallet):
        return
    try:
        result = await validator.subnet_core_client.get_latest_epoch_summary()
        orchestrators = result.get("orchestrators", [])
        if not orchestrators:
            return
        added = 0
        for item in orchestrators:
            hotkey = item.get("hotkey", "")
            if not hotkey:
                continue
            uid = item.get("uid")
            if hotkey not in validator.orchestrators:
                validator.orchestrators[hotkey] = OrchestratorInfo(
                    url="via-subnetcore",
                    hotkey=hotkey,
                    uid=uid,
                    is_healthy=True,
                )
                added += 1
            else:
                existing = validator.orchestrators[hotkey]
                if uid is not None and existing.uid is None:
                    existing.uid = uid
            if uid is not None:
                validator._beamcore_worker_counts[uid] = 0
        if added:
            logger.info(
                f"SubnetCore discovery (epoch summary): added {added} new orchestrator(s), "
                f"total={len(validator.orchestrators)}"
            )
    except Exception as exc:
        logger.warning(f"BeamCore orchestrator discovery failed: {exc}")
