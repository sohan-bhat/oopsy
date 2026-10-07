package remote

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func argv(s string) []string { return strings.Fields(s) }

func TestCalls(t *testing.T) {
	cwd := filepath.FromSlash("/work")
	env := []string{"NS=prod", "PATH=/bin"}
	got := calls(`cd infra && KUBECONFIG=/k sudo kubectl delete pod web -n "$NS"; echo done`, cwd, env)
	if len(got) != 2 {
		t.Fatalf("got %d calls", len(got))
	}
	c := got[0]
	if c.cwd != filepath.Join(cwd, "infra") || !reflect.DeepEqual(unwrap(c.argv), argv("kubectl delete pod web -n prod")) {
		t.Errorf("call %+v", c)
	}
	if !reflect.DeepEqual(c.assigns, []string{"KUBECONFIG=/k"}) || lookupEnv(c.env, "KUBECONFIG") != "/k" {
		t.Errorf("assigns %q", c.assigns)
	}
	if got := calls(`X=web; kubectl delete pod $X`, cwd, env); got[0].unresolved || got[0].argv[3] != "web" {
		t.Errorf("variable set on the line: %+v", got[0])
	}
	if got := calls(`kubectl delete pod $(kubectl get pods -o name)`, cwd, env); !got[0].unresolved {
		t.Error("command substitution should be unresolved")
	}
	if got := calls(`kubectl delete pod $UNSET_THING`, cwd, env); !got[0].unresolved {
		t.Error("unknown variable should be unresolved")
	}
	// PowerShell syntax the POSIX parser rejects.
	got = calls(`kubectl.exe delete pod web; if ($?) { Write-Host "ok" }`, cwd, env)
	if len(got) == 0 || progName(got[0].argv[0]) != "kubectl" || got[0].argv[3] != "web" {
		t.Errorf("rough split: %+v", got)
	}
	// Windows paths keep their backslashes.
	got = calls(`C:\tools\kubectl.exe delete pod "my web"`, cwd, env)
	if len(got) != 1 || got[0].argv[0] != `C:\tools\kubectl.exe` || got[0].argv[3] != "my web" {
		t.Errorf("windows path: %+v", got)
	}
}

func TestUnwrap(t *testing.T) {
	for in, want := range map[string]string{
		"sudo -E kubectl delete pod x":      "kubectl delete pod x",
		"env KUBECONFIG=/k kubectl get":     "kubectl get",
		"time nice terraform destroy":       "terraform destroy",
		"/usr/local/bin/helm uninstall web": "/usr/local/bin/helm uninstall web",
	} {
		if got := unwrap(argv(in)); !reflect.DeepEqual(got, argv(want)) {
			t.Errorf("%s: %q", in, got)
		}
	}
	if progName(`C:\bin\Kubectl.EXE`) != "kubectl" || progName("/opt/bin/tofu") != "tofu" {
		t.Error("progName")
	}
}
