"""
On-Chain Transaction Verifier

Verifies orchestrator payment tx_hashes against blockchain records.
Used by validators to ensure orchestrators actually paid workers on-chain.

Supports two payment types:
1. TAO transfers (Balances.transfer) - Legacy
2. ALPHA transfers (Utility.batch_all with SubtensorModule.transfer_stake + System.remark_with_event)
"""

import logging
from dataclasses import dataclass
from typing import Dict, Optional

logger = logging.getLogger(__name__)


@dataclass
class TxVerificationResult:
    """Result of verifying a transaction on-chain."""
    is_valid: bool
    error: Optional[str] = None
    from_address: Optional[str] = None
    to_address: Optional[str] = None
    amount_tao: Optional[float] = None


@dataclass
class AlphaPaymentVerificationResult:
    """Result of verifying an ALPHA payment on-chain."""
    is_valid: bool
    error: Optional[str] = None
    sender_coldkey: Optional[str] = None
    recipient_coldkey: Optional[str] = None
    amount_alpha: Optional[float] = None  # In ALPHA (not RAO)
    memo: Optional[str] = None  # transfer_id from remark_with_event
    hotkey: Optional[str] = None  # Hotkey used in transfer_stake


class TxVerifier:
    """
    Verifies transfer transactions on the Bittensor chain.

    Checks that:
    1. tx_hash exists and is a valid transfer
    2. Sender matches expected orchestrator coldkey
    3. Recipient matches expected worker address
    4. Amount matches expected payment (within tolerance)
    """

    def __init__(self, subtensor):
        """
        Initialize verifier with subtensor connection.

        Args:
            subtensor: Bittensor subtensor instance
        """
        self.subtensor = subtensor
        self._cache: Dict[str, TxVerificationResult] = {}

    def verify_transfer(
        self,
        tx_hash: str,
        expected_from: str,
        expected_to: str,
        expected_amount: float,
        tolerance: float = 0.01,
    ) -> TxVerificationResult:
        """
        Verify a transfer transaction on-chain.

        Args:
            tx_hash: Transaction hash in format "{extrinsic_hash}:{block_hash}"
            expected_from: Expected sender address (orchestrator coldkey)
            expected_to: Expected recipient address (worker payment address)
            expected_amount: Expected TAO amount
            tolerance: Allowed amount deviation (default 1%)

        Returns:
            TxVerificationResult with verification status
        """
        # Check cache first
        if tx_hash in self._cache:
            return self._cache[tx_hash]

        # Query chain for extrinsic
        result = self._query_extrinsic(tx_hash)

        if not result.is_valid:
            self._cache[tx_hash] = result
            return result

        # Validate sender
        if result.from_address != expected_from:
            result = TxVerificationResult(
                is_valid=False,
                error=f"sender mismatch: expected {expected_from[:16]}..., got {result.from_address[:16] if result.from_address else 'None'}...",
                from_address=result.from_address,
                to_address=result.to_address,
                amount_tao=result.amount_tao,
            )
            self._cache[tx_hash] = result
            return result

        # Validate recipient
        if result.to_address != expected_to:
            result = TxVerificationResult(
                is_valid=False,
                error=f"recipient mismatch: expected {expected_to[:16]}..., got {result.to_address[:16] if result.to_address else 'None'}...",
                from_address=result.from_address,
                to_address=result.to_address,
                amount_tao=result.amount_tao,
            )
            self._cache[tx_hash] = result
            return result

        # Validate amount (within tolerance)
        if result.amount_tao is not None and expected_amount > 0:
            deviation = abs(result.amount_tao - expected_amount) / expected_amount
            if deviation > tolerance:
                result = TxVerificationResult(
                    is_valid=False,
                    error=f"amount mismatch: expected {expected_amount:.6f} TAO, got {result.amount_tao:.6f} TAO (deviation: {deviation:.2%})",
                    from_address=result.from_address,
                    to_address=result.to_address,
                    amount_tao=result.amount_tao,
                )
                self._cache[tx_hash] = result
                return result

        # All checks passed
        self._cache[tx_hash] = result
        return result

    def _query_extrinsic(self, tx_hash: str) -> TxVerificationResult:
        """
        Query the blockchain for extrinsic details.

        Args:
            tx_hash: Either "{extrinsic_hash}:{block_hash}" (new format) or
                     just "{extrinsic_hash}" (legacy format)

        Returns:
            TxVerificationResult with transfer details or error
        """
        try:
            # Get substrate interface
            substrate = self.subtensor.substrate

            # Parse tx_hash - new format includes block_hash
            # Format: {extrinsic_hash}:{block_hash}
            extrinsic_hash = tx_hash
            block_hash = None

            if ":" in tx_hash and tx_hash.count(":") == 1:
                parts = tx_hash.split(":")
                # Both parts should start with 0x for valid hashes
                if parts[0].startswith("0x") and parts[1].startswith("0x"):
                    extrinsic_hash = parts[0]
                    block_hash = parts[1]

            if not block_hash:
                # Legacy format - can't verify without block_hash
                return TxVerificationResult(
                    is_valid=False,
                    error=f"missing block_hash in tx_hash (legacy format): {tx_hash[:20]}...",
                )

            # Get the block and find the extrinsic by hash
            block = substrate.get_block(block_hash=block_hash)
            if not block:
                return TxVerificationResult(
                    is_valid=False,
                    error=f"block not found: {block_hash[:20]}...",
                )

            # Find extrinsic by hash in block
            extrinsic_data = None
            for ext in block.get("extrinsics", []):
                ext_hash_raw = ext.extrinsic_hash if hasattr(ext, "extrinsic_hash") else None
                if ext_hash_raw:
                    # Convert bytes to hex string for comparison
                    ext_hash_str = "0x" + ext_hash_raw.hex() if isinstance(ext_hash_raw, bytes) else str(ext_hash_raw)
                    if ext_hash_str.lower() == extrinsic_hash.lower():
                        # Get the value dict from the extrinsic
                        extrinsic_data = ext.value if hasattr(ext, "value") else None
                        break

            if not extrinsic_data:
                return TxVerificationResult(
                    is_valid=False,
                    error=f"extrinsic not found in block: {extrinsic_hash[:20]}...",
                )

            # Extract transfer details from extrinsic
            # Look for Balances.transfer or Balances.transfer_keep_alive call
            call = extrinsic_data.get("call", {})
            call_module = call.get("call_module", "")
            call_function = call.get("call_function", "")

            if call_module != "Balances" or "transfer" not in call_function.lower():
                return TxVerificationResult(
                    is_valid=False,
                    error=f"not a transfer: {call_module}.{call_function}",
                )

            # Extract sender (from address field in extrinsic)
            sender = None
            if "address" in extrinsic_data:
                sender = extrinsic_data["address"]
            elif "signature" in extrinsic_data:
                sig_data = extrinsic_data["signature"]
                if isinstance(sig_data, dict) and "address" in sig_data:
                    sender = sig_data["address"]

            # Extract recipient and amount from call args
            call_args = call.get("call_args", [])
            recipient = None
            amount_raw = None

            for arg in call_args:
                if arg.get("name") in ["dest", "destination"]:
                    dest_value = arg.get("value", {})
                    if isinstance(dest_value, dict):
                        recipient = dest_value.get("Id") or dest_value.get("id")
                    elif isinstance(dest_value, str):
                        recipient = dest_value
                elif arg.get("name") in ["value", "amount"]:
                    amount_raw = arg.get("value", 0)

            # Convert amount from raw (rao) to TAO
            amount_tao = None
            if amount_raw is not None:
                amount_tao = float(amount_raw) / 1e9  # 1 TAO = 1e9 rao

            if not sender or not recipient:
                return TxVerificationResult(
                    is_valid=False,
                    error=f"could not parse transfer details from extrinsic",
                )

            return TxVerificationResult(
                is_valid=True,
                from_address=sender,
                to_address=recipient,
                amount_tao=amount_tao,
            )

        except Exception as e:
            logger.error(f"Error querying extrinsic {tx_hash[:20]}...: {e}")
            return TxVerificationResult(
                is_valid=False,
                error=f"query error: {str(e)[:100]}",
            )

    def clear_cache(self):
        """Clear the verification cache."""
        self._cache.clear()

    def get_cache_stats(self) -> Dict:
        """Get cache statistics."""
        valid = sum(1 for r in self._cache.values() if r.is_valid)
        invalid = len(self._cache) - valid
        return {
            "total": len(self._cache),
            "valid": valid,
            "invalid": invalid,
        }


class AlphaPaymentVerifier:
    """
    Verifies ALPHA token payment transactions on the Bittensor chain.

    ALPHA payments are made via utility.batch_all containing:
    1. System.remark_with_event - Contains transfer_id as memo
    2. SubtensorModule.transfer_stake - Transfers staked ALPHA to worker coldkey

    Checks that:
    1. tx_hash exists and is a valid batch_all transaction
    2. Batch contains remark_with_event with expected transfer_id
    3. Batch contains transfer_stake to correct worker coldkey
    4. Amount is >= expected (1 ALPHA default)
    """

    # Minimum ALPHA payment (in RAO). 1 ALPHA = 1e9 RAO
    MIN_ALPHA_RAO = int(1e9)

    def __init__(self, subtensor):
        """
        Initialize verifier with subtensor connection.

        Args:
            subtensor: Bittensor subtensor instance
        """
        self.subtensor = subtensor
        self._cache: Dict[str, AlphaPaymentVerificationResult] = {}

    def verify_alpha_payment(
        self,
        tx_hash: str,
        expected_transfer_id: str,
        expected_worker_coldkey: str,
        min_amount_alpha: float = 1.0,
    ) -> AlphaPaymentVerificationResult:
        """
        Verify an ALPHA payment transaction on-chain.

        Args:
            tx_hash: Transaction hash in format "{extrinsic_hash}:{block_hash}"
            expected_transfer_id: Expected transfer_id in remark_with_event memo
            expected_worker_coldkey: Expected recipient coldkey
            min_amount_alpha: Minimum ALPHA amount (default 1.0)

        Returns:
            AlphaPaymentVerificationResult with verification status
        """
        # Check cache first (key includes expected values for accurate caching)
        cache_key = f"{tx_hash}:{expected_transfer_id}:{expected_worker_coldkey}"
        if cache_key in self._cache:
            return self._cache[cache_key]

        # Parse and query the extrinsic
        result = self._query_batch_extrinsic(tx_hash)

        if not result.is_valid:
            self._cache[cache_key] = result
            return result

        # Validate memo matches expected transfer_id
        if result.memo != expected_transfer_id:
            result = AlphaPaymentVerificationResult(
                is_valid=False,
                error=f"memo mismatch: expected '{expected_transfer_id}', got '{result.memo}'",
                sender_coldkey=result.sender_coldkey,
                recipient_coldkey=result.recipient_coldkey,
                amount_alpha=result.amount_alpha,
                memo=result.memo,
                hotkey=result.hotkey,
            )
            self._cache[cache_key] = result
            return result

        # Validate recipient coldkey
        if result.recipient_coldkey != expected_worker_coldkey:
            result = AlphaPaymentVerificationResult(
                is_valid=False,
                error=f"recipient mismatch: expected {expected_worker_coldkey[:16]}..., got {result.recipient_coldkey[:16] if result.recipient_coldkey else 'None'}...",
                sender_coldkey=result.sender_coldkey,
                recipient_coldkey=result.recipient_coldkey,
                amount_alpha=result.amount_alpha,
                memo=result.memo,
                hotkey=result.hotkey,
            )
            self._cache[cache_key] = result
            return result

        # Validate amount (must be >= min_amount_alpha)
        if result.amount_alpha is not None and result.amount_alpha < min_amount_alpha:
            result = AlphaPaymentVerificationResult(
                is_valid=False,
                error=f"amount too low: expected >= {min_amount_alpha} ALPHA, got {result.amount_alpha:.6f} ALPHA",
                sender_coldkey=result.sender_coldkey,
                recipient_coldkey=result.recipient_coldkey,
                amount_alpha=result.amount_alpha,
                memo=result.memo,
                hotkey=result.hotkey,
            )
            self._cache[cache_key] = result
            return result

        # All checks passed
        self._cache[cache_key] = result
        return result

    def _query_batch_extrinsic(self, tx_hash: str) -> AlphaPaymentVerificationResult:
        """
        Query the blockchain for a batch_all extrinsic containing ALPHA payment.

        Args:
            tx_hash: Format "{extrinsic_hash}:{block_hash}"

        Returns:
            AlphaPaymentVerificationResult with extracted details or error
        """
        try:
            # Get substrate interface
            substrate = self.subtensor.substrate

            # Parse tx_hash - must have block_hash
            if ":" not in tx_hash or tx_hash.count(":") != 1:
                return AlphaPaymentVerificationResult(
                    is_valid=False,
                    error=f"invalid tx_hash format (expected extrinsic_hash:block_hash): {tx_hash[:30]}...",
                )

            parts = tx_hash.split(":")
            if not (parts[0].startswith("0x") and parts[1].startswith("0x")):
                return AlphaPaymentVerificationResult(
                    is_valid=False,
                    error=f"invalid tx_hash format (hashes must start with 0x): {tx_hash[:30]}...",
                )

            extrinsic_hash = parts[0]
            block_hash = parts[1]

            # Get the block
            block = substrate.get_block(block_hash=block_hash)
            if not block:
                return AlphaPaymentVerificationResult(
                    is_valid=False,
                    error=f"block not found: {block_hash[:20]}...",
                )

            # Find extrinsic by hash in block
            extrinsic_data = None
            for ext in block.get("extrinsics", []):
                ext_hash_raw = ext.extrinsic_hash if hasattr(ext, "extrinsic_hash") else None
                if ext_hash_raw:
                    ext_hash_str = "0x" + ext_hash_raw.hex() if isinstance(ext_hash_raw, bytes) else str(ext_hash_raw)
                    if ext_hash_str.lower() == extrinsic_hash.lower():
                        extrinsic_data = ext.value if hasattr(ext, "value") else None
                        break

            if not extrinsic_data:
                return AlphaPaymentVerificationResult(
                    is_valid=False,
                    error=f"extrinsic not found in block: {extrinsic_hash[:20]}...",
                )

            # Verify it's a batch_all call
            call = extrinsic_data.get("call", {})
            call_module = call.get("call_module", "")
            call_function = call.get("call_function", "")

            if call_module != "Utility" or call_function != "batch_all":
                return AlphaPaymentVerificationResult(
                    is_valid=False,
                    error=f"not a batch_all call: {call_module}.{call_function}",
                )

            # Extract sender coldkey from extrinsic
            sender_coldkey = None
            if "address" in extrinsic_data:
                sender_coldkey = extrinsic_data["address"]
            elif "signature" in extrinsic_data:
                sig_data = extrinsic_data["signature"]
                if isinstance(sig_data, dict) and "address" in sig_data:
                    sender_coldkey = sig_data["address"]

            # Parse nested calls in the batch
            call_args = call.get("call_args", [])
            nested_calls = None
            for arg in call_args:
                if arg.get("name") == "calls":
                    nested_calls = arg.get("value", [])
                    break

            if not nested_calls:
                return AlphaPaymentVerificationResult(
                    is_valid=False,
                    error="batch_all has no nested calls",
                )

            # Extract memo from remark_with_event and transfer details from transfer_stake
            memo = None
            recipient_coldkey = None
            amount_rao = None
            hotkey = None

            for nested_call in nested_calls:
                nested_module = nested_call.get("call_module", "")
                nested_function = nested_call.get("call_function", "")
                nested_args = nested_call.get("call_args", [])

                if nested_module == "System" and nested_function == "remark_with_event":
                    # Extract memo from remark
                    for arg in nested_args:
                        if arg.get("name") == "remark":
                            remark_value = arg.get("value")
                            if isinstance(remark_value, bytes):
                                memo = remark_value.decode("utf-8", errors="ignore")
                            elif isinstance(remark_value, str):
                                memo = remark_value
                            elif isinstance(remark_value, list):
                                # Sometimes returned as list of ints (bytes)
                                memo = bytes(remark_value).decode("utf-8", errors="ignore")

                elif nested_module == "SubtensorModule" and nested_function == "transfer_stake":
                    # Extract transfer_stake details
                    for arg in nested_args:
                        arg_name = arg.get("name", "")
                        arg_value = arg.get("value")

                        if arg_name == "destination_coldkey":
                            recipient_coldkey = arg_value
                        elif arg_name == "alpha_amount":
                            amount_rao = arg_value
                        elif arg_name == "hotkey":
                            hotkey = arg_value

            # Validate we found both components
            if memo is None:
                return AlphaPaymentVerificationResult(
                    is_valid=False,
                    error="batch_all missing System.remark_with_event call",
                    sender_coldkey=sender_coldkey,
                )

            if recipient_coldkey is None or amount_rao is None:
                return AlphaPaymentVerificationResult(
                    is_valid=False,
                    error="batch_all missing SubtensorModule.transfer_stake call",
                    sender_coldkey=sender_coldkey,
                    memo=memo,
                )

            # Convert amount from RAO to ALPHA
            amount_alpha = float(amount_rao) / 1e9

            return AlphaPaymentVerificationResult(
                is_valid=True,
                sender_coldkey=sender_coldkey,
                recipient_coldkey=recipient_coldkey,
                amount_alpha=amount_alpha,
                memo=memo,
                hotkey=hotkey,
            )

        except Exception as e:
            logger.error(f"Error querying batch extrinsic {tx_hash[:30]}...: {e}")
            return AlphaPaymentVerificationResult(
                is_valid=False,
                error=f"query error: {str(e)[:100]}",
            )

    def clear_cache(self):
        """Clear the verification cache."""
        self._cache.clear()

    def get_cache_stats(self) -> Dict:
        """Get cache statistics."""
        valid = sum(1 for r in self._cache.values() if r.is_valid)
        invalid = len(self._cache) - valid
        return {
            "total": len(self._cache),
            "valid": valid,
            "invalid": invalid,
        }

    def verify_pop_batch(
        self,
        proofs: list,
        min_amount_alpha: float = 1.0,
    ) -> Dict[str, Dict]:
        """
        Independently verify Proof of Payment (PoP) for a batch of paid proofs.

        This allows validators to verify ALPHA payments on-chain without trusting
        BeamCore's pop_verified flag.

        Usage:
            # Fetch paid proofs from BeamCore
            data = await subnet_core_client.get_paid_proofs_for_pop_verification(epoch=123)

            # Verify independently
            results = alpha_verifier.verify_pop_batch(data["proofs"])

            # Check for mismatches
            mismatches = [t for t, r in results.items() if not r["match"]]

        Args:
            proofs: List of proof dicts from SubnetCoreClient.get_paid_proofs_for_pop_verification()
                    Each proof should have: tx_hash, transfer_id, worker_coldkey
            min_amount_alpha: Minimum ALPHA amount to verify (default 1.0)

        Returns:
            Dict of task_id -> {
                "verified": bool,           # Our verification result
                "beamcore_verified": bool,  # BeamCore's result (for comparison)
                "match": bool,              # Whether our result matches BeamCore
                "error": str or None,       # Error if verification failed
                "amount_alpha": float,      # Amount in ALPHA
                "memo": str,                # Memo from transaction
            }
        """
        results = {}

        for proof in proofs:
            task_id = proof.get("task_id")
            tx_hash = proof.get("tx_hash")
            transfer_id = proof.get("transfer_id")
            worker_coldkey = proof.get("worker_coldkey")
            beamcore_verified = proof.get("pop_verified")

            # Skip if missing required fields
            if not tx_hash:
                results[task_id] = {
                    "verified": False,
                    "beamcore_verified": beamcore_verified,
                    "match": beamcore_verified is False or beamcore_verified is None,
                    "error": "Missing tx_hash",
                    "amount_alpha": None,
                    "memo": None,
                }
                continue

            if not transfer_id:
                results[task_id] = {
                    "verified": False,
                    "beamcore_verified": beamcore_verified,
                    "match": beamcore_verified is False or beamcore_verified is None,
                    "error": "Missing transfer_id for memo verification",
                    "amount_alpha": None,
                    "memo": None,
                }
                continue

            if not worker_coldkey:
                results[task_id] = {
                    "verified": False,
                    "beamcore_verified": beamcore_verified,
                    "match": beamcore_verified is False or beamcore_verified is None,
                    "error": "Missing worker_coldkey for recipient verification",
                    "amount_alpha": None,
                    "memo": None,
                }
                continue

            # Verify on-chain
            result = self.verify_alpha_payment(
                tx_hash=tx_hash,
                expected_transfer_id=transfer_id,
                expected_worker_coldkey=worker_coldkey,
                min_amount_alpha=min_amount_alpha,
            )

            results[task_id] = {
                "verified": result.is_valid,
                "beamcore_verified": beamcore_verified,
                "match": result.is_valid == beamcore_verified,
                "error": result.error,
                "amount_alpha": result.amount_alpha,
                "memo": result.memo,
            }

        return results
