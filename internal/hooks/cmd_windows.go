//go:build windows

package hooks

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"

	"golang.org/x/sys/windows/registry"
)

// regAutoRun is the real HKCU AutoRun value.
type regAutoRun struct{}

var autoRun autoRunStore = regAutoRun{}

func (regAutoRun) get() (autoRunValue, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, cmdAutoRunKey, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return autoRunValue{}, nil
	}
	if err != nil {
		return autoRunValue{}, err
	}
	defer k.Close()
	s, typ, err := k.GetStringValue("AutoRun")
	if errors.Is(err, registry.ErrNotExist) {
		return autoRunValue{}, nil
	}
	if err != nil {
		return autoRunValue{}, err
	}
	return autoRunValue{Exists: true, Value: s, Expand: typ == registry.EXPAND_SZ}, nil
}

func (regAutoRun) set(v autoRunValue) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, cmdAutoRunKey, registry.SET_VALUE|registry.QUERY_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	switch {
	case !v.Exists:
		if err := k.DeleteValue("AutoRun"); err != nil && !errors.Is(err, registry.ErrNotExist) {
			return err
		}
		return nil
	case v.Expand:
		return k.SetExpandStringValue("AutoRun", v.Value)
	}
	return k.SetStringValue("AutoRun", v.Value)
}

// findClink returns the Clink program, or "" when Clink is not installed.
func findClink() string {
	if p := os.Getenv("OOPS_CLINK"); p != "" {
		if p == "none" {
			return ""
		}
		return p
	}
	if p, err := exec.LookPath("clink"); err == nil {
		return p
	}
	for _, dir := range []string{
		filepath.Join(os.Getenv("ProgramFiles(x86)"), "clink"),
		filepath.Join(os.Getenv("ProgramFiles"), "clink"),
		filepath.Join(home(), "scoop", "apps", "clink", "current"),
		filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs", "clink"),
	} {
		for _, name := range []string{"clink_x64.exe", "clink.bat"} {
			if p := filepath.Join(dir, name); exists(p) {
				return p
			}
		}
	}
	return ""
}

// clinkScripts registers or removes a scripts folder with Clink, which
// keeps the list in the registry for every Clink install.
func clinkScripts(clink string, add bool, dir string) error {
	verb := "uninstallscripts"
	if add {
		verb = "installscripts"
	}
	return exec.Command(clink, verb, dir).Run()
}
