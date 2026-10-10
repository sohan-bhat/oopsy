package classify

import (
	"encoding/base64"
	"strings"
	"unicode/utf16"
)

// native handles programs run from PowerShell: git, cmd /c, nested shells.
func (ps *psState) native(name string, rest []psTok) {
	var argv []string
	for _, t := range rest {
		switch t.kind {
		case tWord:
			argv = append(argv, t.val)
		case tParam:
			a := t.raw // native programs get options as typed, case and all
			if t.arg != nil {
				a += ":" + t.arg.val
			}
			argv = append(argv, a)
		}
	}
	switch name {
	case "git":
		bs := newState(ps.cwd, ps.rawEnv)
		hGit(bs, "git", argv)
		ps.actions = append(ps.actions, bs.actions...)
	case "pwsh", "powershell":
		if script := psCommandArg(argv); script != "" {
			c := ps.child()
			c.run(script, nil)
			ps.actions = append(ps.actions, c.actions...)
		}
	case "bash", "sh", "wsl":
		bs := newState(ps.cwd, ps.rawEnv)
		if name == "wsl" {
			bs.dispatch(argv, false)
		} else {
			bs.shellC(argv)
		}
		ps.actions = append(ps.actions, bs.actions...)
	}
}

// psCommandArg extracts the script from pwsh -Command ... or
// -EncodedCommand base64.
func psCommandArg(argv []string) string {
	for i, a := range argv {
		l := strings.ToLower(a)
		if !strings.HasPrefix(l, "-") && !strings.HasPrefix(l, "/") {
			continue
		}
		l = strings.TrimLeft(l, "-/")
		switch {
		case l == "e" || l == "ec" || strings.HasPrefix("encodedcommand", l) && len(l) >= 2 && l != "ex" && l != "executionpolicy":
			if strings.HasPrefix("executionpolicy", l) && len(l) > 1 && l[1] == 'x' {
				continue
			}
			if i+1 < len(argv) {
				return decodePSBase64(argv[i+1])
			}
		case l == "c" || strings.HasPrefix("command", l) && len(l) >= 2:
			return strings.Join(argv[i+1:], " ")
		}
	}
	return ""
}

func decodePSBase64(s string) string {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(b)%2 != 0 {
		return ""
	}
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = uint16(b[2*i]) | uint16(b[2*i+1])<<8
	}
	return string(utf16.Decode(u))
}
