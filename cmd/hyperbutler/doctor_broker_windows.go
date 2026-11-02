//go:build windows

package main

import (
	"context"
	"errors"
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
	// hcsAccess reports whether this process may call HCS itself (nil: assume so).
	hcsAccess func() bool
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
	hcsAccess: canUseHCS,
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
// pipe name. hyperbutler start boots through the service when it is installed (starting
// it when it is stopped) and in its own process otherwise (ADR 0004, Installer (M4)),
// so only a service start cannot use is a failure: one with an older protocol. Without
// the service, a user who cannot use HCS directly (not in Hyper-V Administrators, not
// elevated) gets a warning: start runs the VM itself and HCS refuses it.
func brokerCheck(p brokerProbe) DoctorCheck {
	pipe := broker.ServicePipe
	c := DoctorCheck{ID: "broker", Args: msgArgs("service", broker.ServiceName, "pipe", pipe)}
	st, err := p.query()
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		// The service admits interactive sign-ins and services only; this one (ssh, a
		// batch task) is neither, so start cannot use it from here.
		c.Msg = "doctor.broker.notInteractive"
		if p.hcsAccess != nil && p.hcsAccess() {
			c.Status = doctorInfo
			c.Summary = "this sign-in is not interactive, so the broker service does not serve it; hyperbutler start runs the VM itself, with your Hyper-V access"
			return c
		}
		c.Status = doctorWarn
		c.Summary = "this sign-in is not interactive, so the broker service does not serve it, and your account cannot use HCS itself: hyperbutler start cannot start the VM from here"
		c.Fix = "Start hyperbutler from an interactive sign-in (your desktop), or an administrator runs sc.exe start " + broker.ServiceName + "."
		c.FixMsg = "doctor.fix.brokerInteractive"
		return c
	}
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
	case !st.Installed && p.hcsAccess != nil && !p.hcsAccess():
		c.Status = doctorWarn
		c.Summary = "the broker service is not installed, and your account cannot use HCS itself (not in Hyper-V Administrators, not elevated), so hyperbutler start cannot start the VM"
		c.Fix = "Install hyperbutler with its installer (the .msi of the release): it installs the broker service, and then no group membership is needed (docs/install.md)."
		c.Msg, c.FixMsg = "doctor.broker.absentNoAccess", "doctor.fix.brokerInstall"
		return c
	case !st.Installed:
		c.Status, c.Summary = doctorInfo, "the broker service is not installed; hyperbutler start runs the VM in its own process (Hyper-V Administrators or elevation)"
		c.Msg = "doctor.broker.absent"
		return c
	case st.State == svc.Stopped && p.exists(pipe):
		return squatted()
	case st.State == svc.Stopped && st.Disabled:
		// start does not fall back while the service is installed (ADR 0004, M4).
		c.Status, c.Summary = doctorFail, "the broker service is disabled, so hyperbutler start cannot start the VM through it"
		c.Fix = "An administrator enables it again: sc.exe config " + broker.ServiceName + " start= delayed-auto, then sc.exe start " + broker.ServiceName + " (a repair of hyperbutler leaves a disabled service alone)."
		c.Msg, c.FixMsg = "doctor.broker.disabled", "doctor.fix.brokerEnable"
		return c
	case st.State == svc.Stopped:
		c.Status, c.Summary = doctorWarn, "the broker service is installed but stopped; hyperbutler start starts it"
		c.Fix = "Run hyperbutler start from your desktop sign-in, which starts the service; or an administrator runs sc.exe start " + broker.ServiceName + ". If it stops again, the Application event log (source " + broker.EventSource + ") says why."
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
		c.Status = doctorFail // start boots through it and refuses its protocol
		c.Summary = fmt.Sprintf("the broker service is version %s, protocol %d; this hyperbutler needs protocol %d or later", v, pv, minBrokerProto)
		c.Fix = "Install the hyperbutler release that matches this one: it updates the broker, which needs administrator rights once."
		c.Msg, c.FixMsg = "doctor.broker.old", "doctor.fix.brokerUpgrade"
		return c
	}
	c.Status, c.Summary = doctorOK, fmt.Sprintf("the broker service is running, version %s (protocol %d)", v, pv)
	c.Msg = "doctor.broker.ok"
	return c
}
