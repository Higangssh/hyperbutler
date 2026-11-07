// Testcontainers for Node's basic sample: nginx with a wait strategy, reached through the
// mapped port from the host. With COMPAT_LEAK=1 it also leaves a container running, for
// run.ps1 to see Ryuk remove it after this process ends.
import { GenericContainer, Wait } from "testcontainers";

const nginx = await new GenericContainer("nginx:1.27-alpine")
  .withExposedPorts(80)
  .withWaitStrategy(Wait.forHttp("/", 80).withStartupTimeout(120000))
  .start();
try {
  const url = `http://${nginx.getHost()}:${nginx.getMappedPort(80)}/`;
  console.log("nginx at", url);
  const resp = await fetch(url);
  const body = await resp.text();
  if (resp.status !== 200 || !body.includes("nginx")) {
    throw new Error(`GET ${url}: status ${resp.status}, body ${body.slice(0, 200)}`);
  }
  if (process.env.COMPAT_LEAK === "1") {
    await new GenericContainer("alpine:3.20").withCommand(["sleep", "600"]).start();
  }
} finally {
  await nginx.stop();
}
console.log("ok");
// The sockets to the engine and Ryuk would keep the process alive otherwise.
process.exit(0);
