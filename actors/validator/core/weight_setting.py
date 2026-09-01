"""Chain weight publication using BeamCore's persisted vectors."""

import logging
from datetime import datetime
from typing import List, Optional, Tuple

logger = logging.getLogger(__name__)


async def _maybe_set_weights(validator) -> None:
    """Set weights on chain if enough blocks have passed"""
    if validator.settings.disable_weight_set:
        return

    if validator.subtensor is None:
        logger.debug("Skipping weight setting - no subtensor")
        return

    current_block = validator.subtensor.block
    blocks_since = current_block - validator.last_weight_block

    def _fmt_wait(blocks: int) -> str:
        secs = blocks * 12
        return f"~{secs // 60}m {secs % 60}s" if secs >= 60 else f"~{secs}s"

    # Chain uses strict `blocks_since > rate_limit`, so we must wait for blocks_since >= rate_limit + 1
    effective_limit = max(validator.settings.blocks_between_weights, validator._chain_weights_rate_limit)
    if blocks_since <= effective_limit:
        blocks_remaining = effective_limit - blocks_since + 1
        logger.info("Next weight set window in %s (%d blocks)", _fmt_wait(blocks_remaining), blocks_remaining)
        return

    await validator._set_weights()

async def _set_weights(validator, subnet_core_available: bool) -> None:
    """Set weights using BeamCore's persisted epoch snapshot."""
    if not subnet_core_available or not validator.subnet_core_client:
        logger.warning("BeamCore client not available, skipping weight setting")
        return

    weight_snapshot = await validator._get_persisted_weight_snapshot()
    if not weight_snapshot:
        logger.warning("No persisted BeamCore weight snapshot available")
        return

    uids, weights, formula_version, params_hash, data_epoch, _no_weight_period, _burn_reason = weight_snapshot

    # Set weights on chain
    try:
        # Check if commit-reveal is enabled on this subnet
        commit_reveal_enabled = False
        if validator.subtensor is not None:
            try:
                cr_result = validator.subtensor.query_module(
                    "SubtensorModule", "CommitRevealWeightsEnabled", [validator.settings.netuid]
                )
                commit_reveal_enabled = bool(cr_result.value) if cr_result else False
            except Exception as e:
                logger.debug(f"Could not check commit-reveal status: {e}")

        if commit_reveal_enabled and validator.subtensor is not None:
            result = validator.subtensor.set_weights(
                wallet=validator.wallet,
                netuid=validator.settings.netuid,
                uids=list(uids),
                weights=list(weights),
                wait_for_inclusion=True,
                wait_for_finalization=False,
                wait_for_revealed_execution=True,
            )
            success = result.success
            message = result.message or result.error or ""
            weight_method = "timelocked_commit_reveal"
        elif validator.fiber_chain is not None and validator.uid is not None:
            success, message = validator.fiber_chain.set_weights(
                keypair=validator.wallet,  # Pass full wallet, not just hotkey
                validator_uid=validator.uid,
                uids=uids,
                weights=weights,
                wait_for_inclusion=True,
                wait_for_finalization=False,
            )
            weight_method = "Fiber"
        elif validator.subtensor is not None:
            success, message = validator.subtensor.set_weights(
                wallet=validator.wallet,
                netuid=validator.settings.netuid,
                uids=uids,
                weights=weights,
                wait_for_inclusion=True,
                wait_for_finalization=False,
            )
            weight_method = "bittensor"
        else:
            logger.warning("No method available to set weights")
            return

        if success:
            validator.last_weight_block = validator.subtensor.block if validator.subtensor else 0
            _kw, _uw, _ww = 9, 4, 10
            _info_items = [
                ("Block",   str(validator.last_weight_block)),
                ("Epoch",   str(data_epoch)),
                ("Method",  weight_method),
                ("Formula", formula_version),
            ]
            if _no_weight_period:
                _info_items.append(("Period", f"BURN — {_burn_reason}"))
            _vw = max(len(v) for _, v in _info_items)
            _hw = _kw + _vw - _uw - _ww - 3  # derived: header_outer == uid_outer
            _hw = max(_hw, 20)                # ensure hotkey col is readable
            _vw = _uw + _hw + _ww + 3 - _kw  # recompute in case _hw was clamped
            _hotkeys = getattr(validator.metagraph, "hotkeys", []) if validator.metagraph else []
            _sorted_w = sorted(zip(uids, weights), key=lambda x: -x[1])
            _uid_rows = []
            for _uid, _w in _sorted_w:
                if _uid == 0 and _w > 0:
                    _hk = "(burn)"
                elif 0 <= _uid < len(_hotkeys):
                    _hk = (_hotkeys[_uid][:_hw - 3] + "...") if len(_hotkeys[_uid]) > _hw else _hotkeys[_uid]
                else:
                    _hk = "unknown"
                _uid_rows.append(f"│ {_uid:>{_uw}} │ {_hk:<{_hw}} │ {_w:>{_ww}.4f} │")
            _inner = _kw + _vw + 5
            _top    = f"┌{'─' * _inner}┐"
            _title  = f"│{'Weight Set Successfully':^{_inner}}│"
            _hsep1  = f"├{'─' * (_kw + 2)}┬{'─' * (_vw + 2)}┤"
            _info   = "\n".join(f"│ {k:<{_kw}} │ {v:<{_vw}} │" for k, v in _info_items)
            _hsep2  = f"├{'─' * (_uw + 2)}┬{'─' * (_hw + 2)}┬{'─' * (_ww + 2)}┤"
            _uidhdr = f"│ {'UID':>{_uw}} │ {'Hotkey':<{_hw}} │ {'Weight':>{_ww}} │"
            _hsep3  = f"├{'─' * (_uw + 2)}┼{'─' * (_hw + 2)}┼{'─' * (_ww + 2)}┤"
            _bot    = f"└{'─' * (_uw + 2)}┴{'─' * (_hw + 2)}┴{'─' * (_ww + 2)}┘"
            print("\n".join([_top, _title, _hsep1, _info, _hsep2, _uidhdr, _hsep3] + _uid_rows + [_bot]), flush=True)

            validator.weights_history.append(
                {
                    "block": validator.last_weight_block,
                    "timestamp": datetime.utcnow().isoformat(),
                    "weights": {uid: round(w, 6) for uid, w in zip(uids, weights)},
                    "weight_method": weight_method,
                    "formula_version": formula_version,
                }
            )

            if len(validator.weights_history) > 100:
                validator.weights_history = validator.weights_history[-100:]

            if validator.subnet_core_client:
                try:
                    await validator.subnet_core_client.submit_weight_proof(
                        epoch=data_epoch,
                        block_number=validator.last_weight_block,
                        netuid=validator.settings.netuid,
                        uids=list(uids),
                        weights=list(weights),
                        formula_version=formula_version,
                        params_hash=params_hash,
                    )
                    logger.debug("Weight proof submitted to BeamCore")
                except Exception as _e:
                    _body = getattr(getattr(_e, "response", None), "text", None)
                    logger.warning("Failed to submit weight proof: %s%s", _e, f" — {_body}" if _body else "")

        else:
            _cur_block = validator.subtensor.block if validator.subtensor else "?"
            _msg = message or "(empty — likely chain rate limit or rejection)"
            logger.error(
                "Failed to set weights: method=%s block=%s last_weight_block=%s message=%r",
                weight_method, _cur_block, validator.last_weight_block, _msg,
            )
            # Back off: treat this attempt as the new baseline so we don't
            # retry every block after a chain rejection.
            validator.last_weight_block = validator.subtensor.block if validator.subtensor else validator.last_weight_block

    except Exception as e:
        logger.error(f"Error setting weights: {e}", exc_info=True)

async def _get_persisted_weight_snapshot(
    validator,
) -> Optional[Tuple[List[int], List[float], str, Optional[str], int]]:
    """Fetch recommended weights from BeamCore epoch summary (ops-materialized)."""
    if not validator.subnet_core_client:
        return None
    try:
        snapshot = await validator.subnet_core_client.get_latest_epoch_summary()
    except Exception as exc:
        logger.warning("BeamCore epoch summary unavailable: %s", exc)
        return None

    uids = snapshot.get("uids") or []
    weights = snapshot.get("weights") or []
    if not uids or len(uids) != len(weights):
        logger.warning("BeamCore epoch summary missing uids/weights vectors")
        return None
    fv = str(snapshot.get("formula_version") or "prism_final_x_task_done_count")
    ph = snapshot.get("params_hash")
    if isinstance(ph, str):
        params_hash: Optional[str] = ph
    else:
        params_hash = None
    data_epoch = int(snapshot.get("epoch", validator.current_epoch))

    _no_weight_period = bool(snapshot.get("no_weight_period"))
    _burn_reason = snapshot.get("reason", "") if _no_weight_period else ""
    return (list(uids), list(weights), fv, params_hash, data_epoch, _no_weight_period, _burn_reason)
