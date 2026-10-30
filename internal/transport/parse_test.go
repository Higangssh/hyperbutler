package transport

import (
	"strings"
	"testing"

	"github.com/Microsoft/go-winio/pkg/guid"
)

const testVM = "6d3a1f4e-8c2b-4f7a-9e01-2b3c4d5e6f70"

func TestParseHvsockTarget(t *testing.T) {
	vm := guid.GUID{Data1: 0x6d3a1f4e, Data2: 0x8c2b, Data3: 0x4f7a, Data4: [8]byte{0x9e, 0x01, 0x2b, 0x3c, 0x4d, 0x5e, 0x6f, 0x70}}
	port2375 := VsockServiceID(2375)
	for _, tc := range []struct {
		in      string
		want    HvsockTarget
		wantErr bool
	}{
		{in: testVM + ":vsock:2375", want: HvsockTarget{vm, port2375}},
		{in: "{" + strings.ToUpper(testVM) + "}:vsock:2375", want: HvsockTarget{vm, port2375}},
		{in: testVM + ":00000947-facb-11e6-bd58-64006a7986d3", want: HvsockTarget{vm, port2375}},
		{in: testVM + ":vsock:0", want: HvsockTarget{vm, VsockServiceID(0)}},
		{in: testVM + ":vsock:4294967295", want: HvsockTarget{vm, VsockServiceID(0xffffffff)}},
		{in: "", wantErr: true},
		{in: testVM, wantErr: true},
		{in: testVM + ":", wantErr: true},
		{in: testVM + ":vsock:", wantErr: true},
		{in: testVM + ":vsock:4294967296", wantErr: true},
		{in: testVM + ":vsock:-1", wantErr: true},
		{in: testVM + ":vsock:+1", wantErr: true},
		{in: testVM + ":vsock: 1", wantErr: true},
		{in: testVM + ":vsock:0x10", wantErr: true},
		{in: "6d3a1f4e+8c2b-4f7a-9e01-2b3c4d5e6f70:vsock:1", wantErr: true},
		{in: "+d3a1f4e-8c2b-4f7a-9e01-2b3c4d5e6f70:vsock:1", wantErr: true},
		{in: "6d3a1f4e-8c2b-4f7a-9e01-2b3c4d5e6f7g:vsock:1", wantErr: true},
		{in: "{" + testVM + ":vsock:1", wantErr: true},
		{in: "{}:vsock:1", wantErr: true},
		{in: testVM + ":vsock:1" + strings.Repeat("0", 200), wantErr: true},
	} {
		got, err := ParseHvsockTarget(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseHvsockTarget(%q) = %v, want error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseHvsockTarget(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseHvsockTarget(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestVsockServiceIDMatchesWinio(t *testing.T) {
	// go-winio documents this template; keep ours in sync.
	mustEqual(t, VsockServiceID(0x94f).String(), "0000094f-facb-11e6-bd58-64006a7986d3")
}

func TestValidatePipeName(t *testing.T) {
	for _, tc := range []struct {
		in string
		ok bool
	}{
		{DefaultPipeName, true},
		{`\\.\PIPE\docker_engine_winbox`, true},
		{`\\.\pipe\a`, true},
		{`\\.\pipe\` + strings.Repeat("a", 256-9), true},
		{`\\.\pipe\` + strings.Repeat("a", 256-8), false},
		{`\\.\pipe\` + strings.Repeat("\U0001F600", 123), true},  // 9 + 246 = 255 units
		{`\\.\pipe\` + strings.Repeat("\U0001F600", 124), false}, // 9 + 248 = 257 units
		{``, false},
		{`\\.\pipe\`, false},
		{`\\.\pipe`, false},
		{`\\server\pipe\x`, false},
		{`\\?\pipe\x`, false},
		{`\Device\NamedPipe\x`, false},
		{`//./pipe/x`, false},
		{`\\.\pipe\a\b`, false},
		{`\\.\pipe\a/b`, false},
		{`\\.\pipe\a` + "\x00", false},
		{`\\.\pipe\a` + "\n", false},
		{`\\.\pipe\a` + "\xff", false},
	} {
		err := ValidatePipeName(tc.in)
		if (err == nil) != tc.ok {
			t.Errorf("ValidatePipeName(%q) = %v, want ok=%v", tc.in, err, tc.ok)
		}
	}
}
