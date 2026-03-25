"""
Metagraph Sync - Metagraph synchronization, validator discovery, and registry management.
"""

import asyncio
import logging
import time
from typing import Any, Dict, Optional

import aiohttp

from .config import OrchestratorSettings

logger = logging.getLogger(__name__)


class MetagraphSync:
    """Manages metagraph synchronization, validator discovery, and registry registration."""

    def __init__(self, settings: OrchestratorSettings):
        self.settings = settings

    async def metagraph_sync_loop(
        self,
        running_flag,
        subtensor_ref,
        metagraph_ref,
        our_uid_ref,
        find_our_uid_fn,
        distribute_rewards_fn,
        discover_validators_fn,
    ) -> None:
        """Background loop for syncing metagraph."""
        while running_flag():
            try:
                await asyncio.sleep(60)
                metagraph = metagraph_ref()
                subtensor = subtensor_ref()
                if metagraph and subtensor:
                    metagraph.sync(subtensor=subtensor)

                if our_uid_ref() is None:
                    find_our_uid_fn()

                distribute_rewards_fn()
                await discover_validators_fn()

            except asyncio.CancelledError:
                break
            except Exception as e:
                logger.error(f"Error syncing metagraph: {e}")

    async def discover_validators(
        self,
        validators: dict,
        metagraph,
        our_uid,
        settings: OrchestratorSettings,
    ) -> None:
        """Discover validators from metagraph or manual config."""
        validators.clear()

        # Check for manual validator endpoints first
        if settings.validator_endpoints:
            for entry in settings.validator_endpoints.split(","):
                parts = entry.strip().split(":")
                if len(parts) >= 3:
                    hotkey = parts[0]
                    ip = parts[1]
                    port = int(parts[2])
                    validators[hotkey] = {
                        "uid": 0,
                        "stake": 0,
                        "ip": ip,
                        "port": port,
                    }
                    logger.info(f"Added manual validator: {hotkey[:16]}... at {ip}:{port}")

            if validators:
                logger.info(f"Using {len(validators)} manually configured validators (VALIDATOR_ENDPOINTS set)")
                return

        # Discover from metagraph
        if not metagraph:
            logger.warning(
                "No metagraph available for validator discovery. "
                "Payment proofs will NOT be submitted. "
                "Fix: ensure metagraph is synced or configure VALIDATOR_ENDPOINTS."
            )
            return

        logger.info(
            f"Discovering validators from metagraph: "
            f"netuid={metagraph.netuid}, neurons={len(metagraph.hotkeys)}, "
            f"min_stake={settings.min_validator_stake}"
        )

        skipped_low_stake = 0
        skipped_no_dividends = 0
        skipped_bad_axon = 0

        for uid in range(len(metagraph.hotkeys)):
            if uid == our_uid:
                continue

            stake = float(metagraph.S[uid])
            dividends = float(metagraph.D[uid]) if hasattr(metagraph, 'D') else 0.0

            if stake < settings.min_validator_stake:
                skipped_low_stake += 1
                continue

            if hasattr(metagraph, 'D') and dividends == 0:
                skipped_no_dividends += 1
                continue

            hotkey = metagraph.hotkeys[uid]
            axon = metagraph.axons[uid]

            # Validate axon has usable IP/port
            if not axon.ip or axon.ip == "0.0.0.0" or not axon.port:
                skipped_bad_axon += 1
                logger.debug(f"Skipping UID {uid}: invalid axon ip={axon.ip} port={axon.port}")
                continue

            validators[hotkey] = {
                "uid": uid,
                "stake": stake,
                "ip": axon.ip,
                "port": axon.port,
                "dividends": dividends,
            }

        logger.info(
            f"Validator discovery complete: found={len(validators)}, "
            f"skipped_low_stake={skipped_low_stake}, skipped_no_dividends={skipped_no_dividends}, "
            f"skipped_bad_axon={skipped_bad_axon}, our_uid={our_uid}"
        )

        if len(validators) == 0:
            logger.error(
                "NO VALIDATORS DISCOVERED! Payment proofs will NOT be submitted. "
                "Check: (1) metagraph is synced, (2) min_validator_stake is not too high, "
                "(3) validators have dividends > 0, (4) validator axons have valid IP/port. "
                "Or configure VALIDATOR_ENDPOINTS manually."
            )

    async def discover_validators_manual(self, validators: dict, settings: OrchestratorSettings) -> None:
        """Discover validators from manual config only (for local mode)."""
        validators.clear()

        if settings.validator_endpoints:
            for entry in settings.validator_endpoints.split(","):
                parts = entry.strip().split(":")
                if len(parts) >= 3:
                    hotkey = parts[0]
                    ip = parts[1]
                    port = int(parts[2])
                    validators[hotkey] = {
                        "uid": 0,
                        "stake": 0,
                        "ip": ip,
                        "port": port,
                    }
                    logger.info(f"Added manual validator: {hotkey[:16]}... at {ip}:{port}")

            if validators:
                logger.info(f"Using {len(validators)} manually configured validators")

    def find_our_uid(self, metagraph, hotkey: str) -> Optional[int]:
        """Find our UID in the metagraph."""
        if metagraph is None or hotkey is None:
            return None

        try:
            for uid in range(len(metagraph.hotkeys)):
                if metagraph.hotkeys[uid] == hotkey:
                    logger.info(f"Found our UID: {uid}")
                    return uid

            logger.warning(f"Hotkey {hotkey[:16]}... not found in metagraph")
        except Exception as e:
            logger.error(f"Error finding UID: {e}")
        return None

    def get_our_emission(self, metagraph, our_uid) -> float:
        """Get our current emission from the metagraph (in alpha/ध)."""
        if metagraph is None or our_uid is None:
            return 0.0

        try:
            emission = float(metagraph.E[our_uid])
            return emission
        except Exception as e:
            logger.error(f"Error getting emission: {e}")
            return 0.0

    # =========================================================================
    # Registry Management
    # =========================================================================

    async def registry_loop(self, running_flag, register_fn, heartbeat_fn, unregister_fn) -> None:
        """Background loop for registry registration and heartbeat."""
        await register_fn()

        while running_flag():
            try:
                await asyncio.sleep(self.settings.registry_heartbeat_interval)
                await heartbeat_fn()

            except asyncio.CancelledError:
                await unregister_fn()
                break
            except Exception as e:
                logger.error(f"Error in registry loop: {e}")

    async def get_external_ip(self) -> str:
        """Get external IP address."""
        if self.settings.external_ip:
            return self.settings.external_ip

        try:
            async with aiohttp.ClientSession() as session:
                async with session.get("https://api.ipify.org", timeout=aiohttp.ClientTimeout(total=5)) as resp:
                    if resp.status == 200:
                        return await resp.text()
        except Exception as e:
            logger.warning(f"Failed to auto-detect external IP: {e}")

        return "127.0.0.1"

    async def register_with_registry(self, hotkey: str, wallet, our_uid, workers_ref) -> None:
        """
        DEPRECATED: Register with the discovery registry via HTTP.

        Registration is now handled via WebSocket in main.py (_connect_and_register_ws).
        This method is kept for backward compatibility but should not be called.
        """
        logger.warning("register_with_registry is deprecated - use WebSocket registration in main.py")
        try:
            external_ip = await self.get_external_ip()
            url = f"http://{external_ip}:{self.settings.api_port}"

            message = f"{hotkey}:{url}:{self.settings.region}"
            signature = ""
            if wallet:
                try:
                    signature = wallet.hotkey.sign(message.encode()).hex()
                except Exception as e:
                    logger.warning(f"Failed to sign registration: {e}")

            payload = {
                "hotkey": hotkey,
                "url": url,
                "ip": external_ip,
                "port": self.settings.api_port,
                "region": self.settings.region,
                "max_workers": self.settings.max_workers,
                "uid": our_uid,
                "signature": signature,
            }

            async with aiohttp.ClientSession() as session:
                async with session.post(
                    f"{self.settings.registry_url}/orchestrators/register",
                    json=payload,
                    timeout=aiohttp.ClientTimeout(total=10),
                ) as resp:
                    if resp.status == 200:
                        logger.info(f"Registered with registry at {self.settings.registry_url}")
                    else:
                        text = await resp.text()
                        logger.warning(f"Failed to register with registry: {resp.status} - {text[:100]}")

        except Exception as e:
            logger.warning(f"Failed to register with registry: {e}")

    async def send_registry_heartbeat(
        self,
        hotkey: str,
        wallet,
        workers_values,
        total_bytes_relayed: int,
        WorkerStatus,
        subtensor=None,
        pending_payments: int = 0,
    ) -> None:
        """Send heartbeat to registry."""
        try:
            active_workers = len([w for w in workers_values if w.status == WorkerStatus.ACTIVE])
            avg_bandwidth = sum(w.bandwidth_ema for w in workers_values) / max(1, len(list(workers_values)))

            timestamp = int(time.time())
            message = f"{hotkey}:{timestamp}"
            signature = ""
            if wallet:
                try:
                    signature = wallet.hotkey.sign(message.encode()).hex()
                except Exception as e:
                    logger.debug(f"Failed to sign heartbeat: {e}")

            # Query balance for heartbeat
            balance_tao = -1.0
            if subtensor and wallet:
                try:
                    bal = subtensor.get_balance(wallet.hotkey.ss58_address)
                    balance_tao = float(bal)
                except Exception as e:
                    logger.debug(f"Failed to query balance for heartbeat: {e}")

            payload = {
                "hotkey": hotkey,
                "current_workers": active_workers,
                "avg_bandwidth_mbps": avg_bandwidth,
                "total_bytes_relayed": total_bytes_relayed,
                "signature": signature,
                "balance_tao": balance_tao,
                "pending_payments": pending_payments,
            }

            async with aiohttp.ClientSession() as session:
                async with session.post(
                    f"{self.settings.registry_url}/orchestrators/heartbeat",
                    json=payload,
                    timeout=aiohttp.ClientTimeout(total=5),
                ) as resp:
                    if resp.status != 200:
                        text = await resp.text()
                        logger.debug(f"Registry heartbeat failed: {resp.status} - {text[:50]}")

        except Exception as e:
            logger.debug(f"Failed to send registry heartbeat: {e}")

    async def unregister_from_registry(self, hotkey: str) -> None:
        """Unregister from registry on shutdown."""
        try:
            async with aiohttp.ClientSession() as session:
                async with session.delete(
                    f"{self.settings.registry_url}/orchestrators/{hotkey}",
                    params={"signature": ""},
                    timeout=aiohttp.ClientTimeout(total=5),
                ) as resp:
                    if resp.status == 200:
                        logger.info("Unregistered from registry")

        except Exception as e:
            logger.debug(f"Failed to unregister from registry: {e}")
