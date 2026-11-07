"""Docker SDK for Python's basic sample: ping, pull, run, logs, remove."""

import docker

IMAGE = "alpine:3.20"
WORD = "hello-from-docker-py"

client = docker.from_env()
assert client.ping(), "ping"
print("API", client.version()["ApiVersion"])
client.images.pull(IMAGE)
container = client.containers.run(IMAGE, ["echo", WORD], detach=True)
try:
    status = container.wait(timeout=120)
    assert status["StatusCode"] == 0, status
    logs = container.logs().decode()
    assert WORD in logs, logs
finally:
    container.remove(force=True)
print("ok")
