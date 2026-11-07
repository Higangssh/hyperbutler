"""Testcontainers for Python's basic sample: nginx with a wait strategy, reached through
the mapped port from the host. With COMPAT_LEAK=1 it also leaves a container running, for
run.ps1 to see Ryuk remove it after this process ends."""

import os
import urllib.request

from testcontainers.core.container import DockerContainer
from testcontainers.core.wait_strategies import HttpWaitStrategy

nginx = (
    DockerContainer("nginx:1.27-alpine")
    .with_exposed_ports(80)
    .waiting_for(HttpWaitStrategy(80, "/").with_startup_timeout(120))
)
with nginx:
    host = nginx.get_container_host_ip()
    port = nginx.get_exposed_port(80)
    url = f"http://{host}:{port}/"
    print("nginx at", url)
    with urllib.request.urlopen(url, timeout=30) as resp:
        body = resp.read().decode()
    assert resp.status == 200 and "nginx" in body, (resp.status, body[:200])

    if os.environ.get("COMPAT_LEAK") == "1":
        DockerContainer("alpine:3.20").with_command("sleep 600").start()
        # On a clean exit the library's own atexit hook stops Ryuk before it reaps
        # anything. Ryuk is for a process that dies, so die: only the socket closes.
        print("ok", flush=True)
        os._exit(0)
print("ok")
