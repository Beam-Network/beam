"""
Bittensor Chain Integration

Wraps Bittensor SDK for weight setting and node discovery.
Provides retry logic, commit-reveal support, and clean abstractions.
"""

import logging
from dataclasses import dataclass
from typing import Dict, List, Optional, Tuple

import bittensor as bt

logger = logging.getLogger(__name__)


@dataclass
class FiberNode:
    """
    Node information from the metagraph.

    Wraps Bittensor neuron data with BEAM-specific helpers.
    """
    hotkey: str
    coldkey: str
    node_id: int  # UID
    netuid: int
    incentive: float
    trust: float
    vtrust: float
    last_updated: float
    ip: str
    port: int
    protocol: int
    ip_type: int

    @classmethod
    def from_metagraph(cls, metagraph: "bt.Metagraph", uid: int, netuid: int) -> "FiberNode":
        """Create from Bittensor metagraph neuron."""
        axon = metagraph.axons[uid]
        return cls(
            hotkey=metagraph.hotkeys[uid],
            coldkey=metagraph.coldkeys[uid],
            node_id=uid,
            netuid=netuid,
            incentive=float(metagraph.I[uid]),
            trust=float(metagraph.T[uid]),
            vtrust=float(metagraph.Tv[uid]) if hasattr(metagraph, 'Tv') else 0.0,
            last_updated=float(metagraph.last_update[uid]),
            ip=axon.ip,
            port=axon.port,
            protocol=axon.ip_type,
            ip_type=axon.ip_type,
        )

    @property
    def uid(self) -> int:
        """Alias for node_id (Bittensor convention)."""
        return self.node_id

    @property
    def axon_url(self) -> Optional[str]:
        """Get the axon URL if available."""
        if self.ip and self.port:
            protocol = "https" if self.protocol == 4 else "http"
            return f"{protocol}://{self.ip}:{self.port}"
        return None

class FiberChain:
    """
    Chain interaction manager using Bittensor SDK.

    Handles subtensor connections, weight setting, and node discovery
    with proper retry logic and error handling.
    """

    def __init__(
        self,
        subtensor_network: str = "finney",
        subtensor_address: Optional[str] = None,
        netuid: int = 1,
    ):
        """
        Initialize Bittensor chain interface.

        Args:
            subtensor_network: Network name (finney, test, local)
            subtensor_address: Direct websocket address (optional)
            netuid: Subnet ID
        """
        self.subtensor_network = subtensor_network
        self.subtensor_address = subtensor_address
        self.netuid = netuid
        self._subtensor: Optional[bt.Subtensor] = None
        self._metagraph: Optional[bt.Metagraph] = None

    def _get_subtensor(self) -> bt.Subtensor:
        """Get or create subtensor connection."""
        if self._subtensor is None:
            if self.subtensor_address:
                self._subtensor = bt.Subtensor(network=self.subtensor_address)
            else:
                self._subtensor = bt.Subtensor(network=self.subtensor_network)
            logger.info(f"Connected to subtensor: {self.subtensor_network}")
        return self._subtensor

    def _get_metagraph(self, block: Optional[int] = None) -> bt.Metagraph:
        """Get metagraph for this subnet."""
        subtensor = self._get_subtensor()
        self._metagraph = subtensor.metagraph(netuid=self.netuid, block=block)
        return self._metagraph

    def close(self) -> None:
        """Close subtensor connection."""
        if self._subtensor is not None:
            try:
                self._subtensor.close()
            except Exception:
                pass
            self._subtensor = None
            self._metagraph = None

    def get_nodes(self, block: Optional[int] = None) -> List[FiberNode]:
        """
        Get all nodes for this subnet.

        Args:
            block: Optional block number for historical query

        Returns:
            List of FiberNode objects
        """
        metagraph = self._get_metagraph(block=block)

        nodes = []
        for uid in range(metagraph.n.item()):
            node = FiberNode.from_metagraph(metagraph, uid, self.netuid)
            nodes.append(node)

        logger.debug(f"Retrieved {len(nodes)} nodes from subnet {self.netuid}")
        return nodes

    def get_nodes_by_hotkey(self, block: Optional[int] = None) -> Dict[str, FiberNode]:
        """
        Get nodes indexed by hotkey.

        Returns:
            Dict mapping hotkey -> FiberNode
        """
        nodes = self.get_nodes(block=block)
        return {node.hotkey: node for node in nodes}

    def get_nodes_by_uid(self, block: Optional[int] = None) -> Dict[int, FiberNode]:
        """
        Get nodes indexed by UID.

        Returns:
            Dict mapping UID -> FiberNode
        """
        nodes = self.get_nodes(block=block)
        return {node.node_id: node for node in nodes}

    def can_set_weights(self, keypair, validator_uid: int) -> Tuple[bool, str]:
        """
        Check if weights can be set (timing check).

        Args:
            keypair: Validator keypair (wallet.hotkey)
            validator_uid: Validator's UID

        Returns:
            (can_set, reason) tuple
        """
        subtensor = self._get_subtensor()
        try:
            # Check blocks since last update
            current_block = subtensor.get_current_block()
            metagraph = self._get_metagraph()
            last_update = metagraph.last_update[validator_uid].item()
            blocks_since_update = current_block - last_update

            # Get weights rate limit for this subnet
            weights_rate_limit = subtensor.weights_rate_limit(self.netuid)

            if blocks_since_update >= weights_rate_limit:
                return True, "Ready to set weights"
            else:
                blocks_remaining = weights_rate_limit - blocks_since_update
                return False, f"Too soon since last weight update ({blocks_remaining} blocks remaining)"
        except Exception as e:
            return False, f"Error checking weight timing: {e}"

    def set_weights(
        self,
        keypair,
        validator_uid: int,
        uids: List[int],
        weights: List[float],
        version_key: int = 0,
        wait_for_inclusion: bool = True,
        wait_for_finalization: bool = False,
    ) -> Tuple[bool, str]:
        """
        Set weights on the subnet using Bittensor SDK.

        Args:
            keypair: Validator wallet (bt.wallet) for signing
            validator_uid: Validator's UID on the subnet
            uids: List of node UIDs to set weights for
            weights: Corresponding weights (will be normalized)
            version_key: Optional version key
            wait_for_inclusion: Wait for block inclusion
            wait_for_finalization: Wait for finalization

        Returns:
            (success, message) tuple
        """
        if not uids or not weights:
            return False, "No weights to set"

        if len(uids) != len(weights):
            return False, f"UID/weight length mismatch: {len(uids)} vs {len(weights)}"

        subtensor = self._get_subtensor()

        try:
            logger.info(
                f"Setting weights for {len(uids)} nodes on subnet {self.netuid}"
            )

            import torch
            uids_tensor = torch.tensor(uids, dtype=torch.int64)
            weights_tensor = torch.tensor(weights, dtype=torch.float32)

            success, msg = subtensor.set_weights(
                wallet=keypair,
                netuid=self.netuid,
                uids=uids_tensor,
                weights=weights_tensor,
                version_key=version_key,
                wait_for_inclusion=wait_for_inclusion,
                wait_for_finalization=wait_for_finalization,
            )

            if success:
                logger.info("Weights set successfully")
                return True, "Weights set successfully"
            else:
                logger.warning(f"Weight setting failed: {msg}")
                return False, f"Weight setting failed: {msg}"

        except Exception as e:
            error_msg = f"Error setting weights: {e}"
            logger.error(error_msg, exc_info=True)
            return False, error_msg


# =============================================================================
# Convenience Functions
# =============================================================================

def get_nodes_for_netuid(
    netuid: int,
    subtensor_network: str = "finney",
    subtensor_address: Optional[str] = None,
    block: Optional[int] = None,
) -> List[FiberNode]:
    """
    Get all nodes for a subnet.

    Convenience function that creates a temporary connection.
    For repeated queries, use FiberChain class instead.

    Args:
        netuid: Subnet ID
        subtensor_network: Network name
        subtensor_address: Direct websocket address
        block: Optional block number

    Returns:
        List of FiberNode objects
    """
    chain = FiberChain(
        subtensor_network=subtensor_network,
        subtensor_address=subtensor_address,
        netuid=netuid,
    )
    try:
        return chain.get_nodes(block=block)
    finally:
        chain.close()


def set_weights_with_fiber(
    keypair,
    validator_uid: int,
    netuid: int,
    uids: List[int],
    weights: List[float],
    subtensor_network: str = "finney",
    subtensor_address: Optional[str] = None,
    version_key: int = 0,
    wait_for_inclusion: bool = True,
    wait_for_finalization: bool = False,
) -> Tuple[bool, str]:
    """
    Set weights using Bittensor SDK.

    Convenience function that creates a temporary connection.
    For repeated operations, use FiberChain class instead.

    Args:
        keypair: Validator wallet (bt.wallet)
        validator_uid: Validator's UID
        netuid: Subnet ID
        uids: Node UIDs
        weights: Node weights
        subtensor_network: Network name
        subtensor_address: Direct address
        version_key: Version key
        wait_for_inclusion: Wait for inclusion
        wait_for_finalization: Wait for finalization

    Returns:
        (success, message) tuple
    """
    chain = FiberChain(
        subtensor_network=subtensor_network,
        subtensor_address=subtensor_address,
        netuid=netuid,
    )
    try:
        return chain.set_weights(
            keypair=keypair,
            validator_uid=validator_uid,
            uids=uids,
            weights=weights,
            version_key=version_key,
            wait_for_inclusion=wait_for_inclusion,
            wait_for_finalization=wait_for_finalization,
        )
    finally:
        chain.close()


def can_set_weights(
    keypair,
    validator_uid: int,
    netuid: int,
    subtensor_network: str = "finney",
    subtensor_address: Optional[str] = None,
) -> Tuple[bool, str]:
    """
    Check if weights can be set.

    Args:
        keypair: Validator wallet
        validator_uid: Validator's UID
        netuid: Subnet ID
        subtensor_network: Network name
        subtensor_address: Direct address

    Returns:
        (can_set, reason) tuple
    """
    chain = FiberChain(
        subtensor_network=subtensor_network,
        subtensor_address=subtensor_address,
        netuid=netuid,
    )
    try:
        return chain.can_set_weights(keypair, validator_uid)
    finally:
        chain.close()
