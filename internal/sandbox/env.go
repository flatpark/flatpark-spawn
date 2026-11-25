package sandbox

import (
	"bufio"
	"os"
	"strings"

	"github.com/flatpark/flatpark-spawn/internal/names"
)

// The host process starts from the session helper's environment (the
// host's HOME, DISPLAY, D-Bus and ssh-agent addresses, XDG_*) and gets the
// caller's environment on top, so that what an app sets for its child
// (GIT_TERMINAL_PROMPT=0, LC_ALL=C, CI=1 ...) reaches it. Left out:
// variables that describe the sandbox itself, plumbing the host already has
// under its own values, and values naming sandbox-only paths.
var dropExact = map[string]bool{
	// replaced on the host: the agent sets the login PATH
	"PATH": true,
	// shell and process bookkeeping
	"SHLVL": true, "PWD": true, "OLDPWD": true, "_": true, "BASH_ENV": true, "ENV": true,
	"container": true,
	// the host's own values must win
	"HOME": true, "USER": true, "LOGNAME": true, "SHELL": true,
	"DBUS_SESSION_BUS_ADDRESS": true, "DBUS_SYSTEM_BUS_ADDRESS": true,
	"AT_SPI_BUS_ADDRESS": true, "SSH_AUTH_SOCK": true,
	"DISPLAY": true, "XAUTHORITY": true, "WAYLAND_DISPLAY": true,
	"PULSE_SERVER": true, "PULSE_CLIENTCONFIG": true, "PIPEWIRE_REMOTE": true,
	// the runtime's layout
	"PYTHONPATH": true, "PYTHONHOME": true, "PYTHONUSERBASE": true,
	"PKG_CONFIG_PATH": true, "ACLOCAL_PATH": true, "C_INCLUDE_PATH": true,
	"CPLUS_INCLUDE_PATH": true, "LIBRARY_PATH": true, "MANPATH": true, "INFOPATH": true,
	"GTK_PATH": true, "GTK_EXE_PREFIX": true, "GTK_DATA_PREFIX": true, "GTK_IM_MODULE_FILE": true,
	"GI_TYPELIB_PATH": true, "GSETTINGS_SCHEMA_DIR": true, "GSETTINGS_BACKEND": true,
	"XCURSOR_PATH": true, "XKB_CONFIG_ROOT": true, "XLOCALEDIR": true, "LOCPATH": true,
	"GCONV_PATH": true, "FONTCONFIG_FILE": true, "FONTCONFIG_PATH": true,
	"SSL_CERT_DIR": true, "SSL_CERT_FILE": true, "ALSA_CONFIG_PATH": true,
	"LIBGL_DRIVERS_PATH": true, "LIBVA_DRIVERS_PATH": true, "VDPAU_DRIVER_PATH": true,
	"OCL_ICD_VENDORS": true, "TERMINFO_DIRS": true,
	// Electron runs a host Electron app as plain node with this set
	"ELECTRON_RUN_AS_NODE": true,
}

var dropPrefixes = []string{
	"FLATPAK_", names.EnvPrefix + "_", "ZYPAK_", "BASH_FUNC_", "LD_", "XDG_",
	"GST_", "GIO_", "GDK_PIXBUF_", "VK_", "__EGL_", "__GLX_", "QT_PLUGIN", "QML",
}

// envPrivate are paths that never exist on the host. Unlike exec arguments
// (preload/route.c), /usr and /bin are not on this list: an EDITOR or PAGER
// of /usr/bin/vim usually means the same file on the host.
var envPrivate = []string{"/app", "/run/flatpak", "/run/host", "/.flatpak-info"}

// HostEnv filters the caller's environment for the host.
func HostEnv(environ []string, tmpShared bool) map[string]string {
	out := make(map[string]string)
	for _, kv := range environ {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" || dropExact[k] {
			continue
		}
		if hasAnyPrefix(k, dropPrefixes) || privateValue(v, tmpShared) {
			continue
		}
		out[k] = v
	}
	return out
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

func privateValue(v string, tmpShared bool) bool {
	list := envPrivate
	if !tmpShared {
		list = append(list[:len(list):len(list)], "/tmp", "/var/tmp")
	}
	for i := 0; i < len(v); i++ {
		if v[i] != '/' || (i > 0 && !strings.ContainsRune(" =:;,\"'", rune(v[i-1]))) {
			continue
		}
		for _, p := range list {
			rest := v[i:]
			if strings.HasPrefix(rest, p) && (len(rest) == len(p) || strings.ContainsRune("/ :;,\"'", rune(rest[len(p)]))) {
				return true
			}
		}
	}
	return false
}

// CompatEnv is host-spawn's -env: only the listed variables (default TERM).
func CompatEnv(list string) map[string]string {
	out := make(map[string]string)
	for _, k := range strings.Split(list, ",") {
		if v, ok := os.LookupEnv(k); ok && k != "" {
			out[k] = v
		}
	}
	return out
}

// TmpShared reports a --filesystem=/tmp grant, read from /.flatpak-info.
// (--filesystem=host does not share /tmp.)
func TmpShared(info string) bool {
	f, err := os.Open(info)
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	inContext := false
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "[") {
			inContext = line == "[Context]"
			continue
		}
		if !inContext || !strings.HasPrefix(line, "filesystems=") {
			continue
		}
		for _, e := range strings.Split(strings.TrimPrefix(line, "filesystems="), ";") {
			if p, _, _ := strings.Cut(e, ":"); p == "/tmp" {
				return true
			}
		}
	}
	return false
}
