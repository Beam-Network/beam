"""Command-line and environment configuration for the Beam worker."""

import argparse
import os


def get_config(bt_module):
    """Build the historical Bittensor-backed worker configuration."""
    parser = argparse.ArgumentParser(description="Beam Network Worker")
    bt_module.Wallet.add_args(parser)
    bt_module.Subtensor.add_args(parser)
    return bt_module.Config(parser)


def apply_env_config_overrides(config, environ=None) -> None:
    """Apply deployment environment overrides after Bittensor defaults."""
    environment = environ if environ is not None else os.environ
    overrides = (
        ("WALLET_NAME", "wallet", "name"),
        ("WALLET_HOTKEY", "wallet", "hotkey"),
        ("WALLET_PATH", "wallet", "path"),
        ("SUBTENSOR_NETWORK", "subtensor", "network"),
    )
    for env_name, section_name, key in overrides:
        value = environment.get(env_name)
        if not value:
            continue
        section = getattr(config, section_name, None)
        if section is None:
            section = {}
            setattr(config, section_name, section)
        if isinstance(section, dict):
            section[key] = value
        else:
            setattr(section, key, value)
