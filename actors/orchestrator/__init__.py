"""
BEAM Orchestrator - Central coordinator for decentralized bandwidth mining.

The Orchestrator is a centralized service that:
- Manages unlimited off-chain workers directly
- Assigns tasks to workers based on geographic and performance criteria
- Aggregates bandwidth proofs for validator verification
- Reports work summaries to validators for scoring

This architecture bypasses the 192 miner UID limitation by moving worker management off-chain.

Note: Bittensor subnets have 64 validator UIDs and 192 miner UIDs max.
"""
