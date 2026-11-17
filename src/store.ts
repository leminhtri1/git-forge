// The forge's whole state, in memory: repositories with their git objects and refs, and the issue
// tracker (labels, issues, comments, pull requests) beside them. Ids and timestamps come from counters,
// never the clock, so the same sequence of requests always builds the same state.

import { config, isoAt } from './config.ts';
import {
  buildCommit,
  buildTree,
  hashObject,
  isTreeMode,
  parseCommit,
  parseTag,
  parseTree,
  signature,
  type Commit,
  type GitObject,
  type ObjectType,
  type TreeEntry,
} from './git/objects.ts';

// ------------------------------------------------------------------ entities

export interface Label {
  id: number;
  name: string;
  color: string;
  description: string | null;
}

export interface Comment {
  id: number;
  body: string;
  createdAt: string;
  updatedAt: string;
}

export interface Issue {
  id: number;
  number: number;
  title: string;
  body: string | null;
  state: 'open' | 'closed';
  stateReason: string | null;
  labels: string[]; // label keys (lowercase names)
  comments: Comment[];
  seq: number;
  createdAt: string;
  updatedAt: string;
  closedAt: string | null;
  isPull: boolean;
}

export interface Pull {
  id: number;
  number: number;
  issue: Issue; // shared title/body/state/comments, as GitHub models it
  headRef: string; // branch name
  baseRef: string;
  merged: boolean;
  mergedAt: string | null;
  mergeCommitSha: string | null;
  /** Tips frozen when the pull closes; open pulls read their branches live. */
  closedHeadSha: string | null;
  closedBaseSha: string | null;
  draft: boolean;
}

let seqCounter = 0;
/** A monotonically increasing sequence number and its timestamp. */
export function tick(): { seq: number; iso: string } {
  seqCounter += 1;
  return { seq: seqCounter, iso: isoAt(seqCounter) };
}

const ids = { repo: 0, issue: 0, pull: 0, comment: 0, label: 0 };
export function nextId(kind: keyof typeof ids): number {
  ids[kind] += 1;
  return ids[kind];
}

// ------------------------------------------------------------------ repositories

export interface FlatEntry {
  readonly mode: string;
  readonly id: string;
}

export class Repo {
  readonly id: number;
  readonly owner: string;
  name: string;
  description: string | null;
  homepage: string | null;
  isPrivate: boolean;
  defaultBranch: string;
  readonly createdAt: string;
  updatedAt: string;
  pushedAt: string | null;
  readonly seq: number;

  readonly objects = new Map<string, GitObject>();
  readonly refs = new Map<string, string>();
  /** Bumped on every ref change: keys the fetch response cache. */
  refsVersion = 0;

  readonly labels = new Map<string, Label>();
  readonly issues = new Map<number, Issue>();
  readonly pulls = new Map<number, Pull>();
  nextNumber = 1;

  constructor(owner: string, name: string, description: string | null, isPrivate: boolean) {
    const t = tick();
    this.id = nextId('repo');
    this.owner = owner;
    this.name = name;
    this.description = description;
    this.homepage = null;
    this.isPrivate = isPrivate;
    this.defaultBranch = config.defaultBranch;
    this.createdAt = t.iso;
    this.updatedAt = t.iso;
    this.pushedAt = null;
    this.seq = t.seq;
  }

  get fullName(): string {
    return `${this.owner}/${this.name}`;
  }

  get headRefName(): string {
    return `refs/heads/${this.defaultBranch}`;
  }

  isEmpty(): boolean {
    for (const name of this.refs.keys()) if (name.startsWith('refs/heads/')) return false;
    return true;
  }

  // ---- objects

  get(id: string): GitObject | undefined {
    return this.objects.get(id);
  }

  put(type: ObjectType, data: Buffer): string {
    const id = hashObject(type, data);
    if (!this.objects.has(id)) this.objects.set(id, { type, data });
    return id;
  }

  commit(id: string): Commit | undefined {
    const object = this.objects.get(id);
    return object !== undefined && object.type === 'commit' ? parseCommit(object.data) : undefined;
  }

  tree(id: string): TreeEntry[] | undefined {
    const object = this.objects.get(id);
    return object !== undefined && object.type === 'tree' ? parseTree(object.data) : undefined;
  }

  /** Follow tag objects down to the first non-tag object id. */
  peel(id: string): string {
    let current = id;
    for (let i = 0; i < 100; i++) {
      const object = this.objects.get(current);
      if (object === undefined || object.type !== 'tag') return current;
      current = parseTag(object.data).object;
    }
    return current;
  }

  // ---- refs

  setRef(name: string, id: string | null): void {
    if (id === null) this.refs.delete(name);
    else this.refs.set(name, id);
    this.refsVersion += 1;
    const t = tick();
    this.pushedAt = t.iso;
    this.updatedAt = t.iso;
  }

  branchTip(branch: string): string | undefined {
    return this.refs.get(`refs/heads/${branch}`);
  }

  /** A branch, tag, full ref name or commit id -> commit id. */
  resolve(rev: string | undefined | null): string | undefined {
    if (rev === undefined || rev === null || rev === '') return this.branchTip(this.defaultBranch);
    const direct = this.refs.get(rev) ?? this.refs.get(`refs/heads/${rev}`) ?? this.refs.get(`refs/tags/${rev}`);
    if (direct !== undefined) return this.peel(direct);
    if (/^[0-9a-f]{40}$/.test(rev) && this.objects.has(rev)) return this.peel(rev);
    if (/^[0-9a-f]{4,39}$/.test(rev)) {
      for (const id of this.objects.keys()) if (id.startsWith(rev)) return this.peel(id);
    }
    return undefined;
  }

  // ---- trees as flat path maps

  flatten(treeId: string, prefix = '', out = new Map<string, FlatEntry>()): Map<string, FlatEntry> {
    const entries = this.tree(treeId) ?? [];
    for (const entry of entries) {
      const path = prefix === '' ? entry.name : `${prefix}/${entry.name}`;
      if (isTreeMode(entry.mode)) this.flatten(entry.id, path, out);
      else out.set(path, { mode: entry.mode, id: entry.id });
    }
    return out;
  }

  /** Build (and store) the nested trees for a flat path map; returns the root tree id. */
  writeFlat(files: ReadonlyMap<string, FlatEntry>): string {
    interface Dir {
      files: TreeEntry[];
      dirs: Map<string, Dir>;
    }
    const root: Dir = { files: [], dirs: new Map() };
    for (const [path, entry] of files) {
      const parts = path.split('/');
      let dir = root;
      for (let i = 0; i < parts.length - 1; i++) {
        let next = dir.dirs.get(parts[i]!);
        if (next === undefined) {
          next = { files: [], dirs: new Map() };
          dir.dirs.set(parts[i]!, next);
        }
        dir = next;
      }
      dir.files.push({ mode: entry.mode, name: parts[parts.length - 1]!, id: entry.id });
    }
    const write = (dir: Dir): string => {
      const entries: TreeEntry[] = [...dir.files];
      for (const [name, child] of dir.dirs) entries.push({ mode: '40000', name, id: write(child) });
      return this.put('tree', buildTree(entries));
    };
    return write(root);
  }

  /** The tree entry at `path` under a root tree ('' is the root itself). */
  entryAt(rootTree: string, path: string): { mode: string; id: string } | undefined {
    const parts = path.split('/').filter((p) => p !== '');
    let current: { mode: string; id: string } = { mode: '40000', id: rootTree };
    for (const part of parts) {
      if (!isTreeMode(current.mode)) return undefined;
      const entries = this.tree(current.id);
      const found = entries?.find((e) => e.name === part);
      if (found === undefined) return undefined;
      current = { mode: found.mode, id: found.id };
    }
    return current;
  }

  // ---- commits

  /** Make a commit as the viewer at the next tick. */
  makeCommit(tree: string, parents: readonly string[], message: string): string {
    const t = tick();
    const sig = signature(config.viewer.name, config.viewer.email, config.nowSec + t.seq);
    const body = message.endsWith('\n') ? message : `${message}\n`;
    return this.put('commit', buildCommit({ tree, parents, author: sig, committer: sig, message: body }));
  }

  /** Whether `ancestor` is reachable from `descendant` (a commit is its own ancestor). */
  isAncestor(ancestor: string, descendant: string): boolean {
    if (ancestor === descendant) return true;
    const seen = new Set<string>([descendant]);
    const queue = [descendant];
    while (queue.length > 0) {
      const id = queue.shift()!;
      const commit = this.commit(id);
      if (commit === undefined) continue;
      for (const parent of commit.parents) {
        if (parent === ancestor) return true;
        if (!seen.has(parent)) {
          seen.add(parent);
          queue.push(parent);
        }
      }
    }
    return false;
  }

  /** A best common ancestor of two commits (nearest to `b` in breadth-first order), or undefined. */
  mergeBase(a: string, b: string): string | undefined {
    const ancestors = new Set<string>();
    const queue = [a];
    while (queue.length > 0) {
      const id = queue.shift()!;
      if (ancestors.has(id)) continue;
      ancestors.add(id);
      const commit = this.commit(id);
      if (commit !== undefined) queue.push(...commit.parents);
    }
    const seen = new Set<string>();
    const walk = [b];
    while (walk.length > 0) {
      const id = walk.shift()!;
      if (seen.has(id)) continue;
      seen.add(id);
      if (ancestors.has(id)) return id;
      const commit = this.commit(id);
      if (commit !== undefined) walk.push(...commit.parents);
    }
    return undefined;
  }

  /** Commits reachable from `tip`, newest first by committer time (ties: discovery order). */
  log(tip: string, limit = Infinity, stopAt?: Set<string>): string[] {
    const out: string[] = [];
    const seen = new Set<string>();
    const pending: { id: string; time: number; order: number }[] = [];
    let order = 0;
    const push = (id: string): void => {
      if (seen.has(id) || stopAt?.has(id)) return;
      seen.add(id);
      const commit = this.commit(id);
      if (commit === undefined) return;
      const time = Number(/ (\d+) [+-]\d{4}$/.exec(commit.committer)?.[1] ?? 0);
      pending.push({ id, time, order: order++ });
    };
    push(tip);
    while (pending.length > 0 && out.length < limit) {
      let best = 0;
      for (let i = 1; i < pending.length; i++) {
        const p = pending[i]!;
        const q = pending[best]!;
        if (p.time > q.time || (p.time === q.time && p.order < q.order)) best = i;
      }
      const next = pending.splice(best, 1)[0]!;
      out.push(next.id);
      for (const parent of this.commit(next.id)?.parents ?? []) push(parent);
    }
    return out;
  }
}

// ------------------------------------------------------------------ the store

export class Store {
  private readonly repos = new Map<string, Repo>();

  private key(owner: string, name: string): string {
    return `${owner.toLowerCase()}/${name.toLowerCase()}`;
  }

  find(owner: string, name: string): Repo | undefined {
    const bare = name.endsWith('.git') ? name.slice(0, -4) : name;
    return this.repos.get(this.key(owner, bare));
  }

  create(owner: string, name: string, description: string | null, isPrivate: boolean): Repo {
    const repo = new Repo(owner, name, description, isPrivate);
    this.repos.set(this.key(owner, name), repo);
    return repo;
  }

  delete(repo: Repo): void {
    this.repos.delete(this.key(repo.owner, repo.name));
  }

  rename(repo: Repo, name: string): void {
    this.repos.delete(this.key(repo.owner, repo.name));
    repo.name = name;
    this.repos.set(this.key(repo.owner, name), repo);
  }

  /** The owner's repositories, oldest first. */
  list(owner: string): Repo[] {
    const lower = owner.toLowerCase();
    return [...this.repos.values()].filter((repo) => repo.owner.toLowerCase() === lower).sort((a, b) => a.seq - b.seq);
  }

  byId(id: number): Repo | undefined {
    for (const repo of this.repos.values()) if (repo.id === id) return repo;
    return undefined;
  }

  all(): Iterable<Repo> {
    return this.repos.values();
  }
}

export const store = new Store();

/** Whether `owner` is the account this forge serves (logins compare case-insensitively). */
export function isViewer(owner: string): boolean {
  return owner.toLowerCase() === config.viewer.login.toLowerCase();
}

/** The repository at owner/name if the viewer may see it (another owner's name is simply not found). */
export function lookupRepo(owner: string, name: string): Repo | undefined {
  if (!isViewer(owner)) return undefined;
  return store.find(owner, name);
}
