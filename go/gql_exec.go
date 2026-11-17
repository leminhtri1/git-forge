package main

// A small GraphQL executor (the Go twin of src/graphql/execute.ts): a schema of object, interface, union,
// enum, input and scalar types with field resolvers; document validation before anything runs (unknown
// fields and arguments reported as GitHub reports them); variable coercion; fragments; @include/@skip;
// and introspection served from the same type definitions. Results are ordered objects (selection order).

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// Resolver returns a field's value; it fails by panicking (*FieldError, *HTTPError, *inputError).
type Resolver func(parent any, args map[string]any) any

type ArgDef struct {
	Name       string
	Type       string
	Default    any
	HasDefault bool
}

type FieldDef struct {
	Name    string
	Type    string
	Args    []ArgDef
	Resolve Resolver
}

type TypeDef struct {
	Kind          string // OBJECT, INTERFACE, UNION, ENUM, INPUT_OBJECT, SCALAR
	Name          string
	Fields        []*FieldDef
	fieldIndex    map[string]*FieldDef
	Interfaces    []string
	PossibleTypes []string
	Values        []string
	InputFields   []ArgDef
}

func (t *TypeDef) Field(name string) *FieldDef {
	if t.fieldIndex == nil {
		t.fieldIndex = map[string]*FieldDef{}
		for _, f := range t.Fields {
			t.fieldIndex[f.Name] = f
		}
	}
	return t.fieldIndex[name]
}

func (t *TypeDef) AddField(f *FieldDef) {
	if t.Field(f.Name) != nil {
		for i, existing := range t.Fields {
			if existing.Name == f.Name {
				t.Fields[i] = f
			}
		}
	} else {
		t.Fields = append(t.Fields, f)
	}
	t.fieldIndex[f.Name] = f
}

func (t *TypeDef) isComposite() bool {
	return t.Kind == "OBJECT" || t.Kind == "INTERFACE" || t.Kind == "UNION"
}

type Schema struct {
	Types    map[string]*TypeDef
	Order    []string
	Query    string
	Mutation string
}

// FieldError becomes an errors entry carrying GitHub's type (NOT_FOUND, UNPROCESSABLE, ...).
type FieldError struct {
	Type    string
	Message string
}

func fieldErr(message, typ string) *FieldError { return &FieldError{Type: typ, Message: message} }

type inputError struct{ msg string }

// typeNamed lets a runtime value name its concrete type (for interfaces and unions).
type typeNamed interface{ TypeName() string }

// ---------------------------------------------------------------- type references

type typeRef struct {
	kind string // NON_NULL, LIST, NAMED
	name string
	of   *typeRef
}

var refCache = map[string]*typeRef{}

func parseTypeRef(text string) *typeRef {
	if r, ok := refCache[text]; ok {
		return r
	}
	var r *typeRef
	switch {
	case strings.HasSuffix(text, "!"):
		r = &typeRef{kind: "NON_NULL", of: parseTypeRef(text[:len(text)-1])}
	case strings.HasPrefix(text, "["):
		r = &typeRef{kind: "LIST", of: parseTypeRef(text[1 : len(text)-1])}
	default:
		r = &typeRef{kind: "NAMED", name: text}
	}
	refCache[text] = r
	return r
}

func namedType(r *typeRef) string {
	for r.kind != "NAMED" {
		r = r.of
	}
	return r.name
}

func typeText(r *typeRef) string {
	switch r.kind {
	case "NON_NULL":
		return typeText(r.of) + "!"
	case "LIST":
		return "[" + typeText(r.of) + "]"
	}
	return r.name
}

// ---------------------------------------------------------------- introspection

type introType struct {
	schema *Schema
	ref    *typeRef
}

func (t *introType) def() *TypeDef {
	if t.ref.kind != "NAMED" {
		return nil
	}
	return t.schema.Types[t.ref.name]
}

type introField struct {
	schema *Schema
	f      *FieldDef
}

type introInput struct {
	schema *Schema
	a      ArgDef
}

func f(name, typ string, resolve Resolver, args ...ArgDef) *FieldDef {
	return &FieldDef{Name: name, Type: typ, Args: args, Resolve: resolve}
}

func arg(name, typ string) ArgDef { return ArgDef{Name: name, Type: typ} }

func argDefault(name, typ string, def any) ArgDef {
	return ArgDef{Name: name, Type: typ, Default: def, HasDefault: true}
}

func constant(v any) Resolver { return func(any, map[string]any) any { return v } }

func introspectionTypes() []*TypeDef {
	includeDeprecated := argDefault("includeDeprecated", "Boolean", false)
	return []*TypeDef{
		{Kind: "OBJECT", Name: "__Schema", Fields: []*FieldDef{
			f("description", "String", constant("The git-forge GraphQL schema")),
			f("types", "[__Type!]!", func(p any, _ map[string]any) any {
				s := p.(*Schema)
				out := []any{}
				for _, name := range s.Order {
					out = append(out, &introType{s, &typeRef{kind: "NAMED", name: name}})
				}
				return out
			}),
			f("queryType", "__Type!", func(p any, _ map[string]any) any {
				s := p.(*Schema)
				return &introType{s, &typeRef{kind: "NAMED", name: s.Query}}
			}),
			f("mutationType", "__Type", func(p any, _ map[string]any) any {
				s := p.(*Schema)
				return &introType{s, &typeRef{kind: "NAMED", name: s.Mutation}}
			}),
			f("subscriptionType", "__Type", constant(nil)),
			f("directives", "[__Directive!]!", constant([]any{
				Obj{{"name", "include"}, {"locations", []any{"FIELD", "FRAGMENT_SPREAD", "INLINE_FRAGMENT"}}},
				Obj{{"name", "skip"}, {"locations", []any{"FIELD", "FRAGMENT_SPREAD", "INLINE_FRAGMENT"}}},
			})),
		}},
		{Kind: "OBJECT", Name: "__Type", Fields: []*FieldDef{
			f("kind", "__TypeKind!", func(p any, _ map[string]any) any {
				t := p.(*introType)
				if t.ref.kind != "NAMED" {
					return t.ref.kind
				}
				if d := t.def(); d != nil {
					return d.Kind
				}
				return "SCALAR"
			}),
			f("name", "String", func(p any, _ map[string]any) any {
				t := p.(*introType)
				if t.ref.kind != "NAMED" {
					return nil
				}
				return t.ref.name
			}),
			f("description", "String", constant(nil)),
			f("specifiedByURL", "String", constant(nil)),
			f("fields", "[__Field!]", func(p any, _ map[string]any) any {
				t := p.(*introType)
				d := t.def()
				if d == nil || (d.Kind != "OBJECT" && d.Kind != "INTERFACE") {
					return nil
				}
				out := []any{}
				for _, fd := range d.Fields {
					if !strings.HasPrefix(fd.Name, "__") {
						out = append(out, &introField{t.schema, fd})
					}
				}
				return out
			}, includeDeprecated),
			f("interfaces", "[__Type!]", func(p any, _ map[string]any) any {
				t := p.(*introType)
				d := t.def()
				if d == nil {
					return nil
				}
				if d.Kind == "OBJECT" {
					out := []any{}
					for _, n := range d.Interfaces {
						out = append(out, &introType{t.schema, &typeRef{kind: "NAMED", name: n}})
					}
					return out
				}
				if d.Kind == "INTERFACE" {
					return []any{}
				}
				return nil
			}),
			f("possibleTypes", "[__Type!]", func(p any, _ map[string]any) any {
				t := p.(*introType)
				d := t.def()
				if d == nil || (d.Kind != "INTERFACE" && d.Kind != "UNION") {
					return nil
				}
				out := []any{}
				for _, n := range d.PossibleTypes {
					out = append(out, &introType{t.schema, &typeRef{kind: "NAMED", name: n}})
				}
				return out
			}),
			f("enumValues", "[__EnumValue!]", func(p any, _ map[string]any) any {
				d := p.(*introType).def()
				if d == nil || d.Kind != "ENUM" {
					return nil
				}
				out := []any{}
				for _, v := range d.Values {
					out = append(out, Obj{{"name", v}})
				}
				return out
			}, includeDeprecated),
			f("inputFields", "[__InputValue!]", func(p any, _ map[string]any) any {
				t := p.(*introType)
				d := t.def()
				if d == nil || d.Kind != "INPUT_OBJECT" {
					return nil
				}
				out := []any{}
				for _, a := range d.InputFields {
					out = append(out, &introInput{t.schema, a})
				}
				return out
			}, includeDeprecated),
			f("ofType", "__Type", func(p any, _ map[string]any) any {
				t := p.(*introType)
				if t.ref.kind == "NAMED" {
					return nil
				}
				return &introType{t.schema, t.ref.of}
			}),
			f("isOneOf", "Boolean", constant(false)),
		}},
		{Kind: "OBJECT", Name: "__Field", Fields: []*FieldDef{
			f("name", "String!", func(p any, _ map[string]any) any { return p.(*introField).f.Name }),
			f("description", "String", constant(nil)),
			f("args", "[__InputValue!]!", func(p any, _ map[string]any) any {
				fl := p.(*introField)
				out := []any{}
				for _, a := range fl.f.Args {
					out = append(out, &introInput{fl.schema, a})
				}
				return out
			}, includeDeprecated),
			f("type", "__Type!", func(p any, _ map[string]any) any {
				fl := p.(*introField)
				return &introType{fl.schema, parseTypeRef(fl.f.Type)}
			}),
			f("isDeprecated", "Boolean!", constant(false)),
			f("deprecationReason", "String", constant(nil)),
		}},
		{Kind: "OBJECT", Name: "__InputValue", Fields: []*FieldDef{
			f("name", "String!", func(p any, _ map[string]any) any { return p.(*introInput).a.Name }),
			f("description", "String", constant(nil)),
			f("type", "__Type!", func(p any, _ map[string]any) any {
				in := p.(*introInput)
				return &introType{in.schema, parseTypeRef(in.a.Type)}
			}),
			f("defaultValue", "String", func(p any, _ map[string]any) any {
				in := p.(*introInput)
				if !in.a.HasDefault {
					return nil
				}
				return string(toJSON(in.a.Default))
			}),
			f("isDeprecated", "Boolean!", constant(false)),
			f("deprecationReason", "String", constant(nil)),
		}},
		{Kind: "OBJECT", Name: "__EnumValue", Fields: []*FieldDef{
			f("name", "String!", nil),
			f("description", "String", constant(nil)),
			f("isDeprecated", "Boolean!", constant(false)),
			f("deprecationReason", "String", constant(nil)),
		}},
		{Kind: "OBJECT", Name: "__Directive", Fields: []*FieldDef{
			f("name", "String!", nil),
			f("description", "String", constant(nil)),
			f("locations", "[__DirectiveLocation!]!", nil),
			f("args", "[__InputValue!]!", func(any, map[string]any) any { return []any{} }),
			f("isRepeatable", "Boolean!", constant(false)),
		}},
		{Kind: "ENUM", Name: "__TypeKind", Values: []string{"SCALAR", "OBJECT", "INTERFACE", "UNION", "ENUM", "INPUT_OBJECT", "LIST", "NON_NULL"}},
		{Kind: "ENUM", Name: "__DirectiveLocation", Values: []string{"QUERY", "MUTATION", "SUBSCRIPTION", "FIELD", "FRAGMENT_DEFINITION", "FRAGMENT_SPREAD", "INLINE_FRAGMENT", "VARIABLE_DEFINITION"}},
	}
}

// buildSchema adds the built-in scalars, the introspection types and Query.__type/__schema.
func buildSchema(types []*TypeDef, query, mutation string) *Schema {
	s := &Schema{Types: map[string]*TypeDef{}, Query: query, Mutation: mutation}
	add := func(t *TypeDef) {
		if _, exists := s.Types[t.Name]; !exists {
			s.Order = append(s.Order, t.Name)
		}
		s.Types[t.Name] = t
	}
	for _, scalar := range []string{"String", "Int", "Float", "Boolean", "ID"} {
		add(&TypeDef{Kind: "SCALAR", Name: scalar})
	}
	for _, t := range types {
		add(t)
	}
	for _, t := range introspectionTypes() {
		add(t)
	}
	if root := s.Types[query]; root != nil {
		root.Field("") // build the index
		root.AddField(f("__type", "__Type", func(_ any, a map[string]any) any {
			name, _ := a["name"].(string)
			if _, ok := s.Types[name]; !ok {
				return nil
			}
			return &introType{s, &typeRef{kind: "NAMED", name: name}}
		}, arg("name", "String!")))
		root.AddField(f("__schema", "__Schema!", func(any, map[string]any) any { return s }))
	}
	return s
}

// ---------------------------------------------------------------- validation

func gqlError(message string, loc *gqlLoc, path []any, ext Obj) Obj {
	e := Obj{{"message", message}}
	if loc != nil {
		e = append(e, KV{"locations", []any{Obj{{"line", loc.Line}, {"column", loc.Column}}}})
	}
	if path != nil {
		e = append(e, KV{"path", path})
	}
	if ext != nil {
		e = append(e, KV{"extensions", ext})
	}
	return e
}

func extendPath(path []any, key any) []any {
	out := make([]any, len(path), len(path)+1)
	copy(out, path)
	return append(out, key)
}

func validateDocument(s *Schema, doc *gqlDocument, op *gqlOperation) []any {
	var errs []any
	opName := "query"
	root := s.Query
	if op.Operation == "mutation" {
		opName, root = "mutation", s.Mutation
	}
	visiting := map[string]bool{}
	var walk func(typeName string, sels []*gqlSelection, path []any)
	walk = func(typeName string, sels []*gqlSelection, path []any) {
		def := s.Types[typeName]
		for _, sel := range sels {
			loc := sel.Loc
			switch sel.Kind {
			case "Field":
				fieldPath := extendPath(path, sel.key())
				if sel.Name == "__typename" {
					if sel.Selections != nil {
						errs = append(errs, gqlError("Selections can't be made on scalars (field '__typename' returns String but has selections [...])", &loc, fieldPath, nil))
					}
					continue
				}
				var fd *FieldDef
				if def != nil && (def.Kind == "OBJECT" || def.Kind == "INTERFACE") {
					fd = def.Field(sel.Name)
				}
				if fd == nil {
					errs = append(errs, gqlError(fmt.Sprintf("Field '%s' doesn't exist on type '%s'", sel.Name, typeName), &loc, fieldPath,
						Obj{{"code", "undefinedField"}, {"typeName", typeName}, {"fieldName", sel.Name}}))
					continue
				}
				for _, a := range sel.Args {
					found := false
					for _, ad := range fd.Args {
						if ad.Name == a.Name {
							found = true
						}
					}
					if !found {
						errs = append(errs, gqlError(fmt.Sprintf("Field '%s' doesn't accept argument '%s'", sel.Name, a.Name), &loc, fieldPath,
							Obj{{"code", "argumentNotAccepted"}, {"name", sel.Name}, {"typeName", "Field"}, {"argumentName", a.Name}}))
					}
				}
				for _, ad := range fd.Args {
					if !strings.HasSuffix(ad.Type, "!") || ad.HasDefault {
						continue
					}
					given := false
					for _, a := range sel.Args {
						if a.Name == ad.Name {
							given = true
						}
					}
					if !given {
						errs = append(errs, gqlError(fmt.Sprintf("Field '%s' is missing required arguments: %s", sel.Name, ad.Name), &loc, fieldPath,
							Obj{{"code", "missingRequiredArguments"}, {"className", "Field"}, {"name", sel.Name}, {"arguments", ad.Name}}))
					}
				}
				target := s.Types[namedType(parseTypeRef(fd.Type))]
				composite := target != nil && target.isComposite()
				switch {
				case composite && sel.Selections == nil:
					errs = append(errs, gqlError(fmt.Sprintf("Field must have selections (field '%s' returns %s but has no selections. Did you mean '%s { ... }'?)", sel.Name, target.Name, sel.Name), &loc, fieldPath,
						Obj{{"code", "selectionMismatch"}, {"nodeName", "field '" + sel.Name + "'"}, {"typeName", target.Name}}))
				case !composite && sel.Selections != nil:
					errs = append(errs, gqlError(fmt.Sprintf("Selections can't be made on scalars (field '%s' returns %s but has selections [...])", sel.Name, namedType(parseTypeRef(fd.Type))), &loc, fieldPath,
						Obj{{"code", "selectionMismatch"}}))
				case composite:
					walk(target.Name, sel.Selections, fieldPath)
				}
			case "InlineFragment":
				cond := sel.TypeCondition
				if cond == "" {
					cond = typeName
				}
				if s.Types[cond] == nil {
					errs = append(errs, gqlError(fmt.Sprintf("No such type %s, so it can't be a fragment condition", cond), &loc, extendPath(path, "... on "+cond),
						Obj{{"code", "undefinedType"}, {"typeName", cond}}))
					continue
				}
				walk(cond, sel.Selections, extendPath(path, "... on "+cond))
			case "FragmentSpread":
				frag := doc.Fragments[sel.Name]
				if frag == nil {
					errs = append(errs, gqlError(fmt.Sprintf("Fragment %s was used, but not defined", sel.Name), &loc, extendPath(path, "... "+sel.Name),
						Obj{{"code", "useAndDefineFragment"}, {"fragmentName", sel.Name}}))
					continue
				}
				if visiting[frag.Name] {
					errs = append(errs, gqlError(fmt.Sprintf("Fragment %s contains an infinite loop", frag.Name), &loc, path, nil))
					continue
				}
				if s.Types[frag.TypeCondition] == nil {
					floc := frag.Loc
					errs = append(errs, gqlError(fmt.Sprintf("No such type %s, so it can't be a fragment condition", frag.TypeCondition), &floc, []any{"fragment " + frag.Name}, nil))
					continue
				}
				visiting[frag.Name] = true
				walk(frag.TypeCondition, frag.Selections, []any{"fragment " + frag.Name})
				delete(visiting, frag.Name)
			}
		}
	}
	rootPath := []any{opName}
	if op.Name != "" {
		rootPath = []any{opName + " " + op.Name}
	}
	if s.Types[root] == nil {
		errs = append(errs, gqlError("Schema is not configured for "+opName+"s", &op.Loc, rootPath, nil))
	} else {
		walk(root, op.Selections, rootPath)
	}
	for _, v := range op.Variables {
		named := namedType(parseTypeRef(v.Type))
		def := s.Types[named]
		if def == nil {
			errs = append(errs, gqlError(fmt.Sprintf("%s isn't a defined input type (on $%s)", named, v.Name), &op.Loc, rootPath,
				Obj{{"code", "variableRequiresValidType"}, {"typeName", named}, {"variableName", v.Name}}))
		} else if def.Kind != "SCALAR" && def.Kind != "ENUM" && def.Kind != "INPUT_OBJECT" {
			errs = append(errs, gqlError(fmt.Sprintf("%s isn't a valid input type (on $%s)", named, v.Name), &op.Loc, rootPath, nil))
		}
	}
	return errs
}

// ---------------------------------------------------------------- values and arguments

// absent marks "no value given" (distinct from JSON null).
type absentT struct{}

var absent = absentT{}

func coerceInput(s *Schema, ref *typeRef, value any, where string) any {
	if ref.kind == "NON_NULL" {
		if value == nil || value == absent {
			panic(&inputError{fmt.Sprintf("%s expected a value of type %s, got null", where, typeText(ref))})
		}
		return coerceInput(s, ref.of, value, where)
	}
	if value == absent {
		return absent
	}
	if value == nil {
		return nil
	}
	if ref.kind == "LIST" {
		list, ok := value.([]any)
		if !ok {
			list = []any{value}
		}
		out := make([]any, len(list))
		for i, item := range list {
			out[i] = coerceInput(s, ref.of, item, fmt.Sprintf("%s[%d]", where, i))
			if out[i] == absent {
				out[i] = nil
			}
		}
		return out
	}
	def := s.Types[ref.name]
	if def == nil {
		return value
	}
	bad := func(want string) {
		panic(&inputError{fmt.Sprintf("%s could not coerce %s to %s", where, string(toJSON(value)), want)})
	}
	switch def.Kind {
	case "SCALAR":
		switch def.Name {
		case "Int":
			n, ok := value.(float64)
			if !ok || n != math.Trunc(n) {
				bad("Int")
			}
			return n
		case "Float":
			if _, ok := value.(float64); !ok {
				bad("Float")
			}
		case "Boolean":
			if _, ok := value.(bool); !ok {
				bad("Boolean")
			}
		case "ID":
			switch x := value.(type) {
			case string:
				return x
			case float64:
				return fmt.Sprint(int64(x))
			}
			bad("ID")
		case "String":
			if _, ok := value.(string); !ok {
				bad("String")
			}
		}
		return value
	case "ENUM":
		str, ok := value.(string)
		valid := false
		for _, v := range def.Values {
			if v == str {
				valid = true
			}
		}
		if !ok || !valid {
			panic(&inputError{fmt.Sprintf("%s expected one of %s (%s), got %s", where, def.Name, strings.Join(def.Values, ", "), string(toJSON(value)))})
		}
		return str
	case "INPUT_OBJECT":
		in, ok := value.(map[string]any)
		if !ok {
			panic(&inputError{fmt.Sprintf("%s expected an input object %s", where, def.Name)})
		}
		known := map[string]bool{}
		for _, fd := range def.InputFields {
			known[fd.Name] = true
		}
		keys := make([]string, 0, len(in))
		for k := range in {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if !known[k] {
				panic(&inputError{fmt.Sprintf("%s has the field %s, which %s does not define", where, k, def.Name)})
			}
		}
		out := map[string]any{}
		for _, fd := range def.InputFields {
			given, present := in[fd.Name]
			if !present {
				if fd.HasDefault {
					given = fd.Default
				} else {
					given = absent
				}
			}
			c := coerceInput(s, parseTypeRef(fd.Type), given, where+"."+fd.Name)
			if c != absent {
				out[fd.Name] = c
			}
		}
		return out
	}
	return value
}

func literal(v *gqlValue, vars map[string]any) any {
	switch v.Kind {
	case "Variable":
		if val, ok := vars[v.Str]; ok {
			return val
		}
		return absent
	case "Int", "Float":
		return v.Num
	case "String", "Enum":
		return v.Str
	case "Boolean":
		return v.Bool
	case "Null":
		return nil
	case "List":
		out := make([]any, 0, len(v.List))
		for _, item := range v.List {
			x := literal(item, vars)
			if x == absent {
				x = nil
			}
			out = append(out, x)
		}
		return out
	case "Object":
		out := map[string]any{}
		for _, fl := range v.Fields {
			x := literal(fl.Value, vars)
			if x != absent {
				out[fl.Name] = x
			}
		}
		return out
	}
	return nil
}

func argumentsOf(s *Schema, fd *FieldDef, node *gqlSelection, vars map[string]any) map[string]any {
	args := map[string]any{}
	for _, ad := range fd.Args {
		var value any = absent
		for _, a := range node.Args {
			if a.Name == ad.Name {
				value = literal(a.Value, vars)
			}
		}
		if value == absent && ad.HasDefault {
			value = ad.Default
		}
		c := coerceInput(s, parseTypeRef(ad.Type), value, fmt.Sprintf("Argument '%s' on Field '%s'", ad.Name, node.Name))
		if c != absent {
			args[ad.Name] = c
		}
	}
	return args
}

func skipped(dirs []gqlDirective, vars map[string]any) bool {
	for _, d := range dirs {
		var value any
		for _, a := range d.Args {
			if a.Name == "if" {
				value = literal(a.Value, vars)
			}
		}
		if d.Name == "skip" && value == true {
			return true
		}
		if d.Name == "include" && value == false {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- execution

type runState struct {
	s      *Schema
	doc    *gqlDocument
	vars   map[string]any
	errors []any
}

func typeMatches(s *Schema, cond, runtime string) bool {
	if cond == "" || cond == runtime {
		return true
	}
	def := s.Types[cond]
	if def != nil && (def.Kind == "INTERFACE" || def.Kind == "UNION") {
		for _, p := range def.PossibleTypes {
			if p == runtime {
				return true
			}
		}
	}
	return false
}

type fieldGroup struct {
	key   string
	nodes []*gqlSelection
}

func collectFields(st *runState, runtime string, sels []*gqlSelection, groups []*fieldGroup) []*fieldGroup {
	for _, sel := range sels {
		if skipped(sel.Directives, st.vars) {
			continue
		}
		switch sel.Kind {
		case "Field":
			k := sel.key()
			found := false
			for _, g := range groups {
				if g.key == k {
					g.nodes = append(g.nodes, sel)
					found = true
					break
				}
			}
			if !found {
				groups = append(groups, &fieldGroup{k, []*gqlSelection{sel}})
			}
		case "InlineFragment":
			if typeMatches(st.s, sel.TypeCondition, runtime) {
				groups = collectFields(st, runtime, sel.Selections, groups)
			}
		case "FragmentSpread":
			if frag := st.doc.Fragments[sel.Name]; frag != nil && typeMatches(st.s, frag.TypeCondition, runtime) {
				groups = collectFields(st, runtime, frag.Selections, groups)
			}
		}
	}
	return groups
}

func runtimeTypeOf(s *Schema, declared string, value any) string {
	def := s.Types[declared]
	if def != nil && (def.Kind == "INTERFACE" || def.Kind == "UNION") {
		switch v := value.(type) {
		case typeNamed:
			return v.TypeName()
		case Obj:
			if n, ok := v.Get("__typename"); ok {
				if str, isStr := n.(string); isStr {
					return str
				}
			}
		}
	}
	return declared
}

func defaultResolve(parent any, name string) any {
	switch p := parent.(type) {
	case Obj:
		v, _ := p.Get(name)
		return v
	case map[string]any:
		return p[name]
	}
	return nil
}

func (st *runState) executeObject(typeName string, parent any, sels []*gqlSelection, path []any) Obj {
	def := st.s.Types[typeName]
	out := Obj{}
	for _, g := range collectFields(st, typeName, sels, nil) {
		node := g.nodes[0]
		if node.Name == "__typename" {
			out = append(out, KV{g.key, typeName})
			continue
		}
		if def == nil {
			continue
		}
		fd := def.Field(node.Name)
		if fd == nil {
			continue
		}
		fieldPath := extendPath(path, g.key)
		value, failed := st.resolveField(fd, node, parent, fieldPath)
		if failed {
			out = append(out, KV{g.key, nil})
			continue
		}
		var merged []*gqlSelection
		for _, n := range g.nodes {
			merged = append(merged, n.Selections...)
		}
		out = append(out, KV{g.key, st.complete(parseTypeRef(fd.Type), value, merged, fieldPath)})
	}
	return out
}

func (st *runState) resolveField(fd *FieldDef, node *gqlSelection, parent any, path []any) (value any, failed bool) {
	defer func() {
		if r := recover(); r != nil {
			st.errors = append(st.errors, fieldErrorJSON(r, node, path))
			value, failed = nil, true
		}
	}()
	args := argumentsOf(st.s, fd, node, st.vars)
	if fd.Resolve != nil {
		return fd.Resolve(parent, args), false
	}
	return defaultResolve(parent, node.Name), false
}

func fieldErrorJSON(r any, node *gqlSelection, path []any) Obj {
	loc := []any{Obj{{"line", node.Loc.Line}, {"column", node.Loc.Column}}}
	switch e := r.(type) {
	case *FieldError:
		return Obj{{"type", e.Type}, {"path", path}, {"locations", loc}, {"message", e.Message}}
	case *HTTPError:
		typ := "UNPROCESSABLE"
		if e.Status == 404 {
			typ = "NOT_FOUND"
		} else if e.Status == 403 {
			typ = "FORBIDDEN"
		}
		message := e.Message
		if errsV, ok := e.Extra.Get("errors"); ok {
			if list, isList := errsV.([]any); isList {
				var parts []string
				for _, item := range list {
					if o, isObj := item.(Obj); isObj {
						if m, has := o.Get("message"); has {
							if str, isStr := m.(string); isStr && str != "" {
								parts = append(parts, str)
							}
						}
					}
				}
				if len(parts) > 0 {
					message = strings.Join(parts, "; ")
				}
			}
		}
		return Obj{{"type", typ}, {"path", path}, {"locations", loc}, {"message", message}}
	case *inputError:
		return Obj{{"path", path}, {"locations", loc}, {"message", e.msg}, {"extensions", Obj{{"code", "argumentLiteralsIncompatible"}}}}
	}
	return Obj{{"path", path}, {"locations", loc}, {"message", fmt.Sprint(r)}}
}

func (st *runState) complete(ref *typeRef, value any, sels []*gqlSelection, path []any) any {
	if ref.kind == "NON_NULL" {
		return st.complete(ref.of, value, sels, path)
	}
	if value == nil || value == absent {
		return nil
	}
	if ref.kind == "LIST" {
		list, ok := value.([]any)
		if !ok {
			list = []any{value}
		}
		out := make([]any, len(list))
		for i, item := range list {
			out[i] = st.complete(ref.of, item, sels, extendPath(path, i))
		}
		return out
	}
	def := st.s.Types[ref.name]
	if def == nil || def.Kind == "SCALAR" || def.Kind == "ENUM" {
		return value
	}
	return st.executeObject(runtimeTypeOf(st.s, ref.name, value), value, sels, path)
}

func runOperation(s *Schema, request map[string]any) Obj {
	query, ok := request["query"].(string)
	if !ok || strings.TrimSpace(query) == "" {
		return Obj{{"errors", []any{Obj{{"message", "A query attribute must be specified and must be a string."}}}}}
	}
	doc, err := parseGQLDocument(query)
	if err != nil {
		se := err.(*gqlSyntaxError)
		return Obj{{"errors", []any{Obj{{"message", se.Message}, {"locations", []any{Obj{{"line", se.Loc.Line}, {"column", se.Loc.Column}}}}}}}}
	}
	opName, _ := request["operationName"].(string)
	var op *gqlOperation
	if opName != "" {
		for _, o := range doc.Operations {
			if o.Name == opName {
				op = o
			}
		}
	} else if len(doc.Operations) == 1 {
		op = doc.Operations[0]
	}
	if op == nil {
		msg := "An operation name is required"
		if opName != "" {
			msg = fmt.Sprintf("No operation named %q", opName)
		}
		return Obj{{"errors", []any{Obj{{"message", msg}}}}}
	}
	if errs := validateDocument(s, doc, op); len(errs) > 0 {
		return Obj{{"errors", errs}}
	}
	provided, _ := request["variables"].(map[string]any)
	vars := map[string]any{}
	for _, def := range op.Variables {
		var value any = absent
		if v, present := provided[def.Name]; present {
			value = v
		}
		if value == absent && def.Default != nil {
			value = literal(def.Default, nil)
		}
		var coerced any
		var bad *inputError
		func() {
			defer func() {
				if r := recover(); r != nil {
					if ie, isInput := r.(*inputError); isInput {
						bad = ie
						return
					}
					panic(r)
				}
			}()
			coerced = coerceInput(s, parseTypeRef(def.Type), value, fmt.Sprintf("Variable $%s of type %s", def.Name, def.Type))
		}()
		if bad != nil {
			v := value
			if v == absent {
				v = nil
			}
			return Obj{{"errors", []any{Obj{{"message", bad.msg + " (was provided invalid value)"}, {"locations", []any{Obj{{"line", op.Loc.Line}, {"column", op.Loc.Column}}}}, {"extensions", Obj{{"value", v}, {"problems", []any{}}}}}}}}
		}
		if coerced != absent {
			vars[def.Name] = coerced
		}
	}
	st := &runState{s: s, doc: doc, vars: vars}
	root := s.Query
	if op.Operation == "mutation" {
		root = s.Mutation
	}
	data := st.executeObject(root, Obj{}, op.Selections, []any{})
	if len(st.errors) > 0 {
		return Obj{{"data", data}, {"errors", st.errors}}
	}
	return Obj{{"data", data}}
}

// executeGraphQL runs one request body: an operation object, or an array of them (batching).
func executeGraphQL(s *Schema, body any) any {
	switch b := body.(type) {
	case []any:
		out := make([]any, len(b))
		for i, item := range b {
			if m, ok := item.(map[string]any); ok {
				out[i] = runOperation(s, m)
			} else {
				out[i] = Obj{{"errors", []any{Obj{{"message", "Each batched operation must be an object"}}}}}
			}
		}
		return out
	case map[string]any:
		return runOperation(s, b)
	}
	return Obj{{"errors", []any{Obj{{"message", "The request body must be a JSON object"}}}}}
}
