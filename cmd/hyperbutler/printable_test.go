package main

import "testing"

func TestPrintable(t *testing.T) {
	in := "ok\tline\r\n\x1b[2Jclear\x07bell\rover\xc2\x9bcsi sep\xffbad\n"
	got := string(printable([]byte(in)))
	want := "ok\tline\r\n\\u001b[2Jclear\\u0007bell\\rover\\u009bcsi\\u2028sep\\xffbad\n"
	if got != want {
		t.Fatalf("printable:\n got %q\nwant %q", got, want)
	}
}
