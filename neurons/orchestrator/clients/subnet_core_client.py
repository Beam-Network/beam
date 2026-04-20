"""
BeamCore API Client for Orchestrators

Client for orchestrators to publish Proof-of-Bandwidth to the private
BeamCore service.

Uses WebSocket for real-time notifications (transfers, task results, stale tasks).
Falls back to HTTP polling if WebSocket connection fails.
"""

import asyncio
import json
import logging
import time
import uuid
from typing import Optional, List, Dict, Any, Callable
from dataclasses import dataclass, asdict

import httpx
import websockets
from websockets.exceptions import ConnectionClosed

logger = logging.getLogger(__name__)


@dataclass
class PoBSubmission:
    """Proof-of-Bandwidth submission."""

    task_id: str
    epoch: int
    worker_id: str
    worker_hotkey: str
    start_time_us: int
    end_time_us: int
    bytes_relayed: int
    bandwidth_mbps: float
    chunk_hash: str
    worker_signature: str
    orchestrator_signature: str
    canary_proof: Optional[str] = None
    source_region: Optional[str] = None
    dest_region: Optional[str] = None


@dataclass
class WorkerRegistration:
    """Worker registration data.

    Sent to SubnetCore so it can resolve the privacy-preserving worker_id.
    The orchestrator sends the hotkey; SubnetCore returns the worker_id.
    """

    hotkey: str
    ip: str
    port: int
    region: str
    latitude: Optional[float] = None
    longitude: Optional[float] = None
    max_concurrent_tasks: int = 10


@dataclass
class WorkerUpdate:
    """Worker update data."""

    status: Optional[str] = None
    bandwidth_mbps: Optional[float] = None
    bandwidth_ema: Optional[float] = None
    latency_ms: Optional[float] = None
    success_rate: Optional[float] = None
    trust_score: Optional[float] = None
    fraud_score: Optional[float] = None
    bytes_relayed_total: Optional[int] = None
    bytes_relayed_epoch: Optional[int] = None
    total_tasks: Optional[int] = None
    successful_tasks: Optional[int] = None
    failed_tasks: Optional[int] = None
    rewards_earned_epoch: Optional[int] = None
    rewards_earned_total: Optional[int] = None


@dataclass
class WorkerPaymentData:
    """Worker payment record."""

    orchestrator_hotkey: str
    epoch: int
    worker_id: str
    worker_hotkey: str
    bytes_relayed: int
    tasks_completed: int
    amount_earned: int
    # Task linkage - links payment to specific proof-of-bandwidth
    task_id: Optional[str] = None
    # On-chain transfer proof
    tx_hash: Optional[str] = None
    block_number: Optional[int] = None
    # Legacy merkle proof
    merkle_proof: Optional[str] = None
    leaf_index: Optional[int] = None


@dataclass
class EpochPaymentData:
    """Epoch payment summary."""

    epoch: int
    total_distributed: int
    worker_count: int
    total_bytes_relayed: int
    merkle_root: str
    submitted_to_validators: bool = False


@dataclass
class TaskExecutionContext:
    """Execution context for real data transfer - passed to workers."""

    transfer_id: str
    stream_id: str
    gateway_url: str  # REQUIRED - where workers fetch chunks
    destination_url: str  # REQUIRED - where workers send data
    chunk_indices: List[int]
    source_type: str = "http"


@dataclass
class TaskCreate:
    """Task creation data."""

    task_id: str
    worker_id: str
    chunk_size: int
    chunk_hash: str
    deadline_us: int
    source_region: Optional[str] = None
    dest_region: Optional[str] = None
    canary_hex: Optional[str] = None
    canary_offset: Optional[int] = None
    # Execution context - REQUIRED for real data transfer
    execution_context: Optional[TaskExecutionContext] = None


@dataclass
class TaskUpdate:
    """Task update data."""

    status: Optional[str] = None
    started_at: Optional[str] = None
    completed_at: Optional[str] = None
    bytes_relayed: Optional[int] = None
    bandwidth_mbps: Optional[float] = None
    latency_ms: Optional[float] = None


@dataclass
class ReceiverCodeData:
    """Receiver code data for P2P transfers."""

    receiver_code: str
    worker_id: str
    hotkey: str
    ip: str
    port: int
    expires_at: Optional[str] = None


# =============================================================================
# Blind Worker Data Classes
# =============================================================================

@dataclass
class BlindSessionValidation:
    """Result of blind session validation."""

    valid: bool
    blind_worker_id: Optional[str] = None
    trust_score: Optional[float] = None
    tier: Optional[str] = None
    success_rate: Optional[float] = None
    total_tasks: Optional[int] = None
    avg_bandwidth_mbps: Optional[float] = None
    claimed_region: Optional[str] = None


@dataclass
class BlindTrustScore:
    """Trust score for a blind worker."""

    blind_worker_id: str
    trust_score: float
    tier: str
    success_rate: float
    total_tasks: int
    avg_bandwidth_mbps: float
    claimed_region: Optional[str] = None


@dataclass
class BlindTrustReport:
    """Trust report for a blind worker."""

    blind_worker_id: str
    epoch: int
    tasks_completed: int = 0
    tasks_failed: int = 0
    bytes_relayed: int = 0
    avg_bandwidth_mbps: float = 0.0
    avg_latency_ms: float = 0.0


@dataclass
class BlindPaymentRequest:
    """Payment request for a blind worker."""

    blind_worker_id: str
    amount_dtao: int
    epoch: int
    task_id: Optional[str] = None


class SubnetCoreClient:
    """
    Client for communicating with BeamCore via WebSocket and HTTP.

    Orchestrators use this client to:
    - Receive real-time notifications via WebSocket (transfers, task results)
    - Submit PoB and task updates via HTTP

    WebSocket is the primary communication method. HTTP polling is used as fallback.
    """

    def __init__(
        self,
        base_url: str,
        orchestrator_hotkey: str,
        orchestrator_uid: int,
        timeout: float = 30.0,
        signer=None,
    ):
        """
        Initialize the client.

        Args:
            base_url: Base URL of BeamCore (e.g., http://localhost:8080)
            orchestrator_hotkey: This orchestrator's hotkey for authentication
            orchestrator_uid: This orchestrator's UID
            timeout: Request timeout in seconds
            signer: Optional bittensor wallet hotkey with .sign() method
        """
        self.base_url = base_url.rstrip("/")
        self.orchestrator_hotkey = orchestrator_hotkey
        self.orchestrator_uid = orchestrator_uid
        self.timeout = timeout
        self.signer = signer
        self._client: Optional[httpx.AsyncClient] = None

        # Polling state (used as fallback when WebSocket unavailable)
        self._last_event_id: int = 0  # Track last seen event for polling
        self._task_completion_handler: Optional[Callable] = None
        self._transfer_handler: Optional[Callable] = None
        self._stale_task_handler: Optional[Callable] = None  # Handler for stale tasks
        self._stats_provider: Optional[Callable] = None  # Returns heartbeat stats
        self._worker_update_handler: Optional[Callable] = None  # Handler for worker connect/disconnect push events
        self._polling_tasks: List[asyncio.Task] = []
        self._running = False

        # WebSocket state
        self._ws: Optional[websockets.WebSocketClientProtocol] = None
        self._ws_connected = False
        self._ws_task: Optional[asyncio.Task] = None
        self._ws_heartbeat_task: Optional[asyncio.Task] = None
        self._reconnect_delay = 5.0  # Seconds between reconnection attempts
        self._max_reconnect_delay = 60.0  # Max backoff delay

        # WebSocket registration state
        self._registered = False
        self._registration_config: Optional[Dict[str, Any]] = None

        # API key authentication (for buffer service)
        self._api_key: Optional[str] = None
        self._api_key_expires: Optional[float] = None

    # =========================================================================
    # Handlers for polling notifications
    # =========================================================================

    def set_task_completion_handler(self, handler: Callable):
        """
        Set handler for task completion notifications.

        Handler signature: async def handler(task_completion: dict) -> bool
        Returns True if task is verified and should be acknowledged.
        """
        self._task_completion_handler = handler

    def set_transfer_handler(self, handler: Callable):
        """
        Set handler for new transfer notifications.

        Handler signature: async def handler(transfer: dict) -> None
        """
        self._transfer_handler = handler

    def set_stale_task_handler(self, handler: Callable):
        """
        Set handler for stale task notifications.

        Handler signature: async def handler(stale_task: dict) -> None
        """
        self._stale_task_handler = handler

    def set_stats_provider(self, provider: Callable):
        """
        Set provider for heartbeat stats.

        Provider signature: def provider() -> dict
        Returns dict with keys:
            - current_workers: int
            - avg_bandwidth_mbps: float
            - total_bytes_relayed: int
            - fee_percentage: float
            - balance_tao: float
            - coldkey_balance_tao: float
            - pending_payments: int
        """
        self._stats_provider = provider

    def set_worker_update_handler(self, handler: Callable):
        """
        Set handler for worker connect/disconnect push events.

        Handler signature: async def handler(worker_id: str, event: str) -> None
        Where event is "connected" or "disconnected".
        """
        self._worker_update_handler = handler

    # =========================================================================
    # WebSocket Connection (Primary) + HTTP Polling (Fallback)
    # =========================================================================

    def _get_ws_url(self) -> str:
        """Get WebSocket URL from base URL."""
        # Convert http(s)://host to ws(s)://host
        ws_url = self.base_url.replace("https://", "wss://").replace("http://", "ws://")
        return f"{ws_url}/ws/orchestrators/{self.orchestrator_hotkey}"

    def _sign_ws_auth(self) -> tuple[str, str]:
        """Generate WebSocket authentication signature."""
        timestamp = str(int(time.time()))
        message = f"{self.orchestrator_hotkey}:{timestamp}"
        signature = ""
        if self.signer:
            try:
                sig_bytes = self.signer.sign(message.encode("utf-8"))
                signature = "0x" + (sig_bytes.hex() if isinstance(sig_bytes, bytes) else str(sig_bytes))
            except Exception as e:
                logger.warning(f"Failed to sign WebSocket auth: {e}")
                signature = "unsigned"
        else:
            signature = "unsigned"
        return signature, timestamp

    async def _ensure_api_key(self) -> Optional[str]:
        """
        Ensure we have a valid API key for WebSocket authentication.

        First checks BEAMCORE_API_KEY env var, then uses challenge/verify flow.
        The key is cached and reused until it expires.

        Returns:
            API key string (b1m_xxx format) or None if auth fails
        """
        import os

        # Check if we have a valid cached key
        if self._api_key and self._api_key_expires:
            if time.time() < self._api_key_expires - 60:  # 1 min buffer
                return self._api_key

        # Check for API key in environment variable
        env_api_key = os.environ.get("BEAMCORE_API_KEY")
        if env_api_key and env_api_key.startswith("b1m_"):
            self._api_key = env_api_key
            self._api_key_expires = time.time() + 86400 * 365  # Never expires
            logger.info(f"Using BEAMCORE_API_KEY from environment for {self.orchestrator_hotkey[:16]}...")
            return self._api_key

        if not self.signer:
            logger.error("Cannot get API key: no signer configured and BEAMCORE_API_KEY not set")
            return None

        client = await self._get_client()

        try:
            # Step 1: Request challenge
            challenge_resp = await client.post(
                f"{self.base_url}/auth/challenge",
                json={
                    "hotkey": self.orchestrator_hotkey,
                    "role": "orchestrator",
                },
            )

            if challenge_resp.status_code != 200:
                logger.error(f"Failed to get auth challenge: {challenge_resp.status_code}")
                return None

            challenge_data = challenge_resp.json()
            challenge_id = challenge_data["challenge_id"]
            message = challenge_data["message"]

            # Step 2: Sign the challenge message
            try:
                sig_bytes = self.signer.sign(message.encode("utf-8"))
                signature = "0x" + (sig_bytes.hex() if isinstance(sig_bytes, bytes) else str(sig_bytes))
            except Exception as e:
                logger.error(f"Failed to sign challenge: {e}")
                return None

            # Step 3: Verify signature and get API key
            verify_resp = await client.post(
                f"{self.base_url}/auth/verify",
                json={
                    "challenge_id": challenge_id,
                    "hotkey": self.orchestrator_hotkey,
                    "signature": signature,
                    "key_name": "Orchestrator WebSocket Key",
                },
            )

            if verify_resp.status_code == 409:
                logger.error(
                    "API key already exists for this orchestrator. "
                    "Set BEAMCORE_API_KEY env var with your existing key, or revoke the old key first."
                )
                return None

            if verify_resp.status_code != 200:
                logger.error(f"Failed to verify signature: {verify_resp.status_code} - {verify_resp.text}")
                return None

            verify_data = verify_resp.json()

            if not verify_data.get("success") or not verify_data.get("api_key"):
                logger.error(f"Auth verify failed: {verify_data.get('message', 'Unknown error')}")
                return None

            self._api_key = verify_data["api_key"]
            # Default to 24h expiry if not specified
            self._api_key_expires = time.time() + 86400

            logger.info(f"Obtained API key for orchestrator {self.orchestrator_hotkey[:16]}...")
            logger.info(f"Save this key as BEAMCORE_API_KEY={self._api_key}")
            return self._api_key

        except Exception as e:
            logger.error(f"Failed to get API key: {e}")
            return None

    async def start_polling(
        self,
        heartbeat_interval: float = 30.0,
        transfer_poll_interval: float = 11.0,
        results_poll_interval: float = 5.0,
    ):
        """
        Start WebSocket connection for real-time notifications.

        WebSocket replaces HTTP polling when available.
        HTTP transfer polling is still kept as a fallback for local or
        degraded environments where the buffer/WebSocket path is unavailable.

        Args:
            heartbeat_interval: Seconds between heartbeats (default 30s)
            transfer_poll_interval: Seconds between HTTP fallback polls when WebSocket is unavailable
            results_poll_interval: Unused (WebSocket provides real-time updates)
        """
        if self._running:
            logger.warning("Already running")
            return

        self._running = True

        # Start WebSocket connection (handles transfers, results, stale tasks)
        self._ws_task = asyncio.create_task(self._ws_connection_loop())

        # Start HTTP fallback transfer polling. The loop is dormant while the
        # WebSocket is connected, and becomes active only when WS is unavailable.
        safe_transfer_poll_interval = max(transfer_poll_interval, 11.0)
        self._polling_tasks.append(
            asyncio.create_task(self._transfer_poll_loop(safe_transfer_poll_interval))
        )

        # Start heartbeat loop (still uses HTTP)
        self._ws_heartbeat_task = asyncio.create_task(self._heartbeat_loop(heartbeat_interval))

        logger.info(
            f"Started WebSocket connection to {self._get_ws_url()}, "
            f"heartbeat={heartbeat_interval}s, fallback_transfer_poll={safe_transfer_poll_interval}s"
        )

    async def stop_polling(self):
        """Stop WebSocket connection and heartbeat loop."""
        self._running = False
        self._ws_connected = False

        # Close WebSocket
        if self._ws:
            try:
                await self._ws.close()
            except Exception:
                pass
            self._ws = None

        # Cancel tasks
        for task in [self._ws_task, self._ws_heartbeat_task]:
            if task:
                task.cancel()
                try:
                    await task
                except asyncio.CancelledError:
                    pass

        self._ws_task = None
        self._ws_heartbeat_task = None
        logger.info("WebSocket connection stopped")

    async def _ws_connection_loop(self):
        """Maintain WebSocket connection with automatic reconnection."""
        reconnect_delay = self._reconnect_delay

        while self._running:
            try:
                await self._connect_websocket()
                reconnect_delay = self._reconnect_delay  # Reset on successful connection
                await self._ws_message_loop()
            except ConnectionClosed as e:
                logger.warning(f"WebSocket closed: {e}")
            except Exception as e:
                logger.error(f"WebSocket error: {e}")

            self._ws_connected = False
            self._ws = None

            if self._running:
                logger.info(f"Reconnecting WebSocket in {reconnect_delay}s...")
                await asyncio.sleep(reconnect_delay)
                reconnect_delay = min(reconnect_delay * 1.5, self._max_reconnect_delay)

    def set_registration_config(
        self,
        url: str,
        region: str,
        max_workers: int = 10000,
        uid: int = None,
        fee_percentage: float = 0.0,
    ):
        """
        Set registration config for auto-registration after WebSocket connects.

        This should be called before start_polling() so the orchestrator
        registers via WebSocket immediately after connection.

        Args:
            url: Orchestrator's API URL (e.g., http://ip:port)
            region: Geographic region
            max_workers: Maximum workers this orchestrator can handle
            uid: Bittensor UID (optional)
            fee_percentage: Fee percentage charged to workers
        """
        self._registration_config = {
            "url": url,
            "region": region,
            "max_workers": max_workers,
            "uid": uid,
            "fee_percentage": fee_percentage,
        }
        logger.info(f"Registration config set: region={region}, max_workers={max_workers}")

    async def _connect_websocket(self):
        """Connect to WebSocket endpoint."""
        # Get API key for authentication (required by buffer service)
        api_key = await self._ensure_api_key()
        if not api_key:
            logger.error("Failed to obtain API key for WebSocket connection")
            raise ConnectionError("Cannot connect without API key")

        signature, timestamp = self._sign_ws_auth()
        url = self._get_ws_url()

        headers = {
            "x-api-key": api_key,
            "x-signature": signature,
            "x-timestamp": timestamp,
        }

        logger.info(f"Connecting to WebSocket: {url}")
        self._ws = await websockets.connect(
            url,
            additional_headers=headers,
            ping_interval=30,
            ping_timeout=10,
        )
        self._ws_connected = True
        self._registered = False  # Reset on new connection
        logger.info(f"WebSocket connected to {url}")

        # Auto-register if config is set
        if self._registration_config:
            await self.register_via_websocket(
                url=self._registration_config["url"],
                region=self._registration_config["region"],
                max_workers=self._registration_config["max_workers"],
                uid=self._registration_config["uid"],
                fee_percentage=self._registration_config["fee_percentage"],
            )

    async def _ws_message_loop(self):
        """Process incoming WebSocket messages."""
        while self._running and self._ws:
            try:
                message = await self._ws.recv()
                data = json.loads(message)
                await self._handle_ws_message(data)
            except json.JSONDecodeError as e:
                logger.warning(f"Invalid JSON from WebSocket: {e}")

    async def _handle_ws_message(self, data: dict):
        """Handle incoming WebSocket message."""
        msg_type = data.get("type")

        if msg_type == "connected":
            logger.info(f"WebSocket connected to buffer: {data.get('buffer_id')}")

        elif msg_type == "transfer_assignment":
            # New transfer to process (single assignment)
            logger.info(f"Received transfer assignment: {data.get('transfer_id')}")
            if self._transfer_handler:
                try:
                    await self._transfer_handler(data)
                except Exception as e:
                    logger.error(f"Error handling transfer: {e}")

        elif msg_type == "transfer_assignments":
            # Batch of transfer assignments (multiple src×dest combos)
            assignments = data.get("assignments", [])
            logger.info(f"Received {len(assignments)} batched transfer assignments")
            if self._transfer_handler:
                for assignment in assignments:
                    try:
                        await self._transfer_handler(assignment)
                    except Exception as e:
                        logger.error(f"Error handling batched transfer: {e}")

        elif msg_type == "task_result":
            # Task completion notification
            logger.info(f"Received task result: {data.get('task_id')}")
            if self._task_completion_handler:
                try:
                    verified = await self._task_completion_handler(data)
                    if verified:
                        task_id = data.get("task_id")
                        if task_id:
                            await self.acknowledge_task_completions([task_id])
                except Exception as e:
                    logger.error(f"Error handling task result: {e}")

        elif msg_type == "stale_task":
            # Stale task notification
            logger.info(f"Received stale task: {data.get('task_id')}")
            if self._stale_task_handler:
                try:
                    await self._stale_task_handler(data)
                except Exception as e:
                    logger.error(f"Error handling stale task: {e}")

        elif msg_type == "worker_update":
            # Worker connect/disconnect push event
            worker_id = data.get("worker_id")
            event = data.get("event")
            logger.debug(f"Worker update: {worker_id} - {event}")
            if self._worker_update_handler and worker_id and event:
                try:
                    await self._worker_update_handler(worker_id, event)
                except Exception as e:
                    logger.error(f"Error handling worker_update: {e}")

        elif msg_type == "heartbeat_ack":
            logger.debug("WebSocket heartbeat acknowledged")

        elif msg_type == "register_ack":
            logger.info(f"Registration acknowledged: {data.get('status')}")
            self._registered = True

        elif msg_type == "register_result":
            status = data.get("status")
            slot = data.get("slot_number")
            logger.info(f"Registration result: status={status}, slot={slot}")
            self._registered = status in ("assigned", "updated")

        elif msg_type == "register_error":
            logger.error(f"Registration failed: {data.get('error')}")
            self._registered = False

        elif msg_type == "ping":
            # Respond to server ping
            if self._ws:
                await self._ws.send(json.dumps({"type": "pong"}))

        else:
            logger.debug(f"Unknown WebSocket message type: {msg_type}")

    async def _send_ws_heartbeat(self):
        """Send heartbeat over WebSocket with full stats."""
        if not self._ws or not self._ws_connected:
            return

        stats = {}
        if self._stats_provider:
            try:
                stats = self._stats_provider()
            except Exception as e:
                logger.warning(f"Stats provider error: {e}")

        message = {
            "type": "heartbeat",
            "worker_count": stats.get("current_workers", 0),
            "avg_bandwidth_mbps": stats.get("avg_bandwidth_mbps", 0.0),
            "total_bytes_relayed": stats.get("total_bytes_relayed", 0),
            "fee_percentage": stats.get("fee_percentage", 0.0),
            "balance_tao": stats.get("balance_tao", -1.0),
            "coldkey_balance_tao": stats.get("coldkey_balance_tao", -1.0),
            "pending_payments": stats.get("pending_payments", 0),
        }

        try:
            await self._ws.send(json.dumps(message))
        except Exception as e:
            logger.warning(f"Failed to send WebSocket heartbeat: {e}")

    async def register_via_websocket(
        self,
        url: str,
        region: str,
        max_workers: int = 10000,
        uid: int = None,
        fee_percentage: float = 0.0,
    ) -> bool:
        """
        Register orchestrator via WebSocket.

        Sends a register message over the WebSocket connection instead of HTTP POST.
        The signature proves ownership of the hotkey.

        Args:
            url: Orchestrator's API URL (e.g., http://ip:port)
            region: Geographic region
            max_workers: Maximum workers this orchestrator can handle
            uid: Bittensor UID (optional)
            fee_percentage: Fee percentage charged to workers

        Returns:
            True if registration message was sent successfully
        """
        if not self._ws or not self._ws_connected:
            logger.warning("Cannot register via WebSocket: not connected")
            return False

        # Sign registration data: "{hotkey}:{url}:{region}"
        reg_message = f"{self.orchestrator_hotkey}:{url}:{region}"
        signature = ""
        if self.signer:
            try:
                signature = self.signer.sign(reg_message.encode()).hex()
            except Exception as e:
                logger.warning(f"Failed to sign registration: {e}")

        message = {
            "type": "register",
            "url": url,
            "region": region,
            "max_workers": max_workers,
            "uid": uid,
            "fee_percentage": fee_percentage,
            "signature": signature,
        }

        try:
            await self._ws.send(json.dumps(message))
            logger.info(f"Sent registration via WebSocket: region={region}, fee={fee_percentage}%")
            return True
        except Exception as e:
            logger.error(f"Failed to send registration via WebSocket: {e}")
            return False

    async def _heartbeat_loop(self, interval: float):
        """Send periodic heartbeats via WebSocket."""
        while self._running:
            try:
                if self._ws_connected:
                    await self._send_ws_heartbeat()
                    logger.debug("WebSocket heartbeat sent")
                else:
                    logger.debug("Skipping heartbeat: WebSocket not connected")
            except Exception as e:
                logger.warning(f"Heartbeat failed: {e}")

            await asyncio.sleep(interval)

    # Legacy HTTP polling methods (kept for fallback/compatibility)

    async def _transfer_poll_loop(self, interval: float):
        """Poll for pending transfers (fallback if WebSocket unavailable)."""
        while self._running:
            if not self._ws_connected:
                try:
                    transfers = await self.get_pending_transfers()
                    for transfer in transfers:
                        if self._transfer_handler:
                            try:
                                await self._transfer_handler(transfer)
                            except Exception as e:
                                logger.error(f"Error handling transfer: {e}")
                except Exception as e:
                    logger.warning(f"Transfer poll failed: {e}")

            await asyncio.sleep(interval)

    async def _results_poll_loop(self, interval: float):
        """Poll for pending task results (fallback if WebSocket unavailable)."""
        while self._running:
            if not self._ws_connected:
                try:
                    results = await self.get_pending_task_results()
                    for result in results:
                        if self._task_completion_handler:
                            try:
                                verified = await self._task_completion_handler(result)
                                if verified:
                                    task_id = result.get("task_id")
                                    if task_id:
                                        await self.acknowledge_task_completions([task_id])
                            except Exception as e:
                                logger.error(f"Error handling task result: {e}")
                except Exception as e:
                    logger.warning(f"Task results poll failed: {e}")

            await asyncio.sleep(interval)

    # =========================================================================
    # HTTP Endpoints
    # =========================================================================

    async def send_heartbeat(self) -> Dict[str, Any]:
        """
        Send heartbeat to SubnetCore via HTTP.

        DEPRECATED: Use WebSocket heartbeat instead. This endpoint is deprecated
        and will be removed after 2026-04-15. Heartbeats are now sent automatically
        via WebSocket when connected.

        POST /orchestrators/heartbeat
        """
        import warnings
        warnings.warn(
            "send_heartbeat() is deprecated. Heartbeats are now sent via WebSocket.",
            DeprecationWarning,
            stacklevel=2,
        )
        client = await self._get_client()

        timestamp = int(time.time())

        # Sign the heartbeat message: "hotkey:timestamp"
        message = f"{self.orchestrator_hotkey}:{timestamp}"
        signature = ""
        if self.signer:
            try:
                sig_bytes = self.signer.sign(message.encode("utf-8"))
                signature = sig_bytes.hex() if isinstance(sig_bytes, bytes) else str(sig_bytes)
            except Exception as e:
                logger.warning(f"Failed to sign heartbeat message: {e}")
                signature = "unsigned"
        else:
            signature = "unsigned"

        # Get stats from provider if available
        stats = {}
        if self._stats_provider:
            try:
                stats = self._stats_provider()
            except Exception as e:
                logger.warning(f"Failed to get heartbeat stats: {e}")

        # Build heartbeat body
        body = {
            "hotkey": self.orchestrator_hotkey,
            "current_workers": stats.get("current_workers", 0),
            "signature": signature,
            "avg_bandwidth_mbps": stats.get("avg_bandwidth_mbps", 0.0),
            "total_bytes_relayed": stats.get("total_bytes_relayed", 0),
            "fee_percentage": stats.get("fee_percentage", 0.0),
            "balance_tao": stats.get("balance_tao", -1.0),
            "coldkey_balance_tao": stats.get("coldkey_balance_tao", -1.0),
            "pending_payments": stats.get("pending_payments", 0),
        }

        try:
            response = await client.post(
                f"{self.base_url}/orchestrators/heartbeat",
                json=body,
            )
            response.raise_for_status()
            return response.json()

        except httpx.HTTPStatusError as e:
            logger.error(f"HTTP error sending heartbeat: {e.response.status_code}")
            raise
        except Exception as e:
            logger.error(f"Error sending heartbeat: {e}")
            raise

    async def set_ready(self, ready: bool) -> bool:
        """
        Toggle this orchestrator's readiness to receive transfers on BeamCore.

        PATCH /orchestrators/ready
        Returns the new ready state as confirmed by BeamCore.
        """
        client = await self._get_client()
        try:
            response = await client.patch(
                f"{self.base_url}/orchestrators/ready",
                json={"ready": ready},
            )
            response.raise_for_status()
            result = response.json()
            confirmed = result.get("ready", ready)
            logger.info(
                f"Orchestrator ready={confirmed} set on BeamCore "
                f"(uid={result.get('uid')})"
            )
            return confirmed
        except httpx.HTTPStatusError as e:
            logger.error(f"HTTP error setting ready flag: {e.response.status_code} - {e.response.text[:200]}")
            raise
        except Exception as e:
            logger.error(f"Error setting ready flag: {e}")
            raise

    async def get_pending_transfers(self) -> List[Dict[str, Any]]:
        """
        Poll for pending transfers.

        GET /orchestrators/pending-transfers
        """
        client = await self._get_client()

        try:
            response = await client.get(
                f"{self.base_url}/orchestrators/pending-transfers",
                params={"since_event_id": self._last_event_id},
            )
            response.raise_for_status()
            data = response.json()

            # Update cursor
            now_event_id = data.get("now_event_id", 0)
            if now_event_id > self._last_event_id:
                self._last_event_id = now_event_id

            return data.get("transfers", [])

        except httpx.HTTPStatusError as e:
            logger.error(f"HTTP error getting pending transfers: {e.response.status_code}")
            return []
        except Exception as e:
            logger.error(f"Error getting pending transfers: {type(e).__name__}: {repr(e)}")
            # Reset client on connection errors to force reconnection
            if isinstance(e, (httpx.ReadError, httpx.ConnectError, httpx.RemoteProtocolError)):
                self._client = None
            return []

    async def post_chunk_assignments(
        self,
        transfer_id: str,
        gateway_url: str,
        object_id: str,
        destination_url: str,
        total_size: int,
        chunk_size: int,
        assignments: List[Dict[str, Any]],
        auth_token: str = None,
        source_urls: Dict[str, str] = None,
        dest_urls: Dict[str, str] = None,
    ) -> Dict[str, Any]:
        """
        POST chunk assignments to SubnetCore.

        POST /orchestrators/assignments

        For S3 transfers, gateway_url/destination_url may be empty and
        source_urls/dest_urls provide per-chunk presigned URLs instead.
        """
        client = await self._get_client()

        body = {
            "transfer_id": transfer_id,
            "gateway_url": gateway_url,
            "object_id": object_id,
            "destination_url": destination_url,
            "total_size": total_size,
            "chunk_size": chunk_size,
            "assignments": assignments,
        }
        if auth_token:
            body["auth_token"] = auth_token
        if source_urls:
            body["source_urls"] = source_urls
        if dest_urls:
            body["dest_urls"] = dest_urls

        try:
            response = await client.post(
                f"{self.base_url}/orchestrators/assignments",
                json=body,
            )
            response.raise_for_status()
            return response.json()

        except httpx.HTTPStatusError as e:
            logger.error(f"HTTP error posting assignments: {e.response.status_code}")
            raise
        except Exception as e:
            logger.error(f"Error posting assignments: {e}")
            raise

    async def create_tasks_batch(self, tasks: List["TaskCreate"]) -> Dict[str, Any]:
        """
        Batch create tasks.

        POST /orchestrators/tasks/batch
        """
        client = await self._get_client()

        try:
            response = await client.post(
                f"{self.base_url}/orchestrators/tasks/batch",
                json=[asdict(t) for t in tasks],
            )
            response.raise_for_status()
            result = response.json()
            logger.info(f"Batch created {len(tasks)} tasks")
            return result

        except httpx.HTTPStatusError as e:
            logger.error(f"HTTP error batch creating tasks: {e.response.status_code}")
            raise
        except Exception as e:
            logger.error(f"Error batch creating tasks: {e}")
            raise

    async def acknowledge_task_completions(
        self,
        task_ids: List[str],
        verified: bool = True,
    ) -> Dict[str, Any]:
        """
        Acknowledge task completions to SubnetCore.

        This triggers PoB creation and payment tracking.

        Args:
            task_ids: List of task IDs to acknowledge
            verified: Whether the orchestrator verified the completions

        Returns:
            Acknowledgment result with counts
        """
        client = await self._get_client()

        try:
            response = await client.post(
                f"{self.base_url}/orchestrators/tasks/acknowledge",
                json={
                    "task_ids": task_ids,
                    "verified": verified,
                },
            )
            response.raise_for_status()
            return response.json()

        except httpx.HTTPStatusError as e:
            logger.error(f"HTTP error acknowledging tasks: {e.response.status_code}")
            raise
        except Exception as e:
            logger.error(f"Error acknowledging tasks: {e}")
            raise

    async def get_pending_task_results(self, limit: int = 100) -> List[Dict[str, Any]]:
        """
        Get pending task results awaiting acknowledgment.

        GET /orchestrators/tasks/pending-results
        """
        client = await self._get_client()

        try:
            response = await client.get(
                f"{self.base_url}/orchestrators/tasks/pending-results",
                params={"limit": limit},
            )
            response.raise_for_status()
            data = response.json()
            return data.get("pending_results", [])

        except httpx.HTTPStatusError as e:
            logger.error(f"HTTP error getting pending results: {e.response.status_code}")
            return []
        except Exception as e:
            logger.error(f"Error getting pending results: {type(e).__name__}: {repr(e)}")
            # Reset client on connection errors to force reconnection
            if isinstance(e, (httpx.ReadError, httpx.ConnectError, httpx.RemoteProtocolError)):
                self._client = None
            return []

    # =========================================================================
    # HTTP Auth & Client
    # =========================================================================

    def _auth_headers(self) -> dict:
        """Build fresh auth headers with current timestamp, nonce, and signature."""
        timestamp = str(int(time.time()))
        nonce = uuid.uuid4().hex[:8]
        action = "request"

        # Build canonical message matching Core API's expected format:
        # "{type}_auth:{hotkey}:{timestamp}:{action}:{nonce}"
        message = f"orchestrator_auth:{self.orchestrator_hotkey}:{timestamp}:{action}:{nonce}"

        # Sign with wallet if available, otherwise use placeholder
        signature = ""
        if self.signer:
            try:
                sig_bytes = self.signer.sign(message.encode("utf-8"))
                signature = sig_bytes.hex() if isinstance(sig_bytes, bytes) else str(sig_bytes)
            except Exception as e:
                logger.warning(f"Failed to sign auth message: {e}")
                signature = "unsigned"
        else:
            signature = "unsigned"

        headers = {
            "X-Hotkey": self.orchestrator_hotkey,  # Required for rate limiting
            "X-Orchestrator-Hotkey": self.orchestrator_hotkey,
            "X-Orchestrator-Uid": str(self.orchestrator_uid),
            "X-Orchestrator-Timestamp": timestamp,
            "X-Orchestrator-Nonce": nonce,
            "X-Orchestrator-Signature": signature,
            "X-Orchestrator-Action": action,
        }

        # Include API key if available (preferred auth method)
        if self._api_key:
            headers["X-Api-Key"] = self._api_key

        return headers

    async def _get_client(self) -> httpx.AsyncClient:
        """Get or create HTTP client with auth headers injected per-request."""
        if self._client is None:
            self._client = httpx.AsyncClient(
                timeout=self.timeout,
                event_hooks={
                    "request": [self._inject_auth_headers],
                },
            )
        return self._client

    async def _inject_auth_headers(self, request: httpx.Request):
        """Inject fresh auth headers into every outgoing request."""
        # Skip API key fetch for auth endpoints (they're public)
        if "/auth/challenge" not in str(request.url) and "/auth/verify" not in str(request.url):
            # Ensure we have an API key for protected endpoints
            if not self._api_key:
                await self._ensure_api_key()

        headers = self._auth_headers()
        for key, value in headers.items():
            request.headers[key] = value

    async def close(self):
        """Close the HTTP client."""
        if self._client:
            await self._client.aclose()
            self._client = None

    # =========================================================================
    # PoB Submission
    # =========================================================================

    async def publish_pob(self, pob: PoBSubmission) -> Dict[str, Any]:
        """
        Publish a single Proof-of-Bandwidth.

        Args:
            pob: PoBSubmission object

        Returns:
            Response dict with status and task_id
        """
        client = await self._get_client()

        try:
            response = await client.post(
                f"{self.base_url}/pob/publish",
                json=asdict(pob),
            )
            response.raise_for_status()
            result = response.json()
            logger.debug(f"Published PoB: task={pob.task_id[:16]}...")
            return result

        except httpx.HTTPStatusError as e:
            logger.error(f"HTTP error publishing PoB: {e.response.status_code} - {e.response.text}")
            raise
        except Exception as e:
            logger.error(f"Error publishing PoB: {e}")
            raise

    async def publish_pob_batch(self, proofs: List[PoBSubmission]) -> Dict[str, Any]:
        """
        Publish multiple proofs in a batch.

        Args:
            proofs: List of PoBSubmission objects

        Returns:
            Response dict with status and count
        """
        client = await self._get_client()

        try:
            response = await client.post(
                f"{self.base_url}/pob/publish/batch",
                json=[asdict(p) for p in proofs],
            )
            response.raise_for_status()
            result = response.json()
            logger.info(f"Batch published: {len(proofs)} proofs")
            return result

        except httpx.HTTPStatusError as e:
            logger.error(f"HTTP error batch publishing: {e.response.status_code}")
            raise
        except Exception as e:
            logger.error(f"Error batch publishing: {e}")
            raise

    # =========================================================================
    # Worker Management
    # =========================================================================

    async def register_worker(self, worker: WorkerRegistration) -> Dict[str, Any]:
        """Register a worker with SubnetCore.

        SubnetCore resolves the hotkey to the privacy-preserving worker_id
        that was issued when the worker first registered.  The returned
        worker_id should be used as the canonical identifier by the
        orchestrator.
        """
        client = await self._get_client()

        try:
            response = await client.post(
                f"{self.base_url}/orchestrators/workers/register",
                json=asdict(worker),
            )
            response.raise_for_status()
            result = response.json()
            worker_id = result.get("worker_id", "?")
            logger.debug(f"Registered worker with SubnetCore: {worker.hotkey[:16]}... -> {worker_id[:16]}...")
            return result

        except httpx.HTTPStatusError as e:
            logger.error(f"HTTP error registering worker: {e.response.status_code}")
            raise
        except Exception as e:
            logger.error(f"Error registering worker: {e}")
            raise

    async def update_worker(self, worker_id: str, update: WorkerUpdate) -> Dict[str, Any]:
        """Update worker data."""
        client = await self._get_client()

        # Filter out None values
        data = {k: v for k, v in asdict(update).items() if v is not None}

        try:
            response = await client.patch(
                f"{self.base_url}/orchestrators/workers/{worker_id}",
                json=data,
            )
            response.raise_for_status()
            return response.json()

        except httpx.HTTPStatusError as e:
            logger.error(f"HTTP error updating worker: {e.response.status_code}")
            raise
        except Exception as e:
            logger.error(f"Error updating worker: {e}")
            raise

    async def list_workers(
        self,
        status: Optional[str] = None,
        region: Optional[str] = None,
        limit: int = 100,
    ) -> Dict[str, Any]:
        """List workers for this orchestrator."""
        client = await self._get_client()

        params = {"limit": limit}
        if status:
            params["status"] = status
        if region:
            params["region"] = region

        try:
            response = await client.get(
                f"{self.base_url}/orchestrators/workers",
                params=params,
            )
            response.raise_for_status()
            return response.json()

        except httpx.HTTPStatusError as e:
            # Extract response body for better error messages (e.g., rate limit details)
            try:
                error_detail = e.response.json().get("detail", {})
                error_msg = error_detail.get("message", str(e))
            except Exception:
                error_msg = e.response.text[:200] if e.response.text else str(e)
            logger.error(f"HTTP error listing workers: {e.response.status_code} - {error_msg}")
            raise
        except Exception as e:
            logger.error(f"Error listing workers: {e}")
            raise

    async def get_worker(self, worker_id: str) -> Dict[str, Any]:
        """Get a specific worker (affiliation-scoped)."""
        client = await self._get_client()

        try:
            response = await client.get(
                f"{self.base_url}/orchestrators/workers/{worker_id}",
            )
            response.raise_for_status()
            return response.json()

        except httpx.HTTPStatusError as e:
            logger.error(f"HTTP error getting worker: {e.response.status_code}")
            raise
        except Exception as e:
            logger.error(f"Error getting worker: {e}")
            raise

    async def get_worker_hotkey(self, worker_id: str) -> Optional[str]:
        """
        Resolve a worker_id to its hotkey regardless of affiliation.

        Uses the unscoped /workers/{id}/hotkey endpoint — unlike get_worker()
        this works for workers completing tasks on behalf of other orchestrators
        (e.g. speculative/recovery tasks assigned cross-orchestrator).
        """
        client = await self._get_client()

        try:
            response = await client.get(
                f"{self.base_url}/orchestrators/workers/{worker_id}/hotkey",
            )
            response.raise_for_status()
            data = response.json()
            return data.get("hotkey")
        except Exception as e:
            logger.debug(f"Hotkey lookup failed for {worker_id[:16]}...: {e}")
            return None

    # =========================================================================
    # Payment Address Resolution
    # =========================================================================

    async def get_task_payment_address(self, task_id: str) -> Dict[str, Any]:
        """
        Get the derived payment address for a completed task from SubnetCore.

        SubnetCore resolves task → worker → payment_pubkey → ephemeral address.
        The orchestrator should transfer TAO to this address, NOT worker.hotkey.

        Returns:
            dict with: address, task_id, worker_id, bytes_relayed, amount_owed, epoch
        """
        client = await self._get_client()

        try:
            response = await client.get(
                f"{self.base_url}/orchestrators/tasks/{task_id}/payment-address",
            )
            response.raise_for_status()
            result = response.json()
            logger.debug(f"Resolved payment address for task {task_id[:16]}...: {result.get('address', '')[:16]}...")
            return result

        except httpx.HTTPStatusError as e:
            logger.error(f"HTTP error getting payment address for task {task_id[:16]}...: {e.response.status_code}")
            raise
        except Exception as e:
            logger.error(f"Error getting payment address for task {task_id[:16]}...: {e}")
            raise

    async def get_paid_task_ids(self) -> List[str]:
        """
        Fetch task IDs already paid by this orchestrator from SubnetCore.
        Used on startup to seed the dedup set and prevent double-payments.
        """
        client = await self._get_client()
        try:
            response = await client.get(
                f"{self.base_url}/orchestrators/payments/paid-task-ids",
            )
            response.raise_for_status()
            result = response.json()
            task_ids = result.get("task_ids", [])
            logger.info(f"Loaded {len(task_ids)} paid task IDs from SubnetCore")
            return task_ids
        except Exception as e:
            logger.warning(f"Could not load paid task IDs from SubnetCore: {e}")
            return []

    # =========================================================================
    # Payment Tracking
    # =========================================================================

    async def record_worker_payment(self, payment: WorkerPaymentData) -> Dict[str, Any]:
        """Record a worker payment for an epoch."""
        client = await self._get_client()

        try:
            response = await client.post(
                f"{self.base_url}/orchestrators/payments/worker",
                json=asdict(payment),
            )
            response.raise_for_status()
            result = response.json()
            logger.debug(f"Recorded payment: worker={payment.worker_id[:16]}..., epoch={payment.epoch}")
            return result

        except httpx.HTTPStatusError as e:
            logger.error(f"HTTP error recording worker payment: {e.response.status_code}")
            raise
        except Exception as e:
            logger.error(f"Error recording worker payment: {e}")
            raise

    async def record_epoch_payment(self, payment: EpochPaymentData) -> Dict[str, Any]:
        """Record epoch payment summary with merkle root."""
        client = await self._get_client()

        try:
            response = await client.post(
                f"{self.base_url}/orchestrators/payments/epoch",
                json=asdict(payment),
            )
            response.raise_for_status()
            result = response.json()
            logger.info(f"Recorded epoch payment: epoch={payment.epoch}, workers={payment.worker_count}")
            return result

        except httpx.HTTPStatusError as e:
            logger.error(f"HTTP error recording epoch payment: {e.response.status_code}")
            raise
        except Exception as e:
            logger.error(f"Error recording epoch payment: {e}")
            raise

    async def get_epoch_payments(self, epoch: int) -> Dict[str, Any]:
        """Get all worker payments for an epoch."""
        client = await self._get_client()

        try:
            response = await client.get(
                f"{self.base_url}/orchestrators/payments/epoch/{epoch}",
            )
            response.raise_for_status()
            return response.json()

        except httpx.HTTPStatusError as e:
            logger.error(f"HTTP error getting epoch payments: {e.response.status_code}")
            raise
        except Exception as e:
            logger.error(f"Error getting epoch payments: {e}")
            raise

    # =========================================================================
    # ALPHA Payment - Transfer Validation & Recording
    # =========================================================================

    async def validate_transfer_for_payment(self, task_id: str) -> Dict[str, Any]:
        """
        Validate that a transfer is eligible for ALPHA payment.

        GET /pob/{task_id}/validate-transfer

        Returns:
            {"valid": true, "transfer_id": "xfer-xxx"} if eligible
            {"valid": false, "error": "..."} if not eligible

        Rejection rules:
        - Transfer status is 'cancelled' or 'failed'
        - Transfer created_at > 60 minutes ago (expired)
        """
        client = await self._get_client()

        try:
            response = await client.get(
                f"{self.base_url}/pob/{task_id}/validate-transfer",
            )
            response.raise_for_status()
            return response.json()

        except httpx.HTTPStatusError as e:
            logger.warning(f"HTTP error validating transfer for task {task_id[:16]}...: {e.response.status_code}")
            return {"valid": False, "error": f"HTTP {e.response.status_code}"}
        except Exception as e:
            logger.warning(f"Error validating transfer for task {task_id[:16]}...: {e}")
            return {"valid": False, "error": str(e)}

    async def record_pob_payment(
        self,
        task_id: str,
        tx_hash: str,
        amount_rao: int,
    ) -> Dict[str, Any]:
        """
        Record ALPHA payment for a PoB after successful on-chain transfer.

        POST /pob/{task_id}/payment

        Args:
            task_id: The task ID linked to the PoB
            tx_hash: On-chain transaction hash (extrinsic_hash:block_hash format)
            amount_rao: Amount paid in RAO (1 ALPHA = 1e9 RAO)

        Returns:
            Response dict with status, task_id, payment_status, message
        """
        client = await self._get_client()

        try:
            response = await client.post(
                f"{self.base_url}/pob/{task_id}/payment",
                json={
                    "tx_hash": tx_hash,
                    "amount_rao": amount_rao,
                },
            )
            response.raise_for_status()
            result = response.json()
            logger.info(f"Recorded ALPHA payment for task {task_id[:16]}...: tx={tx_hash[:24]}...")
            return result

        except httpx.HTTPStatusError as e:
            logger.error(f"HTTP error recording PoB payment: {e.response.status_code} - {e.response.text}")
            raise
        except Exception as e:
            logger.error(f"Error recording PoB payment: {e}")
            raise

    async def backfill_unpaid_tasks(self, max_tasks: int = 100) -> Dict[str, Any]:
        """
        Backfill WorkerPayment records for acknowledged tasks that never got payments.

        This handles cases where the immediate acknowledgment failed or tasks
        were acknowledged before the immediate-ack fix was deployed.

        Args:
            max_tasks: Maximum number of tasks to process per call

        Returns:
            Dict with backfilled_count, skipped_count, errors
        """
        client = await self._get_client()

        try:
            response = await client.post(
                f"{self.base_url}/orchestrators/payments/backfill",
                json={"max_tasks": max_tasks},
            )
            response.raise_for_status()
            result = response.json()
            if result.get("backfilled_count", 0) > 0:
                logger.info(
                    f"Backfilled {result['backfilled_count']} unpaid tasks "
                    f"(skipped {result.get('skipped_count', 0)})"
                )
            return result

        except httpx.HTTPStatusError as e:
            logger.warning(f"HTTP error backfilling tasks: {e.response.status_code}")
            return {"backfilled_count": 0, "skipped_count": 0, "errors": [str(e)]}
        except Exception as e:
            logger.warning(f"Error backfilling tasks: {e}")
            return {"backfilled_count": 0, "skipped_count": 0, "errors": [str(e)]}

    # =========================================================================
    # Task Tracking
    # =========================================================================

    async def create_task(self, task: TaskCreate) -> Dict[str, Any]:
        """Create a new bandwidth task."""
        client = await self._get_client()

        try:
            response = await client.post(
                f"{self.base_url}/orchestrators/tasks",
                json=asdict(task),
            )
            response.raise_for_status()
            result = response.json()
            logger.debug(f"Created task: {task.task_id[:16]}...")
            return result

        except httpx.HTTPStatusError as e:
            logger.error(f"HTTP error creating task: {e.response.status_code}")
            raise
        except Exception as e:
            logger.error(f"Error creating task: {e}")
            raise

    async def update_task(self, task_id: str, update: TaskUpdate) -> Dict[str, Any]:
        """Update task status/results."""
        client = await self._get_client()

        # Filter out None values
        data = {k: v for k, v in asdict(update).items() if v is not None}

        try:
            response = await client.patch(
                f"{self.base_url}/orchestrators/tasks/{task_id}",
                json=data,
            )
            response.raise_for_status()
            return response.json()

        except httpx.HTTPStatusError as e:
            logger.error(f"HTTP error updating task: {e.response.status_code}")
            raise
        except Exception as e:
            logger.error(f"Error updating task: {e}")
            raise

    async def list_tasks(
        self,
        status: Optional[str] = None,
        worker_id: Optional[str] = None,
        limit: int = 100,
    ) -> Dict[str, Any]:
        """List tasks for this orchestrator."""
        client = await self._get_client()

        params = {"limit": limit}
        if status:
            params["status"] = status
        if worker_id:
            params["worker_id"] = worker_id

        try:
            response = await client.get(
                f"{self.base_url}/orchestrators/tasks",
                params=params,
            )
            response.raise_for_status()
            return response.json()

        except httpx.HTTPStatusError as e:
            logger.error(f"HTTP error listing tasks: {e.response.status_code}")
            raise
        except Exception as e:
            logger.error(f"Error listing tasks: {e}")
            raise

    async def get_stale_tasks(self, timeout_seconds: int = 30) -> List[Dict[str, Any]]:
        """
        Get tasks that have become stale (acknowledged but not accepted within timeout).

        GET /orchestrators/tasks/stale?timeout_seconds=30

        Args:
            timeout_seconds: How long since acknowledgment before a task is stale

        Returns:
            List of stale task dicts with task_id, worker_id, etc.
        """
        client = await self._get_client()

        try:
            response = await client.get(
                f"{self.base_url}/orchestrators/tasks/stale",
                params={"timeout_seconds": timeout_seconds},
            )
            response.raise_for_status()
            data = response.json()
            return data.get("stale_tasks", [])

        except httpx.HTTPStatusError as e:
            logger.error(f"HTTP error getting stale tasks: {e.response.status_code}")
            return []
        except Exception as e:
            logger.error(f"Error getting stale tasks: {e}")
            return []

    async def reassign_task(self, task_id: str, new_worker_id: str) -> Dict[str, Any]:
        """
        Reassign a stale task to a new worker.

        POST /orchestrators/tasks/reassign?task_id=...&new_worker_id=...

        Args:
            task_id: The task to reassign
            new_worker_id: The new worker to assign the task to

        Returns:
            Response dict with status
        """
        client = await self._get_client()

        try:
            response = await client.post(
                f"{self.base_url}/orchestrators/tasks/reassign",
                params={
                    "task_id": task_id,
                    "new_worker_id": new_worker_id,
                },
            )
            response.raise_for_status()
            result = response.json()
            logger.info(f"Reassigned task {task_id[:16]}... to worker {new_worker_id[:16]}...")
            return result

        except httpx.HTTPStatusError as e:
            logger.error(f"HTTP error reassigning task: {e.response.status_code}")
            raise
        except Exception as e:
            logger.error(f"Error reassigning task: {e}")
            raise

    # =========================================================================
    # Receiver Codes
    # =========================================================================

    async def create_receiver_code(self, data: ReceiverCodeData) -> Dict[str, Any]:
        """Create a receiver code for P2P transfers."""
        client = await self._get_client()

        try:
            response = await client.post(
                f"{self.base_url}/orchestrators/receiver-codes",
                json=asdict(data),
            )
            response.raise_for_status()
            result = response.json()
            logger.debug(f"Created receiver code: {data.receiver_code}")
            return result

        except httpx.HTTPStatusError as e:
            logger.error(f"HTTP error creating receiver code: {e.response.status_code}")
            raise
        except Exception as e:
            logger.error(f"Error creating receiver code: {e}")
            raise

    async def get_receiver_code(self, code: str) -> Dict[str, Any]:
        """Get receiver code details."""
        client = await self._get_client()

        try:
            response = await client.get(
                f"{self.base_url}/orchestrators/receiver-codes/{code}",
            )
            response.raise_for_status()
            return response.json()

        except httpx.HTTPStatusError as e:
            logger.error(f"HTTP error getting receiver code: {e.response.status_code}")
            raise
        except Exception as e:
            logger.error(f"Error getting receiver code: {e}")
            raise

    # =========================================================================
    # Health Check
    # =========================================================================

    async def health_check(self) -> bool:
        """
        Check if BeamCore is healthy.

        Returns:
            True if healthy, False otherwise
        """
        client = await self._get_client()

        try:
            response = await client.get(f"{self.base_url}/health")
            return response.status_code == 200

        except Exception as e:
            logger.warning(f"Health check failed: {e}")
            return False

    # =========================================================================
    # Blind Worker Session Validation
    # =========================================================================

    async def validate_blind_session(
        self,
        session_token: str,
    ) -> BlindSessionValidation:
        """
        Validate a blind worker's session token.

        Called when a worker connects via WebSocket with a session token.
        Returns worker info (with trust score) if valid.

        Args:
            session_token: The session token presented by the worker

        Returns:
            BlindSessionValidation with worker info if valid
        """
        client = await self._get_client()

        try:
            response = await client.post(
                f"{self.base_url}/workers/blind/session/validate",
                json={"session_token": session_token},
            )
            response.raise_for_status()
            data = response.json()

            return BlindSessionValidation(
                valid=data.get("valid", False),
                blind_worker_id=data.get("blind_worker_id"),
                trust_score=data.get("trust_score"),
                tier=data.get("tier"),
                success_rate=data.get("success_rate"),
                total_tasks=data.get("total_tasks"),
                avg_bandwidth_mbps=data.get("avg_bandwidth_mbps"),
                claimed_region=data.get("claimed_region"),
            )

        except httpx.HTTPStatusError as e:
            logger.error(f"HTTP error validating session: {e.response.status_code}")
            return BlindSessionValidation(valid=False)
        except Exception as e:
            logger.error(f"Error validating session: {e}")
            return BlindSessionValidation(valid=False)

    async def invalidate_blind_session(self, session_token: str) -> bool:
        """
        Invalidate a blind worker's session (on disconnect).

        Args:
            session_token: The session token to invalidate

        Returns:
            True if successful
        """
        client = await self._get_client()

        try:
            response = await client.post(
                f"{self.base_url}/workers/blind/session/invalidate",
                params={"session_token": session_token},
            )
            response.raise_for_status()
            return response.json().get("success", False)

        except Exception as e:
            logger.error(f"Error invalidating session: {e}")
            return False

    # =========================================================================
    # Blind Worker Trust Scores
    # =========================================================================

    async def get_blind_trust_scores(
        self,
        min_trust: float = 0.0,
        region: Optional[str] = None,
    ) -> List[BlindTrustScore]:
        """
        Get trust scores for all workers with active sessions to this orchestrator.

        Called periodically to get updated trust information for task selection.

        Args:
            min_trust: Minimum trust score filter
            region: Region filter

        Returns:
            List of BlindTrustScore objects
        """
        client = await self._get_client()

        params = {}
        if min_trust > 0:
            params["min_trust"] = min_trust
        if region:
            params["region"] = region

        try:
            response = await client.get(
                f"{self.base_url}/trust/scores",
                params=params,
            )
            response.raise_for_status()
            data = response.json()

            return [
                BlindTrustScore(
                    blind_worker_id=w["blind_worker_id"],
                    trust_score=w["trust_score"],
                    tier=w["tier"],
                    success_rate=w["success_rate"],
                    total_tasks=w["total_tasks"],
                    avg_bandwidth_mbps=w["avg_bandwidth_mbps"],
                    claimed_region=w.get("claimed_region"),
                )
                for w in data.get("workers", [])
            ]

        except httpx.HTTPStatusError as e:
            logger.error(f"HTTP error getting trust scores: {e.response.status_code}")
            return []
        except Exception as e:
            logger.error(f"Error getting trust scores: {e}")
            return []

    async def get_blind_worker_trust(self, blind_worker_id: str) -> Optional[BlindTrustScore]:
        """
        Get trust score for a specific blind worker.

        Args:
            blind_worker_id: The worker's blind ID

        Returns:
            BlindTrustScore or None if not found
        """
        client = await self._get_client()

        try:
            response = await client.get(
                f"{self.base_url}/trust/scores/{blind_worker_id}",
            )
            response.raise_for_status()
            w = response.json()

            return BlindTrustScore(
                blind_worker_id=w["blind_worker_id"],
                trust_score=w["trust_score"],
                tier=w["tier"],
                success_rate=w["success_rate"],
                total_tasks=w["total_tasks"],
                avg_bandwidth_mbps=w["avg_bandwidth_mbps"],
                claimed_region=w.get("claimed_region"),
            )

        except httpx.HTTPStatusError as e:
            if e.response.status_code == 404:
                return None
            logger.error(f"HTTP error getting worker trust: {e.response.status_code}")
            return None
        except Exception as e:
            logger.error(f"Error getting worker trust: {e}")
            return None

    # =========================================================================
    # Blind Worker Trust Reports
    # =========================================================================

    async def submit_blind_trust_report(self, report: BlindTrustReport) -> bool:
        """
        Submit a trust report for a blind worker.

        Called to report worker performance, which contributes to their trust score.

        Args:
            report: Trust report data

        Returns:
            True if successful
        """
        client = await self._get_client()

        try:
            response = await client.post(
                f"{self.base_url}/trust/report",
                json=asdict(report),
            )
            response.raise_for_status()
            data = response.json()
            return data.get("success", False)

        except httpx.HTTPStatusError as e:
            logger.error(f"HTTP error submitting trust report: {e.response.status_code}")
            return False
        except Exception as e:
            logger.error(f"Error submitting trust report: {e}")
            return False

    async def submit_blind_trust_reports_batch(
        self,
        reports: List[BlindTrustReport],
    ) -> Dict[str, Any]:
        """
        Submit multiple trust reports in a batch.

        More efficient than individual reports at epoch end.

        Args:
            reports: List of trust reports

        Returns:
            Response with success counts
        """
        client = await self._get_client()

        try:
            response = await client.post(
                f"{self.base_url}/trust/report/batch",
                json=[asdict(r) for r in reports],
            )
            response.raise_for_status()
            return response.json()

        except httpx.HTTPStatusError as e:
            logger.error(f"HTTP error submitting batch trust reports: {e.response.status_code}")
            return {"success": False, "error": str(e)}
        except Exception as e:
            logger.error(f"Error submitting batch trust reports: {e}")
            return {"success": False, "error": str(e)}

    # =========================================================================
    # Blind Worker Payments
    # =========================================================================

    async def submit_blind_payment(
        self,
        payment: BlindPaymentRequest,
    ) -> Dict[str, Any]:
        """
        Submit a payment for a blind worker.

        Core will resolve the blind_worker_id to a real wallet and
        execute the on-chain transfer.

        Args:
            payment: Payment request

        Returns:
            Response with payment_id and status
        """
        client = await self._get_client()

        try:
            response = await client.post(
                f"{self.base_url}/payments/blind",
                json=asdict(payment),
            )
            response.raise_for_status()
            result = response.json()
            logger.debug(
                f"Submitted blind payment: {payment.blind_worker_id[:16]}... "
                f"amount={payment.amount_dtao}"
            )
            return result

        except httpx.HTTPStatusError as e:
            logger.error(f"HTTP error submitting blind payment: {e.response.status_code}")
            return {"success": False, "error": str(e)}
        except Exception as e:
            logger.error(f"Error submitting blind payment: {e}")
            return {"success": False, "error": str(e)}

    async def submit_blind_payments_batch(
        self,
        payments: List[BlindPaymentRequest],
        epoch: int,
    ) -> Dict[str, Any]:
        """
        Submit a batch of payments for blind workers.

        More efficient than individual payments for epoch settlement.

        Args:
            payments: List of payment requests
            epoch: Epoch for all payments

        Returns:
            Response with batch_id and totals
        """
        client = await self._get_client()

        try:
            response = await client.post(
                f"{self.base_url}/payments/blind/batch",
                json={
                    "epoch": epoch,
                    "payments": [asdict(p) for p in payments],
                },
            )
            response.raise_for_status()
            result = response.json()
            logger.info(
                f"Submitted batch blind payments: {result.get('payment_count', 0)} payments, "
                f"total={result.get('total_amount', 0)} dTAO"
            )
            return result

        except httpx.HTTPStatusError as e:
            logger.error(f"HTTP error submitting batch payments: {e.response.status_code}")
            return {"success": False, "error": str(e)}
        except Exception as e:
            logger.error(f"Error submitting batch payments: {e}")
            return {"success": False, "error": str(e)}

    async def get_blind_payment_status(self, payment_id: str) -> Optional[Dict[str, Any]]:
        """
        Get status of a blind payment.

        Args:
            payment_id: Payment ID

        Returns:
            Payment status dict or None if not found
        """
        client = await self._get_client()

        try:
            response = await client.get(
                f"{self.base_url}/payments/blind/{payment_id}",
            )
            response.raise_for_status()
            return response.json()

        except httpx.HTTPStatusError as e:
            if e.response.status_code == 404:
                return None
            logger.error(f"HTTP error getting payment status: {e.response.status_code}")
            return None
        except Exception as e:
            logger.error(f"Error getting payment status: {e}")
            return None

    # =========================================================================
    # S3 Transfer Management
    # =========================================================================

    async def get_s3_transfer_status(self, transfer_id: str) -> Optional[Dict[str, Any]]:
        """
        Get status of an S3 transfer.

        GET /s3/transfers/{transfer_id}/status

        Returns:
            Dict with transfer_id, status, total_chunks, parts_completed, etc.
            None if not found.
        """
        client = await self._get_client()

        try:
            response = await client.get(
                f"{self.base_url}/s3/transfers/{transfer_id}/status",
            )
            response.raise_for_status()
            return response.json()

        except httpx.HTTPStatusError as e:
            if e.response.status_code == 404:
                return None
            logger.error(f"HTTP error getting S3 transfer status: {e.response.status_code}")
            return None
        except Exception as e:
            logger.error(f"Error getting S3 transfer status: {e}")
            return None

    async def complete_s3_transfer(self, transfer_id: str) -> Dict[str, Any]:
        """
        Complete an S3 multipart upload.

        POST /s3/transfers/{transfer_id}/complete

        Called when all parts have been uploaded. SubnetCore uses stored
        credentials and ETags to call CompleteMultipartUpload.

        Returns:
            Dict with success, transfer_id, location, parts_completed, total_parts, message
        """
        client = await self._get_client()

        try:
            response = await client.post(
                f"{self.base_url}/s3/transfers/{transfer_id}/complete",
            )
            response.raise_for_status()
            result = response.json()
            if result.get("success"):
                logger.info(f"S3 transfer {transfer_id} completed: {result.get('location')}")
            else:
                logger.warning(f"S3 transfer {transfer_id} completion failed: {result.get('message')}")
            return result

        except httpx.HTTPStatusError as e:
            logger.error(f"HTTP error completing S3 transfer: {e.response.status_code}")
            raise
        except Exception as e:
            logger.error(f"Error completing S3 transfer: {e}")
            raise


# =============================================================================
# Global Client Instance
# =============================================================================

_client: Optional[SubnetCoreClient] = None


def get_subnet_core_client() -> Optional[SubnetCoreClient]:
    """Get the global SubnetCoreClient instance."""
    return _client


def init_subnet_core_client(
    base_url: str,
    orchestrator_hotkey: str,
    orchestrator_uid: int,
    timeout: float = 30.0,
    signer=None,
) -> SubnetCoreClient:
    """
    Initialize the global SubnetCoreClient instance.

    Args:
        base_url: Base URL of BeamCore
        orchestrator_hotkey: This orchestrator's hotkey
        orchestrator_uid: This orchestrator's UID
        timeout: Request timeout
        signer: Optional bittensor wallet hotkey with .sign() method

    Returns:
        The initialized client
    """
    global _client
    _client = SubnetCoreClient(base_url, orchestrator_hotkey, orchestrator_uid, timeout, signer=signer)
    logger.info(f"SubnetCoreClient initialized: {base_url} (signer={'yes' if signer else 'none'})")
    return _client


async def close_subnet_core_client():
    """Close the global client."""
    global _client
    if _client:
        await _client.close()
        _client = None
