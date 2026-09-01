"""Orchestrator initialization, startup, and shutdown lifecycle."""

import asyncio
import logging
import time
from typing import Optional

import bittensor as bt

from clients import close_subnet_core_client

logger = logging.getLogger(__name__)

SUBTENSOR_INIT_MAX_ATTEMPTS = 5
SUBTENSOR_INIT_BASE_DELAY_SECONDS = 2.0


async def initialize(orchestrator) -> None:
    """Initialize the Orchestrator."""
    logger.info("Initializing BEAM Orchestrator...")

    # Skip bittensor initialization in local mode
    if orchestrator.settings.local_mode:
        logger.info(
            "Running in LOCAL MODE - skipping Bittensor wallet/subtensor initialization"
        )
        orchestrator.wallet = None
        orchestrator.hotkey = orchestrator.settings.local_orchestrator_hotkey
        orchestrator.subtensor = None
        orchestrator.metagraph = None
        orchestrator.our_uid = 0
    else:
        # Load wallet (for signing reports)
        orchestrator.wallet = bt.Wallet(
            name=orchestrator.settings.wallet_name,
            hotkey=orchestrator.settings.wallet_hotkey,
            path=orchestrator.settings.wallet_path,
        )
        orchestrator.hotkey = orchestrator.wallet.hotkey.ss58_address
        logger.info(f"Orchestrator wallet: {orchestrator.hotkey}")

        if orchestrator.settings.uid is not None:
            # UID pre-configured via ORCHESTRATOR_UID — skip slow subtensor init
            orchestrator.our_uid = orchestrator.settings.uid
            orchestrator.subtensor = None
            orchestrator.metagraph = None
            logger.info("Using configured orchestrator UID %s — skipping subtensor init", orchestrator.our_uid)
        else:
            orchestrator._initialize_subtensor_and_metagraph_with_retry()

            # Find our UID in the metagraph
            orchestrator._find_our_uid()

    # Initialize SubnetCoreClient for API-based data operations
    await orchestrator._init_subnet_core_client()

    # Skip chain-dependent initialization in local mode
    if not orchestrator.settings.local_mode and orchestrator.subtensor is not None and orchestrator.metagraph is not None:
        # Sync epoch from chain block number so it matches the validator's numbering
        await orchestrator._sync_epoch_from_chain_async()
        await orchestrator._refresh_subnet_price_cache_async()

        # Initialize epoch emission tracking
        orchestrator._reward_mgr.epoch_start_emission = orchestrator.get_our_emission()
        orchestrator._reward_mgr.last_emission_check = orchestrator._reward_mgr.epoch_start_emission
        logger.info(f"Initial emission: {orchestrator._reward_mgr.epoch_start_emission:.6f} ध")

        # Note: Validator discovery removed - BeamCore handles PRISM evidence centrally
    else:
        logger.info("Skipping chain sync and emission tracking")

    # Initialize orchestrator manager for incentive mechanism
    await orchestrator._init_orch_manager()

    # Add mock worker if requested
    if orchestrator.settings.add_mock_worker:
        await orchestrator._add_local_mock_worker()
        logger.info("Added mock worker for testing")

    logger.info("Orchestrator initialized")

def _initialize_subtensor_and_metagraph_with_retry(orchestrator) -> None:
    target = orchestrator.settings.subtensor_address or orchestrator.settings.subtensor_network
    last_error: Optional[Exception] = None

    for attempt in range(1, SUBTENSOR_INIT_MAX_ATTEMPTS + 1):
        try:
            # The public test endpoint can intermittently return transient
            # internal errors during runtime bootstrap. Treat those as
            # retryable instead of aborting the whole orchestrator process.
            if orchestrator.settings.subtensor_address:
                orchestrator.subtensor = bt.Subtensor(network=orchestrator.settings.subtensor_address)
            else:
                orchestrator.subtensor = bt.Subtensor(network=orchestrator.settings.subtensor_network)
            logger.info(f"Connected to subtensor: {orchestrator.subtensor.network}")

            orchestrator.metagraph = bt.Metagraph(netuid=orchestrator.settings.netuid)
            orchestrator.metagraph.sync(subtensor=orchestrator.subtensor)
            return
        except Exception as exc:
            last_error = exc
            if attempt >= SUBTENSOR_INIT_MAX_ATTEMPTS:
                break

            delay_seconds = SUBTENSOR_INIT_BASE_DELAY_SECONDS * attempt
            logger.warning(
                "Subtensor bootstrap attempt %s/%s failed for %s: %s. Retrying in %.1fs",
                attempt,
                SUBTENSOR_INIT_MAX_ATTEMPTS,
                target,
                exc,
                delay_seconds,
            )
            time.sleep(delay_seconds)

    assert last_error is not None
    raise RuntimeError(
        f"Failed to initialize subtensor/metagraph after {SUBTENSOR_INIT_MAX_ATTEMPTS} attempts for {target}"
    ) from last_error

async def start(orchestrator) -> None:
    """Start the Orchestrator background services."""
    orchestrator._running = True

    def running() -> bool:
        return orchestrator._running

    orchestrator._background_tasks = [
        asyncio.create_task(orchestrator._metagraph_sync_loop()),
        asyncio.create_task(orchestrator._worker_mgr.worker_health_loop(running)),
        asyncio.create_task(orchestrator._worker_mgr.worker_sync_loop(running, interval_seconds=60)),
        asyncio.create_task(orchestrator._epoch_management_loop()),
    ]
    logger.info("Worker sync loop started (syncs from SubnetCore every 60s)")

    logger.info("Orchestrator started")

async def stop(orchestrator) -> None:
    """Stop the Orchestrator."""
    orchestrator._running = False

    for task in orchestrator._background_tasks:
        task.cancel()
        try:
            await task
        except asyncio.CancelledError:
            pass

    if hasattr(orchestrator.worker_gateway, "stop"):
        await orchestrator.worker_gateway.stop()

    if SUBNET_CORE_CLIENT_AVAILABLE and orchestrator.subnet_core_client:
        # Stop HTTP polling
        if hasattr(orchestrator.subnet_core_client, "stop_polling"):
            await orchestrator.subnet_core_client.stop_polling()
        await close_subnet_core_client()
        logger.info("SubnetCoreClient closed")

    logger.info("Orchestrator stopped")
