"""Initialization, health, and long-running lifecycle of the validator."""

import asyncio
import logging
import os
from datetime import datetime
from typing import Optional

import aiohttp
import bittensor as bt
from chain import FiberChain, FiberNode
from core.redundancy import create_redundancy_system, initialize_with_recovery

logger = logging.getLogger(__name__)


async def initialize(validator) -> None:
    """Initialize the Validator node"""
    logger.debug("Initializing Validator node...")

    if validator.settings.local_mode:
        # Local development mode - skip Bittensor network connection
        logger.debug("Running in LOCAL MODE - skipping Bittensor network connection")
        await validator._initialize_local_mode()
    else:
        # Mainnet/Testnet mode with Bittensor
        await validator._initialize_bittensor_mode()

    logger.debug("Validator node initialized")

async def _initialize_local_mode(validator) -> None:
    """Initialize validator in local development mode (no Bittensor connection)"""
    # Create a mock wallet for local testing
    validator.wallet = bt.Wallet(
        name=validator.settings.wallet_name,
        hotkey=validator.settings.wallet_hotkey,
        path=validator.settings.wallet_path,
    )
    validator.hotkey = validator.wallet.hotkey.ss58_address

    # Set mock values for local development
    validator.uid = 1
    validator.is_registered = True

    # Connect to subtensor for testnet
    if validator.settings.subtensor_address:
        validator.subtensor = bt.Subtensor(network=validator.settings.subtensor_address)
    else:
        validator.subtensor = bt.Subtensor(network=validator.settings.subtensor_network)
    logger.debug(f"Connected to subtensor: {validator.subtensor.network}")

    # Load metagraph for testnet data
    try:
        validator.metagraph = bt.Metagraph(
            netuid=validator.settings.netuid,
            network=validator.subtensor.chain_endpoint,
        )
        validator.metagraph.sync(subtensor=validator.subtensor)
        logger.debug(f"Metagraph loaded with {len(validator.metagraph.hotkeys)} workers")
    except Exception as e:
        logger.warning(f"Failed to load metagraph: {e}")
        validator.metagraph = None

    # Initialize Fiber chain interface
    if os.getenv("BEAM_VALIDATOR_SKIP_FIBER", "").lower() in ("true", "1", "yes"):
        validator.fiber_chain = None
        validator._fiber_nodes = {}
        logger.debug("Fiber chain skipped (BEAM_VALIDATOR_SKIP_FIBER)")
    else:
        try:
            validator.fiber_chain = FiberChain(
                subtensor_network=validator.settings.subtensor_network,
                subtensor_address=validator.settings.subtensor_address,
                netuid=validator.settings.netuid,
            )
            validator._fiber_nodes = validator.fiber_chain.get_nodes_by_hotkey()
            logger.debug(f"Fiber chain initialized with {len(validator._fiber_nodes)} nodes")
        except Exception as e:
            logger.warning("Fiber chain init failed: %s", e, exc_info=True)
            validator.fiber_chain = None

    # Create HTTP session for local mode communication
    validator._http_session = aiohttp.ClientSession(timeout=aiohttp.ClientTimeout(total=60))

    # Add local orchestrator as a connection
    validator.connections[1] = {
        "uid": 1,
        "hotkey": "local-orchestrator",
        "ip": "127.0.0.1",
        "port": 8000,
        "url": validator.settings.orchestrator_url,
        "last_seen": datetime.utcnow(),
        "is_local": True,
    }

    # Discover orchestrators
    await validator._discover_orchestrators()

    logger.debug(f"Local mode initialized with hotkey: {validator.hotkey}")

async def _initialize_bittensor_mode(validator) -> None:
    """Initialize validator in Bittensor network mode"""
    # Load wallet
    validator.wallet = bt.Wallet(
        name=validator.settings.wallet_name,
        hotkey=validator.settings.wallet_hotkey,
        path=validator.settings.wallet_path,
    )
    validator.hotkey = validator.wallet.hotkey.ss58_address
    logger.debug(f"Wallet loaded: {validator.hotkey}")

    # Connect to subtensor
    if validator.settings.subtensor_address:
        validator.subtensor = bt.Subtensor(network=validator.settings.subtensor_address)
    else:
        validator.subtensor = bt.Subtensor(network=validator.settings.subtensor_network)
    logger.debug(f"Connected to subtensor: {validator.subtensor.network}")

    # Load metagraph
    validator.metagraph = bt.Metagraph(
        netuid=validator.settings.netuid,
        network=validator.subtensor.chain_endpoint,
    )
    validator.metagraph.sync(subtensor=validator.subtensor)

    # Initialize Fiber chain interface
    if os.getenv("BEAM_VALIDATOR_SKIP_FIBER", "").lower() in ("true", "1", "yes"):
        validator.fiber_chain = None
        validator._fiber_nodes = {}
        logger.debug("Fiber chain skipped (BEAM_VALIDATOR_SKIP_FIBER)")
    else:
        try:
            validator.fiber_chain = FiberChain(
                subtensor_network=validator.settings.subtensor_network,
                subtensor_address=validator.settings.subtensor_address,
                netuid=validator.settings.netuid,
            )
            validator._fiber_nodes = validator.fiber_chain.get_nodes_by_hotkey()
            logger.debug(f"Fiber chain initialized with {len(validator._fiber_nodes)} nodes")
        except Exception as e:
            logger.warning("Fiber chain init failed: %s", e, exc_info=True)
            validator.fiber_chain = None

    # Check registration
    await validator._check_registration()

    # Seed last_weight_block from chain so a restart doesn't immediately retry set_weights
    if validator.uid is not None and validator.subtensor is not None:
        try:
            last_update_vec = validator.subtensor.query_module(
                "SubtensorModule", "LastUpdate", [validator.settings.netuid]
            )
            vec = (last_update_vec.value or []) if last_update_vec else []
            if validator.uid < len(vec):
                validator.last_weight_block = int(vec[validator.uid])
                _sw_rows = [
                    ("UID",        str(validator.uid)),
                    ("Last Block", str(validator.last_weight_block)),
                ]
                _sw_kw = max(len(k) for k, _ in _sw_rows)
                _sw_vw = max(len(v) for _, v in _sw_rows)
                _sw_in = _sw_kw + _sw_vw + 5
                print("\n".join([
                    f"┌{'─' * _sw_in}┐",
                    f"│{'Chain Weight Block':^{_sw_in}}│",
                    f"├{'─' * (_sw_kw + 2)}┬{'─' * (_sw_vw + 2)}┤",
                    *[f"│ {k:<{_sw_kw}} │ {v:<{_sw_vw}} │" for k, v in _sw_rows],
                    f"└{'─' * (_sw_kw + 2)}┴{'─' * (_sw_vw + 2)}┘",
                ]), flush=True)
        except Exception as _exc:
            logger.debug("Could not read LastUpdate from chain: %s", _exc)

    # Cache the chain's weights_rate_limit so we can compute wait times without hitting chain
    if validator.subtensor is not None:
        try:
            validator._chain_weights_rate_limit = validator.subtensor.weights_rate_limit(validator.settings.netuid) or 0
            logger.info("Chain weights_rate_limit for netuid %s: %s blocks", validator.settings.netuid, validator._chain_weights_rate_limit)
        except Exception as _exc:
            logger.debug("Could not read weights_rate_limit: %s", _exc)

    # Setup dendrite for querying connections
    validator.dendrite = bt.Dendrite(wallet=validator.wallet)

    # Create HTTP session
    validator._http_session = aiohttp.ClientSession(timeout=aiohttp.ClientTimeout(total=60))

async def _check_registration(validator) -> None:
    """Check if this hotkey is registered on the subnet"""
    # Try Fiber first if available
    uid = validator._get_uid_for_hotkey(validator.hotkey)

    if uid is not None:
        validator.uid = uid
        validator.is_registered = True
        logger.debug(f"Registered on subnet {validator.settings.netuid} with UID {validator.uid}")
    else:
        validator.is_registered = False
        logger.warning(f"Hotkey {validator.hotkey} not registered on subnet")

def _get_uid_for_hotkey(validator, hotkey: str) -> Optional[int]:
    """Get UID for a hotkey using Fiber or metagraph fallback."""
    # Try Fiber first (if available and cached)
    if validator._fiber_nodes:
        node = validator._fiber_nodes.get(hotkey)
        if node:
            logger.debug(f"_get_uid_for_hotkey: {hotkey[:16]}... -> UID {node.uid} (via Fiber)")
            return node.uid

    # Fallback to metagraph
    if validator.metagraph and hotkey in validator.metagraph.hotkeys:
        uid = validator.metagraph.hotkeys.index(hotkey)
        logger.debug(f"_get_uid_for_hotkey: {hotkey[:16]}... -> UID {uid} (via metagraph)")
        return uid

    logger.warning(
        f"_get_uid_for_hotkey: {hotkey[:16]}... -> NOT FOUND (fiber_nodes={len(validator._fiber_nodes)}, metagraph={'yes' if validator.metagraph else 'no'})"
    )
    return None

def _get_node_info(validator, hotkey: str) -> Optional[FiberNode]:
    """Get full node info for a hotkey using Fiber."""
    if validator._fiber_nodes:
        node = validator._fiber_nodes.get(hotkey)
        if node:
            logger.debug(f"_get_node_info: found node for {hotkey[:16]}... uid={node.uid}")
        else:
            logger.debug(f"_get_node_info: no Fiber node for {hotkey[:16]}...")
        return node
    logger.debug(f"_get_node_info: no Fiber nodes available, cannot look up {hotkey[:16]}...")
    return None

async def start(validator) -> None:
    """Start the Validator node"""
    if not validator.is_registered:
        logger.error("Cannot start: not registered on subnet")
        return

    # Initialize redundancy system
    await validator._initialize_redundancy()

    # Load pending challenges from database (state recovery)
    await validator._load_pending_challenges()

    validator._running = True
    validator._main_loop_task = asyncio.create_task(validator._main_loop())
    validator._heartbeat_task = asyncio.create_task(validator._heartbeat_loop())

    logger.info("Validator node started")

async def _initialize_redundancy(validator) -> None:
    """Initialize redundancy and failover systems"""
    try:
        validator.health_monitor, validator.checkpoint_manager, validator.recovery_manager = (
            create_redundancy_system(validator)
        )

        recovered = await initialize_with_recovery(validator)
        if recovered:
            logger.info("Validator state restored from checkpoint")

        await validator.health_monitor.start()
        await validator.checkpoint_manager.start()

        logger.info("Redundancy system initialized")

    except Exception as e:
        logger.warning(f"Failed to initialize redundancy system: {e}")

async def stop(validator) -> None:
    """Stop the Validator node"""
    validator._running = False

    # Cancel heartbeat task
    if hasattr(validator, "_heartbeat_task") and validator._heartbeat_task:
        validator._heartbeat_task.cancel()
        try:
            await validator._heartbeat_task
        except asyncio.CancelledError:
            pass

    if validator._main_loop_task:
        validator._main_loop_task.cancel()
        try:
            await validator._main_loop_task
        except asyncio.CancelledError:
            pass

    # Stop redundancy systems
    if validator.health_monitor:
        await validator.health_monitor.stop()
    if validator.checkpoint_manager:
        await validator.checkpoint_manager.stop()

    # Close HTTP session if open
    if validator._http_session and not validator._http_session.closed:
        await validator._http_session.close()
        validator._http_session = None

    logger.info("Validator node stopped")

async def _submit_beamcore_heartbeat(validator, subnet_core_available: bool) -> None:
    """POST /validators/heartbeat to BeamCore."""
    if not subnet_core_available or not validator.subnet_core_client:
        return

    health_info = None
    if validator.health_monitor:
        try:
            report = await validator.health_monitor.run_health_checks()
            health_info = {
                "status": report.get("status"),
                "checks_passed": report.get("checks_passed", 0),
                "checks_failed": report.get("checks_failed", 0),
            }
        except Exception:
            pass

    status = "online"
    if health_info and health_info.get("checks_failed", 0) > 0:
        status = "degraded"

    result = await validator.subnet_core_client.submit_heartbeat(
        validator_uid=validator.uid,
        status=status,
        last_epoch_scored=validator.current_epoch or None,
        health_info=health_info,
        external_url=validator.settings.external_url,
    )
    api_key = result.get("api_key")
    if api_key:
        validator.subnet_core_client._api_key = api_key
    _hb_items = [
        ("Status", status),
        ("UID",    str(validator.uid)),
        ("Epoch",  str(validator.current_epoch) if validator.current_epoch else "—"),
    ]
    if api_key:
        _hb_items.append(("API Key", "received"))
    _hk, _hv = 7, max(len(v) for _, v in _hb_items)
    _hv = max(_hv, 8)
    _top   = f"┌{'─' * (_hk + _hv + 5)}┐"
    _title = f"│{'Heartbeat':^{_hk + _hv + 5}}│"
    _sep   = f"├{'─' * (_hk + 2)}┬{'─' * (_hv + 2)}┤"
    _body  = "\n".join(f"│ {k:<{_hk}} │ {v:<{_hv}} │" for k, v in _hb_items)
    _bot   = f"└{'─' * (_hk + 2)}┴{'─' * (_hv + 2)}┘"
    print("\n".join([_top, _title, _sep, _body, _bot]), flush=True)

async def _heartbeat_loop(validator) -> None:
    """Send periodic heartbeats to SubnetCore while running."""
    heartbeat_interval = max(5, int(validator.settings.heartbeat_interval_seconds))

    while validator._running:
        try:
            await validator._submit_beamcore_heartbeat()
        except asyncio.CancelledError:
            break
        except Exception as e:
            target = getattr(validator.subnet_core_client, "base_url", validator.settings.core_server_url)
            logger.warning(
                "Heartbeat error (%s) posting to %s: %s",
                type(e).__name__,
                target,
                e,
            )
            # Don't break - continue trying
        try:
            await asyncio.sleep(heartbeat_interval)
        except asyncio.CancelledError:
            break

async def _main_loop(validator) -> None:
    """Main validator loop"""
    while validator._running:
        try:
            # Sync metagraph
            await validator._sync_metagraph()

            # Discover orchestrators from BeamCore first, then filter connections
            await validator._discover_orchestrators()
            await validator._update_connections()


            # Generate and send bandwidth challenges
            # DISABLED: Challenges temporarily disabled - endpoint deprecated
            # await validator._generate_and_send_challenges()

            # Issue additional challenges
            # DISABLED: Challenges temporarily disabled - endpoint deprecated
            # await validator._issue_challenges()

            # Collect and verify local proof submissions
            await validator._collect_pob_results()
            # Spot-check proofs
            await validator._spot_check_proofs()

            # Update scores
            await validator._update_scores()

            # Check for epoch change and broadcast (must run before weight-setting
            # so validator.current_epoch is correct on the first main-loop iteration)
            await validator._check_epoch()

            # Set weights if needed
            await validator._maybe_set_weights()

            # Periodic cleanup
            await validator._expire_penalties_and_challenges()

        except Exception as e:
            logger.error(f"Error in main loop: {e}", exc_info=True)

            if validator.recovery_manager and validator.recovery_manager.should_attempt_recovery():
                logger.warning("Health issues detected, attempting recovery...")
                success = await validator.recovery_manager.attempt_recovery()
                if success:
                    logger.info("Recovery successful")
                else:
                    logger.error("Recovery failed")

        await asyncio.sleep(validator.settings.sync_interval)
