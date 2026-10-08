package remote

import (
	"reflect"
	"testing"
)

func TestDockerParse(t *testing.T) {
	d, ok := parseDocker(argv("volume rm -f data cache"))
	if !ok || !reflect.DeepEqual(d.Names, []string{"data", "cache"}) {
		t.Errorf("rm: %+v %v", d, ok)
	}
	d, ok = parseDocker(argv("--context remote volume remove data"))
	if !ok || !reflect.DeepEqual(d.Globals, []string{"--context", "remote"}) || d.Names[0] != "data" {
		t.Errorf("globals: %+v %v", d, ok)
	}
	d, ok = parseDocker(argv("-H=tcp://h:2375 volume rm data"))
	if !ok || !reflect.DeepEqual(d.Globals, []string{"-H=tcp://h:2375"}) {
		t.Errorf("host: %+v %v", d, ok)
	}
	d, ok = parseDocker(argv("volume prune -a -f --filter label=env=dev"))
	if !ok || !d.Prune || !d.All || !reflect.DeepEqual(d.Filters, []string{"label=env=dev"}) {
		t.Errorf("prune: %+v %v", d, ok)
	}
	for _, line := range []string{"volume ls", "rm container", "volume rm", "run -v data:/x alpine", "volume inspect data"} {
		if _, ok := parseDocker(argv(line)); ok {
			t.Errorf("docker %s should save nothing", line)
		}
	}
}
