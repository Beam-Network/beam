"""
Worker Manager - Worker lifecycle, verification, and health monitoring.
"""

import asyncio
import hashlib
import logging
import os
import time
from collections import defaultdict
from datetime import datetime, timedelta
from typing import Any, Dict, List, Optional, Set

import aiohttp

from .config import OrchestratorSettings


def sha256(data: bytes) -> str:
    """Compute SHA256 hash, return hex string."""
    if isinstance(data, str):
        data = data.encode()
    return hashlib.sha256(data).hexdigest()

logger = logging.getLogger(__name__)


class WorkerManager:
    """Manages worker registration, verification, health checks, and WebSocket connections."""

    def __init__(self, settings: OrchestratorSettings, subnet_core_client_ref=None):
        self.settings = settings
        # Callable that returns subnet_core_client (to avoid circular refs)
        self._get_subnet_core_client = subnet_core_client_ref or (lambda: None)

        # Worker registry (local cache, SubnetCore is source of truth)
        self.workers: Dict[str, Any] = {}  # worker_id -> Worker
        self.workers_by_hotkey: Dict[str, str] = {}  # hotkey -> worker_id
        self.workers_by_region: Dict[str, Set[str]] = defaultdict(set)

        # WebSocket connections
        self.worker_connections: Dict[str, Any] = {}  # worker_id -> WebSocket

    async def register_worker(
        self,
        hotkey: str,
        ip: str,
        port: int,
        region: str,
        bandwidth_mbps: float = 0.0,
        subnet_core_client=None,
    ) -> Optional[Any]:
        """Register a new worker with the Orchestrator.

        Registration goes through SubnetCore first to validate the worker
        identity. Only workers known to SubnetCore are accepted.
        """
        from .orchestrator import Worker, WorkerStatus

        # Check if already registered
        if hotkey in self.workers_by_hotkey:
            existing_id = self.workers_by_hotkey[hotkey]
            existing_worker = self.workers.get(existing_id)
            if existing_worker:
                is_local = existing_worker.ip in ("127.0.0.1", "localhost", "::1") or existing_worker.region == "local"
                if is_local and existing_worker.status == WorkerStatus.OFFLINE:
                    existing_worker.status = WorkerStatus.ACTIVE
                    existing_worker.last_seen = datetime.utcnow()
                    logger.info(f"Worker {existing_id} reactivated (local development mode)")
                else:
                    logger.warning(f"Worker with hotkey {hotkey[:16]}... already registered")
                return existing_worker
            return None

        # Check capacity
        if len(self.workers) >= self.settings.max_workers:
            logger.warning("Max workers limit reached")
            return None

        # Register with SubnetCore to get the canonical worker_id
        # SubnetCore is the source of truth — it issued the worker_id when
        # the worker first called POST /workers/register.
        if subnet_core_client:
            try:
                from clients.subnet_core_client import WorkerRegistration
                registration = WorkerRegistration(
                    hotkey=hotkey,
                    ip=ip,
                    port=port,
                    region=region,
                )
                result = await subnet_core_client.register_worker(registration)
                worker_id = result.get("worker_id")
                if not worker_id:
                    logger.error(
                        f"SubnetCore did not return worker_id for {hotkey[:16]}..."
                    )
                    return None
                logger.info(f"SubnetCore resolved worker_id for {hotkey[:16]}...: {worker_id[:16]}...")
            except Exception as e:
                logger.error(
                    f"SubnetCore rejected worker registration for {hotkey[:16]}...: {e}"
                )
                return None
        else:
            logger.error(
                f"Cannot register worker {hotkey[:16]}...: SubnetCore client not available"
            )
            return None

        # Create local worker state (SubnetCore validated)
        worker = Worker(
            worker_id=worker_id,
            hotkey=hotkey,
            ip=ip,
            port=port,
            region=region,
            bandwidth_mbps=bandwidth_mbps,
            status=WorkerStatus.PENDING,
        )

        # Store worker
        self.workers[worker_id] = worker
        self.workers_by_hotkey[hotkey] = worker_id
        self.workers_by_region[region].add(worker_id)

        logger.info(
            f"Worker registered: {worker_id} "
            f"(hotkey: {hotkey[:16]}..., region: {region})"
        )

        # Schedule verification task
        asyncio.create_task(self._verify_new_worker(worker_id))

        return worker

    async def _verify_new_worker(self, worker_id: str) -> None:
        """Verify a newly registered worker."""
        from .orchestrator import WorkerStatus

        worker = self.workers.get(worker_id)
        if not worker:
            return

        # Skip verification for local workers
        if worker.ip in ("127.0.0.1", "localhost", "::1") or worker.region == "local":
            worker.status = WorkerStatus.ACTIVE
            worker.trust_score = 0.5
            logger.info(f"Worker {worker_id} auto-activated (local development mode)")
            return

        verification_results = {
            "connectivity": False,
            "bandwidth": False,
            "geographic": False,
            "sybil_check": False,
        }

        try:
            connectivity_ok = await self._verify_connectivity(worker)
            verification_results["connectivity"] = connectivity_ok

            if not connectivity_ok:
                logger.warning(f"Worker {worker_id} failed connectivity check")
                worker.status = WorkerStatus.SUSPENDED
                return

            bandwidth_ok, measured_bandwidth = await self._verify_bandwidth(worker)
            verification_results["bandwidth"] = bandwidth_ok

            if bandwidth_ok:
                worker.bandwidth_mbps = measured_bandwidth
                worker.bandwidth_ema = measured_bandwidth

            geo_ok = await self._verify_geographic(worker)
            verification_results["geographic"] = geo_ok

            sybil_ok = await self._check_sybil(worker)
            verification_results["sybil_check"] = sybil_ok

            if all(verification_results.values()):
                worker.status = WorkerStatus.ACTIVE
                logger.info(
                    f"Worker {worker_id} verified and activated "
                    f"(bandwidth: {measured_bandwidth:.1f} Mbps)"
                )
            elif verification_results["connectivity"] and verification_results["bandwidth"]:
                worker.status = WorkerStatus.ACTIVE
                issues = [k for k, v in verification_results.items() if not v]
                logger.warning(f"Worker {worker_id} activated with issues: {issues}")
            else:
                worker.status = WorkerStatus.SUSPENDED
                failed = [k for k, v in verification_results.items() if not v]
                logger.warning(f"Worker {worker_id} failed verification: {failed}")

        except Exception as e:
            logger.error(f"Error verifying worker {worker_id}: {e}")
            if worker_id in self.workers:
                worker.status = WorkerStatus.SUSPENDED
                logger.warning(f"Worker {worker_id} suspended due to verification error")

    async def _verify_connectivity(self, worker) -> bool:
        """Verify worker endpoint is reachable."""
        try:
            url = f"http://{worker.ip}:{worker.port}/health"
            async with aiohttp.ClientSession() as session:
                async with session.get(url, timeout=aiohttp.ClientTimeout(total=10)) as response:
                    if response.status == 200:
                        return True
                    logger.debug(f"Worker {worker.worker_id} health check returned {response.status}")
                    return False
        except aiohttp.ClientError as e:
            logger.debug(f"Worker {worker.worker_id} connectivity check failed: {e}")
            return False
        except asyncio.TimeoutError:
            logger.debug(f"Worker {worker.worker_id} connectivity check timed out")
            return False

    async def _verify_bandwidth(self, worker) -> tuple:
        """Verify worker can handle bandwidth by sending test data."""
        try:
            test_size = 1024 * 1024  # 1 MB
            test_data = os.urandom(test_size)
            test_hash = sha256(test_data).hex()

            url = f"http://{worker.ip}:{worker.port}/verify/bandwidth"
            start_time = time.time()

            async with aiohttp.ClientSession() as session:
                async with session.post(
                    url,
                    data=test_data,
                    headers={"Content-Type": "application/octet-stream", "X-Test-Hash": test_hash},
                    timeout=aiohttp.ClientTimeout(total=30),
                ) as response:
                    if response.status != 200:
                        return False, 0.0
                    result = await response.json()
                    end_time = time.time()
                    duration = end_time - start_time

                    if result.get("hash") != test_hash:
                        logger.warning(f"Worker {worker.worker_id} returned wrong hash")
                        return False, 0.0

                    bandwidth_mbps = (test_size * 8) / duration / 1_000_000
                    min_bandwidth = self.settings.min_worker_bandwidth_mbps
                    if bandwidth_mbps < min_bandwidth:
                        logger.warning(
                            f"Worker {worker.worker_id} bandwidth too low: "
                            f"{bandwidth_mbps:.1f} Mbps (min: {min_bandwidth} Mbps)"
                        )
                        return False, bandwidth_mbps
                    return True, bandwidth_mbps

        except aiohttp.ClientError as e:
            logger.debug(f"Worker {worker.worker_id} bandwidth test failed: {e}")
            return False, 0.0
        except asyncio.TimeoutError:
            logger.debug(f"Worker {worker.worker_id} bandwidth test timed out")
            return False, 0.0
        except Exception as e:
            logger.error(f"Worker {worker.worker_id} bandwidth test error: {e}")
            return False, 0.0

    async def _verify_geographic(self, worker) -> bool:
        """Verify worker's claimed geographic location."""
        try:
            url = f"http://ip-api.com/json/{worker.ip}?fields=status,country,regionName,lat,lon"
            async with aiohttp.ClientSession() as session:
                async with session.get(url, timeout=aiohttp.ClientTimeout(total=10)) as response:
                    if response.status != 200:
                        return True
                    data = await response.json()
                    if data.get("status") != "success":
                        return True

                    worker.latitude = data.get("lat", 0.0)
                    worker.longitude = data.get("lon", 0.0)

                    claimed_region = worker.region.upper() if worker.region else ""
                    region_mapping = {
                        "US": ["United States", "USA"],
                        "EU": ["Germany", "France", "Netherlands", "United Kingdom", "Ireland",
                               "Spain", "Italy", "Poland", "Sweden", "Belgium"],
                        "APAC": ["Japan", "Singapore", "Australia", "South Korea", "India",
                                 "Hong Kong", "Taiwan"],
                    }
                    actual_country = data.get("country", "")
                    for region_code, countries in region_mapping.items():
                        if claimed_region == region_code:
                            if actual_country in countries:
                                return True
                            else:
                                logger.warning(
                                    f"Worker {worker.worker_id} claimed {claimed_region} "
                                    f"but IP is in {actual_country}"
                                )
                                return False
                    return True
        except Exception as e:
            logger.warning(f"Geographic verification failed for {worker.worker_id}: {e}")
            return False

    async def _check_sybil(self, worker) -> bool:
        """Check for Sybil attack indicators."""
        ip_count = sum(1 for w in self.workers.values() if w.ip == worker.ip and w.worker_id != worker.worker_id)
        if ip_count > 5:
            logger.warning(f"Worker {worker.worker_id} shares IP with {ip_count} other workers")
            return False

        if worker.hotkey in self.workers_by_hotkey:
            existing = self.workers_by_hotkey[worker.hotkey]
            if existing != worker.worker_id:
                logger.warning(f"Worker {worker.worker_id} hotkey already registered as {existing}")
                return False

        recent_registrations = sum(
            1 for w in self.workers.values()
            if w.ip == worker.ip
            and (datetime.utcnow() - w.registered_at).total_seconds() < 3600
        )
        if recent_registrations > 10:
            logger.warning(f"Worker {worker.worker_id} IP has {recent_registrations} registrations in last hour")
            return False

        return True

    async def deregister_worker(self, worker_id: str) -> bool:
        """Remove a worker from the Orchestrator."""
        worker = self.workers.get(worker_id)
        if not worker:
            return False

        del self.workers[worker_id]
        if worker.hotkey in self.workers_by_hotkey:
            del self.workers_by_hotkey[worker.hotkey]
        if worker_id in self.workers_by_region.get(worker.region, set()):
            self.workers_by_region[worker.region].remove(worker_id)

        logger.info(f"Worker deregistered: {worker_id}")
        return True

    async def worker_heartbeat(
        self,
        worker_id: str,
        bandwidth_mbps: float,
        active_tasks: int,
        bytes_relayed: int = 0,
    ) -> bool:
        """Process heartbeat from a worker."""
        from .orchestrator import WorkerStatus

        worker = self.workers.get(worker_id)
        if not worker:
            return False

        worker.last_seen = datetime.utcnow()
        worker.bandwidth_mbps = bandwidth_mbps
        worker.update_bandwidth_ema(bandwidth_mbps)
        worker.active_tasks = active_tasks

        if bytes_relayed > 0:
            worker.bytes_relayed_total += bytes_relayed
            worker.bytes_relayed_epoch += bytes_relayed

        if worker.status == WorkerStatus.OFFLINE:
            worker.status = WorkerStatus.ACTIVE
            logger.info(f"Worker {worker_id} reconnected")

        return True

    def get_worker(self, worker_id: str) -> Optional[Any]:
        """Get worker by ID."""
        return self.workers.get(worker_id)

    async def get_available_workers(
        self,
        region: Optional[str] = None,
        min_bandwidth: float = 0.0,
    ) -> List[Any]:
        """Get list of available workers from SubnetCore."""
        from .orchestrator import Worker, WorkerStatus

        # Query SubnetCore for workers (source of truth)
        subnet_core_client = self._get_subnet_core_client()
        if subnet_core_client:
            try:
                workers_data = await subnet_core_client.list_workers(
                    status="active",
                    region=region,
                )
                workers_list = workers_data.get("workers", [])
                logger.debug(f"Got {len(workers_list)} workers from SubnetCore")

                # Convert to Worker objects
                result = []
                for w in workers_list:
                    bandwidth = w.get("bandwidth_mbps", 0.0)
                    if bandwidth >= min_bandwidth:
                        worker = Worker(
                            worker_id=w.get("worker_id", ""),
                            hotkey=w.get("hotkey", ""),
                            ip=w.get("ip", ""),
                            port=w.get("port", 0),
                            region=w.get("region", "unknown"),
                            bandwidth_mbps=bandwidth,
                            status=WorkerStatus.ACTIVE,
                            trust_score=w.get("trust_score", 0.5),
                        )
                        result.append(worker)
                return result
            except Exception as e:
                logger.warning(f"Failed to get workers from SubnetCore: {e}")

        # Fallback to local cache if SubnetCore unavailable
        workers = [
            w for w in self.workers.values()
            if w.is_available and w.bandwidth_mbps >= min_bandwidth
        ]
        if region:
            workers = [w for w in workers if w.region == region]
        return workers

    def register_worker_connection(self, worker_id: str, websocket: Any) -> None:
        """Register a worker's WebSocket connection."""
        self.worker_connections[worker_id] = websocket
        logger.info(f"Worker {worker_id} WebSocket registered")

    def unregister_worker_connection(self, worker_id: str) -> None:
        """Unregister a worker's WebSocket connection."""
        if worker_id in self.worker_connections:
            del self.worker_connections[worker_id]
            logger.info(f"Worker {worker_id} WebSocket unregistered")

    async def worker_health_loop(self, running_flag) -> None:
        """Background loop for checking worker health."""
        while running_flag():
            try:
                await asyncio.sleep(30)
                await self._check_worker_health()
            except asyncio.CancelledError:
                break
            except Exception as e:
                logger.error(f"Error in worker health check: {e}")

    async def _check_worker_health(self) -> None:
        """Check health of all workers."""
        from .orchestrator import WorkerStatus

        timeout = timedelta(seconds=self.settings.worker_timeout_seconds)
        now = datetime.utcnow()

        for worker in self.workers.values():
            if worker.status == WorkerStatus.ACTIVE:
                is_local = worker.ip in ("127.0.0.1", "localhost", "::1") or worker.region == "local"
                if is_local:
                    continue
                if now - worker.last_seen > timeout:
                    worker.status = WorkerStatus.OFFLINE
                    logger.warning(f"Worker {worker.worker_id} marked offline (heartbeat timeout)")

    def _generate_worker_id(self, hotkey: str, ip: str, port: int) -> str:
        """Generate unique worker ID."""
        import hashlib
        data = f"{hotkey}:{ip}:{port}:{time.time()}"
        return hashlib.sha256(data.encode()).hexdigest()[:16]

    async def sync_workers_from_subnetcore(self) -> int:
        """
        Sync workers from SubnetCore.

        Discovers affiliated workers from SubnetCore and updates the local cache.
        Returns the number of workers synced.

        SubnetCore returns anonymous worker data (no hotkey/ip/port/region for privacy):
        - worker_id: Worker identifier
        - status: Worker status
        - bandwidth_mbps: Current bandwidth from latest heartbeat
        - success_rate: Task success rate
        - trust_score: Trust score
        - total_tasks: Total tasks completed
        - bytes_relayed_total: Total bytes transferred
        - last_seen: Last heartbeat time
        """
        from .orchestrator import Worker, WorkerStatus

        subnet_core_client = self._get_subnet_core_client()
        if not subnet_core_client:
            logger.warning("Cannot sync workers: SubnetCore client not available")
            return 0

        try:
            workers_data = await subnet_core_client.list_workers(status="active")
            workers_list = workers_data.get("workers", [])

            if not workers_list:
                logger.info("No workers returned from SubnetCore")
                return 0

            synced = 0
            for w in workers_list:
                worker_id = w.get("worker_id", "")
                if not worker_id:
                    continue

                # Update or create worker in local cache
                if worker_id in self.workers:
                    # Update existing worker with performance metrics
                    existing = self.workers[worker_id]
                    existing.bandwidth_mbps = w.get("bandwidth_mbps", existing.bandwidth_mbps)
                    existing.trust_score = w.get("trust_score", existing.trust_score)
                    existing.success_rate = w.get("success_rate", existing.success_rate)
                    existing.total_tasks = w.get("total_tasks", existing.total_tasks)
                    existing.bytes_relayed_total = w.get("bytes_relayed_total", existing.bytes_relayed_total)
                    existing.global_pending_tasks = w.get("pending_tasks", 0)  # Global task count from BeamCore
                    existing.last_seen = datetime.utcnow()
                    if existing.status == WorkerStatus.OFFLINE:
                        existing.status = WorkerStatus.ACTIVE
                else:
                    # Add new worker to cache (anonymous - no hotkey/ip/port/region)
                    worker = Worker(
                        worker_id=worker_id,
                        hotkey="",  # Anonymous - not provided by SubnetCore
                        ip="0.0.0.0",  # Anonymous
                        port=0,  # Anonymous
                        region="unknown",  # Anonymous
                        bandwidth_mbps=w.get("bandwidth_mbps", 0.0),
                        status=WorkerStatus.ACTIVE,
                        trust_score=w.get("trust_score", 0.5),
                        success_rate=w.get("success_rate", 1.0),
                        total_tasks=w.get("total_tasks", 0),
                        bytes_relayed_total=w.get("bytes_relayed_total", 0),
                        global_pending_tasks=w.get("pending_tasks", 0),  # Global task count from BeamCore
                    )
                    self.workers[worker_id] = worker
                    # No hotkey/region indexing since anonymous

                synced += 1

            logger.info(f"Synced {synced} workers from SubnetCore (total in cache: {len(self.workers)})")
            return synced

        except Exception as e:
            logger.error(f"Failed to sync workers from SubnetCore: {e}")
            return 0

    async def worker_sync_loop(self, running_flag, interval_seconds: int = 60) -> None:
        """Background loop for syncing workers from SubnetCore."""
        # Initial sync on startup
        await asyncio.sleep(5)  # Wait for SubnetCore client to initialize
        await self.sync_workers_from_subnetcore()

        while running_flag():
            try:
                await asyncio.sleep(interval_seconds)
                await self.sync_workers_from_subnetcore()
            except asyncio.CancelledError:
                break
            except Exception as e:
                logger.error(f"Error in worker sync loop: {e}")
