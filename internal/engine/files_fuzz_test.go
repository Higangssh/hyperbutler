package engine

import (
	"reflect"
	"strings"
	"testing"
)

// The state directory's files can be left half written by a crash, edited by the user,
// or replaced by anything with write access to the directory. Every accepted document
// must validate, and must survive a re-encode unchanged.

func FuzzParseState(f *testing.F) {
	f.Add([]byte(`{"version":1,"phase":"running","pid":4,"pidCreated":1,"vmId":"hyperbutler-65fd2a1a-c01f6278e7ec","runtimeId":"6cf7ba4c-5e89-5915-bea5-3642d57a897b","pipe":"\\\\.\\pipe\\docker_engine_hyperbutler","dataDir":"C:\\x","started":"2026-10-30T08:00:00Z","readyMillis":1700,"updated":"2026-10-30T08:00:01Z"}`))
	f.Add([]byte(`{"version":1,"phase":"failed","pid":0,"pidCreated":0,"pipe":"","dataDir":"","started":"0001-01-01T00:00:00Z","updated":"0001-01-01T00:00:00Z","error":"boom","errorCode":"hcs-access-denied"}`))
	f.Add([]byte(`{"version":1,"phase":"running","pid":0}`))
	f.Add([]byte(`{"version":1,"phase":"stopped","pid":1e3}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		s, err := ParseState(b)
		if err != nil {
			return
		}
		if err := s.Validate(); err != nil {
			t.Fatalf("accepted an invalid state: %v", err)
		}
		if s.Phase.Live() && s.PID <= 0 {
			t.Fatalf("live phase %q with pid %d", s.Phase, s.PID)
		}
		if s.VMID != "" && !strings.HasPrefix(s.VMID, "hyperbutler-") {
			t.Fatalf("foreign VM id %q", s.VMID)
		}
		again, err := encode(s)
		if err != nil {
			t.Fatal(err)
		}
		s2, err := ParseState(again)
		if err != nil || !reflect.DeepEqual(s, s2) {
			t.Fatalf("re-encode changed the state: %v\n%+v\n%+v", err, s, s2)
		}
	})
}

func FuzzParseSettings(f *testing.F) {
	f.Add([]byte(`{"version":1,"dataDir":"C:\\Users\\x\\AppData\\Local\\hyperbutler","diskSizeBytes":68719476736,"memoryMB":4096,"cpus":4}`))
	f.Add([]byte(`{"version":1,"dataDir":"/home/x","diskSizeBytes":68719476736,"memoryMB":4096,"cpus":4,"pipe":"\\\\.\\pipe\\x"}`))
	f.Add([]byte(`{"version":1,"dataDir":"C:\\x","diskSizeBytes":-1,"memoryMB":4096,"cpus":4}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		s, err := ParseSettings(b)
		if err != nil {
			return
		}
		if err := s.Validate(); err != nil {
			t.Fatalf("accepted invalid settings: %v", err)
		}
		if s.DiskSize%(1<<20) != 0 || s.MemoryMB < MinMemoryMB || s.CPUs < 1 {
			t.Fatalf("out of range: %+v", s)
		}
		again, err := encode(s)
		if err != nil {
			t.Fatal(err)
		}
		s2, err := ParseSettings(again)
		if err != nil || *s2 != *s {
			t.Fatalf("re-encode changed the settings: %v", err)
		}
	})
}

func FuzzParseDiskMeta(f *testing.F) {
	f.Add([]byte(`{"version":1,"sizeBytes":68719476736,"blockSize":1048576,"metadataBytes":4194304,"created":"2026-10-30T00:00:00Z"}`))
	f.Add([]byte(`{"version":1,"sizeBytes":68719476736,"blockSize":33554432,"metadataBytes":0,"created":"2026-10-30T00:00:00Z"}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		d, err := ParseDiskMeta(b)
		if err != nil {
			return
		}
		if d.BlockSize != 1<<20 || d.MetadataBytes > 1<<30 || d.SizeBytes > MaxDiskSize {
			t.Fatalf("out of range: %+v", d)
		}
		again, err := encode(d)
		if err != nil {
			t.Fatal(err)
		}
		d2, err := ParseDiskMeta(again)
		if err != nil || *d2 != *d {
			t.Fatalf("re-encode changed the disk meta: %v", err)
		}
	})
}

func FuzzParseSize(f *testing.F) {
	for _, s := range []string{"64GiB", "4096", "1t", "0b", "16777215TiB", "18446744073709551615", "1.5G", " 2 mib"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		n, err := ParseSize(s)
		if err != nil {
			return
		}
		if len(s) > maxSizeLen {
			t.Fatalf("accepted %d bytes of input", len(s))
		}
		back, err := ParseSize(FormatSize(n))
		if err != nil || back != n {
			t.Fatalf("%q = %d, but FormatSize gives %q = %d (%v)", s, n, FormatSize(n), back, err)
		}
	})
}
