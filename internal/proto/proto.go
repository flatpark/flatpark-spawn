// Package proto is the one-message reply the host agent sends back over the
// control socket (fd 3 on the host, a SOCK_SEQPACKET pair) before it either
// execs the command or exits. A command that started sends Found and then
// the socket closes on exec; anything else means it did not start.
package proto

import (
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// MaxMessage bounds every datagram on the control socket, both ways. A
// SOCK_SEQPACKET datagram must fit the sender's socket buffer
// (net.core.wmem_default, 212992 bytes on common kernels; 204800 bytes were
// measured to pass and 212992 to fail with EMSGSIZE), so 128 KiB leaves room.
// It holds a reply with a long login PATH and the session fingerprint, or the
// caller's environment (Linux itself limits one variable to 128 KiB).
const MaxMessage = 128 << 10

// TooLargeError: a datagram that would exceed MaxMessage.
type TooLargeError struct {
	What string
	Size int
}

func (e *TooLargeError) Error() string {
	return fmt.Sprintf("%s is %d bytes, more than the control socket's limit of %d", e.What, e.Size, MaxMessage)
}

// TruncatedError: a datagram longer than the receive buffer arrived; the
// kernel dropped the rest. Reported as such, never as a malformed message.
type TruncatedError struct{ Size int }

func (e *TruncatedError) Error() string {
	return fmt.Sprintf("control message truncated: %d bytes, buffer %d", e.Size, MaxMessage)
}

// Send writes one datagram of at most MaxMessage bytes.
func Send(fd int, what string, b []byte) error {
	if len(b) > MaxMessage {
		return &TooLargeError{What: what, Size: len(b)}
	}
	for {
		_, err := unix.Write(fd, b)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if errors.Is(err, unix.EMSGSIZE) {
			return &TooLargeError{What: what, Size: len(b)}
		}
		return err
	}
}

// Recv reads one datagram. MSG_TRUNC makes the kernel return the datagram's
// real length, so a longer one is detected instead of silently cut.
func Recv(fd int) ([]byte, error) {
	buf := make([]byte, MaxMessage)
	for {
		n, _, flags, _, err := unix.Recvmsg(fd, buf, nil, unix.MSG_TRUNC)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if n > len(buf) || flags&unix.MSG_TRUNC != 0 {
			return nil, &TruncatedError{Size: n}
		}
		if n == 0 {
			return nil, io.EOF
		}
		return buf[:n], nil
	}
}

// Kind of reply.
type Kind string

const (
	// Found: the command exists on the host; the agent execs it next.
	Found Kind = "found"
	// None: the host has no such command, even after refreshing the login PATH.
	None Kind = "none"
	// NoCwd: the working directory does not exist on the host.
	NoCwd Kind = "nocwd"
	// Fail: anything else; Msg says what.
	Fail Kind = "error"
)

// Reply is the agent's answer.
type Reply struct {
	Kind Kind
	// Gen identifies the host's login PATH and the state of its
	// directories; the sandbox drops its lookup cache when it changes.
	Gen string
	// Path is the resolved executable (Found) or the directory (NoCwd).
	Path string
	// LoginPath and Shell (Found): what the command runs with, so that the
	// sandbox can start that executable again directly, without the agent.
	LoginPath string
	Shell     string
	// Session fingerprints the session helper's environment (EnvHashes),
	// so that a direct start sends only the variables that differ.
	Session string
	Msg     string
}

const fields = 7

// Encode returns the datagram.
func (r Reply) Encode() []byte {
	return []byte(strings.Join([]string{string(r.Kind), r.Gen, r.Path, r.LoginPath, r.Shell, r.Session, r.Msg}, "\n"))
}

// Decode parses a datagram.
func Decode(b []byte) (Reply, error) {
	parts := strings.SplitN(string(b), "\n", fields)
	if len(parts) != fields {
		return Reply{}, errors.New("malformed agent reply")
	}
	r := Reply{Kind: Kind(parts[0]), Gen: parts[1], Path: parts[2], LoginPath: parts[3], Shell: parts[4], Session: parts[5], Msg: parts[6]}
	switch r.Kind {
	case Found, None, NoCwd, Fail:
		return r, nil
	}
	return Reply{}, errors.New("unknown agent reply " + parts[0])
}

// Fit returns the encoded reply, dropping the optional session fingerprint
// when the whole would not fit in one datagram.
func (r Reply) Fit() ([]byte, error) {
	b := r.Encode()
	if len(b) > MaxMessage && r.Session != "" {
		r.Session = ""
		b = r.Encode()
	}
	if len(b) > MaxMessage {
		return nil, &TooLargeError{What: "the agent's reply", Size: len(b)}
	}
	return b, nil
}

// EncodeEnv is the first datagram on the control socket: the environment
// the command gets on top of the session helper's. It travels here and not
// in HostCommand's environment (so that the agent sees the session's own)
// nor in argv (world-readable in /proc/PID/cmdline).
func EncodeEnv(env map[string]string) []byte {
	var b strings.Builder
	for k, v := range env {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(v)
		b.WriteByte(0)
	}
	return []byte(b.String())
}

// DecodeEnv reverses EncodeEnv.
func DecodeEnv(b []byte) map[string]string {
	env := make(map[string]string)
	for _, kv := range strings.Split(string(b), "\x00") {
		if k, v, ok := strings.Cut(kv, "="); ok && k != "" {
			env[k] = v
		}
	}
	return env
}

// EnvHashes fingerprints an environment: "NAME=fnv64hex;..." (sorted).
func EnvHashes(environ []string) string {
	parts := make([]string, 0, len(environ))
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok && k != "" && !strings.ContainsAny(k, ";\n") {
			parts = append(parts, k+"="+HashValue(v))
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, ";")
}

// HashValue is the fingerprint of one value.
func HashValue(v string) string {
	h := fnv.New64a()
	h.Write([]byte(v))
	return strconv.FormatUint(h.Sum64(), 16)
}

// ParseHashes reads EnvHashes.
func ParseHashes(s string) map[string]string {
	m := make(map[string]string)
	for _, p := range strings.Split(s, ";") {
		if k, h, ok := strings.Cut(p, "="); ok {
			m[k] = h
		}
	}
	return m
}
