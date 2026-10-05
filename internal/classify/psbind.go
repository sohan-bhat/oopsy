package classify

import (
	"strings"
)

// psValue is one bound argument value.
type psValue struct {
	s       string
	ok      bool
	literal bool // from -LiteralPath: no wildcards
}

// psArgs is a command's arguments bound to parameters.
type psArgs struct {
	named      map[string][]psValue
	switches   map[string]bool
	positional [][]psValue
	blocks     []string
	parens     []string
	unknown    bool // some argument could not be resolved
}

func (a psArgs) has(name string) bool { return a.switches[name] || len(a.named[name]) > 0 }

// values returns the values bound to name, falling back to the given
// positional slot.
func (a psArgs) values(name string, pos int) []psValue {
	if v := a.named[name]; len(v) > 0 {
		return v
	}
	if pos >= 0 && pos < len(a.positional) {
		return a.positional[pos]
	}
	return nil
}

// psCommonValue lists common parameters that take a value.
var psCommonValue = map[string]bool{"erroraction": true, "ea": true, "warningaction": true, "wa": true, "informationaction": true,
	"infa": true, "errorvariable": true, "ev": true, "warningvariable": true, "wv": true, "informationvariable": true, "iv": true,
	"outvariable": true, "ov": true, "outbuffer": true, "ob": true, "pipelinevariable": true, "pv": true, "progressaction": true, "proga": true}

// bind matches arguments to a command spec. Parameter names may be
// abbreviated, as PowerShell allows. eval resolves (...) subexpressions
// that only compute a path, such as (Join-Path $env:TEMP build).
func bind(spec psSpec, toks []psTok, eval func(string) (string, bool)) psArgs {
	a := psArgs{named: map[string][]psValue{}, switches: map[string]bool{}}
	// list reads a value and any ", value" after it, starting at toks[i].
	list := func(i int, literal bool) ([]psValue, int) {
		var vals []psValue
		for {
			t := toks[i]
			v := psValue{literal: literal}
			switch t.kind {
			case tWord:
				v.s, v.ok = t.val, t.ok
			case tParen:
				v.s, v.ok = unknown, false
				if eval != nil {
					if s, ok := eval(t.val); ok {
						v.s, v.ok = s, true
					}
				}
				if !v.ok {
					a.parens = append(a.parens, t.val)
				}
			default:
				return vals, i - 1
			}
			a.unknown = a.unknown || !v.ok
			vals = append(vals, v)
			if i+2 < len(toks) && toks[i+1].kind == tOp && toks[i+1].val == "," {
				i += 2
				continue
			}
			return vals, i
		}
	}
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		switch t.kind {
		case tParam:
			name := spec.resolve(t.val)
			takes := spec.params[name] || psCommonValue[t.val]
			if t.arg != nil {
				if !takes {
					a.switches[name] = !strings.EqualFold(t.arg.val, "false")
					continue
				}
				v := psValue{s: t.arg.val, ok: t.arg.ok, literal: name == "literalpath"}
				a.unknown = a.unknown || !v.ok
				a.named[name] = append(a.named[name], v)
				continue
			}
			if !takes {
				a.switches[name] = true
				continue
			}
			if i+1 < len(toks) && (toks[i+1].kind == tWord || toks[i+1].kind == tParen) {
				vals, j := list(i+1, name == "literalpath")
				a.named[name] = append(a.named[name], vals...)
				i = j
			}
		case tWord, tParen:
			vals, j := list(i, false)
			a.positional = append(a.positional, vals)
			i = j
		case tBlock:
			a.blocks = append(a.blocks, t.val)
		}
	}
	return a
}

// psSpec describes a cmdlet's parameters: canonical lower case names mapped
// to whether they take a value, aliases, and positional order.
type psSpec struct {
	params  map[string]bool
	aliases map[string]string
}

func (s psSpec) resolve(name string) string {
	if c, ok := s.aliases[name]; ok {
		return c
	}
	if _, ok := s.params[name]; ok {
		return name
	}
	match := ""
	for p := range s.params {
		if strings.HasPrefix(p, name) {
			if match != "" {
				return name // ambiguous
			}
			match = p
		}
	}
	if match != "" {
		return match
	}
	return name
}

func spec(values, switches string, aliases ...string) psSpec {
	s := psSpec{params: map[string]bool{}, aliases: map[string]string{"lp": "literalpath", "pspath": "literalpath"}}
	for _, v := range strings.Fields(values) {
		s.params[v] = true
	}
	for _, v := range strings.Fields(switches) {
		s.params[v] = false
	}
	for _, sw := range []string{"whatif", "confirm", "verbose", "debug"} {
		s.params[sw] = false
	}
	for i := 0; i+1 < len(aliases); i += 2 {
		s.aliases[aliases[i]] = aliases[i+1]
	}
	return s
}

var (
	specRemove  = spec("path literalpath filter include exclude credential stream", "recurse force", "wi", "whatif", "cf", "confirm")
	specMove    = spec("path literalpath destination filter include exclude credential", "force passthru", "wi", "whatif")
	specCopy    = spec("path literalpath destination filter include exclude credential tosession fromsession", "recurse container force passthru", "wi", "whatif")
	specContent = spec("path literalpath value filter include exclude encoding stream delimiter credential", "force nonewline passthru asbytestream", "wi", "whatif")
	specClear   = spec("path literalpath filter include exclude stream credential", "force", "wi", "whatif")
	specOutFile = spec("filepath literalpath encoding width inputobject", "append force noclobber nonewline", "path", "filepath", "wi", "whatif")
	specTee     = spec("filepath literalpath variable inputobject encoding", "append", "path", "filepath")
	specNewItem = spec("path name itemtype value target credential", "force", "type", "itemtype", "wi", "whatif")
	specLoc     = spec("path literalpath stackname", "passthru")
	specGCI     = spec("path literalpath filter include exclude depth attributes", "recurse force name file directory hidden readonly system followsymlink", "s", "recurse", "ad", "directory", "af", "file", "h", "hidden")
	specGetItem = spec("path literalpath filter include exclude stream credential", "force")
	specJoin    = spec("path childpath additionalchildpath", "resolve")
	specGeneric = spec("", "")
)

var psAliases = map[string]string{
	"rm": "remove-item", "del": "remove-item", "erase": "remove-item", "rd": "remove-item", "rmdir": "remove-item", "ri": "remove-item",
	"mv": "move-item", "move": "move-item", "mi": "move-item",
	"cp": "copy-item", "copy": "copy-item", "cpi": "copy-item",
	"sc": "set-content", "ac": "add-content", "clc": "clear-content", "tee": "tee-object", "ni": "new-item",
	"cd": "set-location", "sl": "set-location", "chdir": "set-location", "pushd": "push-location",
	"ls": "get-childitem", "dir": "get-childitem", "gci": "get-childitem", "gi": "get-item", "rvpa": "resolve-path",
	"%": "foreach-object", "foreach": "foreach-object", "?": "where-object", "where": "where-object",
	"select": "select-object", "sort": "sort-object", "iex": "invoke-expression", "icm": "invoke-command",
}

// psName normalizes a command name: module qualified, aliased and native
// forms all map to one lower case name.
func psName(n string) string {
	n = strings.ToLower(n)
	if i := strings.LastIndex(n, `\`); i >= 0 && strings.Contains(n[:i], ".") && !strings.ContainsAny(n[:i], ":/") {
		n = n[i+1:] // Microsoft.PowerShell.Management\Remove-Item
	}
	if a, ok := psAliases[n]; ok {
		return a
	}
	base := n
	if strings.ContainsAny(base, `\/`) {
		base = base[strings.LastIndexAny(base, `\/`)+1:]
	}
	for _, ext := range []string{".exe", ".cmd", ".bat", ".com"} {
		base = strings.TrimSuffix(base, ext)
	}
	return base
}
