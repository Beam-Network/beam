"""
Rate Limiting Middleware for BEAM Orchestrator

Implements sliding window rate limiting with:
- Per-IP rate limiting for general API access
- Per-worker rate limiting for authenticated workers
- Per-endpoint rate limiting for sensitive operations
- DDoS protection with burst detection
"""

import asyncio
import logging
import time
from collections import defaultdict
from dataclasses import dataclass, field
from typing import Callable, Dict, Optional, Set

from fastapi import Request, Response
from starlette.middleware.base import BaseHTTPMiddleware
from starlette.responses import JSONResponse

logger = logging.getLogger(__name__)


@dataclass
class RateLimitConfig:
    """Configuration for a rate limit rule."""

    requests_per_minute: int = 60
    requests_per_second: int = 10
    burst_limit: int = 20  # Max requests in a 1-second burst
    block_duration_seconds: int = 60  # Duration to block after exceeding limits


@dataclass
class RateLimitBucket:
    """Sliding window rate limit bucket."""

    window_start: float = 0.0
    request_count: int = 0
    second_window_start: float = 0.0
    second_count: int = 0
    burst_timestamps: list = field(default_factory=list)
    blocked_until: float = 0.0


class RateLimiter:
    """
    Sliding window rate limiter with multiple tiers.

    Supports:
    - IP-based rate limiting
    - Worker-authenticated rate limiting (higher limits)
    - Endpoint-specific limits
    - Burst detection and blocking
    """

    def __init__(self):
        # Rate limit buckets by key (IP or worker_id)
        self._buckets: Dict[str, RateLimitBucket] = defaultdict(RateLimitBucket)

        # Blocked IPs/workers
        self._blocked: Dict[str, float] = {}

        # Endpoint-specific configs
        self._endpoint_configs: Dict[str, RateLimitConfig] = {}

        # Default configs by tier
        self._default_config = RateLimitConfig(
            requests_per_minute=60,
            requests_per_second=10,
            burst_limit=20,
            block_duration_seconds=60,
        )

        self._worker_config = RateLimitConfig(
            requests_per_minute=300,  # Higher for authenticated workers
            requests_per_second=30,
            burst_limit=50,
            block_duration_seconds=30,
        )

        self._validator_config = RateLimitConfig(
            requests_per_minute=600,  # Highest for validators
            requests_per_second=50,
            burst_limit=100,
            block_duration_seconds=10,
        )

        # Whitelist for internal/trusted IPs
        self._whitelist: Set[str] = {"127.0.0.1", "::1"}

        # Cleanup task
        self._cleanup_task: Optional[asyncio.Task] = None

    def configure_endpoint(self, path: str, config: RateLimitConfig) -> None:
        """Configure rate limits for a specific endpoint."""
        self._endpoint_configs[path] = config

    def add_to_whitelist(self, ip: str) -> None:
        """Add IP to whitelist (bypasses rate limiting)."""
        self._whitelist.add(ip)

    def remove_from_whitelist(self, ip: str) -> None:
        """Remove IP from whitelist."""
        self._whitelist.discard(ip)

    def _get_config(
        self, path: str, is_worker: bool = False, is_validator: bool = False
    ) -> RateLimitConfig:
        """Get rate limit config for path and client type."""
        # Check endpoint-specific config first
        if path in self._endpoint_configs:
            return self._endpoint_configs[path]

        # Use tier-based configs
        if is_validator:
            return self._validator_config
        if is_worker:
            return self._worker_config
        return self._default_config

    def _get_key(self, request: Request) -> str:
        """Generate rate limit key from request."""
        # Try to get worker ID from header for authenticated requests
        worker_id = request.headers.get("X-Worker-ID")
        if worker_id:
            return f"worker:{worker_id}"

        # Try to get validator hotkey
        validator_hotkey = request.headers.get("X-Validator-Hotkey")
        if validator_hotkey:
            return f"validator:{validator_hotkey}"

        # Fall back to IP
        client_ip = self._get_client_ip(request)
        return f"ip:{client_ip}"

    def _get_client_ip(self, request: Request) -> str:
        """Extract client IP from request, handling proxies."""
        # Check X-Forwarded-For header (common in proxy setups)
        forwarded = request.headers.get("X-Forwarded-For")
        if forwarded:
            # Take the first IP in the chain (original client)
            return forwarded.split(",")[0].strip()

        # Check X-Real-IP header
        real_ip = request.headers.get("X-Real-IP")
        if real_ip:
            return real_ip

        # Fall back to direct connection IP
        if request.client:
            return request.client.host
        return "unknown"

    def is_allowed(self, request: Request) -> tuple[bool, Optional[str], Optional[int]]:
        """
        Check if request is allowed under rate limits.

        Returns:
            (allowed, reason, retry_after_seconds)
        """
        client_ip = self._get_client_ip(request)

        # Check whitelist
        if client_ip in self._whitelist:
            return True, None, None

        key = self._get_key(request)
        now = time.time()

        # Check if blocked
        if key in self._blocked:
            if now < self._blocked[key]:
                retry_after = int(self._blocked[key] - now)
                return False, "Rate limit exceeded, temporarily blocked", retry_after
            else:
                # Unblock
                del self._blocked[key]

        # Determine client type
        is_worker = key.startswith("worker:")
        is_validator = key.startswith("validator:")

        # Get config for this request
        config = self._get_config(request.url.path, is_worker, is_validator)

        # Get or create bucket
        bucket = self._buckets[key]

        # Check per-minute limit (sliding window)
        minute_window_start = now - 60
        if bucket.window_start < minute_window_start:
            # Reset window
            bucket.window_start = now
            bucket.request_count = 0

        if bucket.request_count >= config.requests_per_minute:
            # Block this client
            self._blocked[key] = now + config.block_duration_seconds
            logger.warning(
                f"Rate limit exceeded for {key}: {bucket.request_count}/min (limit: {config.requests_per_minute})"
            )
            return False, "Rate limit exceeded (per minute)", config.block_duration_seconds

        # Check per-second limit
        second_window_start = now - 1
        if bucket.second_window_start < second_window_start:
            bucket.second_window_start = now
            bucket.second_count = 0

        if bucket.second_count >= config.requests_per_second:
            return False, "Rate limit exceeded (per second)", 1

        # Check burst (requests in last 100ms)
        burst_window = now - 0.1
        bucket.burst_timestamps = [t for t in bucket.burst_timestamps if t > burst_window]
        if len(bucket.burst_timestamps) >= config.burst_limit:
            logger.warning(
                f"Burst detected for {key}: {len(bucket.burst_timestamps)} requests in 100ms"
            )
            self._blocked[key] = now + config.block_duration_seconds
            return False, "Burst rate limit exceeded", config.block_duration_seconds

        # Request allowed - update counters
        bucket.request_count += 1
        bucket.second_count += 1
        bucket.burst_timestamps.append(now)

        return True, None, None

    async def start_cleanup(self) -> None:
        """Start background cleanup task."""
        if self._cleanup_task is None:
            self._cleanup_task = asyncio.create_task(self._cleanup_loop())

    async def stop_cleanup(self) -> None:
        """Stop background cleanup task."""
        if self._cleanup_task:
            self._cleanup_task.cancel()
            try:
                await self._cleanup_task
            except asyncio.CancelledError:
                pass
            self._cleanup_task = None

    async def _cleanup_loop(self) -> None:
        """Periodically clean up old buckets and expired blocks."""
        while True:
            try:
                await asyncio.sleep(60)  # Run every minute
                self._cleanup()
            except asyncio.CancelledError:
                break
            except Exception as e:
                logger.error(f"Error in rate limiter cleanup: {e}")

    def _cleanup(self) -> None:
        """Clean up stale rate limit data."""
        now = time.time()
        stale_threshold = now - 300  # 5 minutes

        # Clean up old buckets
        stale_keys = [
            key for key, bucket in self._buckets.items() if bucket.window_start < stale_threshold
        ]
        for key in stale_keys:
            del self._buckets[key]

        # Clean up expired blocks
        expired_blocks = [key for key, until in self._blocked.items() if until < now]
        for key in expired_blocks:
            del self._blocked[key]

        if stale_keys or expired_blocks:
            logger.debug(
                f"Rate limiter cleanup: removed {len(stale_keys)} buckets, {len(expired_blocks)} blocks"
            )

    def get_stats(self) -> dict:
        """Get rate limiter statistics."""
        return {
            "active_buckets": len(self._buckets),
            "blocked_clients": len(self._blocked),
            "whitelisted_ips": len(self._whitelist),
            "endpoint_configs": len(self._endpoint_configs),
        }


# Global rate limiter instance
_rate_limiter: Optional[RateLimiter] = None


def get_rate_limiter() -> RateLimiter:
    """Get global rate limiter instance."""
    global _rate_limiter
    if _rate_limiter is None:
        _rate_limiter = RateLimiter()
    return _rate_limiter


class RateLimitMiddleware(BaseHTTPMiddleware):
    """
    FastAPI middleware for rate limiting.

    Usage:
        from middleware.rate_limiting import RateLimitMiddleware, get_rate_limiter

        rate_limiter = get_rate_limiter()
        app.add_middleware(RateLimitMiddleware, rate_limiter=rate_limiter)
    """

    def __init__(self, app, rate_limiter: Optional[RateLimiter] = None):
        super().__init__(app)
        self.rate_limiter = rate_limiter or get_rate_limiter()

        # Paths to skip rate limiting (health checks, etc.)
        self.skip_paths = {
            "/health",
            "/health/live",
            "/health/ready",
            "/docs",
            "/openapi.json",
            "/redoc",
        }

    async def dispatch(self, request: Request, call_next: Callable) -> Response:
        """Process request through rate limiter."""
        # Skip WebSocket requests — BaseHTTPMiddleware cannot handle them
        if request.scope.get("type") == "websocket":
            return await call_next(request)

        # Skip rate limiting for certain paths
        if request.url.path in self.skip_paths:
            return await call_next(request)

        # Check rate limit
        allowed, reason, retry_after = self.rate_limiter.is_allowed(request)

        if not allowed:
            headers = {}
            if retry_after:
                headers["Retry-After"] = str(retry_after)

            return JSONResponse(
                status_code=429,
                content={
                    "error": "Too Many Requests",
                    "detail": reason,
                    "retry_after": retry_after,
                },
                headers=headers,
            )

        # Process request
        response = await call_next(request)

        # Add rate limit headers
        self.rate_limiter.get_stats()
        response.headers["X-RateLimit-Active"] = "true"

        return response
