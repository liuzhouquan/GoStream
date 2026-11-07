"""Resolve the shared external acceptance-stack target without unsafe defaults."""

import os
from urllib.parse import urlsplit


TARGET_ENV_NAMES = (
    "GOSTREAM_API_A", "GOSTREAM_API_B", "GOSTREAM_BASE_URL", "GOSTREAM_COMPOSE_ACTIVE",
    "API_A", "API_B", "BASE",
)


def has_external_acceptance_target(environ=None):
    env = os.environ if environ is None else environ
    return any(env.get(name) for name in TARGET_ENV_NAMES)


def _origin(value):
    parsed = urlsplit(value)
    if (parsed.scheme not in ("http", "https") or not parsed.hostname
            or parsed.username or parsed.password or parsed.query or parsed.fragment
            or parsed.path not in ("", "/")):
        raise ValueError("acceptance API endpoints must be explicit HTTP(S) origins without credentials")
    try:
        port = parsed.port
    except ValueError as error:
        raise ValueError("invalid acceptance API port") from error
    host = parsed.hostname.lower()
    if ":" in host:
        host = "[" + host + "]"
    default_port = 80 if parsed.scheme == "http" else 443
    suffix = "" if port is None or port == default_port else ":" + str(port)
    return parsed.scheme + "://" + host + suffix


def resolve_acceptance_target(mode, environ=None):
    env = os.environ if environ is None else environ
    external = has_external_acceptance_target(env)
    if mode == "sqlite" and external:
        raise ValueError("SQLite mode conflicts with an external acceptance-stack environment")
    if mode == "auto":
        mode = "compose" if external else "sqlite"
    if mode == "sqlite":
        return mode, None, None
    if mode != "compose":
        raise ValueError("unknown acceptance mode")
    api_a = (env.get("GOSTREAM_API_A") or env.get("GOSTREAM_BASE_URL")
             or env.get("API_A") or env.get("BASE"))
    api_b = env.get("GOSTREAM_API_B") or env.get("API_B")
    if not api_a or not api_b:
        raise ValueError("Compose acceptance requires two explicit API endpoints (GOSTREAM_API_A and GOSTREAM_API_B)")
    api_a, api_b = _origin(api_a), _origin(api_b)
    if api_a == api_b:
        raise ValueError("Compose acceptance requires two distinct API endpoints")
    return mode, api_a, api_b
