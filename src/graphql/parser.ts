// A GraphQL document parser (the executable subset of the October 2021 spec): operations with
// variable definitions, selection sets, aliases, arguments, directives, fragments and inline fragments,
// and every literal form. Type-system definitions are not accepted: this server only executes.

export interface Location {
  readonly line: number;
  readonly column: number;
}

export type ValueNode =
  | { kind: 'Variable'; name: string }
  | { kind: 'Int'; value: number }
  | { kind: 'Float'; value: number }
  | { kind: 'String'; value: string }
  | { kind: 'Boolean'; value: boolean }
  | { kind: 'Null' }
  | { kind: 'Enum'; value: string }
  | { kind: 'List'; values: ValueNode[] }
  | { kind: 'Object'; fields: { name: string; value: ValueNode }[] };

export interface Argument {
  name: string;
  value: ValueNode;
}

export interface Directive {
  name: string;
  args: Argument[];
}

export interface FieldNode {
  kind: 'Field';
  alias: string | null;
  name: string;
  args: Argument[];
  directives: Directive[];
  selections: Selection[] | null;
  loc: Location;
}

export interface InlineFragmentNode {
  kind: 'InlineFragment';
  typeCondition: string | null;
  directives: Directive[];
  selections: Selection[];
  loc: Location;
}

export interface FragmentSpreadNode {
  kind: 'FragmentSpread';
  name: string;
  directives: Directive[];
  loc: Location;
}

export type Selection = FieldNode | InlineFragmentNode | FragmentSpreadNode;

export interface VariableDefinition {
  name: string;
  type: string; // as written, e.g. "[IssueState!]"
  defaultValue: ValueNode | null;
}

export interface OperationNode {
  kind: 'Operation';
  operation: 'query' | 'mutation' | 'subscription';
  name: string | null;
  variables: VariableDefinition[];
  directives: Directive[];
  selections: Selection[];
  loc: Location;
}

export interface FragmentNode {
  kind: 'Fragment';
  name: string;
  typeCondition: string;
  selections: Selection[];
  loc: Location;
}

export interface DocumentNode {
  operations: OperationNode[];
  fragments: Map<string, FragmentNode>;
}

export class GraphQLSyntaxError extends Error {
  readonly loc: Location;
  constructor(message: string, loc: Location) {
    super(message);
    this.loc = loc;
  }
}

// ------------------------------------------------------------------ lexer

type TokenKind = 'punct' | 'name' | 'int' | 'float' | 'string' | 'eof';

interface Token {
  kind: TokenKind;
  value: string;
  loc: Location;
}

function tokenize(source: string): Token[] {
  const tokens: Token[] = [];
  let at = 0;
  let line = 1;
  let lineStart = 0;
  const loc = (): Location => ({ line, column: at - lineStart + 1 });
  while (at < source.length) {
    const ch = source[at]!;
    if (ch === '\n') {
      at++;
      line++;
      lineStart = at;
      continue;
    }
    if (ch === ' ' || ch === '\t' || ch === '\r' || ch === ',' || ch === '﻿') {
      at++;
      continue;
    }
    if (ch === '#') {
      while (at < source.length && source[at] !== '\n') at++;
      continue;
    }
    const start = loc();
    if (ch === '.' && source.startsWith('...', at)) {
      tokens.push({ kind: 'punct', value: '...', loc: start });
      at += 3;
      continue;
    }
    if ('!$&()[]{}:=@|'.includes(ch)) {
      tokens.push({ kind: 'punct', value: ch, loc: start });
      at++;
      continue;
    }
    if (/[A-Za-z_]/.test(ch)) {
      let end = at + 1;
      while (end < source.length && /[A-Za-z0-9_]/.test(source[end]!)) end++;
      tokens.push({ kind: 'name', value: source.slice(at, end), loc: start });
      at = end;
      continue;
    }
    if (ch === '-' || /[0-9]/.test(ch)) {
      const match = /^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?/.exec(source.slice(at));
      if (match === null) throw new GraphQLSyntaxError(`Syntax Error: Invalid number, unexpected character ${JSON.stringify(ch)}.`, start);
      tokens.push({ kind: match[2] !== undefined || match[3] !== undefined ? 'float' : 'int', value: match[0], loc: start });
      at += match[0].length;
      continue;
    }
    if (ch === '"') {
      if (source.startsWith('"""', at)) {
        const end = source.indexOf('"""', at + 3);
        if (end < 0) throw new GraphQLSyntaxError('Syntax Error: Unterminated string.', start);
        const raw = source.slice(at + 3, end).replace(/\\"""/g, '"""');
        for (const c of raw) if (c === '\n') line++;
        tokens.push({ kind: 'string', value: blockString(raw), loc: start });
        at = end + 3;
        continue;
      }
      let value = '';
      let i = at + 1;
      for (;;) {
        if (i >= source.length || source[i] === '\n') throw new GraphQLSyntaxError('Syntax Error: Unterminated string.', start);
        const c = source[i]!;
        if (c === '"') break;
        if (c === '\\') {
          const e = source[i + 1];
          const map: Record<string, string> = { '"': '"', '\\': '\\', '/': '/', b: '\b', f: '\f', n: '\n', r: '\r', t: '\t' };
          if (e === 'u') {
            value += String.fromCharCode(parseInt(source.slice(i + 2, i + 6), 16));
            i += 6;
            continue;
          }
          if (e === undefined || map[e] === undefined) throw new GraphQLSyntaxError('Syntax Error: Invalid character escape sequence.', start);
          value += map[e];
          i += 2;
          continue;
        }
        value += c;
        i++;
      }
      tokens.push({ kind: 'string', value, loc: start });
      at = i + 1;
      continue;
    }
    throw new GraphQLSyntaxError(`Syntax Error: Unexpected character ${JSON.stringify(ch)}.`, start);
  }
  tokens.push({ kind: 'eof', value: '', loc: loc() });
  return tokens;
}

function blockString(raw: string): string {
  const lines = raw.split(/\r\n|\n|\r/);
  let common: number | null = null;
  for (let i = 1; i < lines.length; i++) {
    const l = lines[i]!;
    const indent = l.length - l.trimStart().length;
    if (indent < l.length && (common === null || indent < common)) common = indent;
  }
  if (common !== null) for (let i = 1; i < lines.length; i++) lines[i] = lines[i]!.slice(common);
  while (lines.length > 0 && lines[0]!.trim() === '') lines.shift();
  while (lines.length > 0 && lines[lines.length - 1]!.trim() === '') lines.pop();
  return lines.join('\n');
}

// ------------------------------------------------------------------ parser

class Parser {
  private readonly tokens: Token[];
  private at = 0;

  constructor(source: string) {
    this.tokens = tokenize(source);
  }

  private peek(): Token {
    return this.tokens[this.at]!;
  }

  private next(): Token {
    return this.tokens[this.at++]!;
  }

  private isPunct(value: string): boolean {
    const t = this.peek();
    return t.kind === 'punct' && t.value === value;
  }

  private expectPunct(value: string): Token {
    const t = this.next();
    if (t.kind !== 'punct' || t.value !== value) throw new GraphQLSyntaxError(`Syntax Error: Expected "${value}", found ${describe(t)}.`, t.loc);
    return t;
  }

  private expectName(): Token {
    const t = this.next();
    if (t.kind !== 'name') throw new GraphQLSyntaxError(`Syntax Error: Expected Name, found ${describe(t)}.`, t.loc);
    return t;
  }

  document(): DocumentNode {
    const operations: OperationNode[] = [];
    const fragments = new Map<string, FragmentNode>();
    while (this.peek().kind !== 'eof') {
      const t = this.peek();
      if (t.kind === 'punct' && t.value === '{') {
        operations.push({ kind: 'Operation', operation: 'query', name: null, variables: [], directives: [], selections: this.selectionSet(), loc: t.loc });
        continue;
      }
      if (t.kind === 'name' && (t.value === 'query' || t.value === 'mutation' || t.value === 'subscription')) {
        operations.push(this.operation());
        continue;
      }
      if (t.kind === 'name' && t.value === 'fragment') {
        const fragment = this.fragment();
        fragments.set(fragment.name, fragment);
        continue;
      }
      throw new GraphQLSyntaxError(`Syntax Error: Unexpected ${describe(t)}.`, t.loc);
    }
    if (operations.length === 0) throw new GraphQLSyntaxError('Syntax Error: the document defines no operation.', this.peek().loc);
    return { operations, fragments };
  }

  private operation(): OperationNode {
    const t = this.next();
    const operation = t.value as OperationNode['operation'];
    let name: string | null = null;
    if (this.peek().kind === 'name') name = this.next().value;
    const variables: VariableDefinition[] = [];
    if (this.isPunct('(')) {
      this.next();
      while (!this.isPunct(')')) {
        this.expectPunct('$');
        const varName = this.expectName().value;
        this.expectPunct(':');
        const type = this.typeRef();
        let defaultValue: ValueNode | null = null;
        if (this.isPunct('=')) {
          this.next();
          defaultValue = this.value(true);
        }
        this.directives();
        variables.push({ name: varName, type, defaultValue });
      }
      this.next();
    }
    const directives = this.directives();
    return { kind: 'Operation', operation, name, variables, directives, selections: this.selectionSet(), loc: t.loc };
  }

  private fragment(): FragmentNode {
    const t = this.next();
    const name = this.expectName().value;
    const on = this.expectName();
    if (on.value !== 'on') throw new GraphQLSyntaxError(`Syntax Error: Expected "on", found ${describe(on)}.`, on.loc);
    const typeCondition = this.expectName().value;
    this.directives();
    return { kind: 'Fragment', name, typeCondition, selections: this.selectionSet(), loc: t.loc };
  }

  private typeRef(): string {
    let text: string;
    if (this.isPunct('[')) {
      this.next();
      text = `[${this.typeRef()}]`;
      this.expectPunct(']');
    } else {
      text = this.expectName().value;
    }
    if (this.isPunct('!')) {
      this.next();
      text += '!';
    }
    return text;
  }

  private directives(): Directive[] {
    const list: Directive[] = [];
    while (this.isPunct('@')) {
      this.next();
      const name = this.expectName().value;
      list.push({ name, args: this.isPunct('(') ? this.arguments(false) : [] });
    }
    return list;
  }

  private selectionSet(): Selection[] {
    this.expectPunct('{');
    const selections: Selection[] = [];
    while (!this.isPunct('}')) {
      if (this.peek().kind === 'eof') throw new GraphQLSyntaxError('Syntax Error: Expected Name, found <EOF>.', this.peek().loc);
      selections.push(this.selection());
    }
    this.next();
    if (selections.length === 0) throw new GraphQLSyntaxError('Syntax Error: Expected Name, found "}".', this.peek().loc);
    return selections;
  }

  private selection(): Selection {
    const t = this.peek();
    if (t.kind === 'punct' && t.value === '...') {
      this.next();
      const after = this.peek();
      if (after.kind === 'name' && after.value === 'on') {
        this.next();
        const typeCondition = this.expectName().value;
        const directives = this.directives();
        return { kind: 'InlineFragment', typeCondition, directives, selections: this.selectionSet(), loc: t.loc };
      }
      if (after.kind === 'name') {
        this.next();
        return { kind: 'FragmentSpread', name: after.value, directives: this.directives(), loc: t.loc };
      }
      const directives = this.directives();
      return { kind: 'InlineFragment', typeCondition: null, directives, selections: this.selectionSet(), loc: t.loc };
    }
    const first = this.expectName();
    let alias: string | null = null;
    let name = first.value;
    if (this.isPunct(':')) {
      this.next();
      alias = first.value;
      name = this.expectName().value;
    }
    const args = this.isPunct('(') ? this.arguments(false) : [];
    const directives = this.directives();
    const selections = this.isPunct('{') ? this.selectionSet() : null;
    return { kind: 'Field', alias, name, args, directives, selections, loc: first.loc };
  }

  private arguments(constant: boolean): Argument[] {
    this.expectPunct('(');
    const args: Argument[] = [];
    while (!this.isPunct(')')) {
      const name = this.expectName().value;
      this.expectPunct(':');
      args.push({ name, value: this.value(constant) });
    }
    this.next();
    return args;
  }

  private value(constant: boolean): ValueNode {
    const t = this.next();
    switch (t.kind) {
      case 'punct':
        if (t.value === '$' && !constant) return { kind: 'Variable', name: this.expectName().value };
        if (t.value === '[') {
          const values: ValueNode[] = [];
          while (!this.isPunct(']')) values.push(this.value(constant));
          this.next();
          return { kind: 'List', values };
        }
        if (t.value === '{') {
          const fields: { name: string; value: ValueNode }[] = [];
          while (!this.isPunct('}')) {
            const name = this.expectName().value;
            this.expectPunct(':');
            fields.push({ name, value: this.value(constant) });
          }
          this.next();
          return { kind: 'Object', fields };
        }
        break;
      case 'int':
        return { kind: 'Int', value: Number(t.value) };
      case 'float':
        return { kind: 'Float', value: Number(t.value) };
      case 'string':
        return { kind: 'String', value: t.value };
      case 'name':
        if (t.value === 'true') return { kind: 'Boolean', value: true };
        if (t.value === 'false') return { kind: 'Boolean', value: false };
        if (t.value === 'null') return { kind: 'Null' };
        return { kind: 'Enum', value: t.value };
      default:
        break;
    }
    throw new GraphQLSyntaxError(`Syntax Error: Unexpected ${describe(t)}.`, t.loc);
  }
}

function describe(t: Token): string {
  if (t.kind === 'eof') return '<EOF>';
  if (t.kind === 'punct') return `"${t.value}"`;
  if (t.kind === 'string') return `String "${t.value}"`;
  return `${t.kind === 'name' ? 'Name' : t.kind === 'int' ? 'Int' : 'Float'} "${t.value}"`;
}

export function parseDocument(source: string): DocumentNode {
  return new Parser(source).document();
}
