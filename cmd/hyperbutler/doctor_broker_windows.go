//go:build windows

package main

import (
	"context"
	"fmt"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"

	"github.com/Higangssh/hyperbutler/internal/broker"
	"github.com/Higangssh/hyperbutler/internal/broker/proto"
)

// minBrokerProto is the oldest broker protocol this CLI speaks. A broker that reports
// an older one needs the broker of this release, which an administrator installs once
// (ADR 0004, Upgrades).
const minBrokerProto = proto.Version

// brokerProbe is what brokerCheck reads of the machine; tests replace it.
type brokerProbe struct {
	query   func() (broker.ServiceState, error)
	exists  func(pipe string) bool
	version func(pipe string) (uint8, string, error)
	// owner returns the SID of the pipe's owner, and self this user's SID.
	owner func(pipe string) (string, error)
	self  func() (string, error)
}

var machineBroker = brokerProbe{
	query:   func() (broker.ServiceState, error) { return broker.QueryService(broker.ServiceName) },
	exists:  func(pipe string) bool { return pipeExists(pipe) },
	version: brokerVersion,
	owner:   pipeOwner,
	self: func() (string, error) {
		tu, err := windows.GetCurrentProcessToken().GetTokenUser()
		if err != nil {
			return "", err
		}
		return tu.User.Sid.String(), nil
	},
}

// pipeOwner reads the owner of the pipe's security descriptor. It opens the pipe for
// READ_CONTROL only, at anonymous impersonation level, so whoever serves it learns
// nothing about this user; the open takes one of its instances for a moment.
func pipeOwner(pipe string) (string, error) {
	p, err := windows.UTF16PtrFromString(pipe)
	if err != nil {
		return "", err
	}
	h, err := windows.CreateFile(p, windows.READ_CONTROL, 0, nil, windows.OPEN_EXISTING,
		windows.SECURITY_SQOS_PRESENT|windows.SECURITY_ANONYMOUS, 0)
	if err != nil {
		return "", err
	}
	defer func() { _ = windows.CloseHandle(h) }()
	sd, err := windows.GetSecurityInfo(h, windows.SE_KERNEL_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return "", err
	}
	o, _, err := sd.Owner()
	if err != nil || o == nil {
		return "", fmt.Errorf("no owner: %w", err)
	}
	return o.String(), nil
}

// brokerVersion asks the broker on pipe for its version. Dial checks first that the
// pipe is SYSTEM's (or a service's), so a program squatting on the name never gets a
// connection it could impersonate.
func brokerVersion(pipe string) (uint8, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, err := broker.Dial(ctx, pipe)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = c.Close() }()
	return c.Version()
}

// brokerCheck reports the HCS access broker service (ADR 0004): whether it is installed
// and running, its version against this CLI's, and whether another program holds its
// pipe name. hyperbutler start runs the VM in its own process for now, so nothing here
// blocks it: a problem is a warning.
func brokerCheck(p brokerProbe) DoctorCheck {
	pipe := broker.ServicePipe
	c := DoctorCheck{ID: "broker", Args: msgArgs("service", broker.ServiceName, "pipe", pipe)}
	st, err := p.query()
	if err != nil {
		c.Status, c.Summary = doctorWarn, "cannot query the broker service: "+err.Error()
		c.Msg, c.Args["detail"] = "doctor.broker.unknown", err.Error()
		return c
	}
	squatted := func() DoctorCheck {
		// A broker this user runs in the foreground with -pipe set to the service's name
		// owns its pipe as this user: that is not a squatter.
		if p.owner != nil && p.self != nil {
			o, oerr := p.owner(pipe)
			me, serr := p.self()
			if oerr == nil && serr == nil && o == me {
				c.Status = doctorInfo
				c.Summary = pipe + " is served by a hyperbutler-broker that you run yourself, in the foreground, not by the service"
				c.Msg = "doctor.broker.foreground"
				return c
			}
		}
		c.Status = doctorWarn
		c.Summary = fmt.Sprintf("%s exists although the broker service is not running: another program holds the broker's pipe name, so the broker cannot start", pipe)
		c.Fix = "Find and stop that program (it is not hyperbutler's), then start the service: sc.exe start " + broker.ServiceName + " (as administrator)."
		c.Msg, c.FixMsg = "doctor.broker.squatted", "doctor.fix.brokerSquatted"
		return c
	}
	switch {
	case !st.Installed && p.exists(pipe):
		return squatted()
	case !st.Installed:
		c.Status, c.Summary = doctorInfo, "the broker service is not installed; hyperbutler start runs the VM in its own process (Hyper-V Administrators)"
		c.Msg = "doctor.broker.absent"
		return c
	case st.State == svc.Stopped && p.exists(pipe):
		return squatted()
	case st.State == svc.Stopped:
		c.Status, c.Summary = doctorWarn, "the broker service is installed but stopped"
		c.Fix = "An administrator starts it: sc.exe start " + broker.ServiceName + ". If it stops again, the Application event log (source " + broker.EventSource + ") says why."
		c.Msg, c.FixMsg = "doctor.broker.stopped", "doctor.fix.brokerStart"
		return c
	case st.State != svc.Running:
		state := broker.StateName(st.State)
		c.Status, c.Summary = doctorInfo, "the broker service is "+state
		c.Msg, c.Args["state"] = "doctor.broker.pending", state
		return c
	}
	pv, v, err := p.version(pipe)
	if err != nil {
		c.Status, c.Summary = doctorWarn, "the broker service is running but does not answer on "+pipe+": "+err.Error()
		c.Fix = "An administrator restarts it: sc.exe stop " + broker.ServiceName + ", then sc.exe start " + broker.ServiceName + "."
		c.Msg, c.FixMsg, c.Args["detail"] = "doctor.broker.noAnswer", "doctor.fix.brokerRestart", err.Error()
		return c
	}
	c.Args["version"], c.Args["proto"], c.Args["min"] = v, fmt.Sprint(pv), fmt.Sprint(minBrokerProto)
	if pv < minBrokerProto {
		c.Status = doctorWarn
		c.Summary = fmt.Sprintf("the broker service is version %s, protocol %d; this hyperbutler needs protocol %d or later", v, pv, minBrokerProto)
		c.Fix = "Install the hyperbutler release that matches this one: it updates the broker, which needs administrator rights once."
		c.Msg, c.FixMsg = "doctor.broker.old", "doctor.fix.brokerUpgrade"
		return c
	}
	c.Status, c.Summary = doctorOK, fmt.Sprintf("the broker service is running, version %s (protocol %d)", v, pv)
	c.Msg = "doctor.broker.ok"
	return c
}
