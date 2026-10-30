package engine

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"
)

// Auto-publishing: when a container with published ports starts in the guest, dockerd's
// docker-proxy listens on the guest's eth0 for each host port. winbox mirrors each
// one on the host's loopback, so `docker run -p 8080:80` is reachable at
// 127.0.0.1:8080 on Windows. The JSON comes from the guest's dockerd and is untrusted.

const (
	maxInspectBytes      = 1 << 20
	maxPortsPerContainer = 64
)

type portBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

type containerInspect struct {
	ID              string `json:"Id"`
	NetworkSettings struct {
		Ports map[string][]portBinding `json:"Ports"`
	} `json:"NetworkSettings"`
}

// publishedPorts returns the guest TCP host ports that a container publishes on all
// guest addresses or on guestIP. Bindings to another guest address (for example
// 127.0.0.1 inside the guest) are not reachable through the guest's eth0 and are
// skipped.
// skipped counts the TCP bindings dropped for that reason, so the caller can say why
// such a port is not reachable.
func publishedPorts(inspect []byte, guestIP netip.Addr) (id string, ports []uint16, skipped int, err error) {
	if len(inspect) > maxInspectBytes {
		return "", nil, 0, errors.New("container inspect document is too large")
	}
	var c containerInspect
	if err := json.Unmarshal(inspect, &c); err != nil {
		return "", nil, 0, fmt.Errorf("container inspect: %w", err)
	}
	if !validContainerID(c.ID) {
		return "", nil, 0, fmt.Errorf("container inspect: bad id %q", c.ID)
	}
	seen := map[uint16]bool{}
	for key, bindings := range c.NetworkSettings.Ports {
		_, proto, _ := strings.Cut(key, "/")
		if proto != "tcp" {
			continue
		}
		for _, b := range bindings {
			if !reachableBinding(b.HostIP, guestIP) {
				skipped++
				continue
			}
			p, err := strconv.ParseUint(b.HostPort, 10, 16)
			if err != nil || p == 0 || seen[uint16(p)] {
				continue
			}
			if len(seen) >= maxPortsPerContainer {
				return "", nil, 0, fmt.Errorf("container %s publishes more than %d ports", c.ID[:12], maxPortsPerContainer)
			}
			seen[uint16(p)] = true
			ports = append(ports, uint16(p))
		}
	}
	sort.Slice(ports, func(i, j int) bool { return ports[i] < ports[j] })
	return c.ID, ports, skipped, nil
}

// maxEventLine bounds one /events message. dockerd writes one JSON object per line;
// real ones are a few hundred bytes.
const maxEventLine = 64 << 10

var errEventTooLong = errors.New("docker event longer than 64 KiB")

// readEventLine returns the next line of the event stream without buffering more than
// maxEventLine bytes of it (br must be at least that large).
func readEventLine(br *bufio.Reader) ([]byte, error) {
	line, err := br.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		return nil, errEventTooLong
	}
	return line, err
}

func reachableBinding(hostIP string, guestIP netip.Addr) bool {
	if hostIP == "" {
		return true
	}
	a, err := netip.ParseAddr(hostIP)
	if err != nil {
		return false
	}
	return a.IsUnspecified() || a.Unmap() == guestIP
}

// validContainerID accepts docker's 64 hex digit IDs.
func validContainerID(id string) bool {
	if len(id) != 64 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// dockerEvent is the subset of a /events message the engine reacts to.
type dockerEvent struct {
	Type   string `json:"Type"`
	Action string `json:"Action"`
	Actor  struct {
		ID string `json:"ID"`
	} `json:"Actor"`
}
