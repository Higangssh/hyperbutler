package main

import (
	"strings"
	"testing"
)

func TestParseCIM(t *testing.T) {
	out := "\r\n1234,7340032,215,16,51150848\r\n  42,209715200,0,5,1075908608  \r\n\r\n"
	got, err := ParseCIM(out)
	if err != nil {
		t.Fatal(err)
	}
	want := map[uint32]ProcSample{
		1234: {PID: 1234, WorkingSetPriv: 7340032, HandleCount: 215, ThreadCount: 16, PrivateBytes: 51150848},
		42:   {PID: 42, WorkingSetPriv: 209715200, HandleCount: 0, ThreadCount: 5, PrivateBytes: 1075908608},
	}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("PID %d: got %+v, want %+v", k, got[k], v)
		}
	}
	if m, err := ParseCIM(""); err != nil || len(m) != 0 {
		t.Errorf("empty output: %v, %v", m, err)
	}
}

func TestParseCIMRejects(t *testing.T) {
	for _, in := range []string{
		"1,2,3,4",
		"1,2,3,4,5,6",
		"0,2,3,4,5",
		"4294967296,2,3,4,5",
		"1,-2,3,4,5",
		"1,+2,3,4,5",
		"1,2.5,3,4,5",
		"1,,3,4,5",
		"1,2,3,4,",
		"1, 2,3,4,5",
		"1,2,3,4,18446744073709551616",
		"1,2,3,4,5\n1,2,3,4,5",
		"Get-CimInstance : Access denied",
		"1,2,3,4,5\nAt line:1 char:1",
		"1,0x10,3,4,5",
		"1,2,3,4,5\x00",
		strings.Repeat("9", maxCIMOutput+1),
	} {
		if m, err := ParseCIM(in); err == nil {
			t.Errorf("ParseCIM(%.40q) = %v, want error", in, m)
		}
	}
}

func TestParseStats(t *testing.T) {
	good := `{"goroutines":12,"handles":150,"memory_limit":9223372036854775807,"memstats":{"heap_inuse":4194304}}`
	s, err := parseStats(strings.NewReader(good + "\n"))
	if err != nil || s.Goroutines != 12 || s.Handles != 150 || s.MemStats.HeapInuse != 4194304 {
		t.Fatalf("%+v, %v", s, err)
	}
	for _, in := range []string{
		"",
		"{}",
		`{"goroutines":0}`,
		`{"goroutines":-1}`,
		`{"goroutines":1,"handles":-2}`,
		`{"goroutines":1,"extra":true}`,
		`{"goroutines":1}{"goroutines":2}`,
		`{"goroutines":1.5}`,
		`{"goroutines":"1"}`,
		`[1]`,
		`{"goroutines":1,"memstats":{"heap_inuse":-1}}`,
	} {
		if s, err := parseStats(strings.NewReader(in)); err == nil {
			t.Errorf("parseStats(%q) = %+v, want error", in, s)
		}
	}
}
