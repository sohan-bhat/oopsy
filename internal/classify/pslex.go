package classify

import (
	"strings"
	"unicode"
)

// PowerShell lexing. This is not a full PowerShell parser: it understands
// enough of argument mode (quotes, backtick escapes, variables, parameters,
// arrays, script blocks, subexpressions and redirections) to find every
// path a command line is about to destroy, and it errs toward saving more.

const (
	tWord  = iota // an argument or command name
	tParam        // -Name or -Name:value
	tOp           // ; | && || & , and newlines (as ;)
	tRedir        // > >> 2> *> with the target in val ("" for 2>&1)
	tBlock        // { ... }, raw content in val
	tParen        // ( ... ), $( ... ), @( ... ), raw content in val
)

// unknown marks a value we cannot know before the command runs. item marks
// the current pipeline object ($_ or $PSItem).
const (
	unknown = "\x00unknown"
	psItem  = "\x00item"
)

type psTok struct {
	kind        int
	val         string // resolved text, operator, parameter name or block content
	raw         string // source text of a word, for assignments
	ok          bool   // the word resolved fully
	quoted      bool   // the word was (partly) quoted
	arg         *psTok // value given as -Name:value
	stream      string // redirection source stream (1, 2, *, ...)
	appendRedir bool
}

type psLexer struct {
	src  []rune
	i    int
	vars func(name string) (string, bool)
}

func isPSSpace(r rune) bool { return r == ' ' || r == '\t' || r == '\f' || r == '\v' || r == 0xA0 }

func isDash(r rune) bool { return r == '-' || r == '–' || r == '—' || r == '―' }

func (l *psLexer) peek(off int) rune {
	if l.i+off < len(l.src) {
		return l.src[l.i+off]
	}
	return 0
}

// next returns the next token, or false at the end of input.
func (l *psLexer) next() (psTok, bool) {
	for l.i < len(l.src) {
		r := l.src[l.i]
		switch {
		case isPSSpace(r):
			l.i++
		case r == '`' && (l.peek(1) == '\n' || l.peek(1) == '\r'):
			l.i += 2 // line continuation
			if l.peek(0) == '\n' {
				l.i++
			}
		case r == '#':
			for l.i < len(l.src) && l.src[l.i] != '\n' {
				l.i++
			}
		case r == '<' && l.peek(1) == '#':
			end := strings.Index(string(l.src[l.i:]), "#>")
			if end < 0 {
				l.i = len(l.src)
			} else {
				l.i += len([]rune(string(l.src[l.i:])[:end])) + 2
			}
		default:
			return l.token(), true
		}
	}
	return psTok{}, false
}

func (l *psLexer) token() psTok {
	r := l.src[l.i]
	switch {
	case r == '\n' || r == '\r' || r == ';':
		l.i++
		return psTok{kind: tOp, val: ";"}
	case r == '|':
		l.i++
		if l.peek(0) == '|' {
			l.i++
			return psTok{kind: tOp, val: "||"}
		}
		return psTok{kind: tOp, val: "|"}
	case r == '&':
		l.i++
		if l.peek(0) == '&' {
			l.i++
			return psTok{kind: tOp, val: "&&"}
		}
		return psTok{kind: tOp, val: "&"}
	case r == ',':
		l.i++
		return psTok{kind: tOp, val: ","}
	case r == '{':
		return psTok{kind: tBlock, val: l.balanced('{', '}')}
	case r == '(':
		return psTok{kind: tParen, val: l.balanced('(', ')')}
	case (r == '$' || r == '@') && l.peek(1) == '(':
		l.i++
		return psTok{kind: tParen, val: l.balanced('(', ')')}
	case r == '@' && l.peek(1) == '{':
		l.i++
		return psTok{kind: tBlock, val: l.balanced('{', '}')}
	case r == ')' || r == '}':
		l.i++
		return psTok{kind: tOp, val: ";"}
	}
	if t, ok := l.redirect(); ok {
		return t
	}
	if isDash(r) && (unicode.IsLetter(l.peek(1)) || l.peek(1) == '_' || l.peek(1) == '?') {
		return l.param()
	}
	return l.word()
}

// redirect reads > >> 2> 2>> *> 2>&1 and their target.
func (l *psLexer) redirect() (psTok, bool) {
	start := l.i
	stream := "1"
	if r := l.src[l.i]; (r >= '1' && r <= '6' || r == '*') && l.peek(1) == '>' {
		stream = string(r)
		l.i++
	}
	if l.peek(0) != '>' {
		l.i = start
		return psTok{}, false
	}
	l.i++
	t := psTok{kind: tRedir, stream: stream}
	if l.peek(0) == '>' {
		t.appendRedir = true
		l.i++
	}
	if l.peek(0) == '&' {
		l.i += 2 // 2>&1 merges streams, no file
		return t, true
	}
	for l.i < len(l.src) && isPSSpace(l.src[l.i]) {
		l.i++
	}
	if l.i < len(l.src) {
		w := l.word()
		t.val, t.ok = w.val, w.ok
		if strings.EqualFold(w.raw, "$null") {
			t.val = ""
		}
	}
	return t, true
}

func (l *psLexer) param() psTok {
	l.i++ // the dash
	start := l.i
	for l.i < len(l.src) && (unicode.IsLetter(l.src[l.i]) || unicode.IsDigit(l.src[l.i]) || l.src[l.i] == '_' || l.src[l.i] == '?') {
		l.i++
	}
	t := psTok{kind: tParam, val: strings.ToLower(string(l.src[start:l.i])), raw: "-" + string(l.src[start:l.i]), ok: true}
	if l.peek(0) == ':' {
		l.i++
		if l.i < len(l.src) && !isPSSpace(l.src[l.i]) {
			if v, ok := l.next(); ok {
				t.arg = &v
			}
		}
	}
	return t
}

// balanced reads from an opening bracket to its match, skipping quotes, and
// returns the content between them.
func (l *psLexer) balanced(open, close rune) string {
	depth := 0
	start := l.i + 1
	for l.i < len(l.src) {
		r := l.src[l.i]
		switch {
		case r == '`':
			l.i++
		case r == '\'' || r == '"':
			l.skipQuoted(r)
			continue
		case r == open:
			depth++
		case r == close:
			depth--
			if depth == 0 {
				l.i++
				return string(l.src[start : l.i-1])
			}
		}
		l.i++
	}
	return string(l.src[min(start, len(l.src)):])
}

func (l *psLexer) skipQuoted(q rune) {
	l.i++
	for l.i < len(l.src) {
		r := l.src[l.i]
		if r == '`' && q == '"' {
			l.i += 2
			continue
		}
		if r == q {
			if l.peek(1) == q {
				l.i += 2
				continue
			}
			l.i++
			return
		}
		l.i++
	}
}

// word reads one argument: barewords, quoted strings and variables joined
// together, until whitespace or an operator.
func (l *psLexer) word() psTok {
	var b strings.Builder
	t := psTok{kind: tWord, ok: true}
	start := l.i
	for l.i < len(l.src) {
		r := l.src[l.i]
		if isPSSpace(r) || r == '\n' || r == '\r' || r == ';' || r == '|' || r == '&' || r == ',' || r == ')' || r == '}' {
			break
		}
		if (r == '>' || (r >= '1' && r <= '6' || r == '*') && l.peek(1) == '>') && l.i > start {
			break
		}
		switch {
		case r == '`':
			if l.i+1 < len(l.src) {
				b.WriteRune(l.src[l.i+1])
			}
			l.i += 2
		case r == '\'':
			t.quoted = true
			b.WriteString(l.single())
		case r == '"':
			t.quoted = true
			s, ok := l.double()
			b.WriteString(s)
			t.ok = t.ok && ok
		case r == '@' && (l.peek(1) == '"' || l.peek(1) == '\''):
			t.quoted = true
			s, ok := l.hereString()
			b.WriteString(s)
			t.ok = t.ok && ok
		case r == '$':
			s, ok := l.variable(true)
			b.WriteString(s)
			t.ok = t.ok && ok
		case (r == '(' || r == '{') && l.i > start:
			// A subexpression glued to a word, such as foo$(bar): unknowable.
			l.balanced(r, map[rune]rune{'(': ')', '{': '}'}[r])
			t.ok = false
		default:
			b.WriteRune(r)
			l.i++
		}
	}
	t.raw = string(l.src[start:l.i])
	t.val = b.String()
	if !t.ok {
		t.val = unknown
	}
	return t
}

func (l *psLexer) single() string {
	var b strings.Builder
	l.i++
	for l.i < len(l.src) {
		r := l.src[l.i]
		if r == '\'' || r == '‘' || r == '’' {
			if l.peek(1) == r {
				b.WriteRune(r)
				l.i += 2
				continue
			}
			l.i++
			break
		}
		b.WriteRune(r)
		l.i++
	}
	return b.String()
}

var psEscapes = map[rune]string{'0': "\x00", 'a': "\a", 'b': "\b", 'e': "\x1b", 'f': "\f", 'n': "\n", 'r': "\r", 't': "\t", 'v': "\v"}

func (l *psLexer) double() (string, bool) {
	var b strings.Builder
	ok := true
	l.i++
	for l.i < len(l.src) {
		r := l.src[l.i]
		switch {
		case r == '`' && l.i+1 < len(l.src):
			n := l.src[l.i+1]
			if e, found := psEscapes[n]; found {
				b.WriteString(e)
			} else {
				b.WriteRune(n)
			}
			l.i += 2
		case r == '"' || r == '“' || r == '”':
			if l.peek(1) == '"' {
				b.WriteRune('"')
				l.i += 2
				continue
			}
			l.i++
			return b.String(), ok
		case r == '$':
			s, vok := l.variable(false)
			b.WriteString(s)
			ok = ok && vok
		default:
			b.WriteRune(r)
			l.i++
		}
	}
	return b.String(), ok
}

func (l *psLexer) hereString() (string, bool) {
	q := l.src[l.i+1]
	rest := string(l.src[l.i+2:])
	end := strings.Index(rest, "\n"+string(q)+"@")
	body := rest
	if end >= 0 {
		body = rest[:end]
		l.i += 2 + len([]rune(rest[:end])) + 3
	} else {
		l.i = len(l.src)
	}
	body = strings.TrimPrefix(strings.TrimPrefix(body, "\r"), "\n")
	if q == '\'' {
		return body, true
	}
	sub := &psLexer{src: []rune("\"" + strings.ReplaceAll(body, "\"", "`\"") + "\""), vars: l.vars}
	return sub.double()
}

// variable reads $name, ${name}, $env:NAME, $_ and member access such as
// $_.FullName or $pwd.Path. Member access is only followed in barewords.
func (l *psLexer) variable(bare bool) (string, bool) {
	l.i++ // $
	if l.peek(0) == '(' {
		l.balanced('(', ')')
		return unknown, false
	}
	var name string
	if l.peek(0) == '{' {
		name = l.balanced('{', '}')
	} else {
		start := l.i
		for l.i < len(l.src) {
			r := l.src[l.i]
			if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '?' || (r == ':' && l.i > start && l.peek(1) != ':' && l.peek(1) != '\\' && l.peek(1) != '/') {
				l.i++
				continue
			}
			break
		}
		name = string(l.src[start:l.i])
		if name == "" {
			if l.peek(0) == '$' || l.peek(0) == '^' {
				l.i++
				return unknown, false
			}
			return "$", true
		}
	}
	member := ""
	if bare {
		for l.peek(0) == '.' && (unicode.IsLetter(l.peek(1)) || l.peek(1) == '_') {
			l.i++
			start := l.i
			for l.i < len(l.src) && (unicode.IsLetter(l.src[l.i]) || unicode.IsDigit(l.src[l.i]) || l.src[l.i] == '_') {
				l.i++
			}
			member = strings.ToLower(string(l.src[start:l.i]))
		}
	}
	lname := strings.ToLower(name)
	if lname == "_" || lname == "psitem" {
		return psItem, true
	}
	v, ok := l.vars(name)
	if !ok {
		return unknown, false
	}
	if v == psItem {
		return psItem, true // $f.FullName in foreach ($f in ...)
	}
	if member != "" {
		if (lname == "pwd" || lname == "home") && (member == "path" || member == "providerpath") {
			return v, true
		}
		return unknown, false
	}
	return v, true
}
