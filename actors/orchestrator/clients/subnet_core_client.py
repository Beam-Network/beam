"""
BeamCore API and NATS control client for orchestrators.
ORCH_GATEWAY_URL is retained as the participant-facing configuration name, but
it now points to the NATS orchestrator control endpoint, not a websocket gateway.
"""

import asyncio
import inspect
import json
import logging
import os
import pathlib
import ssl
import time
import uuid
from typing import Any, Callable, Dict, Optional

import httpx
import msgpack
import nats

from clients import control_auth, control_connection, control_messages, control_protocol
from core.task_offer_dispatcher import TaskOfferDispatcher

from middleware.metrics import BEAMCORE_UPSTREAM_DEGRADED

logger = logging.getLogger(__name__)

SCHEMA_VERSION = "orchestrator-control/v1"
SOFTWARE_VERSION = os.environ.get("BEAM_SOFTWARE_VERSION", "0.2.0")
SUBJECT_PREFIX = os.environ.get("ORCHESTRATOR_CONTROL_SUBJECT_PREFIX", "beam.orch.control").strip(".")
BEAM_ENV = os.environ.get("BEAM_ENV", "dev")
REQUEST_TIMEOUT = float(os.environ.get("ORCHESTRATOR_CONTROL_REQUEST_TIMEOUT_SECONDS", "0.75"))
TASK_RESULT_TIMEOUT = float(os.environ.get("ORCHESTRATOR_CONTROL_TASK_RESULT_TIMEOUT_SECONDS", "15.0"))
REQUEST_RETRY_ATTEMPTS = max(1, int(os.environ.get("ORCHESTRATOR_CONTROL_REQUEST_RETRY_ATTEMPTS", "3")))
REQUEST_RETRY_BACKOFF_SECONDS = max(0.0, float(os.environ.get("ORCHESTRATOR_CONTROL_REQUEST_RETRY_BACKOFF_SECONDS", "0.05")))
HEARTBEAT_INTERVAL = float(os.environ.get("ORCHESTRATOR_CONTROL_HEARTBEAT_INTERVAL_SECONDS", "5"))
STARTUP_CONNECT_ATTEMPTS = max(1, int(os.environ.get("ORCHESTRATOR_CONTROL_STARTUP_CONNECT_ATTEMPTS", "12")))
STARTUP_CONNECT_BACKOFF_SECONDS = max(0.1, float(os.environ.get("ORCHESTRATOR_CONTROL_STARTUP_CONNECT_BACKOFF_SECONDS", "1.0")))
REGISTRATION_RECOVERY_BACKOFF_SECONDS = max(0.25, float(os.environ.get("ORCHESTRATOR_CONTROL_REGISTRATION_RECOVERY_BACKOFF_SECONDS", "1.0")))
ORCH_GATEWAY_TLS_SERVER_NAME = (
    os.environ.get("ORCH_GATEWAY_TLS_SERVER_NAME", "").strip()
    or os.environ.get("NATS_TLS_SERVER_NAME", "").strip()
)


def _validate_nats_url(value: str) -> str:
    return control_protocol.validate_nats_url(value)


def _tls_context_for_url(url: str) -> tuple[Optional[ssl.SSLContext], Optional[str]]:
    return control_protocol.tls_context_for_url(url, ORCH_GATEWAY_TLS_SERVER_NAME)


def _tls_handshake_first_for_url(url: str) -> bool:
    return control_protocol.tls_handshake_first_for_url(url)


def _subject_hotkey(hotkey: str) -> str:
    return control_protocol.subject_hotkey(hotkey)


def _subject(direction: str, hotkey: str, message_type: str) -> str:
    return control_protocol.subject(
        direction,
        hotkey,
        message_type,
        prefix=SUBJECT_PREFIX,
        environment=BEAM_ENV,
    )


def _pack(envelope: dict[str, Any]) -> bytes:
    return control_protocol.pack(envelope)


def _unpack(data: bytes) -> dict[str, Any]:
    return control_protocol.unpack(data)


class SubnetCoreClient:
    def __init__(
        self,
        base_url: str,
        ws_base_url: str,
        orchestrator_hotkey: str,
        orchestrator_uid: int,
        timeout: float = 30.0,
        signer=None
    ):
        self.base_url = base_url.rstrip("/")
        self.ws_base_url = _validate_nats_url(ws_base_url)
        self.orchestrator_hotkey = orchestrator_hotkey
        self.orchestrator_uid = orchestrator_uid
        self.software_version = SOFTWARE_VERSION
        self.timeout = timeout
        self.signer = signer
        self._client: Optional[httpx.AsyncClient] = None
        self._worker_update_handler: Optional[Callable] = None
        self._worker_gateway = None
        self._task_offer_dispatcher: Optional[TaskOfferDispatcher] = None
        self._running = False
        self._registered = False
        self._registration_config: Optional[Dict[str, Any]] = None
        self._operator_ready = False
        self._desired_ready = False
        self._last_confirmed_ready: Optional[bool] = None
        self._ready_sync_task: Optional[asyncio.Task] = None
        self._heartbeat_task: Optional[asyncio.Task] = None
        self._registration_recovery_task: Optional[asyncio.Task] = None
        self._nc = None
        self._subscription = None
        self._connect_lock = asyncio.Lock()
        self._auth_recovery_task: Optional[asyncio.Task] = None
        self._api_key: Optional[str] = None
        self._api_key_expires: Optional[float] = None
        self._api_key_source: Optional[str] = None
        self._key_cache_path = pathlib.Path(f"/tmp/beam_orch_api_key_{orchestrator_hotkey[:16]}.json")
        self._load_cached_key()
        self._beamcore_upstream_degraded = False

    def _load_cached_key(self) -> None:
        control_auth.load_cached_key(self)

    def _persist_key(self) -> None:
        control_auth.persist_key(self)

    def _clear_cached_key(self) -> None:
        control_auth.clear_cached_key(self)

    def set_worker_update_handler(self, handler: Callable):
        self._worker_update_handler = handler

    def set_worker_gateway(self, gateway) -> None:
        self._worker_gateway = gateway
        gateway.set_upstream(self)
        self._task_offer_dispatcher = TaskOfferDispatcher(self._deliver_task_offer_batch_to_workers)
        self._desired_ready = self._registration_ready()

    def _registration_ready(self) -> bool:
        if not self._operator_ready:
            return False
        if self._worker_gateway is None:
            return False
        return int(getattr(self._worker_gateway, "connected_count", 0) or 0) > 0

    def prime_ready_state(self, ready: bool) -> None:
        self._operator_ready = ready
        self._desired_ready = self._registration_ready()

    def is_beamcore_upstream_degraded(self) -> bool:
        return self._beamcore_upstream_degraded

    def _note_beamcore_upstream_recovered(self, reason: str) -> None:
        if not self._beamcore_upstream_degraded:
            return
        self._beamcore_upstream_degraded = False
        BEAMCORE_UPSTREAM_DEGRADED.set(0)
        logger.info("BeamCore NATS control recovered (%s)", reason)

    def _nats_is_closed(self) -> bool:
        return control_connection._nats_is_closed(self)

    @staticmethod
    def _is_authorization_error(exc: Exception) -> bool:
        return control_connection._is_authorization_error(exc)

    def _schedule_auth_recovery(self) -> None:
        control_connection._schedule_auth_recovery(self)

    async def _recover_from_nats_authorization_error(self) -> None:
        await control_connection._recover_from_nats_authorization_error(self)

    async def _ensure_nats_connection(self, reason: str) -> None:
        await control_connection._ensure_nats_connection(self, reason)

    async def _request_retry_delay(self, attempt: int) -> None:
        await control_connection._request_retry_delay(self, attempt)

    async def _ensure_api_key(self) -> Optional[str]:
        return await control_connection._ensure_api_key(self)

    async def start_polling(self):
        await control_connection.start_polling(self)

    async def _ensure_http_registration(self) -> None:
        await control_connection._ensure_http_registration(self)

    async def _close_nats_session(self, *, drain: bool = False) -> None:
        await control_connection._close_nats_session(self, drain=drain)

    async def _connect_nats_session(self) -> None:
        await control_connection._connect_nats_session(self)

    async def _on_nats_disconnected(self) -> None:
        await control_connection._on_nats_disconnected(self)

    async def _on_nats_reconnected(self) -> None:
        await control_connection._on_nats_reconnected(self)

    async def _on_nats_error(self, exc: Exception) -> None:
        await control_connection._on_nats_error(self, exc)

    async def _on_nats_closed(self) -> None:
        await control_connection._on_nats_closed(self)

    async def _recover_nats_connection(
        self,
        reason: str,
        *,
        force: bool = False,
    ) -> None:
        await control_connection._recover_nats_connection(self, reason, force=force)

    def _schedule_registration_recovery(self, reason: str) -> None:
        control_connection._schedule_registration_recovery(self, reason)

    async def _recover_registration_loop(self, reason: str) -> None:
        await control_connection._recover_registration_loop(self, reason)

    async def stop_polling(self):
        await control_connection.stop_polling(self)

    def set_registration_config(
        self,
        url: str,
        region: str,
        max_workers: int = 10000,
        uid: int = None,
        fee_percentage: float = 0.0,
        gateway_url: Optional[str] = None,
    ):
        control_connection.set_registration_config(
            self,
            url,
            region,
            max_workers,
            uid,
            fee_percentage,
            gateway_url,
        )

    async def _register_via_nats(self) -> bool:
        return await control_connection._register_via_nats(self)
    async def _heartbeat_loop(self) -> None:
        await control_messages._heartbeat_loop(self)

    def _envelope(
        self,
        message_type: str,
        payload: dict[str, Any],
        request_id: Optional[str] = None,
    ) -> dict[str, Any]:
        return control_messages._envelope(self, message_type, payload, request_id)

    async def _send_nats_publish(
        self,
        message_type: str,
        payload: dict[str, Any],
    ) -> None:
        await control_messages._send_nats_publish(self, message_type, payload)

    async def _send_nats_request(
        self,
        message_type: str,
        payload: dict[str, Any],
        timeout: float = REQUEST_TIMEOUT,
        attempts: int = REQUEST_RETRY_ATTEMPTS,
    ) -> dict[str, Any]:
        return await control_messages._send_nats_request(
            self,
            message_type,
            payload,
            timeout,
            attempts,
        )

    async def _handle_runtime_message(self, msg) -> None:
        await control_messages._handle_runtime_message(self, msg)

    async def _handle_task_offer_batch(self, data: dict) -> None:
        await control_messages._handle_task_offer_batch(self, data)

    async def _deliver_task_offer_batch_to_workers(self, data: dict) -> None:
        await control_messages._deliver_task_offer_batch_to_workers(self, data)

    def _schedule_ready_sync_if_needed(self) -> None:
        control_messages._schedule_ready_sync_if_needed(self)

    async def _sync_ready_state_in_background(self) -> None:
        await control_messages._sync_ready_state_in_background(self)

    async def _apply_desired_ready_state(self) -> bool:
        return await control_messages._apply_desired_ready_state(self)

    async def send_task_result_strict(self, payload: dict) -> Dict[str, Any]:
        return await control_messages.send_task_result_strict(self, payload)

    async def send_task_result(self, payload: dict) -> Dict[str, Any]:
        return await control_messages.send_task_result(self, payload)

    async def send_capability_update(self, manifest: dict) -> Dict[str, Any]:
        return await control_messages.send_capability_update(self, manifest)

    async def update_worker_gateway(
        self,
        gateway_url: str,
        max_workers: int = 10000,
        health: str = "healthy",
    ) -> Dict[str, Any]:
        return await control_messages.update_worker_gateway(
            self,
            gateway_url,
            max_workers,
            health,
        )

    async def sync_ready_if_eligible(self) -> bool:
        return await control_messages.sync_ready_if_eligible(self)

    async def set_ready(self, ready: bool) -> bool:
        return await control_messages.set_ready(self, ready)
    def _auth_headers(self) -> dict:
        return control_auth.auth_headers(self)

    async def _get_client(self) -> httpx.AsyncClient:
        return await control_auth.get_client(self, httpx)

    async def _inject_auth_headers(self, request: httpx.Request):
        await control_auth.inject_auth_headers(self, request)
    async def close(self):
        await self.stop_polling()
        if self._client:
            await self._client.aclose()
            self._client = None


_client: Optional[SubnetCoreClient] = None


def get_subnet_core_client() -> Optional[SubnetCoreClient]:
    return _client


def init_subnet_core_client(
    base_url: str,
    ws_base_url: str,
    orchestrator_hotkey: str,
    orchestrator_uid: int,
    timeout: float = 30.0,
    signer=None
) -> SubnetCoreClient:
    global _client
    _client = SubnetCoreClient(
        base_url,
        ws_base_url,
        orchestrator_hotkey,
        orchestrator_uid,
        timeout,
        signer=signer,
    )
    logger.info("SubnetCoreClient initialized: http=%s nats=%s (signer=%s)", base_url, ws_base_url, "yes" if signer else "none")
    return _client


async def close_subnet_core_client():
    global _client
    if _client:
        await _client.close()
        _client = None
