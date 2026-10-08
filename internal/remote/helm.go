package remote

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/sohan-bhat/oopsy/internal/snap"
)

func init() {
	snap.RegisterRecordKind("helm", snap.RecordKind{Restore: restoreHelm})
}

// helmToKube maps Helm's connection flags to kubectl's, since Oopsy reads
// and writes the release storage with kubectl.
var helmToKube = map[string]string{
	"--kube-context": "--context", "--kubeconfig": "--kubeconfig", "--kube-apiserver": "--server",
	"--kube-token": "--token", "--kube-as-user": "--as", "--kube-as-group": "--as-group",
	"--kube-ca-file": "--certificate-authority", "--kube-tls-server-name": "--tls-server-name",
}

var helmValue = map[string]bool{"-n": true, "--namespace": true, "--registry-config": true, "--repository-cache": true,
	"--repository-config": true, "--burst-limit": true, "--qps": true, "--cascade": true, "--description": true,
	"--timeout": true, "--kube-insecure-skip-tls-verify": false}

// helmSpec is a parsed helm uninstall.
type helmSpec struct {
	Releases  []string
	Namespace string
	Kube      []string // kubectl flags for the same cluster
	Context   string
}

func parseHelm(args []string) (helmSpec, bool) {
	var h helmSpec
	var ops []string
	dry := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			ops = append(ops, a)
			continue
		}
		name, val, hasVal := strings.Cut(a, "=")
		if len(name) > 2 && name[1] == 'n' && name[0] == '-' && name[1:2] != "-" {
			name, val, hasVal = "-n", name[2:], true // -nprod
		}
		kube, isKube := helmToKube[name]
		if (isKube || helmValue[name]) && !hasVal {
			if i+1 >= len(args) {
				break
			}
			i++
			val = args[i]
		}
		switch {
		case isKube:
			h.Kube = append(h.Kube, kube+"="+val)
			if kube == "--context" {
				h.Context = val
			}
		case name == "-n" || name == "--namespace":
			h.Namespace = val
		case name == "--kube-insecure-skip-tls-verify":
			h.Kube = append(h.Kube, "--insecure-skip-tls-verify")
		case name == "--dry-run":
			dry = val != "false" && val != "none"
		}
	}
	if len(ops) < 2 || dry {
		return h, false
	}
	switch ops[0] {
	case "uninstall", "delete", "del", "un":
	default:
		return h, false
	}
	h.Releases = ops[1:]
	return h, true
}

func helmTargets(c call) []Target {
	h, ok := parseHelm(c.argv[1:])
	if !ok {
		return nil
	}
	if h.Namespace == "" {
		h.Namespace = lookupEnv(c.env, "HELM_NAMESPACE")
	}
	driver := strings.ToLower(lookupEnv(c.env, "HELM_DRIVER"))
	kind := "secret"
	switch driver {
	case "", "secret", "secrets":
	case "configmap", "configmaps":
		kind = "configmap"
	default:
		return []Target{{Tool: "helm", Describe: "nothing, Helm keeps releases in " + driver, Capture: func(string) (*snap.Record, error) {
			return nil, fmt.Errorf("helm uninstall: not saved because HELM_DRIVER is %s; Oopsy can save releases kept in secrets or configmaps", driver)
		}}}
	}
	kc := c
	kc.argv = []string{"kubectl"}
	t := newTool(kc, h.Kube)
	var out []Target
	for _, rel := range h.Releases {
		where := ""
		if h.Namespace != "" {
			where = " in namespace " + h.Namespace
		}
		out = append(out, Target{Tool: "helm", Describe: "Helm release " + rel + where + ": its history and the manifest of its resources", Capture: func(dir string) (*snap.Record, error) {
			return captureHelm(dir, t, h, kind, rel)
		}})
	}
	return out
}

// helmState is what a helm record keeps in its metadata.
type helmState struct {
	Tool      tool   `json:"tool"`
	Release   string `json:"release"`
	Namespace string `json:"namespace"`
	Revision  int    `json:"revision"`
	Chart     string `json:"chart"`
}

// release is the part of a stored Helm release Oopsy reads.
type release struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Version   int    `json:"version"`
	Manifest  string `json:"manifest"`
	Info      struct {
		Status string `json:"status"`
	} `json:"info"`
	Chart struct {
		Metadata struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"metadata"`
	} `json:"chart"`
}

// decodeRelease unpacks Helm's storage format: base64 of gzipped JSON,
// itself base64 encoded once more when it sits in a secret's data.
func decodeRelease(it map[string]any, secret bool) (*release, error) {
	data, _ := it["data"].(map[string]any)
	s, _ := data["release"].(string)
	b := []byte(s)
	if secret {
		var err error
		if b, err = base64.StdEncoding.DecodeString(s); err != nil {
			return nil, err
		}
	}
	z, err := base64.StdEncoding.DecodeString(string(b))
	if err != nil {
		return nil, err
	}
	if len(z) > 2 && z[0] == 0x1f && z[1] == 0x8b {
		zr, err := gzip.NewReader(bytes.NewReader(z))
		if err != nil {
			return nil, err
		}
		if z, err = io.ReadAll(zr); err != nil {
			return nil, err
		}
	}
	var r release
	return &r, json.Unmarshal(z, &r)
}

func captureHelm(dir string, t tool, h helmSpec, kind, rel string) (*snap.Record, error) {
	if _, err := exec.LookPath(t.Bin); err != nil {
		return nil, fmt.Errorf("helm uninstall %s: not saved because Oopsy needs kubectl to read the release, and it is not on PATH", rel)
	}
	if h.Context == "" {
		if out, err := t.run(captureTimeout, nil, nil, "config", "current-context"); err == nil {
			if ctx := strings.TrimSpace(string(out)); ctx != "" {
				t.Globals = append(t.Globals, "--context="+ctx)
			}
		}
	}
	args := []string{"get", kind, "--selector=owner=helm,name=" + rel, "--output=json"}
	if h.Namespace != "" {
		args = append(args, "--namespace="+h.Namespace)
	}
	out, err := t.run(captureTimeout, nil, nil, args...)
	if err != nil {
		return nil, fmt.Errorf("helm uninstall %s: could not read the release first: %v", rel, err)
	}
	items, err := parseItems(out)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, nil // no such release
	}
	var latest *release
	for _, it := range items {
		delete(metaOf(it), "managedFields")
		r, err := decodeRelease(it, kind == "secret")
		if err == nil && (latest == nil || r.Version > latest.Version) {
			latest = r
		}
	}
	if latest == nil {
		return nil, fmt.Errorf("helm uninstall %s: the stored release could not be decoded", rel)
	}
	data, _ := json.MarshalIndent(map[string]any{"apiVersion": "v1", "kind": "List", "items": items}, "", " ")
	if err := writeFile(dir, "release.json", data); err != nil {
		return nil, fmt.Errorf("helm uninstall %s: %v", rel, err)
	}
	if err := writeFile(dir, "manifest.yaml", []byte(latest.Manifest)); err != nil {
		return nil, fmt.Errorf("helm uninstall %s: %v", rel, err)
	}
	ns := latest.Namespace
	if ns == "" {
		ns = nsOf(items[0])
	}
	chart := latest.Chart.Metadata.Name + " " + latest.Chart.Metadata.Version
	st := helmState{Tool: t, Release: rel, Namespace: ns, Revision: latest.Version, Chart: chart}
	label := fmt.Sprintf("helm release %s in %s (chart %s, revision %d, %s)", rel, ns, chart, latest.Version, plural(len(items), "stored revision"))
	return &snap.Record{Kind: "helm", Label: label, Meta: meta(st), Size: int64(len(data) + len(latest.Manifest))}, nil
}

// restoreHelm puts the release's stored history back, so helm list and
// helm upgrade see it again, and recreates the resources from the manifest
// of the last revision. Resources made by chart hooks and data inside
// volumes are not part of the manifest and do not come back.
func restoreHelm(s *snap.Snapshot, r snap.Record) snap.Outcome {
	fail := func(msg string) snap.Outcome { return snap.Outcome{Path: r.Label, Status: "failed", Detail: msg} }
	var st helmState
	if err := unmeta(r.Meta, &st); err != nil {
		return fail("the saved details are unreadable")
	}
	dir := s.RecordDir(r)
	items, err := readItems(filepath.Join(dir, "release.json"))
	if err != nil {
		return fail(err.Error())
	}
	t := st.Tool
	var parts []string
	var errs []string
	manifest := filepath.Join(dir, "manifest.yaml")
	if b, _ := os.ReadFile(manifest); len(bytes.TrimSpace(b)) > 0 {
		out, err := t.run(restoreTimeout, nil, nil, "create", "--namespace="+st.Namespace, "--filename="+manifest)
		created := strings.Count(string(out), " created")
		other := otherErrors(err)
		if other != "" {
			errs = append(errs, other)
		}
		parts = append(parts, fmt.Sprintf("recreated %s from the saved manifest", plural(created, "resource")))
		if err != nil && other == "" {
			parts = append(parts, "the rest still existed and were left as they are")
		}
		// Mark them as Helm's, as helm install does, so helm upgrade adopts them.
		_, _ = t.run(restoreTimeout, nil, nil, "label", "--namespace="+st.Namespace, "--filename="+manifest, "--overwrite", "app.kubernetes.io/managed-by=Helm")
		_, _ = t.run(restoreTimeout, nil, nil, "annotate", "--namespace="+st.Namespace, "--filename="+manifest, "--overwrite",
			"meta.helm.sh/release-name="+st.Release, "meta.helm.sh/release-namespace="+st.Namespace)
	}
	rep := putBack(t, dir, items, true)
	errs = append(errs, rep.errors...)
	if len(rep.errors) == 0 {
		parts = append([]string{fmt.Sprintf("release %s is back in helm list at revision %d", st.Release, st.Revision)}, parts...)
	}
	parts = append(parts, "resources made by chart hooks and data inside volumes were not saved")
	detail := strings.Join(parts, "; ")
	if len(errs) > 0 {
		return fail(detail + "; kubectl said: " + strings.Join(errs, " "))
	}
	return snap.Outcome{Path: r.Label, Status: "record", Detail: detail}
}

// otherErrors drops "already exists" errors, which only mean a resource
// survived the uninstall (such as one kept by helm.sh/resource-policy).
func otherErrors(err error) string {
	if err == nil {
		return ""
	}
	full := err.Error()
	var ce *cmdError
	if errors.As(err, &ce) {
		full = ce.stderr
	}
	var keep []string
	for _, l := range strings.Split(full, "\n") {
		l = strings.TrimSpace(l)
		if l != "" && !strings.Contains(l, "AlreadyExists") && !strings.Contains(l, "already exists") {
			keep = append(keep, l)
		}
	}
	return firstLines(strings.Join(keep, "\n"), 3)
}
