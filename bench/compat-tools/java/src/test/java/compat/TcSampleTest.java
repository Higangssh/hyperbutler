package compat;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertTrue;

import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.time.Duration;
import org.junit.jupiter.api.Test;
import org.testcontainers.containers.GenericContainer;
import org.testcontainers.containers.wait.strategy.Wait;

/**
 * Testcontainers for Java's basic sample: nginx with a wait strategy, reached through the
 * mapped port from the host. With COMPAT_LEAK=1 it also leaves a container running, for
 * run.ps1 to see Ryuk remove it after this process ends.
 */
class TcSampleTest {
    @Test
    @SuppressWarnings("resource")
    void nginx() throws Exception {
        try (GenericContainer<?> nginx = new GenericContainer<>("nginx:1.27-alpine")
                .withExposedPorts(80)
                .waitingFor(Wait.forHttp("/").withStartupTimeout(Duration.ofMinutes(2)))) {
            nginx.start();
            URI url = URI.create("http://" + nginx.getHost() + ":" + nginx.getMappedPort(80) + "/");
            HttpResponse<String> resp = HttpClient.newHttpClient().send(
                    HttpRequest.newBuilder(url).build(), HttpResponse.BodyHandlers.ofString());
            assertEquals(200, resp.statusCode());
            assertTrue(resp.body().contains("nginx"));
        }
        if ("1".equals(System.getenv("COMPAT_LEAK"))) {
            new GenericContainer<>("alpine:3.20").withCommand("sleep", "600").start();
        }
    }
}
