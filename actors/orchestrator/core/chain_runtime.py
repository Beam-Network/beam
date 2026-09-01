"""Chain RPC isolation, metagraph synchronization, and epoch advancement."""

import asyncio
import logging
import time
from datetime import datetime, timedelta

logger = logging.getLogger(__name__)


def _find_our_uid(orchestrator) -> None:
    if orchestrator.metagraph is None or orchestrator.hotkey is None:
        return
    for uid in range(len(orchestrator.metagraph.hotkeys)):
        if orchestrator.metagraph.hotkeys[uid] == orchestrator.hotkey:
            logger.info(f"Found our UID: {uid}")
            orchestrator.our_uid = uid
            return
    logger.warning(f"Hotkey {orchestrator.hotkey[:16]}... not found in metagraph")

def get_our_emission(orchestrator) -> float:
    """Get our emission converted from alpha to TAO.

    Uses a cached subnet price only. Price refreshes run in a background
    thread so slow chain RPCs cannot block NATS control handling.
    """
    if orchestrator.metagraph is None or orchestrator.our_uid is None:
        return 0.0
    try:
        emission_alpha = float(orchestrator.metagraph.E[orchestrator.our_uid])
    except Exception as e:
        logger.error(f"Error getting emission: {e}")
        return 0.0
    if emission_alpha <= 0 or not orchestrator.subtensor:
        return emission_alpha

    now = time.time()
    cache_ttl = 900
    alpha_per_tao = getattr(orchestrator, "_cached_alpha_per_tao", 0)
    cache_age = now - getattr(orchestrator, "_cached_price_at", 0)
    if alpha_per_tao > 0 and cache_age <= cache_ttl:
        emission_tao = emission_alpha / alpha_per_tao
        logger.debug(
            "Emission: %.4f alpha -> %.9f TAO (rate: %.2f alpha/TAO)",
            emission_alpha,
            emission_tao,
            alpha_per_tao,
        )
        return emission_tao
    if alpha_per_tao > 0:
        logger.debug("Using stale subnet price cache for emission conversion")
        return emission_alpha / alpha_per_tao

    return emission_alpha

# Note: _discover_validators removed - BeamCore handles PRISM evidence centrally

# =========================================================================
# Background Loops
# =========================================================================

async def _run_chain_rpc(orchestrator, label: str, func):
    """Run blocking Bittensor RPCs outside the asyncio event loop."""
    async with orchestrator._chain_rpc_lock:
        started = time.monotonic()
        result = await asyncio.to_thread(func)
        elapsed = time.monotonic() - started
        if elapsed >= 5:
            logger.warning("%s took %.1fs", label, elapsed)
        return result

def _sync_metagraph_from_chain(orchestrator) -> None:
    if orchestrator.metagraph and orchestrator.subtensor:
        orchestrator.metagraph.sync(subtensor=orchestrator.subtensor)

async def _sync_metagraph_from_chain_async(orchestrator) -> None:
    await orchestrator._run_chain_rpc("metagraph sync", orchestrator._sync_metagraph_from_chain)

def _refresh_subnet_price_cache(orchestrator) -> None:
    if orchestrator.subtensor is None:
        return
    price = orchestrator.subtensor.get_subnet_price(orchestrator.settings.netuid)
    orchestrator._cached_alpha_per_tao = float(price)
    orchestrator._cached_price_at = time.time()

async def _refresh_subnet_price_cache_async(orchestrator) -> None:
    try:
        await orchestrator._run_chain_rpc("subnet price refresh", orchestrator._refresh_subnet_price_cache)
    except Exception as exc:
        logger.warning("Could not refresh subnet price cache: %s", exc)

async def _sync_epoch_from_chain_async(orchestrator) -> None:
    await orchestrator._run_chain_rpc("chain epoch sync", orchestrator._sync_epoch_from_chain)

async def _metagraph_sync_loop(orchestrator) -> None:
    """Background loop for syncing metagraph."""
    sync_interval = 300  # Sync every 5 minutes (validators/stake change infrequently)

    while orchestrator._running:
        try:
            await asyncio.sleep(sync_interval)
            await orchestrator._sync_metagraph_from_chain_async()

            # Always re-check UID after sync — hotkey may have moved to a
            # different UID slot after re-registration (stale UID causes
            # task evidence to be attributed by BeamCore).
            old_uid = orchestrator.our_uid
            orchestrator._find_our_uid()
            if orchestrator.our_uid != old_uid and orchestrator.subnet_core_client is not None:
                logger.info(
                    f"UID changed {old_uid} → {orchestrator.our_uid}, updating SubnetCoreClient"
                )
                orchestrator.subnet_core_client.orchestrator_uid = orchestrator.our_uid

            await orchestrator._refresh_subnet_price_cache_async()
            orchestrator.distribute_rewards_to_workers()
            # Note: Validator discovery removed - BeamCore handles PRISM evidence centrally

        except asyncio.CancelledError:
            break
        except Exception as e:
            logger.error(f"Error syncing metagraph: {e}")

def _sync_epoch_from_chain(orchestrator) -> None:
    """Set current_epoch from the chain block number to match validator epoch numbering."""
    if orchestrator.subtensor:
        try:
            block = orchestrator.subtensor.block
            # Use 360 blocks per epoch to match SubnetCore's epoch calculation
            epoch_length_blocks = 360
            chain_epoch = block // epoch_length_blocks

            # Always sync if epochs differ significantly or chain_epoch is newer
            # This handles the case where old epoch calculation (block//25) was used
            # and produced epochs like 258007 which are > correct epoch ~17925
            should_sync = (
                chain_epoch > orchestrator.current_epoch  # Normal case: new epoch
                or orchestrator.current_epoch > 100_000  # Old epoch calculation was used
                or chain_epoch != orchestrator.current_epoch  # Any mismatch (first sync)
            )

            if should_sync and chain_epoch != orchestrator.current_epoch:
                logger.info(
                    f"Synced epoch from chain: {orchestrator.current_epoch} -> {chain_epoch} "
                    f"(block={block})"
                )
                orchestrator.current_epoch = chain_epoch
        except Exception as e:
            logger.warning(f"Failed to sync epoch from chain: {e}")

async def _epoch_management_loop(orchestrator) -> None:
    """Background loop for managing epochs."""
    epoch_duration = timedelta(minutes=5)
    chain_sync_interval = 600  # Re-sync with chain every 10 minutes
    last_chain_sync = time.time()

    while orchestrator._running:
        try:
            await asyncio.sleep(60)

            # Re-sync epoch from chain periodically (every 10 min) to correct drift
            now = time.time()
            if now - last_chain_sync >= chain_sync_interval:
                await orchestrator._sync_epoch_from_chain_async()
                last_chain_sync = now

            # Check if epoch should change (time-based fallback)
            if datetime.utcnow() - orchestrator.epoch_start_time >= epoch_duration:
                await orchestrator._advance_epoch()

        except asyncio.CancelledError:
            break
        except Exception as e:
            logger.error(f"Error in epoch management: {e}")

async def _advance_epoch(orchestrator) -> None:
    """Advance to next epoch.  Must always increment the counter."""
    prev_epoch = orchestrator.current_epoch
    summary = None

    await orchestrator._refresh_subnet_price_cache_async()

    try:
        summary = orchestrator._build_epoch_summary()
        orchestrator.epoch_summaries[orchestrator.current_epoch] = summary
    except Exception as e:
        logger.error(f"Error building epoch summary: {e}")

    try:
        orchestrator.distribute_rewards_at_epoch_end()
    except Exception as e:
        logger.error(f"Error distributing epoch rewards: {e}")


    for worker in orchestrator.workers.values():
        worker.bytes_relayed_epoch = 0
        worker.rewards_earned_epoch = 0

    orchestrator._reward_mgr.epoch_start_emission = orchestrator.get_our_emission()

    # Always advance — never let a sub-step failure block epoch progression
    orchestrator.current_epoch += 1
    orchestrator.epoch_start_time = datetime.utcnow()

    tasks = summary.total_tasks if summary else "?"
    bytes_relayed = summary.total_bytes_relayed if summary else "?"
    logger.info(
        f"Advanced to epoch {orchestrator.current_epoch} "
        f"(previous epoch {prev_epoch}: {tasks} tasks, {bytes_relayed} bytes)"
    )
