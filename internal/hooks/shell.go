package hooks

import (
	"fmt"
	"strings"

	"github.com/sohan-bhat/oopsy/internal/classify"
)

// ShellInit returns the hook code that `eval "$(oops shell-init zsh)"` loads.
// Each hook does a regex prefilter in the shell itself, so harmless commands
// never start a process.
func ShellInit(shell, bin string) (string, error) {
	re := classify.QuickPattern
	q := shQuote(bin)
	switch shell {
	case "zsh":
		return fmt.Sprintf(`# Oopsy: snapshot destructive commands before they run. Undo with: oops
if [[ -z "${__OOPSY_LOADED:-}" ]]; then
  typeset -g __OOPSY_LOADED=1
  typeset -g __OOPSY_RE=%s
  __oopsy_preexec() {
    local cmd="${3:-$1}"
    [[ "$cmd" =~ $__OOPSY_RE ]] || return 0
    %s hook zsh -- "$cmd"
  }
  autoload -Uz add-zsh-hook
  add-zsh-hook preexec __oopsy_preexec
  export OOPSY_SHELL_HOOK=zsh
fi
`, shQuote(re), q), nil
	case "bash":
		return fmt.Sprintf(`# Oopsy: snapshot destructive commands before they run. Undo with: oops
if [ -z "${__OOPSY_LOADED:-}" ]; then
  __OOPSY_LOADED=1
  __OOPSY_RE=%s
  __oopsy_run() {
    [[ "$1" =~ $__OOPSY_RE ]] || return 0
    %s hook bash -- "$1"
  }
  if [ -n "${bash_preexec_imported:-}${__bp_imported:-}" ]; then
    __oopsy_preexec() { __oopsy_run "$1"; }
    preexec_functions+=(__oopsy_preexec)
  else
    __oopsy_debug() {
      [ -n "${COMP_LINE:-}" ] && return 0
      [ -n "${__oopsy_busy:-}" ] && return 0
      case "$BASH_COMMAND" in __oopsy*|"$PROMPT_COMMAND") return 0;; esac
      __oopsy_busy=1
      __oopsy_run "$BASH_COMMAND"
      __oopsy_busy=
      return 0
    }
    __oopsy_prev_trap="$(trap -p DEBUG)"
    if [ -n "$__oopsy_prev_trap" ]; then
      __oopsy_prev_trap="${__oopsy_prev_trap#trap -- \'}"
      __oopsy_prev_trap="${__oopsy_prev_trap%%\' DEBUG}"
      trap "__oopsy_debug; $__oopsy_prev_trap" DEBUG
    else
      trap '__oopsy_debug' DEBUG
    fi
  fi
  export OOPSY_SHELL_HOOK=bash
fi
`, shQuote(re), q), nil
	case "fish":
		return fmt.Sprintf(`# Oopsy: snapshot destructive commands before they run. Undo with: oops
if not set -q __OOPSY_LOADED
    set -g __OOPSY_LOADED 1
    function __oopsy_preexec --on-event fish_preexec
        string match -qr -- %s $argv[1]; or return 0
        %s hook fish -- $argv[1]
    end
    set -gx OOPSY_SHELL_HOOK fish
end
`, shQuote(re), q), nil
	case "powershell", "pwsh":
		return powerShellInit(bin), nil
	}
	return "", fmt.Errorf("unknown shell %q (use zsh, bash, fish or powershell)", shell)
}

func shQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/._-+") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
