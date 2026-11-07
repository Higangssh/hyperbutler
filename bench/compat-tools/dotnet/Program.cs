// Testcontainers for .NET's basic sample: nginx with a wait strategy, reached through the
// mapped port from the host. With COMPAT_LEAK=1 it also leaves a container running, for
// run.ps1 to see Ryuk remove it after this process ends.
using DotNet.Testcontainers.Builders;

var nginx = new ContainerBuilder()
    .WithImage("nginx:1.27-alpine")
    .WithPortBinding(80, true)
    .WithWaitStrategy(Wait.ForUnixContainer().UntilHttpRequestIsSucceeded(r => r.ForPort(80)))
    .Build();
await nginx.StartAsync();
try
{
    var url = $"http://{nginx.Hostname}:{nginx.GetMappedPublicPort(80)}/";
    Console.WriteLine($"nginx at {url}");
    using var http = new HttpClient();
    var body = await http.GetStringAsync(url);
    if (!body.Contains("nginx"))
    {
        throw new Exception($"GET {url}: {body}");
    }
    if (Environment.GetEnvironmentVariable("COMPAT_LEAK") == "1")
    {
        await new ContainerBuilder().WithImage("alpine:3.20").WithCommand("sleep", "600").WithCleanUp(true).Build().StartAsync();
    }
}
finally
{
    await nginx.DisposeAsync();
}
Console.WriteLine("ok");
