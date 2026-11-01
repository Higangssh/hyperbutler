package mcpserver

import "slices"

// Which settings config_set may change (docs/mcp.md, "Which settings config_set may
// change"). This is an allowlist: a key is settable over MCP only when it is listed with
// allowed true, so a key added to internal/config later is refused until someone decides
// (TestConfigPolicyCoversEveryKey fails until it is listed). A key is allowed only when
// its value is bounded by its validator, a change is undone by setting it back, and it
// neither widens what containers or the network can reach nor changes what is trusted
// or what runs at sign-in.
type configRule struct {
	allowed bool
	// why is the reason, said to the client when the key is refused.
	why string
}

var configPolicy = map[string]configRule{
	// Allowed: bounded and reversible, and they apply only at the next VM start.
	"memory":             {true, "guest memory, within the validator's bounds; applies at the next VM start"},
	"cpus":               {true, "virtual CPUs, at most the host's; applies at the next VM start"},
	"disk.size":          {true, "the data disk's size before it exists, within the validator's bounds; sparse, and fixed once the disk exists"},
	"engine.memoryLimit": {true, "the engine process's Go soft memory limit; applies at the next VM start"},
	"context.manage":     {true, "whether start selects the hyperbutler docker context; changes no running state"},

	// Refused: secrets, paths, reach, trust, persistence and network policy.
	"proxy.http":          {false, "it can hold credentials, and a proxy chosen by text the model read could see every pull"},
	"proxy.https":         {false, "it can hold credentials, and a proxy chosen by text the model read could see every pull"},
	"proxy.noProxy":       {false, "it decides which hosts dockerd reaches around the proxy, an egress policy that is the user's choice"},
	"ca.extraFile":        {false, "it names a file on this machine and adds a CA dockerd trusts, which would let an injected prompt intercept TLS"},
	"ca.import":           {false, "it decides whether dockerd trusts the Windows root stores' CAs, a TLS trust decision that is the user's"},
	"data.dir":            {false, "it names a directory on this machine where hyperbutler creates the data disk"},
	"pipe":                {false, "it chooses the Docker API pipe in a namespace other programs share, and could take a pipe such as Docker Desktop's docker_engine"},
	"hostGateway.enabled": {false, "it lets containers reach services on this machine's 127.0.0.1, and applies at once"},
	"hostGateway.ports":   {false, "it chooses which of this machine's loopback ports containers reach, and applies at once"},
	"autostart":           {false, "it creates a Task Scheduler task that runs at every sign-in"},
	"updateCheck":         {false, "whether hyperbutler makes its one network request is a network-policy choice for the user, in either direction"},
	"network.frameCheck":  {false, "turning it off trades data integrity for throughput, a choice that is the user's"},
}

// configSetRefused says why a key cannot be set over MCP, or is empty.
func configSetRefused(name string) string {
	r, ok := configPolicy[name]
	switch {
	case !ok:
		return "it is not on the list of settings this server may change (a new setting is refused until it is reviewed)"
	case !r.allowed:
		return r.why
	}
	return ""
}

// settableKeys lists the keys config_set accepts, for its description.
func settableKeys() []string {
	var out []string
	for k, r := range configPolicy {
		if r.allowed {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}
