package e2e

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// needCluster skips unless kubectl reaches a cluster. CI starts a kind
// cluster and sets KUBECONFIG, since the tests run with a fake HOME.
func needCluster(t *testing.T, e *env) {
	t.Helper()
	needTools(t, "kubectl")
	if out, err := e.tool("", "kubectl", "version", "--request-timeout=10s"); err != nil {
		if os.Getenv("OOPS_TEST_REMOTE") == "1" {
			t.Fatalf("no Kubernetes cluster: %v\n%s", err, out)
		}
		t.Skip("no Kubernetes cluster reachable")
	}
}

func randName(prefix string) string {
	b := make([]byte, 4)
	rand.Read(b)
	return prefix + "-" + hex.EncodeToString(b)
}

// tempNamespace creates a namespace that is deleted when the test ends.
func tempNamespace(t *testing.T, e *env) string {
	ns := randName("oopsy")
	e.mustTool("kubectl", "create", "namespace", ns)
	t.Cleanup(func() { e.tool("", "kubectl", "delete", "namespace", ns, "--wait=false", "--ignore-not-found") })
	return ns
}

const k8sApp = `apiVersion: v1
kind: ConfigMap
metadata:
  name: app
  labels: {app: web, tier: config}
data:
  greeting: hello
  config.json: '{"replicas": 2, "debug": false}'
---
apiVersion: v1
kind: Secret
metadata:
  name: creds
  labels: {app: web}
type: Opaque
stringData:
  password: s3cret
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  labels: {app: web}
spec:
  replicas: 1
  selector: {matchLabels: {app: web}}
  template:
    metadata: {labels: {app: web}}
    spec:
      containers:
      - name: pause
        image: registry.k8s.io/pause:3.9
        env: [{name: MODE, value: prod}]
---
apiVersion: v1
kind: Service
metadata:
  name: web
  labels: {app: web}
spec:
  selector: {app: web}
  ports: [{port: 80, targetPort: 8080}]
`

// k8sState is the part of objects that undo must bring back: labels,
// annotations, data and spec, without fields the server assigns anew.
func k8sState(t *testing.T, e *env, ns string, refs ...string) map[string]string {
	t.Helper()
	out := e.mustTool("kubectl", append([]string{"get", "-n", ns, "-o", "json"}, refs...)...)
	var list struct {
		Kind  string           `json:"kind"`
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		t.Fatal(err)
	}
	if list.Kind != "List" { // one object comes back on its own
		var one map[string]any
		json.Unmarshal([]byte(out), &one)
		list.Items = []map[string]any{one}
	}
	state := map[string]string{}
	for _, it := range list.Items {
		md := it["metadata"].(map[string]any)
		if ann, ok := md["annotations"].(map[string]any); ok {
			delete(ann, "deployment.kubernetes.io/revision")
		}
		if spec, ok := it["spec"].(map[string]any); ok && it["kind"] == "Service" {
			delete(spec, "clusterIP")
			delete(spec, "clusterIPs")
		}
		keep := map[string]any{"labels": md["labels"], "annotations": md["annotations"], "data": it["data"], "spec": it["spec"], "type": it["type"]}
		b, _ := json.Marshal(keep)
		state[fmt.Sprintf("%s/%s", it["kind"], md["name"])] = string(b)
	}
	return state
}

func compareK8s(t *testing.T, before, after map[string]string) {
	t.Helper()
	if len(before) == 0 {
		t.Fatal("nothing to compare")
	}
	if !reflect.DeepEqual(before, after) {
		var keys []string
		for k := range before {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if before[k] != after[k] {
				t.Errorf("%s differs:\n before %s\n after  %s", k, before[k], after[k])
			}
		}
		t.Fatalf("objects not restored state for state (%d before, %d after)", len(before), len(after))
	}
}

func TestKubectlDeleteRestores(t *testing.T) {
	e := newEnv(t)
	needCluster(t, e)
	ns := tempNamespace(t, e)
	os.WriteFile(filepath.Join(e.work, "app.yaml"), []byte(k8sApp), 0o644)
	e.mustTool("kubectl", "apply", "-n", ns, "-f", "app.yaml")
	refs := []string{"configmap/app", "secret/creds", "deployment/web", "service/web"}
	before := k8sState(t, e, ns, refs...)

	msg := e.hookThenRun("kubectl delete -n " + ns + " " + strings.Join(refs, " "))
	if !strings.Contains(msg, "saved snapshot") {
		t.Fatalf("no snapshot: %s", msg)
	}
	if out, _ := e.tool("", "kubectl", "get", "-n", ns, "configmap/app"); !strings.Contains(out, "NotFound") {
		t.Fatalf("delete did not happen: %s", out)
	}
	show := e.mustOops("show", "1")
	t.Logf("oops show:\n%s", show)
	out := e.mustOops("-y")
	t.Logf("oops -y:\n%s", out)
	compareK8s(t, before, k8sState(t, e, ns, refs...))

	// Undo twice keeps what exists now.
	out = e.mustOops("undo", "1", "-y")
	if !strings.Contains(out, "exist again") {
		t.Errorf("second undo should leave existing objects alone: %s", out)
	}
	e.logRecords()
}

func TestKubectlDeleteNamespaceRestoresContents(t *testing.T) {
	e := newEnv(t)
	needCluster(t, e)
	ns := tempNamespace(t, e)
	os.WriteFile(filepath.Join(e.work, "app.yaml"), []byte(k8sApp), 0o644)
	e.mustTool("kubectl", "apply", "-n", ns, "-f", "app.yaml")
	refs := []string{"configmap/app", "secret/creds", "deployment/web", "service/web"}
	before := k8sState(t, e, ns, refs...)

	e.hookThenRun("kubectl delete namespace " + ns)
	start := time.Now()
	out := e.mustOops("-y")
	t.Logf("oops -y after %s:\n%s", time.Since(start), out)
	compareK8s(t, before, k8sState(t, e, ns, refs...))
	e.logRecords()
}

func TestKubectlApplyUndoPutsBackPreviousVersion(t *testing.T) {
	e := newEnv(t)
	needCluster(t, e)
	ns := tempNamespace(t, e)
	os.WriteFile(filepath.Join(e.work, "app.yaml"), []byte(k8sApp), 0o644)
	e.mustTool("kubectl", "apply", "-n", ns, "-f", "app.yaml")
	before := k8sState(t, e, ns, "configmap/app", "deployment/web")

	changed := strings.Replace(k8sApp, "greeting: hello", "greeting: goodbye\n  extra: added", 1)
	changed = strings.Replace(changed, "value: prod", "value: broken", 1)
	changed += "---\napiVersion: v1\nkind: ConfigMap\nmetadata: {name: brand-new}\ndata: {a: b}\n"
	os.WriteFile(filepath.Join(e.work, "app.yaml"), []byte(changed), 0o644)
	e.hookThenRun("kubectl apply -n " + ns + " -f app.yaml")
	if after := k8sState(t, e, ns, "configmap/app"); after["ConfigMap/app"] == before["ConfigMap/app"] {
		t.Fatal("apply changed nothing")
	}
	out := e.mustOops("-y")
	t.Logf("oops -y:\n%s", out)
	compareK8s(t, before, k8sState(t, e, ns, "configmap/app", "deployment/web"))
	if !strings.Contains(out, "configmap "+ns+"/brand-new") {
		t.Errorf("the object the apply created should be listed: %s", out)
	}
	e.mustTool("kubectl", "get", "-n", ns, "configmap/brand-new") // left in place
	e.logRecords()
}

func TestHelmUninstallRestores(t *testing.T) {
	e := newEnv(t)
	needCluster(t, e)
	needTools(t, "helm")
	ns := tempNamespace(t, e)
	chart := filepath.Join(e.work, "chart")
	os.MkdirAll(filepath.Join(chart, "templates"), 0o755)
	os.WriteFile(filepath.Join(chart, "Chart.yaml"), []byte("apiVersion: v2\nname: demo\nversion: 1.2.3\n"), 0o644)
	os.WriteFile(filepath.Join(chart, "values.yaml"), []byte("greeting: hello\n"), 0o644)
	os.WriteFile(filepath.Join(chart, "templates", "cm.yaml"), []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ .Release.Name }}-cfg
  labels: {app: demo}
data:
  greeting: {{ .Values.greeting | quote }}
  password: {{ randAlphaNum 16 | quote }}
`), 0o644)
	e.mustTool("helm", "install", "rel", chart, "-n", ns)
	e.mustTool("helm", "upgrade", "rel", chart, "-n", ns, "--set", "greeting=hi")
	before := k8sState(t, e, ns, "configmap/rel-cfg")

	e.hookThenRun("helm uninstall rel -n " + ns)
	if out := e.mustTool("helm", "list", "-n", ns, "-q"); strings.Contains(out, "rel") {
		t.Fatalf("uninstall did not happen: %s", out)
	}
	out := e.mustOops("-y")
	t.Logf("oops -y:\n%s", out)
	compareK8s(t, before, k8sState(t, e, ns, "configmap/rel-cfg"))
	var rels []struct {
		Name     string `json:"name"`
		Revision string `json:"revision"`
		Status   string `json:"status"`
	}
	json.Unmarshal([]byte(e.mustTool("helm", "list", "-n", ns, "-o", "json")), &rels)
	if len(rels) != 1 || rels[0].Name != "rel" || rels[0].Status != "deployed" || rels[0].Revision != "2" {
		t.Fatalf("release not back in helm list: %+v", rels)
	}
	if hist := e.mustTool("helm", "history", "rel", "-n", ns); strings.Count(hist, "\n") != 3 {
		t.Errorf("history should have both revisions:\n%s", hist)
	}
	// Helm manages it again: an upgrade works and keeps going from revision 2.
	e.mustTool("helm", "upgrade", "rel", chart, "-n", ns, "--set", "greeting=again")
	e.logRecords()
}
