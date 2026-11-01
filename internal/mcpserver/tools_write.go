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
			"It does nothing if the engine already runs. Returns the command's output in untrustedText (it can quote dockerd and the guest). " + noArgsDoc,
	}, startTimeout, func(ctx context.Context, _ noArgs) (any, error) {
		out, err := t.o.Host.Start(ctx)
		return engineCommandResult("hyperbutler start failed", out, err)
	})
	add(s, &mcp.Tool{Name: "stop", Annotations: stopping,
		Description: "Stops the hyperbutler engine, as `hyperbutler stop`: every running container stops, and docker commands fail until a start. " +
			"The output, in untrustedText, lists the containers that stop and which come back at the next start. Confirm with the user first. " + noArgsDoc,
	}, startTimeout, func(ctx context.Context, _ noArgs) (any, error) {
		out, err := t.o.Host.Stop(ctx)
		return engineCommandResult("hyperbutler stop failed", out, err)
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
	restarts := "no (default) or on-failure (at most " + strconv.Itoa(maxRetries) + " retries)"
	if t.o.AllowPersistent {
		restarts = "no (default), on-failure (at most " + strconv.Itoa(maxRetries) + " retries), always or unless-stopped"
	}
	add(s, &mcp.Tool{Name: "run", Annotations: writes,
		Description: "Creates and starts a container in the background, like `docker run -d`. The image must be in the engine (see pull). " +
			"TCP ports are reachable from Windows on 127.0.0.1 and [::1] only (the engine mirrors them there, never on another address); " +
			"UDP ports are published in the guest only. There are no bind mounts, volumes, privileges or host networking. " +
			fmt.Sprintf("Memory is capped (default %d MiB, or half the VM's memory if that is less; memoryMiB can choose up to half the VM's), ", defaultRunMemoryMiB) +
			fmt.Sprintf("at most %d processes, and CPUs at most the VM's. Restart policy: %s. ", runPidsLimit, restarts) +
			"The container gets the label " + mcpLabel + "=run. Returns its id, name and state; dockerd's warnings, if any, are in untrustedText.",
	}, writeTimeout, t.run)
	registries := "Docker Hub images only (references without a registry host, pulled from Docker Hub or the registry mirrors daemon.json sets)"
	if len(t.o.AllowRegistries) > 0 {
		registries += ", and images on " + strings.Join(t.o.AllowRegistries, ", ")
	}
	add(s, &mcp.Tool{Name: "pull", Annotations: pulling,
		Description: "Pulls an image, like `docker pull` without credentials (public images only): " + registries + ". " +
			fmt.Sprintf("A reference without a tag pulls :latest. At most %d pulls run at once, each for at most %v. ", maxPulls, pullTimeout) +
			"Returns the last progress messages in untrustedText; a failure's text from dockerd or the registry is in the error's untrustedText.",
	}, pullTimeout, t.pull)
	add(s, &mcp.Tool{Name: "config_set", Annotations: writes,
		Description: "Changes one hyperbutler setting, as `hyperbutler config set KEY VALUE`, with the same validation, and says what restart it needs; it never restarts anything. " +
			"Only these keys can be set from here: " + strings.Join(settableKeys(), ", ") + ". " +
			"The others (proxies, CAs, paths, the pipe, the host gateway, autostart, the update check) are refused: the user sets them with the CLI.",
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
	}, pruneTimeout, t.prune)
}

// CommandResult is the result of config_set: the CLI command's output.
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

// EngineCommandResult is the result of start and stop. Their output can quote dockerd
// and the guest (a daemon.json error, why a start failed), so it is untrusted text.
type EngineCommandResult struct {
	OK            bool   `json:"ok"`
	Truncated     bool   `json:"truncated"`
	UntrustedNote string `json:"untrustedNote"`
	UntrustedText string `json:"untrustedText"`
}

func engineCommandResult(failed, out string, err error) (any, error) {
	text := cleanText(out, true)
	cut := false
	if len(text) > maxStatusText {
		text, cut = clip(text, maxStatusText), true
	}
	if err != nil {
		if text != "" {
			text = err.Error() + "\n" + text
		} else {
			text = err.Error()
		}
		return nil, &untrustedError{msg: failed, text: text, err: err}
	}
	return EngineCommandResult{OK: true, Truncated: cut, UntrustedNote: untrustedNote, UntrustedText: text}, nil
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
	Ports   []string `json:"ports,omitempty" jsonschema:"ports to publish, HOST:CONTAINER or HOST:CONTAINER/udp; TCP ports are reachable from Windows on 127.0.0.1 and [::1] only"`
	Restart string   `json:"restart,omitempty" jsonschema:"restart policy: no (default) or on-failure; always and unless-stopped need the server's --allow-persistent"`
	// MemoryMiB and CPUs lower the default limits.
	MemoryMiB int     `json:"memoryMiB,omitempty" jsonschema:"the container's memory limit in MiB (default 1024, or half the VM's memory if less; at most half the VM's)"`
	CPUs      float64 `json:"cpus,omitempty" jsonschema:"how many CPUs the container may use, such as 0.5 (default and maximum: the VM's)"`
}

// The bounds on run's arguments.
const (
	maxArgs   = 64
	maxArgLen = 4096
	maxPorts  = 32
)

// The limits every container run creates has (docs/mcp.md, "What run creates").
const (
	// defaultRunMemoryMiB is the default memory limit, lowered to half the VM's memory
	// when that is less; a client can choose up to half the VM's, never more.
	defaultRunMemoryMiB = 1024
	// minRunMemoryMiB is dockerd's own minimum (6 MB), rounded up.
	minRunMemoryMiB = 6
	// runPidsLimit bounds the processes (a fork bomb stays in its container).
	runPidsLimit = 512
	// maxRetries is on-failure's retry count.
	maxRetries = 5
)

// vmLimits is what dockerd reports about the VM: its memory and CPUs.
type vmLimits struct {
	MemBytes int64
	CPUs     int
}

type portBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

// runBody checks in and returns the container create request, without the limits that
// depend on the VM (runLimits adds them). persistent allows the restart policies that
// bring a container back with the engine.
//
// Ports are bound to every guest address (HostIp ""), as `docker run -p P:C` does: the
// engine mirrors such a binding on Windows' 127.0.0.1 and [::1] and on no other host
// address (internal/engine, publish.go). A binding to 127.0.0.1 would be the guest's own
// loopback, which the engine cannot reach and does not mirror.
func runBody(in runIn, persistent bool) (map[string]any, error) {
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
	restart := map[string]any{"Name": in.Restart}
	switch in.Restart {
	case "":
		restart["Name"] = "no"
	case "no":
	case "on-failure":
		restart["MaximumRetryCount"] = maxRetries
	case "always", "unless-stopped":
		if !persistent {
			return nil, fmt.Errorf("restart %s would bring the container back at every engine start, which this server does not allow "+
				"(the user can allow it with `hyperbutler mcp --allow-write --allow-persistent`); use no or on-failure", in.Restart)
		}
	default:
		return nil, fmt.Errorf("restart %q: want no, on-failure, always or unless-stopped", clip(in.Restart, 40))
	}
	if in.MemoryMiB != 0 && in.MemoryMiB < minRunMemoryMiB || in.MemoryMiB < 0 {
		return nil, fmt.Errorf("memoryMiB %d is below %d", in.MemoryMiB, minRunMemoryMiB)
	}
	if in.CPUs != 0 && in.CPUs < 0.01 || in.CPUs < 0 {
		return nil, fmt.Errorf("cpus %g is below 0.01", in.CPUs)
	}
	exposed := map[string]struct{}{}
	bindings := map[string][]portBinding{}
	for _, p := range in.Ports {
		m := rePort.FindStringSubmatch(p)
		if m == nil {
			return nil, fmt.Errorf("port %q: want HOST:CONTAINER or HOST:CONTAINER/udp (the engine chooses the host address: Windows' loopback)", clip(p, 40))
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
		bindings[k] = append(bindings[k], portBinding{HostIP: "", HostPort: strconv.Itoa(hp)})
	}
	body := map[string]any{
		"Image":        in.Image,
		"Labels":       map[string]string{mcpLabel: "run"},
		"ExposedPorts": exposed,
		"HostConfig": map[string]any{
			"PortBindings":  bindings,
			"RestartPolicy": restart,
			"PidsLimit":     runPidsLimit,
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

// runLimits adds the memory and CPU limits for a VM of vm to body: memory
// defaultRunMemoryMiB, or half the VM's if less, and the client's memoryMiB up to half
// the VM's; CPUs the VM's, or the client's cpus up to them.
func runLimits(body map[string]any, in runIn, vm vmLimits) error {
	if vm.MemBytes <= 0 || vm.CPUs <= 0 {
		return errors.New("dockerd did not report the VM's memory and CPUs, so the container's limits cannot be set")
	}
	half := int(vm.MemBytes >> 20 / 2)
	mem := min(defaultRunMemoryMiB, half)
	if in.MemoryMiB != 0 {
		if in.MemoryMiB > half {
			return fmt.Errorf("memoryMiB %d is more than half the VM's memory (%d MiB)", in.MemoryMiB, half)
		}
		mem = in.MemoryMiB
	}
	cpus := float64(vm.CPUs)
	if in.CPUs != 0 {
		if in.CPUs > cpus {
			return fmt.Errorf("cpus %g is more than the VM's %d", in.CPUs, vm.CPUs)
		}
		cpus = in.CPUs
	}
	hc := body["HostConfig"].(map[string]any)
	hc["Memory"] = int64(mem) << 20
	hc["MemorySwap"] = int64(mem) << 20 // no swap on top
	hc["NanoCpus"] = int64(cpus * 1e9)
	return nil
}

func (t *toolset) run(ctx context.Context, in runIn) (any, error) {
	body, err := runBody(in, t.o.AllowPersistent)
	if err != nil {
		return nil, err
	}
	d, err := t.docker(ctx)
	if err != nil {
		return nil, err
	}
	defer d.close()
	var info apiInfo
	if err := d.getJSON(ctx, "/info", nil, &info); err != nil {
		return nil, err
	}
	if err := runLimits(body, in, vmLimits{MemBytes: info.MemTotal, CPUs: info.NCPU}); err != nil {
		return nil, err
	}
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
		ID    string `json:"id"`
		Name  string `json:"name"`
		State string `json:"state"`
		// dockerd's warnings, one per line.
		UntrustedNote string `json:"untrustedNote,omitempty"`
		UntrustedText string `json:"untrustedText,omitempty"`
	}{ID: shortID(created.ID), Name: field(strings.TrimPrefix(ins.Name, "/"), &cut), State: field(ins.State.Status, &cut)}
	var warnings []string
	for i, w := range created.Warnings {
		if i == 8 {
			break
		}
		warnings = append(warnings, field(w, &cut))
	}
	if len(warnings) > 0 {
		r.UntrustedNote, r.UntrustedText = untrustedNote, strings.Join(warnings, "\n")
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
	if err := t.pullAllowed(in.Image); err != nil {
		return nil, err
	}
	select {
	case t.pulls <- struct{}{}:
		defer func() { <-t.pulls }()
	default:
		return nil, fmt.Errorf("%d pulls are running already, the most this server runs at once: try again when one has finished", maxPulls)
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
			return nil, &untrustedError{msg: "pull " + in.Image + " failed (what dockerd or the registry said is in untrustedText)", text: clip(m.Error, 1024)}
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
	// The progress messages are dockerd's, but quote what the registry sent (tags,
	// digests): untrusted text, one message per line.
	return struct {
		Image         string `json:"image"`
		UntrustedNote string `json:"untrustedNote"`
		UntrustedText string `json:"untrustedText"`
	}{name + ":" + tag, untrustedNote, strings.Join(last, "\n")}, nil
}

// --- config_set ------------------------------------------------------------------------

type configSetIn struct {
	Key   string `json:"key" jsonschema:"the setting, such as memory or cpus (the settings tool lists them)"`
	Value string `json:"value" jsonschema:"its new value, as hyperbutler config set takes it"`
}

func (t *toolset) configSet(ctx context.Context, in configSetIn) (any, error) {
	k, err := config.Lookup(in.Key)
	if err != nil {
		return nil, err
	}
	// configpolicy.go: an allowlist, so a new key is refused until it is reviewed.
	if why := configSetRefused(k.Name); why != "" {
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
