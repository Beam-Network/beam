"""
Epoch Manager - Epoch lifecycle, advancement, and payment proof generation.
"""

import asyncio
import logging
from datetime import datetime, timedelta
from typing import Any, Dict, List, Optional

import aiohttp

from .config import OrchestratorSettings

logger = logging.getLogger(__name__)

# Optional imports
try:
    from db.database import Database
    DB_AVAILABLE = True
except ImportError:
    DB_AVAILABLE = False

# Payment merkle tree functionality moved to BeamCore
create_payment_merkle_tree = None


class EpochManager:
    """Manages epoch lifecycle, advancement, and payment proof generation."""

    def __init__(self, settings: OrchestratorSettings):
        self.settings = settings

    async def epoch_management_loop(
        self,
        running_flag,
        current_epoch_ref,
        epoch_start_time_ref,
        advance_epoch_fn,
        process_payment_retry_fn,
    ) -> None:
        """Background loop for managing epochs."""
        epoch_duration = timedelta(minutes=5)

        while running_flag():
            try:
                await asyncio.sleep(60)

                # Process any queued payment retries
                await process_payment_retry_fn()

                # Check if epoch should change
                if datetime.utcnow() - epoch_start_time_ref() >= epoch_duration:
                    await advance_epoch_fn()

            except asyncio.CancelledError:
                break
            except Exception as e:
                logger.error(f"Error in epoch management: {e}")

    async def advance_epoch(
        self,
        current_epoch: int,
        epoch_start_time: datetime,
        build_epoch_summary_fn,
        epoch_summaries: dict,
        distribute_rewards_fn,
        generate_payment_proofs_fn,
        blind_mode_enabled: bool,
        blind_workers: dict,
        submit_blind_trust_reports_fn,
        submit_blind_payments_fn,
        reset_blind_epoch_stats_fn,
        workers_values,
        get_our_emission,
    ) -> tuple:
        """
        Advance to next epoch.

        Returns (new_epoch, new_epoch_start_time, new_epoch_start_emission).
        """
        summary = build_epoch_summary_fn(current_epoch, epoch_start_time)
        epoch_summaries[current_epoch] = summary

        distribute_rewards_fn()

        await generate_payment_proofs_fn(current_epoch)

        if blind_mode_enabled and blind_workers:
            await submit_blind_trust_reports_fn(current_epoch)
            await submit_blind_payments_fn(current_epoch)
            await reset_blind_epoch_stats_fn()

        for worker in workers_values:
            worker.bytes_relayed_epoch = 0
            worker.rewards_earned_epoch = 0

        new_epoch_start_emission = get_our_emission()
        new_epoch = current_epoch + 1
        new_epoch_start_time = datetime.utcnow()

        logger.info(
            f"Advanced to epoch {new_epoch} "
            f"(previous: {summary.total_tasks} tasks, {summary.total_bytes_relayed} bytes)"
        )

        return new_epoch, new_epoch_start_time, new_epoch_start_emission

    async def generate_payment_proofs(
        self,
        epoch: int,
        workers_values,
        hotkey: str,
        our_uid,
        db,
        wallet,
        subnet_core=None,
    ) -> Optional[str]:
        """
        Generate merkle tree payment proofs for the epoch and submit to BeamCore.

        Returns the merkle root, or None if no payments to record.

        BeamCore is the central PoB authority - validators read from BeamCore.

        Payment data source priority:
        1. Local worker stats (bytes_relayed_epoch, rewards_earned_epoch)
        2. BeamCore epoch payments (if local is empty and subnet_core is available)
        """
        # Collect payment data from local workers first
        payments = []
        for worker in workers_values:
            if worker.rewards_earned_epoch > 0 or worker.bytes_relayed_epoch > 0:
                payments.append({
                    "worker_id": worker.worker_id,
                    "worker_hotkey": worker.hotkey,
                    "epoch": epoch,
                    "bytes_relayed": worker.bytes_relayed_epoch,
                    "amount_earned": worker.rewards_earned_epoch,
                })

        # Fallback: fetch from BeamCore if no local payments
        if not payments and subnet_core:
            try:
                beamcore_payments = await subnet_core.get_epoch_payments(epoch)
                bc_payment_list = beamcore_payments.get("payments", [])
                if bc_payment_list:
                    logger.info(f"Fetched {len(bc_payment_list)} payments from BeamCore for epoch {epoch}")
                    for p in bc_payment_list:
                        payments.append({
                            "worker_id": p.get("worker_id", ""),
                            "worker_hotkey": p.get("worker_hotkey", ""),
                            "epoch": epoch,
                            "bytes_relayed": p.get("bytes_relayed", 0),
                            "amount_earned": p.get("amount_earned", 0),
                        })
            except Exception as e:
                logger.warning(f"Failed to fetch payments from BeamCore for epoch {epoch}: {e}")

        if not payments:
            logger.debug(f"No payments to record for epoch {epoch}")
            return None

        # Calculate totals for BeamCore reporting
        total_distributed = sum(p["amount_earned"] for p in payments)
        total_bytes = sum(p["bytes_relayed"] for p in payments)
        merkle_root = "0x" + "0" * 64  # Placeholder if merkle not available

        # Update BeamCore with payment proof FIRST (always, regardless of merkle availability)
        if subnet_core:
            try:
                from clients.subnet_core_client import EpochPaymentData
                update_data = EpochPaymentData(
                    epoch=epoch,
                    total_distributed=total_distributed,
                    worker_count=len(payments),
                    total_bytes_relayed=total_bytes,
                    merkle_root=merkle_root,
                    submitted_to_validators=True,
                )
                await subnet_core.record_epoch_payment(update_data)
                logger.info(f"BeamCore record updated with submitted_to_validators=True for epoch {epoch}")
            except Exception as e:
                logger.warning(f"Failed to update BeamCore submitted status for epoch {epoch}: {e}")

        # Generate merkle proofs if available (optional enhancement)
        if create_payment_merkle_tree is None:
            logger.info(
                f"Epoch {epoch} payment summary submitted to BeamCore: "
                f"{len(payments)} workers, {total_bytes/1e6:.2f} MB, {total_distributed/1e9:.6f} TAO"
            )
            return merkle_root

        try:
            tree, payments_with_proofs = create_payment_merkle_tree(payments)
            merkle_root = tree.root

            # Save to local DB if available (optional - BeamCore is source of truth)
            if db:
                try:
                    await db.save_worker_payments_batch(payments_with_proofs)
                    await db.save_epoch_payment(
                        epoch=epoch,
                        orchestrator_hotkey=hotkey or "",
                        orchestrator_uid=our_uid,
                        total_distributed=total_distributed,
                        worker_count=len(payments),
                        total_bytes_relayed=total_bytes,
                        merkle_root=merkle_root,
                    )
                except Exception as db_err:
                    logger.debug(f"Local DB save failed (non-fatal): {db_err}")

            logger.info(
                f"Generated payment proofs for epoch {epoch}: "
                f"{len(payments)} workers, {total_distributed/1e9:.6f} ध, "
                f"root: {merkle_root[:16]}..."
            )

            # Update BeamCore again with actual merkle root (if different from placeholder)
            if subnet_core and merkle_root != "0x" + "0" * 64:
                try:
                    from clients.subnet_core_client import EpochPaymentData
                    update_data = EpochPaymentData(
                        epoch=epoch,
                        total_distributed=total_distributed,
                        worker_count=len(payments),
                        total_bytes_relayed=total_bytes,
                        merkle_root=merkle_root,
                        submitted_to_validators=True,
                    )
                    await subnet_core.record_epoch_payment(update_data)
                except Exception as e:
                    logger.debug(f"Failed to update BeamCore with merkle root: {e}")

            # Note: Validator submission is handled centrally by BeamCore
            # Validators read PoB data from BeamCore, not from orchestrators directly

            return merkle_root

        except Exception as e:
            logger.error(f"Error generating payment proofs: {e}")
            return merkle_root

