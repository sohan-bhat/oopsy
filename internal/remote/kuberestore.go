package remote

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sohan-bhat/oopsy/internal/snap"
)

func init() {
	snap.RegisterRecordKind("kubectl", snap.RecordKind{Restore: restoreKube})
}

func restoreKube(s *snap.Snapshot, r snap.Record) snap.Outcome {
	fail := func(msg string) snap.Outcome { return snap.Outcome{Path: r.Label, Status: "failed", Detail: msg} }
	var st kubeState
	if err := unmeta(r.Meta, &st); err != nil {
		return fail("the saved details are unreadable")
	}
	dir := s.RecordDir(r)
	items, err := readItems(filepath.Join(dir, "objects.json"))
	if err != nil {
		return fail(err.Error())
	}
	rep := putBack(st.Tool, dir, items, st.Mode != "delete")
	var parts []string
	if st.Mode == "delete" {
		parts = append(parts, rep.summary("recreated"))
	} else {
		parts = append(parts, rep.summary("put back the previous version of"))
		parts = append(parts, createdSince(st, items)...)
	}
	if st.Prune {
		parts = append(parts, "objects removed by --prune were not saved")
	}
	detail := strings.Join(nonEmpty(parts), "; ")
	if len(rep.errors) > 0 {
		return fail(detail + "; kubectl said: " + strings.Join(rep.errors, " "))
	}
	return snap.Outcome{Path: r.Label, Status: "record", Detail: detail}
}

func readItems(p string) ([]map[string]any, error) {
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("the saved objects are missing: %v", err)
	}
	items, err := parseItems(data)
	if err != nil {
		return nil, fmt.Errorf("the saved objects are unreadable: %v", err)
	}
	return items, nil
}

func nonEmpty(list []string) []string {
	var out []string
	for _, s := range list {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// report says what putBack did.
type report struct {
	created, replaced, leftAlone, owned []string
	pvcs                                int
	errors                              []string
}

func (r report) summary(verb string) string {
	var parts []string
	if n := len(r.created) + len(r.replaced); n > 0 {
		parts = append(parts, fmt.Sprintf("%s %s (%s)", verb, plural(n, "object"), listSome(append(r.created, r.replaced...), 4)))
	}
	if len(r.leftAlone) > 0 {
		parts = append(parts, "not touched, since they exist again: "+listSome(r.leftAlone, 3))
	}
	if len(r.owned) > 0 {
		parts = append(parts, "left to their controllers to recreate, as with pods of a deployment: "+listSome(r.owned, 3))
	}
	if r.pvcs > 0 {
		parts = append(parts, "persistent volume claims come back as new empty volumes, since the data was in the deleted volume")
	}
	return strings.Join(parts, "; ")
}

// putBack recreates the saved objects that are missing, and with replace
// also puts back the saved version of objects that exist. Objects that a
// controller owns are left to it, since recreating them by hand would
// fight the controller.
func putBack(t tool, dir string, items []map[string]any, replace bool) report {
	var rep report
	var cands []map[string]any
	for _, it := range items {
		switch {
		case controlled(it):
			rep.owned = append(rep.owned, objRef(it))
		case autoCreated(it):
		default:
			cands = append(cands, it)
		}
	}
	if len(cands) == 0 {
		return rep
	}
	existing := currentObjects(t, dir, cands)
	if waitNamespaces(t, existing) {
		existing = currentObjects(t, dir, cands)
	}
	var first, rest, repl []map[string]any
	for _, it := range cands {
		cur, ok := existing[objKey(it)]
		switch {
		case !ok:
			c := cleanObj(it, true)
			if k := kindOf(c); k == "Namespace" || k == "CustomResourceDefinition" {
				first = append(first, c)
			} else {
				rest = append(rest, c)
			}
		case replace:
			if !sameObject(it, cur) {
				repl = append(repl, cleanObj(it, false))
			}
		default:
			rep.leftAlone = append(rep.leftAlone, objRef(it))
		}
	}
	for _, batch := range [][]map[string]any{first, rest} {
		if len(batch) == 0 {
			continue
		}
		if err := kubeSend(t, dir, "create", batch); err != nil {
			rep.errors = append(rep.errors, err.Error())
		}
		created := currentObjects(t, dir, batch)
		for _, it := range batch {
			if _, ok := created[objKey(it)]; ok {
				rep.created = append(rep.created, objRef(it))
				if kindOf(it) == "PersistentVolumeClaim" {
					rep.pvcs++
				}
			}
		}
	}
	if len(repl) > 0 {
		if err := kubeSend(t, dir, "replace", repl); err != nil {
			rep.errors = append(rep.errors, err.Error())
		} else {
			for _, it := range repl {
				rep.replaced = append(rep.replaced, objRef(it))
			}
		}
	}
	return rep
}

// kubeSend runs kubectl create or replace on a list of objects.
func kubeSend(t tool, dir, verb string, objs []map[string]any) error {
	data, err := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "List", "items": objs})
	if err != nil {
		return err
	}
	_, err = t.run(restoreTimeout, bytes.NewReader(data), nil, verb, "--filename=-")
	return err
}

// currentObjects reads which of objs exist now, keyed by objKey.
func currentObjects(t tool, dir string, objs []map[string]any) map[string]map[string]any {
	data, _ := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "List", "items": objs})
	out, err := t.run(restoreTimeout, bytes.NewReader(data), nil, "get", "--filename=-", "--output=json", "--ignore-not-found")
	m := map[string]map[string]any{}
	if err != nil {
		return m
	}
	items, _ := parseItems(out)
	for _, it := range items {
		m[objKey(it)] = it
	}
	return m
}

// waitNamespaces waits for namespaces that are still being deleted to go
// away, since nothing can be created in them until then.
func waitNamespaces(t tool, existing map[string]map[string]any) bool {
	var names []string
	for _, it := range existing {
		if kindOf(it) == "Namespace" && metaOf(it)["deletionTimestamp"] != nil {
			names = append(names, nameOf(it))
		}
	}
	if len(names) == 0 {
		return false
	}
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		out, err := t.run(captureTimeout, nil, nil, append([]string{"get", "namespace", "--output=name", "--ignore-not-found"}, names...)...)
		if err == nil && strings.TrimSpace(string(out)) == "" {
			break
		}
		time.Sleep(2 * time.Second)
	}
	return true
}

// controlled reports objects with a controlling owner, such as a pod made
// by a ReplicaSet.
func controlled(it map[string]any) bool {
	refs, _ := metaOf(it)["ownerReferences"].([]any)
	for _, r := range refs {
		if m, ok := r.(map[string]any); ok && m["controller"] == true {
			return true
		}
	}
	return false
}

// sameObject reports whether the live object still matches the saved one,
// in which case there is nothing to put back.
func sameObject(saved, live map[string]any) bool {
	a, _ := metaOf(saved)["resourceVersion"].(string)
	b, _ := metaOf(live)["resourceVersion"].(string)
	return a != "" && a == b
}

// serverFields are set by the API server and refused or meaningless when
// an object is sent back.
var serverFields = []string{"uid", "resourceVersion", "creationTimestamp", "managedFields", "generation",
	"selfLink", "deletionTimestamp", "deletionGracePeriodSeconds"}

// cleanObj prepares a saved object to be sent back. Everything the user
// set stays, including the kubectl last applied annotation: create and
// replace store it as it was, so a later kubectl apply merges as before.
// For create, fields tied to the deleted object's identity go too: owner
// references (the owner's uid changed), a Service's cluster IP (it may be
// taken by now), a claim's bound volume (it was released or deleted) and a
// Job's generated selector.
func cleanObj(it map[string]any, create bool) map[string]any {
	var o map[string]any
	b, _ := json.Marshal(it)
	_ = json.Unmarshal(b, &o)
	md := metaOf(o)
	for _, f := range serverFields {
		delete(md, f)
	}
	delete(o, "status")
	if !create {
		return o
	}
	delete(md, "ownerReferences")
	spec, _ := o["spec"].(map[string]any)
	if spec == nil {
		return o
	}
	switch kindOf(o) {
	case "Service":
		if ip, _ := spec["clusterIP"].(string); ip != "None" {
			delete(spec, "clusterIP")
			delete(spec, "clusterIPs")
		}
	case "PersistentVolumeClaim":
		delete(spec, "volumeName")
		if ann, ok := md["annotations"].(map[string]any); ok {
			for _, a := range []string{"pv.kubernetes.io/bind-completed", "pv.kubernetes.io/bound-by-controller",
				"volume.beta.kubernetes.io/storage-provisioner", "volume.kubernetes.io/storage-provisioner", "volume.kubernetes.io/selected-node"} {
				delete(ann, a)
			}
		}
	case "Job":
		if spec["manualSelector"] != true {
			delete(spec, "selector")
			if tmpl, ok := spec["template"].(map[string]any); ok {
				if tm, ok := tmpl["metadata"].(map[string]any); ok {
					if l, ok := tm["labels"].(map[string]any); ok {
						delete(l, "controller-uid")
						delete(l, "batch.kubernetes.io/controller-uid")
					}
				}
			}
		}
	}
	return o
}

// createdSince lists objects an apply or replace created, which did not
// exist before. They are left in place: undo only ever brings things back,
// and deleting a new object could lose data written to it since (a new
// volume claim, a secret something now reads). The exact command to remove
// them is printed instead.
func createdSince(st kubeState, before []map[string]any) []string {
	if len(st.Get) == 0 {
		return nil
	}
	out, err := st.Tool.run(restoreTimeout, nil, nil, st.Get...)
	if err != nil {
		return []string{"could not check for objects the command created, since its files are no longer readable"}
	}
	now, _ := parseItems(out)
	had := map[string]bool{}
	for _, it := range before {
		had[objKey(it)] = true
	}
	var refs, args []string
	for _, it := range now {
		if !had[objKey(it)] {
			refs = append(refs, objRef(it))
			arg := strings.ToLower(kindOf(it)) + "/" + nameOf(it)
			if ns := nsOf(it); ns != "" {
				arg += " -n " + ns
			}
			args = append(args, arg)
		}
	}
	if len(refs) == 0 {
		return nil
	}
	return []string{fmt.Sprintf("%s created by the command did not exist before and %s left in place (%s); to remove: kubectl delete %s",
		plural(len(refs), "object"), isAre(len(refs)), listSome(refs, 4), strings.Join(args, "; kubectl delete "))}
}

func isAre(n int) string {
	if n == 1 {
		return "was"
	}
	return "were"
}
