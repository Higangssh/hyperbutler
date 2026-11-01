package dockerapi

// --- the Docker API's types, the fields used -------------------------------------------

// Port is one entry of a container's Ports.
type Port struct {
	IP          string `json:"IP"`
	PrivatePort uint16 `json:"PrivatePort"`
	PublicPort  uint16 `json:"PublicPort"`
	Type        string `json:"Type"`
}

// Container is one entry of GET /containers/json.
type Container struct {
	ID      string            `json:"Id"`
	Names   []string          `json:"Names"`
	Image   string            `json:"Image"`
	State   string            `json:"State"`
	Status  string            `json:"Status"`
	Created int64             `json:"Created"`
	Ports   []Port            `json:"Ports"`
	Labels  map[string]string `json:"Labels"`
}

// Inspect is the part of GET /containers/{id}/json used.
type Inspect struct {
	ID     string `json:"Id"`
	Name   string `json:"Name"`
	Config struct {
		Tty bool `json:"Tty"`
	} `json:"Config"`
	State struct {
		Status    string `json:"Status"`
		Running   bool   `json:"Running"`
		ExitCode  int    `json:"ExitCode"`
		StartedAt string `json:"StartedAt"`
	} `json:"State"`
	HostConfig struct {
		RestartPolicy struct {
			Name              string `json:"Name"`
			MaximumRetryCount int    `json:"MaximumRetryCount"`
		} `json:"RestartPolicy"`
	} `json:"HostConfig"`
}

// Image is one entry of GET /images/json.
type Image struct {
	ID          string   `json:"Id"`
	RepoTags    []string `json:"RepoTags"`
	RepoDigests []string `json:"RepoDigests"`
	Created     int64    `json:"Created"`
	Size        int64    `json:"Size"`
	SharedSize  int64    `json:"SharedSize"`
	Containers  int64    `json:"Containers"`
}

// Volume is one entry of GET /volumes.
type Volume struct {
	Name      string `json:"Name"`
	Driver    string `json:"Driver"`
	Scope     string `json:"Scope"`
	CreatedAt string `json:"CreatedAt"`
	UsageData *struct {
		Size     int64 `json:"Size"`
		RefCount int64 `json:"RefCount"`
	} `json:"UsageData"`
}

// DF is the part of GET /system/df used.
type DF struct {
	LayersSize int64   `json:"LayersSize"`
	Images     []Image `json:"Images"`
	Containers []struct {
		State  string `json:"State"`
		SizeRw int64  `json:"SizeRw"`
	} `json:"Containers"`
	Volumes    []Volume `json:"Volumes"`
	BuildCache []struct {
		Size   int64 `json:"Size"`
		InUse  bool  `json:"InUse"`
		Shared bool  `json:"Shared"`
	} `json:"BuildCache"`
}

// Info is the part of GET /info used.
type Info struct {
	ServerVersion     string `json:"ServerVersion"`
	MemTotal          int64  `json:"MemTotal"`
	NCPU              int    `json:"NCPU"`
	Containers        int    `json:"Containers"`
	ContainersRunning int    `json:"ContainersRunning"`
	Images            int    `json:"Images"`
}

// Stats is the part of GET /containers/{id}/stats?stream=false used.
type Stats struct {
	MemoryStats struct {
		Usage uint64            `json:"usage"`
		Limit uint64            `json:"limit"`
		Stats map[string]uint64 `json:"stats"`
	} `json:"memory_stats"`
}
