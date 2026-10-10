package hooks

import (
	"fmt"
	"strings"

	"github.com/sohan-bhat/oopsy/internal/classify"
)

// powerShellInit returns the hook `oops shell-init powershell` prints.
//
// PowerShell has no preexec event, so the hook wraps PSConsoleHostReadLine,
// the function PSReadLine uses to read each command line. The wrapper gets
// the finished line after Enter and before PowerShell runs it, which works
// with any key bindings and keeps the console output in order. A regex runs
// first in PowerShell itself, so harmless commands never start a process.
func powerShellInit(bin string) string {
	return fmt.Sprintf(`# Oopsy: snapshot destructive commands before they run. Undo with: oops
if (-not $global:__OopsyLoaded) {
  $global:__OopsyLoaded = $true
  $global:__OopsyBin = %s
  $global:__OopsyRe = %s
  function global:__OopsyCheck([string]$Line) {
    if ([string]::IsNullOrWhiteSpace($Line) -or $Line -notmatch $global:__OopsyRe) { return }
    $exit = $global:LASTEXITCODE
    $env:OOPSY_LINE = $Line
    $env:OOPSY_CWD = $ExecutionContext.SessionState.Path.CurrentFileSystemLocation.ProviderPath
    try { & $global:__OopsyBin hook powershell } catch { }
    finally {
      Remove-Item Env:OOPSY_LINE, Env:OOPSY_CWD -ErrorAction SilentlyContinue
      $global:LASTEXITCODE = $exit
    }
  }
  if (Test-Path Function:\PSConsoleHostReadLine) {
    $global:__OopsyReadLine = ${function:PSConsoleHostReadLine}
    function global:PSConsoleHostReadLine {
      $line = & $global:__OopsyReadLine
      __OopsyCheck $line
      $line
    }
    $env:OOPSY_SHELL_HOOK = 'powershell'
  }
}
`, psQuote(bin), psQuote(classify.QuickPatternWindows))
}

// psQuote makes a PowerShell single quoted string.
func psQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
