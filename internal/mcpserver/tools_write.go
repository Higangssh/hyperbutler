package mcpserver

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Higangssh/hyperbutler/internal/config"
)

// mcpLabel marks the containers the run tool creates.
const mcpLabel = "dev.hyperbutler.mcp"

func (t *toolset) addWrite(s *mcp.Server) {
	add(s, &mcp.Tool{Name: "start", Annotations: writes,
		Description: "Starts the hyperbutler engine (the VM and dockerd), as `hyperbutler start`, with the guest image the engine last ran. " +
			"It does nothing if the engine already runs. Returns the command's output. " + noArgsDoc,
	}, startTimeout, func(ctx context.Context, _ noArgs) (any, error) {
		return t.commandResult(t.o.Host.Start(ctx))
	})
	add(s, &mcp.Tool{Name: "stop", Annotations: stopping,
		Description: "Stops the hyperbutler engine, as `hyperbutler stop`: every running container stops, and docker commands fail until a start. " +
			"The output lists the containers that stop and which come back at the next start. Confirm with the user first. " + noArgsDoc,
	}, startTimeout, func(ctx context.Context, _ noArgs) (any, error) {
		return t.commandResult(t.o.Host.Stop(ctx))
	})
	for _, a := range []struct{ name, verb, desc string }{
		{"container_start", "start", "Starts a stopped container."},
		{"container_stop", "stop", "Stops a running container (SIGTERM, then SIGKILL after timeoutSeconds)."},
		{"container_restart", "restart", "Restarts a container (stop, then start)."},
	} {
		ann := writes
		if a.verb != "start" {
			ann = stopping
		}
		add(s, &mcp.Tool{Name: a.name, Annotations: ann, Description: a.desc + " Returns its state afterwards."},
			writeTimeout, func(ctx context.Context, in containerActionIn) (any, error) {
				return t.containerAction(ctx, a.verb, in)
			})
	}
	add(s, &mcp.Tool{Name: "run", Annotations: writes,
		Description: "Creates and starts a container in the background, like `docker run -d`. The image must be in the engine (see pull). " +
			"Ports are published on 127.0.0.1 only; there are no bind mounts, volumes, privileges or host networking. " +
			"The container gets the label " + mcpLabel + "=run. Returns its id and name.",
	}, writeTimeout, t.run)
	add(s, &mcp.Tool{Name: "pull", Annotations: pulling,
		Description: "Pulls an image from its registry, like `docker pull` without credentials (public images only). " +
			"A reference without a tag pulls :latest. Returns the last progress messages.",
	}, pullTimeout, t.pull)
	add(s, &mcp.Tool{Name: "config_set", Annotations: writes,
		Description: "Changes one hyperbutler setting, as `hyperbutler config set KEY VALUE`, with the same validation, and says what restart it needs; it never restarts anything. " +
			"The settings tool lists the keys. Secret and path settings (proxy.http, proxy.https, ca.extraFile, data.dir) cannot be set from here: use the CLI.",
	}, writeTimeout, t.configSet)
}

func (t *toolset) addDelete(s *mcp.Server) {
	add(s, &mcp.Tool{Name: "container_rm", Annotations: deleting,
		Description: "Removes a container, as `docker rm` (force: also a running one). Its anonymous volumes stay. Confirm with the user first.",
	}, writeTimeout, t.containerRm)
	add(s, &mcp.Tool{Name: "image_rm", Annotations: deleting,
		Description: "Removes an image or one of its tags, as `docker rmi` (force: also one a stopped container uses). Confirm with the user first.",
	}, writeTimeout, t.imageRm)
	add(s, &mcp.Tool{Name: "volume_rm", Annotations: deleting,
		Description: "Removes a volume and its data, as `docker volume rm`. A volume in use is refused. Confirm with the user first.",
	}, writeTimeout, t.volumeRm)
	add(s, &mcp.Tool{Name: "prune", Annotations: deleting,
		Description: "Removes unused objects of one kind, as `docker <kind> prune`: containers (stopped ones), images (dangling ones), " +
			"volumes (unused anonymous ones), networks (unused ones) or build-cache. Confirm with the user first.",
	}, pullTimeout, t.prune)
}

// CommandResult is the result of start, stop and config_set: the CLI command's output.
type CommandResult struct {
	OK        bool   `json:"ok"`
	Output    string `json:"output"`
	Truncated bool   `json:"truncated"`
}

func (t *toolset) commandResult(out string, err error) (any, error) {
	text := cleanText(out, true)
	cut := false
	if len(text) > maxStatusText {
		text, cut = clip(text, maxStatusText), true
	}
	if err != nil {
		if text != "" {
			return nil, fmt.Errorf("%w\n%s", err, text)
		}
		return nil, err
	}
	return CommandResult{OK: true, Output: text, Truncated: cut}, nil
}

// --- container start, stop, restart ----------------------------------------------------

type containerActionIn struct {
	Container      string `json:"container" jsonschema:"the container's name or id"`
	TimeoutSeconds int    `json:"timeoutSeconds,omitempty" jsonschema:"stop and restart: seconds to wait before SIGKILL, 0 to 300 (default 10)"`
}

// ContainerState is a container's state after an action.
type ContainerState struct {
	Container string `json:"container"`
	Action    string `json:"action"`
	Changed   bool   `json:"changed"`
	State     string `json:"state"`
	ExitCode  int    `json:"exitCode"`
}

func (t *toolset) containerAction(ctx context.Context, verb string, in containerActionIn) (any, error) {
	if err := validName("container", in.Container); err != nil {
		return nil, err
	}
	if in.TimeoutSeconds < 0 || in.TimeoutSeconds > 300 {
		return nil, fmt.Errorf("timeoutSeconds %d is outside 0..300", in.TimeoutSeconds)
	}
	d, err := t.docker(ctx)
	if err != nil {
		return nil, err
	}
	defer d.close()
	q := url.Values{}
	if verb != "start" {
		ts := in.TimeoutSeconds
		if ts == 0 {
			ts = 10
		}
		q.Set("t", strconv.Itoa(ts))
	}
	resp, err := d.do(ctx, http.MethodPost, "/containers/"+in.Container+"/"+verb, q, nil)
	if err != nil {
		return nil, err
	}
	changed := resp.StatusCode != http.StatusNotModified // 304: already started or stopped
	_ = resp.Body.Close()
	var ins apiInspect
	if err := d.getJSON(ctx, "/containers/"+in.Container+"/json", nil, &ins); err != nil {
		return nil, err
	}
	cut := false
	return ContainerState{Container: in.Container, Action: verb, Changed: changed, State: field(ins.State.Status, &cut), ExitCode: ins.State.ExitCode}, nil
}

// --- run -------------------------------------------------------------------------------

type runIn struct {
	Image   string   `json:"image" jsonschema:"the image, such as busybox:1.37 (pull it first)"`
	Name    string   `json:"name,omitempty" jsonschema:"the container's name (default: one dockerd picks)"`
	Command []string `json:"command,omitempty" jsonschema:"the command and its arguments (default: the image's)"`
	Env     []string `json:"env,omitempty" jsonschema:"environment variables, NAME=value"`
	Ports   []string `json:"ports,omitempty" jsonschema:"ports to publish, HOST:CONTAINER or HOST:CONTAINER/udp; on 127.0.0.1 only"`
	Restart string   `json:"restart,omitempty" jsonschema:"restart policy: no (default), always, unless-stopped or on-failure"`
}

// The bounds on run's arguments.
const (
	maxArgs   = 64
	maxArgLen = 4096
	maxPorts  = 32
)

type portBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

// runBody checks in and returns the container create request.
func runBody(in runIn) (map[string]any, error) {
	if err := validImageRef(in.Image); err != nil {
		return nil, err
	}
	if in.Name != "" {
		if err := validName("name", in.Name); err != nil || len(in.Name) < 2 {
			return nil, fmt.Errorf("name %q is not a valid container name (2 or more letters, digits, '_', '.', '-')", clip(in.Name, 80))
		}
	}
	if len(in.Command) > maxArgs || len(in.Env) > maxArgs || len(in.Ports) > maxPorts {
		return nil, fmt.Errorf("at most %d command arguments, %d env entries and %d ports", maxArgs, maxArgs, maxPorts)
	}
	for _, a := range append(append([]string{}, in.Command...), in.Env...) {
		if len(a) > maxArgLen {
			return nil, fmt.Errorf("an argument or env entry is longer than %d bytes", maxArgLen)
		}
	}
	for _, e := range in.Env {
		if !reEnv.MatchString(e) {
			return nil, fmt.Errorf("env entry %q is not NAME=value", clip(strings.SplitN(e, "=", 2)[0], 40))
		}
	}
	switch in.Restart {
	case "":
		in.Restart = "no"
	case "no", "always", "unless-stopped", "on-failure":
	default:
		return nil, fmt.Errorf("restart %q: want no, always, unless-stopped or on-failure", clip(in.Restart, 40))
	}
	exposed := map[string]struct{}{}
	bindings := map[string][]portBinding{}
	for _, p := range in.Ports {
		m := rePort.FindStringSubmatch(p)
		if m == nil {
			return nil, fmt.Errorf("port %q: want HOST:CONTAINER or HOST:CONTAINER/udp (published on 127.0.0.1)", clip(p, 40))
		}
		hp, _ := strconv.Atoi(m[1])
		cp, _ := strconv.Atoi(m[2])
		if hp < 1 || hp > 65535 || cp < 1 || cp > 65535 {
			return nil, fmt.Errorf("port %q: ports are 1 to 65535", p)
		}
		proto := m[3]
		if proto == "" {
			proto = "tcp"
		}
		k := strconv.Itoa(cp) + "/" + proto
		exposed[k] = struct{}{}
		bindings[k] = append(bindings[k], portBinding{HostIP: "127.0.0.1", HostPort: strconv.Itoa(hp)})
	}
	body := map[string]any{
		"Image":        in.Image,
		"Labels":       map[string]string{mcpLabel: "run"},
		"ExposedPorts": exposed,
		"HostConfig": map[string]any{
			"PortBindings":  bindings,
			"RestartPolicy": map[string]any{"Name": in.Restart},
		},
	}
	if len(in.Command) > 0 {
		body["Cmd"] = in.Command
	}
	if len(in.Env) > 0 {
		body["Env"] = in.Env
	}
	return body, nil
}

func (t *toolset) run(ctx context.Context, in runIn) (any, error) {
	body, err := runBody(in)
	if err != nil {
		return nil, err
	}
	d, err := t.docker(ctx)
	if err != nil {
		return nil, err
	}
	defer d.close()
	q := url.Values{}
	if in.Name != "" {
		q.Set("name", in.Name)
	}
	var created struct {
		ID       string   `json:"Id"`
		Warnings []string `json:"Warnings"`
	}
	if err := d.callJSON(ctx, http.MethodPost, "/containers/create", q, body, &created); err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("image %s is not in the engine: pull it first (the pull tool): %w", in.Image, err)
		}
		return nil, err
	}
	if err := validName("container id", created.ID); err != nil {
		return nil, err
	}
	if err := d.callJSON(ctx, http.MethodPost, "/containers/"+created.ID+"/start", nil, nil, nil); err != nil {
		return nil, fmt.Errorf("container %s was created but did not start (remove it with docker rm): %w", shortID(created.ID), err)
	}
	var ins apiInspect
	if err := d.getJSON(ctx, "/containers/"+created.ID+"/json", nil, &ins); err != nil {
		return nil, err
	}
	cut := false
	r := struct {
		ID       string   `json:"id"`
		Name     string   `json:"name"`
		State    string   `json:"state"`
		Warnings []string `json:"warnings,omitempty"`
	}{ID: shortID(created.ID), Name: field(strings.TrimPrefix(ins.Name, "/"), &cut), State: field(ins.State.Status, &cut)}
	for i, w := range created.Warnings {
		if i == 8 {
			break
		}
		r.Warnings = append(r.Warnings, field(w, &cut))
	}
	return r, nil
}

// --- pull ------------------------------------------------------------------------------

type pullIn struct {
	Image string `json:"image" jsonschema:"the image reference, such as busybox, busybox:1.37 or ghcr.io/owner/name:tag"`
}

// maxPullLines is how many of the last progress messages pull returns.
const maxPullLines = 10

func (t *toolset) pull(ctx context.Context, in pullIn) (any, error) {
	if err := validImageRef(in.Image); err != nil {
		return nil, err
	}
	name, tag := splitImageRef(in.Image)
	d, err := t.docker(ctx)
	if err != nil {
		return nil, err
	}
	defer d.close()
	resp, err := d.do(ctx, http.MethodPost, "/images/create", url.Values{"fromImage": {name}, "tag": {tag}}, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	// One progress message per line; the stream is as long as the pull takes.
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	var last []string
	for sc.Scan() {
		var m struct {
			Status      string `json:"status"`
			ID          string `json:"id"`
			Error       string `json:"error"`
			ErrorDetail struct {
				Message string `json:"message"`
			} `json:"errorDetail"`
		}
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		if m.Error != "" {
			return nil, errors.New("pull " + in.Image + " failed; dockerd or the registry said (untrusted text): " + clip(m.Error, 1024))
		}
		if m.Status == "Downloading" || m.Status == "Extracting" || m.Status == "Waiting" {
			continue // progress bars
		}
		line := m.Status
		if m.ID != "" {
			line = m.ID + ": " + line
		}
		last = append(last, clip(cleanText(line, false), maxField))
		if len(last) > maxPullLines {
			last = last[1:]
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("pull %s: %w", in.Image, err)
	}
	return struct {
		Image  string   `json:"image"`
		Status []string `json:"status"`
	}{name + ":" + tag, last}, nil
}

// --- config_set ------------------------------------------------------------------------

type configSetIn struct {
	Key   string `json:"key" jsonschema:"the setting, such as memory or cpus (the settings tool lists them)"`
	Value string `json:"value" jsonschema:"its new value, as hyperbutler config set takes it"`
}

// configSetRefused says why a key cannot be set over MCP, or is empty. Secret keys
// (proxy URLs, which carry credentials) would put a password in the AI client's context
// and logs, and a proxy chosen by text the model read could intercept every pull. Path
// keys (ca.extraFile, data.dir) make hyperbutler read or create files where a client
// says: ca.extraFile adds a trusted root CA for dockerd, which would let an injected
// prompt intercept TLS. They stay CLI-only.
func configSetRefused(k *config.Key) string {
	switch {
	case k.Secret:
		return "it can hold credentials"
	case k.Type == "path":
		return "it names a file or directory on this machine"
	}
	return ""
}

func (t *toolset) configSet(ctx context.Context, in configSetIn) (any, error) {
	k, err := config.Lookup(in.Key)
	if err != nil {
		return nil, err
	}
	if why := configSetRefused(k); why != "" {
		return nil, fmt.Errorf("%s cannot be set from MCP because %s. Set it in a terminal with `hyperbutler config set %s VALUE`", k.Name, why, k.Name)
	}
	if len(in.Value) > maxArgLen {
		return nil, fmt.Errorf("value is longer than %d bytes", maxArgLen)
	}
	return t.commandResult(t.o.Host.ConfigSet(ctx, k.Name, in.Value))
}

// --- deletes ---------------------------------------------------------------------------

type containerRmIn struct {
	Container string `json:"container" jsonschema:"the container's name or id"`
	Force     bool   `json:"force,omitempty" jsonschema:"also remove it while it runs (it is killed)"`
}

func (t *toolset) containerRm(ctx context.Context, in containerRmIn) (any, error) {
	if err := validName("container", in.Container); err != nil {
		return nil, err
	}
	d, err := t.docker(ctx)
	if err != nil {
		return nil, err
	}
	defer d.close()
	q := url.Values{}
	if in.Force {
		q.Set("force", "1")
	}
	if err := d.callJSON(ctx, http.MethodDelete, "/containers/"+in.Container, q, nil, nil); err != nil {
		return nil, err
	}
	return struct {
		Removed string `json:"removed"`
	}{in.Container}, nil
}

type imageRmIn struct {
	Image string `json:"image" jsonschema:"the image reference or id"`
	Force bool   `json:"force,omitempty" jsonschema:"also remove it when stopped containers use it, or it has several tags"`
}

func (t *toolset) imageRm(ctx context.Context, in imageRmIn) (any, error) {
	if err := validImageRef(in.Image); err != nil {
		return nil, err
	}
	d, err := t.docker(ctx)
	if err != nil {
		return nil, err
	}
	defer d.close()
	q := url.Values{}
	if in.Force {
		q.Set("force", "1")
	}
	var r []struct {
		Untagged string `json:"Untagged"`
		Deleted  string `json:"Deleted"`
	}
	if err := d.callJSON(ctx, http.MethodDelete, "/images/"+in.Image, q, nil, &r); err != nil {
		return nil, err
	}
	out := struct {
		Untagged  []string `json:"untagged"`
		Deleted   []string `json:"deleted"`
		Truncated bool     `json:"truncated"`
	}{Untagged: []string{}, Deleted: []string{}}
	for i, e := range r {
		if i == maxItems {
			out.Truncated = true
			break
		}
		if e.Untagged != "" {
			out.Untagged = append(out.Untagged, field(e.Untagged, &out.Truncated))
		}
		if e.Deleted != "" {
			out.Deleted = append(out.Deleted, shortID(e.Deleted))
		}
	}
	return out, nil
}

type volumeRmIn struct {
	Volume string `json:"volume" jsonschema:"the volume's name"`
}

func (t *toolset) volumeRm(ctx context.Context, in volumeRmIn) (any, error) {
	if err := validName("volume", in.Volume); err != nil {
		return nil, err
	}
	d, err := t.docker(ctx)
	if err != nil {
		return nil, err
	}
	defer d.close()
	if err := d.callJSON(ctx, http.MethodDelete, "/volumes/"+in.Volume, nil, nil, nil); err != nil {
		return nil, err
	}
	return struct {
		Removed string `json:"removed"`
	}{in.Volume}, nil
}

type pruneIn struct {
	Kind string `json:"kind" jsonschema:"containers, images, volumes, networks or build-cache"`
}

// prunePaths are the prune API of each kind.
var prunePaths = map[string]string{
	"containers":  "/containers/prune",
	"images":      "/images/prune",
	"volumes":     "/volumes/prune",
	"networks":    "/networks/prune",
	"build-cache": "/build/prune",
}

func (t *toolset) prune(ctx context.Context, in pruneIn) (any, error) {
	path, ok := prunePaths[in.Kind]
	if !ok {
		return nil, fmt.Errorf("kind %q: want containers, images, volumes, networks or build-cache", clip(in.Kind, 40))
	}
	d, err := t.docker(ctx)
	if err != nil {
		return nil, err
	}
	defer d.close()
	var r struct {
		ContainersDeleted []string `json:"ContainersDeleted"`
		ImagesDeleted     []any    `json:"ImagesDeleted"`
		VolumesDeleted    []string `json:"VolumesDeleted"`
		NetworksDeleted   []string `json:"NetworksDeleted"`
		CachesDeleted     []string `json:"CachesDeleted"`
		SpaceReclaimed    int64    `json:"SpaceReclaimed"`
	}
	if err := d.callJSON(ctx, http.MethodPost, path, nil, nil, &r); err != nil {
		return nil, err
	}
	n := len(r.ContainersDeleted) + len(r.ImagesDeleted) + len(r.VolumesDeleted) + len(r.NetworksDeleted) + len(r.CachesDeleted)
	return struct {
		Kind           string `json:"kind"`
		Removed        int    `json:"removed"`
		SpaceReclaimed int64  `json:"spaceReclaimedBytes"`
	}{in.Kind, n, r.SpaceReclaimed}, nil
}
