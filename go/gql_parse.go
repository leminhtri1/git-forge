package main

// A GraphQL document parser (the executable subset of the spec): operations with variable definitions,
// selection sets, aliases, arguments, directives, fragments and inline fragments, and every literal.

import (
	"fmt"
	"strconv"
	"strings"
)

type gqlLoc struct{ Line, Column int }

type gqlValue struct {
	Kind   string // Variable, Int, Float, String, Boolean, Null, Enum, List, Object
	Str    string // Variable name, String, Enum
	Num    float64
	Bool   bool
	List   []*gqlValue
	Fields []gqlArg
}

type gqlArg struct {
	Name  string
	Value *gqlValue
}

type gqlDirective struct {
	Name string
	Args []gqlArg
}

type gqlSelection struct {
	Kind          string // Field, InlineFragment, FragmentSpread
	Alias         string
	Name          string // field name or fragment name
	Args          []gqlArg
	Directives    []gqlDirective
	Selections    []*gqlSelection // nil for a leaf field
	TypeCondition string
	Loc           gqlLoc
}

func (s *gqlSelection) key() string {
	if s.Alias != "" {
		return s.Alias
	}
	return s.Name
}

type gqlVarDef struct {
	Name    string
	Type    string
	Default *gqlValue
}

type gqlOperation struct {
	Operation  string
	Name       string
	Variables  []gqlVarDef
	Selections []*gqlSelection
	Loc        gqlLoc
}

type gqlFragment struct {
	Name, TypeCondition string
	Selections          []*gqlSelection
	Loc                 gqlLoc
}

type gqlDocument struct {
	Operations []*gqlOperation
	Fragments  map[string]*gqlFragment
}

type gqlSyntaxError struct {
	Message string
	Loc     gqlLoc
}

func (e *gqlSyntaxError) Error() string { return e.Message }

// ---------------------------------------------------------------- lexer

type gqlToken struct {
	Kind  string // punct, name, int, float, string, eof
	Value string
	Loc   gqlLoc
}

func isNameStart(c byte) bool { return c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') }
func isNameChar(c byte) bool  { return isNameStart(c) || (c >= '0' && c <= '9') }
func isDigit(c byte) bool     { return c >= '0' && c <= '9' }

func gqlTokenize(src string) ([]gqlToken, error) {
	var tokens []gqlToken
	at, line, lineStart := 0, 1, 0
	loc := func() gqlLoc { return gqlLoc{line, at - lineStart + 1} }
	for at < len(src) {
		ch := src[at]
		switch {
		case ch == '\n':
			at++
			line++
			lineStart = at
			continue
		case ch == ' ' || ch == '\t' || ch == '\r' || ch == ',':
			at++
			continue
		case strings.HasPrefix(src[at:], "\xef\xbb\xbf"):
			at += 3
			continue
		case ch == '#':
			for at < len(src) && src[at] != '\n' {
				at++
			}
			continue
		}
		start := loc()
		if strings.HasPrefix(src[at:], "...") {
			tokens = append(tokens, gqlToken{"punct", "...", start})
			at += 3
			continue
		}
		if strings.IndexByte("!$&()[]{}:=@|", ch) >= 0 {
			tokens = append(tokens, gqlToken{"punct", string(ch), start})
			at++
			continue
		}
		if isNameStart(ch) {
			end := at + 1
			for end < len(src) && isNameChar(src[end]) {
				end++
			}
			tokens = append(tokens, gqlToken{"name", src[at:end], start})
			at = end
			continue
		}
		if ch == '-' || isDigit(ch) {
			end := at
			if src[end] == '-' {
				end++
			}
			for end < len(src) && isDigit(src[end]) {
				end++
			}
			kind := "int"
			if end < len(src) && src[end] == '.' {
				kind = "float"
				end++
				for end < len(src) && isDigit(src[end]) {
					end++
				}
			}
			if end < len(src) && (src[end] == 'e' || src[end] == 'E') {
				kind = "float"
				end++
				if end < len(src) && (src[end] == '+' || src[end] == '-') {
					end++
				}
				for end < len(src) && isDigit(src[end]) {
					end++
				}
			}
			if end == at || (end == at+1 && ch == '-') {
				return nil, &gqlSyntaxError{fmt.Sprintf("Syntax Error: Invalid number, unexpected character %q.", ch), start}
			}
			tokens = append(tokens, gqlToken{kind, src[at:end], start})
			at = end
			continue
		}
		if ch == '"' {
			if strings.HasPrefix(src[at:], `"""`) {
				end := strings.Index(src[at+3:], `"""`)
				if end < 0 {
					return nil, &gqlSyntaxError{"Syntax Error: Unterminated string.", start}
				}
				raw := strings.ReplaceAll(src[at+3:at+3+end], `\"""`, `"""`)
				line += strings.Count(raw, "\n")
				tokens = append(tokens, gqlToken{"string", blockString(raw), start})
				at += end + 6
				continue
			}
			var b strings.Builder
			i := at + 1
			for {
				if i >= len(src) || src[i] == '\n' {
					return nil, &gqlSyntaxError{"Syntax Error: Unterminated string.", start}
				}
				c := src[i]
				if c == '"' {
					break
				}
				if c == '\\' {
					if i+1 >= len(src) {
						return nil, &gqlSyntaxError{"Syntax Error: Unterminated string.", start}
					}
					e := src[i+1]
					switch e {
					case '"', '\\', '/':
						b.WriteByte(e)
					case 'b':
						b.WriteByte('\b')
					case 'f':
						b.WriteByte('\f')
					case 'n':
						b.WriteByte('\n')
					case 'r':
						b.WriteByte('\r')
					case 't':
						b.WriteByte('\t')
					case 'u':
						if i+6 > len(src) {
							return nil, &gqlSyntaxError{"Syntax Error: Invalid Unicode escape sequence.", start}
						}
						n, err := strconv.ParseUint(src[i+2:i+6], 16, 32)
						if err != nil {
							return nil, &gqlSyntaxError{"Syntax Error: Invalid Unicode escape sequence.", start}
						}
						b.WriteRune(rune(n))
						i += 6
						continue
					default:
						return nil, &gqlSyntaxError{"Syntax Error: Invalid character escape sequence.", start}
					}
					i += 2
					continue
				}
				b.WriteByte(c)
				i++
			}
			tokens = append(tokens, gqlToken{"string", b.String(), start})
			at = i + 1
			continue
		}
		return nil, &gqlSyntaxError{fmt.Sprintf("Syntax Error: Unexpected character %q.", ch), start}
	}
	tokens = append(tokens, gqlToken{"eof", "", loc()})
	return tokens, nil
}

func blockString(raw string) string {
	lines := strings.Split(strings.ReplaceAll(strings.ReplaceAll(raw, "\r\n", "\n"), "\r", "\n"), "\n")
	common := -1
	for _, l := range lines[1:] {
		indent := len(l) - len(strings.TrimLeft(l, " \t"))
		if indent < len(l) && (common < 0 || indent < common) {
			common = indent
		}
	}
	if common > 0 {
		for i := 1; i < len(lines); i++ {
			if len(lines[i]) >= common {
				lines[i] = lines[i][common:]
			}
		}
	}
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}

// ---------------------------------------------------------------- parser

type gqlParser struct {
	tokens []gqlToken
	at     int
}

func describeToken(t gqlToken) string {
	switch t.Kind {
	case "eof":
		return "<EOF>"
	case "punct":
		return `"` + t.Value + `"`
	case "string":
		return `String "` + t.Value + `"`
	case "name":
		return `Name "` + t.Value + `"`
	case "int":
		return `Int "` + t.Value + `"`
	}
	return `Float "` + t.Value + `"`
}

func (p *gqlParser) peek() gqlToken { return p.tokens[p.at] }
func (p *gqlParser) next() gqlToken {
	t := p.tokens[p.at]
	if t.Kind != "eof" {
		p.at++
	}
	return t
}
func (p *gqlParser) isPunct(v string) bool { t := p.peek(); return t.Kind == "punct" && t.Value == v }

func (p *gqlParser) expectPunct(v string) {
	t := p.next()
	if t.Kind != "punct" || t.Value != v {
		panic(&gqlSyntaxError{fmt.Sprintf(`Syntax Error: Expected "%s", found %s.`, v, describeToken(t)), t.Loc})
	}
}

func (p *gqlParser) expectName() gqlToken {
	t := p.next()
	if t.Kind != "name" {
		panic(&gqlSyntaxError{fmt.Sprintf("Syntax Error: Expected Name, found %s.", describeToken(t)), t.Loc})
	}
	return t
}

func (p *gqlParser) document() *gqlDocument {
	doc := &gqlDocument{Fragments: map[string]*gqlFragment{}}
	for p.peek().Kind != "eof" {
		t := p.peek()
		switch {
		case t.Kind == "punct" && t.Value == "{":
			doc.Operations = append(doc.Operations, &gqlOperation{Operation: "query", Selections: p.selectionSet(), Loc: t.Loc})
		case t.Kind == "name" && (t.Value == "query" || t.Value == "mutation" || t.Value == "subscription"):
			doc.Operations = append(doc.Operations, p.operation())
		case t.Kind == "name" && t.Value == "fragment":
			f := p.fragment()
			doc.Fragments[f.Name] = f
		default:
			panic(&gqlSyntaxError{fmt.Sprintf("Syntax Error: Unexpected %s.", describeToken(t)), t.Loc})
		}
	}
	if len(doc.Operations) == 0 {
		panic(&gqlSyntaxError{"Syntax Error: the document defines no operation.", p.peek().Loc})
	}
	return doc
}

func (p *gqlParser) operation() *gqlOperation {
	t := p.next()
	op := &gqlOperation{Operation: t.Value, Loc: t.Loc}
	if p.peek().Kind == "name" {
		op.Name = p.next().Value
	}
	if p.isPunct("(") {
		p.next()
		for !p.isPunct(")") {
			p.expectPunct("$")
			name := p.expectName().Value
			p.expectPunct(":")
			def := gqlVarDef{Name: name, Type: p.typeRef()}
			if p.isPunct("=") {
				p.next()
				def.Default = p.value(true)
			}
			p.directives()
			op.Variables = append(op.Variables, def)
		}
		p.next()
	}
	p.directives()
	op.Selections = p.selectionSet()
	return op
}

func (p *gqlParser) fragment() *gqlFragment {
	t := p.next()
	name := p.expectName().Value
	on := p.expectName()
	if on.Value != "on" {
		panic(&gqlSyntaxError{fmt.Sprintf(`Syntax Error: Expected "on", found %s.`, describeToken(on)), on.Loc})
	}
	cond := p.expectName().Value
	p.directives()
	return &gqlFragment{Name: name, TypeCondition: cond, Selections: p.selectionSet(), Loc: t.Loc}
}

func (p *gqlParser) typeRef() string {
	var text string
	if p.isPunct("[") {
		p.next()
		text = "[" + p.typeRef() + "]"
		p.expectPunct("]")
	} else {
		text = p.expectName().Value
	}
	if p.isPunct("!") {
		p.next()
		text += "!"
	}
	return text
}

func (p *gqlParser) directives() []gqlDirective {
	var list []gqlDirective
	for p.isPunct("@") {
		p.next()
		name := p.expectName().Value
		var args []gqlArg
		if p.isPunct("(") {
			args = p.arguments(false)
		}
		list = append(list, gqlDirective{name, args})
	}
	return list
}

func (p *gqlParser) selectionSet() []*gqlSelection {
	p.expectPunct("{")
	var sels []*gqlSelection
	for !p.isPunct("}") {
		if p.peek().Kind == "eof" {
			panic(&gqlSyntaxError{"Syntax Error: Expected Name, found <EOF>.", p.peek().Loc})
		}
		sels = append(sels, p.selection())
	}
	p.next()
	if len(sels) == 0 {
		panic(&gqlSyntaxError{`Syntax Error: Expected Name, found "}".`, p.peek().Loc})
	}
	return sels
}

func (p *gqlParser) selection() *gqlSelection {
	t := p.peek()
	if t.Kind == "punct" && t.Value == "..." {
		p.next()
		after := p.peek()
		if after.Kind == "name" && after.Value == "on" {
			p.next()
			cond := p.expectName().Value
			dirs := p.directives()
			return &gqlSelection{Kind: "InlineFragment", TypeCondition: cond, Directives: dirs, Selections: p.selectionSet(), Loc: t.Loc}
		}
		if after.Kind == "name" {
			p.next()
			return &gqlSelection{Kind: "FragmentSpread", Name: after.Value, Directives: p.directives(), Loc: t.Loc}
		}
		dirs := p.directives()
		return &gqlSelection{Kind: "InlineFragment", Directives: dirs, Selections: p.selectionSet(), Loc: t.Loc}
	}
	first := p.expectName()
	s := &gqlSelection{Kind: "Field", Name: first.Value, Loc: first.Loc}
	if p.isPunct(":") {
		p.next()
		s.Alias = first.Value
		s.Name = p.expectName().Value
	}
	if p.isPunct("(") {
		s.Args = p.arguments(false)
	}
	s.Directives = p.directives()
	if p.isPunct("{") {
		s.Selections = p.selectionSet()
	}
	return s
}

func (p *gqlParser) arguments(constant bool) []gqlArg {
	p.expectPunct("(")
	var args []gqlArg
	for !p.isPunct(")") {
		name := p.expectName().Value
		p.expectPunct(":")
		args = append(args, gqlArg{name, p.value(constant)})
	}
	p.next()
	return args
}

func (p *gqlParser) value(constant bool) *gqlValue {
	t := p.next()
	switch t.Kind {
	case "punct":
		switch t.Value {
		case "$":
			if !constant {
				return &gqlValue{Kind: "Variable", Str: p.expectName().Value}
			}
		case "[":
			v := &gqlValue{Kind: "List", List: []*gqlValue{}}
			for !p.isPunct("]") {
				if p.peek().Kind == "eof" {
					panic(&gqlSyntaxError{"Syntax Error: Expected Name, found <EOF>.", p.peek().Loc})
				}
				v.List = append(v.List, p.value(constant))
			}
			p.next()
			return v
		case "{":
			v := &gqlValue{Kind: "Object"}
			for !p.isPunct("}") {
				name := p.expectName().Value
				p.expectPunct(":")
				v.Fields = append(v.Fields, gqlArg{name, p.value(constant)})
			}
			p.next()
			return v
		}
	case "int":
		n, _ := strconv.ParseFloat(t.Value, 64)
		return &gqlValue{Kind: "Int", Num: n}
	case "float":
		n, _ := strconv.ParseFloat(t.Value, 64)
		return &gqlValue{Kind: "Float", Num: n}
	case "string":
		return &gqlValue{Kind: "String", Str: t.Value}
	case "name":
		switch t.Value {
		case "true":
			return &gqlValue{Kind: "Boolean", Bool: true}
		case "false":
			return &gqlValue{Kind: "Boolean", Bool: false}
		case "null":
			return &gqlValue{Kind: "Null"}
		}
		return &gqlValue{Kind: "Enum", Str: t.Value}
	}
	panic(&gqlSyntaxError{fmt.Sprintf("Syntax Error: Unexpected %s.", describeToken(t)), t.Loc})
}

func parseGQLDocument(src string) (doc *gqlDocument, err error) {
	defer func() {
		if r := recover(); r != nil {
			if se, ok := r.(*gqlSyntaxError); ok {
				doc, err = nil, se
				return
			}
			panic(r)
		}
	}()
	tokens, terr := gqlTokenize(src)
	if terr != nil {
		return nil, terr
	}
	p := &gqlParser{tokens: tokens}
	return p.document(), nil
}
