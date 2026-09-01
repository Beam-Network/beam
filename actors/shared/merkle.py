"""
Shared Merkle tree implementation for payment proofs.

Used by: orchestrator, validator
This eliminates duplicate merkle.py files across workers.
"""

import hashlib
import json
from typing import Any, Dict, List, Tuple


def hash_leaf(data: Dict[str, Any]) -> str:
    """
    Hash a payment leaf.

    Leaf format: {worker_id, worker_hotkey, epoch, bytes_relayed, amount_earned}
    """
    # Create deterministic string representation
    leaf_str = json.dumps(data, sort_keys=True, separators=(",", ":"))
    return "0x" + hashlib.sha256(leaf_str.encode()).hexdigest()


def hash_pair(left: str, right: str) -> str:
    """Hash two nodes together. Order matters - no sorting."""
    # Remove 0x prefix, concatenate (order preserved), hash
    left_bytes = bytes.fromhex(left[2:])
    right_bytes = bytes.fromhex(right[2:])

    combined = left_bytes + right_bytes
    return "0x" + hashlib.sha256(combined).hexdigest()


class MerkleTree:
    """Merkle tree for payment proofs."""

    def __init__(self, leaves: List[Dict[str, Any]]):
        """
        Build a merkle tree from payment data.

        Args:
            leaves: List of payment dicts with worker_id, amount, etc.
        """
        self.original_leaves = leaves
        self.leaf_hashes = [hash_leaf(leaf) for leaf in leaves]
        self.tree = self._build_tree(self.leaf_hashes)

    def _build_tree(self, leaves: List[str]) -> List[List[str]]:
        """Build the merkle tree from leaf hashes."""
        if not leaves:
            return [[]]

        tree = [leaves]

        while len(tree[-1]) > 1:
            level = tree[-1]
            next_level = []

            for i in range(0, len(level), 2):
                left = level[i]
                # If odd number of nodes, duplicate the last one
                right = level[i + 1] if i + 1 < len(level) else level[i]
                next_level.append(hash_pair(left, right))

            tree.append(next_level)

        return tree

    @property
    def root(self) -> str:
        """Get the merkle root."""
        if not self.tree or not self.tree[-1]:
            return "0x" + "0" * 64
        return self.tree[-1][0]

    def get_proof(self, index: int) -> List[str]:
        """
        Get the merkle proof for a leaf at the given index.

        Returns list of sibling hashes from leaf to root.
        """
        if index < 0 or index >= len(self.leaf_hashes):
            raise ValueError(f"Index {index} out of range")

        proof = []
        current_index = index

        for level in self.tree[:-1]:  # Exclude root level
            if len(level) == 1:
                break

            # Get sibling index
            if current_index % 2 == 0:
                sibling_index = current_index + 1
            else:
                sibling_index = current_index - 1

            # Add sibling to proof (or self if at end)
            if sibling_index < len(level):
                proof.append(level[sibling_index])
            else:
                proof.append(level[current_index])

            # Move to parent index
            current_index = current_index // 2

        return proof

    def verify_proof(
        self,
        leaf_data: Dict[str, Any],
        proof: List[str],
        index: int,
    ) -> bool:
        """
        Verify a merkle proof.

        Args:
            leaf_data: The original payment data
            proof: List of sibling hashes
            index: Original leaf index

        Returns:
            True if proof is valid
        """
        current_hash = hash_leaf(leaf_data)
        current_index = index

        for sibling_hash in proof:
            if current_index % 2 == 0:
                current_hash = hash_pair(current_hash, sibling_hash)
            else:
                current_hash = hash_pair(sibling_hash, current_hash)
            current_index = current_index // 2

        return current_hash == self.root

    def to_dict(self) -> Dict[str, Any]:
        """Export tree as dictionary."""
        return {
            "root": self.root,
            "leaf_count": len(self.leaf_hashes),
            "leaves": self.leaf_hashes,
            "tree_levels": len(self.tree),
        }


def create_payment_merkle_tree(
    payments: List[Dict[str, Any]],
) -> Tuple[MerkleTree, List[Dict[str, Any]]]:
    """
    Create a merkle tree for payment records.

    Args:
        payments: List of payment dicts with:
            - worker_id
            - worker_hotkey
            - epoch
            - bytes_relayed
            - amount_earned

    Returns:
        Tuple of (MerkleTree, payments_with_proofs)
    """
    if not payments:
        return MerkleTree([]), []

    # Build tree
    tree = MerkleTree(payments)

    # Add proofs to each payment
    payments_with_proofs = []
    for i, payment in enumerate(payments):
        proof = tree.get_proof(i)
        payment_with_proof = {
            **payment,
            "leaf_index": i,
            "merkle_proof": json.dumps(proof),
        }
        payments_with_proofs.append(payment_with_proof)

    return tree, payments_with_proofs


def verify_payment_inclusion(
    payment: Dict[str, Any],
    merkle_root: str,
    proof: List[str],
    leaf_index: int,
) -> bool:
    """
    Verify that a payment is included in the merkle tree.

    Args:
        payment: Payment data (worker_id, amount, etc.)
        merkle_root: Expected root hash
        proof: Merkle proof (list of sibling hashes)
        leaf_index: Position in the tree

    Returns:
        True if payment is verified
    """
    # Reconstruct the leaf data used for hashing
    leaf_data = {
        "worker_id": payment["worker_id"],
        "worker_hotkey": payment["worker_hotkey"],
        "epoch": payment["epoch"],
        "bytes_relayed": payment["bytes_relayed"],
        "amount_earned": payment["amount_earned"],
    }

    current_hash = hash_leaf(leaf_data)
    current_index = leaf_index

    for sibling_hash in proof:
        if current_index % 2 == 0:
            current_hash = hash_pair(current_hash, sibling_hash)
        else:
            current_hash = hash_pair(sibling_hash, current_hash)
        current_index = current_index // 2

    return current_hash == merkle_root
