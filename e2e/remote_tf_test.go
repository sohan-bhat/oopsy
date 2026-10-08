package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The config uses only terraform_data, which is built into Terraform, so
// no provider is downloaded.
const tfConfig = `resource "terraform_data" "server" {
  input = "web-1"
}

resource "terraform_data" "settings" {
  input = {
    replicas = 3
    region   = "eu"
  }
  triggers_replace = [terraform_data.server.id]
}

output "settings" {
  value = terraform_data.settings.output
}
`

// tfStatePull returns the state as parsed JSON, without the serial, which
// a push has to raise.
func tfStatePull(t *testing.T, e *env) (map[string]any, float64) {
	t.Helper()
	var st map[string]any
	if err := json.Unmarshal([]byte(e.mustTool("terraform", "state", "pull")), &st); err != nil {
		t.Fatal(err)
	}
	serial, _ := st["serial"].(float64)
	delete(st, "serial")
	return st, serial
}

func TestTerraformDestroyRestoresState(t *testing.T) {
	needTools(t, "terraform")
	// With the project mirror off, the state file is not saved as a file,
	// as with a remote backend, so the state push alone puts it back. With
	// it on, the file comes back first and the push has nothing to do.
	t.Run("state push", func(t *testing.T) {
		t.Setenv("OOPS_NO_MIRROR", "1")
		testTerraformDestroy(t, true)
	})
	t.Run("with the files", func(t *testing.T) { testTerraformDestroy(t, false) })
}

func testTerraformDestroy(t *testing.T, pushed bool) {
	e := newEnv(t)
	os.WriteFile(filepath.Join(e.work, "main.tf"), []byte(tfConfig), 0o644)
	e.mustTool("terraform", "init", "-input=false", "-no-color")
	e.mustTool("terraform", "apply", "-auto-approve", "-input=false", "-no-color")
	before, serial := tfStatePull(t, e)
	if res, _ := before["resources"].([]any); len(res) != 2 {
		t.Fatalf("expected 2 resources, got %v", before["resources"])
	}

	msg := e.hookThenRun("terraform destroy -auto-approve -input=false -no-color")
	if !strings.Contains(msg, "saved snapshot") {
		t.Fatalf("no snapshot: %s", msg)
	}
	if gone, _ := tfStatePull(t, e); len(gone["resources"].([]any)) != 0 {
		t.Fatal("destroy left resources in the state")
	}
	out := e.mustOops("-y")
	t.Logf("oops -y:\n%s", out)
	if !strings.Contains(out, "terraform apply") || !strings.Contains(out, "not saved") {
		t.Errorf("undo should say resources must be recreated and data is not saved: %s", out)
	}
	after, newSerial := tfStatePull(t, e)
	if !reflect.DeepEqual(before, after) {
		a, _ := json.MarshalIndent(before, "", " ")
		b, _ := json.MarshalIndent(after, "", " ")
		t.Fatalf("state differs:\nbefore %s\nafter %s", a, b)
	}
	if pushed && newSerial <= serial {
		t.Errorf("serial %v should be newer than %v", newSerial, serial)
	}
	// terraform_data lives only in state, so the restored state is complete:
	// a plan has nothing to do.
	if out, err := e.tool("", "terraform", "plan", "-detailed-exitcode", "-input=false", "-no-color"); err != nil {
		t.Fatalf("plan after undo is not clean: %v\n%s", err, out)
	}
	e.logRecords()
}

func TestTerraformApplyUndo(t *testing.T) {
	needTools(t, "terraform")
	// Without the project mirror the state file is not saved as a file, as
	// with a remote backend, so only the state push puts it back.
	t.Setenv("OOPS_NO_MIRROR", "1")
	e := newEnv(t)
	os.WriteFile(filepath.Join(e.work, "main.tf"), []byte(tfConfig), 0o644)
	e.mustTool("terraform", "init", "-input=false", "-no-color")
	e.mustTool("terraform", "apply", "-auto-approve", "-input=false", "-no-color")
	before, _ := tfStatePull(t, e)

	os.WriteFile(filepath.Join(e.work, "main.tf"), []byte(strings.Replace(tfConfig, "web-1", "web-2", 1)+
		"resource \"terraform_data\" \"extra\" {\n  input = 1\n}\n"), 0o644)
	e.hookThenRun("terraform -chdir=. apply -auto-approve -input=false -no-color")
	out := e.mustOops("-y")
	t.Logf("oops -y:\n%s", out)
	after, _ := tfStatePull(t, e)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("state after undoing the apply differs from before it")
	}
	if !strings.Contains(out, "terraform_data.extra") {
		t.Errorf("undo should name the resource the apply created: %s", out)
	}
}
