package remote

import (
	"encoding/json"
	"testing"
)

func TestParseItems(t *testing.T) {
	one := `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"a","namespace":"default"}}`
	list := `{"apiVersion":"v1","kind":"List","items":[` + one + `,{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"web","namespace":"prod"}}]}`
	for in, n := range map[string]int{"": 0, "\n": 0, one: 1, list: 2, one + "\n" + one: 2} {
		items, err := parseItems([]byte(in))
		if err != nil || len(items) != n {
			t.Errorf("%q: %d items, %v", in, len(items), err)
		}
	}
	items, _ := parseItems([]byte(list))
	if objKey(items[1]) != "apps|Deployment|prod|web" || objRef(items[1]) != "deployment prod/web" {
		t.Errorf("key %q ref %q", objKey(items[1]), objRef(items[1]))
	}
}

func obj(s string) map[string]any {
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		panic(err)
	}
	return m
}

func TestCleanObj(t *testing.T) {
	svc := obj(`{"apiVersion":"v1","kind":"Service","metadata":{"name":"web","uid":"u","resourceVersion":"9","creationTimestamp":"x",
		"ownerReferences":[{"uid":"o"}],"labels":{"app":"web"},"annotations":{"kubectl.kubernetes.io/last-applied-configuration":"{}"}},
		"spec":{"clusterIP":"10.0.0.5","clusterIPs":["10.0.0.5"],"ports":[{"port":80}]},"status":{"loadBalancer":{}}}`)
	c := cleanObj(svc, true)
	md := metaOf(c)
	for _, f := range []string{"uid", "resourceVersion", "creationTimestamp", "ownerReferences"} {
		if _, ok := md[f]; ok {
			t.Errorf("%s kept", f)
		}
	}
	if _, ok := c["status"]; ok {
		t.Error("status kept")
	}
	if _, ok := c["spec"].(map[string]any)["clusterIP"]; ok {
		t.Error("cluster IP kept on create")
	}
	if md["labels"].(map[string]any)["app"] != "web" || md["annotations"] == nil {
		t.Error("labels or annotations lost")
	}
	if metaOf(svc)["uid"] != "u" {
		t.Error("the saved object was modified")
	}
	r := cleanObj(svc, false)
	if r["spec"].(map[string]any)["clusterIP"] != "10.0.0.5" || metaOf(r)["ownerReferences"] == nil {
		t.Error("replace should keep the cluster IP and owners")
	}
	headless := cleanObj(obj(`{"kind":"Service","metadata":{"name":"h"},"spec":{"clusterIP":"None"}}`), true)
	if headless["spec"].(map[string]any)["clusterIP"] != "None" {
		t.Error("headless service lost clusterIP None")
	}
	job := cleanObj(obj(`{"kind":"Job","metadata":{"name":"j"},"spec":{"selector":{"matchLabels":{"controller-uid":"x"}},
		"template":{"metadata":{"labels":{"controller-uid":"x","batch.kubernetes.io/controller-uid":"x","app":"j"}}}}}`), true)
	spec := job["spec"].(map[string]any)
	labels := spec["template"].(map[string]any)["metadata"].(map[string]any)["labels"].(map[string]any)
	if spec["selector"] != nil || labels["controller-uid"] != nil || labels["app"] != "j" {
		t.Errorf("job not cleaned: %v", spec)
	}
	pvc := cleanObj(obj(`{"kind":"PersistentVolumeClaim","metadata":{"name":"d","annotations":{"pv.kubernetes.io/bind-completed":"yes"}},"spec":{"volumeName":"pv-1"}}`), true)
	if pvc["spec"].(map[string]any)["volumeName"] != nil || len(metaOf(pvc)["annotations"].(map[string]any)) != 0 {
		t.Error("claim still bound to its old volume")
	}
}

func TestControlled(t *testing.T) {
	if !controlled(obj(`{"metadata":{"ownerReferences":[{"kind":"ReplicaSet","controller":true}]}}`)) {
		t.Error("pod of a ReplicaSet should be controlled")
	}
	if controlled(obj(`{"metadata":{"ownerReferences":[{"kind":"ConfigMap"}]}}`)) || controlled(obj(`{"metadata":{}}`)) {
		t.Error("plain owner or none is not a controller")
	}
}
