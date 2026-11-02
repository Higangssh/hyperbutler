//go:build windows

package main

import (
	"errors"
	"strings"
	"testing"

	"golang.org/x/sys/windows/svc"

	"github.com/Higangssh/hyperbutler/internal/broker"
	"github.com/Higangssh/hyperbutler/internal/broker/proto"
	"github.com/Higangssh/hyperbutler/internal/gui/i18n"
)

// fakeBroker is a machine state for brokerCheck.
type fakeBroker struct {
	st         broker.ServiceState
	queryErr   error
	pipe       bool
	proto      uint8
	version    string
	versionErr error
	dialed     bool
	// pipeOwner is the pipe's owner SID ("" when it cannot be read).
	pipeOwner string
}

const testSelf = "S-1-5-21-1-2-3-1001"

func (f *fakeBroker) probe() brokerProbe {
	return brokerProbe{
		query:  func() (broker.ServiceState, error) { return f.st, f.queryErr },
		exists: func(p string) bool { return f.pipe && p == broker.ServicePipe },
		version: func(p string) (uint8, string, error) {
			f.dialed = true
			if p != broker.ServicePipe {
				return 0, "", errors.New("wrong pipe " + p)
			}
			return f.proto, f.version, f.versionErr
		},
		owner: func(string) (string, error) {
			if f.pipeOwner == "" {
				return "", errors.New("all pipe instances are busy")
			}
			return f.pipeOwner, nil
		},
		self: func() (string, error) { return testSelf, nil },
	}
}

var brokerCases = []struct {
	name   string
	f      fakeBroker
	status string
	msg    string
	fix    string
	dial   bool
}{
	{"not installed", fakeBroker{}, doctorInfo, "doctor.broker.absent", "", false},
	{"not installed, pipe taken", fakeBroker{pipe: true}, doctorWarn, "doctor.broker.squatted", "doctor.fix.brokerSquatted", false},
	{"stopped", fakeBroker{st: broker.ServiceState{Installed: true, State: svc.Stopped}}, doctorWarn, "doctor.broker.stopped", "doctor.fix.brokerStart", false},
	{"stopped, pipe taken", fakeBroker{st: broker.ServiceState{Installed: true, State: svc.Stopped}, pipe: true}, doctorWarn, "doctor.broker.squatted", "doctor.fix.brokerSquatted", false},
	{"stopped, pipe of another user", fakeBroker{st: broker.ServiceState{Installed: true, State: svc.Stopped}, pipe: true, pipeOwner: "S-1-5-21-9-9-9-1002"}, doctorWarn, "doctor.broker.squatted", "doctor.fix.brokerSquatted", false},
	{"stopped, pipe of SYSTEM", fakeBroker{st: broker.ServiceState{Installed: true, State: svc.Stopped}, pipe: true, pipeOwner: "S-1-5-18"}, doctorWarn, "doctor.broker.squatted", "doctor.fix.brokerSquatted", false},
	{"stopped, own foreground broker", fakeBroker{st: broker.ServiceState{Installed: true, State: svc.Stopped}, pipe: true, pipeOwner: testSelf}, doctorInfo, "doctor.broker.foreground", "", false},
	{"not installed, own foreground broker", fakeBroker{pipe: true, pipeOwner: testSelf}, doctorInfo, "doctor.broker.foreground", "", false},
	{"starting", fakeBroker{st: broker.ServiceState{Installed: true, State: svc.StartPending}}, doctorInfo, "doctor.broker.pending", "", false},
	{"query refused", fakeBroker{queryErr: errors.New("open service: access denied")}, doctorWarn, "doctor.broker.unknown", "", false},
	{"running", fakeBroker{st: broker.ServiceState{Installed: true, State: svc.Running, PID: 42}, pipe: true, proto: proto.Version, version: "0.2.0"}, doctorOK, "doctor.broker.ok", "", true},
	{"running, newer protocol", fakeBroker{st: broker.ServiceState{Installed: true, State: svc.Running}, pipe: true, proto: proto.Version + 1, version: "0.3.0"}, doctorOK, "doctor.broker.ok", "", true},
	{"running, older protocol", fakeBroker{st: broker.ServiceState{Installed: true, State: svc.Running}, pipe: true, proto: proto.Version - 1, version: "0.1.0"}, doctorWarn, "doctor.broker.old", "doctor.fix.brokerUpgrade", true},
	{"running, no answer", fakeBroker{st: broker.ServiceState{Installed: true, State: svc.Running}, versionErr: errors.New("the pipe belongs to S-1-5-21-1-2-3-1001, not to the broker")}, doctorWarn, "doctor.broker.noAnswer", "doctor.fix.brokerRestart", true},
}

func TestBrokerCheck(t *testing.T) {
	en := i18n.Raw(i18n.Source)
	for _, c := range brokerCases {
		f := c.f
		got := brokerCheck(f.probe())
		if got.ID != "broker" || got.Status != c.status || got.Msg != c.msg || got.FixMsg != c.fix {
			t.Errorf("%s: %+v", c.name, got)
		}
		if f.dialed != c.dial {
			t.Errorf("%s: dialed %v, want %v", c.name, f.dialed, c.dial)
		}
		if got.Summary == "" || (c.fix != "") != (got.Fix != "") {
			t.Errorf("%s: summary %q, fix %q", c.name, got.Summary, got.Fix)
		}
		for _, k := range []string{got.Msg, got.FixMsg} {
			if k == "" {
				continue
			}
			s, ok := en[k]
			if !ok {
				t.Errorf("%s: %s is not in the catalog", c.name, k)
				continue
			}
			for _, p := range i18n.Placeholders(s) {
				if _, ok := got.Args[p]; !ok {
					t.Errorf("%s: %s uses {%s}, which the check does not send (%v)", c.name, k, p, got.Args)
				}
			}
		}
	}
	// What the user is told when the name is squatted: another program, not ours.
	f := fakeBroker{pipe: true}
	if got := brokerCheck(f.probe()); !strings.Contains(got.Summary, "another program holds") {
		t.Errorf("squatted: %q", got.Summary)
	}
	f = fakeBroker{st: broker.ServiceState{Installed: true, State: svc.Running}, pipe: true, proto: proto.Version, version: "0.2.0"}
	if got := brokerCheck(f.probe()); got.Args["version"] != "0.2.0" || got.Args["proto"] == "" {
		t.Errorf("running: %v", got.Args)
	}
}

// TestBrokerCheckMachine runs the check against this machine: it must not fail, and
// without the service it is a note.
func TestBrokerCheckMachine(t *testing.T) {
	c := brokerCheck(machineBroker)
	if c.Status == doctorFail {
		t.Errorf("%+v", c)
	}
	t.Logf("%s: %s", c.Status, c.Summary)
}

// TestPipeOwner reads the owner of a real pipe: one this process creates is its user's.
func TestPipeOwner(t *testing.T) {
	name := `\\.\pipe\hyperbutler_doctor_owner_test`
	l, err := broker.ListenPipe(broker.PipeConfig{Name: name, Instances: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	me, err := machineBroker.self()
	if err != nil {
		t.Fatal(err)
	}
	if o, err := pipeOwner(name); err != nil || o != me {
		t.Errorf("owner %q, %v; want %s", o, err, me)
	}
	if _, err := pipeOwner(`\\.\pipe\hyperbutler_doctor_owner_test_none`); err == nil {
		t.Error("a missing pipe has an owner")
	}
}
