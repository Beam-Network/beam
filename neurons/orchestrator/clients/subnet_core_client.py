"""
BeamCore API and NATS control client for orchestrators.

ORCH_GATEWAY_URL points to the BeamCore NATS orchestrator control endpoint.
"""

import asyncio
import inspect
import json
import logging
import os
import pathlib
import time
import uuid
from typing import Any, Callable, Dict, Optional

import httpx
import msgpack
import nats

from core.task_offer_dispatcher import TaskOfferDispatcher

from middleware.metrics import BEAMCORE_UPSTREAM_DEGRADED

logger = logging.getLogger(__name__)

SCHEMA_VERSION = "orchestrator-control/v1"
SUBJECT_PREFIX = os.environ.get("ORCHESTRATOR_CONTROL_SUBJECT_PREFIX", "beam.orch.control").strip(".")
BEAM_ENV = os.environ.get("BEAM_ENV", "prod")
REQUEST_TIMEOUT = float(os.environ.get("ORCHESTRATOR_CONTROL_REQUEST_TIMEOUT_SECONDS", "0.75"))
TASK_RESULT_TIMEOUT = float(os.environ.get("ORCHESTRATOR_CONTROL_TASK_RESULT_TIMEOUT_SECONDS", "15.0"))
TASK_ACCEPT_TIMEOUT = float(os.environ.get("ORCHESTRATOR_CONTROL_TASK_ACCEPT_TIMEOUT_SECONDS", "5.0"))
TASK_ACCEPT_RETRY_ATTEMPTS = max(
    1,
    int(os.environ.get("ORCHESTRATOR_CONTROL_TASK_ACCEPT_RETRY_ATTEMPTS", "3")),
)
REQUEST_RETRY_ATTEMPTS = max(1, int(os.environ.get("ORCHESTRATOR_CONTROL_REQUEST_RETRY_ATTEMPTS", "3")))
REQUEST_RETRY_BACKOFF_SECONDS = max(0.0, float(os.environ.get("ORCHESTRATOR_CONTROL_REQUEST_RETRY_BACKOFF_SECONDS", "0.05")))
HEARTBEAT_INTERVAL = float(os.environ.get("ORCHESTRATOR_CONTROL_HEARTBEAT_INTERVAL_SECONDS", "5"))
STARTUP_CONNECT_ATTEMPTS = max(1, int(os.environ.get("ORCHESTRATOR_CONTROL_STARTUP_CONNECT_ATTEMPTS", "12")))
STARTUP_CONNECT_BACKOFF_SECONDS = max(0.1, float(os.environ.get("ORCHESTRATOR_CONTROL_STARTUP_CONNECT_BACKOFF_SECONDS", "1.0")))


def _validate_nats_url(value: str) -> str:
    clean = value.strip().rstrip("/")
    if clean.startswith(("http://", "https://", "ws://", "wss://")):
        raise ValueError("Set ORCH_GATEWAY_URL to a NATS endpoint using nats:// or tls://")
    if not clean.startswith(("nats://", "tls://")):
        raise ValueError("Set ORCH_GATEWAY_URL to a NATS endpoint using nats:// or tls://")
    return clean


def _subject_hotkey(hotkey: str) -> str:
    return hotkey.lower()


def _subject(direction: str, hotkey: str, message_type: str) -> str:
    return f"{SUBJECT_PREFIX}.{BEAM_ENV}.{direction}.{_subject_hotkey(hotkey)}.{message_type}"


def _pack(envelope: dict[str, Any]) -> bytes:
    return msgpack.packb(envelope, use_bin_type=True)


def _unpack(data: bytes) -> dict[str, Any]:
    value = msgpack.unpackb(data, raw=False)
    if not isinstance(value, dict):
        raise ValueError("NATS control envelope must be a map")
    return value


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
        try:
            if self._key_cache_path.exists():
                data = json.loads(self._key_cache_path.read_text())
                if data.get("expires") and time.time() < data["expires"] - 60:
                    self._api_key = data["key"]
                    self._api_key_expires = data["expires"]
                    self._api_key_source = "cache"
                    logger.info("Loaded cached API key from disk for %s", self.orchestrator_hotkey[:16])
        except Exception:
            pass

    def _persist_key(self) -> None:
        try:
            self._key_cache_path.write_text(json.dumps({"key": self._api_key, "expires": self._api_key_expires}))
        except Exception:
            pass

    def _clear_cached_key(self) -> None:
        self._api_key = None
        self._api_key_expires = None
        self._api_key_source = None
        try:
            self._key_cache_path.unlink(missing_ok=True)
        except Exception:
            pass

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
        nc = self._nc
        if nc is None:
            return True
        value = getattr(nc, "is_closed", False)
        return bool(value() if callable(value) else value)

    @staticmethod
    def _is_authorization_error(exc: Exception) -> bool:
        return "authorization" in str(exc).lower()

    def _schedule_auth_recovery(self) -> None:
        if not self._running:
            return
        if self._api_key_source == "env":
            logger.error("BeamCore NATS auth returned unauthorized for BEAMCORE_ORCHESTRATOR_API_KEY")
            return
        if not self.signer:
            logger.error("BeamCore NATS auth returned unauthorized and signer support is unavailable")
            return
        if self._auth_recovery_task and not self._auth_recovery_task.done():
            return
        self._auth_recovery_task = asyncio.create_task(self._recover_from_nats_authorization_error())

    async def _recover_from_nats_authorization_error(self) -> None:
        try:
            logger.warning("BeamCore NATS auth requested a fresh orchestrator API key")
            self._clear_cached_key()
            await self._recover_nats_connection("NATS authorization rejection", force=True)
        except Exception as exc:
            logger.warning("BeamCore NATS control auth recovery failed: %s", exc)
        finally:
            self._auth_recovery_task = None

    async def _ensure_nats_connection(self, reason: str) -> None:
        if self._nc is not None and not self._nats_is_closed():
            return
        await self._recover_nats_connection(reason, force=True)

    async def _request_retry_delay(self, attempt: int) -> None:
        delay = min(0.25, REQUEST_RETRY_BACKOFF_SECONDS * (2 ** max(0, attempt - 1)))
        if delay > 0:
            await asyncio.sleep(delay)

    async def _ensure_api_key(self) -> Optional[str]:
        if self._api_key and self._api_key_expires and time.time() < self._api_key_expires - 60:
            if not self._api_key_source:
                self._api_key_source = "cache"
            return self._api_key

        env_api_key = os.environ.get("BEAMCORE_ORCHESTRATOR_API_KEY")
        if env_api_key and (env_api_key.startswith("b1m_") or env_api_key.startswith("bck_")):
            self._api_key = env_api_key
            self._api_key_expires = time.time() + 86400 * 365
            self._api_key_source = "env"
            logger.info("Using BEAMCORE_ORCHESTRATOR_API_KEY from environment for %s...", self.orchestrator_hotkey[:16])
            return self._api_key

        if not self.signer:
            logger.error("Cannot get API key: no signer configured and BEAMCORE_ORCHESTRATOR_API_KEY not set")
            return None

        client = await self._get_client()
        try:
            challenge_resp = await client.post(f"{self.base_url}/auth/challenge", json={"hotkey": self.orchestrator_hotkey, "role": "orchestrator"})
            if challenge_resp.status_code == 429:
                retry_after = challenge_resp.headers.get("Retry-After", "300")
                logger.error("Too many auth challenge requests; retry after %s seconds", retry_after)
                return None
            if challenge_resp.status_code != 200:
                logger.error("Failed to get auth challenge: %s", challenge_resp.status_code)
                return None
            challenge_data = challenge_resp.json()
            message = challenge_data["message"]
            try:
                sig_bytes = self.signer.sign(message.encode("utf-8"))
                signature = "0x" + (sig_bytes.hex() if isinstance(sig_bytes, bytes) else str(sig_bytes))
            except Exception as e:
                logger.error("Failed to sign challenge: %s", e)
                return None
            verify_resp = await client.post(
                f"{self.base_url}/auth/verify",
                json={
                    "challenge_id": challenge_data["challenge_id"],
                    "hotkey": self.orchestrator_hotkey,
                    "signature": signature,
                    "role": "orchestrator",
                    "key_name": "Orchestrator NATS Control Key",
                },
            )
            if verify_resp.status_code != 200:
                logger.error("Failed to verify signature: %s - %s", verify_resp.status_code, verify_resp.text)
                return None
            verify_data = verify_resp.json()
            if not verify_data.get("success") or not verify_data.get("api_key"):
                logger.error("Auth verify failed: %s", verify_data.get("message", "Unknown error"))
                return None
            self._api_key = verify_data["api_key"]
            self._api_key_expires = time.time() + 86400
            self._api_key_source = "signed"
            self._persist_key()
            logger.info("Obtained API key for orchestrator %s...", self.orchestrator_hotkey[:16])
            return self._api_key
        except Exception as e:
            logger.error("Failed to get API key: %s", e)
            return None

    async def start_polling(self):
        if self._running:
            logger.warning("Already running")
            return
        self._running = True
        last_error: Optional[Exception] = None
        for attempt in range(1, STARTUP_CONNECT_ATTEMPTS + 1):
            try:
                await self._connect_nats_session()
                registered = await self._register_via_nats()
                if not registered:
                    raise RuntimeError("NATS control registration failed")
                self._heartbeat_task = asyncio.create_task(self._heartbeat_loop())
                logger.info("Started NATS control connection to %s", self.ws_base_url)
                return
            except Exception as exc:
                last_error = exc
                await self._close_nats_session()
                if attempt >= STARTUP_CONNECT_ATTEMPTS:
                    self._running = False
                    break
                self._beamcore_upstream_degraded = True
                BEAMCORE_UPSTREAM_DEGRADED.set(1)
                delay = min(5.0, STARTUP_CONNECT_BACKOFF_SECONDS * attempt)
                logger.warning(
                    "BeamCore NATS control startup handshake attempt %s/%s failed: %s; retrying in %.1fs",
                    attempt,
                    STARTUP_CONNECT_ATTEMPTS,
                    exc,
                    delay,
                )
                await asyncio.sleep(delay)
        raise RuntimeError("Cannot start NATS control after startup retries") from last_error

    async def _close_nats_session(self, *, drain: bool = False) -> None:
        subscription = self._subscription
        nc = self._nc
        self._subscription = None
        self._nc = None
        self._registered = False
        if subscription:
            try:
                await subscription.unsubscribe()
            except Exception:
                pass
        if nc:
            try:
                close_result = nc.drain() if drain else nc.close()
                if inspect.isawaitable(close_result):
                    await close_result
            except Exception:
                pass

    async def _connect_nats_session(self) -> None:
        last_error: Optional[Exception] = None

        async def connect_once(key: str):
            return await nats.connect(
                servers=[self.ws_base_url],
                user=self.orchestrator_hotkey,
                password=key,
                name=f"beam-orchestrator-{self.orchestrator_hotkey[:12]}",
                max_reconnect_attempts=-1,
                reconnect_time_wait=1,
                disconnected_cb=self._on_nats_disconnected,
                reconnected_cb=self._on_nats_reconnected,
                error_cb=self._on_nats_error,
                closed_cb=self._on_nats_closed,
            )

        for attempt in range(1, STARTUP_CONNECT_ATTEMPTS + 1):
            try:
                api_key = await self._ensure_api_key()
                if not api_key:
                    raise RuntimeError("NATS control requires an orchestrator API key before connect")

                try:
                    self._nc = await connect_once(api_key)
                except Exception as exc:
                    if self._api_key_source == "env" and self._is_authorization_error(exc):
                        raise
                    if self._api_key_source != "env" and self.signer and self._is_authorization_error(exc):
                        logger.warning("BeamCore NATS auth requested a fresh orchestrator API key")
                        self._clear_cached_key()
                        api_key = await self._ensure_api_key()
                        if not api_key:
                            raise RuntimeError("Cannot refresh NATS control orchestrator API key") from exc
                        self._nc = await connect_once(api_key)
                    else:
                        raise

                self._subscription = await self._nc.subscribe(
                    _subject("runtime", self.orchestrator_hotkey, "*"),
                    cb=self._handle_runtime_message,
                )
                return
            except Exception as exc:
                last_error = exc
                if attempt >= STARTUP_CONNECT_ATTEMPTS:
                    break
                self._beamcore_upstream_degraded = True
                BEAMCORE_UPSTREAM_DEGRADED.set(1)
                delay = min(5.0, STARTUP_CONNECT_BACKOFF_SECONDS * attempt)
                logger.warning(
                    "BeamCore NATS control startup attempt %s/%s failed: %s; retrying in %.1fs",
                    attempt,
                    STARTUP_CONNECT_ATTEMPTS,
                    exc,
                    delay,
                )
                await asyncio.sleep(delay)

        raise RuntimeError("Cannot connect to NATS control after startup retries") from last_error

    async def _on_nats_disconnected(self) -> None:
        self._beamcore_upstream_degraded = True
        BEAMCORE_UPSTREAM_DEGRADED.set(1)
        logger.warning("BeamCore NATS control disconnected")

    async def _on_nats_reconnected(self) -> None:
        logger.info("BeamCore NATS control reconnected")
        if not self._running:
            return
        try:
            await self._register_via_nats()
            self._schedule_ready_sync_if_needed()
            self._note_beamcore_upstream_recovered("NATS reconnect")
        except Exception as exc:
            logger.warning("BeamCore NATS control re-registration failed after reconnect: %s", exc)

    async def _on_nats_error(self, exc: Exception) -> None:
        logger.warning("BeamCore NATS control error: %s", exc)
        if self._is_authorization_error(exc):
            self._schedule_auth_recovery()

    async def _on_nats_closed(self) -> None:
        logger.warning("BeamCore NATS control closed")

    async def _recover_nats_connection(self, reason: str, *, force: bool = False) -> None:
        if not self._running:
            return
        async with self._connect_lock:
            if self._nc is not None and not force and not self._nats_is_closed():
                return
            old_nc = self._nc
            self._nc = None
            self._subscription = None
            self._registered = False
            if old_nc:
                try:
                    close_result = old_nc.close()
                    if inspect.isawaitable(close_result):
                        await close_result
                except Exception:
                    pass
            await self._connect_nats_session()
        await self._register_via_nats()
        self._schedule_ready_sync_if_needed()
        logger.info("Recovered BeamCore NATS control connection after %s", reason)
    async def stop_polling(self):
        self._running = False
        if self._heartbeat_task:
            self._heartbeat_task.cancel()
            try:
                await self._heartbeat_task
            except asyncio.CancelledError:
                pass
            self._heartbeat_task = None
        if self._ready_sync_task:
            self._ready_sync_task.cancel()
            try:
                await self._ready_sync_task
            except asyncio.CancelledError:
                pass
            self._ready_sync_task = None
        if self._auth_recovery_task:
            self._auth_recovery_task.cancel()
            try:
                await self._auth_recovery_task
            except asyncio.CancelledError:
                pass
            self._auth_recovery_task = None
        if self._task_offer_dispatcher:
            await self._task_offer_dispatcher.stop()
        if self._subscription:
            await self._subscription.unsubscribe()
            self._subscription = None
        if self._nc:
            await self._nc.drain()
            self._nc = None
        self._registered = False
        logger.info("NATS control connection stopped")

    def set_registration_config(self, url: str, region: str, max_workers: int = 10000, uid: int = None, fee_percentage: float = 0.0, gateway_url: Optional[str] = None):
        self._registration_config = {
            "url": url,
            "region": region,
            "max_workers": max_workers,
            "uid": uid,
            "fee_percentage": fee_percentage,
            "gateway_url": gateway_url,
        }
        logger.info("Registration config set: region=%s, max_workers=%s", region, max_workers)

    async def _register_via_nats(self) -> bool:
        if not self._registration_config:
            logger.warning("Cannot register via NATS: registration config not set")
            return False
        cfg = dict(self._registration_config)
        reg_message = f"{self.orchestrator_hotkey}:{cfg.get('url') or ''}:{cfg.get('region') or ''}"
        signature = ""
        if self.signer:
            try:
                sig_bytes = self.signer.sign(reg_message.encode())
                signature = "0x" + (sig_bytes.hex() if isinstance(sig_bytes, bytes) else str(sig_bytes))
            except Exception as e:
                logger.warning("Failed to sign registration: %s", e)
        self._desired_ready = self._registration_ready()
        cfg["ready"] = self._desired_ready
        cfg["signature"] = signature
        response = await self._send_nats_request("register", cfg, timeout=max(REQUEST_TIMEOUT, 3.0))
        if response.get("type") == "register_ack":
            self._registered = True
            logger.info("Registered via NATS control: status=%s", response.get("status"))
            self._schedule_ready_sync_if_needed()
            return True
        self._registered = False
        logger.error("Registration failed: %s", response.get("message") or response.get("reason") or response)
        return False

    async def _heartbeat_loop(self) -> None:
        while self._running:
            try:
                await self._send_nats_publish("heartbeat", {})
            except Exception as exc:
                logger.debug("heartbeat publish failed: %s", exc)
            await asyncio.sleep(HEARTBEAT_INTERVAL)

    def _envelope(self, message_type: str, payload: dict[str, Any], request_id: Optional[str] = None) -> dict[str, Any]:
        envelope = {
            "message_id": f"orchestrator-control/v1:{BEAM_ENV}:{self.orchestrator_hotkey}:{message_type}:{request_id or uuid.uuid4().hex}",
            "schema_version": SCHEMA_VERSION,
            "environment": BEAM_ENV,
            "hotkey": self.orchestrator_hotkey,
            "message_type": message_type,
            "occurred_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
            "producer": "orchestrator",
            "payload": payload,
        }
        if request_id is not None:
            envelope["request_id"] = request_id
        return envelope

    async def _send_nats_publish(self, message_type: str, payload: dict[str, Any]) -> None:
        await self._ensure_nats_connection(f"{message_type}_publish_disconnected")
        if not self._nc:
            raise RuntimeError("NATS control is not connected")
        payload_bytes = _pack(self._envelope(message_type, payload))
        subject = _subject("orch", self.orchestrator_hotkey, message_type)
        try:
            await self._nc.publish(subject, payload_bytes)
        except Exception:
            if self._nats_is_closed():
                await self._recover_nats_connection(f"{message_type}_publish_closed", force=True)
                if not self._nc:
                    raise RuntimeError("NATS control is not connected")
                await self._nc.publish(subject, payload_bytes)
                return
            raise

    async def _send_nats_request(
        self,
        message_type: str,
        payload: dict[str, Any],
        timeout: float = REQUEST_TIMEOUT,
        attempts: int = REQUEST_RETRY_ATTEMPTS,
    ) -> dict[str, Any]:
        request_id = payload.get("request_id") or uuid.uuid4().hex
        request_payload = _pack(self._envelope(message_type, {**payload, "request_id": request_id}, request_id))
        subject = _subject("orch", self.orchestrator_hotkey, message_type)
        last_error: Exception | None = None
        request_attempts = max(1, attempts)
        for attempt in range(request_attempts):
            await self._ensure_nats_connection(f"{message_type}_request_disconnected")
            if not self._nc:
                raise RuntimeError("NATS control is not connected")
            try:
                msg = await self._nc.request(subject, request_payload, timeout=timeout)
                envelope = _unpack(msg.data)
                response = envelope.get("payload") or {}
                if not isinstance(response, dict):
                    raise RuntimeError("invalid BeamCore response payload")
                if response.get("type") == "error":
                    reason = response.get("reason") or response.get("message") or "NATS control request failed"
                    if reason == "orchestrator_not_registered" and message_type != "register":
                        self._registered = False
                        await self._register_via_nats()
                        continue
                    raise RuntimeError(reason)
                return response
            except Exception as exc:
                last_error = exc
                if self._nats_is_closed():
                    await self._recover_nats_connection(f"{message_type}_request_closed", force=True)
                if attempt < request_attempts - 1:
                    await self._request_retry_delay(attempt)
        raise RuntimeError(f"NATS control request failed: {last_error}") from last_error
    async def _handle_runtime_message(self, msg) -> None:
        try:
            envelope = _unpack(msg.data)
            payload = envelope.get("payload") or {}
            if not isinstance(payload, dict):
                raise ValueError("payload must be an object")
            msg_type = envelope.get("message_type") or payload.get("type")
            if msg_type == "worker_task_offer_batch":
                self._note_beamcore_upstream_recovered("worker_task_offer_batch from BeamCore")
                accepted = await self._handle_task_offer_batch(payload)
                if msg.reply:
                    response = self._envelope("worker_task_offer_batch", {"type": "worker_task_offer_batch_ack", "accepted": accepted, "batch_id": payload.get("batch_id")}, envelope.get("request_id"))
                    await msg.respond(_pack(response))
            elif msg_type == "error":
                logger.warning("BeamCore NATS control error: %s", payload)
            else:
                logger.debug("Unknown NATS control message type: %s", msg_type)
                if msg.reply:
                    await msg.respond(_pack(self._envelope("error", {"type": "error", "reason": "unknown_message_type"}, envelope.get("request_id"))))
        except Exception as exc:
            logger.error("Error handling NATS control message: %s", exc)
            if msg.reply:
                await msg.respond(_pack(self._envelope("error", {"type": "error", "reason": "handler_error"})))

    async def _handle_task_offer_batch(self, data: dict) -> bool:
        batch_id = data.get("batch_id")
        offers = data.get("offers") or []
        if not isinstance(offers, list) or not offers:
            logger.warning("worker_task_offer_batch missing offers: batch=%s", batch_id)
            return False
        if not self._worker_gateway:
            logger.warning("No local worker gateway available for batch %s", batch_id)
            return False
        if self._task_offer_dispatcher:
            await self._task_offer_dispatcher.start()
            accepted = self._task_offer_dispatcher.enqueue_offer(data)
            if accepted:
                logger.info("worker_task_offer_batch queued for local workers: batch=%s offers=%s", batch_id, len(offers))
            return accepted
        await self._deliver_task_offer_batch_to_workers(data)
        return True

    async def _deliver_task_offer_batch_to_workers(self, data: dict) -> None:
        batch_id = data.get("batch_id")
        offers = data.get("offers") or []
        if not isinstance(offers, list) or not offers or not self._worker_gateway:
            return
        delivered = 0
        for offer in offers:
            if not isinstance(offer, dict):
                continue
            workers = self._worker_gateway.get_workers_round_robin(1)
            if not workers:
                logger.warning("No connected local workers for batch %s", batch_id)
                break
            worker_id = workers[0]
            if await self._worker_gateway.deliver_task_offer(worker_id, offer):
                delivered += 1
            else:
                logger.warning("Failed to forward task offer: batch=%s worker=%s task=%s", batch_id, worker_id, offer.get("task_id"))
        logger.info("worker_task_offer_batch delivered locally: batch=%s offers=%s delivered=%s", batch_id, len(offers), delivered)


    def _schedule_ready_sync_if_needed(self) -> None:
        if not self._running or not self._registered:
            return
        if self._last_confirmed_ready == self._desired_ready:
            return
        if self._ready_sync_task and not self._ready_sync_task.done():
            return
        self._ready_sync_task = asyncio.create_task(self._sync_ready_state_in_background())

    async def _sync_ready_state_in_background(self) -> None:
        try:
            while self._running and self._registered and self._last_confirmed_ready != self._desired_ready:
                try:
                    applied = await self._apply_desired_ready_state()
                    if applied:
                        return
                except Exception as exc:
                    logger.warning("Failed to sync queued ready=%s through NATS control: %s", self._desired_ready, exc)
                await asyncio.sleep(5.0)
        except asyncio.CancelledError:
            raise
        finally:
            self._ready_sync_task = None

    async def _apply_desired_ready_state(self) -> bool:
        requested_ready = self._desired_ready
        response = await self._send_nats_request("set_ready", {"ready": requested_ready})
        confirmed = bool(response.get("ready", requested_ready))
        self._desired_ready = confirmed
        self._last_confirmed_ready = confirmed
        logger.info("Orchestrator ready=%s set on BeamCore (uid=%s)", confirmed, response.get("uid"))
        return confirmed == requested_ready
    async def send_task_accept(self, task_id: str, worker_id: str, offer_id: Optional[str], worker_version: Optional[str]) -> Dict[str, Any]:
        try:
            return await self._send_nats_request(
                "task_accept",
                {
                    "task_id": task_id,
                    "worker_id": worker_id,
                    "offer_id": offer_id or task_id,
                    "worker_version": worker_version,
                },
                timeout=max(REQUEST_TIMEOUT, TASK_ACCEPT_TIMEOUT),
                attempts=TASK_ACCEPT_RETRY_ATTEMPTS,
            )
        except Exception as exc:
            logger.warning("send_task_accept send error: %s", exc)
            return {"type": "task_accept_ack", "task_id": task_id, "offer_id": offer_id or task_id, "accepted": False, "reason": "beamcore_accept_forward_failed"}

    async def send_task_reject(self, task_id: str, worker_id: str, offer_id: Optional[str], reason: Optional[str] = None) -> Dict[str, Any]:
        try:
            payload = {"task_id": task_id, "worker_id": worker_id, "offer_id": offer_id or task_id}
            if reason:
                payload["reason"] = reason
            return await self._send_nats_request("task_reject", payload)
        except Exception as exc:
            logger.warning("send_task_reject send error: %s", exc)
            return {"type": "task_reject_ack", "task_id": task_id, "offer_id": offer_id or task_id, "accepted": False, "reason": "beamcore_reject_forward_failed"}

    async def send_task_result_strict(self, payload: dict) -> Dict[str, Any]:
        task_id = payload.get("task_id")
        offer_id = payload.get("offer_id") or task_id
        if not task_id or not offer_id:
            return {"type": "task_result_ack", "task_id": task_id, "offer_id": offer_id, "received": False, "completed": False, "reason": "missing_task_or_offer_id"}
        message = {"task_id": task_id, "offer_id": offer_id, "worker_id": payload.get("worker_id"), "success": bool(payload.get("success"))}
        for key in ("etag", "chunk_hash", "error"):
            if payload.get(key) is not None:
                message[key] = payload[key]
        return await self._send_nats_request("task_result", message, timeout=max(REQUEST_TIMEOUT, TASK_RESULT_TIMEOUT))

    async def send_task_result(self, payload: dict) -> Dict[str, Any]:
        task_id = payload.get("task_id")
        offer_id = payload.get("offer_id") or task_id
        try:
            return await self.send_task_result_strict(payload)
        except Exception as exc:
            logger.warning("send_task_result send error: %s", exc)
            return {"type": "task_result_ack", "task_id": task_id, "offer_id": offer_id, "received": False, "completed": False, "reason": "beamcore_result_forward_failed"}
    async def update_worker_gateway(self, gateway_url: str, max_workers: int = 10000, health: str = "healthy") -> Dict[str, Any]:
        return await self._send_nats_request("gateway_update", {"gateway_url": gateway_url, "max_workers": max_workers, "health": health})

    async def sync_ready_if_eligible(self) -> bool:
        self._desired_ready = self._registration_ready()
        if not self._registered:
            logger.info("Ready intent recorded; NATS control registration is not complete")
            return False
        if not self._desired_ready:
            if self._last_confirmed_ready is not False:
                try:
                    await self._apply_desired_ready_state()
                except Exception as exc:
                    self._schedule_ready_sync_if_needed()
                    logger.info("Queued ready=False after transient NATS control sync failure: %s", exc)
            return False
        return await self._apply_desired_ready_state()

    async def set_ready(self, ready: bool) -> bool:
        self._operator_ready = ready
        self._desired_ready = self._registration_ready()
        if not self._registered:
            logger.info("Queued ready=%s until NATS control registration completes", self._desired_ready)
            return False
        try:
            return await self._apply_desired_ready_state()
        except Exception as exc:
            self._schedule_ready_sync_if_needed()
            logger.info("Queued ready=%s after transient NATS control sync failure: %s", self._desired_ready, exc)
            return False

    def _auth_headers(self) -> dict:
        timestamp = str(int(time.time()))
        nonce = uuid.uuid4().hex[:8]
        action = "request"
        message = f"orchestrator_auth:{self.orchestrator_hotkey}:{timestamp}:{action}:{nonce}"
        signature = ""
        if self.signer:
            try:
                sig_bytes = self.signer.sign(message.encode("utf-8"))
                signature = sig_bytes.hex() if isinstance(sig_bytes, bytes) else str(sig_bytes)
            except Exception as e:
                logger.warning("Failed to sign auth message: %s", e)
                signature = "unsigned"
        else:
            signature = "unsigned"
        headers = {
            "X-Hotkey": self.orchestrator_hotkey,
            "X-Orchestrator-Hotkey": self.orchestrator_hotkey,
            "X-Orchestrator-Uid": str(self.orchestrator_uid),
            "X-Orchestrator-Timestamp": timestamp,
            "X-Orchestrator-Nonce": nonce,
            "X-Orchestrator-Signature": signature,
            "X-Orchestrator-Action": action,
        }
        if self._api_key:
            headers["X-Api-Key"] = self._api_key
        return headers

    async def _get_client(self) -> httpx.AsyncClient:
        if self._client is None:
            self._client = httpx.AsyncClient(timeout=self.timeout, event_hooks={"request": [self._inject_auth_headers]})
        return self._client

    async def _inject_auth_headers(self, request: httpx.Request):
        if "/auth/challenge" not in str(request.url) and "/auth/verify" not in str(request.url):
            if not self._api_key:
                await self._ensure_api_key()
        for key, value in self._auth_headers().items():
            request.headers[key] = value

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
