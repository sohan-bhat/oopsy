package remote

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/sohan-bhat/oopsy/internal/snap"
)

func kubeTargets(c call) []Target {
	k, ok := parseKube(c.argv[1:])
	if !ok {
		return nil
	}
	name := progName(c.argv[0])
	if k.Stdin {
		return []Target{{Tool: name, Describe: "nothing, the objects come from standard input", Capture: func(string) (*snap.Record, error) {
			return nil, fmt.Errorf("%s %s: not saved because the objects come from standard input", name, k.Sub)
		}}}
	}
	get, ok := k.getArgs(c.cwd)
	if !ok {
		return nil
	}
	t := newTool(c, k.Globals)
	return []Target{{Tool: name, Describe: k.describe(), Capture: func(dir string) (*snap.Record, error) {
		return captureKube(dir, t, k, get)
	}}}
}

// kubeState is what a kubectl record keeps in its metadata.
type kubeState struct {
	Tool  tool     `json:"tool"`
	Mode  string   `json:"mode"`          // delete, apply or replace
	Get   []string `json:"get,omitempty"` // the selection, to find objects created by apply
	Prune bool     `json:"prune,omitempty"`
	Count int      `json:"count"`
}

func captureKube(dir string, t tool, k kubeSpec, get []string) (*snap.Record, error) {
	if k.Context == "" {
		// Pin the cluster: undo must not go wherever the current context
		// points at by then.
		args := []string{"config", "current-context"}
		if out, err := t.run(captureTimeout, nil, nil, args...); err == nil {
			if ctx := strings.TrimSpace(string(out)); ctx != "" {
				t.Globals = append(t.Globals, "--context="+ctx)
			}
		}
	}
	out, err := t.run(captureTimeout, nil, nil, get...)
	if err != nil {
		return nil, fmt.Errorf("%s %s: could not read the objects first: %v", t.Name, k.Sub, err)
	}
	items, err := parseItems(out)
	if err != nil {
		return nil, fmt.Errorf("%s %s: could not read kubectl output: %v", t.Name, k.Sub, err)
	}
	var nsNames []string
	for _, it := range items {
		if kindOf(it) == "Namespace" {
			nsNames = append(nsNames, nameOf(it))
		}
	}
	if len(nsNames) > 0 && k.Sub == "delete" {
		for _, ns := range nsNames {
			out, err := t.run(captureTimeout, nil, nil, "get", nsContents, "--namespace="+ns, "--output=json", "--ignore-not-found")
			if err != nil {
				return nil, fmt.Errorf("%s delete: could not read what is inside namespace %s: %v", t.Name, ns, err)
			}
			inner, err := parseItems(out)
			if err != nil {
				return nil, err
			}
			for _, it := range inner {
				if !autoCreated(it) {
					items = append(items, it)
				}
			}
		}
	}
	if len(items) == 0 && k.Sub == "delete" {
		return nil, nil // nothing there to lose
	}
	for _, it := range items {
		if md, ok := it["metadata"].(map[string]any); ok {
			delete(md, "managedFields") // never restorable, and often half the size
		}
	}
	data, err := json.MarshalIndent(map[string]any{"apiVersion": "v1", "kind": "List", "items": items}, "", " ")
	if err != nil {
		return nil, err
	}
	if err := writeFile(dir, "objects.json", data); err != nil {
		return nil, fmt.Errorf("%s %s: %v", t.Name, k.Sub, err)
	}
	st := kubeState{Tool: t, Mode: k.Sub, Prune: k.Prune, Count: len(items)}
	if k.Sub != "delete" {
		st.Get = get
	}
	return &snap.Record{Kind: "kubectl", Label: kubeLabel(k, items), Meta: meta(st), Size: int64(len(data))}, nil
}

// nsContents are the namespaced types saved with a deleted namespace. It
// covers what applications are made of; custom resources are not included.
const nsContents = "deployments.apps,statefulsets.apps,daemonsets.apps,replicasets.apps,pods,services,configmaps,secrets," +
	"persistentvolumeclaims,serviceaccounts,roles.rbac.authorization.k8s.io,rolebindings.rbac.authorization.k8s.io," +
	"ingresses.networking.k8s.io,networkpolicies.networking.k8s.io,cronjobs.batch,jobs.batch," +
	"poddisruptionbudgets.policy,horizontalpodautoscalers.autoscaling,limitranges,resourcequotas"

// autoCreated reports objects Kubernetes makes in every new namespace.
func autoCreated(it map[string]any) bool {
	switch kindOf(it) + "/" + nameOf(it) {
	case "ServiceAccount/default", "ConfigMap/kube-root-ca.crt":
		return true
	}
	if kindOf(it) == "Secret" && it["type"] == "kubernetes.io/service-account-token" {
		return true
	}
	return false
}

func kubeLabel(k kubeSpec, items []map[string]any) string {
	ctx := ""
	if k.Context != "" {
		ctx = " on " + k.Context
	}
	switch {
	case len(items) == 0:
		return fmt.Sprintf("kubectl %s of %s%s (none of them existed before)", k.Sub, kubeWhat(k), ctx)
	case len(items) == 1:
		return fmt.Sprintf("kubectl %s: %s%s", k.Sub, objRef(items[0]), ctx)
	}
	refs := make([]string, len(items))
	for i, it := range items {
		refs[i] = objRef(it)
	}
	return fmt.Sprintf("kubectl %s: %s (%s)%s", k.Sub, plural(len(items), "object"), listSome(refs, 3), ctx)
}

func kubeWhat(k kubeSpec) string {
	if len(k.Files) > 0 {
		names := make([]string, len(k.Files))
		for i, f := range k.Files {
			names[i] = filepath.Base(f)
		}
		return strings.Join(names, ", ")
	}
	if k.Kustomize != "" {
		return k.Kustomize
	}
	return strings.Join(k.Operands, " ")
}

// parseItems reads kubectl get -o json output: one object, a List, or
// nothing at all when --ignore-not-found found nothing.
func parseItems(out []byte) ([]map[string]any, error) {
	var items []map[string]any
	dec := json.NewDecoder(strings.NewReader(string(out)))
	for dec.More() {
		var obj map[string]any
		if err := dec.Decode(&obj); err != nil {
			return nil, err
		}
		if list, ok := obj["items"].([]any); ok && strings.HasSuffix(fmt.Sprint(obj["kind"]), "List") {
			for _, x := range list {
				if m, ok := x.(map[string]any); ok {
					items = append(items, m)
				}
			}
			continue
		}
		if obj["kind"] != nil {
			items = append(items, obj)
		}
	}
	return items, nil
}

func kindOf(it map[string]any) string { s, _ := it["kind"].(string); return s }

func metaOf(it map[string]any) map[string]any {
	m, _ := it["metadata"].(map[string]any)
	if m == nil {
		m = map[string]any{}
		it["metadata"] = m
	}
	return m
}

func nameOf(it map[string]any) string { s, _ := metaOf(it)["name"].(string); return s }

func nsOf(it map[string]any) string { s, _ := metaOf(it)["namespace"].(string); return s }

// objKey identifies an object across versions of its API.
func objKey(it map[string]any) string {
	group, _ := it["apiVersion"].(string)
	if i := strings.LastIndex(group, "/"); i >= 0 {
		group = group[:i]
	} else {
		group = ""
	}
	return group + "|" + kindOf(it) + "|" + nsOf(it) + "|" + nameOf(it)
}

// objRef is how objects are named to the user: deployment default/web.
func objRef(it map[string]any) string {
	n := nameOf(it)
	if ns := nsOf(it); ns != "" {
		n = ns + "/" + n
	}
	return strings.ToLower(kindOf(it)) + " " + n
}
