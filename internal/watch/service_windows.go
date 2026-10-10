package watch

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf16"

	"golang.org/x/sys/windows"
)

// Windows: a Scheduled Task that starts at logon with limited rights, made
// from XML so it can run without a time limit and on battery (a task made
// with plain schtasks /SC ONLOGON is stopped after 72 hours and when the
// laptop unplugs). It runs the daemon through conhost --headless, so no
// window appears, and is started right away with schtasks /Run.

func taskName() string { return label("OopsyWatch") }

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")
	return r.Replace(s)
}

// quoteArg quotes one command line argument the way Windows programs read it.
func quoteArg(s string) string { return syscall.EscapeArg(s) }

// taskXML is the task definition for the current user.
func taskXML(sid, exe string, args []string) string {
	conhost := filepath.Join(os.Getenv("SystemRoot"), "System32", "conhost.exe")
	if os.Getenv("SystemRoot") == "" {
		conhost = `C:\Windows\System32\conhost.exe`
	}
	cmdArgs := []string{"--headless", quoteArg(exe)}
	for _, a := range args {
		cmdArgs = append(cmdArgs, quoteArg(a))
	}
	return `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Description>Oopsy watch: keeps the previous version of files you change, so oops can undo them.</Description>
  </RegistrationInfo>
  <Triggers>
    <LogonTrigger>
      <Enabled>true</Enabled>
      <UserId>` + xmlEscape(sid) + `</UserId>
    </LogonTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author">
      <UserId>` + xmlEscape(sid) + `</UserId>
      <LogonType>InteractiveToken</LogonType>
      <RunLevel>LeastPrivilege</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <AllowHardTerminate>true</AllowHardTerminate>
    <StartWhenAvailable>true</StartWhenAvailable>
    <IdleSettings>
      <StopOnIdleEnd>false</StopOnIdleEnd>
      <RestartOnIdle>false</RestartOnIdle>
    </IdleSettings>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>true</Enabled>
    <Hidden>true</Hidden>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <Priority>7</Priority>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>` + xmlEscape(conhost) + `</Command>
      <Arguments>` + xmlEscape(strings.Join(cmdArgs, " ")) + `</Arguments>
    </Exec>
  </Actions>
</Task>
`
}

func currentSID() string {
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return ""
	}
	return tu.User.Sid.String()
}

// writeUTF16 writes s as UTF-16 LE with a byte order mark, which is what
// schtasks /XML expects.
func writeUTF16(p, s string) error {
	u := utf16.Encode([]rune(s))
	b := make([]byte, 2+2*len(u))
	b[0], b[1] = 0xFF, 0xFE
	for i, c := range u {
		binary.LittleEndian.PutUint16(b[2+2*i:], c)
	}
	return os.WriteFile(p, b, 0o600)
}

// createTask registers the task without starting it.
func createTask(name, exe string, args []string) error {
	x := filepath.Join(Dir(), "task.xml")
	if err := writeUTF16(x, taskXML(currentSID(), exe, args)); err != nil {
		return err
	}
	defer os.Remove(x)
	return run("schtasks", "/Create", "/TN", name, "/XML", x, "/F")
}

func taskExists(name string) bool { return run("schtasks", "/Query", "/TN", name) == nil }

func deleteTask(name string) error {
	_ = run("schtasks", "/End", "/TN", name)
	return run("schtasks", "/Delete", "/TN", name, "/F")
}

func installService(exe string) (serviceInfo, string, error) {
	name := taskName()
	if err := createTask(name, exe, runArgs()); err != nil {
		return serviceInfo{}, "", err
	}
	if err := run("schtasks", "/Run", "/TN", name); err != nil {
		_ = deleteTask(name)
		return serviceInfo{}, "", err
	}
	return serviceInfo{Manager: "task", Path: name}, "", nil
}

func removeService(si *serviceInfo) error {
	if si.Manager != "task" {
		return nil
	}
	if !taskExists(si.Path) {
		return nil
	}
	return deleteTask(si.Path)
}
