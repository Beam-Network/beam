"""
Validator Configuration

Settings for the Validator node.

Environment variables use BEAM_VALIDATOR_ prefix, except for shared subnet config:
- BEAM_NETUID (shared across all subnet components)
- BEAM_SUBTENSOR_NETWORK (shared across all subnet components)
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
    # Bittensor Configuration (Testnet: netuid=304, network=test)
    # These use BEAM_ prefix (shared across subnet components)
    # ==========================================================================

    wallet_name: str = "default"
    wallet_hotkey: str = "default"
    wallet_path: str = "~/.bittensor/wallets"

    # Shared subnet config - uses BEAM_ prefix (not BEAM_VALIDATOR_)
    netuid: int = Field(default=304, validation_alias="BEAM_NETUID")
    subtensor_network: str = Field(default="test", validation_alias="BEAM_SUBTENSOR_NETWORK")
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

    blocks_between_weights: int = 100  # ~20 minutes
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

    # URL of the private BeamCore service for score submission
    subnet_core_url: str = "http://localhost:8080"

    # ==========================================================================
    # Redis (for caching, optional)
    # ==========================================================================

    redis_host: str = "localhost"
    redis_port: int = 6379
    redis_db: int = 1
    redis_password: Optional[str] = None

    # Legacy: Database URL - kept for backward compatibility but not used for score submission
    # Score submission now goes through BeamCore API
    database_url: Optional[str] = None

    # ==========================================================================
    # Payment Proof Verification
    # ==========================================================================

    # Penalty for missing payment proofs (0.0 - 1.0, multiplied against score)
    missing_proof_penalty: float = 0.5  # 50% score reduction
    invalid_proof_penalty: float = 0.3  # 30% score reduction
    proof_lookback_epochs: int = 10  # How many epochs to consider for compliance

    # ==========================================================================
    # Orchestrator Connection (v2)
    # ==========================================================================

    orchestrator_url: str = "http://localhost:8000"  # Default orchestrator endpoint

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
    # Check for LOCAL_MODE without prefix (for consistency with orchestrator)
    local_mode = os.getenv("LOCAL_MODE", "").lower() in ("true", "1", "yes")

    # Allow DATABASE_URL without prefix as fallback
    db_url = (
        os.getenv("BEAM_VALIDATOR_DATABASE_URL") or
        os.getenv("BEAM_DATABASE_URL") or
        os.getenv("DATABASE_URL")
    )

    if db_url:
        return Settings(local_mode=local_mode, database_url=db_url)
    return Settings(local_mode=local_mode)
