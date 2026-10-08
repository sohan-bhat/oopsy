package hooks

import (
	"fmt"
	"strings"

	"github.com/sohan-bhat/oopsy/internal/classify"
	"github.com/sohan-bhat/oopsy/internal/preload"
)

// ShellInit returns the hook code that `eval "$(oops shell-init zsh)"` loads.
// Each hook does a regex prefilter in the shell itself, so harmless commands
// never start a process.
func ShellInit(shell, bin string) (string, error) {
	s, err := shellInit(shell, bin)
	if err != nil {
		return "", err
	}
	return s + preloadLines(shell), nil
}

// preloadLines exports the Linux preload library (see internal/preload) for
// programs started from this shell.
func preloadLines(shell string) string {
	lib := preload.Install()
	if lib == "" {
		return ""
	}
	q := shQuote(lib)
	switch shell {
	case "zsh", "bash":
		return fmt.Sprintf(`case ":${LD_PRELOAD:-}:" in *:%s:*) ;; *) export LD_PRELOAD=%s"${LD_PRELOAD:+:$LD_PRELOAD}" ;; esac
`, lib, q)
	case "fish":
		return fmt.Sprintf(`if not string match -q -- '*%s*' "$LD_PRELOAD"
    if test -n "$LD_PRELOAD"; set -gx LD_PRELOAD %s":$LD_PRELOAD"; else; set -gx LD_PRELOAD %s; end
end
`, lib, q, q)
	}
	return ""
}

func shellInit(shell, bin string) (string, error) {
	re := classify.ReadOnlyPattern
	q := shQuote(bin)
	switch shell {
	case "zsh":
		return fmt.Sprintf(`# Oopsy: snapshot before commands that can change files. Undo with: oops
if [[ -z "${__OOPSY_LOADED:-}" ]]; then
  typeset -g __OOPSY_LOADED=1
  typeset -g __OOPSY_RO=%s
  typeset -g __oopsy_pending=
  __oopsy_preexec() {
    local cmd="${3:-$1}"
    [[ "$cmd" =~ $__OOPSY_RO ]] && return 0
    __oopsy_pending=1
    OOPSY_SESSION=$$ %s hook zsh -- "$cmd"
  }
  __oopsy_precmd() {
    [[ -n "$__oopsy_pending" ]] || return 0
    __oopsy_pending=
    ( OOPSY_SESSION=$$ OOPSY_POST_QUIET=1 %s hook zsh-post </dev/null >/dev/null 2>&1 & ) 2>/dev/null
  }
  autoload -Uz add-zsh-hook
  add-zsh-hook preexec __oopsy_preexec
  add-zsh-hook precmd __oopsy_precmd
  export OOPSY_SHELL_HOOK=zsh
fi
`, shQuote(re), q, q), nil
	case "bash":
		return fmt.Sprintf(`# Oopsy: snapshot before commands that can change files. Undo with: oops
if [ -z "${__OOPSY_LOADED:-}" ]; then
  __OOPSY_LOADED=1
  __OOPSY_RO=%s
  __oopsy_pending=
  __oopsy_run() {
    [[ "$1" =~ $__OOPSY_RO ]] && return 0
    __oopsy_pending=1
    OOPSY_SESSION=$$ %s hook bash -- "$1"
  }
  __oopsy_post() {
    [ -n "$__oopsy_pending" ] || return 0
    __oopsy_pending=
    ( OOPSY_SESSION=$$ OOPSY_POST_QUIET=1 %s hook bash-post </dev/null >/dev/null 2>&1 & )
  }
  if [ -n "${bash_preexec_imported:-}${__bp_imported:-}" ]; then
    __oopsy_preexec() { __oopsy_run "$1"; }
    preexec_functions+=(__oopsy_preexec)
    precmd_functions+=(__oopsy_post)
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
      __oopsy_prev_trap="${__oopsy_prev_trap%%%%\' DEBUG}"
      trap "__oopsy_debug; $__oopsy_prev_trap" DEBUG
    else
      trap '__oopsy_debug' DEBUG
    fi
    PROMPT_COMMAND="__oopsy_post${PROMPT_COMMAND:+;$PROMPT_COMMAND}"
  fi
  export OOPSY_SHELL_HOOK=bash
fi
`, shQuote(re), q, q), nil
	case "fish":
		return fmt.Sprintf(`# Oopsy: snapshot before commands that can change files. Undo with: oops
if not set -q __OOPSY_LOADED
    set -g __OOPSY_LOADED 1
    set -g __oopsy_pending
    function __oopsy_preexec --on-event fish_preexec
        string match -qr -- %s $argv[1]; and return 0
        set -g __oopsy_pending 1
        OOPSY_SESSION=$fish_pid %s hook fish -- $argv[1]
    end
    function __oopsy_postexec --on-event fish_postexec
        test -n "$__oopsy_pending"; or return 0
        set -g __oopsy_pending
        OOPSY_SESSION=$fish_pid OOPSY_POST_QUIET=1 %s hook fish-post </dev/null >/dev/null 2>&1 &
        disown 2>/dev/null
    end
    set -gx OOPSY_SHELL_HOOK fish
end
`, shQuote(re), q, q), nil
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
