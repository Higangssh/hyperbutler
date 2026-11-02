//go:build windows

package broker

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"
)

// The broker as a Windows service (ADR 0004, Implementation decision 1: LocalSystem for
// now). ServicePipe is its shared endpoint, which interactive users may connect to.
const (
	ServiceName        = "hyperbutler-broker"
	ServiceDisplayName = "hyperbutler broker"
	ServiceDescription = "Starts the hyperbutler VM for users without administrator rights. " +
		"It accepts only hyperbutler's own requests, from users signed in at this PC, over a local pipe."
	ServicePipe = `\\.\pipe\hyperbutler-broker`
	// ServicePipeInstances is the shared endpoint's instance count: with MaxConnsPerUser
	// 2, it serves up to 8 users at once.
	ServicePipeInstances = maxPipeInstances
	// EventSource is the Windows Event Log source of the service, registered at install
	// and removed at uninstall. The service writes its log there and nowhere else.
	EventSource = ServiceName
)

// Event IDs of the service's events. EventCreate.exe, the message file the source is
// registered with, has messages for IDs 1 to 1000 that print the text as it is.
const (
	eventInfo    = 1
	eventWarning = 2
	eventError   = 3
)

// Service-specific exit codes, which the SCM records and which trigger the recovery
// actions (InstallConfig.RecoverOnNonCrashFailures).
const (
	exitStartFailed  = 1
	exitPipeTaken    = 2
	exitServeStopped = 3
)

// Wait hints the service reports while it starts and stops, and how often it reports a
// new checkpoint in between, so the SCM sees progress and does not give up.
const (
	serviceStartHint = 60 * time.Second // the HCS backend's sweep may be slow
	serviceStopHint  = 30 * time.Second // leases end with a terminate each
	serviceTick      = 2 * time.Second
)

// Hard limits on starting and stopping. A backend that hangs (an HCS call that never
// returns, a start in flight) must not keep the service pending forever: past the
// limit the service gives up, logs it, and ends with exitServeStopped (or
// exitStartFailed), and the process exits. VMs whose handles it held go with it
// (ShouldTerminateOnLastHandleClosed, ADR 0004 rule 9).
const (
	serviceStartLimit = 3 * time.Minute
	serviceStopLimit  = 2 * time.Minute
)

// ServiceInstance is a running broker inside the service.
type ServiceInstance interface {
	// Done delivers Serve's result if it returns before Stop: the broker stopped
	// serving on its own.
	Done() <-chan error
	// Stop stops accepting, ends every connection (and so every lease: their VMs are
	// terminated), terminates the VMs left, and closes the pipe. It returns when done.
	Stop()
}

// Serving is a Server serving a PipeListener or Endpoints, as ServiceInstance.
type Serving struct {
	srv     *Server
	l       io.Closer
	cleanup func()
	done    chan error
	once    sync.Once
}

// startServing runs serve (srv's, on l) in a new goroutine; Stop closes l, which ends
// it. cleanup, if not nil, runs last in Stop, after the server is closed: it closes the
// backend, which terminates the VMs that no lease held.
func startServing(srv *Server, l io.Closer, serve func() error, cleanup func()) *Serving {
	s := &Serving{srv: srv, l: l, cleanup: cleanup, done: make(chan error, 1)}
	go func() { s.done <- serve() }()
	return s
}

func (s *Serving) Done() <-chan error { return s.done }

// Stop closes the listener, which interrupts every connection and waits for Serve to
// return; each connection's end releases its lease. Then it closes the server and runs
// cleanup. It is safe to call more than once.
func (s *Serving) Stop() {
	s.once.Do(func() {
		_ = s.l.Close()
		_ = s.srv.Close()
		if s.cleanup != nil {
			s.cleanup()
		}
	})
}

// ServiceConfig is what StartService builds the service's broker from.
type ServiceConfig struct {
	// Pipe is the endpoint (ServicePipe; tests use their own), with Instances instances.
	Pipe      string
	Instances int
	Version   string
	Backend   Backend
	// Sweep, if set, terminates the VMs an earlier broker left. It runs only once the
	// pipe is this broker's: a broker that cannot serve (another process holds the name,
	// which may be a broker still running its VMs) touches no VM.
	Sweep func() int
	// Cleanup, if set, runs when the broker stops, or at once if StartService fails.
	Cleanup func()
	// Infof receives the broker's events, and Warnf the server's internal errors.
	Infof, Warnf func(format string, args ...any)
}

// StartService creates the server and the pipe, sweeps, and starts serving.
func StartService(cfg ServiceConfig) (_ *Serving, err error) {
	defer func() {
		if err != nil && cfg.Cleanup != nil {
			cfg.Cleanup()
		}
	}()
	if cfg.Infof == nil {
		cfg.Infof = func(string, ...any) {}
	}
	srv, err := New(Config{Backend: cfg.Backend, Version: cfg.Version, Logf: cfg.Warnf})
	if err != nil {
		return nil, err
	}
	// The shared endpoint, and each user's own on request (ADR 0004 rule 5).
	eps, err := ListenEndpoints(srv, EndpointsConfig{Name: cfg.Pipe, Instances: cfg.Instances})
	if err != nil {
		_ = srv.Close()
		return nil, err
	}
	if cfg.Sweep != nil {
		if n := cfg.Sweep(); n > 0 {
			cfg.Infof("terminated %d VM(s) an earlier broker left", n)
		}
	}
	return startServing(srv, eps, eps.Serve, cfg.Cleanup), nil
}

// EventLogger is where the service writes its log. Info and Warning may be rate
// limited, since clients can cause them; Status and Error, the service's own lifecycle
// events, are not.
type EventLogger interface {
	Info(msg string)
	Warning(msg string)
	Error(msg string)
	Status(msg string)
}

// RunService runs the calling process as the Windows service ServiceName: start builds
// the broker, and the SCM's Stop and Shutdown stop it. It returns when the service has
// stopped. The process must have been started by the SCM (svc.IsWindowsService).
func RunService(start func() (ServiceInstance, error), log EventLogger) error {
	return svc.Run(ServiceName, &serviceHandler{start: start, log: log, tick: serviceTick,
		startLimit: serviceStartLimit, stopLimit: serviceStopLimit})
}

type serviceHandler struct {
	start                 func() (ServiceInstance, error)
	log                   EventLogger
	tick                  time.Duration
	startLimit, stopLimit time.Duration
}

func millis(d time.Duration) uint32 { return uint32(d / time.Millisecond) } //nolint:gosec // G115: the hints are constants under a minute

// pending runs fn while reporting state with a new checkpoint every tick, so the SCM
// sees progress within hint. It returns false if fn has not returned within limit; fn
// then keeps running, and the caller ends the service.
func (h *serviceHandler) pending(st chan<- svc.Status, state svc.State, hint, limit time.Duration, fn func()) bool {
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	t := time.NewTicker(h.tick)
	defer t.Stop()
	deadline := time.NewTimer(limit)
	defer deadline.Stop()
	cp := uint32(1)
	st <- svc.Status{State: state, WaitHint: millis(hint), CheckPoint: cp}
	for {
		select {
		case <-done:
			return true
		case <-deadline.C:
			return false
		case <-t.C:
			cp++
			st <- svc.Status{State: state, WaitHint: millis(hint), CheckPoint: cp}
		}
	}
}

// stop stops inst within the stop limit; it reports whether it did.
func (h *serviceHandler) stop(st chan<- svc.Status, inst ServiceInstance) bool {
	if h.pending(st, svc.StopPending, serviceStopHint, h.stopLimit, inst.Stop) {
		return true
	}
	h.log.Error(FormatEvent("hyperbutler broker did not stop within %v; the process exits, and the VMs it ran end with it", h.stopLimit))
	return false
}

// Execute is the service's main function (svc.Handler). A start that fails, and a
// broker that stops serving on its own, end the service with a service-specific exit
// code, so the SCM's recovery actions apply; a Stop or Shutdown ends it with 0.
func (h *serviceHandler) Execute(_ []string, req <-chan svc.ChangeRequest, st chan<- svc.Status) (bool, uint32) {
	var (
		inst ServiceInstance
		err  error
	)
	// inst and err are read only once pending saw the start return.
	if !h.pending(st, svc.StartPending, serviceStartHint, h.startLimit, func() { inst, err = h.start() }) {
		h.log.Error(FormatEvent("hyperbutler broker did not start within %v", h.startLimit))
		return true, exitStartFailed
	}
	if err != nil {
		if errors.Is(err, ErrPipeExists) {
			h.log.Error(FormatEvent("hyperbutler broker did not start: another program holds the pipe name %s (%v). "+
				"The broker does not fall back to another name; hyperbutler doctor reports this. Stop that program, then start the service.",
				ServicePipe, err))
			return true, exitPipeTaken
		}
		h.log.Error(FormatEvent("hyperbutler broker did not start: %v", err))
		return true, exitStartFailed
	}
	const accepts = svc.AcceptStop | svc.AcceptShutdown
	st <- svc.Status{State: svc.Running, Accepts: accepts}
	h.log.Status(FormatEvent("hyperbutler broker is serving %s", ServicePipe))
	for {
		select {
		case err := <-inst.Done():
			h.log.Error(FormatEvent("hyperbutler broker stopped serving: %v", err))
			h.stop(st, inst)
			return true, exitServeStopped
		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				st <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				if !h.stop(st, inst) {
					return true, exitServeStopped
				}
				h.log.Status(FormatEvent("hyperbutler broker stopped"))
				return false, 0
			default:
				h.log.Warning(FormatEvent("unexpected service control request %d", c.Cmd))
			}
		}
	}
}

// maxEventRunes caps one event's text; the event log takes up to 31839 characters.
const maxEventRunes = 4096

// FormatEvent formats an event log message. Messages carry error texts, which may
// quote what a caller sent, so the result is valid UTF-8 without control characters
// (except tab and newline), without "%%" (which the event viewer would expand as a
// parameter insertion), and at most maxEventRunes long.
func FormatEvent(format string, args ...any) string {
	s := strings.ToValidUTF8(fmt.Sprintf(format, args...), "�")
	var b strings.Builder
	n := 0
	prevPct := false
	for _, r := range s {
		space := false
		switch {
		case r == '\t' || r == '\n':
		case r < 0x20 || r == 0x7f || r >= 0x80 && r < 0xa0:
			r = '?'
		case r == '%' && prevPct:
			// Break "%%" up: "%%n" is a parameter insertion to the event viewer.
			space = true
		}
		need := 1
		if space {
			need = 2
		}
		if n+need > maxEventRunes {
			b.WriteString("…")
			break
		}
		if space {
			b.WriteRune(' ')
		}
		prevPct = r == '%'
		b.WriteRune(r)
		n += need
	}
	return b.String()
}

// eventWriter is the part of eventlog.Log that EventLog uses.
type eventWriter interface {
	Info(eid uint32, msg string) error
	Warning(eid uint32, msg string) error
	Error(eid uint32, msg string) error
	Close() error
}

// maxEventsPerWindow bounds the info and warning events per eventWindow, so a client
// that makes the broker log errors cannot flood the event log. Errors are not limited:
// only the service's own start and stop write them.
const (
	maxEventsPerWindow = 30
	eventWindow        = time.Minute
)

// EventLog writes the service's log to the Windows Event Log, under EventSource.
type EventLog struct {
	w   eventWriter
	now func() time.Time

	mu      sync.Mutex
	window  time.Time
	n       int
	dropped int
}

// OpenEventLog opens the event log for EventSource.
func OpenEventLog() (*EventLog, error) {
	l, err := eventlog.Open(EventSource)
	if err != nil {
		return nil, err
	}
	return newEventLog(l, time.Now), nil
}

func newEventLog(w eventWriter, now func() time.Time) *EventLog {
	return &EventLog{w: w, now: now}
}

// Close closes the event log.
func (l *EventLog) Close() error { return l.w.Close() }

// admit reports whether one more info or warning event fits in the current window, and
// returns how many were dropped in the window before, to be reported once.
func (l *EventLog) admit() (ok bool, dropped int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now := l.now(); now.Sub(l.window) >= eventWindow {
		dropped, l.dropped = l.dropped, 0
		l.window, l.n = now, 0
	}
	if l.n >= maxEventsPerWindow {
		l.dropped++
		return false, dropped
	}
	l.n++
	return true, dropped
}

func (l *EventLog) limited(write func(uint32, string) error, eid uint32, msg string) {
	ok, dropped := l.admit()
	if dropped > 0 {
		_ = l.w.Warning(eventWarning, FormatEvent("%d messages were not logged (more than %d a minute)", dropped, maxEventsPerWindow))
	}
	if ok {
		_ = write(eid, msg)
	}
}

func (l *EventLog) Info(msg string)    { l.limited(l.w.Info, eventInfo, msg) }
func (l *EventLog) Warning(msg string) { l.limited(l.w.Warning, eventWarning, msg) }
func (l *EventLog) Error(msg string)   { _ = l.w.Error(eventError, msg) }

// Status logs one of the service's own lifecycle events (serving, stopped) as info,
// outside the limit, so client-caused warnings cannot hide them.
func (l *EventLog) Status(msg string) { _ = l.w.Info(eventInfo, msg) }

// Infof and Warnf format with FormatEvent; they fit Config.Logf and HCSConfig.Logf.
func (l *EventLog) Infof(format string, args ...any) { l.Info(FormatEvent(format, args...)) }
func (l *EventLog) Warnf(format string, args ...any) { l.Warning(FormatEvent(format, args...)) }

// ServiceState is what QueryService reports.
type ServiceState struct {
	Installed bool
	State     svc.State
	PID       uint32
}

// QueryService reads a service's state with the rights any user has: it connects to
// the SCM with SC_MANAGER_CONNECT and opens the service with SERVICE_QUERY_STATUS only.
func QueryService(name string) (ServiceState, error) {
	m, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return ServiceState{}, fmt.Errorf("connect to the service control manager: %w", err)
	}
	defer func() { _ = windows.CloseServiceHandle(m) }()
	n, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return ServiceState{}, err
	}
	h, err := windows.OpenService(m, n, windows.SERVICE_QUERY_STATUS)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return ServiceState{}, nil
	}
	if err != nil {
		return ServiceState{}, fmt.Errorf("open service %s: %w", name, err)
	}
	s := &mgr.Service{Name: name, Handle: h}
	defer func() { _ = s.Close() }()
	q, err := s.Query()
	if err != nil {
		return ServiceState{}, fmt.Errorf("query service %s: %w", name, err)
	}
	return ServiceState{Installed: true, State: q.State, PID: q.ProcessId}, nil
}

// RequestServiceStart asks the SCM to start the service with the rights a user may
// have: SC_MANAGER_CONNECT, and SERVICE_START on the service, which the broker's
// service grants interactive users (serviceSDDL). A service that runs already is not
// an error. It does not wait for the service to run.
func RequestServiceStart(name string) error {
	m, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return fmt.Errorf("connect to the service control manager: %w", err)
	}
	defer func() { _ = windows.CloseServiceHandle(m) }()
	n, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	h, err := windows.OpenService(m, n, windows.SERVICE_START)
	if err != nil {
		return fmt.Errorf("open service %s to start it: %w", name, err)
	}
	defer func() { _ = windows.CloseServiceHandle(h) }()
	if err := windows.StartService(h, 0, nil); err != nil && !errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
		return fmt.Errorf("start service %s: %w", name, err)
	}
	return nil
}

// StateName is a service state in words, for messages.
func StateName(s svc.State) string {
	switch s {
	case svc.Stopped:
		return "stopped"
	case svc.StartPending:
		return "starting"
	case svc.StopPending:
		return "stopping"
	case svc.Running:
		return "running"
	case svc.ContinuePending:
		return "resuming"
	case svc.PausePending:
		return "pausing"
	case svc.Paused:
		return "paused"
	}
	return fmt.Sprintf("state %d", s)
}
