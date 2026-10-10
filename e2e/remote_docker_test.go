package e2e

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// needDocker skips unless a Docker daemon answers.
func needDocker(t *testing.T, e *env) {
	t.Helper()
	needTools(t, "docker")
	out, err := e.tool("", "docker", "version", "--format", "{{.Server.Os}}")
	if err != nil {
		if os.Getenv("OOPS_TEST_REMOTE") == "1" {
			t.Fatalf("docker daemon not reachable: %v\n%s", err, out)
		}
		t.Skip("docker daemon not reachable")
	}
	if strings.TrimSpace(out) != "linux" {
		t.Skip("the docker daemon runs " + strings.TrimSpace(out) + " containers, and the test uses a Linux image")
	}
}

// volumePrint lists every entry of a volume with its type, mode, owner,
// size, link target and content hash.
func volumePrint(t *testing.T, e *env, vol string) string {
	t.Helper()
	return e.mustTool("docker", "run", "--rm", "-v", vol+":/v:ro", "alpine:3.20", "sh", "-c",
		`cd /v && find . | sort | while read -r f; do
			if [ -L "$f" ]; then echo "$f link $(readlink "$f")"
			elif [ -f "$f" ]; then echo "$f $(stat -c '%a %u %g %s %Y' "$f") $(sha256sum < "$f")"
			else echo "$f dir $(stat -c '%a %u %g' "$f")"; fi
		done`)
}

func TestDockerVolumeRmRestores(t *testing.T) {
	e := newEnv(t)
	needDocker(t, e)
	vol := randName("oopsy-vol")
	t.Cleanup(func() {
		e.tool("", "docker", "volume", "rm", "-f", vol)
		e.tool("", "docker", "volume", "rm", "-f", vol+"-oops-1")
	})
	e.mustTool("docker", "volume", "create", "--label", "app=db", "--label", "env=test", vol)
	e.mustTool("docker", "run", "--rm", "-v", vol+":/v", "alpine:3.20", "sh", "-c", `set -e
		mkdir -p /v/data/nested /v/empty
		head -c 3000000 /dev/urandom > /v/data/blob.bin
		echo 'select 1;' > /v/data/nested/q.sql
		printf 'line\n%.0s' $(seq 1 20000) > /v/data/log.txt
		ln -s data/nested/q.sql /v/link
		chown -R 999:999 /v/data && chmod 640 /v/data/nested/q.sql && chmod 700 /v/empty
		touch -d '2021-03-04 05:06:07' /v/data/log.txt`)
	before := volumePrint(t, e, vol)

	msg := e.hookThenRun("docker volume rm " + vol)
	if !strings.Contains(msg, "saved snapshot") {
		t.Fatalf("no snapshot: %s", msg)
	}
	if _, err := e.tool("", "docker", "volume", "inspect", vol); err == nil {
		t.Fatal("volume still exists")
	}
	out := e.mustOops("-y")
	t.Logf("oops -y:\n%s", out)
	if after := volumePrint(t, e, vol); after != before {
		t.Fatalf("volume differs:\nbefore\n%s\nafter\n%s", before, after)
	}
	var info []struct {
		Labels map[string]string `json:"Labels"`
	}
	json.Unmarshal([]byte(e.mustTool("docker", "volume", "inspect", vol)), &info)
	if len(info) != 1 || info[0].Labels["app"] != "db" || info[0].Labels["env"] != "test" {
		t.Errorf("labels not restored: %+v", info)
	}
	// Undo again while the volume exists: the files go into a new volume.
	out = e.mustOops("undo", "1", "-y")
	if !strings.Contains(out, vol+"-oops-1") {
		t.Errorf("second undo should not overwrite the volume: %s", out)
	}
	e.logRecords()
}

func TestDockerVolumeInUseIsNotSaved(t *testing.T) {
	e := newEnv(t)
	needDocker(t, e)
	vol := randName("oopsy-busy")
	ctr := randName("oopsy-ctr")
	t.Cleanup(func() {
		e.tool("", "docker", "rm", "-f", ctr)
		e.tool("", "docker", "volume", "rm", "-f", vol)
	})
	e.mustTool("docker", "volume", "create", vol)
	e.mustTool("docker", "create", "--name", ctr, "-v", vol+":/v", "alpine:3.20", "true")
	_, msg, _ := e.oopsCmd("", "hook", "zsh", "--", "docker volume rm "+vol)
	if !strings.Contains(msg, "used by a container") {
		t.Errorf("expected an in use reason, got %q", msg)
	}
}
