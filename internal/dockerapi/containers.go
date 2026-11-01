package dockerapi

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ShortID is an image or container id's first 12 hex digits, without "sha256:".
func ShortID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		id = id[:12]
	}
	return id
}

// ContainerName is a container's name without the leading "/", or its short id.
func ContainerName(id string, names []string) string {
	for _, n := range names {
		if n = strings.TrimPrefix(n, "/"); n != "" && !strings.Contains(n, "/") {
			return n
		}
	}
	return ShortID(id)
}

// ComposeLabel is a Compose project or service label when it looks like a name Compose
// makes, or empty. Labels are free text anyone can set with docker run --label, so other
// text is left out.
func ComposeLabel(v string) string {
	if len(v) > 64 || !reName.MatchString(v) {
		return ""
	}
	return v
}

// The Compose labels.
const (
	LabelComposeProject = "com.docker.compose.project"
	LabelComposeService = "com.docker.compose.service"
)

// PortText is a published port as `docker ps` shows it.
func PortText(p Port) string {
	if p.PublicPort == 0 {
		return fmt.Sprintf("%d/%s", p.PrivatePort, p.Type)
	}
	ip := p.IP
	if strings.Contains(ip, ":") {
		ip = "[" + ip + "]"
	}
	return fmt.Sprintf("%s:%d->%d/%s", ip, p.PublicPort, p.PrivatePort, p.Type)
}

// RestartPolicies inspects up to maxN of list's containers, from the first, atOnce at a
// time, and returns the restart policy of each by index ("" for a container removed
// meanwhile). An index is missing when the cap or deadline left it out. Any error other
// than a container's 404 fails the call.
func RestartPolicies(ctx context.Context, d *Client, list []Container, deadline time.Time, maxN, atOnce int) (map[int]string, error) {
	n := min(len(list), maxN)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		mu    sync.Mutex
		out   = map[int]string{}
		first error
		wg    sync.WaitGroup
		next  = make(chan int)
	)
	for range min(n, atOnce) {
		wg.Go(func() {
			for i := range next {
				var ins Inspect
				err := d.GetJSON(ctx, "/containers/"+url.PathEscape(list[i].ID)+"/json", nil, &ins)
				mu.Lock()
				switch {
				case err == nil:
					out[i] = ins.HostConfig.RestartPolicy.Name
				case IsNotFound(err): // removed meanwhile: it has none
					out[i] = ""
				case first == nil:
					first = err
					cancel()
				}
				mu.Unlock()
			}
		})
	}
	for i := range n {
		if time.Now().After(deadline) || ctx.Err() != nil {
			break
		}
		next <- i
	}
	close(next)
	wg.Wait()
	if first != nil {
		return nil, first
	}
	return out, nil
}

// REQ-UX-017: before a stop or restart of the VM or of dockerd, every front end says
// which running containers stop and which of them come back.

// RunningContainer is a running container that a stop or restart is about to stop.
type RunningContainer struct {
	Name  string `json:"name"`
	Image string `json:"image"`
	// RestartPolicy is its restart policy's name as dockerd has it ("always",
	// "unless-stopped", "on-failure", "no"); empty for none.
	RestartPolicy string `json:"restartPolicy"`
	// ComesBack says dockerd starts it again when it starts: the policy is always or
	// unless-stopped.
	ComesBack bool `json:"comesBack"`
}

// ComesBack reports whether a container with restart policy p is started again when
// dockerd starts after a stop.
func ComesBack(p string) bool { return p == "always" || p == "unless-stopped" }

// MayComeBack reports whether c comes back depending on how it stops: on-failure
// restarts a container whose exit code at the stop is not 0, such as one killed after
// it ignored SIGTERM, but not one that exits cleanly.
func (c RunningContainer) MayComeBack() bool { return !c.ComesBack && c.RestartPolicy == "on-failure" }

// ListRunning asks for the running containers and their restart policies (one inspect
// each).
func ListRunning(ctx context.Context, d *Client) ([]RunningContainer, error) {
	var list []Container
	if err := d.GetJSON(ctx, "/containers/json", nil, &list); err != nil {
		return nil, err
	}
	out := make([]RunningContainer, 0, len(list))
	for _, c := range list {
		rc := RunningContainer{Name: ContainerName(c.ID, c.Names), Image: c.Image}
		var in Inspect
		if err := d.GetJSON(ctx, "/containers/"+url.PathEscape(c.ID)+"/json", nil, &in); err != nil {
			return nil, err
		}
		rc.RestartPolicy = in.HostConfig.RestartPolicy.Name
		rc.ComesBack = ComesBack(rc.RestartPolicy)
		out = append(out, rc)
	}
	return out, nil
}
