package watch

import (
	"strings"
	"testing"
)

func TestUnit(t *testing.T) {
	u := unit("/home/me/my bin/oops", []string{"watch", "run", "--home", "/home/me/.oops"})
	for _, want := range []string{
		`ExecStart="/home/me/my bin/oops" watch run --home /home/me/.oops`,
		"Restart=on-failure",
		"WantedBy=default.target",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("unit lacks %q:\n%s", want, u)
		}
	}
}
