"""NATS session startup, recovery, registration, and shutdown."""

import asyncio
import inspect
import logging
import os
from typing import Optional

import nats

from clients import control_auth, control_protocol
from middleware.metrics import BEAMCORE_UPSTREAM_DEGRADED

logger = logging.getLogger(__name__)

SUBJECT_PREFIX = os.environ.get(
    "ORCHESTRATOR_CONTROL_SUBJECT_PREFIX", "beam.orch.control"
).strip(".")
BEAM_ENV = os.environ.get("BEAM_ENV", "dev")
REQUEST_TIMEOUT = float(
    os.environ.get("ORCHESTRATOR_CONTROL_REQUEST_TIMEOUT_SECONDS", "0.75")
)
REQUEST_RETRY_BACKOFF_SECONDS = max(
    0.0,
    float(
        os.environ.get(
            "ORCHESTRATOR_CONTROL_REQUEST_RETRY_BACKOFF_SECONDS",
            "0.05",
        )
    ),
)
STARTUP_CONNECT_ATTEMPTS = max(
    1, int(os.environ.get("ORCHESTRATOR_CONTROL_STARTUP_CONNECT_ATTEMPTS", "12"))
)
STARTUP_CONNECT_BACKOFF_SECONDS = max(
    0.1,
    float(
        os.environ.get(
            "ORCHESTRATOR_CONTROL_STARTUP_CONNECT_BACKOFF_SECONDS",
            "1.0",
        )
    ),
)
REGISTRATION_RECOVERY_BACKOFF_SECONDS = max(
    0.25,
    float(
        os.environ.get(
            "ORCHESTRATOR_CONTROL_REGISTRATION_RECOVERY_BACKOFF_SECONDS",
            "1.0",
        )
    ),
)
ORCH_GATEWAY_TLS_SERVER_NAME = (
    os.environ.get("ORCH_GATEWAY_TLS_SERVER_NAME", "").strip()
    or os.environ.get("NATS_TLS_SERVER_NAME", "").strip()
)


def _tls_context_for_url(url):
    return control_protocol.tls_context_for_url(url, ORCH_GATEWAY_TLS_SERVER_NAME)


def _tls_handshake_first_for_url(url):
    return control_protocol.tls_handshake_first_for_url(url)


def _subject(direction, hotkey, message_type):
    return control_protocol.subject(
        direction,
        hotkey,
        message_type,
        prefix=SUBJECT_PREFIX,
        environment=BEAM_ENV,
    )


def _nats_is_closed(client) -> bool:
    nc = client._nc
    if nc is None:
        return True
    value = getattr(nc, "is_closed", False)
    return bool(value() if callable(value) else value)

def _is_authorization_error(exc: Exception) -> bool:
    return "authorization" in str(exc).lower()

def _schedule_auth_recovery(client) -> None:
    if not client._running:
        return
    if client._api_key_source == "env":
        logger.error("BeamCore NATS auth rejected BEAMCORE_ORCHESTRATOR_API_KEY from environment")
        return
    if not client.signer:
        logger.error("BeamCore NATS auth rejected API key and no signer is available to mint a replacement")
        return
    if client._auth_recovery_task and not client._auth_recovery_task.done():
        return
    client._auth_recovery_task = asyncio.create_task(client._recover_from_nats_authorization_error())

async def _recover_from_nats_authorization_error(client) -> None:
    try:
        logger.warning("BeamCore NATS auth rejected cached API key; minting a fresh orchestrator API key")
        client._clear_cached_key()
        await client._recover_nats_connection("NATS authorization rejection", force=True)
    except Exception as exc:
        logger.warning("BeamCore NATS control auth recovery failed: %s", exc)
    finally:
        client._auth_recovery_task = None

async def _ensure_nats_connection(client, reason: str) -> None:
    if client._nc is not None and not client._nats_is_closed():
        return
    await client._recover_nats_connection(reason, force=True)

async def _request_retry_delay(client, attempt: int) -> None:
    delay = min(0.25, REQUEST_RETRY_BACKOFF_SECONDS * (2 ** max(0, attempt - 1)))
    if delay > 0:
        await asyncio.sleep(delay)

async def _ensure_api_key(client) -> Optional[str]:
    return await control_auth.ensure_api_key(client, os.environ)
async def start_polling(client):
    if client._running:
        logger.warning("Already running")
        return
    client._running = True
    last_error: Optional[Exception] = None
    for attempt in range(1, STARTUP_CONNECT_ATTEMPTS + 1):
        try:
            await client._ensure_http_registration()
            await client._connect_nats_session()
            registered = await client._register_via_nats()
            if not registered:
                raise RuntimeError("NATS control registration was rejected")
            client._heartbeat_task = asyncio.create_task(client._heartbeat_loop())
            logger.info("Started NATS control connection to %s", client.ws_base_url)
            return
        except Exception as exc:
            last_error = exc
            await client._close_nats_session()
            if attempt >= STARTUP_CONNECT_ATTEMPTS:
                client._running = False
                break
            client._beamcore_upstream_degraded = True
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

async def _ensure_http_registration(client) -> None:
    if not client._registration_config:
        raise RuntimeError("Cannot register with BeamCore: registration config not set")
    if not await client._ensure_api_key():
        raise RuntimeError("Cannot register with BeamCore without an orchestrator API key")

    config = client._registration_config
    payload = {
        "hotkey": client.orchestrator_hotkey,
        "url": config.get("url"),
        "region": config.get("region"),
        "max_workers": config.get("max_workers"),
        "uid": config.get("uid"),
        "fee_percentage": config.get("fee_percentage"),
    }
    http_client = await client._get_client()
    response = await http_client.post(
        f"{client.base_url}/orchestrators/register",
        json={key: value for key, value in payload.items() if value is not None},
    )
    if response.status_code != 200:
        raise RuntimeError(f"BeamCore HTTP registration failed with status {response.status_code}")

async def _close_nats_session(client, *, drain: bool = False) -> None:
    subscription = client._subscription
    nc = client._nc
    client._subscription = None
    client._nc = None
    client._registered = False
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

async def _connect_nats_session(client) -> None:
    last_error: Optional[Exception] = None

    async def connect_once(key: str):
        tls_context, tls_hostname = _tls_context_for_url(client.ws_base_url)
        return await nats.connect(
            servers=[client.ws_base_url],
            user=client.orchestrator_hotkey,
            password=key,
            name=f"beam-orchestrator-{client.orchestrator_hotkey[:12]}",
            tls=tls_context,
            tls_hostname=tls_hostname,
            tls_handshake_first=_tls_handshake_first_for_url(client.ws_base_url),
            max_reconnect_attempts=-1,
            reconnect_time_wait=1,
            disconnected_cb=client._on_nats_disconnected,
            reconnected_cb=client._on_nats_reconnected,
            error_cb=client._on_nats_error,
            closed_cb=client._on_nats_closed,
        )

    for attempt in range(1, STARTUP_CONNECT_ATTEMPTS + 1):
        try:
            api_key = await client._ensure_api_key()
            if not api_key:
                raise RuntimeError("Cannot connect to NATS control without an orchestrator API key")

            try:
                client._nc = await connect_once(api_key)
            except Exception as exc:
                if client._api_key_source == "env" and client._is_authorization_error(exc):
                    raise
                if client._api_key_source != "env" and client.signer and client._is_authorization_error(exc):
                    logger.warning("BeamCore NATS auth rejected cached API key; minting a fresh orchestrator API key")
                    client._clear_cached_key()
                    api_key = await client._ensure_api_key()
                    if not api_key:
                        raise RuntimeError("Cannot refresh NATS control orchestrator API key") from exc
                    client._nc = await connect_once(api_key)
                else:
                    raise

            client._subscription = await client._nc.subscribe(
                _subject("runtime", client.orchestrator_hotkey, "*"),
                cb=client._handle_runtime_message,
            )
            return
        except Exception as exc:
            last_error = exc
            if attempt >= STARTUP_CONNECT_ATTEMPTS:
                break
            client._beamcore_upstream_degraded = True
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

async def _on_nats_disconnected(client) -> None:
    client._registered = False
    client._beamcore_upstream_degraded = True
    BEAMCORE_UPSTREAM_DEGRADED.set(1)
    logger.warning("BeamCore NATS control disconnected")

async def _on_nats_reconnected(client) -> None:
    logger.info("BeamCore NATS control reconnected")
    if not client._running:
        return
    try:
        registered = await client._register_via_nats()
        if not registered:
            raise RuntimeError("NATS control registration was rejected")
        client._schedule_ready_sync_if_needed()
        client._note_beamcore_upstream_recovered("NATS reconnect")
    except Exception as exc:
        logger.warning("BeamCore NATS control re-registration failed after reconnect: %s", exc)
        client._registered = False
        client._schedule_registration_recovery("NATS reconnect")

async def _on_nats_error(client, exc: Exception) -> None:
    logger.warning("BeamCore NATS control error: %s", exc)
    if client._is_authorization_error(exc):
        client._schedule_auth_recovery()

async def _on_nats_closed(client) -> None:
    logger.warning("BeamCore NATS control closed")

async def _recover_nats_connection(client, reason: str, *, force: bool = False) -> None:
    if not client._running:
        return
    async with client._connect_lock:
        if client._nc is not None and not force and not client._nats_is_closed():
            return
        old_nc = client._nc
        client._nc = None
        client._subscription = None
        client._registered = False
        if old_nc:
            try:
                close_result = old_nc.close()
                if inspect.isawaitable(close_result):
                    await close_result
            except Exception:
                pass
        await client._connect_nats_session()
    registered = await client._register_via_nats()
    if not registered:
        raise RuntimeError("NATS control registration was rejected")
    client._schedule_ready_sync_if_needed()
    logger.info("Recovered BeamCore NATS control connection after %s", reason)

def _schedule_registration_recovery(client, reason: str) -> None:
    if not client._running:
        return
    if client._registration_recovery_task and not client._registration_recovery_task.done():
        return
    client._registration_recovery_task = asyncio.create_task(client._recover_registration_loop(reason))

async def _recover_registration_loop(client, reason: str) -> None:
    attempt = 0
    try:
        while client._running and not client._registered:
            attempt += 1
            try:
                if client._nc is None or client._nats_is_closed():
                    await client._recover_nats_connection(f"{reason}_registration_retry", force=True)
                else:
                    registered = await client._register_via_nats()
                    if not registered:
                        raise RuntimeError("NATS control registration was rejected")
                client._schedule_ready_sync_if_needed()
                client._note_beamcore_upstream_recovered(f"{reason} registration retry")
                return
            except Exception as exc:
                delay = min(30.0, REGISTRATION_RECOVERY_BACKOFF_SECONDS * (2 ** min(attempt - 1, 5)))
                logger.warning(
                    "BeamCore NATS control registration retry %s after %s failed: %s; retrying in %.1fs",
                    attempt,
                    reason,
                    exc,
                    delay,
                )
                await asyncio.sleep(delay)
    except asyncio.CancelledError:
        raise
    finally:
        client._registration_recovery_task = None
async def stop_polling(client):
    client._running = False
    if client._heartbeat_task:
        client._heartbeat_task.cancel()
        try:
            await client._heartbeat_task
        except asyncio.CancelledError:
            pass
        client._heartbeat_task = None
    if client._ready_sync_task:
        client._ready_sync_task.cancel()
        try:
            await client._ready_sync_task
        except asyncio.CancelledError:
            pass
        client._ready_sync_task = None
    if client._auth_recovery_task:
        client._auth_recovery_task.cancel()
        try:
            await client._auth_recovery_task
        except asyncio.CancelledError:
            pass
        client._auth_recovery_task = None
    if client._registration_recovery_task:
        client._registration_recovery_task.cancel()
        try:
            await client._registration_recovery_task
        except asyncio.CancelledError:
            pass
        client._registration_recovery_task = None
    if client._task_offer_dispatcher:
        await client._task_offer_dispatcher.stop()
    if client._subscription:
        await client._subscription.unsubscribe()
        client._subscription = None
    if client._nc:
        await client._nc.drain()
        client._nc = None
    client._registered = False
    logger.info("NATS control connection stopped")

def set_registration_config(client, url: str, region: str, max_workers: int = 10000, uid: int = None, fee_percentage: float = 0.0, gateway_url: Optional[str] = None):
    client._registration_config = {
        "url": url,
        "region": region,
        "max_workers": max_workers,
        "uid": uid,
        "fee_percentage": fee_percentage,
        "gateway_url": gateway_url,
    }
    logger.info("Registration config set: region=%s, max_workers=%s", region, max_workers)

async def _register_via_nats(client) -> bool:
    if not client._registration_config:
        logger.warning("Cannot register via NATS: registration config not set")
        return False
    cfg = dict(client._registration_config)
    reg_message = f"{client.orchestrator_hotkey}:{cfg.get('url') or ''}:{cfg.get('region') or ''}"
    signature = ""
    if client.signer:
        try:
            sig_bytes = client.signer.sign(reg_message.encode())
            signature = "0x" + (sig_bytes.hex() if isinstance(sig_bytes, bytes) else str(sig_bytes))
        except Exception as e:
            logger.warning("Failed to sign registration: %s", e)
    client._desired_ready = client._registration_ready()
    cfg["ready"] = client._desired_ready
    cfg["signature"] = signature
    response = await client._send_nats_request("register", cfg, timeout=max(REQUEST_TIMEOUT, 3.0))
    if response.get("type") == "register_ack":
        client._registered = True
        logger.info("Registered via NATS control: status=%s", response.get("status"))
        client._schedule_ready_sync_if_needed()
        if client._worker_gateway and hasattr(client._worker_gateway, "refresh_capabilities"):
            client._worker_gateway.refresh_capabilities()
        return True
    client._registered = False
    logger.error("Registration failed: %s", response.get("message") or response.get("reason") or response)
    return False
