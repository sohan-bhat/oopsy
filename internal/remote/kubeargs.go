package remote

import "strings"

// kubeGlobalValue are kubectl's global flags that take a value. They are
// passed on to every kubectl call Oopsy makes, so it talks to the same
// cluster as the user's command.
var kubeGlobalValue = map[string]bool{
	"--context": true, "--kubeconfig": true, "--cluster": true, "--user": true,
	"--server": true, "-s": true, "--token": true, "--as": true, "--as-group": true,
	"--as-uid": true, "--certificate-authority": true, "--client-certificate": true,
	"--client-key": true, "--tls-server-name": true, "--request-timeout": true,
	"--cache-dir": true, "--username": true, "--password": true, "--profile": true,
	"--profile-output": true, "-v": true, "--v": true, "--vmodule": true,
}

var kubeGlobalBool = map[string]bool{"--insecure-skip-tls-verify": true, "--match-server-version": true, "--warnings-as-errors": true}

// kubeValue are subcommand flags of delete, apply and replace that take a
// value. Flags with optional values (--dry-run, --cascade, --validate) only
// take one after an equals sign, as in kubectl itself.
var kubeValue = map[string]bool{
	"-f": true, "--filename": true, "-k": true, "--kustomize": true, "-l": true, "--selector": true,
	"--field-selector": true, "--grace-period": true, "--timeout": true, "-o": true, "--output": true,
	"--raw": true, "--field-manager": true, "--prune-allowlist": true, "--prune-whitelist": true,
	"--template": true, "--subresource": true, "-n": true, "--namespace": true,
}

// kubeSpec is a parsed kubectl delete, apply or replace.
type kubeSpec struct {
	Sub        string   // delete, apply or replace
	Globals    []string // connection flags, normalized to --flag=value
	Namespace  string
	Context    string
	Kubeconfig string
	Operands   []string // TYPE NAME..., TYPE/NAME...
	Files      []string
	Kustomize  string
	Selector   string
	FieldSel   string
	All        bool
	AllNS      bool
	Recursive  bool
	Prune      bool
	Stdin      bool // -f - reads objects from standard input
	DryRun     bool
}

func parseKube(args []string) (kubeSpec, bool) {
	var k kubeSpec
	var ops []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			ops = append(ops, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			ops = append(ops, a)
			continue
		}
		name, val, hasVal := strings.Cut(a, "=")
		if !strings.HasPrefix(a, "--") && len(name) > 2 {
			// A short flag with its value attached (-nprod) or a cluster (-Rf dir).
			cluster := name[1:]
			for j := 0; j < len(cluster); j++ {
				f := "-" + cluster[j:j+1]
				if kubeValue[f] || kubeGlobalValue[f] {
					rest := cluster[j+1:]
					if hasVal {
						rest += "=" + val
					}
					name, val, hasVal = f, rest, true
					if rest == "" {
						hasVal = false
					}
					break
				}
				k.flag(f, "")
				name = ""
			}
			if name == "" {
				continue
			}
		}
		takesValue := kubeValue[name] || kubeGlobalValue[name]
		if takesValue && !hasVal {
			if i+1 >= len(args) {
				break
			}
			i++
			val = args[i]
		}
		if !takesValue && !hasVal {
			val = ""
		}
		if kubeGlobalValue[name] {
			long := name
			if long == "-s" {
				long = "--server"
			}
			if name != "-v" && name != "--v" && name != "--vmodule" && !strings.HasPrefix(name, "--profile") {
				k.Globals = append(k.Globals, long+"="+val)
			}
			switch long {
			case "--context":
				k.Context = val
			case "--kubeconfig":
				k.Kubeconfig = val
			}
			continue
		}
		if kubeGlobalBool[name] {
			k.Globals = append(k.Globals, a)
			continue
		}
		k.flag(name, val)
	}
	if len(ops) == 0 {
		return k, false
	}
	k.Sub, k.Operands = ops[0], ops[1:]
	switch k.Sub {
	case "delete", "apply", "replace":
	default:
		return k, false
	}
	if k.DryRun {
		return k, false
	}
	return k, true
}

func (k *kubeSpec) flag(name, val string) {
	switch name {
	case "-n", "--namespace":
		k.Namespace = val
	case "-f", "--filename":
		if val == "-" {
			k.Stdin = true
		} else {
			k.Files = append(k.Files, val)
		}
	case "-k", "--kustomize":
		k.Kustomize = val
	case "-l", "--selector":
		k.Selector = val
	case "--field-selector":
		k.FieldSel = val
	case "--all":
		k.All = val != "false"
	case "-A", "--all-namespaces":
		k.AllNS = val != "false"
	case "-R", "--recursive":
		k.Recursive = val != "false"
	case "--prune":
		k.Prune = val != "false"
	case "--dry-run":
		k.DryRun = val != "none" && val != "false"
	}
}

// getArgs is the kubectl get that reads exactly the objects the command
// will change, in JSON so restoring needs no YAML library.
func (k kubeSpec) getArgs(cwd string) ([]string, bool) {
	args := []string{"get"}
	switch {
	case len(k.Files) > 0 || k.Kustomize != "":
		for _, f := range k.Files {
			if !strings.Contains(f, "://") {
				f = absIn(cwd, f)
			}
			args = append(args, "--filename="+f)
		}
		if k.Kustomize != "" {
			args = append(args, "--kustomize="+absIn(cwd, k.Kustomize))
		}
		if k.Recursive {
			args = append(args, "--recursive")
		}
	case len(k.Operands) > 0:
		if k.Sub != "delete" {
			return nil, false
		}
		named := len(k.Operands) > 1 || strings.Contains(k.Operands[0], "/")
		if !named && !k.All && k.Selector == "" && k.FieldSel == "" {
			return nil, false // kubectl refuses a bare type
		}
		args = append(args, k.Operands...)
	default:
		return nil, false
	}
	if k.Selector != "" {
		args = append(args, "--selector="+k.Selector)
	}
	if k.FieldSel != "" {
		args = append(args, "--field-selector="+k.FieldSel)
	}
	if k.AllNS {
		args = append(args, "--all-namespaces")
	} else if k.Namespace != "" {
		args = append(args, "--namespace="+k.Namespace)
	}
	return append(args, "--output=json", "--ignore-not-found"), true
}

// namespaces lists namespaces the command deletes by name, whose contents
// go with them.
func (k kubeSpec) namespaces() []string {
	if k.Sub != "delete" || len(k.Operands) == 0 {
		return nil
	}
	isNS := func(t string) bool { return t == "ns" || t == "namespace" || t == "namespaces" }
	var out []string
	if t, _, _ := strings.Cut(k.Operands[0], "/"); !strings.Contains(k.Operands[0], "/") && isNS(t) {
		return k.Operands[1:]
	}
	for _, op := range k.Operands {
		if t, n, ok := strings.Cut(op, "/"); ok && isNS(t) {
			out = append(out, n)
		}
	}
	return out
}

func (k kubeSpec) describe() string {
	what := strings.Join(k.Operands, " ")
	if len(k.Files) > 0 {
		what = "the objects in " + strings.Join(k.Files, ", ")
	}
	if k.Kustomize != "" {
		what = "the objects in kustomization " + k.Kustomize
	}
	if k.Selector != "" {
		what += " matching " + k.Selector
	}
	where := ""
	if k.AllNS {
		where = " in every namespace"
	} else if k.Namespace != "" {
		where = " in namespace " + k.Namespace
	}
	if k.Context != "" {
		where += " on context " + k.Context
	}
	s := "Kubernetes objects " + what + where + " as JSON"
	if len(k.namespaces()) > 0 {
		s += ", with the common objects inside the namespaces"
	}
	return s
}
