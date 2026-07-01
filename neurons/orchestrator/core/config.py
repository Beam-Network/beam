"""
Orchestrator Configuration

Settings for the BEAM Orchestrator service (single-node deployment).
"""

import os
from functools import lru_cache
from typing import List, Optional

from pydantic import Field
from pydantic_settings import BaseSettings


class OrchestratorSettings(BaseSettings):
    """Orchestrator configuration settings."""

    # ==========================================================================
    # API Settings
    # ==========================================================================
    api_host: str = Field(default="0.0.0.0", env="ORCHESTRATOR_HOST")
    api_port: int = Field(default=8000, env="API_PORT")  # Also accepts ORCHESTRATOR_PORT
    log_level: str = Field(default="INFO", env="LOG_LEVEL")

    # Local mode - skip Bittensor wallet/subtensor initialization for development
    local_mode: bool = Field(default=False, env="LOCAL_MODE")
    local_orchestrator_hotkey: str = Field(
        default="local-dev-hotkey", env="LOCAL_ORCHESTRATOR_HOTKEY"
    )

    # Add mock worker for testing (use with real wallet but no real miners)
    add_mock_worker: bool = Field(default=False, env="ADD_MOCK_WORKER")

    # Mock worker hotkey for local task-result simulation
    mock_worker_hotkey: Optional[str] = Field(default=None, env="MOCK_WORKER_HOTKEY")

    region: str = Field(default="US", env="REGION")  # US, EU, APAC, RESERVE (BeamCore registration)

    # ==========================================================================
    # Subnet Settings
    # ==========================================================================
    netuid: int = Field(default=105, env="NETUID")
    subtensor_network: str = Field(default="finney", env="SUBTENSOR_NETWORK")
    subtensor_address: Optional[str] = Field(default=None, env="SUBTENSOR_ADDRESS")

    # ==========================================================================
    # Orchestrator Wallet (for signing reports to validators)
    # ==========================================================================
    wallet_name: str = Field(default="orchestrator", env="WALLET_NAME")
    wallet_hotkey: str = Field(default="default", env="WALLET_HOTKEY")
    wallet_path: str = Field(default="~/.bittensor/wallets", env="WALLET_PATH")

    # ==========================================================================
    # Orchestrator UID (on-chain miner slot)
    # ==========================================================================
    # If set, uses this UID for registration. Otherwise auto-detects from metagraph.
    # Get your UID: btcli subnet metagraph --netuid 105 --subtensor.network finney
    uid: Optional[int] = Field(default=None, env="ORCHESTRATOR_UID")

    # ==========================================================================
    # Fee Settings (% of emission shared with workers)
    # ==========================================================================
    fee_percentage: float = Field(default=0.0, env="FEE_PERCENTAGE")  # 0-100%

    # ==========================================================================
    # Compensation reference settings
    # ==========================================================================
    # Reference per-chunk amount for local accounting and operator-defined compensation workflows.
    alpha_per_chunk: float = Field(default=0.5, env="ALPHA_PER_CHUNK")

    # ==========================================================================
    # Readiness
    # ==========================================================================
    # When True, orchestrator signals BeamCore that it is ready to receive transfers.
    # Set via READY=true env var or the websocket set_ready flow at runtime.
    # Default is False — new orchestrators are excluded from routing until explicit opt-in.
    ready: bool = Field(default=False, env="READY")

    # ==========================================================================
    # Worker Management
    # ==========================================================================
    max_workers: int = Field(default=10000, env="MAX_WORKERS")
    worker_timeout_seconds: int = Field(default=300, env="WORKER_TIMEOUT")
    min_worker_bandwidth_mbps: float = Field(default=10.0, env="MIN_WORKER_BANDWIDTH")
    worker_heartbeat_interval: int = Field(default=30, env="WORKER_HEARTBEAT_INTERVAL")

    # ==========================================================================
    # Task Settings
    # ==========================================================================
    max_concurrent_tasks: int = Field(default=1000, env="MAX_CONCURRENT_TASKS")
    task_timeout_seconds: int = Field(default=120, env="TASK_TIMEOUT")
    chunk_size_bytes: int = Field(default=1024 * 1024, env="CHUNK_SIZE")  # 1 MB

    # ==========================================================================
    # Anti-Fraud Settings
    # ==========================================================================
    enable_geo_verification: bool = Field(default=True, env="ENABLE_GEO_VERIFICATION")
    enable_latency_verification: bool = Field(default=True, env="ENABLE_LATENCY_VERIFICATION")
    max_suspicious_score: float = Field(default=0.3, env="MAX_SUSPICIOUS_SCORE")

    # ==========================================================================
    # BeamCore API
    # ==========================================================================
    core_server_url: str = Field(default="https://beamcore.b1m.ai", env="CORE_SERVER_URL")

    orch_gateway_url: Optional[str] = Field(default=None, env="ORCH_GATEWAY_URL")

    # Orch-gateway WebSocket transport (high-latency / cross-region: increase these)
    orch_ws_open_timeout: float = Field(default=60.0, env="ORCH_WS_OPEN_TIMEOUT")
    orch_ws_close_timeout: float = Field(default=20.0, env="ORCH_WS_CLOSE_TIMEOUT")
    orch_ws_ping_interval: float = Field(default=30.0, env="ORCH_WS_PING_INTERVAL")
    orch_ws_ping_timeout: float = Field(default=45.0, env="ORCH_WS_PING_TIMEOUT")

    worker_gateway_url: Optional[str] = Field(default=None, env="ORCHESTRATOR_WORKER_GATEWAY_URL")

    # ==========================================================================
    # Worker Scoring Weights (for selection)
    # ==========================================================================
    weight_trust: float = Field(default=0.30, env="WEIGHT_TRUST")
    weight_latency: float = Field(default=0.25, env="WEIGHT_LATENCY")
    weight_load: float = Field(default=0.20, env="WEIGHT_LOAD")
    weight_bandwidth: float = Field(default=0.15, env="WEIGHT_BANDWIDTH")
    weight_success: float = Field(default=0.10, env="WEIGHT_SUCCESS")

    # ==========================================================================
    # Reward Distribution Weights
    # ==========================================================================
    # Primary factor: bytes relayed (work done)
    reward_weight_bytes: float = Field(default=0.50, env="REWARD_WEIGHT_BYTES")
    # Quality factors
    reward_weight_success_rate: float = Field(default=0.20, env="REWARD_WEIGHT_SUCCESS_RATE")
    reward_weight_latency: float = Field(default=0.15, env="REWARD_WEIGHT_LATENCY")
    reward_weight_trust: float = Field(default=0.15, env="REWARD_WEIGHT_TRUST")

    # ==========================================================================
    # BEAM Storage Settings (Hub)
    # ==========================================================================
    storage_gateway_url: str = Field(
        default="https://storage.beam.network", env="STORAGE_GATEWAY_URL"
    )
    storage_replication_factor: int = Field(default=3, env="STORAGE_REPLICATION_FACTOR")

    # External IP for registration (auto-detected if not set)
    external_ip: Optional[str] = Field(default=None, env="EXTERNAL_IP")

    # ==========================================================================
    # Client Authentication
    # ==========================================================================
    # Master toggle for client authentication
    client_auth_enabled: bool = Field(default=True, env="CLIENT_AUTH_ENABLED")

    # If true, only whitelisted clients can register
    client_whitelist_only: bool = Field(default=False, env="CLIENT_WHITELIST_ONLY")

    # Pre-approved hotkeys (comma-separated SS58 addresses)
    client_pre_approved_hotkeys: Optional[str] = Field(
        default=None, env="CLIENT_PRE_APPROVED_HOTKEYS"
    )

    # Admin hotkeys for client management (comma-separated SS58 addresses)
    client_admin_hotkeys: Optional[str] = Field(default=None, env="CLIENT_ADMIN_HOTKEYS")

    # Signature expiration time (seconds)
    client_signature_max_age_seconds: int = Field(
        default=300, env="CLIENT_SIGNATURE_MAX_AGE_SECONDS"
    )

    # ==========================================================================
    # Subnet Participant Authentication (Validators & Workers)
    # ==========================================================================
    # Master toggle for subnet participant auth (validators and workers)
    subnet_auth_enabled: bool = Field(default=True, env="SUBNET_AUTH_ENABLED")

    # Require metagraph verification (hotkey must be registered on subnet)
    subnet_auth_require_metagraph: bool = Field(default=True, env="SUBNET_AUTH_REQUIRE_METAGRAPH")

    # Whitelisted hotkeys that bypass metagraph check (comma-separated)
    subnet_auth_whitelist: Optional[str] = Field(default=None, env="SUBNET_AUTH_WHITELIST")

    # ==========================================================================
    # Subnet Partner Program (free access for other Bittensor subnets)
    # ==========================================================================
    # Enable subnet partner registration (hotkeys from other subnets get free access)
    subnet_partner_enabled: bool = Field(default=True, env="SUBNET_PARTNER_ENABLED")

    def model_post_init(self, __context) -> None:
        object.__setattr__(self, "log_level", self.log_level.upper())

        if not self.orch_gateway_url:
            self.orch_gateway_url = os.environ.get("ORCHESTRATOR_WS_BASE_URL")

        if not self.orch_gateway_url:
            raise ValueError("ORCH_GATEWAY_URL is required")

    # ==========================================================================
    # Client Tiers
    # ==========================================================================
    # Basic tier
    client_tier_basic_rpm: int = Field(default=30, env="CLIENT_TIER_BASIC_RPM")
    client_tier_basic_daily_bytes: int = Field(
        default=1_073_741_824, env="CLIENT_TIER_BASIC_DAILY_BYTES"
    )  # 1GB
    client_tier_basic_concurrent: int = Field(default=2, env="CLIENT_TIER_BASIC_CONCURRENT")

    # Standard tier
    client_tier_standard_rpm: int = Field(default=120, env="CLIENT_TIER_STANDARD_RPM")
    client_tier_standard_daily_bytes: int = Field(
        default=10_737_418_240, env="CLIENT_TIER_STANDARD_DAILY_BYTES"
    )  # 10GB
    client_tier_standard_concurrent: int = Field(default=10, env="CLIENT_TIER_STANDARD_CONCURRENT")

    # Premium tier
    client_tier_premium_rpm: int = Field(default=600, env="CLIENT_TIER_PREMIUM_RPM")
    client_tier_premium_daily_bytes: int = Field(
        default=107_374_182_400, env="CLIENT_TIER_PREMIUM_DAILY_BYTES"
    )  # 100GB
    client_tier_premium_concurrent: int = Field(default=50, env="CLIENT_TIER_PREMIUM_CONCURRENT")

    # ==========================================================================
    # CORS Settings
    # ==========================================================================
    # Allowed origins for CORS (comma-separated, use "*" for all - NOT RECOMMENDED for production)
    cors_allowed_origins: str = Field(default="", env="CORS_ALLOWED_ORIGINS")

    # Allow credentials (cookies, authorization headers)
    cors_allow_credentials: bool = Field(default=False, env="CORS_ALLOW_CREDENTIALS")

    # Allowed HTTP methods (comma-separated)
    cors_allowed_methods: str = Field(
        default="GET,POST,PUT,DELETE,OPTIONS", env="CORS_ALLOWED_METHODS"
    )

    # Allowed HTTP headers (comma-separated)
    cors_allowed_headers: str = Field(default="*", env="CORS_ALLOWED_HEADERS")

    # ==========================================================================
    # Compliance / Audit Settings
    # ==========================================================================
    # Enable audit event publishing to BeamCore
    audit_enabled: bool = Field(default=True, env="AUDIT_ENABLED")

    # Redis URL for audit event queue (same Redis as BeamCore consumes from)
    audit_redis_url: Optional[str] = Field(default=None, env="AUDIT_REDIS_URL")

    # Redis stream name for audit events
    audit_stream: str = Field(default="audit:events", env="AUDIT_STREAM")

    # Source identifier for audit events
    audit_source: str = Field(default="datapipe_subnet", env="AUDIT_SOURCE")

    class Config:
        env_file = ".env"
        extra = "ignore"

    def get_pre_approved_hotkeys(self) -> List[str]:
        """Parse pre-approved client hotkeys from comma-separated string."""
        if not self.client_pre_approved_hotkeys:
            return []
        return [h.strip() for h in self.client_pre_approved_hotkeys.split(",") if h.strip()]

    def get_client_admin_hotkeys(self) -> List[str]:
        """Parse client admin hotkeys from comma-separated string."""
        admins = []
        if self.client_admin_hotkeys:
            admins.extend([h.strip() for h in self.client_admin_hotkeys.split(",") if h.strip()])
        return admins

    def get_subnet_auth_whitelist(self) -> set:
        """Parse subnet auth whitelist from comma-separated string."""
        if not self.subnet_auth_whitelist:
            return set()
        return {h.strip() for h in self.subnet_auth_whitelist.split(",") if h.strip()}

    def get_tier_config(self, tier: str) -> dict:
        """
        Get configuration for a specific tier.

        Args:
            tier: "basic", "standard", or "premium"

        Returns:
            Dict with rpm, daily_bytes, concurrent limits
        """
        tier_configs = {
            "basic": {
                "rate_limit_rpm": self.client_tier_basic_rpm,
                "daily_transfer_limit_bytes": self.client_tier_basic_daily_bytes,
                "max_concurrent_transfers": self.client_tier_basic_concurrent,
            },
            "standard": {
                "rate_limit_rpm": self.client_tier_standard_rpm,
                "daily_transfer_limit_bytes": self.client_tier_standard_daily_bytes,
                "max_concurrent_transfers": self.client_tier_standard_concurrent,
            },
            "premium": {
                "rate_limit_rpm": self.client_tier_premium_rpm,
                "daily_transfer_limit_bytes": self.client_tier_premium_daily_bytes,
                "max_concurrent_transfers": self.client_tier_premium_concurrent,
            },
        }
        return tier_configs.get(tier, tier_configs["basic"])

    def get_cors_origins(self) -> List[str]:
        """
        Parse CORS allowed origins from comma-separated string.

        Returns empty list if not configured (CORS disabled).
        """
        if not self.cors_allowed_origins:
            return []
        return [o.strip() for o in self.cors_allowed_origins.split(",") if o.strip()]

    def get_cors_methods(self) -> List[str]:
        """Parse CORS allowed methods from comma-separated string."""
        return [m.strip() for m in self.cors_allowed_methods.split(",") if m.strip()]

    def get_cors_headers(self) -> List[str]:
        """Parse CORS allowed headers from comma-separated string."""
        if self.cors_allowed_headers == "*":
            return ["*"]
        return [h.strip() for h in self.cors_allowed_headers.split(",") if h.strip()]


@lru_cache
def get_settings() -> OrchestratorSettings:
    """Get cached settings instance."""
    return OrchestratorSettings()
