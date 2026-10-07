package remote

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestKubeGetArgs(t *testing.T) {
	cwd := filepath.FromSlash("/work")
	abs := func(p string) string { return filepath.Join(cwd, p) }
	cases := []struct {
		line string
		want []string // nil when nothing is saved
	}{
		{"delete pod web", []string{"get", "pod", "web", "--output=json", "--ignore-not-found"}},
		{"delete pod/web svc/web -n prod", []string{"get", "pod/web", "svc/web", "--namespace=prod", "--output=json", "--ignore-not-found"}},
		{"-n prod delete deploy a b", []string{"get", "deploy", "a", "b", "--namespace=prod", "--output=json", "--ignore-not-found"}},
		{"delete --namespace=prod configmap app", []string{"get", "configmap", "app", "--namespace=prod", "--output=json", "--ignore-not-found"}},
		{"delete -nprod cm app", []string{"get", "cm", "app", "--namespace=prod", "--output=json", "--ignore-not-found"}},
		{"delete pods -l app=web", []string{"get", "pods", "--selector=app=web", "--output=json", "--ignore-not-found"}},
		{"delete pods --all -A", []string{"get", "pods", "--all-namespaces", "--output=json", "--ignore-not-found"}},
		{"delete pods --all --all-namespaces -n x", []string{"get", "pods", "--all-namespaces", "--output=json", "--ignore-not-found"}},
		{"delete pods --field-selector status.phase=Failed", []string{"get", "pods", "--field-selector=status.phase=Failed", "--output=json", "--ignore-not-found"}},
		{"delete -f app.yaml", []string{"get", "--filename=" + abs("app.yaml"), "--output=json", "--ignore-not-found"}},
		{"delete -Rf manifests", []string{"get", "--filename=" + abs("manifests"), "--recursive", "--output=json", "--ignore-not-found"}},
		{"delete -f a.yaml --filename=b.yaml -R", []string{"get", "--filename=" + abs("a.yaml"), "--filename=" + abs("b.yaml"), "--recursive", "--output=json", "--ignore-not-found"}},
		{"delete -k overlays/prod", []string{"get", "--kustomize=" + abs("overlays/prod"), "--output=json", "--ignore-not-found"}},
		{"apply -f app.yaml -n prod", []string{"get", "--filename=" + abs("app.yaml"), "--namespace=prod", "--output=json", "--ignore-not-found"}},
		{"apply -f https://example.com/x.yaml", []string{"get", "--filename=https://example.com/x.yaml", "--output=json", "--ignore-not-found"}},
		{"replace --force -f app.yaml", []string{"get", "--filename=" + abs("app.yaml"), "--output=json", "--ignore-not-found"}},
		{"delete pod web --grace-period 0 --force", []string{"get", "pod", "web", "--output=json", "--ignore-not-found"}},
		{"delete pod web --cascade=orphan --wait=false", []string{"get", "pod", "web", "--output=json", "--ignore-not-found"}},
		{"delete pods", nil}, // kubectl refuses a bare type
		{"get pods", nil},    // not destructive
		{"delete pod web --dry-run=client", nil},
		{"apply -f app.yaml --dry-run=server", nil},
		{"apply view-last-applied deploy/web", nil},
		{"delete", nil},
	}
	for _, c := range cases {
		k, ok := parseKube(argv(c.line))
		var got []string
		if ok {
			got, _ = k.getArgs(cwd)
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("kubectl %s:\n got  %q\n want %q", c.line, got, c.want)
		}
	}
}

func TestKubeGlobals(t *testing.T) {
	k, ok := parseKube(argv("--context prod --kubeconfig /k/config -v 6 delete pod web --insecure-skip-tls-verify -s https://api:6443"))
	if !ok {
		t.Fatal("not parsed")
	}
	want := []string{"--context=prod", "--kubeconfig=/k/config", "--insecure-skip-tls-verify", "--server=https://api:6443"}
	if !reflect.DeepEqual(k.Globals, want) {
		t.Errorf("globals %q, want %q", k.Globals, want)
	}
	if k.Context != "prod" || k.Sub != "delete" {
		t.Errorf("context %q sub %q", k.Context, k.Sub)
	}
	k, _ = parseKube(argv("delete -f -"))
	if !k.Stdin {
		t.Error("-f - should read stdin")
	}
	k, _ = parseKube(argv("apply --prune -l app=web -f dir"))
	if !k.Prune || k.Selector != "app=web" {
		t.Errorf("prune: %+v", k)
	}
}

func TestKubeNamespaces(t *testing.T) {
	for line, want := range map[string][]string{
		"delete ns a b":             {"a", "b"},
		"delete namespace/a pod/x":  {"a"},
		"delete pod a":              nil,
		"apply -f ns.yaml":          nil,
		"delete namespaces staging": {"staging"},
	} {
		k, _ := parseKube(argv(line))
		if got := k.namespaces(); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: %q, want %q", line, got, want)
		}
	}
}
