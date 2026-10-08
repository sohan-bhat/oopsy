package remote

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestTerraformParse(t *testing.T) {
	cwd := filepath.FromSlash("/work")
	cases := []struct {
		line, mode, dir string
	}{
		{"destroy", "destroy", cwd},
		{"destroy -auto-approve", "destroy", cwd},
		{"-chdir=infra destroy", "destroy", filepath.Join(cwd, "infra")},
		{"apply", "apply", cwd},
		{"apply -auto-approve plan.out", "apply", cwd},
		{"apply -destroy -auto-approve", "destroy", cwd},
		{"state rm aws_instance.web", "state rm", cwd},
		{"state rm -dry-run aws_instance.web", "", ""},
		{"state list", "", ""},
		{"plan", "", ""},
		{"destroy -help", "", ""},
		{"-version", "", ""},
	}
	for _, c := range cases {
		sp, ok := parseTF(argv(c.line), cwd)
		if !ok {
			sp = tfSpec{}
		}
		if sp.Mode != c.mode || sp.Dir != c.dir {
			t.Errorf("terraform %s: got %+v, want mode %q dir %q", c.line, sp, c.mode, c.dir)
		}
	}
}

func TestWithSerial(t *testing.T) {
	in := []byte(`{"version":4,"serial":3,"lineage":"abc","resources":[{"a":1.50}]}`)
	out, err := withSerial(in, 9)
	if err != nil {
		t.Fatal(err)
	}
	var b map[string]any
	json.Unmarshal(out, &b)
	if b["serial"] != float64(9) || b["lineage"] != "abc" {
		t.Errorf("got %s", out)
	}
	if !bytes.Contains(out, []byte(`1.50`)) {
		t.Error("numbers should be kept byte for byte")
	}
}

func TestStateAddresses(t *testing.T) {
	var sf stateFile
	json.Unmarshal([]byte(`{"resources":[{"mode":"managed","type":"terraform_data","name":"a","instances":[{}]},
		{"mode":"data","type":"x","name":"d","instances":[{}]},{"module":"module.m","mode":"managed","type":"t","name":"b","instances":[{}]}]}`), &sf)
	got := sf.addresses()
	if len(got) != 2 || got[0] != "module.m.t.b" || got[1] != "terraform_data.a" {
		t.Errorf("%q", got)
	}
}
