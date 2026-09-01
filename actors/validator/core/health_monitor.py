"""Validator health monitoring service."""

import asyncio
import logging
import time
from datetime import datetime
from typing import Any, Callable, Dict, List, Optional

from core.redundancy_config import (
    CRITICAL_THRESHOLD_FAILURES,
    HEALTH_CHECK_INTERVAL_SECONDS,
    UNHEALTHY_THRESHOLD_FAILURES,
)
from core.redundancy_models import (
    ComponentHealth,
    HealthCheck,
    HealthReport,
    HealthStatus,
)

logger = logging.getLogger("core.redundancy")


class HealthMonitor:
    """
    Monitors validator health and detects issues proactively.

    Runs periodic health checks on critical components:
    - Bittensor connection
    - Metagraph sync
    - Memory usage
    - Task processing
    - Weight setting
    """

    def __init__(self, validator: Any):
        self.validator = validator
        self.start_time = datetime.utcnow()
        self.consecutive_failures = 0
        self.last_healthy = datetime.utcnow()
        self.checks: List[HealthCheck] = []
        self._running = False
        self._task: Optional[asyncio.Task] = None

        # Custom health check functions
        self._custom_checks: Dict[str, Callable] = {}

    async def start(self) -> None:
        """Start the health monitoring loop"""
        if self._running:
            return

        self._running = True
        self._task = asyncio.create_task(self._health_loop())
        logger.info("Health monitor started")

    async def stop(self) -> None:
        """Stop the health monitoring loop"""
        self._running = False
        if self._task:
            self._task.cancel()
            try:
                await self._task
            except asyncio.CancelledError:
                pass
        logger.info("Health monitor stopped")

    def register_check(self, name: str, check_fn: Callable) -> None:
        """Register a custom health check function"""
        self._custom_checks[name] = check_fn

    async def _health_loop(self) -> None:
        """Main health checking loop"""
        await asyncio.sleep(HEALTH_CHECK_INTERVAL_SECONDS)
        while self._running:
            try:
                report = await self.run_health_checks()

                if report.status == HealthStatus.HEALTHY:
                    self.consecutive_failures = 0
                    self.last_healthy = datetime.utcnow()
                else:
                    self.consecutive_failures += 1
                    logger.warning(
                        f"Health check failed: {report.status.value}, "
                        f"consecutive failures: {self.consecutive_failures}"
                    )

                    if self.consecutive_failures >= CRITICAL_THRESHOLD_FAILURES:
                        logger.error("Validator health is CRITICAL - intervention may be needed")
                        # Could trigger alerts here

            except Exception as e:
                logger.error(f"Error in health check loop: {e}")
                self.consecutive_failures += 1

            await asyncio.sleep(HEALTH_CHECK_INTERVAL_SECONDS)

    async def run_health_checks(self) -> HealthReport:
        """Run all health checks and return a report"""
        checks = []

        # Check 1: Bittensor connection
        checks.append(await self._check_bittensor_connection())

        # Check 2: Metagraph sync
        checks.append(await self._check_metagraph())

        # Check 3: Memory usage
        checks.append(self._check_memory())

        # Check 4: Task processing
        checks.append(self._check_task_processing())

        # Check 5: Weight setting recency
        checks.append(self._check_weight_setting())

        # Run custom checks
        for name, check_fn in self._custom_checks.items():
            try:
                result = await check_fn() if asyncio.iscoroutinefunction(check_fn) else check_fn()
                checks.append(result)
            except Exception as e:
                checks.append(
                    HealthCheck(
                        component=name,
                        status=ComponentHealth.ERROR,
                        message=f"Check failed: {e}",
                    )
                )

        self.checks = checks

        # Determine overall status
        status = self._determine_status(checks)

        return HealthReport(
            status=status,
            checks=checks,
            consecutive_failures=self.consecutive_failures,
            last_healthy=self.last_healthy,
            uptime_seconds=(datetime.utcnow() - self.start_time).total_seconds(),
        )

    async def _check_bittensor_connection(self) -> HealthCheck:
        """Check Bittensor subtensor connection"""
        start = time.time()

        if self.validator.subtensor is None:
            return HealthCheck(
                component="bittensor",
                status=ComponentHealth.ERROR,
                message="Subtensor not initialized",
            )

        try:
            block = self.validator.subtensor.block
            latency = (time.time() - start) * 1000

            if block is None or block <= 0:
                return HealthCheck(
                    component="bittensor",
                    status=ComponentHealth.WARNING,
                    message="Unable to get block number",
                    latency_ms=latency,
                )

            return HealthCheck(
                component="bittensor",
                status=ComponentHealth.OK,
                message=f"Connected, block {block}",
                latency_ms=latency,
            )

        except Exception as e:
            return HealthCheck(
                component="bittensor",
                status=ComponentHealth.ERROR,
                message=f"Connection error: {e}",
                latency_ms=(time.time() - start) * 1000,
            )

    async def _check_metagraph(self) -> HealthCheck:
        """Check metagraph sync status"""
        if self.validator.metagraph is None:
            return HealthCheck(
                component="metagraph",
                status=ComponentHealth.ERROR,
                message="Metagraph not initialized",
            )

        try:
            n = self.validator.metagraph.n.item()

            if n <= 0:
                return HealthCheck(
                    component="metagraph",
                    status=ComponentHealth.WARNING,
                    message="Metagraph empty",
                )

            return HealthCheck(
                component="metagraph",
                status=ComponentHealth.OK,
                message=f"Synced, {n} workers",
            )

        except Exception as e:
            return HealthCheck(
                component="metagraph",
                status=ComponentHealth.ERROR,
                message=f"Sync error: {e}",
            )

    def _check_memory(self) -> HealthCheck:
        """Check memory usage"""
        try:
            import psutil

            process = psutil.Process()
            memory_mb = process.memory_info().rss / 1024 / 1024
            memory_percent = process.memory_percent()

            if memory_percent > 90:
                return HealthCheck(
                    component="memory",
                    status=ComponentHealth.ERROR,
                    message=f"Critical: {memory_mb:.0f}MB ({memory_percent:.1f}%)",
                )
            elif memory_percent > 75:
                return HealthCheck(
                    component="memory",
                    status=ComponentHealth.WARNING,
                    message=f"High: {memory_mb:.0f}MB ({memory_percent:.1f}%)",
                )

            return HealthCheck(
                component="memory",
                status=ComponentHealth.OK,
                message=f"{memory_mb:.0f}MB ({memory_percent:.1f}%)",
            )

        except ImportError:
            return HealthCheck(
                component="memory",
                status=ComponentHealth.UNKNOWN,
                message="psutil not available",
            )
        except Exception as e:
            return HealthCheck(
                component="memory",
                status=ComponentHealth.ERROR,
                message=f"Check failed: {e}",
            )

    def _check_task_processing(self) -> HealthCheck:
        """Check task processing health"""
        pending = len(getattr(self.validator, "pending_tasks", {}))
        results = len(getattr(self.validator, "task_results", {}))

        if pending > 1000:
            return HealthCheck(
                component="tasks",
                status=ComponentHealth.WARNING,
                message=f"High pending: {pending} pending, {results} completed",
            )

        return HealthCheck(
            component="tasks",
            status=ComponentHealth.OK,
            message=f"{pending} pending, {results} completed",
        )

    def _check_weight_setting(self) -> HealthCheck:
        """Check weight setting recency"""
        last_block = getattr(self.validator, "last_weight_block", 0)

        if self.validator.subtensor is None:
            return HealthCheck(
                component="weights",
                status=ComponentHealth.UNKNOWN,
                message="Subtensor not available",
            )

        try:
            current_block = self.validator.subtensor.block
            blocks_since = current_block - last_block

            if last_block == 0:
                return HealthCheck(
                    component="weights",
                    status=ComponentHealth.WARNING,
                    message="No weights set yet",
                )

            if blocks_since > 500:
                return HealthCheck(
                    component="weights",
                    status=ComponentHealth.WARNING,
                    message=f"Stale: {blocks_since} blocks since last set",
                )

            return HealthCheck(
                component="weights",
                status=ComponentHealth.OK,
                message=f"Set at block {last_block} ({blocks_since} blocks ago)",
            )

        except Exception as e:
            return HealthCheck(
                component="weights",
                status=ComponentHealth.ERROR,
                message=f"Check failed: {e}",
            )

    def _determine_status(self, checks: List[HealthCheck]) -> HealthStatus:
        """Determine overall health status from individual checks"""
        error_count = sum(1 for c in checks if c.status == ComponentHealth.ERROR)
        warning_count = sum(1 for c in checks if c.status == ComponentHealth.WARNING)

        if error_count >= 2:
            return HealthStatus.CRITICAL
        elif error_count == 1:
            return HealthStatus.UNHEALTHY
        elif warning_count >= 2:
            return HealthStatus.DEGRADED
        elif warning_count == 1:
            return HealthStatus.DEGRADED

        return HealthStatus.HEALTHY

    def get_status(self) -> HealthStatus:
        """Get current health status"""
        if self.consecutive_failures >= CRITICAL_THRESHOLD_FAILURES:
            return HealthStatus.CRITICAL
        elif self.consecutive_failures >= UNHEALTHY_THRESHOLD_FAILURES:
            return HealthStatus.UNHEALTHY
        elif self.consecutive_failures > 0:
            return HealthStatus.DEGRADED
        return HealthStatus.HEALTHY


# =============================================================================
