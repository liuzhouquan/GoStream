"""Read deployed identities, never container environment/configuration values."""
import json
import re
import subprocess
from urllib.parse import urlsplit

from launcher_lifecycle import LauncherInterrupted


def inspect_api_deployment(project, urls):
    """Read-only identities at measurement start; unavailable is explicitly unverified."""
    if not project:
        return {"verified": False, "reason": "no owned Compose project supplied"}
    def run(args):
        return subprocess.check_output(args, text=True, stderr=subprocess.DEVNULL, timeout=15).strip()
    try:
        ids = run(["docker", "ps", "-q", "--filter", f"label=com.docker.compose.project={project}",
                   "--filter", "label=com.docker.compose.service=api"]).split()
        if len(ids) != len(urls):
            raise ValueError("API container count does not match measured endpoints")
        # Select only non-secret fields. docker inspect's full Config contains credentials.
        template = '{"id":{{json .Id}},"image":{{json .Image}},"ports":{{json .NetworkSettings.Ports}}}'
        records = [json.loads(run(["docker", "inspect", "--format", template, cid])) for cid in ids]
        identities = []
        for url in urls:
            endpoint = urlsplit(url)
            if endpoint.hostname not in ("127.0.0.1", "localhost") or endpoint.scheme != "http":
                raise ValueError("only local published API endpoints can be associated")
            matching = [r for r in records if any(
                binding["HostPort"] == str(endpoint.port or 80)
                and binding["HostIp"] in ("127.0.0.1", "0.0.0.0", "")
                for binding in (r["ports"] or {}).get("8080/tcp", []) or [])]
            if len(matching) != 1:
                raise ValueError("measured API port does not identify exactly one container")
            record = matching[0]
            if not re.fullmatch(r"sha256:[a-f0-9]{64}", record["image"]):
                raise ValueError("missing immutable image ID")
            # Hash the actual PID 1 executable, not an arbitrary checkout or on-disk replacement.
            digest = run(["docker", "exec", record["id"], "sha256sum", "/proc/1/exe"]).split()[0]
            if not re.fullmatch(r"[a-f0-9]{64}", digest):
                raise ValueError("invalid executable digest")
            identities.append({"api": url, "container_id": record["id"],
                               "image_id": record["image"], "binary_sha256": digest})
        if len({item["container_id"] for item in identities}) != len(urls):
            raise ValueError("measurement endpoints must identify distinct API containers")
        return {"verified": True, "scope": "deployed identities at measurement start; not proof of checkout equivalence",
                "instances": identities}
    except LauncherInterrupted:
        raise
    except (OSError, subprocess.SubprocessError, ValueError, KeyError, IndexError, TypeError) as error:
        return {"verified": False, "reason": type(error).__name__}
