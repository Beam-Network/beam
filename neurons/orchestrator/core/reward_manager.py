"""
Reward Manager - Worker payments, retry queue, and reward distribution.
"""

import logging
import time
from typing import Any, Dict, List, Optional, Set

from .config import OrchestratorSettings

logger = logging.getLogger(__name__)

# Optional imports
try:
    from clients import PoBSubmission
    SUBNET_CORE_CLIENT_AVAILABLE = True
except ImportError:
    SUBNET_CORE_CLIENT_AVAILABLE = False

try:
    from db.database import Database
    DB_AVAILABLE = True
except ImportError:
    DB_AVAILABLE = False

import hashlib
import json as _json

# Import Balance for TAO transfers
try:
    from bittensor.utils.balance import Balance
except ImportError:
    Balance = None


def _compute_payment_merkle_root(leaves: list) -> str:
    """Compute merkle root from payment leaf dicts (inline, no external deps)."""
    if not leaves:
        return "0x" + "0" * 64

    # Hash each leaf: deterministic JSON -> SHA-256
    hashes = []
    for leaf in leaves:
        leaf_str = _json.dumps(leaf, sort_keys=True, separators=(',', ':'))
        h = hashlib.sha256(leaf_str.encode()).digest()
        hashes.append(h)

    # Build tree bottom-up
    while len(hashes) > 1:
        next_level = []
        for i in range(0, len(hashes), 2):
            left = hashes[i]
            right = hashes[i + 1] if i + 1 < len(hashes) else hashes[i]
            next_level.append(hashlib.sha256(left + right).digest())
        hashes = next_level

    return "0x" + hashes[0].hex()


class RewardManager:
    """Manages worker payments, retry queue, and reward distribution."""

    def __init__(self, settings: OrchestratorSettings):
        self.settings = settings

        # Payment retry queue
        self._payment_retry_queue: List[dict] = []
        self._max_payment_retries: int = 5

        # Dedup: task IDs that have already been paid (prevents double-payment)
        self._paid_task_ids: Set[str] = set()

        # Reward tracking
        self.total_rewards_distributed: float = 0.0
        self.last_emission_check: float = 0.0
        self.epoch_start_emission: float = 0.0

        # Per-epoch accumulators for epoch summary reporting
        self._epoch_totals: Dict[int, Dict[str, Any]] = {}  # epoch -> {distributed_nano, worker_ids, bytes_relayed}

    async def load_paid_tasks(self, subnet_core_client) -> int:
        """
        Load already-paid task IDs from SubnetCore on startup.
        Returns the number of task IDs loaded.
        """
        if not SUBNET_CORE_CLIENT_AVAILABLE or not subnet_core_client:
            return 0
        try:
            task_ids = await subnet_core_client.get_paid_task_ids()
            self._paid_task_ids.update(task_ids)
            logger.info(f"Loaded {len(task_ids)} paid task IDs into dedup set")
            return len(task_ids)
        except Exception as e:
            logger.warning(f"Failed to load paid task IDs: {e}")
            return 0

    async def pay_worker_immediately(
        self,
        worker,
        proof,
        current_epoch: int,
        get_our_emission,
        total_bytes_relayed: int,
        wallet,
        subtensor,
        hotkey: str,
        db,
        subnet_core_client,
        fee_percentage: float = 12.0,
        use_alpha: bool = False,
        netuid: int = 105,
        alpha_per_chunk: float = 1.0,
    ) -> Optional[float]:
        """
        Calculate, transfer, and record immediate payment for a completed task.

        Returns the reward amount in TAO, or None if payment failed.
        """
        # Skip tasks with no bytes relayed — no work done, nothing to pay
        if not proof.bytes_relayed or proof.bytes_relayed <= 0:
            logger.debug(
                f"Skipping payment for task {proof.task_id[:16]}...: 0 bytes relayed"
            )
            return None

        # Dedup: skip if this task was already paid
        if proof.task_id in self._paid_task_ids:
            logger.warning(
                f"DEDUP: task {proof.task_id[:16]}... already paid — skipping"
            )
            return None

        # ALPHA payment: Validate transfer before payment
        transfer_id = None
        if use_alpha and SUBNET_CORE_CLIENT_AVAILABLE and subnet_core_client:
            try:
                validation = await subnet_core_client.validate_transfer_for_payment(proof.task_id)
                if not validation.get("valid"):
                    error = validation.get("error", "Unknown validation error")
                    logger.warning(
                        f"ALPHA payment REJECTED for task {proof.task_id[:16]}...: {error}"
                    )
                    return None
                transfer_id = validation.get("transfer_id")
                logger.debug(f"Transfer validated for ALPHA payment: {transfer_id}")
            except Exception as e:
                logger.warning(f"Transfer validation failed for task {proof.task_id[:16]}...: {e}")
                # Fall through to TAO payment if validation fails
                use_alpha = False

        # Extract worker info from proof (always available, even when worker object is None)
        worker_id = proof.worker_id
        worker_hotkey = proof.worker_hotkey

        current_emission = get_our_emission()
        emission_delta = current_emission - self.last_emission_check

        if emission_delta <= 0:
            if total_bytes_relayed > 0:
                emission_rate = current_emission / total_bytes_relayed
            else:
                emission_rate = 1e-6 / (1024 * 1024)
        else:
            emission_rate = emission_delta / max(proof.bytes_relayed, 1)

        base_reward = proof.bytes_relayed * emission_rate
        quality_multiplier = self._calculate_quality_multiplier(worker)
        reward = base_reward * quality_multiplier

        # Apply fee_percentage: orchestrator shares this % of emission with workers
        if fee_percentage > 0:
            fee_multiplier = 1.0 + (fee_percentage / 100.0)
            reward *= fee_multiplier

        # Bittensor minimum transfer is 500 rao (existential deposit).
        # Top up dust payments to reach the minimum so workers always get paid.
        MIN_CHAIN_TRANSFER = 5e-7  # 500 rao = 0.0000005 TAO
        if reward < MIN_CHAIN_TRANSFER:
            logger.debug(
                f"Reward {reward:.12f} TAO topped up to chain minimum "
                f"{MIN_CHAIN_TRANSFER} TAO for task {proof.task_id[:16]}..."
            )
            reward = MIN_CHAIN_TRANSFER

        max_reward = current_emission * 0.1
        if max_reward > 0:
            reward = min(reward, max_reward)

        if wallet and subtensor:
            try:
                # Cache balance for 30s to avoid concurrent subtensor websocket calls
                import time as _time
                now = _time.time()
                if not hasattr(self, '_cached_balance') or (now - getattr(self, '_cached_balance_at', 0)) > 30:
                    self._cached_balance = float(subtensor.get_balance(wallet.hotkey.ss58_address))
                    self._cached_balance_at = now
                available = self._cached_balance
                fee_buffer = 0.001
                spendable = max(0.0, available - fee_buffer)

                if reward > spendable:
                    if spendable <= min_reward:
                        worker_hk = proof.worker_hotkey if proof else (worker.hotkey if worker else "unknown")
                        logger.warning(
                            f"Insufficient balance ({available:.4f} TAO) to pay worker "
                            f"{worker_hk[:16]}... — queuing {reward:.9f} TAO for retry"
                        )
                        self._queue_failed_payment(worker, proof, reward)
                        return None
                    else:
                        shortfall = reward - spendable
                        logger.warning(
                            f"Balance cap: {reward:.9f} TAO → {spendable:.9f} TAO "
                            f"(shortfall {shortfall:.9f}). Queuing remainder."
                        )
                        self._queue_failed_payment(worker, proof, shortfall)
                        reward = spendable
            except Exception as e:
                logger.error(f"Could not check balance — queuing payment for retry: {e}")
                self._queue_failed_payment(worker, proof, reward)
                self.last_emission_check = current_emission
                return None

        # Resolve payment address via SubnetCore, with fallback to worker_hotkey
        payment_dest = None
        if SUBNET_CORE_CLIENT_AVAILABLE and subnet_core_client:
            try:
                payment_info = await subnet_core_client.get_task_payment_address(proof.task_id)
                payment_dest = payment_info.get("address")
                if not payment_dest:
                    raise ValueError("SubnetCore returned empty payment address")
                logger.debug(
                    f"Resolved payment address for task {proof.task_id[:16]}...: {payment_dest[:16]}..."
                )
            except Exception as e:
                # Fallback: use worker_hotkey directly as payment address
                if proof.worker_hotkey:
                    payment_dest = proof.worker_hotkey
                    logger.warning(
                        f"Payment address lookup failed for task {proof.task_id[:16]}...: {e} "
                        f"— using worker_hotkey {payment_dest[:16]}... as fallback"
                    )
                else:
                    logger.error(
                        f"Failed to resolve payment address from SubnetCore for task "
                        f"{proof.task_id[:16]}...: {e} — queuing for retry"
                    )
                    self._queue_failed_payment(worker, proof, reward)
                    self.last_emission_check = current_emission
                    return None
        else:
            # No SubnetCore client - use worker_hotkey directly
            if proof.worker_hotkey:
                payment_dest = proof.worker_hotkey
                logger.info(
                    f"SubnetCore client not available — using worker_hotkey "
                    f"{payment_dest[:16]}... as payment address"
                )
            else:
                logger.error(
                    f"SubnetCore client not available and no worker_hotkey — "
                    f"cannot resolve payment address. Queuing payment for retry."
                )
                self._queue_failed_payment(worker, proof, reward)
                self.last_emission_check = current_emission
                return None

        tx_hash = ""
        transfer_success = False
        alpha_amount_rao = 0

        # =====================================================================
        # ALPHA Payment Path: transfer_stake with on-chain memo
        # =====================================================================
        if use_alpha and transfer_id and wallet and subtensor:
            try:
                # Resolve worker's coldkey (required for transfer_stake)
                worker_coldkey = await self._resolve_worker_coldkey(worker_hotkey, subtensor, netuid)
                if not worker_coldkey:
                    logger.error(
                        f"Cannot pay ALPHA: failed to resolve coldkey for worker {worker_hotkey[:16]}..."
                    )
                    self._queue_failed_payment(worker, proof, reward)
                    self.last_emission_check = current_emission
                    return None

                # Transfer ALPHA with on-chain memo
                # Memo format: {transfer_id}:{task_id} to ensure uniqueness per worker/task
                # This prevents reusing the same memo for multiple payments
                payment_memo = f"{transfer_id}:{proof.task_id}"
                alpha_amount_rao = int(alpha_per_chunk * 1e9)
                tx_hash = await self.transfer_alpha_with_memo(
                    worker_coldkey=worker_coldkey,
                    amount_alpha=alpha_per_chunk,
                    transfer_id=payment_memo,
                    wallet=wallet,
                    subtensor=subtensor,
                    netuid=netuid,
                )

                if tx_hash:
                    transfer_success = True
                    # Record payment on PoB record via BeamCore
                    if SUBNET_CORE_CLIENT_AVAILABLE and subnet_core_client:
                        try:
                            await subnet_core_client.record_pob_payment(
                                task_id=proof.task_id,
                                tx_hash=tx_hash,
                                amount_rao=alpha_amount_rao,
                            )
                        except Exception as e:
                            logger.warning(f"Failed to record PoB payment: {e}")
                    logger.info(
                        f"ALPHA payment SUCCESS: {alpha_per_chunk} ALPHA to {worker_coldkey[:16]}... "
                        f"transfer={transfer_id} tx={tx_hash[:24]}..."
                    )
                else:
                    logger.error(
                        f"ALPHA payment FAILED for task {proof.task_id[:16]}... — queuing for retry"
                    )
                    self._queue_failed_payment(worker, proof, reward)

            except Exception as e:
                logger.error(f"Error in ALPHA payment: {e}", exc_info=True)
                self._queue_failed_payment(worker, proof, reward)

        # =====================================================================
        # TAO Payment Path: standard transfer (fallback when use_alpha=False)
        # =====================================================================
        elif wallet and subtensor and reward > 0:
            try:
                # Convert reward to Balance object (required by new bittensor API)
                amount = Balance.from_tao(reward) if Balance else reward
                response = subtensor.transfer(
                    wallet=wallet,
                    destination_ss58=payment_dest,
                    amount=amount,
                    wait_for_inclusion=True,
                    wait_for_finalization=False,
                )
                # Handle both old bool return and new ExtrinsicResponse
                success = response.is_success if hasattr(response, 'is_success') else bool(response)
                if success:
                    transfer_success = True
                    # Extract real blockchain tx hash from ExtrinsicResponse
                    # SDK v10+: tx hash is in response.extrinsic_receipt.extrinsic_hash
                    # Also extract block_hash for on-chain verification
                    tx_hash = None
                    block_hash = None
                    if hasattr(response, 'extrinsic_receipt') and response.extrinsic_receipt:
                        receipt = response.extrinsic_receipt
                        if hasattr(receipt, 'extrinsic_hash') and receipt.extrinsic_hash:
                            tx_hash = str(receipt.extrinsic_hash)
                        if hasattr(receipt, 'block_hash') and receipt.block_hash:
                            block_hash = str(receipt.block_hash)
                    if not tx_hash:
                        # Fallback to synthetic hash if real hash not available
                        tx_hash = f"transfer:{hotkey[:8]}:{payment_dest[:8]}:{int(time.time())}"
                        logger.warning(f"Could not extract real tx hash from response: {type(response)}")
                    elif block_hash:
                        # Combine extrinsic_hash:block_hash for validator verification
                        tx_hash = f"{tx_hash}:{block_hash}"
                    logger.info(
                        f"On-chain transfer SUCCESS: {reward:.9f} TAO to {payment_dest[:16]}... tx={tx_hash[:24]}..."
                    )
                else:
                    logger.error(
                        f"On-chain transfer FAILED: {reward:.9f} TAO to {payment_dest[:16]}..."
                    )
                    self._queue_failed_payment(worker, proof, reward)
            except Exception as e:
                logger.error(f"Error transferring TAO to worker: {e}")
                self._queue_failed_payment(worker, proof, reward)
        else:
            if not wallet:
                logger.warning("No wallet available for on-chain transfer")
            if not subtensor:
                logger.warning("No subtensor connection for on-chain transfer")

        self.last_emission_check = current_emission

        reward_nano = int(reward * 1e9)

        if transfer_success:
            self._paid_task_ids.add(proof.task_id)
            # Update worker stats only if worker object exists (not in challenge flow)
            if worker is not None:
                worker.rewards_earned_epoch += reward_nano
                worker.rewards_earned_total += reward_nano
            self.total_rewards_distributed += reward

            if DB_AVAILABLE and db:
                try:
                    await db.save_worker_payment(
                        epoch=current_epoch,
                        worker_id=worker_id,
                        worker_hotkey=worker_hotkey,
                        bytes_relayed=proof.bytes_relayed,
                        tasks_completed=1,
                        amount_earned=reward_nano,
                        merkle_proof=tx_hash,
                        leaf_index=0,
                    )
                except Exception as e:
                    logger.warning(f"Failed to save worker payment: {e}")

            if SUBNET_CORE_CLIENT_AVAILABLE and subnet_core_client:
                try:
                    from clients.subnet_core_client import WorkerPaymentData
                    payment_data = WorkerPaymentData(
                        orchestrator_hotkey=hotkey or "",
                        epoch=current_epoch,
                        worker_id=worker_id,
                        worker_hotkey=worker_hotkey,
                        bytes_relayed=proof.bytes_relayed,
                        tasks_completed=1,
                        amount_earned=reward_nano,
                        task_id=proof.task_id,
                        tx_hash=tx_hash,
                    )
                    await subnet_core_client.record_worker_payment(payment_data)
                    logger.debug(f"Payment recorded to SubnetCore: task={proof.task_id[:16]}...")
                except Exception as e:
                    logger.warning(f"Failed to record payment via SubnetCore: {e}")

            # Update epoch summary
            self._track_epoch_payment(current_epoch, worker_id, worker_hotkey, reward_nano, proof.bytes_relayed)
            await self._report_epoch_summary(current_epoch, hotkey, subnet_core_client)

            # Immediately acknowledge task completion after successful payment
            # This ensures tasks are marked as acknowledged without waiting for the next poll cycle
            if SUBNET_CORE_CLIENT_AVAILABLE and subnet_core_client and proof.task_id:
                try:
                    await subnet_core_client.acknowledge_task_completions([proof.task_id], verified=True)
                    logger.debug(f"Immediately acknowledged task {proof.task_id[:16]}... after payment")
                except Exception as e:
                    # Don't fail the payment if ack fails - it will be retried on next poll
                    logger.warning(f"Failed to immediately acknowledge task {proof.task_id[:16]}...: {e}")

        status = "PAID" if transfer_success else "QUEUED FOR RETRY"
        if use_alpha and transfer_success:
            logger.info(
                f"Immediate payment [{status}]: Worker {worker_id[:8]} ({worker_hotkey[:12]}...) "
                f"earned {alpha_per_chunk} ALPHA for {proof.bytes_relayed:,} bytes "
                f"(transfer={transfer_id})"
            )
        else:
            logger.info(
                f"Immediate payment [{status}]: Worker {worker_id[:8]} ({worker_hotkey[:12]}...) "
                f"earned {reward:.9f} TAO for {proof.bytes_relayed:,} bytes "
                f"(quality={quality_multiplier:.2f})"
            )

        return reward if transfer_success else None

    def _track_epoch_payment(self, epoch: int, worker_id: str, worker_hotkey: str, amount_nano: int, bytes_relayed: int):
        """Accumulate payment totals and leaf data for an epoch."""
        if epoch not in self._epoch_totals:
            self._epoch_totals[epoch] = {"distributed_nano": 0, "worker_ids": set(), "bytes_relayed": 0, "leaves": []}
        totals = self._epoch_totals[epoch]
        totals["distributed_nano"] += amount_nano
        totals["worker_ids"].add(worker_id)
        totals["bytes_relayed"] += bytes_relayed
        totals["leaves"].append({
            "worker_id": worker_id,
            "worker_hotkey": worker_hotkey,
            "epoch": epoch,
            "bytes_relayed": bytes_relayed,
            "amount_earned": amount_nano,
        })

    async def _report_epoch_summary(self, epoch: int, hotkey: str, subnet_core_client):
        """Report accumulated epoch totals to SubnetCore so the dashboard can display them."""
        if not SUBNET_CORE_CLIENT_AVAILABLE or not subnet_core_client:
            return
        totals = self._epoch_totals.get(epoch)
        if not totals:
            return
        try:
            from clients.subnet_core_client import EpochPaymentData

            # Compute real merkle root from accumulated payment leaves
            leaves = totals.get("leaves", [])
            merkle_root = _compute_payment_merkle_root(leaves)
            if leaves:
                logger.info(f"[MERKLE] Computed root for epoch {epoch}: {merkle_root} ({len(leaves)} leaves)")

            epoch_data = EpochPaymentData(
                epoch=epoch,
                total_distributed=totals["distributed_nano"],
                worker_count=len(totals["worker_ids"]),
                total_bytes_relayed=totals["bytes_relayed"],
                merkle_root=merkle_root,
                submitted_to_validators=True,
            )
            await subnet_core_client.record_epoch_payment(epoch_data)
            logger.info(f"[MERKLE] Epoch summary sent to SubnetCore: epoch={epoch}, merkle_root={merkle_root}, workers={len(totals['worker_ids'])}")
        except Exception as e:
            logger.error(f"[MERKLE] Failed to report epoch summary: {e}", exc_info=True)

    def _queue_failed_payment(self, worker, proof, reward_tao: float):
        """Queue a failed/partial payment for retry when balance is available."""
        # Use proof attributes (always available) since worker may be None
        self._payment_retry_queue.append({
            "worker_hotkey": proof.worker_hotkey,
            "worker_id": proof.worker_id,
            "task_id": proof.task_id,
            "proof": proof,
            "reward_tao": reward_tao,
            "attempts": 0,
            "queued_at": time.time(),
        })
        logger.info(
            f"Queued payment retry: {reward_tao:.9f} TAO for task {proof.task_id[:16]}... "
            f"(queue size: {len(self._payment_retry_queue)})"
        )

    async def process_payment_retry_queue(self, current_epoch: int, wallet, subtensor, hotkey: str, db, subnet_core_client):
        """Process queued payments when balance is available."""
        if not self._payment_retry_queue or not wallet or not subtensor:
            return

        try:
            balance = subtensor.get_balance(wallet.hotkey.ss58_address)
            available = float(balance) - 0.001
        except Exception as e:
            logger.warning(f"Could not check balance for retry queue: {e}")
            return

        if available <= 0:
            logger.debug(
                f"No balance for payment retries ({len(self._payment_retry_queue)} queued)"
            )
            return

        logger.info(
            f"Processing payment retry queue: {len(self._payment_retry_queue)} items, "
            f"{available:.4f} TAO available"
        )

        completed = set()
        for i, item in enumerate(self._payment_retry_queue):
            if available <= 1e-9:
                break

            task_id = item.get("task_id")

            # Skip zero-byte tasks — no work, nothing to pay
            proof = item.get("proof")
            if proof and getattr(proof, "bytes_relayed", 0) <= 0:
                completed.add(i)
                continue

            # Dedup: skip if already paid (e.g. immediate pay succeeded after queuing shortfall)
            if task_id and task_id in self._paid_task_ids:
                logger.info(f"DEDUP: retry task {task_id[:16]}... already paid — removing from queue")
                completed.add(i)
                continue

            # Resolve payment address via SubnetCore
            payment_dest = None
            if SUBNET_CORE_CLIENT_AVAILABLE and subnet_core_client and task_id:
                try:
                    payment_info = await subnet_core_client.get_task_payment_address(task_id)
                    payment_dest = payment_info.get("address")
                except Exception as e:
                    item["attempts"] += 1
                    logger.warning(
                        f"Failed to resolve payment address for task {task_id[:16]}...: {e} "
                        f"(attempt {item['attempts']}/{self._max_payment_retries})"
                    )
                    continue
            else:
                item["attempts"] += 1
                logger.warning(
                    f"Cannot resolve payment address: SubnetCore unavailable "
                    f"(attempt {item['attempts']}/{self._max_payment_retries})"
                )
                continue

            if not payment_dest:
                item["attempts"] += 1
                logger.warning(f"Empty payment address for task {task_id[:16]}...")
                continue

            reward = min(item["reward_tao"], available)

            # Top up dust payments to chain minimum (500 rao)
            MIN_CHAIN_TRANSFER = 5e-7
            if reward < MIN_CHAIN_TRANSFER:
                reward = MIN_CHAIN_TRANSFER

            try:
                # Convert reward to Balance object (required by new bittensor API)
                amount = Balance.from_tao(reward) if Balance else reward
                response = subtensor.transfer(
                    wallet=wallet,
                    destination_ss58=payment_dest,
                    amount=amount,
                    wait_for_inclusion=True,
                    wait_for_finalization=False,
                )
                # Handle both old bool return and new ExtrinsicResponse
                success = response.is_success if hasattr(response, 'is_success') else bool(response)
                if success:
                    available -= reward
                    if task_id:
                        self._paid_task_ids.add(task_id)
                    # Extract real blockchain tx hash from ExtrinsicResponse
                    # SDK v10+: tx hash is in response.extrinsic_receipt.extrinsic_hash
                    # Also extract block_hash for on-chain verification
                    tx_hash = None
                    block_hash = None
                    if hasattr(response, 'extrinsic_receipt') and response.extrinsic_receipt:
                        receipt = response.extrinsic_receipt
                        if hasattr(receipt, 'extrinsic_hash') and receipt.extrinsic_hash:
                            tx_hash = str(receipt.extrinsic_hash)
                        if hasattr(receipt, 'block_hash') and receipt.block_hash:
                            block_hash = str(receipt.block_hash)
                    if not tx_hash:
                        tx_hash = f"retry:{hotkey[:8]}:{payment_dest[:8]}:{int(time.time())}"
                        logger.warning(f"Could not extract real tx hash from retry response: {type(response)}")
                    elif block_hash:
                        # Combine extrinsic_hash:block_hash for validator verification
                        tx_hash = f"{tx_hash}:{block_hash}"
                    logger.info(
                        f"Retry payment SUCCESS: {reward:.9f} TAO to {payment_dest[:16]}... tx={tx_hash[:24]}..."
                    )

                    proof = item["proof"]
                    reward_nano = int(reward * 1e9)

                    if DB_AVAILABLE and db:
                        try:
                            await db.save_worker_payment(
                                epoch=current_epoch,
                                worker_id=item["worker_id"],
                                worker_hotkey=item["worker_hotkey"],
                                bytes_relayed=proof.bytes_relayed,
                                tasks_completed=1,
                                amount_earned=reward_nano,
                                merkle_proof=tx_hash,
                                leaf_index=0,
                            )
                        except Exception as e:
                            logger.warning(f"Failed to save retry payment: {e}")

                    if SUBNET_CORE_CLIENT_AVAILABLE and subnet_core_client:
                        try:
                            from clients.subnet_core_client import WorkerPaymentData
                            payment_data = WorkerPaymentData(
                                orchestrator_hotkey=hotkey or "",
                                epoch=current_epoch,
                                worker_id=item["worker_id"],
                                worker_hotkey=item["worker_hotkey"],
                                bytes_relayed=proof.bytes_relayed,
                                tasks_completed=1,
                                amount_earned=reward_nano,
                                task_id=proof.task_id,
                                tx_hash=tx_hash,
                            )
                            await subnet_core_client.record_worker_payment(payment_data)
                        except Exception as e:
                            logger.warning(f"Failed to record retry payment via SubnetCore: {e}")

                    self._track_epoch_payment(current_epoch, item["worker_id"], item["worker_hotkey"], reward_nano, proof.bytes_relayed)
                    self.total_rewards_distributed += reward
                    completed.add(i)
                else:
                    item["attempts"] += 1
                    logger.warning(
                        f"Retry transfer failed for task {task_id[:16]}... "
                        f"(attempt {item['attempts']}/{self._max_payment_retries})"
                    )
            except Exception as e:
                item["attempts"] += 1
                logger.warning(f"Retry payment error: {e}")

        expired = [
            i for i, item in enumerate(self._payment_retry_queue)
            if i not in completed and item["attempts"] >= self._max_payment_retries
        ]
        for i in expired:
            item = self._payment_retry_queue[i]
            logger.error(
                f"Payment retry EXHAUSTED ({self._max_payment_retries} attempts) for "
                f"task {item.get('task_id', 'unknown')[:16]}... — {item['reward_tao']:.9f} TAO lost"
            )

        self._payment_retry_queue = [
            item for i, item in enumerate(self._payment_retry_queue)
            if i not in completed and item["attempts"] < self._max_payment_retries
        ]

        if completed:
            await self._report_epoch_summary(current_epoch, hotkey, subnet_core_client)

        if completed or expired:
            logger.info(
                f"Retry queue: {len(completed)} paid, {len(expired)} expired, "
                f"{len(self._payment_retry_queue)} remaining"
            )

    # =========================================================================
    # ALPHA Token Transfer with On-Chain Memo
    # =========================================================================

    async def transfer_alpha_with_memo(
        self,
        worker_coldkey: str,
        amount_alpha: float,
        transfer_id: str,
        wallet,
        subtensor,
        netuid: int,
    ) -> Optional[str]:
        """
        Transfer ALPHA tokens to a worker with on-chain memo via utility.batch_all.

        Batches atomically:
        1. system.remarkWithEvent(transfer_id) - on-chain memo for validator verification
        2. SubtensorModule.transfer_stake(...) - actual ALPHA transfer

        Args:
            worker_coldkey: Destination coldkey address (worker's coldkey)
            amount_alpha: Amount of ALPHA to transfer (e.g., 1.0 for 1 ALPHA)
            transfer_id: Transfer ID to embed as on-chain memo (e.g., "xfer-abc123")
            wallet: Bittensor wallet with coldkey for signing
            subtensor: Subtensor connection
            netuid: Network UID (105 for mainnet, 304 for testnet)

        Returns:
            Transaction hash in "extrinsic_hash:block_hash" format, or None if failed
        """
        if not wallet or not subtensor:
            logger.warning("Cannot transfer ALPHA: wallet or subtensor not available")
            return None

        # Convert to RAO (1 ALPHA = 1e9 RAO)
        amount_rao = int(amount_alpha * 1e9)

        # Minimum transfer is 500,000 RAO (0.0005 TAO equivalent)
        MIN_TRANSFER_RAO = 500_000
        if amount_rao < MIN_TRANSFER_RAO:
            logger.debug(f"ALPHA amount {amount_rao} RAO topped up to minimum {MIN_TRANSFER_RAO} RAO")
            amount_rao = MIN_TRANSFER_RAO

        try:
            # Access the underlying substrate interface
            substrate = subtensor.substrate

            # Compose remark call with transfer_id as memo
            remark_call = substrate.compose_call(
                call_module='System',
                call_function='remark_with_event',
                call_params={'remark': transfer_id.encode()}
            )

            # Compose transfer_stake call
            # Note: Parameter names may vary by SDK version
            transfer_call = substrate.compose_call(
                call_module='SubtensorModule',
                call_function='transfer_stake',
                call_params={
                    'destination_coldkey': worker_coldkey,
                    'hotkey': wallet.hotkey.ss58_address,
                    'origin_netuid': netuid,
                    'destination_netuid': netuid,
                    'alpha_amount': amount_rao,
                }
            )

            # Batch both calls atomically - if either fails, both are reverted
            batch_call = substrate.compose_call(
                call_module='Utility',
                call_function='batch_all',
                call_params={'calls': [remark_call, transfer_call]}
            )

            # Sign with coldkey (required for transfer_stake)
            extrinsic = substrate.create_signed_extrinsic(
                call=batch_call,
                keypair=wallet.coldkey,
            )

            # Submit and wait for inclusion
            receipt = substrate.submit_extrinsic(
                extrinsic,
                wait_for_inclusion=True,
                wait_for_finalization=False,
            )

            if receipt.is_success:
                tx_hash = f"{receipt.extrinsic_hash}:{receipt.block_hash}"
                logger.info(
                    f"ALPHA transfer SUCCESS: {amount_alpha} ALPHA to {worker_coldkey[:16]}... "
                    f"memo={transfer_id} tx={tx_hash[:32]}..."
                )
                return tx_hash
            else:
                error_msg = getattr(receipt, 'error_message', 'Unknown error')
                logger.error(f"ALPHA transfer FAILED: {error_msg}")
                return None

        except Exception as e:
            logger.error(f"Error transferring ALPHA: {e}", exc_info=True)
            return None

    async def _resolve_worker_coldkey(
        self,
        worker_hotkey: str,
        subtensor,
        netuid: int,
    ) -> Optional[str]:
        """
        Resolve a worker's hotkey to their coldkey via metagraph.

        Args:
            worker_hotkey: Worker's hotkey address
            subtensor: Subtensor connection
            netuid: Network UID

        Returns:
            Worker's coldkey address, or None if not found
        """
        try:
            metagraph = subtensor.metagraph(netuid)
            for neuron in metagraph.neurons:
                if neuron.hotkey == worker_hotkey:
                    logger.debug(f"Resolved coldkey for {worker_hotkey[:16]}...: {neuron.coldkey[:16]}...")
                    return neuron.coldkey
            logger.warning(f"Worker hotkey {worker_hotkey[:16]}... not found in metagraph")
            return None
        except Exception as e:
            logger.error(f"Failed to resolve coldkey for {worker_hotkey[:16]}...: {e}")
            return None

    def _calculate_quality_multiplier(self, worker) -> float:
        """
        Calculate quality multiplier for worker rewards.

        Range: 0.5 to 1.5
        """
        # If worker is None (e.g., challenge flow), return neutral multiplier
        if worker is None:
            return 1.0

        multiplier = 1.0
        multiplier += getattr(worker, 'success_rate', 0.0) * 0.25

        latency_ms = getattr(worker, 'latency_ms', 0)
        if latency_ms > 0:
            latency_score = max(0, 1 - (latency_ms / 1000))
            multiplier += latency_score * 0.15

        multiplier += getattr(worker, 'trust_score', 0.5) * 0.10

        return max(0.5, min(1.5, multiplier))

    def _calculate_worker_reward_score(self, worker) -> float:
        """Calculate a worker's reward score based on multiple factors."""
        # If worker is None, return minimal score
        if worker is None:
            return 0.0

        w_bytes = self.settings.reward_weight_bytes
        w_success = self.settings.reward_weight_success_rate
        w_latency = self.settings.reward_weight_latency
        w_trust = self.settings.reward_weight_trust

        bytes_score = float(getattr(worker, 'bytes_relayed_epoch', 0))
        success_score = getattr(worker, 'success_rate', 0.0)

        max_latency_ms = 1000.0
        latency_ms = getattr(worker, 'latency_ms', 0)
        latency_score = max(0.0, 1.0 - (latency_ms / max_latency_ms))
        trust_score = getattr(worker, 'trust_score', 0.5)

        quality_multiplier = (
            w_success * success_score +
            w_latency * latency_score +
            w_trust * trust_score
        ) / (w_success + w_latency + w_trust) if (w_success + w_latency + w_trust) > 0 else 1.0

        final_score = bytes_score * (0.5 + 0.5 * quality_multiplier)
        return final_score

    def distribute_rewards_at_epoch_end(self, workers_values, get_our_emission) -> Dict[str, float]:
        """
        Distribute accumulated epoch emissions to all workers proportionally.

        Returns dict of worker_id -> reward amount in TAO.
        """
        current_emission = get_our_emission()
        epoch_rewards = current_emission - self.epoch_start_emission

        if epoch_rewards <= 0:
            logger.debug("No new emissions this epoch, no rewards to distribute")
            return {}

        contributing_workers = [
            w for w in workers_values
            if w.bytes_relayed_epoch > 0
        ]

        if not contributing_workers:
            logger.debug("No workers contributed this epoch, no rewards to distribute")
            return {}

        worker_scores: Dict[str, float] = {}
        for worker in contributing_workers:
            score = self._calculate_worker_reward_score(worker)
            worker_scores[worker.worker_id] = score

        total_score = sum(worker_scores.values())
        if total_score <= 0:
            logger.warning("Total worker score is 0, distributing equally")
            total_score = len(contributing_workers)
            worker_scores = {w.worker_id: 1.0 for w in contributing_workers}

        rewards_distributed: Dict[str, float] = {}
        for worker in contributing_workers:
            share = worker_scores[worker.worker_id] / total_score
            reward = epoch_rewards * share

            reward_nano = int(reward * 1e9)
            worker.rewards_earned_epoch = reward_nano
            worker.rewards_earned_total += reward_nano

            rewards_distributed[worker.worker_id] = reward

            logger.info(
                f"Worker {worker.worker_id[:8]} ({worker.hotkey[:12]}...) earned {reward:.6f} ध "
                f"({share*100:.2f}% share, {worker.bytes_relayed_epoch:,} bytes, "
                f"success={worker.success_rate:.2f}, latency={worker.latency_ms:.0f}ms)"
            )

        self.total_rewards_distributed += epoch_rewards

        logger.info(
            f"Epoch reward distribution complete: {epoch_rewards:.6f} ध "
            f"to {len(rewards_distributed)} workers "
            f"(total all-time: {self.total_rewards_distributed:.6f} ध)"
        )

        return rewards_distributed

    def distribute_rewards_to_workers(self, get_our_emission) -> Dict[str, float]:
        """
        Legacy method - now just tracks emission changes.

        Actual distribution happens at epoch end via distribute_rewards_at_epoch_end().
        """
        current_emission = get_our_emission()
        if current_emission > self.last_emission_check:
            new_emissions = current_emission - self.last_emission_check
            logger.debug(f"New emissions detected: {new_emissions:.6f} ध (accumulated for epoch end)")
            self.last_emission_check = current_emission

        return {}
