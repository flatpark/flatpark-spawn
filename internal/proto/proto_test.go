package proto

import (
	"errors"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestRoundTrip(t *testing.T) {
	for _, r := range []Reply{
		{Kind: Found, Gen: "abc", Path: "/usr/bin/git", LoginPath: "/a:/b", Shell: "/bin/zsh", Session: "A=1;B=2"},
		{Kind: None, Gen: "abc"},
		{Kind: NoCwd, Path: "/app/x"},
		{Kind: Fail, Msg: "multi\nline"},
	} {
		got, err := Decode(r.Encode())
		if err != nil || got != r {
			t.Fatalf("%+v -> %+v %v", r, got, err)
		}
	}
	for _, b := range []string{"", "found", "found\n\n\n\n\n", "bogus\n\n\n\n\n\n"} {
		if _, err := Decode([]byte(b)); err == nil {
			t.Errorf("%q must not decode", b)
		}
	}
}

func TestEnvBlob(t *testing.T) {
	in := map[string]string{"A": "1", "MULTI": "x\ny=z", "EMPTY": ""}
	out := DecodeEnv(EncodeEnv(in))
	if len(out) != len(in) {
		t.Fatalf("%v", out)
	}
	for k, v := range in {
		if out[k] != v {
			t.Fatalf("%s: %q", k, out[k])
		}
	}
	if len(DecodeEnv(nil)) != 0 {
		t.Fatal("empty blob")
	}
}

func TestEnvHashes(t *testing.T) {
	h := ParseHashes(EnvHashes([]string{"HOME=/home/me", "LANG=C.UTF-8", "BAD;NAME=x", "NOEQ"}))
	if len(h) != 2 || h["HOME"] != HashValue("/home/me") || h["LANG"] != HashValue("C.UTF-8") {
		t.Fatalf("%v", h)
	}
	if HashValue("a") == HashValue("b") {
		t.Fatal("hash")
	}
}

func TestSizeLimits(t *testing.T) {
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(pair[0])
	defer unix.Close(pair[1])
	var tooLarge *TooLargeError
	if err := Send(pair[0], "the environment", make([]byte, MaxMessage+1)); !errors.As(err, &tooLarge) {
		t.Fatalf("over the limit: %v", err)
	}
	if err := Send(pair[0], "x", make([]byte, MaxMessage)); err != nil {
		t.Fatalf("at the limit: %v", err)
	}
	if b, err := Recv(pair[1]); err != nil || len(b) != MaxMessage {
		t.Fatalf("receive at the limit: %d %v", len(b), err)
	}
	// A longer datagram from someone not using Send is detected, not cut.
	if _, err := unix.Write(pair[0], make([]byte, 150<<10)); err != nil {
		t.Fatal(err)
	}
	var truncated *TruncatedError
	if _, err := Recv(pair[1]); !errors.As(err, &truncated) || truncated.Size != 150<<10 {
		t.Fatalf("truncation: %v", err)
	}
	if !strings.Contains((&TruncatedError{Size: 9000}).Error(), "truncated") {
		t.Fatal("message")
	}
}

func TestFitDropsTheFingerprintFirst(t *testing.T) {
	r := Reply{Kind: Found, Path: "/usr/bin/git", LoginPath: "/usr/bin", Session: strings.Repeat("A=0;", MaxMessage/4)}
	b, err := r.Fit()
	if err != nil {
		t.Fatal(err)
	}
	got, _ := Decode(b)
	if got.Session != "" || got.Path != "/usr/bin/git" {
		t.Fatalf("%+v", got)
	}
	r.LoginPath = strings.Repeat("/x:", MaxMessage/3+1)
	if _, err := r.Fit(); err == nil {
		t.Fatal("an essential field over the limit must be an error")
	}
}
