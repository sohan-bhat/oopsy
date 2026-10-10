package watch

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// The scheduled task can be created from its XML, found and deleted. It is
// not run: a test must never leave a daemon behind.
func TestScheduledTask(t *testing.T) {
	t.Setenv("OOPS_HOME", t.TempDir())
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	x := taskXML(currentSID(), `C:\Program Files\oops\oops.exe`, []string{"watch", "run"})
	for _, want := range []string{"<LogonTrigger>", "<RunLevel>LeastPrivilege</RunLevel>", "<ExecutionTimeLimit>PT0S</ExecutionTimeLimit>",
		"<DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>", `--headless &quot;C:\Program Files\oops\oops.exe&quot; watch run`} {
		if !strings.Contains(x, want) {
			t.Errorf("task XML lacks %q:\n%s", want, x)
		}
	}
	name := fmt.Sprintf("OopsyWatchTest%d", os.Getpid())
	if err := createTask(name, `C:\Windows\System32\cmd.exe`, []string{"/c", "exit"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	defer deleteTask(name)
	if !taskExists(name) {
		t.Fatal("task not found after create")
	}
	if err := deleteTask(name); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if taskExists(name) {
		t.Fatal("task still there after delete")
	}
}
