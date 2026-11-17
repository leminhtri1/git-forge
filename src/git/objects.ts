// Git's object model: loose-object hashing and the commit/tree/tag formats, built and parsed by hand.

import { createHash } from 'node:crypto';

export type ObjectType = 'commit' | 'tree' | 'blob' | 'tag';

export interface GitObject {
  readonly type: ObjectType;
  readonly data: Buffer;
}

export const ZERO_ID = '0000000000000000000000000000000000000000';
export const EMPTY_TREE_ID = '4b825dc642cb6eb9a060e54bf8d69288fbee4904';

/** SHA-1 of "<type> <size>\0<data>": the object's id. */
export function hashObject(type: ObjectType, data: Buffer): string {
  const hash = createHash('sha1');
  hash.update(`${type} ${data.length}\0`);
  hash.update(data);
  return hash.digest('hex');
}

export function isObjectId(value: unknown): value is string {
  return typeof value === 'string' && /^[0-9a-f]{40}$/.test(value);
}

// ------------------------------------------------------------------ trees

export interface TreeEntry {
  readonly mode: string; // '100644', '100755', '40000', '120000', '160000'
  readonly name: string;
  readonly id: string;
}

export function isTreeMode(mode: string): boolean {
  return mode === '40000' || mode === '040000';
}

export function parseTree(data: Buffer): TreeEntry[] {
  const entries: TreeEntry[] = [];
  let at = 0;
  while (at < data.length) {
    const space = data.indexOf(0x20, at);
    const nul = data.indexOf(0x00, space + 1);
    if (space < 0 || nul < 0 || nul + 21 > data.length) throw new Error('malformed tree object');
    const mode = data.toString('ascii', at, space);
    const name = data.toString('utf8', space + 1, nul);
    const id = data.toString('hex', nul + 1, nul + 21);
    entries.push({ mode, name, id });
    at = nul + 21;
  }
  return entries;
}

/** Git's tree order: byte order of names, where a directory sorts as if its name ended in '/'. */
function treeSortKey(entry: TreeEntry): string {
  return isTreeMode(entry.mode) ? `${entry.name}/` : entry.name;
}

export function buildTree(entries: readonly TreeEntry[]): Buffer {
  const sorted = [...entries].sort((a, b) => {
    const ka = Buffer.from(treeSortKey(a), 'utf8');
    const kb = Buffer.from(treeSortKey(b), 'utf8');
    return Buffer.compare(ka, kb);
  });
  const parts: Buffer[] = [];
  for (const entry of sorted) {
    const mode = isTreeMode(entry.mode) ? '40000' : entry.mode;
    parts.push(Buffer.from(`${mode} ${entry.name}\0`, 'utf8'), Buffer.from(entry.id, 'hex'));
  }
  return Buffer.concat(parts);
}

// ------------------------------------------------------------------ commits

export interface Commit {
  readonly tree: string;
  readonly parents: readonly string[];
  readonly author: string; // "Name <email> 1700000000 +0000"
  readonly committer: string;
  readonly message: string;
}

export function parseCommit(data: Buffer): Commit {
  const text = data.toString('utf8');
  const split = text.indexOf('\n\n');
  const head = split < 0 ? text : text.slice(0, split);
  const message = split < 0 ? '' : text.slice(split + 2);
  let tree = '';
  const parents: string[] = [];
  let author = '';
  let committer = '';
  for (const line of head.split('\n')) {
    if (line.startsWith('tree ')) tree = line.slice(5);
    else if (line.startsWith('parent ')) parents.push(line.slice(7));
    else if (line.startsWith('author ')) author = line.slice(7);
    else if (line.startsWith('committer ')) committer = line.slice(10);
  }
  return { tree, parents, author, committer, message };
}

export function buildCommit(commit: Commit): Buffer {
  let text = `tree ${commit.tree}\n`;
  for (const parent of commit.parents) text += `parent ${parent}\n`;
  text += `author ${commit.author}\ncommitter ${commit.committer}\n\n${commit.message}`;
  return Buffer.from(text, 'utf8');
}

/** "Name <email> 1700000000 +0000" -> its parts. */
export function parseSignature(sig: string): { name: string; email: string; time: number; tz: string } {
  const match = /^(.*?) <([^>]*)> (-?\d+) ([+-]\d{4})$/.exec(sig);
  if (match === null) return { name: sig, email: '', time: 0, tz: '+0000' };
  return { name: match[1]!, email: match[2]!, time: Number(match[3]), tz: match[4]! };
}

export function signature(name: string, email: string, time: number): string {
  return `${name} <${email}> ${time} +0000`;
}

/** An ISO timestamp from a signature, honouring its timezone offset. */
export function signatureDate(sig: string): string {
  const parsed = parseSignature(sig);
  const sign = parsed.tz.startsWith('-') ? -1 : 1;
  const hours = Number(parsed.tz.slice(1, 3));
  const minutes = Number(parsed.tz.slice(3, 5));
  const offset = sign * (hours * 60 + minutes);
  const local = new Date((parsed.time + offset * 60) * 1000).toISOString().replace(/\.\d{3}Z$/, '');
  if (offset === 0) return `${local}Z`;
  return `${local}${parsed.tz.slice(0, 3)}:${parsed.tz.slice(3)}`;
}

// ------------------------------------------------------------------ tags

export interface Tag {
  readonly object: string;
  readonly type: string;
  readonly tag: string;
}

export function parseTag(data: Buffer): Tag {
  const text = data.toString('utf8');
  const head = text.split('\n\n', 1)[0] ?? '';
  let object = '';
  let type = '';
  let tag = '';
  for (const line of head.split('\n')) {
    if (line.startsWith('object ')) object = line.slice(7);
    else if (line.startsWith('type ')) type = line.slice(5);
    else if (line.startsWith('tag ')) tag = line.slice(4);
  }
  return { object, type, tag };
}
