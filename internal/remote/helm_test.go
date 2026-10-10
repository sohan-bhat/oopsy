package remote

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"path/filepath"
	"reflect"
	"testing"
)

func TestHelmParse(t *testing.T) {
	h, ok := parseHelm(argv("uninstall web api -n prod --kube-context staging --wait --timeout 5m"))
	if !ok || !reflect.DeepEqual(h.Releases, []string{"web", "api"}) || h.Namespace != "prod" || h.Context != "staging" {
		t.Errorf("got %+v %v", h, ok)
	}
	if !reflect.DeepEqual(h.Kube, []string{"--context=staging"}) {
		t.Errorf("kube flags %q", h.Kube)
	}
	if h, _ := parseHelm(argv("uninstall web -nprod")); h.Namespace != "prod" {
		t.Errorf("-nprod: %+v", h)
	}
	for _, line := range []string{"delete web", "del web --namespace=x", "un web", "--kubeconfig /k uninstall web --keep-history"} {
		if _, ok := parseHelm(argv(line)); !ok {
			t.Errorf("helm %s not parsed", line)
		}
	}
	for _, line := range []string{"uninstall web --dry-run", "list", "uninstall", "install web chart"} {
		if _, ok := parseHelm(argv(line)); ok {
			t.Errorf("helm %s should save nothing", line)
		}
	}
}

func TestDecodeRelease(t *testing.T) {
	rel := `{"name":"web","namespace":"prod","version":3,"manifest":"---\nkind: ConfigMap\n","info":{"status":"deployed"},"chart":{"metadata":{"name":"nginx","version":"1.2.3"}}}`
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write([]byte(rel))
	zw.Close()
	helmEnc := base64.StdEncoding.EncodeToString(gz.Bytes())
	secret := obj(`{"kind":"Secret","data":{"release":"` + base64.StdEncoding.EncodeToString([]byte(helmEnc)) + `"}}`)
	r, err := decodeRelease(secret, true)
	if err != nil || r.Name != "web" || r.Version != 3 || r.Chart.Metadata.Name != "nginx" || r.Manifest == "" {
		t.Fatalf("secret: %+v %v", r, err)
	}
	cm := obj(`{"kind":"ConfigMap","data":{"release":"` + helmEnc + `"}}`)
	if r, err := decodeRelease(cm, false); err != nil || r.Info.Status != "deployed" {
		t.Fatalf("configmap: %+v %v", r, err)
	}
}

func TestTargets(t *testing.T) {
	cwd := filepath.FromSlash("/work")
	for line, n := range map[string]int{
		"kubectl delete pod web":                          1,
		"oc delete project x":                             1,
		"terraform destroy -auto-approve":                 1,
		"tofu -chdir=infra apply":                         1,
		"docker volume rm a b":                            2,
		"podman volume prune":                             1,
		"helm uninstall web":                              1,
		"ssh prod rm -rf /":                               0,
		"kubectl get pods && docker ps":                   0,
		"ls":                                              0,
		`C:\tools\kubectl.exe delete -f app.yaml`:         1,
		"kubectl delete pod $(cat names)":                 1, // reported as not saved
		"echo kubectl delete pod web":                     0,
		"terraform plan && terraform apply -auto-approve": 1,
	} {
		if got := Targets(line, cwd, nil); len(got) != n {
			t.Errorf("%s: %d targets, want %d", line, len(got), n)
		}
	}
}
