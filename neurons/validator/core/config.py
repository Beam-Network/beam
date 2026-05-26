"""
Validator Configuration

Settings for the Validator node.

Environment variables use the BEAM_VALIDATOR_ prefix, except for shared subnet config:
- NETUID
- SUBTENSOR_NETWORK
"""

from functools import lru_cache
from typing import Optional

from pydantic import Field
from pydantic_settings import BaseSettings, SettingsConfigDict


class Settings(BaseSettings):
    """Validator node settings loaded from environment variables"""

    model_config = SettingsConfigDict(
        env_file=".env",
        env_file_encoding="utf-8",
        env_prefix="BEAM_VALIDATOR_",
        case_sensitive=False,
        extra="ignore",
    )

    # ==========================================================================
    # Local Development Mode
    # ==========================================================================

    # Note: LOCAL_MODE is read directly from env (no prefix) for consistency
    local_mode: bool = False

    # ==========================================================================
    # Bittensor Configuration
    # ==========================================================================

    wallet_name: str = "default"
    wallet_hotkey: str = "default"
    wallet_path: str = "~/.bittensor/wallets"

    netuid: int = Field(default=105, validation_alias="NETUID")
    subtensor_network: str = Field(default="finney", validation_alias="SUBTENSOR_NETWORK")
    subtensor_address: Optional[str] = None

    # ==========================================================================
    # Network Configuration
    # ==========================================================================

    external_ip: Optional[str] = None
    external_url: Optional[str] = None  # e.g., https://validator.yourplatform.com/
    port: int = 8093

    # ==========================================================================
    # Validation Settings
    # ==========================================================================

    # Task generation
    task_interval_seconds: int = 12  # How often to generate tasks
    tasks_per_epoch: int = 10  # Tasks per connection per epoch
    chunk_size_bytes: int = 10_485_760  # 10 MB

    # PoB verification
    max_clock_drift_us: int = 5_000_000  # 5 second max clock drift
    min_transfer_time_us: int = 50_000  # 50ms minimum (anti-loopback)

    # Bandwidth thresholds
    min_bandwidth_mbps: float = 50.0  # Minimum to be scored
    max_bandwidth_mbps: float = 10_000.0  # 10 Gbps cap

    # ==========================================================================
    # Weight Setting
    # ==========================================================================

    # Override via BEAM_VALIDATOR_BLOCKS_BETWEEN_WEIGHTS env var (e.g., =360 for testnet ~100s/block)
    blocks_between_weights: int = 100  # ~20 minutes on mainnet
    weight_alpha: float = 0.3  # EMA smoothing factor

    # ==========================================================================
    # Scoring Weights
    # ==========================================================================

    score_weight_bandwidth: float = 0.50
    score_weight_uptime: float = 0.20
    score_weight_loss: float = 0.15
    score_weight_tier: float = 0.15

    # ==========================================================================
    # BeamCore API (replaces direct DB access)
    # ==========================================================================

    # BeamCore HTTP base URL for score submission
    core_server_url: str = "https://beamcore.b1m.ai"
    # Required for PoB routes (requireAuth). Validator routes use hotkey signatures only.
    subnet_core_api_key: Optional[str] = None

    # ==========================================================================
    # Redis (for caching, optional)
    # ==========================================================================

    redis_host: str = "localhost"
    redis_port: int = 6379
    redis_db: int = 1
    redis_password: Optional[str] = None

    # ==========================================================================
    # Payment Proof Verification
    # ==========================================================================

    # Penalty for missing payment proofs (0.0 - 1.0, multiplied against score)
    missing_proof_penalty: float = 0.5  # 50% score reduction
    invalid_proof_penalty: float = 0.3  # 30% score reduction
    proof_lookback_epochs: int = 10  # How many epochs to consider for compliance

    # Local-mode only fallback URL used by the standalone validator/orchestrator harness.
    orchestrator_url: str = "http://localhost:8000"

    # ==========================================================================
    # Sync & Timing
    # ==========================================================================

    sync_interval: int = 12  # Metagraph sync interval (match tempo)
    job_timeout_seconds: int = 60  # Task completion timeout

    # ==========================================================================
    # Logging
    # ==========================================================================

    log_level: str = "INFO"
    debug: bool = False


@lru_cache()
def get_settings() -> Settings:
    """Get cached settings instance"""
    import os
    from pathlib import Path

    # Load .env file so LOCAL_MODE is available via os.getenv
    env_file = Path(".env")
    if env_file.exists():
        try:
            from dotenv import load_dotenv
            load_dotenv(env_file, override=False)
        except ImportError:
            pass

    # Accept LOCAL_MODE (no prefix) or BEAM_VALIDATOR_LOCAL_MODE
    local_mode = (
        os.getenv("LOCAL_MODE", "").lower() in ("true", "1", "yes")
        or os.getenv("BEAM_VALIDATOR_LOCAL_MODE", "").lower() in ("true", "1", "yes")
    )
    return Settings(local_mode=local_mode)
