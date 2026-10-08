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
	return fmt.Sprintf(`# Oopsy: snapshot before commands that can change files. Undo with: oops
if (-not $global:__OopsyLoaded) {
  $global:__OopsyLoaded = $true
  $global:__OopsyBin = %s
  $global:__OopsyRo = %s
  $global:__OopsyPending = $false
  function global:__OopsyCheck([string]$Line) {
    if ([string]::IsNullOrWhiteSpace($Line) -or $Line -match $global:__OopsyRo) { return }
    $global:__OopsyPending = $true
    $exit = $global:LASTEXITCODE
    $env:OOPSY_LINE = $Line
    $env:OOPSY_CWD = $ExecutionContext.SessionState.Path.CurrentFileSystemLocation.ProviderPath
    $env:OOPSY_SESSION = "$PID"
    try { & $global:__OopsyBin hook powershell } catch { }
    finally {
      Remove-Item Env:OOPSY_LINE, Env:OOPSY_CWD -ErrorAction SilentlyContinue
      $global:LASTEXITCODE = $exit
    }
  }
  function global:__OopsyPost {
    if (-not $global:__OopsyPending) { return }
    $global:__OopsyPending = $false
    try {
      $psi = New-Object System.Diagnostics.ProcessStartInfo
      $psi.FileName = (Get-Command $global:__OopsyBin -CommandType Application -ErrorAction Stop | Select-Object -First 1).Source
      $psi.Arguments = 'hook powershell-post'
      $psi.UseShellExecute = $false
      $psi.CreateNoWindow = $true
      $psi.EnvironmentVariables['OOPSY_SESSION'] = "$PID"
      $psi.EnvironmentVariables['OOPSY_POST_QUIET'] = '1'
      [void][System.Diagnostics.Process]::Start($psi)
    } catch { }
  }
  if (Test-Path Function:\PSConsoleHostReadLine) {
    $global:__OopsyReadLine = ${function:PSConsoleHostReadLine}
    function global:PSConsoleHostReadLine {
      $line = & $global:__OopsyReadLine
      __OopsyCheck $line
      $line
    }
    $global:__OopsyPrompt = ${function:prompt}
    function global:prompt {
      __OopsyPost
      & $global:__OopsyPrompt
    }
    $env:OOPSY_SHELL_HOOK = 'powershell'
  }
}
`, psQuote(bin), psQuote(classify.ReadOnlyPatternWindows))
}

// psQuote makes a PowerShell single quoted string.
func psQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
