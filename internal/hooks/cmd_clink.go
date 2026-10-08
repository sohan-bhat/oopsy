package hooks

import (
	"fmt"
	"sort"
	"strings"
)

// cmdReadOnly are cmd commands that never change files. Clink's Lua skips
// lines made of one of them with no redirection, chaining or pipes, so they
// never start a process. Every other line goes to oops, which applies the
// full read only check before doing any work.
var cmdReadOnly = []string{
	"cd", "chdir", "cls", "dir", "echo", "exit", "findstr", "help", "hostname", "more",
	"oops", "oops-db", "path", "popd", "pushd", "prompt", "title", "tree", "type",
	"ver", "vol", "where", "whoami",
}

// clinkScript is the Lua script Clink loads in every cmd.exe it injects
// into. onendedit gets each line after Enter and before cmd runs it, so the
// hook runs synchronously and the snapshot exists before the command does
// anything. The after step runs in the background when the next prompt
// starts. OOPSY_CLINK tells the doskey wrapper the hook already ran.
func clinkScript(bin string) string {
	ro := append([]string(nil), cmdReadOnly...)
	sort.Strings(ro)
	var set strings.Builder
	for _, w := range ro {
		fmt.Fprintf(&set, "[%q]=true, ", w)
	}
	q := luaLong(cmdQuote(bin))
	return `-- Oopsy: snapshot before commands that can change files. Undo with: oops
-- Added by oops init. Remove with: oops uninstall
local bin = ` + q + `
local readonly = { ` + strings.TrimSuffix(set.String(), ", ") + ` }
local session = "clink" .. tostring(os.getpid())
local pending = false
local started = false

local function harmless(line)
  if line:find("[&|<>()%^%%!]") then return false end
  local word = line:match("^%s*@?([^%s/]+)")
  if not word then return true end
  return readonly[word:lower()] == true
end

clink.onbeginedit(function()
  if not started then
    started = true
    os.setenv("OOPSY_CLINK", "1")
    os.setenv("OOPSY_SHELL_HOOK", "cmd (Clink)")
  end
  if not pending then return end
  pending = false
  os.setenv("OOPSY_SESSION", session)
  os.setenv("OOPSY_POST_QUIET", "1")
  os.execute('start "" /b ' .. bin .. ' hook cmd-post >nul 2>&1')
  os.setenv("OOPSY_POST_QUIET", "")
end)

clink.onendedit(function(line)
  if harmless(line) then return end
  pending = true
  os.setenv("OOPSY_LINE", line)
  os.setenv("OOPSY_CWD", os.getcwd())
  os.setenv("OOPSY_SESSION", session)
  os.execute(bin .. " hook cmd")
  os.setenv("OOPSY_LINE", "")
  os.setenv("OOPSY_CWD", "")
end)
`
}

// luaLong makes a Lua long string, which takes backslashes literally.
func luaLong(s string) string {
	eq := ""
	for strings.Contains(s, "]"+eq+"]") {
		eq += "="
	}
	return "[" + eq + "[" + s + "]" + eq + "]"
}
