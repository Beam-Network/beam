"""NATS envelopes, runtime messages, offers, results, and readiness."""

import asyncio
import logging
import os
import time
import uuid
from typing import Any, Dict, Optional

from clients import control_protocol

logger = logging.getLogger(__name__)

SCHEMA_VERSION = "orchestrator-control/v1"
SUBJECT_PREFIX = os.environ.get(
    "ORCHESTRATOR_CONTROL_SUBJECT_PREFIX", "beam.orch.control"
).strip(".")
BEAM_ENV = os.environ.get("BEAM_ENV", "dev")
REQUEST_TIMEOUT = float(
    os.environ.get("ORCHESTRATOR_CONTROL_REQUEST_TIMEOUT_SECONDS", "0.75")
)
TASK_RESULT_TIMEOUT = float(
    os.environ.get("ORCHESTRATOR_CONTROL_TASK_RESULT_TIMEOUT_SECONDS", "15.0")
)
REQUEST_RETRY_ATTEMPTS = max(
    1, int(os.environ.get("ORCHESTRATOR_CONTROL_REQUEST_RETRY_ATTEMPTS", "3"))
)
HEARTBEAT_INTERVAL = float(
    os.environ.get("ORCHESTRATOR_CONTROL_HEARTBEAT_INTERVAL_SECONDS", "5")
)


def _pack(envelope):
    return control_protocol.pack(envelope)


def _unpack(data):
    return control_protocol.unpack(data)


def _subject(direction, hotkey, message_type):
    return control_protocol.subject(
        direction,
        hotkey,
        message_type,
        prefix=SUBJECT_PREFIX,
        environment=BEAM_ENV,
    )


async def _heartbeat_loop(client) -> None:
    while client._running:
        try:
            await client._send_nats_publish("heartbeat", {})
            if client._worker_gateway and hasattr(client._worker_gateway, "refresh_capabilities"):
                client._worker_gateway.refresh_capabilities()
        except Exception as exc:
            logger.debug("heartbeat publish failed: %s", exc)
        await asyncio.sleep(HEARTBEAT_INTERVAL)

def _envelope(client, message_type: str, payload: dict[str, Any], request_id: Optional[str] = None) -> dict[str, Any]:
    envelope = {
        "message_id": f"orchestrator-control/v1:{BEAM_ENV}:{client.orchestrator_hotkey}:{message_type}:{request_id or uuid.uuid4().hex}",
        "schema_version": SCHEMA_VERSION,
        "environment": BEAM_ENV,
        "hotkey": client.orchestrator_hotkey,
        "message_type": message_type,
        "occurred_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "producer": "orchestrator",
        "payload": payload,
    }
    if request_id is not None:
        envelope["request_id"] = request_id
    return envelope

async def _send_nats_publish(client, message_type: str, payload: dict[str, Any]) -> None:
    await client._ensure_nats_connection(f"{message_type}_publish_disconnected")
    if not client._nc:
        raise RuntimeError("NATS control is not connected")
    payload_bytes = _pack(client._envelope(message_type, payload))
    subject = _subject("orch", client.orchestrator_hotkey, message_type)
    try:
        await client._nc.publish(subject, payload_bytes)
    except Exception:
        if client._nats_is_closed():
            await client._recover_nats_connection(f"{message_type}_publish_closed", force=True)
            if not client._nc:
                raise RuntimeError("NATS control is not connected")
            await client._nc.publish(subject, payload_bytes)
            return
        raise

async def _send_nats_request(
    client,
    message_type: str,
    payload: dict[str, Any],
    timeout: float = REQUEST_TIMEOUT,
    attempts: int = REQUEST_RETRY_ATTEMPTS,
) -> dict[str, Any]:
    request_id = payload.get("request_id") or uuid.uuid4().hex
    request_payload = _pack(client._envelope(message_type, {**payload, "request_id": request_id}, request_id))
    subject = _subject("orch", client.orchestrator_hotkey, message_type)
    last_error: Exception | None = None
    request_attempts = max(1, attempts)
    for attempt in range(request_attempts):
        await client._ensure_nats_connection(f"{message_type}_request_disconnected")
        if not client._nc:
            raise RuntimeError("NATS control is not connected")
        try:
            msg = await client._nc.request(subject, request_payload, timeout=timeout)
            envelope = _unpack(msg.data)
            response = envelope.get("payload") or {}
            if not isinstance(response, dict):
                raise RuntimeError("invalid BeamCore response payload")
            if response.get("type") == "error":
                reason = response.get("reason") or response.get("message") or "NATS control request failed"
                if reason == "orchestrator_not_registered" and message_type != "register":
                    client._registered = False
                    try:
                        registered = await client._register_via_nats()
                        if not registered:
                            client._schedule_registration_recovery(reason)
                    except Exception:
                        client._schedule_registration_recovery(reason)
                    continue
                raise RuntimeError(reason)
            return response
        except Exception as exc:
            last_error = exc
            if client._nats_is_closed():
                await client._recover_nats_connection(f"{message_type}_request_closed", force=True)
            if attempt < request_attempts - 1:
                await client._request_retry_delay(attempt)
    raise RuntimeError(f"NATS control request failed: {last_error}") from last_error
async def _handle_runtime_message(client, msg) -> None:
    try:
        envelope = _unpack(msg.data)
        payload = envelope.get("payload") or {}
        if not isinstance(payload, dict):
            raise ValueError("payload must be an object")
        msg_type = envelope.get("message_type") or payload.get("type")
        if msg_type == "worker_task_offer_batch":
            client._note_beamcore_upstream_recovered("worker_task_offer_batch from BeamCore")
            await client._handle_task_offer_batch(payload)
        elif msg_type == "error":
            logger.warning("BeamCore NATS control error: %s", payload)
        else:
            logger.debug("Unknown NATS control message type: %s", msg_type)
            if msg.reply:
                await msg.respond(_pack(client._envelope("error", {"type": "error", "reason": "unknown_message_type"}, envelope.get("request_id"))))
    except Exception as exc:
        logger.error("Error handling NATS control message: %s", exc)
        if msg.reply:
            await msg.respond(_pack(client._envelope("error", {"type": "error", "reason": "handler_error"})))

async def _handle_task_offer_batch(client, data: dict) -> None:
    batch_id = data.get("batch_id")
    offers = data.get("offers") or []
    if not isinstance(offers, list) or not offers:
        logger.warning("worker_task_offer_batch missing offers: batch=%s", batch_id)
        return
    if not client._worker_gateway:
        logger.warning("No local worker gateway available for batch %s", batch_id)
        return
    if client._task_offer_dispatcher:
        await client._task_offer_dispatcher.start()
        queued = client._task_offer_dispatcher.enqueue_offer(data)
        if queued:
            logger.info("worker_task_offer_batch queued for local workers: batch=%s offers=%s", batch_id, len(offers))
        return
    await client._deliver_task_offer_batch_to_workers(data)

async def _deliver_task_offer_batch_to_workers(client, data: dict) -> None:
    batch_id = data.get("batch_id")
    offers = data.get("offers") or []
    if not isinstance(offers, list) or not offers or not client._worker_gateway:
        return
    delivered = 0
    for offer in offers:
        if not isinstance(offer, dict):
            continue
        workers = client._worker_gateway.get_workers_round_robin(1)
        if not workers:
            logger.warning("No connected local workers for batch %s", batch_id)
            break
        worker_id = workers[0]
        if await client._worker_gateway.deliver_task_offer(worker_id, offer):
            delivered += 1
        else:
            logger.warning("Failed to forward task offer: batch=%s worker=%s task=%s", batch_id, worker_id, offer.get("task_id"))
    logger.info("worker_task_offer_batch delivered locally: batch=%s offers=%s delivered=%s", batch_id, len(offers), delivered)


def _schedule_ready_sync_if_needed(client) -> None:
    if not client._running or not client._registered:
        return
    if client._last_confirmed_ready == client._desired_ready:
        return
    if client._ready_sync_task and not client._ready_sync_task.done():
        return
    client._ready_sync_task = asyncio.create_task(client._sync_ready_state_in_background())

async def _sync_ready_state_in_background(client) -> None:
    try:
        while client._running and client._registered and client._last_confirmed_ready != client._desired_ready:
            try:
                applied = await client._apply_desired_ready_state()
                if applied:
                    return
            except Exception as exc:
                logger.warning("Failed to sync queued ready=%s through NATS control: %s", client._desired_ready, exc)
            await asyncio.sleep(5.0)
    except asyncio.CancelledError:
        raise
    finally:
        client._ready_sync_task = None

async def _apply_desired_ready_state(client) -> bool:
    requested_ready = client._desired_ready
    response = await client._send_nats_request("set_ready", {"ready": requested_ready})
    confirmed = bool(response.get("ready", requested_ready))
    client._desired_ready = confirmed
    client._last_confirmed_ready = confirmed
    logger.info("Orchestrator ready=%s set on BeamCore (uid=%s)", confirmed, response.get("uid"))
    return confirmed == requested_ready
async def send_task_result_strict(client, payload: dict) -> Dict[str, Any]:
    task_id = payload.get("task_id")
    offer_id = payload.get("offer_id") or task_id
    if not task_id or not offer_id:
        return {"type": "task_result_ack", "task_id": task_id, "offer_id": offer_id, "received": False, "status": "rejected", "reason": "missing_task_or_offer_id"}
    message = {"task_id": task_id, "offer_id": offer_id, "worker_id": payload.get("worker_id"), "success": bool(payload.get("success"))}
    for key in ("bytes_transferred", "etag", "chunk_hash", "error"):
        if payload.get(key) is not None:
            message[key] = payload[key]
    return await client._send_nats_request("task_result", message, timeout=max(REQUEST_TIMEOUT, TASK_RESULT_TIMEOUT))

async def send_task_result(client, payload: dict) -> Dict[str, Any]:
    task_id = payload.get("task_id")
    offer_id = payload.get("offer_id") or task_id
    try:
        return await client.send_task_result_strict(payload)
    except Exception as exc:
        logger.warning("send_task_result send error: %s", exc)
        return {"type": "task_result_ack", "task_id": task_id, "offer_id": offer_id, "received": False, "status": "retry", "reason": "beamcore_result_forward_failed"}

async def send_capability_update(client, manifest: dict) -> Dict[str, Any]:
    if not client._registered:
        return {"type": "capability_update_ack", "accepted": False, "reason": "orchestrator_not_registered"}
    return await client._send_nats_request("capability_update", manifest, timeout=max(REQUEST_TIMEOUT, 3.0))

async def update_worker_gateway(client, gateway_url: str, max_workers: int = 10000, health: str = "healthy") -> Dict[str, Any]:
    return await client._send_nats_request("gateway_update", {"gateway_url": gateway_url, "max_workers": max_workers, "health": health})

async def sync_ready_if_eligible(client) -> bool:
    client._desired_ready = client._registration_ready()
    if not client._registered:
        logger.info("Ready intent recorded; NATS control registration is not complete")
        return False
    if not client._desired_ready:
        if client._last_confirmed_ready is not False:
            try:
                await client._apply_desired_ready_state()
            except Exception as exc:
                client._schedule_ready_sync_if_needed()
                logger.info("Queued ready=False after transient NATS control sync failure: %s", exc)
        return False
    return await client._apply_desired_ready_state()

async def set_ready(client, ready: bool) -> bool:
    client._operator_ready = ready
    client._desired_ready = client._registration_ready()
    if not client._registered:
        logger.info("Queued ready=%s until NATS control registration completes", client._desired_ready)
        return False
    try:
        return await client._apply_desired_ready_state()
    except Exception as exc:
        client._schedule_ready_sync_if_needed()
        logger.info("Queued ready=%s after transient NATS control sync failure: %s", client._desired_ready, exc)
        return False
