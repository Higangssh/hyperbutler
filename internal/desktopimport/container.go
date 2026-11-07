package desktopimport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Higangssh/hyperbutler/internal/desktopimport/source"
	"github.com/Higangssh/hyperbutler/internal/dockerapi"
	"github.com/Higangssh/hyperbutler/internal/engine"
	"github.com/Higangssh/hyperbutler/internal/share"
)

// Labels of a container of a Compose project.
const (
	composeProjectLabel    = "com.docker.compose.project"
	composeWorkingDirLabel = "com.docker.compose.project.working_dir"
)

// containerFromLabel marks a container the import created, with the id of Docker
// Desktop's it was made from: the next import takes it as already there, and any other
// container of that name as a clash. It starts with source.HelperLabel, so a container
// copied from an engine that has one drops it (ownLabel).
const containerFromLabel = "dev.hyperbutler.import.container"

const (
	// maxContainers bounds how many containers one plan inspects: the source could list
	// any number within dockerapi.BodyMax.
	maxContainers = 4096
	// maxCreateBody is what hyperbutler's proxy accepts as a container create (it refuses
	// more, share.maxCreateBody).
	maxCreateBody = 4 << 20
	// maxText bounds one name or value in a plan's sentence, and maxPath a path, which a
	// plan must show whole: the create gets all of it.
	maxText = 200
	maxPath = 4096
	// maxOptValue is how long a security option's value may be and still be shown; a
	// longer one (an inline seccomp profile) is shown as its size.
	maxOptValue = 32
)

// --- docker inspect, the fields used ------------------------------------------------------

type healthConfig struct {
	Test          []string `json:"Test,omitempty"`
	Interval      int64    `json:"Interval,omitempty"`
	Timeout       int64    `json:"Timeout,omitempty"`
	StartPeriod   int64    `json:"StartPeriod,omitempty"`
	StartInterval int64    `json:"StartInterval,omitempty"`
	Retries       int      `json:"Retries,omitempty"`
}

// containerConfig is both inspect's Config and what a create takes from it. Every field
// is one this package chose to carry over: nothing else of the answer reaches hyperbutler.
type containerConfig struct {
	Hostname     string              `json:"Hostname,omitempty"`
	Domainname   string              `json:"Domainname,omitempty"`
	User         string              `json:"User,omitempty"`
	Tty          bool                `json:"Tty,omitempty"`
	OpenStdin    bool                `json:"OpenStdin,omitempty"`
	StdinOnce    bool                `json:"StdinOnce,omitempty"`
	Env          []string            `json:"Env,omitempty"`
	Cmd          []string            `json:"Cmd,omitempty"`
	Entrypoint   []string            `json:"Entrypoint,omitempty"`
	Healthcheck  *healthConfig       `json:"Healthcheck,omitempty"`
	Image        string              `json:"Image"`
	WorkingDir   string              `json:"WorkingDir,omitempty"`
	Volumes      map[string]struct{} `json:"Volumes,omitempty"`
	Labels       map[string]string   `json:"Labels,omitempty"`
	StopSignal   string              `json:"StopSignal,omitempty"`
	StopTimeout  *int                `json:"StopTimeout,omitempty"`
	ExposedPorts map[string]struct{} `json:"ExposedPorts,omitempty"`
}

type portBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

type restartPolicy struct {
	Name              string `json:"Name"`
	MaximumRetryCount int    `json:"MaximumRetryCount,omitempty"`
}

type ulimit struct {
	Name string `json:"Name"`
	Soft int64  `json:"Soft"`
	Hard int64  `json:"Hard"`
}

type logConfig struct {
	Type   string            `json:"Type"`
	Config map[string]string `json:"Config,omitempty"`
}

// mountDriverConfig is read to say it does not carry over; mapMounts clears it.
type mountDriverConfig struct {
	Name    string            `json:"Name,omitempty"`
	Options map[string]string `json:"Options,omitempty"`
}

type mountVolumeOptions struct {
	NoCopy       bool               `json:"NoCopy,omitempty"`
	Labels       map[string]string  `json:"Labels,omitempty"`
	Subpath      string             `json:"Subpath,omitempty"`
	DriverConfig *mountDriverConfig `json:"DriverConfig,omitempty"`
}

// mountBindOptions is read to say it does not carry over; mapMounts clears it.
type mountBindOptions struct {
	Propagation            string `json:"Propagation,omitempty"`
	NonRecursive           bool   `json:"NonRecursive,omitempty"`
	CreateMountpoint       bool   `json:"CreateMountpoint,omitempty"`
	ReadOnlyNonRecursive   bool   `json:"ReadOnlyNonRecursive,omitempty"`
	ReadOnlyForceRecursive bool   `json:"ReadOnlyForceRecursive,omitempty"`
}

type mountTmpfsOptions struct {
	SizeBytes int64 `json:"SizeBytes,omitempty"`
	Mode      int64 `json:"Mode,omitempty"`
}

// mountSpec is one of HostConfig.Mounts (--mount), the options carried over only.
type mountSpec struct {
	Type          string              `json:"Type"`
	Source        string              `json:"Source,omitempty"`
	Target        string              `json:"Target"`
	ReadOnly      bool                `json:"ReadOnly,omitempty"`
	BindOptions   *mountBindOptions   `json:"BindOptions,omitempty"`
	VolumeOptions *mountVolumeOptions `json:"VolumeOptions,omitempty"`
	TmpfsOptions  *mountTmpfsOptions  `json:"TmpfsOptions,omitempty"`
}

// deviceMapping is one --device.
type deviceMapping struct {
	PathOnHost        string `json:"PathOnHost"`
	PathInContainer   string `json:"PathInContainer"`
	CgroupPermissions string `json:"CgroupPermissions"`
}

// deviceRequest is one --gpus.
type deviceRequest struct {
	Driver       string            `json:"Driver,omitempty"`
	Count        int64             `json:"Count,omitempty"`
	DeviceIDs    []string          `json:"DeviceIDs,omitempty"`
	Capabilities [][]string        `json:"Capabilities,omitempty"`
	Options      map[string]string `json:"Options,omitempty"`
}

// hostConfig is both inspect's HostConfig and what a create takes from it.
type hostConfig struct {
	Binds           []string                 `json:"Binds,omitempty"`
	Mounts          []mountSpec              `json:"Mounts,omitempty"`
	NetworkMode     string                   `json:"NetworkMode,omitempty"`
	PortBindings    map[string][]portBinding `json:"PortBindings,omitempty"`
	PublishAllPorts bool                     `json:"PublishAllPorts,omitempty"`
	RestartPolicy   *restartPolicy           `json:"RestartPolicy,omitempty"`
	AutoRemove      bool                     `json:"AutoRemove,omitempty"`
	LogConfig       *logConfig               `json:"LogConfig,omitempty"`

	// Limits.
	Memory            int64    `json:"Memory,omitempty"`
	MemoryReservation int64    `json:"MemoryReservation,omitempty"`
	MemorySwap        int64    `json:"MemorySwap,omitempty"`
	MemorySwappiness  *int64   `json:"MemorySwappiness,omitempty"`
	NanoCPUs          int64    `json:"NanoCpus,omitempty"`
	CPUShares         int64    `json:"CpuShares,omitempty"`
	CPUPeriod         int64    `json:"CpuPeriod,omitempty"`
	CPUQuota          int64    `json:"CpuQuota,omitempty"`
	CpusetCpus        string   `json:"CpusetCpus,omitempty"`
	CpusetMems        string   `json:"CpusetMems,omitempty"`
	PidsLimit         *int64   `json:"PidsLimit,omitempty"`
	BlkioWeight       uint16   `json:"BlkioWeight,omitempty"`
	Ulimits           []ulimit `json:"Ulimits,omitempty"`
	OomKillDisable    *bool    `json:"OomKillDisable,omitempty"`
	OomScoreAdj       int      `json:"OomScoreAdj,omitempty"`
	ShmSize           int64    `json:"ShmSize,omitempty"`

	ReadonlyRootfs bool              `json:"ReadonlyRootfs,omitempty"`
	Init           *bool             `json:"Init,omitempty"`
	DNS            []string          `json:"Dns,omitempty"`
	DNSOptions     []string          `json:"DnsOptions,omitempty"`
	DNSSearch      []string          `json:"DnsSearch,omitempty"`
	ExtraHosts     []string          `json:"ExtraHosts,omitempty"`
	Tmpfs          map[string]string `json:"Tmpfs,omitempty"`
	Sysctls        map[string]string `json:"Sysctls,omitempty"`
	GroupAdd       []string          `json:"GroupAdd,omitempty"`
	CapAdd         []string          `json:"CapAdd,omitempty"`
	CapDrop        []string          `json:"CapDrop,omitempty"`

	// What needs a confirmation (mapContainer) before it goes into a create.
	Privileged        bool            `json:"Privileged,omitempty"`
	PidMode           string          `json:"PidMode,omitempty"`
	IpcMode           string          `json:"IpcMode,omitempty"`
	UTSMode           string          `json:"UTSMode,omitempty"`
	UsernsMode        string          `json:"UsernsMode,omitempty"`
	CgroupnsMode      string          `json:"CgroupnsMode,omitempty"`
	SecurityOpt       []string        `json:"SecurityOpt,omitempty"`
	Devices           []deviceMapping `json:"Devices,omitempty"`
	DeviceCgroupRules []string        `json:"DeviceCgroupRules,omitempty"`
	DeviceRequests    []deviceRequest `json:"DeviceRequests,omitempty"`
}

type endpointIPAM struct {
	IPv4Address  string   `json:"IPv4Address,omitempty"`
	IPv6Address  string   `json:"IPv6Address,omitempty"`
	LinkLocalIPs []string `json:"LinkLocalIPs,omitempty"`
}

type endpointConfig struct {
	IPAMConfig *endpointIPAM `json:"IPAMConfig,omitempty"`
	Aliases    []string      `json:"Aliases,omitempty"`
}

// apiContainerInspect is GET /containers/{id}/json?size=1. It comes from the source
// engine, which could be anything: mapContainer takes nothing from it unchecked.
type apiContainerInspect struct {
	ID       string `json:"Id"`
	Name     string `json:"Name"`
	Image    string `json:"Image"`
	Platform string `json:"Platform"`
	SizeRw   *int64 `json:"SizeRw"`
	State    struct {
		Status string `json:"Status"`
	} `json:"State"`
	Config     containerConfig `json:"Config"`
	HostConfig struct {
		hostConfig
		// Read to say what does not carry over.
		Links        []string `json:"Links"`
		VolumesFrom  []string `json:"VolumesFrom"`
		Runtime      string   `json:"Runtime"`
		VolumeDriver string   `json:"VolumeDriver"`
		CgroupParent string   `json:"CgroupParent"`
	} `json:"HostConfig"`
	NetworkSettings struct {
		Networks map[string]endpointConfig `json:"Networks"`
	} `json:"NetworkSettings"`
	Mounts []struct {
		Type        string `json:"Type"`
		Destination string `json:"Destination"`
	} `json:"Mounts"`
}

// createBody is a POST /containers/create.
type createBody struct {
	containerConfig
	HostConfig       hostConfig `json:"HostConfig"`
	NetworkingConfig *struct {
		EndpointsConfig map[string]*endpointConfig `json:"EndpointsConfig"`
	} `json:"NetworkingConfig,omitempty"`
}

// containerSpec is what Run needs to create a planned container.
type containerSpec struct {
	body     createBody
	networks []networkSpec
}

// networkSpec is a network to create when hyperbutler has none of that name.
type networkSpec struct {
	Name       string            `json:"Name"`
	Driver     string            `json:"Driver"`
	Internal   bool              `json:"Internal,omitempty"`
	EnableIPv6 bool              `json:"EnableIPv6,omitempty"`
	Labels     map[string]string `json:"Labels,omitempty"`
	Options    map[string]string `json:"Options,omitempty"`
	IPAM       *networkIPAM      `json:"IPAM,omitempty"`

	notes []string // what the plan says about what does not carry over
}

type networkIPAM struct {
	Driver string `json:"Driver,omitempty"`
	Config []struct {
		Subnet  string `json:"Subnet,omitempty"`
		Gateway string `json:"Gateway,omitempty"`
		IPRange string `json:"IPRange,omitempty"`
	} `json:"Config,omitempty"`
}

// apiNetwork is GET /networks/{name}, the fields used.
type apiNetwork struct {
	Name       string            `json:"Name"`
	Driver     string            `json:"Driver"`
	Internal   bool              `json:"Internal"`
	EnableIPv6 bool              `json:"EnableIPv6"`
	Labels     map[string]string `json:"Labels"`
	Options    map[string]string `json:"Options"`
	IPAM       *networkIPAM      `json:"IPAM"`
}

// --- from inspect to a create ---------------------------------------------------------------

// mappedContainer is a container as the inspect answer alone describes it. What depends
// on the two engines (its image and volumes, a name clash) is planContainer's.
type mappedContainer struct {
	name, imageID, configImage, state string
	project, projectDir               string
	// unsupported says why it cannot be created again, whatever the user confirms.
	unsupported string
	requires    []string
	capAdd      []string
	binds       []string
	ports       []string
	volumes     []string
	networks    []string
	// staticIP are the networks it has a fixed address on: those need their subnet.
	staticIP map[string]bool
	rw       int64
	notes    []string
	body     createBody
}

// parseInspect decodes an inspect answer, whose size dockerapi.BodyMax already bounds.
func parseInspect(raw []byte) (*apiContainerInspect, error) {
	var in apiContainerInspect
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("decode docker inspect: %w", err)
	}
	return &in, nil
}

var (
	rePortKey   = regexp.MustCompile(`^[0-9]{1,5}(-[0-9]{1,5})?(/(tcp|udp|sctp))?$`)
	reHostPort  = regexp.MustCompile(`^([0-9]{1,5}(-[0-9]{1,5})?)?$`)
	reBindOpts  = regexp.MustCompile(`^[a-zA-Z,]*$`)
	reShortText = regexp.MustCompile(`^[a-zA-Z0-9_.:/-]*$`)
)

// clean is untrusted text as a plan shows it: one line, no control characters, bounded.
func clean(s string) string { return dockerapi.Clean(s, maxText) }

// cleanPath is clean for a path.
func cleanPath(s string) string { return dockerapi.Clean(s, maxPath) }

// mapContainer turns an inspect answer into a create body and what the plan says about
// it, taking only the fields the create body has. It is a pure function of the answer so
// it can be fuzzed: the source engine could send anything.
func mapContainer(in *apiContainerInspect) *mappedContainer {
	m := &mappedContainer{imageID: in.Image, configImage: in.Config.Image, state: dockerapi.Clean(in.State.Status, 32), rw: -1, staticIP: map[string]bool{}}
	m.name = strings.TrimPrefix(in.Name, "/")
	if !dockerapi.IsName(m.name) {
		m.unsupported = "its name is not one hyperbutler accepts: " + clean(m.name)
		m.name = clean(shortID(in.ID)) // FuzzMapContainer found an id with an escape
	}
	if in.SizeRw != nil && *in.SizeRw >= 0 {
		m.rw = *in.SizeRw
	}
	labels := in.Config.Labels
	// An empty value names no project, so it is no reason to skip the checks below.
	if project := labels[composeProjectLabel]; project != "" {
		m.project, m.projectDir = clean(project), cleanPath(labels[composeWorkingDirLabel])
		return m
	}
	for k := range labels {
		if strings.HasPrefix(k, "io.kubernetes.") || strings.HasPrefix(k, "com.docker.swarm.") || strings.HasPrefix(k, "com.docker.desktop.extension") {
			m.fail("it is managed by " + clean(strings.SplitN(k, ".", 3)[1]) + ", not made with docker run")
			break
		}
	}
	if in.Platform != "" && in.Platform != "linux" {
		m.fail("it is a " + clean(in.Platform) + " container")
	}
	hc := in.HostConfig.hostConfig
	if len(in.HostConfig.Links) > 0 {
		m.fail("it uses --link, which names a container of Docker Desktop's")
	}
	if len(in.HostConfig.VolumesFrom) > 0 {
		m.fail("it uses --volumes-from, which names a container of Docker Desktop's")
	}
	if rt := in.HostConfig.Runtime; rt != "" && rt != "runc" {
		m.notes = append(m.notes, "the runtime "+clean(rt)+" is not carried over")
	}
	if vd := in.HostConfig.VolumeDriver; vd != "" && vd != "local" {
		m.notes = append(m.notes, "the volume driver "+clean(vd)+" is not carried over")
	}
	if in.HostConfig.CgroupParent != "" {
		m.notes = append(m.notes, "the cgroup parent "+clean(in.HostConfig.CgroupParent)+" is not carried over")
	}
	if lc := hc.LogConfig; lc != nil && lc.Type != "json-file" && lc.Type != "local" {
		if lc.Type != "" {
			m.notes = append(m.notes, "the log driver "+clean(lc.Type)+" is not carried over")
		}
		hc.LogConfig = nil
	}
	if rp := hc.RestartPolicy; rp != nil {
		switch rp.Name {
		case "", "no", "always", "on-failure", "unless-stopped":
		default:
			m.fail("its restart policy " + clean(rp.Name) + " is not one Docker knows")
		}
	}
	targets := map[string]bool{}
	hc.Binds = m.mapBinds(hc.Binds, targets)
	hc.Mounts = m.mapMounts(hc.Mounts, targets)
	for _, tm := range in.Mounts {
		if tm.Type == "volume" && !targets[tm.Destination] {
			m.notes = append(m.notes, "the anonymous volume at "+clean(tm.Destination)+" is not carried over: the new container gets an empty one")
		}
	}
	slices.Sort(m.volumes)
	m.volumes = slices.Compact(m.volumes)
	m.mapPorts(hc.PortBindings)
	m.mapModes(&hc)
	endpoints := m.mapNetworks(in, &hc)

	if len(hc.CapAdd) > 0 {
		m.capAdd = listed(slices.Sorted(slices.Values(hc.CapAdd)))
	}
	seen := map[string]bool{}
	for _, c := range slices.Sorted(slices.Values(hc.CapAdd)) {
		if n := capName(c); dangerousCaps[n] && !seen[n] {
			seen[n] = true
			m.requires = append(m.requires, "it adds the capability "+n+" (--cap-add)")
		}
	}
	if m.rw != 0 {
		what := "may hold files"
		if m.rw > 0 {
			what = "holds " + engine.HumanSize(uint64(m.rw))
		}
		m.notes = append(m.notes, "its writable layer "+what+" and is not copied: what it wrote outside its volumes stays in Docker Desktop")
	}
	m.notes = listed(m.notes)

	cfg := in.Config
	cfg.Labels = withoutOwnLabels(labels)
	cfg.Labels[importLabel] = "docker-desktop"
	cfg.Labels[containerFromLabel] = dockerapi.Clean(in.ID, 128)
	// A hostname is the id's start unless the user set one.
	if in.ID != "" && strings.HasPrefix(in.ID, cfg.Hostname) {
		cfg.Hostname = ""
	}
	m.body = createBody{containerConfig: cfg, HostConfig: hc}
	if len(endpoints) > 0 {
		m.body.NetworkingConfig = &struct {
			EndpointsConfig map[string]*endpointConfig `json:"EndpointsConfig"`
		}{endpoints}
	}
	if b, err := json.Marshal(m.body); err != nil {
		m.fail("its settings cannot be encoded: " + clean(err.Error()))
	} else if len(b) > maxCreateBody {
		m.fail("its settings are larger than a container create may be")
	}
	return m
}

// fail records why the container cannot be created again; the first reason stays.
func (m *mappedContainer) fail(why string) {
	if m.unsupported == "" {
		m.unsupported = why
	}
}

// desktopHostPrefixes are where Docker Desktop's VM shows the Windows drives, followed by
// the lower-case drive letter or by wsl for a WSL distribution's files.
var desktopHostPrefixes = []string{"/run/desktop/mnt/host/", "/host_mnt/", "/mnt/host/"}

// desktopWSLBinds is where its WSL integration keeps a bind made from inside a
// distribution.
const desktopWSLBinds = "/mnt/wsl/docker-desktop-bind-mounts/" //nolint:gosec // G101: a path; "bind" read as a credential

// hasDotDot reports whether a path has a ".." segment, on either separator. Windows also
// drops a segment's trailing dots and spaces, so ".. " and "..." count.
func hasDotDot(p string) bool {
	for _, seg := range strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' }) {
		if strings.Trim(seg, ". ") == "" && strings.Count(seg, ".") >= 2 {
			return true
		}
	}
	return false
}

// hostPath undoes Docker Desktop's view of a Windows path: the source as the user wrote
// it, so that hyperbutler's own check and proxy read it as such. A path with a ".." stays
// as it is: it is then only a path inside the VM.
func hostPath(src string) string {
	if hasDotDot(src) {
		return src
	}
	if rest, ok := strings.CutPrefix(src, desktopWSLBinds); ok {
		return `\\wsl$\` + strings.ReplaceAll(rest, "/", `\`)
	}
	for _, pre := range desktopHostPrefixes {
		rest, ok := strings.CutPrefix(src, pre)
		if !ok {
			continue
		}
		first, tail, _ := strings.Cut(rest, "/")
		tail = strings.ReplaceAll(tail, "/", `\`)
		switch {
		case first == "wsl":
			return `\\wsl$\` + tail
		case len(first) == 1 && first[0]|0x20 >= 'a' && first[0]|0x20 <= 'z':
			return strings.ToUpper(first) + `:\` + tail
		}
	}
	return src
}

// mapBindSource looks at a path source. keep says the mount goes into the create, with
// the source out; why, when set, is what the user has to confirm.
func mapBindSource(src string) (out string, keep bool, why string) {
	out = hostPath(src)
	if err := share.CheckBindSource(out); err != nil {
		// The whole message: it says what to do. hyperbutler's engine would refuse the
		// source in a create, so a confirmation can only leave the mount out.
		var ue *share.UnsharableError
		if errors.As(err, &ue) {
			ue.Source = cleanPath(ue.Source)
		}
		return "", false, dockerapi.CleanText(err.Error(), false) + " With a confirmation the container is created without this mount"
	}
	switch {
	case !share.WindowsPath(out):
		return out, true, "bind source " + cleanPath(out) + " is a folder inside Docker Desktop's VM: hyperbutler's VM has its own, and the content is not copied"
	case hasDotDot(out):
		return out, true, "bind source " + cleanPath(out) + " goes through a .., so it may not be the folder it seems"
	}
	return out, true, ""
}

// isPathSource tells a path from a volume name in a -v spec.
func isPathSource(src string) bool {
	return strings.HasPrefix(src, "/") || strings.HasPrefix(src, `\`) || share.WindowsPath(src)
}

func (m *mappedContainer) mapBinds(binds []string, targets map[string]bool) []string {
	var out []string
	for _, spec := range binds {
		src, target, opts, err := share.ParseBind(spec)
		switch {
		case err != nil:
			m.fail("its bind " + clean(spec) + " cannot be read")
			continue
		case !reBindOpts.MatchString(opts):
			m.fail("its bind " + clean(spec) + " has options Docker does not know")
			continue
		}
		targets[target] = true
		if !isPathSource(src) {
			if dockerapi.ValidName("volume", src) != nil {
				m.fail("its volume " + clean(src) + " has an invalid name")
				continue
			}
			m.volumes = append(m.volumes, src)
			out = append(out, spec)
			continue
		}
		mapped, keep, why := mapBindSource(src)
		if why != "" {
			m.requires = append(m.requires, why)
		}
		if !keep {
			continue
		}
		s := mapped + ":" + target
		if opts != "" {
			s += ":" + opts
		}
		out = append(out, s)
		m.binds = append(m.binds, cleanPath(mapped)+" -> "+cleanPath(target))
	}
	return out
}

func (m *mappedContainer) mapMounts(mounts []mountSpec, targets map[string]bool) []mountSpec {
	var out []mountSpec
	for _, ms := range mounts {
		if ms.Target == "" || ms.Target[0] != '/' {
			m.fail("a mount of it has no absolute target")
			continue
		}
		targets[ms.Target] = true
		m.noteMountOptions(&ms)
		switch ms.Type {
		case "volume":
			if ms.Source != "" { // empty: anonymous, and the create makes a new one
				if dockerapi.ValidName("volume", ms.Source) != nil {
					m.fail("its volume " + clean(ms.Source) + " has an invalid name")
					continue
				}
				m.volumes = append(m.volumes, ms.Source)
			}
		case "bind":
			if !isPathSource(ms.Source) {
				m.fail("its bind mount source " + clean(ms.Source) + " is not a path")
				continue
			}
			mapped, keep, why := mapBindSource(ms.Source)
			if why != "" {
				m.requires = append(m.requires, why)
			}
			if !keep {
				continue
			}
			ms.Source = mapped
			m.binds = append(m.binds, cleanPath(mapped)+" -> "+cleanPath(ms.Target))
		case "tmpfs":
		default:
			m.fail("it uses a mount of type " + clean(ms.Type))
			continue
		}
		out = append(out, ms)
	}
	return out
}

// noteMountOptions clears a --mount's bind options and volume driver config, which do not
// carry over, and says so: propagation means something on the VM it was set for, and a
// volume is a plain local one here.
func (m *mappedContainer) noteMountOptions(ms *mountSpec) {
	if b := ms.BindOptions; b != nil {
		if *b != (mountBindOptions{}) {
			m.notes = append(m.notes, "the bind options of the mount at "+cleanPath(ms.Target)+" (propagation, recursion, creating the mountpoint) are not carried over")
		}
		ms.BindOptions = nil
	}
	if v := ms.VolumeOptions; v != nil && v.DriverConfig != nil {
		if d := v.DriverConfig; (d.Name != "" && d.Name != "local") || len(d.Options) > 0 {
			m.notes = append(m.notes, "the volume driver settings of the mount at "+cleanPath(ms.Target)+" are not carried over")
		}
		c := *v
		c.DriverConfig = nil
		ms.VolumeOptions = &c
	}
}

func (m *mappedContainer) mapPorts(bindings map[string][]portBinding) {
	for port, list := range bindings {
		if !rePortKey.MatchString(port) {
			m.fail("its published port " + clean(port) + " cannot be read")
			continue
		}
		for _, b := range list {
			if !reHostPort.MatchString(b.HostPort) {
				m.fail("its published port " + clean(b.HostPort) + " cannot be read")
				continue
			}
			if b.HostIP != "" {
				if _, err := netip.ParseAddr(b.HostIP); err != nil {
					m.fail("its published port's address " + clean(b.HostIP) + " cannot be read")
					continue
				}
			}
			m.ports = append(m.ports, strings.TrimPrefix(b.HostIP+":"+b.HostPort, ":")+"->"+port)
		}
	}
	sort.Strings(m.ports)
	m.ports = listed(m.ports)
}

// mapModes sorts the namespace modes and privileges into what needs a confirmation and
// what cannot be recreated: another container's namespace is named by its id here.
func (m *mappedContainer) mapModes(hc *hostConfig) {
	if hc.Privileged {
		m.requires = append(m.requires, "it runs privileged (--privileged)")
	}
	for _, mode := range []struct{ value, what string }{
		{hc.PidMode, "PID"}, {hc.IpcMode, "IPC"}, {hc.UTSMode, "UTS"}, {hc.UsernsMode, "user"}, {hc.CgroupnsMode, "cgroup"},
	} {
		switch {
		case mode.value == "host":
			m.requires = append(m.requires, "it uses the host's "+mode.what+" namespace")
		case strings.HasPrefix(mode.value, "container:"):
			m.fail("it shares the " + mode.what + " namespace of another container")
		case mode.value != "" && mode.value != "private" && mode.value != "shareable" && mode.value != "none":
			m.fail("its " + mode.what + " mode " + clean(mode.value) + " is not one hyperbutler knows")
		}
	}
	for _, d := range hc.Devices {
		m.requires = append(m.requires, "it uses the host device "+cleanPath(d.PathOnHost)+" (--device)")
	}
	for _, r := range hc.DeviceCgroupRules {
		m.requires = append(m.requires, "it has the device cgroup rule "+clean(r))
	}
	for _, d := range hc.DeviceRequests {
		m.requires = append(m.requires, fmt.Sprintf("it requests host devices (--gpus) of the driver %s, count %d, ids [%s], with %d options",
			clean(d.Driver), d.Count, strings.Join(listed(d.DeviceIDs), ", "), len(d.Options)))
	}
	var weak []string
	for _, o := range hc.SecurityOpt {
		if securityKey(o) != "no-new-privileges" {
			weak = append(weak, securityText(o))
		}
	}
	if len(weak) > 0 {
		m.requires = append(m.requires, "it has the security options "+strings.Join(weak, ", "))
	}
}

// dangerousCaps are the capabilities that reach past the container as far as a host
// namespace does, so they need a confirmation; the others are only shown.
var dangerousCaps = map[string]bool{
	"ALL": true, "SYS_ADMIN": true, "SYS_MODULE": true, "SYS_RAWIO": true, "SYS_PTRACE": true,
	"DAC_READ_SEARCH": true, "BPF": true, "PERFMON": true,
}

// capName is a capability as dockerd reads it: in any case, with or without CAP_.
func capName(c string) string {
	return strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(c)), "CAP_")
}

// splitSecurityOpt splits a security option: key=value, or key:value in its older form.
func splitSecurityOpt(o string) (key, value string, has bool) {
	if i := strings.IndexAny(o, "=:"); i >= 0 {
		return o[:i], o[i+1:], true
	}
	return o, "", false
}

// securityKey is a security option's key as a plan shows it.
func securityKey(o string) string {
	key, _, _ := splitSecurityOpt(o)
	return dockerapi.Clean(key, 64)
}

// securityText is a security option as a plan shows it: its key always, its value when
// short. A seccomp profile can be megabytes inline, and must not hide what follows it.
func securityText(o string) string {
	key, value, has := splitSecurityOpt(o)
	switch {
	case !has:
		return securityKey(o)
	case len(value) > maxOptValue:
		return fmt.Sprintf("%s=(%d bytes)", securityKey(key), len(value))
	}
	return securityKey(key) + "=" + dockerapi.CleanText(value, false)
}

// mapNetworks fills m.networks and hc.NetworkMode and returns the endpoints of a create.
func (m *mappedContainer) mapNetworks(in *apiContainerInspect, hc *hostConfig) map[string]*endpointConfig {
	mode := hc.NetworkMode
	switch {
	case mode == "" || mode == "default":
		mode = "bridge"
	case mode == "host":
		m.requires = append(m.requires, "it uses host networking")
	case strings.HasPrefix(mode, "container:"):
		m.fail("it shares the network of another container")
		return nil
	case mode != "bridge" && mode != "none" && dockerapi.ValidName("network", mode) != nil:
		m.fail("its network " + clean(mode) + " has an invalid name")
		return nil
	}
	hc.NetworkMode = mode
	if mode == "host" || mode == "none" {
		return nil
	}
	endpoints := map[string]*endpointConfig{}
	if mode != "bridge" {
		endpoints[mode] = &endpointConfig{}
	}
	for name, ep := range in.NetworkSettings.Networks {
		if name == "bridge" || name == "host" || name == "none" {
			continue
		}
		if dockerapi.ValidName("network", name) != nil {
			m.fail("its network " + clean(name) + " has an invalid name")
			return nil
		}
		e := &endpointConfig{IPAMConfig: ep.IPAMConfig}
		// The engine adds the container's short id as an alias; the new one has its own.
		for _, a := range ep.Aliases {
			if a != "" && !strings.HasPrefix(in.ID, a) {
				e.Aliases = append(e.Aliases, a)
			}
		}
		if ip := ep.IPAMConfig; ip != nil && (ip.IPv4Address != "" || ip.IPv6Address != "") {
			m.staticIP[name] = true
		}
		endpoints[name] = e
	}
	m.networks = slices.Sorted(maps.Keys(endpoints))
	if len(endpoints) == 0 {
		return nil
	}
	if mode == "bridge" {
		endpoints["bridge"] = &endpointConfig{}
	}
	return endpoints
}

// listed makes a list a plan shows safe to print. It never leaves anything out, of the
// list or of an item: what is confirmed must be what is created. Each item bounds its own
// parts when it is made, and dockerapi.BodyMax the whole.
func listed(s []string) []string {
	for i := range s {
		s[i] = dockerapi.CleanText(s[i], false)
	}
	return s
}

// --- the plan --------------------------------------------------------------------------------

// planContainers lists Docker Desktop's containers, its helpers left out, and what the
// import would do with each. It runs after the images and volumes are planned, which a
// container's status depends on. A container that cannot be read is listed as unsupported,
// so one bad answer does not stop the plan.
func (im *Importer) planContainers(ctx context.Context, p *Plan, d dstImages) error {
	var list []apiContainer
	if err := im.src.GetJSON(ctx, "/containers/json", url.Values{"all": {"1"}}, &list); err != nil {
		return fmt.Errorf("list Docker Desktop's containers: %w", err)
	}
	nets := map[string]*apiNetwork{}
	ids := map[string]bool{}
	n := 0
	for _, c := range list {
		if c.helper() || ids[c.ID] {
			continue
		}
		ids[c.ID] = true
		if dockerapi.ValidName("container id", c.ID) != nil {
			p.Notes = append(p.Notes, fmt.Sprintf("skipped a container with an unexpected id %q", dockerapi.Clean(c.ID, 80)))
			continue
		}
		if n++; n > maxContainers {
			p.Notes = append(p.Notes, fmt.Sprintf("Docker Desktop lists more than %d containers; the rest are not offered", maxContainers))
			break
		}
		cp, err := im.planContainer(ctx, p, c, d, nets)
		if err != nil {
			return err
		}
		if cp != nil {
			p.Containers = append(p.Containers, *cp)
		}
	}
	sort.SliceStable(p.Containers, func(i, j int) bool { return p.Containers[i].Name < p.Containers[j].Name })
	refuseDuplicateNames(p)
	return nil
}

// refuseDuplicateNames makes both of two containers with one name unsupported. An engine
// never lists that, but one that does cannot say which of them a name selects.
func refuseDuplicateNames(p *Plan) {
	count := map[string]int{}
	for _, cp := range p.Containers {
		count[cp.Name]++
	}
	for i := range p.Containers {
		if cp := &p.Containers[i]; count[cp.Name] > 1 {
			cp.Status, cp.spec, cp.Project, cp.ProjectDir = StatusUnsupported, nil, "", ""
			cp.Reason = "Docker Desktop lists more than one container of this name"
		}
	}
}

// planContainer plans one container; nil when it went away meanwhile.
func (im *Importer) planContainer(ctx context.Context, p *Plan, c apiContainer, d dstImages, nets map[string]*apiNetwork) (*ContainerPlan, error) {
	cp := &ContainerPlan{Name: c.name(), ID: c.ID, Status: StatusUnsupported, WritableBytes: -1}
	var raw json.RawMessage
	err := im.src.GetJSON(ctx, "/containers/"+c.ID+"/json", url.Values{"size": {"1"}}, &raw)
	switch {
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case dockerapi.IsNotFound(err):
		return nil, nil
	case err != nil:
		cp.Reason = "its docker inspect could not be read: " + clean(err.Error())
		return cp, nil
	}
	in, err := parseInspect(raw)
	if err != nil {
		cp.Reason = "its docker inspect could not be read: " + clean(err.Error())
		return cp, nil
	}
	if in.ID != c.ID {
		cp.Reason = "the engine answered for another container"
		return cp, nil
	}
	m := mapContainer(in)
	cp.Name, cp.ImageID, cp.State = m.name, dockerapi.Clean(m.imageID, 128), m.state
	cp.WritableBytes, cp.Notes = m.rw, m.notes
	cp.Requires, cp.CapAdd, cp.Binds = listed(m.requires), m.capAdd, listed(m.binds)
	cp.Ports, cp.Volumes, cp.Networks = m.ports, m.volumes, m.networks
	if m.project != "" {
		cp.Status, cp.Project, cp.ProjectDir = StatusCompose, m.project, m.projectDir
		where := "in its folder"
		if m.projectDir != "" {
			where = "in " + m.projectDir
		}
		cp.Reason = fmt.Sprintf("part of the Compose project %s: not recreated; run docker compose up %s", m.project, where)
		return cp, nil
	}
	if m.unsupported != "" {
		cp.Reason = m.unsupported
		return cp, nil
	}
	switch clash, err := im.containerClash(ctx, cp); {
	case err != nil:
		return nil, err
	case clash != "":
		cp.Status, cp.Reason = clash, clashReason[clash]
		return cp, nil
	}
	var flagged Status
	cp.Image, flagged, cp.Reason = containerImage(p, d, m)
	if flagged != "" {
		cp.Status = flagged
		return cp, nil
	}
	for _, name := range m.volumes {
		i := slices.IndexFunc(p.Volumes, func(v VolumePlan) bool { return v.Name == name })
		switch {
		case i < 0:
			cp.Reason = "its volume " + name + " is not in Docker Desktop's list of volumes"
			return cp, nil
		case p.Volumes[i].Status == StatusUnsupported:
			cp.Reason = "its volume " + name + " cannot be imported: " + p.Volumes[i].Reason
			return cp, nil
		case len(p.Volumes[i].UsedBy) > 0:
			cp.Notes = append(cp.Notes, "its volume "+name+" is in use in Docker Desktop: stop "+strings.Join(p.Volumes[i].UsedBy, ", ")+" there first, or the volume and this container are refused (--allow-live-volumes copies anyway)")
		}
	}
	spec := &containerSpec{body: m.body}
	for _, name := range m.networks {
		ns, why, err := im.sourceNetwork(ctx, nets, name, m.staticIP[name])
		if err != nil {
			return nil, err
		}
		if why != "" {
			cp.Reason = why
			return cp, nil
		}
		spec.networks = append(spec.networks, *ns)
		cp.Notes = append(cp.Notes, ns.notes...)
		switch err := im.dst.JSON(ctx, http.MethodGet, "/networks/"+name, nil, nil, nil); {
		case dockerapi.IsNotFound(err):
			say := "network " + name + " will be created" + subnetText(ns)
			cp.Notes, cp.networkNotes = append(cp.Notes, say), append(cp.networkNotes, say)
		case err != nil:
			return nil, fmt.Errorf("look for the network %s in hyperbutler: %w", name, err)
		}
	}
	spec.body.Image = cp.Image
	cp.spec = spec
	cp.Status = StatusNew
	if len(cp.Requires) > 0 {
		cp.Status = StatusConfirm
		cp.Reason = "skipped unless confirmed (--confirm-container " + cp.Name + "): " + strings.Join(cp.Requires, "; ")
	}
	return cp, nil
}

// clashReason says what a container of the same name in hyperbutler means.
var clashReason = map[Status]string{
	StatusAlready:  "in hyperbutler: an earlier import created it from this one",
	StatusConflict: "a container of this name is in hyperbutler and is not overwritten; remove or rename it there first",
}

// containerClash looks for a container of cp's name in hyperbutler: "" when there is
// none, StatusAlready when an import made it from this one, else StatusConflict.
func (im *Importer) containerClash(ctx context.Context, cp *ContainerPlan) (Status, error) {
	var got struct {
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
	}
	err := im.dst.JSON(ctx, http.MethodGet, "/containers/"+cp.Name+"/json", nil, nil, &got)
	switch {
	case dockerapi.IsNotFound(err):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("look for the container %s in hyperbutler: %w", cp.Name, err)
	case got.Config.Labels[containerFromLabel] == cp.ID:
		return StatusAlready, nil
	}
	return StatusConflict, nil
}

// containerImage finds the reference to create a container from. It prefers the name
// the container was created with, when that still names the image it runs. flagged is
// the status when the image cannot come along, with the reason why.
func containerImage(p *Plan, d dstImages, m *mappedContainer) (ref string, flagged Status, why string) {
	short := shortID(m.imageID)
	i := slices.IndexFunc(p.Images, func(ip ImagePlan) bool { return ip.ID == m.imageID })
	if i < 0 || p.Images[i].Status == StatusUnsupported {
		switch {
		case m.imageID != "" && d.ids[m.imageID] && dockerapi.ValidImageRef(m.imageID) == nil:
			return m.imageID, "", ""
		case i < 0:
			return "", StatusUnsupported, "its image " + clean(short) + " is not in Docker Desktop's list of images"
		}
		return "", StatusUnsupported, "its image " + clean(short) + " has no tag in Docker Desktop (the tag has moved to a newer one?), so it is not in the import: give it a tag there"
	}
	ip := p.Images[i]
	if ip.Status == StatusConflict {
		return "", StatusConflict, "its image: " + ip.Reason
	}
	if ref := normalizeRef(m.configImage); slices.Contains(ip.Tags, ref) {
		return ref, "", ""
	}
	return ip.Tags[0], "", ""
}

// sourceNetwork reads a network from Docker Desktop, once per plan, and turns it into a
// create. why says why it cannot be created again. Only a bridge network can, and only
// its MTU of the driver's options: a bridge's other options (an interface name, say) mean
// something on Docker Desktop's VM only. The subnet goes along only for a network where a
// container has a fixed address: inspect also shows the subnet the engine picked, which
// may be taken in hyperbutler.
func (im *Importer) sourceNetwork(ctx context.Context, nets map[string]*apiNetwork, name string, static bool) (*networkSpec, string, error) {
	n, ok := nets[name]
	if !ok {
		var got apiNetwork
		err := im.src.GetJSON(ctx, "/networks/"+name, nil, &got)
		switch {
		case ctx.Err() != nil:
			return nil, "", ctx.Err()
		case err != nil:
			return nil, "its network " + name + " could not be read: " + clean(err.Error()), nil
		}
		n = &got
		nets[name] = n
	}
	if n.Driver != "" && n.Driver != "bridge" {
		return nil, "its network " + name + " uses the driver " + clean(n.Driver) + ": only bridge networks are created again", nil
	}
	spec := &networkSpec{Name: name, Driver: "bridge", Internal: n.Internal, EnableIPv6: n.EnableIPv6, Labels: withoutOwnLabels(n.Labels)}
	if mtu, ok := n.Options["com.docker.network.driver.mtu"]; ok && reShortText.MatchString(mtu) {
		spec.Options = map[string]string{"com.docker.network.driver.mtu": mtu}
	}
	if n.IPAM != nil {
		ipam := *n.IPAM
		if ipam.Driver != "" && ipam.Driver != "default" {
			spec.notes = append(spec.notes, "the IPAM driver "+clean(ipam.Driver)+" of network "+name+" is not carried over")
			ipam.Driver = ""
		}
		if static {
			spec.IPAM = &ipam
		}
	}
	return spec, "", nil
}

// subnetText is the subnets a new network takes from Docker Desktop, for a plan.
func subnetText(ns *networkSpec) string {
	if ns.IPAM == nil {
		return ""
	}
	var subnets []string
	for _, c := range ns.IPAM.Config {
		if c.Subnet != "" {
			subnets = append(subnets, clean(c.Subnet))
		}
	}
	if len(subnets) == 0 {
		return ""
	}
	return " (subnet " + strings.Join(subnets, ", ") + ")"
}

// creatable reports whether Run creates this planned container.
func (c ContainerPlan) creatable() bool {
	return c.spec != nil && (c.Status == StatusNew || (c.Status == StatusConfirm && c.confirmed))
}

// findContainer finds a selected container by name, id or id prefix (12 or more hex
// digits).
func findContainer(p *Plan, name string) (int, error) {
	if err := dockerapi.ValidName("container", name); err != nil {
		return -1, err
	}
	i := slices.IndexFunc(p.Containers, func(c ContainerPlan) bool {
		return c.Name == name || c.ID == name || (len(name) >= 12 && isHex(name) && strings.HasPrefix(c.ID, name))
	})
	if i < 0 {
		return -1, fmt.Errorf("container %q is not in Docker Desktop", name)
	}
	return i, nil
}

// resolveContainers turns a selection into the plan's containers. A name that is not in
// the plan is an error. AllContainers leaves out what it should not create without being
// asked, with the reason; a container picked by name stays, and Run refuses it when it
// cannot be created, so the command says so and exits 1.
func resolveContainers(p *Plan, sel Selection) (ctrs []ContainerPlan, skipped []ItemResult, err error) {
	picked := map[string]bool{}
	confirmed := map[string]bool{}
	for _, name := range sel.Containers {
		i, err := findContainer(p, name)
		if err != nil {
			return nil, nil, err
		}
		picked[p.Containers[i].ID] = true
	}
	for _, name := range sel.ConfirmContainers {
		i, err := findContainer(p, name)
		if err != nil {
			return nil, nil, err
		}
		picked[p.Containers[i].ID], confirmed[p.Containers[i].ID] = true, true
	}
	for _, cp := range p.Containers {
		cp.confirmed = confirmed[cp.ID]
		switch {
		case picked[cp.ID]:
			ctrs = append(ctrs, cp)
		case !sel.AllContainers:
		case cp.creatable() || cp.Status == StatusAlready:
			ctrs = append(ctrs, cp)
		default:
			skipped = append(skipped, ItemResult{Kind: KindContainer, Name: cp.Name, ID: cp.ID, Outcome: OutcomeRefused, Reason: cp.Reason})
		}
	}
	return ctrs, skipped, nil
}

// --- creating them -----------------------------------------------------------------------------

// announceContainers says before the copy what the containers about to be created bring
// to the host: added capabilities and the folders they will share, and what was
// confirmed. A plan shows the same, but a command line with names shows no plan.
func announceContainers(ctrs []ContainerPlan, progress func(Event)) {
	for _, cp := range ctrs {
		if !cp.creatable() {
			continue
		}
		if len(cp.CapAdd) > 0 {
			progress(Event{Kind: EventNote, Item: KindContainer, Name: cp.Name, Message: "container " + cp.Name + " adds the capabilities " + strings.Join(cp.CapAdd, ", ")})
		}
		if len(cp.Binds) > 0 {
			progress(Event{Kind: EventNote, Item: KindContainer, Name: cp.Name, Message: "container " + cp.Name + " binds " + strings.Join(cp.Binds, ", ")})
		}
		for _, n := range cp.networkNotes {
			progress(Event{Kind: EventNote, Item: KindContainer, Name: cp.Name, Message: "container " + cp.Name + ": " + n})
		}
		for _, r := range cp.Requires {
			progress(Event{Kind: EventNote, Item: KindContainer, Name: cp.Name, Message: "container " + cp.Name + ", confirmed: " + r})
		}
	}
}

// importContainer creates one planned container in hyperbutler and does not start it:
// Docker Desktop's may still hold its ports. Its restart policy does not start it either:
// dockerd restarts only containers that have run. Its image and volumes must have come
// along (they ran before it); a missing volume is refused, since a create would make an
// empty one that then stands in the way of importing the real one.
func (im *Importer) importContainer(ctx context.Context, st *runState, cp ContainerPlan, res *Result) ItemResult {
	r := ItemResult{Kind: KindContainer, Name: cp.Name, ID: cp.ID}
	refuse := func(why string) ItemResult {
		r.Outcome, r.Reason = OutcomeRefused, why
		return r
	}
	switch {
	case cp.Status == StatusAlready:
		r.Outcome, r.Reason = OutcomeAlready, cp.Reason
		return r
	case !cp.creatable():
		return refuse(cp.Reason)
	}
	switch clash, err := im.containerClash(ctx, &cp); {
	case err != nil:
		return failed(ctx, r, err)
	case clash == StatusAlready:
		r.Outcome, r.Reason = OutcomeAlready, clashReason[clash]
		return r
	case clash == StatusConflict:
		return refuse(clashReason[clash])
	}
	if why := notImported(res, KindImage, cp.ImageID, "image "+cp.Image); why != "" {
		return refuse(why)
	}
	if err := im.dst.JSON(ctx, http.MethodGet, "/images/"+cp.Image+"/json", nil, nil, nil); err != nil {
		if dockerapi.IsNotFound(err) {
			return refuse("its image " + cp.Image + " is not in hyperbutler: add it to the import")
		}
		return failed(ctx, r, err)
	}
	for _, name := range cp.Volumes {
		if why := notImported(res, KindVolume, name, "volume "+name); why != "" {
			return refuse(why)
		}
		v, err := im.dstVolume(ctx, name)
		if err != nil {
			return failed(ctx, r, err)
		}
		if v == nil {
			return refuse("its volume " + name + " is not in hyperbutler: add it to the import")
		}
	}
	for i := range cp.spec.networks {
		if err := im.ensureNetwork(ctx, st, &cp.spec.networks[i]); err != nil {
			return failed(ctx, r, err)
		}
	}
	var created struct {
		ID string `json:"Id"`
	}
	// The run's label tells a container this create made from one that took the name.
	body := cp.spec.body
	body.Labels = maps.Clone(body.Labels)
	if body.Labels == nil {
		body.Labels = map[string]string{}
	}
	body.Labels[source.RunLabel] = st.run.id
	err := im.dst.JSON(ctx, http.MethodPost, "/containers/create", url.Values{"name": {cp.Name}}, body, &created)
	switch {
	case dockerapi.StatusOf(err) == http.StatusConflict:
		return refuse(clashReason[StatusConflict])
	case err != nil:
		left := im.undoCreate(ctx, st, cp, dockerapi.StatusOf(err) != 0)
		return failedLeaving(ctx, r, fmt.Errorf("create it in hyperbutler: %w", err), left)
	}
	if err := im.verifyCreated(ctx, cp, created.ID); err != nil {
		left := ""
		if dockerapi.ValidName("container id", created.ID) != nil {
			left = im.undoCreate(ctx, st, cp, true)
		} else if derr := im.deleteDstHelper(ctx, created.ID); derr != nil {
			left = st.mayRemain(cp)
		}
		return failedLeaving(ctx, r, err, left)
	}
	st.keepNetworks(cp)
	r.Outcome = OutcomeImported
	r.Reason = "created, not started: stop Docker Desktop's first if it holds its ports, then docker start " + cp.Name
	// What the run did is no longer news; what stays behind still is.
	left := slices.DeleteFunc(slices.Clone(cp.Notes), func(n string) bool { return slices.Contains(cp.networkNotes, n) })
	if len(left) > 0 {
		r.Reason += "; " + strings.Join(left, "; ")
	}
	return r
}

// undoCreate deals with a create that failed, which need not mean nothing was made: a
// canceled request can have reached dockerd. It looks the name up on a context that
// outlives a cancel and removes a container with this run's label. It returns "" when
// nothing remains, else what to tell the user. answered says dockerd answered the create;
// otherwise it may still be working on it, so it looks again a little later.
func (im *Importer) undoCreate(ctx context.Context, st *runState, cp ContainerPlan, answered bool) string {
	cctx, cancel := cleanupCtx(ctx)
	defer cancel()
	var got struct {
		ID     string `json:"Id"`
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
	}
	look := func() error { return im.dst.JSON(cctx, http.MethodGet, "/containers/"+cp.Name+"/json", nil, nil, &got) }
	err := look()
	if !answered {
		for _, d := range removeRetries {
			if !dockerapi.IsNotFound(err) {
				break
			}
			t := time.NewTimer(d)
			<-t.C
			err = look()
		}
	}
	switch {
	case dockerapi.IsNotFound(err):
		return ""
	case err != nil, got.Config.Labels[source.RunLabel] != st.run.id, dockerapi.ValidName("container id", got.ID) != nil:
		return st.mayRemain(cp)
	}
	if err := im.deleteDstHelper(ctx, got.ID); err != nil {
		return st.mayRemain(cp)
	}
	return ""
}

// mayRemain keeps the networks of a container that may still be there, which removing
// them would break, and says what to do about it.
func (st *runState) mayRemain(cp ContainerPlan) string {
	st.keepNetworks(cp)
	return "a container named " + cp.Name + " may remain in hyperbutler; check it, and remove it if it should not be there: docker --context hyperbutler rm -v " + cp.Name
}

// keepNetworks marks the networks this run made for cp as in use.
func (st *runState) keepNetworks(cp ContainerPlan) {
	for _, n := range cp.spec.networks {
		if _, ok := st.newNetworks[n.Name]; ok {
			st.newNetworks[n.Name] = true
		}
	}
}

// failedLeaving is failed, for a container: left, when set, replaces its claim that
// nothing is left.
func failedLeaving(ctx context.Context, r ItemResult, err error, left string) ItemResult {
	r = failed(ctx, r, err)
	if left != "" {
		r.Reason = strings.TrimSuffix(r.Reason, "nothing of it is left in hyperbutler") + left
	}
	return r
}

// notImported says why an item a container needs is not usable: it was in this run and
// did not make it. An item not in the run was in hyperbutler at the plan; the caller
// checks it still is.
func notImported(res *Result, kind, key, what string) string {
	for _, it := range res.Items {
		if it.Kind != kind || (kind == KindImage && it.ID != key) || (kind == KindVolume && it.Name != key) {
			continue
		}
		switch it.Outcome {
		case OutcomeImported, OutcomeTagged, OutcomeAlready:
			return ""
		}
		return "its " + what + " was not imported (" + string(it.Outcome) + "): " + it.Reason
	}
	return ""
}

// verifyCreated checks hyperbutler has the container, made from this one and not started.
func (im *Importer) verifyCreated(ctx context.Context, cp ContainerPlan, id string) error {
	if err := dockerapi.ValidName("container id", id); err != nil {
		return err
	}
	var got struct {
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
		State struct {
			Status string `json:"Status"`
		} `json:"State"`
	}
	if err := im.dst.JSON(ctx, http.MethodGet, "/containers/"+id+"/json", nil, nil, &got); err != nil {
		return fmt.Errorf("verify it in hyperbutler: %w", err)
	}
	if got.Config.Labels[containerFromLabel] != cp.ID || got.State.Status != "created" {
		return fmt.Errorf("verification failed: hyperbutler's container is %q, not a new copy of this one", clean(got.State.Status))
	}
	return nil
}

// ensureNetwork creates a network hyperbutler does not have; one of that name is used as
// it is.
func (im *Importer) ensureNetwork(ctx context.Context, st *runState, ns *networkSpec) error {
	err := im.dst.JSON(ctx, http.MethodGet, "/networks/"+ns.Name, nil, nil, nil)
	switch {
	case err == nil:
		return nil
	case !dockerapi.IsNotFound(err):
		return fmt.Errorf("hyperbutler's network %s: %w", ns.Name, err)
	}
	// Recorded first: a canceled create may still have made it.
	if _, ok := st.newNetworks[ns.Name]; !ok {
		st.newNetworks[ns.Name] = false
	}
	if err := im.dst.JSON(ctx, http.MethodPost, "/networks/create", nil, ns, nil); err != nil {
		return fmt.Errorf("create the network %s in hyperbutler: %w", ns.Name, err)
	}
	return nil
}

// removeUnusedNetworks removes the networks this run created for containers that then
// failed, so a failed container leaves nothing behind. dockerd would let it remove one a
// created container names, which is why that container's are marked (mayRemain).
func (im *Importer) removeUnusedNetworks(ctx context.Context, st *runState) {
	for _, name := range sortedKeys(st.newNetworks) {
		if st.newNetworks[name] {
			continue
		}
		cctx, cancel := cleanupCtx(ctx)
		err := im.dst.JSON(cctx, http.MethodDelete, "/networks/"+name, nil, nil, nil)
		cancel()
		if err != nil && !dockerapi.IsNotFound(err) {
			im.note("could not remove the network %s this import created in hyperbutler (%v)", name, err)
		}
	}
}
