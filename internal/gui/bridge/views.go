package bridge

import (
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Higangssh/hyperbutler/internal/config"
	"github.com/Higangssh/hyperbutler/internal/dockerapi"
	"github.com/Higangssh/hyperbutler/internal/engine"
	"github.com/Higangssh/hyperbutler/internal/redact"
	"github.com/Higangssh/hyperbutler/internal/schema"
)

// The views are what the page renders: only the fields it shows, every string that
// someone else wrote cleaned (no control or bidirectional characters), redacted and
// bounded. The page puts them in the document with textContent only.

// maxField bounds one short string (a name, an image, a status).
const maxField = 512

// field is a short string as the page gets it.
func field(s string) string {
	return dockerapi.Clip(dockerapi.CleanText(redact.String(s), false), maxField)
}

// StatusView is the home screen's engine state.
type StatusView struct {
	// Phase is notsetup, stopped, starting, running, stopping or failed.
	Phase   string `json:"phase"`
	Running bool   `json:"running"`
	SetUp   bool   `json:"setUp"`
	// FirstStart says the engine never started here: no data disk, no state.json. The
	// window shows its first-run screen then; settings.json alone (the language picker
	// or a setting saved before the first start) does not count as started.
	FirstStart bool   `json:"firstStart"`
	MemoryMB   int    `json:"memoryMB"`
	CPUs       int    `json:"cpus"`
	DiskUsed   uint64 `json:"diskUsed"`
	DiskMax    uint64 `json:"diskMax"`
	HostFree   uint64 `json:"hostFree"`
	// GuestUsed and GuestSize are the guest file system as the guest reported it.
	GuestUsed uint64 `json:"guestUsed,omitempty"`
	GuestSize uint64 `json:"guestSize,omitempty"`
	// UpSeconds is how long the engine has run.
	UpSeconds int64 `json:"upSeconds,omitempty"`
	// Paused is set while the free-space guard keeps the VM paused.
	Paused bool `json:"paused"`
	// Error is the last failed start, as a code and its English original.
	Error *ErrorView `json:"error,omitempty"`
	Home  string     `json:"home"`
}

// engineCodes maps the engine's state.json error codes to the GUI's.
var engineCodes = map[string]string{
	engine.CodeHCSAccessDenied:  CodeAccessDenied,
	engine.CodeHCSSignInPending: CodeSignInPending,
	engine.CodePipeInUse:        CodePipeInUse,
	engine.CodeAlreadyRunning:   CodeOtherEngine,
	engine.CodeVMExited:         CodeVMExited,
	engine.CodeDataDisk:         CodeDataDisk,
}

// EngineCode is the GUI code of a state.json error code (CodeStartFailed for others).
func EngineCode(c string) string {
	if g, ok := engineCodes[c]; ok {
		return g
	}
	return CodeStartFailed
}

// MakeStatus is the view of a status document.
func MakeStatus(s schema.Status) StatusView {
	v := StatusView{Running: s.Running, SetUp: s.SetUp, MemoryMB: s.MemoryMB, CPUs: s.CPUs,
		DiskUsed: s.DiskUsed, DiskMax: s.DiskMax, HostFree: s.HostFree, Paused: s.GuardPause != nil, Home: field(s.Home)}
	v.FirstStart = !s.Running && s.State == nil && s.Disk == ""
	if s.GuestDisk != nil {
		v.GuestUsed, v.GuestSize = s.GuestDisk.UsedBytes, s.GuestDisk.SizeBytes
	}
	st := s.State
	switch {
	case !s.SetUp:
		v.Phase = "notsetup"
	case s.Running && st != nil && st.Phase.Live():
		v.Phase = string(st.Phase)
		if !st.Started.IsZero() {
			v.UpSeconds = int64(time.Since(st.Started) / time.Second)
		}
	case s.Running:
		v.Phase = "starting"
	case st != nil && st.Phase == engine.PhaseFailed:
		v.Phase = "failed"
		v.Error = &ErrorView{Code: EngineCode(st.ErrorCode), Detail: dockerapi.Clip(dockerapi.CleanText(redact.String(st.Error), true), maxDetail)}
	default:
		v.Phase = "stopped"
	}
	return v
}

// ContainerView is one container in the list.
type ContainerView struct {
	ID      string   `json:"id"` // the full id, which container.action and logs take
	Name    string   `json:"name"`
	Image   string   `json:"image"`
	State   string   `json:"state"` // dockerd's: running, exited, created, paused, ...
	Status  string   `json:"status"`
	Service string   `json:"service,omitempty"`
	Ports   []string `json:"ports"`
}

// GroupView is a Compose project (Project empty: containers of none).
type GroupView struct {
	Project    string          `json:"project"`
	Running    int             `json:"running"`
	Containers []ContainerView `json:"containers"`
}

// ContainersView is the container list, grouped by Compose project (REQ-UX-011).
type ContainersView struct {
	Groups    []GroupView `json:"groups"`
	Total     int         `json:"total"`
	Running   int         `json:"running"`
	Truncated bool        `json:"truncated"`
}

// MakeContainers groups list by Compose project: projects by name, then the
// containers of no project; within a group, by name.
func MakeContainers(list []dockerapi.Container) ContainersView {
	v := ContainersView{Groups: []GroupView{}, Total: len(list)}
	if len(list) > maxContainers {
		list, v.Truncated = list[:maxContainers], true
	}
	groups := map[string]*GroupView{}
	for _, c := range list {
		if !reContainerID.MatchString(c.ID) {
			continue // not an id the page could act on
		}
		cv := ContainerView{ID: c.ID, Name: field(dockerapi.ContainerName(c.ID, c.Names)), Image: field(c.Image),
			State: field(c.State), Status: field(c.Status), Ports: []string{},
			Service: dockerapi.ComposeLabel(c.Labels[dockerapi.LabelComposeService])}
		for i, p := range c.Ports {
			if i == 16 {
				break
			}
			cv.Ports = append(cv.Ports, field(dockerapi.PortText(p)))
		}
		proj := dockerapi.ComposeLabel(c.Labels[dockerapi.LabelComposeProject])
		g := groups[proj]
		if g == nil {
			g = &GroupView{Project: proj, Containers: []ContainerView{}}
			groups[proj] = g
		}
		if c.State == "running" {
			g.Running++
			v.Running++
		}
		g.Containers = append(g.Containers, cv)
	}
	names := make([]string, 0, len(groups))
	for k := range groups {
		names = append(names, k)
	}
	sort.Slice(names, func(i, j int) bool {
		if (names[i] == "") != (names[j] == "") {
			return names[j] == "" // no project last
		}
		return names[i] < names[j]
	})
	for _, n := range names {
		g := groups[n]
		sort.SliceStable(g.Containers, func(i, j int) bool { return g.Containers[i].Name < g.Containers[j].Name })
		v.Groups = append(v.Groups, *g)
	}
	return v
}

// SettingView is one setting. Description is the English one from internal/config;
// the page shows its own translation by key and falls back to it.
type SettingView struct {
	Key      string `json:"key"`
	Value    string `json:"value"`
	Default  string `json:"default"`
	Source   string `json:"source"`
	Type     string `json:"type"`
	Apply    string `json:"apply"`
	Editable bool   `json:"editable"`
	// Confirm says a change needs a confirmation (a token for the key and value).
	Confirm     bool   `json:"confirm,omitempty"`
	Description string `json:"description"`
}

// SettingsView is the settings list.
type SettingsView struct {
	Keys []SettingView `json:"keys"`
}

// MakeSettings is the view of `config list --json`. Values are redacted again: the
// list already hides proxy passwords, and this keeps any other secret out too.
func MakeSettings(l schema.ConfigList) SettingsView {
	v := SettingsView{Keys: []SettingView{}}
	for _, it := range l.Keys {
		v.Keys = append(v.Keys, SettingView{Key: it.Key, Value: field(it.Value), Default: field(it.Default), Source: string(it.Source),
			Type: it.Type, Apply: string(it.Apply), Editable: Editable[it.Key] != CLIOnly, Confirm: Editable[it.Key] == Confirmed,
			Description: field(it.Description)})
	}
	return v
}

// ApplyTimes are the values of SettingView.Apply, for the catalog test.
var ApplyTimes = []config.ApplyTime{config.ApplyLive, config.ApplyDockerd, config.ApplyVM, config.ApplyFirstStart}

// DoctorView is doctor's checks. Summary and Fix are the CLI's English text; the page
// shows a translated title by ID, and Msg and FixMsg with Args where its catalog has
// those sentences.
type DoctorView struct {
	OK     bool          `json:"ok"`
	Checks []CheckResult `json:"checks"`
}

// CheckResult is one check.
type CheckResult struct {
	ID      string `json:"id"`
	Status  string `json:"status"` // ok, info, warn, fail
	Summary string `json:"summary"`
	Fix     string `json:"fix,omitempty"`
	// Msg and FixMsg are catalog keys ("doctor.pipe.free"), Args their values.
	Msg    string            `json:"msg,omitempty"`
	FixMsg string            `json:"fixMsg,omitempty"`
	Args   map[string]string `json:"args,omitempty"`
}

// reCheckKey is a catalog key a check may name; reCheckArg an argument name.
var (
	reCheckKey = regexp.MustCompile(`^doctor\.[a-z][a-zA-Z0-9.-]{0,63}$`)
	reCheckArg = regexp.MustCompile(`^[a-z][a-zA-Z0-9]{0,15}$`)
)

// checkKey is k when it is a catalog key a check may name, else empty.
func checkKey(k string) string {
	if reCheckKey.MatchString(k) {
		return k
	}
	return ""
}

// doctorArgs keeps at most 8 well-named arguments, cleaned like the summary.
func doctorArgs(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := map[string]string{}
	for k, v := range in {
		if len(out) == 8 {
			break
		}
		if reCheckArg.MatchString(k) {
			out[k] = dockerapi.Clip(dockerapi.CleanText(redact.String(v), false), 512)
		}
	}
	return out
}

// MakeDoctor is the view of a doctor result.
func MakeDoctor(r schema.Doctor) DoctorView {
	v := DoctorView{OK: r.OK, Checks: []CheckResult{}}
	for i, c := range r.Checks {
		if i == 64 {
			break
		}
		st := c.Status
		switch st {
		case "ok", "info", "warn", "fail":
		default:
			st = "info"
		}
		v.Checks = append(v.Checks, CheckResult{ID: field(c.ID), Status: st,
			Summary: dockerapi.Clip(dockerapi.CleanText(redact.String(c.Summary), true), 2048),
			Fix:     dockerapi.Clip(dockerapi.CleanText(redact.String(c.Fix), true), 2048),
			Msg:     checkKey(c.Msg), FixMsg: checkKey(c.FixMsg), Args: doctorArgs(c.Args)})
	}
	return v
}

// DiagnoseView is the zip diagnose wrote.
type DiagnoseView struct {
	File    string `json:"file"`
	Files   int    `json:"files"`
	Missing int    `json:"missing"`
}

// StorageView is the storage screen (read-only in this release).
type StorageView struct {
	DiskUsed  uint64 `json:"diskUsed"`
	DiskMax   uint64 `json:"diskMax"`
	HostFree  uint64 `json:"hostFree"`
	GuestUsed uint64 `json:"guestUsed"`
	GuestSize uint64 `json:"guestSize"`
	// Engine says the engine runs, so the Docker numbers below are known.
	Engine      bool         `json:"engine"`
	ImagesSize  int64        `json:"imagesSize"`
	VolumesSize int64        `json:"volumesSize"`
	BuildCache  int64        `json:"buildCache"`
	Images      []ImageView  `json:"images"`
	Volumes     []VolumeView `json:"volumes"`
	ImageCount  int          `json:"imageCount"`
	VolumeCount int          `json:"volumeCount"`
}

// ImageView is one image.
type ImageView struct {
	Name       string `json:"name"`
	Size       int64  `json:"size"`
	Containers int64  `json:"containers"`
}

// VolumeView is one volume.
type VolumeView struct {
	Name  string `json:"name"`
	Size  int64  `json:"size"` // -1: unknown
	InUse bool   `json:"inUse"`
}

const maxStorageItems = 200

func fillStorage(v *StorageView, df dockerapi.DF) {
	v.ImageCount, v.VolumeCount = len(df.Images), len(df.Volumes)
	for _, im := range df.Images {
		v.ImagesSize += max(im.Size-max(im.SharedSize, 0), 0)
		if len(v.Images) == maxStorageItems {
			continue
		}
		name := dockerapi.ShortID(im.ID)
		for _, t := range im.RepoTags {
			if t != "<none>:<none>" {
				name = t
				break
			}
		}
		v.Images = append(v.Images, ImageView{Name: field(name), Size: max(im.Size, 0), Containers: max(im.Containers, 0)})
	}
	for _, vol := range df.Volumes {
		size, inUse := int64(-1), false
		if vol.UsageData != nil {
			size, inUse = vol.UsageData.Size, vol.UsageData.RefCount > 0
			v.VolumesSize += max(size, 0)
		}
		if len(v.Volumes) < maxStorageItems {
			v.Volumes = append(v.Volumes, VolumeView{Name: field(vol.Name), Size: size, InUse: inUse})
		}
	}
	for _, b := range df.BuildCache {
		if !b.Shared {
			v.BuildCache += max(b.Size, 0)
		}
	}
	sort.SliceStable(v.Images, func(i, j int) bool { return v.Images[i].Size > v.Images[j].Size })
	sort.SliceStable(v.Volumes, func(i, j int) bool { return strings.Compare(v.Volumes[i].Name, v.Volumes[j].Name) < 0 })
}
