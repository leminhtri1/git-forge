// A small GraphQL executor: a schema of object/interface/union/enum/input/scalar types with field
// resolvers, document validation (unknown fields, arguments and types are reported the way GitHub reports
// them, before anything runs), variable coercion, fragments, @include/@skip, and introspection served
// from the same type definitions (__type, __schema, __typename).

import { HttpError } from '../http.ts';
import { parseDocument, GraphQLSyntaxError, type DocumentNode, type FieldNode, type Location, type OperationNode, type Selection, type ValueNode } from './parser.ts';

// ------------------------------------------------------------------ schema description

export type Resolver = (parent: any, args: Record<string, any>, ctx: ExecContext) => unknown;

export interface ArgDef {
  type: string;
  description?: string;
  defaultValue?: unknown;
}

export interface FieldDef {
  type: string;
  description?: string;
  args?: Record<string, ArgDef>;
  resolve?: Resolver;
}

export type TypeDef =
  | { kind: 'OBJECT'; name: string; description?: string; interfaces?: string[]; fields: Record<string, FieldDef> }
  | { kind: 'INTERFACE'; name: string; description?: string; fields: Record<string, FieldDef>; possibleTypes: string[] }
  | { kind: 'UNION'; name: string; description?: string; possibleTypes: string[] }
  | { kind: 'ENUM'; name: string; description?: string; values: string[] }
  | { kind: 'INPUT_OBJECT'; name: string; description?: string; fields: Record<string, ArgDef> }
  | { kind: 'SCALAR'; name: string; description?: string };

export interface Schema {
  types: Map<string, TypeDef>;
  query: string;
  mutation: string;
}

export interface ExecContext {
  schema: Schema;
}

export interface GraphQLErrorJson {
  message: string;
  locations?: Location[];
  path?: (string | number)[];
  extensions?: Record<string, unknown>;
  type?: string;
}

/** A resolver's error that becomes an `errors` entry (with GitHub's `type`, e.g. NOT_FOUND). */
export class FieldError extends Error {
  readonly type: string;
  constructor(message: string, type = 'UNPROCESSABLE') {
    super(message);
    this.type = type;
  }
}

// ------------------------------------------------------------------ type references

type TypeRef = { kind: 'NON_NULL'; of: TypeRef } | { kind: 'LIST'; of: TypeRef } | { kind: 'NAMED'; name: string };

const refCache = new Map<string, TypeRef>();

export function parseTypeRef(text: string): TypeRef {
  const cached = refCache.get(text);
  if (cached !== undefined) return cached;
  let ref: TypeRef;
  if (text.endsWith('!')) ref = { kind: 'NON_NULL', of: parseTypeRef(text.slice(0, -1)) };
  else if (text.startsWith('[')) ref = { kind: 'LIST', of: parseTypeRef(text.slice(1, -1)) };
  else ref = { kind: 'NAMED', name: text };
  refCache.set(text, ref);
  return ref;
}

function namedType(ref: TypeRef): string {
  return ref.kind === 'NAMED' ? ref.name : namedType(ref.of);
}

// ------------------------------------------------------------------ introspection meta-types

function typeObject(schema: Schema, ref: TypeRef): Record<string, unknown> {
  return { __ref: ref, __schema: schema };
}

function introspectionTypes(): TypeDef[] {
  const fieldsOf = (t: any): any[] | null => {
    const def: TypeDef | undefined = t.__ref.kind === 'NAMED' ? t.__schema.types.get(t.__ref.name) : undefined;
    if (def === undefined || (def.kind !== 'OBJECT' && def.kind !== 'INTERFACE')) return null;
    return Object.entries(def.fields)
      .filter(([name]) => !name.startsWith('__'))
      .map(([name, field]) => ({ name, field, schema: t.__schema }));
  };
  return [
    {
      kind: 'OBJECT',
      name: '__Schema',
      fields: {
        description: { type: 'String', resolve: () => 'The git-forge GraphQL schema' },
        types: { type: '[__Type!]!', resolve: (s: Schema) => [...s.types.keys()].map((name) => typeObject(s, { kind: 'NAMED', name })) },
        queryType: { type: '__Type!', resolve: (s: Schema) => typeObject(s, { kind: 'NAMED', name: s.query }) },
        mutationType: { type: '__Type', resolve: (s: Schema) => typeObject(s, { kind: 'NAMED', name: s.mutation }) },
        subscriptionType: { type: '__Type', resolve: () => null },
        directives: { type: '[__Directive!]!', resolve: () => [{ name: 'include', locations: ['FIELD', 'FRAGMENT_SPREAD', 'INLINE_FRAGMENT'] }, { name: 'skip', locations: ['FIELD', 'FRAGMENT_SPREAD', 'INLINE_FRAGMENT'] }] },
      },
    },
    {
      kind: 'OBJECT',
      name: '__Type',
      fields: {
        kind: { type: '__TypeKind!', resolve: (t: any) => (t.__ref.kind === 'NAMED' ? (t.__schema.types.get(t.__ref.name)?.kind ?? 'SCALAR') : t.__ref.kind) },
        name: { type: 'String', resolve: (t: any) => (t.__ref.kind === 'NAMED' ? t.__ref.name : null) },
        description: { type: 'String', resolve: (t: any) => (t.__ref.kind === 'NAMED' ? (t.__schema.types.get(t.__ref.name)?.description ?? null) : null) },
        specifiedByURL: { type: 'String', resolve: () => null },
        fields: { type: '[__Field!]', args: { includeDeprecated: { type: 'Boolean', defaultValue: false } }, resolve: (t: any) => fieldsOf(t) },
        interfaces: {
          type: '[__Type!]',
          resolve: (t: any) => {
            const def = t.__ref.kind === 'NAMED' ? t.__schema.types.get(t.__ref.name) : undefined;
            if (def?.kind === 'OBJECT') return (def.interfaces ?? []).map((name: string) => typeObject(t.__schema, { kind: 'NAMED', name }));
            return def?.kind === 'INTERFACE' ? [] : null;
          },
        },
        possibleTypes: {
          type: '[__Type!]',
          resolve: (t: any) => {
            const def = t.__ref.kind === 'NAMED' ? t.__schema.types.get(t.__ref.name) : undefined;
            if (def?.kind === 'INTERFACE' || def?.kind === 'UNION') return def.possibleTypes.map((name: string) => typeObject(t.__schema, { kind: 'NAMED', name }));
            return null;
          },
        },
        enumValues: {
          type: '[__EnumValue!]',
          args: { includeDeprecated: { type: 'Boolean', defaultValue: false } },
          resolve: (t: any) => {
            const def = t.__ref.kind === 'NAMED' ? t.__schema.types.get(t.__ref.name) : undefined;
            return def?.kind === 'ENUM' ? def.values.map((name: string) => ({ name })) : null;
          },
        },
        inputFields: {
          type: '[__InputValue!]',
          args: { includeDeprecated: { type: 'Boolean', defaultValue: false } },
          resolve: (t: any) => {
            const def = t.__ref.kind === 'NAMED' ? t.__schema.types.get(t.__ref.name) : undefined;
            return def?.kind === 'INPUT_OBJECT' ? Object.entries(def.fields).map(([name, arg]) => ({ name, arg, schema: t.__schema })) : null;
          },
        },
        ofType: { type: '__Type', resolve: (t: any) => (t.__ref.kind === 'NAMED' ? null : typeObject(t.__schema, t.__ref.of)) },
        isOneOf: { type: 'Boolean', resolve: () => false },
      },
    },
    {
      kind: 'OBJECT',
      name: '__Field',
      fields: {
        name: { type: 'String!' },
        description: { type: 'String', resolve: (f: any) => f.field.description ?? null },
        args: {
          type: '[__InputValue!]!',
          args: { includeDeprecated: { type: 'Boolean', defaultValue: false } },
          resolve: (f: any) => Object.entries(f.field.args ?? {}).map(([name, arg]) => ({ name, arg, schema: f.schema })),
        },
        type: { type: '__Type!', resolve: (f: any) => typeObject(f.schema, parseTypeRef(f.field.type)) },
        isDeprecated: { type: 'Boolean!', resolve: () => false },
        deprecationReason: { type: 'String', resolve: () => null },
      },
    },
    {
      kind: 'OBJECT',
      name: '__InputValue',
      fields: {
        name: { type: 'String!' },
        description: { type: 'String', resolve: (a: any) => a.arg.description ?? null },
        type: { type: '__Type!', resolve: (a: any) => typeObject(a.schema, parseTypeRef(a.arg.type)) },
        defaultValue: { type: 'String', resolve: (a: any) => (a.arg.defaultValue === undefined ? null : JSON.stringify(a.arg.defaultValue)) },
        isDeprecated: { type: 'Boolean!', resolve: () => false },
        deprecationReason: { type: 'String', resolve: () => null },
      },
    },
    {
      kind: 'OBJECT',
      name: '__EnumValue',
      fields: {
        name: { type: 'String!' },
        description: { type: 'String', resolve: () => null },
        isDeprecated: { type: 'Boolean!', resolve: () => false },
        deprecationReason: { type: 'String', resolve: () => null },
      },
    },
    {
      kind: 'OBJECT',
      name: '__Directive',
      fields: {
        name: { type: 'String!' },
        description: { type: 'String', resolve: () => null },
        locations: { type: '[__DirectiveLocation!]!' },
        args: { type: '[__InputValue!]!', resolve: (d: any, _a, ctx) => [{ name: 'if', arg: { type: 'Boolean!' }, schema: ctx.schema }] },
        isRepeatable: { type: 'Boolean!', resolve: () => false },
      },
    },
    { kind: 'ENUM', name: '__TypeKind', values: ['SCALAR', 'OBJECT', 'INTERFACE', 'UNION', 'ENUM', 'INPUT_OBJECT', 'LIST', 'NON_NULL'] },
    { kind: 'ENUM', name: '__DirectiveLocation', values: ['QUERY', 'MUTATION', 'SUBSCRIPTION', 'FIELD', 'FRAGMENT_DEFINITION', 'FRAGMENT_SPREAD', 'INLINE_FRAGMENT', 'VARIABLE_DEFINITION'] },
  ];
}

/** Assemble a schema; adds the built-in scalars, the introspection types and Query.__type/__schema. */
export function buildSchema(types: TypeDef[], query = 'Query', mutation = 'Mutation'): Schema {
  const map = new Map<string, TypeDef>();
  for (const scalar of ['String', 'Int', 'Float', 'Boolean', 'ID']) map.set(scalar, { kind: 'SCALAR', name: scalar });
  for (const type of [...types, ...introspectionTypes()]) map.set(type.name, type);
  const schema: Schema = { types: map, query, mutation };
  const root = map.get(query);
  if (root?.kind === 'OBJECT') {
    root.fields.__type = { type: '__Type', args: { name: { type: 'String!' } }, resolve: (_p, args) => (map.has(args.name) ? typeObject(schema, { kind: 'NAMED', name: args.name }) : null) };
    root.fields.__schema = { type: '__Schema!', resolve: () => schema };
  }
  return schema;
}

// ------------------------------------------------------------------ validation

function errorAt(message: string, loc: Location | undefined, path: (string | number)[] | undefined, extensions?: Record<string, unknown>): GraphQLErrorJson {
  const error: GraphQLErrorJson = { message };
  if (loc !== undefined) error.locations = [loc];
  if (path !== undefined) error.path = path;
  if (extensions !== undefined) error.extensions = extensions;
  return error;
}

function fieldsOfType(def: TypeDef | undefined): Record<string, FieldDef> | null {
  if (def === undefined) return null;
  if (def.kind === 'OBJECT' || def.kind === 'INTERFACE') return def.fields;
  if (def.kind === 'UNION') return {};
  return null;
}

function validate(schema: Schema, doc: DocumentNode, op: OperationNode): GraphQLErrorJson[] {
  const errors: GraphQLErrorJson[] = [];
  const opName = op.operation === 'mutation' ? 'mutation' : 'query';
  const rootName = op.operation === 'mutation' ? schema.mutation : schema.query;
  const visiting = new Set<string>();
  const walk = (typeName: string, selections: Selection[], path: string[]): void => {
    const def = schema.types.get(typeName);
    const fields = fieldsOfType(def);
    for (const selection of selections) {
      if (selection.kind === 'Field') {
        if (selection.name === '__typename') {
          if (selection.selections !== null) errors.push(errorAt(`Selections can't be made on scalars (field '__typename' returns String but has selections [...])`, selection.loc, [...path, selection.alias ?? selection.name]));
          continue;
        }
        const field = fields?.[selection.name];
        const fieldPath = [...path, selection.alias ?? selection.name];
        if (field === undefined) {
          errors.push(
            errorAt(`Field '${selection.name}' doesn't exist on type '${typeName}'`, selection.loc, fieldPath, {
              code: 'undefinedField',
              typeName,
              fieldName: selection.name,
            }),
          );
          continue;
        }
        for (const arg of selection.args) {
          if (field.args?.[arg.name] === undefined) {
            errors.push(errorAt(`Field '${selection.name}' doesn't accept argument '${arg.name}'`, selection.loc, fieldPath, { code: 'argumentNotAccepted', name: selection.name, typeName: 'Field', argumentName: arg.name }));
          }
        }
        for (const [argName, arg] of Object.entries(field.args ?? {})) {
          if (arg.type.endsWith('!') && arg.defaultValue === undefined && !selection.args.some((a) => a.name === argName)) {
            errors.push(errorAt(`Field '${selection.name}' is missing required arguments: ${argName}`, selection.loc, fieldPath, { code: 'missingRequiredArguments', className: 'Field', name: selection.name, arguments: argName }));
          }
        }
        const target = schema.types.get(namedType(parseTypeRef(field.type)));
        const composite = target !== undefined && (target.kind === 'OBJECT' || target.kind === 'INTERFACE' || target.kind === 'UNION');
        if (composite && selection.selections === null) {
          errors.push(errorAt(`Field must have selections (field '${selection.name}' returns ${target.name} but has no selections. Did you mean '${selection.name} { ... }'?)`, selection.loc, fieldPath, { code: 'selectionMismatch', nodeName: `field '${selection.name}'`, typeName: target.name }));
        } else if (!composite && selection.selections !== null) {
          errors.push(errorAt(`Selections can't be made on scalars (field '${selection.name}' returns ${namedType(parseTypeRef(field.type))} but has selections [...])`, selection.loc, fieldPath, { code: 'selectionMismatch' }));
        } else if (composite && selection.selections !== null) {
          walk(target.name, selection.selections, fieldPath);
        }
        continue;
      }
      if (selection.kind === 'InlineFragment') {
        const condition = selection.typeCondition ?? typeName;
        if (!schema.types.has(condition)) {
          errors.push(errorAt(`No such type ${condition}, so it can't be a fragment condition`, selection.loc, [...path, `... on ${condition}`], { code: 'undefinedType', typeName: condition }));
          continue;
        }
        walk(condition, selection.selections, [...path, `... on ${condition}`]);
        continue;
      }
      const fragment = doc.fragments.get(selection.name);
      if (fragment === undefined) {
        errors.push(errorAt(`Fragment ${selection.name} was used, but not defined`, selection.loc, [...path, `... ${selection.name}`], { code: 'useAndDefineFragment', fragmentName: selection.name }));
        continue;
      }
      if (visiting.has(fragment.name)) {
        errors.push(errorAt(`Fragment ${fragment.name} contains an infinite loop`, selection.loc, path));
        continue;
      }
      if (!schema.types.has(fragment.typeCondition)) {
        errors.push(errorAt(`No such type ${fragment.typeCondition}, so it can't be a fragment condition`, fragment.loc, [`fragment ${fragment.name}`]));
        continue;
      }
      visiting.add(fragment.name);
      walk(fragment.typeCondition, fragment.selections, [`fragment ${fragment.name}`]);
      visiting.delete(fragment.name);
    }
  };
  const rootPath = op.name === null ? [opName] : [`${opName} ${op.name}`];
  if (!schema.types.has(rootName)) errors.push(errorAt(`Schema is not configured for ${opName}s`, op.loc, rootPath));
  else walk(rootName, op.selections, rootPath);
  for (const variable of op.variables) {
    const named = namedType(parseTypeRef(variable.type));
    const def = schema.types.get(named);
    if (def === undefined) errors.push(errorAt(`${named} isn't a defined input type (on $${variable.name})`, op.loc, rootPath, { code: 'variableRequiresValidType', typeName: named, variableName: variable.name }));
    else if (def.kind !== 'SCALAR' && def.kind !== 'ENUM' && def.kind !== 'INPUT_OBJECT') errors.push(errorAt(`${named} isn't a valid input type (on $${variable.name})`, op.loc, rootPath));
  }
  return errors;
}

// ------------------------------------------------------------------ values and arguments

class InputError extends Error {}

function coerceInput(schema: Schema, ref: TypeRef, value: unknown, where: string): unknown {
  if (ref.kind === 'NON_NULL') {
    if (value === null || value === undefined) throw new InputError(`${where} expected a value of type ${typeText(ref)}, got null`);
    return coerceInput(schema, ref.of, value, where);
  }
  if (value === null || value === undefined) return value ?? null;
  if (ref.kind === 'LIST') {
    const list = Array.isArray(value) ? value : [value];
    return list.map((item, i) => coerceInput(schema, ref.of, item, `${where}[${i}]`));
  }
  const def = schema.types.get(ref.name);
  if (def === undefined) return value;
  switch (def.kind) {
    case 'SCALAR':
      if (def.name === 'Int') {
        if (typeof value !== 'number' || !Number.isInteger(value)) throw new InputError(`${where} could not coerce ${JSON.stringify(value)} to Int`);
      } else if (def.name === 'Float') {
        if (typeof value !== 'number') throw new InputError(`${where} could not coerce ${JSON.stringify(value)} to Float`);
      } else if (def.name === 'Boolean') {
        if (typeof value !== 'boolean') throw new InputError(`${where} could not coerce ${JSON.stringify(value)} to Boolean`);
      } else if (def.name === 'ID') {
        if (typeof value !== 'string' && typeof value !== 'number') throw new InputError(`${where} could not coerce ${JSON.stringify(value)} to ID`);
        return String(value);
      } else if (def.name === 'String') {
        if (typeof value !== 'string') throw new InputError(`${where} could not coerce ${JSON.stringify(value)} to String`);
      }
      return value;
    case 'ENUM':
      if (typeof value !== 'string' || !def.values.includes(value)) throw new InputError(`${where} expected one of ${def.name} (${def.values.join(', ')}), got ${JSON.stringify(value)}`);
      return value;
    case 'INPUT_OBJECT': {
      if (typeof value !== 'object' || Array.isArray(value)) throw new InputError(`${where} expected an input object ${def.name}`);
      const input = value as Record<string, unknown>;
      const out: Record<string, unknown> = {};
      for (const key of Object.keys(input)) {
        if (def.fields[key] === undefined) throw new InputError(`${where} has the field ${key}, which ${def.name} does not define`);
      }
      for (const [key, field] of Object.entries(def.fields)) {
        const given = key in input ? input[key] : field.defaultValue;
        const coerced = coerceInput(schema, parseTypeRef(field.type), given, `${where}.${key}`);
        if (coerced !== undefined) out[key] = coerced;
      }
      return out;
    }
    default:
      return value;
  }
}

function typeText(ref: TypeRef): string {
  return ref.kind === 'NON_NULL' ? `${typeText(ref.of)}!` : ref.kind === 'LIST' ? `[${typeText(ref.of)}]` : ref.name;
}

function literal(node: ValueNode, variables: Record<string, unknown>): unknown {
  switch (node.kind) {
    case 'Variable':
      return variables[node.name];
    case 'Int':
    case 'Float':
    case 'String':
    case 'Boolean':
      return node.value;
    case 'Null':
      return null;
    case 'Enum':
      return node.value;
    case 'List':
      return node.values.map((v) => literal(v, variables));
    case 'Object': {
      const out: Record<string, unknown> = {};
      for (const field of node.fields) {
        const v = literal(field.value, variables);
        if (!(field.value.kind === 'Variable' && v === undefined)) out[field.name] = v;
      }
      return out;
    }
  }
}

function argumentsOf(schema: Schema, field: FieldDef, node: FieldNode, variables: Record<string, unknown>): Record<string, any> {
  const args: Record<string, any> = {};
  for (const [name, def] of Object.entries(field.args ?? {})) {
    const given = node.args.find((a) => a.name === name);
    let value: unknown = given === undefined ? undefined : literal(given.value, variables);
    if (value === undefined) value = def.defaultValue;
    const coerced = coerceInput(schema, parseTypeRef(def.type), value, `Argument '${name}' on Field '${node.name}'`);
    if (coerced !== undefined) args[name] = coerced;
  }
  return args;
}

function skipped(directives: { name: string; args: { name: string; value: ValueNode }[] }[], variables: Record<string, unknown>): boolean {
  for (const directive of directives) {
    const ifArg = directive.args.find((a) => a.name === 'if');
    const value = ifArg === undefined ? undefined : literal(ifArg.value, variables);
    if (directive.name === 'skip' && value === true) return true;
    if (directive.name === 'include' && value === false) return true;
  }
  return false;
}

// ------------------------------------------------------------------ execution

interface RunState {
  schema: Schema;
  doc: DocumentNode;
  variables: Record<string, unknown>;
  errors: GraphQLErrorJson[];
  ctx: ExecContext;
}

function typeMatches(schema: Schema, condition: string | null, runtime: string): boolean {
  if (condition === null || condition === runtime) return true;
  const def = schema.types.get(condition);
  return (def?.kind === 'INTERFACE' || def?.kind === 'UNION') && def.possibleTypes.includes(runtime);
}

/** Collect the fields that apply to `runtime`, grouped by response key in document order. */
function collectFields(state: RunState, runtime: string, selections: Selection[], into = new Map<string, FieldNode[]>()): Map<string, FieldNode[]> {
  for (const selection of selections) {
    if (skipped(selection.directives, state.variables)) continue;
    if (selection.kind === 'Field') {
      const key = selection.alias ?? selection.name;
      const list = into.get(key);
      if (list === undefined) into.set(key, [selection]);
      else list.push(selection);
    } else if (selection.kind === 'InlineFragment') {
      if (typeMatches(state.schema, selection.typeCondition, runtime)) collectFields(state, runtime, selection.selections, into);
    } else {
      const fragment = state.doc.fragments.get(selection.name);
      if (fragment !== undefined && typeMatches(state.schema, fragment.typeCondition, runtime)) collectFields(state, runtime, fragment.selections, into);
    }
  }
  return into;
}

function runtimeTypeOf(schema: Schema, declared: string, value: any): string {
  const def = schema.types.get(declared);
  if (def?.kind === 'INTERFACE' || def?.kind === 'UNION') return typeof value?.__typename === 'string' ? value.__typename : declared;
  return declared;
}

function executeObject(state: RunState, typeName: string, parent: unknown, selections: Selection[], path: (string | number)[]): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  const def = state.schema.types.get(typeName);
  const fields = fieldsOfType(def) ?? {};
  for (const [key, nodes] of collectFields(state, typeName, selections)) {
    const node = nodes[0]!;
    if (node.name === '__typename') {
      out[key] = typeName;
      continue;
    }
    const field = fields[node.name];
    if (field === undefined) continue;
    const fieldPath = [...path, key];
    let value: unknown;
    try {
      const args = argumentsOf(state.schema, field, node, state.variables);
      value = field.resolve !== undefined ? field.resolve(parent, args, state.ctx) : (parent as Record<string, unknown> | null)?.[node.name];
    } catch (err) {
      state.errors.push(fieldError(err, node, fieldPath));
      out[key] = null;
      continue;
    }
    const merged = nodes.flatMap((n) => n.selections ?? []);
    out[key] = complete(state, parseTypeRef(field.type), value, merged, fieldPath, node);
  }
  return out;
}

function fieldError(err: unknown, node: FieldNode, path: (string | number)[]): GraphQLErrorJson {
  if (err instanceof FieldError) return { type: err.type, path, locations: [node.loc], message: err.message };
  if (err instanceof HttpError) {
    const type = err.status === 404 ? 'NOT_FOUND' : err.status === 403 ? 'FORBIDDEN' : 'UNPROCESSABLE';
    const detail = Array.isArray(err.extra?.errors) ? (err.extra!.errors as { message?: string }[]).map((e) => e.message).filter(Boolean).join('; ') : '';
    return { type, path, locations: [node.loc], message: detail !== '' ? detail : err.message };
  }
  if (err instanceof InputError) return { path, locations: [node.loc], message: err.message, extensions: { code: 'argumentLiteralsIncompatible' } };
  return { path, locations: [node.loc], message: err instanceof Error ? err.message : String(err) };
}

function complete(state: RunState, ref: TypeRef, value: unknown, selections: Selection[], path: (string | number)[], node: FieldNode): unknown {
  if (ref.kind === 'NON_NULL') return complete(state, ref.of, value, selections, path, node);
  if (value === null || value === undefined) return null;
  if (ref.kind === 'LIST') {
    const list = Array.isArray(value) ? value : [value];
    return list.map((item, i) => complete(state, ref.of, item, selections, [...path, i], node));
  }
  const def = state.schema.types.get(ref.name);
  if (def === undefined || def.kind === 'SCALAR' || def.kind === 'ENUM') return value;
  const runtime = runtimeTypeOf(state.schema, ref.name, value);
  return executeObject(state, runtime, value, selections, path);
}

function runOperation(schema: Schema, request: { query?: unknown; variables?: unknown; operationName?: unknown }): Record<string, unknown> {
  if (typeof request.query !== 'string' || request.query.trim() === '') {
    return { errors: [{ message: 'A query attribute must be specified and must be a string.' }] };
  }
  let doc: DocumentNode;
  try {
    doc = parseDocument(request.query);
  } catch (err) {
    if (err instanceof GraphQLSyntaxError) return { errors: [{ message: err.message, locations: [err.loc] }] };
    throw err;
  }
  const opName = typeof request.operationName === 'string' && request.operationName !== '' ? request.operationName : null;
  let op: OperationNode | undefined;
  if (opName !== null) op = doc.operations.find((o) => o.name === opName);
  else if (doc.operations.length === 1) op = doc.operations[0];
  if (op === undefined) {
    return { errors: [{ message: opName === null ? 'An operation name is required' : `No operation named "${opName}"` }] };
  }
  const errors = validate(schema, doc, op);
  if (errors.length > 0) return { errors };

  const provided = request.variables !== null && typeof request.variables === 'object' && !Array.isArray(request.variables) ? (request.variables as Record<string, unknown>) : {};
  const variables: Record<string, unknown> = {};
  for (const definition of op.variables) {
    let value = definition.name in provided ? provided[definition.name] : undefined;
    if (value === undefined && definition.defaultValue !== null) value = literal(definition.defaultValue, {});
    try {
      const coerced = coerceInput(schema, parseTypeRef(definition.type), value, `Variable $${definition.name} of type ${definition.type}`);
      if (coerced !== undefined) variables[definition.name] = coerced;
    } catch (err) {
      return { errors: [{ message: err instanceof Error ? `${err.message} (was provided invalid value)` : String(err), locations: [op.loc], extensions: { value, problems: [] } }] };
    }
  }
  const state: RunState = { schema, doc, variables, errors: [], ctx: { schema } };
  const root = op.operation === 'mutation' ? schema.mutation : schema.query;
  const data = executeObject(state, root, {}, op.selections, []);
  return state.errors.length > 0 ? { data, errors: state.errors } : { data };
}

/** Execute one request body: an operation object, or an array of them (batching). */
export function executeBody(schema: Schema, body: unknown): unknown {
  if (Array.isArray(body)) return body.map((item) => (item !== null && typeof item === 'object' ? runOperation(schema, item) : { errors: [{ message: 'Each batched operation must be an object' }] }));
  if (body !== null && typeof body === 'object') return runOperation(schema, body as Record<string, unknown>);
  return { errors: [{ message: 'The request body must be a JSON object' }] };
}
