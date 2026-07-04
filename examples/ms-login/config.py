#!/usr/bin/env python3
"""
Configuration management for ms-login.

Loads credentials from a JSON file (default ~/.config/ms_login/credentials.json
or path passed via MS_LOGIN_CONFIG env var).

Schema:
{
  "phone": "138xxxxxxxx",
  "samvnc_base_url": "https://aitun.cc/GMCLNGFZ",
  "vnc_url": "https://aitun.cc/GMCLNGFZ/",
  "service_host": "127.0.0.1",
  "service_port": 8765
}

`phone` is the only required field. The samvnc_base_url defaults to the
aitun relay currently configured on the HPC node.
"""
from __future__ import annotations

import json
import os
from dataclasses import asdict, dataclass, field
from pathlib import Path
from typing import Optional

DEFAULT_CONFIG_PATHS = [
    Path.home() / ".config" / "ms_login" / "credentials.json",
    Path("/home/z/my-project/.config/ms_login/credentials.json"),
    Path("/etc/ms_login/credentials.json"),
]

# Default samvnc base URL — the public aitun relay that currently maps to
# the HPC node's port 8128 (samvnc-server inside proot).
DEFAULT_SAMVNC_BASE_URL = "https://aitun.cc/GMCLNGFZ"


@dataclass
class Config:
    phone: str = ""
    samvnc_base_url: str = DEFAULT_SAMVNC_BASE_URL
    vnc_url: str = ""  # defaults to samvnc_base_url + "/"
    service_host: str = "127.0.0.1"
    service_port: int = 8765
    extra: dict = field(default_factory=dict)

    def __post_init__(self):
        if not self.vnc_url:
            self.vnc_url = self.samvnc_base_url.rstrip("/") + "/"

    def to_dict(self) -> dict:
        d = asdict(self)
        # Mask phone partially for display
        if d.get("phone"):
            p = d["phone"]
            d["phone_masked"] = p[:3] + "*" * max(0, len(p) - 7) + p[-4:] if len(p) >= 7 else "***"
        return d

    def to_masked_dict(self) -> dict:
        """Return config with phone masked (for /config GET endpoint)."""
        d = self.to_dict()
        if d.get("phone"):
            p = d["phone"]
            d["phone"] = p[:3] + "*" * max(0, len(p) - 7) + p[-4:] if len(p) >= 7 else "***"
        return d

    def save(self, path: Path) -> None:
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(json.dumps(self.to_dict(), indent=2, ensure_ascii=False))
        try:
            os.chmod(path, 0o600)
        except OSError:
            pass


def find_config_path() -> Optional[Path]:
    """Find the first existing config path."""
    env_path = os.environ.get("MS_LOGIN_CONFIG")
    if env_path:
        p = Path(env_path)
        if p.exists():
            return p
    for p in DEFAULT_CONFIG_PATHS:
        if p.exists():
            return p
    return None


def default_config_path() -> Path:
    """Return the default path to write a new config to."""
    env_path = os.environ.get("MS_LOGIN_CONFIG")
    if env_path:
        return Path(env_path)
    return DEFAULT_CONFIG_PATHS[0]


def load_config() -> Config:
    """Load config from the first existing path. Returns empty Config if none."""
    p = find_config_path()
    if p is None:
        return Config()
    try:
        data = json.loads(p.read_text())
    except Exception:
        return Config()
    # Pull known fields, dump the rest into `extra`
    known = {f for f in Config.__dataclass_fields__} - {"extra"}
    kwargs = {}
    extra = {}
    for k, v in data.items():
        if k in known:
            kwargs[k] = v
        elif k != "phone_masked":
            extra[k] = v
    kwargs["extra"] = extra
    try:
        return Config(**kwargs)
    except TypeError:
        return Config()


def save_config(cfg: Config) -> Path:
    """Save config to the default path."""
    p = default_config_path()
    cfg.save(p)
    return p
