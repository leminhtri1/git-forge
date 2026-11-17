// Domain operations shared by REST, GraphQL and MCP. Each validates its input the way GitHub's REST API
// does and throws HttpError with GitHub's status codes, so every surface reports the same outcome.

import { config } from './config.ts';
import { HttpError } from './http.ts';
import { ZERO_ID } from './git/objects.ts';
import { lookupRepo, nextId, store, tick, type FlatEntry, type Issue, type Label, type Pull, type Repo } from './store.ts';

const REPO_NAME = /^[A-Za-z0-9._-]{1,100}$/;

function validation(message: string, resource: string, field: string, code = 'invalid'): HttpError {
  return new HttpError(422, message, { errors: [{ resource, field, code, message }] });
}

// ------------------------------------------------------------------ repositories

export function requireRepo(owner: string, name: string): Repo {
  const repo = lookupRepo(owner, name);
  if (repo === undefined) throw new HttpError(404, 'Not Found');
  return repo;
}

export interface RepoInput {
  name?: unknown;
  description?: unknown;
  private?: unknown;
  visibility?: unknown;
  homepage?: unknown;
  auto_init?: unknown;
}

export function createRepository(input: RepoInput): Repo {
  const name = typeof input.name === 'string' ? input.name.trim() : '';
  if (name === '' || !REPO_NAME.test(name) || name === '.' || name === '..') {
    throw validation('Repository creation failed.', 'Repository', 'name', name === '' ? 'missing_field' : 'invalid');
  }
  if (store.find(config.viewer.login, name) !== undefined) {
    throw new HttpError(422, 'Repository creation failed.', {
      errors: [{ resource: 'Repository', code: 'custom', field: 'name', message: 'name already exists on this account' }],
    });
  }
  const isPrivate = input.private === true || input.visibility === 'private' || input.visibility === 'internal';
  const description = typeof input.description === 'string' ? input.description : null;
  const repo = store.create(config.viewer.login, name, description, isPrivate);
  if (typeof input.homepage === 'string') repo.homepage = input.homepage;
  if (input.auto_init === true) {
    const blob = repo.put('blob', Buffer.from(`# ${name}\n${description ? `\n${description}\n` : ''}`, 'utf8'));
    const tree = repo.writeFlat(new Map([['README.md', { mode: '100644', id: blob }]]));
    repo.setRef(repo.headRefName, repo.makeCommit(tree, [], 'Initial commit'));
  }
  return repo;
}

export function updateRepository(repo: Repo, input: Record<string, unknown>): Repo {
  if (input.name !== undefined) {
    if (typeof input.name !== 'string' || !REPO_NAME.test(input.name)) throw validation('Validation Failed', 'Repository', 'name');
    if (input.name.toLowerCase() !== repo.name.toLowerCase() && store.find(repo.owner, input.name) !== undefined) {
      throw validation('name already exists on this account', 'Repository', 'name', 'custom');
    }
    store.rename(repo, input.name);
  }
  if (input.description !== undefined) repo.description = typeof input.description === 'string' ? input.description : null;
  if (input.homepage !== undefined) repo.homepage = typeof input.homepage === 'string' ? input.homepage : null;
  if (input.private !== undefined) repo.isPrivate = input.private === true;
  if (input.visibility !== undefined) repo.isPrivate = input.visibility !== 'public';
  if (typeof input.default_branch === 'string') {
    if (!repo.isEmpty() && repo.branchTip(input.default_branch) === undefined) throw validation('Validation Failed', 'Repository', 'default_branch');
    repo.defaultBranch = input.default_branch;
  }
  repo.updatedAt = tick().iso;
  return repo;
}

export function deleteRepository(repo: Repo): void {
  store.delete(repo);
}

// ------------------------------------------------------------------ labels

function labelKey(name: string): string {
  return name.toLowerCase();
}

export function createLabel(repo: Repo, input: Record<string, unknown>): Label {
  const name = typeof input.name === 'string' ? input.name.trim() : '';
  if (name === '') throw validation('Validation Failed', 'Label', 'name', 'missing_field');
  if (repo.labels.has(labelKey(name))) throw validation('Validation Failed', 'Label', 'name', 'already_exists');
  const color = typeof input.color === 'string' && /^#?[0-9a-fA-F]{6}$/.test(input.color) ? input.color.replace('#', '').toLowerCase() : 'ededed';
  const label: Label = { id: nextId('label'), name, color, description: typeof input.description === 'string' ? input.description : null };
  repo.labels.set(labelKey(name), label);
  return label;
}

export function findLabel(repo: Repo, name: string): Label | undefined {
  return repo.labels.get(labelKey(name));
}

/** Label names (strings or {name}) -> label keys; an unknown label is a 422, as GitHub's issue API answers. */
export function labelKeys(repo: Repo, value: unknown): string[] {
  if (value === undefined || value === null) return [];
  if (!Array.isArray(value)) throw validation('Validation Failed', 'Issue', 'labels');
  const keys: string[] = [];
  for (const item of value) {
    const name = typeof item === 'string' ? item : item !== null && typeof item === 'object' && typeof (item as { name?: unknown }).name === 'string' ? (item as { name: string }).name : null;
    if (name === null) throw validation('Validation Failed', 'Issue', 'labels');
    const label = findLabel(repo, name);
    if (label === undefined) {
      throw new HttpError(422, 'Validation Failed', { errors: [{ value: name, resource: 'Label', field: 'name', code: 'invalid', message: `label ${name} does not exist` }] });
    }
    if (!keys.includes(labelKey(label.name))) keys.push(labelKey(label.name));
  }
  return keys;
}

// ------------------------------------------------------------------ issues

function newIssue(repo: Repo, title: string, body: string | null, labels: string[], isPull: boolean): Issue {
  const t = tick();
  const issue: Issue = {
    id: nextId('issue'),
    number: repo.nextNumber++,
    title,
    body,
    state: 'open',
    stateReason: null,
    labels,
    comments: [],
    seq: t.seq,
    createdAt: t.iso,
    updatedAt: t.iso,
    closedAt: null,
    isPull,
  };
  repo.issues.set(issue.number, issue);
  return issue;
}

export function createIssue(repo: Repo, input: Record<string, unknown>): Issue {
  const title = typeof input.title === 'string' ? input.title : typeof input.title === 'number' ? String(input.title) : '';
  if (title.trim() === '') throw validation('Validation Failed', 'Issue', 'title', 'missing_field');
  if (input.body !== undefined && input.body !== null && typeof input.body !== 'string') throw validation('Validation Failed', 'Issue', 'body');
  const labels = labelKeys(repo, input.labels);
  return newIssue(repo, title, typeof input.body === 'string' ? input.body : null, labels, false);
}

export function requireIssue(repo: Repo, number: number): Issue {
  const issue = repo.issues.get(number);
  if (issue === undefined) throw new HttpError(404, 'Not Found');
  return issue;
}

export function setIssueState(issue: Issue, state: 'open' | 'closed', reason: string | null = null): void {
  if (issue.state === state) return;
  const t = tick();
  issue.state = state;
  issue.updatedAt = t.iso;
  issue.closedAt = state === 'closed' ? t.iso : null;
  issue.stateReason = state === 'closed' ? (reason ?? 'completed') : reason === null ? null : 'reopened';
}

export function updateIssue(repo: Repo, issue: Issue, input: Record<string, unknown>): Issue {
  if (input.title !== undefined) {
    if (typeof input.title !== 'string' || input.title.trim() === '') throw validation('Validation Failed', 'Issue', 'title');
    issue.title = input.title;
  }
  if (input.body !== undefined) issue.body = typeof input.body === 'string' ? input.body : null;
  if (input.labels !== undefined) issue.labels = labelKeys(repo, input.labels);
  if (input.state !== undefined) {
    if (input.state !== 'open' && input.state !== 'closed') throw validation('Validation Failed', 'Issue', 'state');
    setIssueState(issue, input.state, typeof input.state_reason === 'string' ? input.state_reason : null);
  }
  issue.updatedAt = tick().iso;
  return issue;
}

export function addComment(issue: Issue, body: unknown) {
  if (typeof body !== 'string' || body === '') throw validation('Validation Failed', 'IssueComment', 'body', 'missing_field');
  const t = tick();
  const comment = { id: nextId('comment'), body, createdAt: t.iso, updatedAt: t.iso };
  issue.comments.push(comment);
  issue.updatedAt = t.iso;
  return comment;
}

/** Find a comment anywhere in the repository by its id. */
export function findComment(repo: Repo, id: number) {
  for (const issue of repo.issues.values()) {
    const comment = issue.comments.find((c) => c.id === id);
    if (comment !== undefined) return { issue, comment };
  }
  return undefined;
}

// ------------------------------------------------------------------ refs and contents

export function createRef(repo: Repo, ref: unknown, sha: unknown): string {
  if (typeof ref !== 'string' || !ref.startsWith('refs/') || ref.split('/').length < 3) {
    throw new HttpError(422, 'Reference name must start with \'refs/\' and have at least two slashes.');
  }
  if (typeof sha !== 'string' || repo.get(sha) === undefined) throw new HttpError(422, 'Object does not exist');
  if (repo.refs.has(ref)) throw new HttpError(422, 'Reference already exists');
  repo.setRef(ref, sha);
  return ref;
}

export function updateRef(repo: Repo, ref: string, sha: unknown, force: boolean): void {
  const current = repo.refs.get(ref);
  if (current === undefined) throw new HttpError(422, 'Reference does not exist');
  if (typeof sha !== 'string' || repo.get(sha) === undefined) throw new HttpError(422, 'Object does not exist');
  if (!force && !repo.isAncestor(repo.peel(current), repo.peel(sha))) throw new HttpError(422, 'Update is not a fast forward');
  repo.setRef(ref, sha);
}

function normalizePath(path: string): string {
  const clean = path.split('/').filter((p) => p !== '').join('/');
  if (clean === '' || clean.split('/').some((p) => p === '.' || p === '..' || p === '.git')) throw validation('path is invalid', 'Content', 'path');
  return clean;
}

export interface FileCommit {
  created: boolean;
  path: string;
  blobId: string;
  size: number;
  commitId: string;
  branch: string;
}

/** The branch a contents write targets, and its current tip (undefined for an unborn repository). */
function writeTarget(repo: Repo, branch: unknown): { branch: string; tip: string | undefined } {
  const name = typeof branch === 'string' && branch !== '' ? branch.replace(/^refs\/heads\//, '') : repo.defaultBranch;
  const tip = repo.branchTip(name);
  if (tip === undefined && !repo.isEmpty()) throw new HttpError(404, `Branch ${name} not found`);
  return { branch: name, tip };
}

export function putFile(repo: Repo, rawPath: string, input: Record<string, unknown>): FileCommit {
  const path = normalizePath(rawPath);
  if (typeof input.content !== 'string') throw validation('Invalid request.\n\n"content" wasn\'t supplied.', 'Content', 'content', 'missing_field');
  const content = Buffer.from(input.content, 'base64');
  const message = typeof input.message === 'string' && input.message !== '' ? input.message : `Update ${path}`;
  const { branch, tip } = writeTarget(repo, input.branch);
  const files = tip === undefined ? new Map<string, FlatEntry>() : repo.flatten(repo.commit(tip)!.tree);
  const existing = files.get(path);
  if (existing !== undefined) {
    if (typeof input.sha !== 'string' || input.sha === '') throw validation('Invalid request.\n\n"sha" wasn\'t supplied.', 'Content', 'sha', 'missing_field');
    if (input.sha !== existing.id) throw new HttpError(409, `${path} does not match ${input.sha}`);
  } else {
    for (const other of files.keys()) {
      if (other.startsWith(`${path}/`) || path.startsWith(`${other}/`)) throw validation('Validation Failed', 'Content', 'path');
    }
  }
  const blobId = repo.put('blob', content);
  files.set(path, { mode: existing?.mode ?? '100644', id: blobId });
  const tree = repo.writeFlat(files);
  const commitId = repo.makeCommit(tree, tip === undefined ? [] : [tip], message);
  repo.setRef(`refs/heads/${branch}`, commitId);
  return { created: existing === undefined, path, blobId, size: content.length, commitId, branch };
}

export function deleteFile(repo: Repo, rawPath: string, input: Record<string, unknown>): { commitId: string; branch: string } {
  const path = normalizePath(rawPath);
  const { branch, tip } = writeTarget(repo, input.branch);
  if (tip === undefined) throw new HttpError(404, 'Not Found');
  const files = repo.flatten(repo.commit(tip)!.tree);
  const existing = files.get(path);
  if (existing === undefined) throw new HttpError(404, 'Not Found');
  if (typeof input.sha !== 'string' || input.sha === '') throw validation('Invalid request.\n\n"sha" wasn\'t supplied.', 'Content', 'sha', 'missing_field');
  if (input.sha !== existing.id) throw new HttpError(409, `${path} does not match ${input.sha}`);
  files.delete(path);
  const tree = repo.writeFlat(files);
  const message = typeof input.message === 'string' && input.message !== '' ? input.message : `Delete ${path}`;
  const commitId = repo.makeCommit(tree, [tip], message);
  repo.setRef(`refs/heads/${branch}`, commitId);
  return { commitId, branch };
}

// ------------------------------------------------------------------ pull requests

function branchName(repo: Repo, value: unknown, field: string): string {
  if (typeof value !== 'string' || value.trim() === '') throw validation('Validation Failed', 'PullRequest', field, 'missing_field');
  let name = value.trim();
  const colon = name.indexOf(':');
  if (colon >= 0) {
    if (name.slice(0, colon).toLowerCase() !== repo.owner.toLowerCase()) throw validation('Validation Failed', 'PullRequest', field);
    name = name.slice(colon + 1);
  }
  return name.replace(/^refs\/heads\//, '');
}

export function createPull(repo: Repo, input: Record<string, unknown>): Pull {
  const title = typeof input.title === 'string' ? input.title : '';
  if (title.trim() === '') throw validation('Validation Failed', 'PullRequest', 'title', 'missing_field');
  const head = branchName(repo, input.head, 'head');
  const base = branchName(repo, input.base, 'base');
  const headTip = repo.branchTip(head);
  const baseTip = repo.branchTip(base);
  if (baseTip === undefined) throw validation('Validation Failed', 'PullRequest', 'base');
  if (headTip === undefined) throw validation('Validation Failed', 'PullRequest', 'head');
  if (head === base) throw new HttpError(422, 'Validation Failed', { errors: [{ resource: 'PullRequest', code: 'custom', message: `No commits between ${base} and ${head}` }] });
  if (repo.isAncestor(headTip, baseTip)) {
    throw new HttpError(422, 'Validation Failed', { errors: [{ resource: 'PullRequest', code: 'custom', message: `No commits between ${base} and ${head}` }] });
  }
  for (const pull of repo.pulls.values()) {
    if (pull.issue.state === 'open' && pull.headRef === head && pull.baseRef === base) {
      throw new HttpError(422, 'Validation Failed', { errors: [{ resource: 'PullRequest', code: 'custom', message: `A pull request already exists for ${repo.owner}:${head}.` }] });
    }
  }
  const body = typeof input.body === 'string' ? input.body : null;
  const issue = newIssue(repo, title, body, [], true);
  const pull: Pull = {
    id: nextId('pull'),
    number: issue.number,
    issue,
    headRef: head,
    baseRef: base,
    merged: false,
    mergedAt: null,
    mergeCommitSha: null,
    closedHeadSha: null,
    closedBaseSha: null,
    draft: input.draft === true,
  };
  repo.pulls.set(pull.number, pull);
  return pull;
}

export function requirePull(repo: Repo, number: number): Pull {
  const pull = repo.pulls.get(number);
  if (pull === undefined) throw new HttpError(404, 'Not Found');
  return pull;
}

export function pullHeadSha(repo: Repo, pull: Pull): string {
  return pull.closedHeadSha ?? repo.branchTip(pull.headRef) ?? ZERO_ID;
}

export function pullBaseSha(repo: Repo, pull: Pull): string {
  return pull.closedBaseSha ?? repo.branchTip(pull.baseRef) ?? ZERO_ID;
}

export function updatePull(repo: Repo, pull: Pull, input: Record<string, unknown>): Pull {
  if (input.title !== undefined) {
    if (typeof input.title !== 'string' || input.title.trim() === '') throw validation('Validation Failed', 'PullRequest', 'title');
    pull.issue.title = input.title;
  }
  if (input.body !== undefined) pull.issue.body = typeof input.body === 'string' ? input.body : null;
  if (typeof input.base === 'string') {
    const base = branchName(repo, input.base, 'base');
    if (repo.branchTip(base) === undefined) throw validation('Validation Failed', 'PullRequest', 'base');
    pull.baseRef = base;
  }
  if (input.state === 'closed' || input.state === 'open') {
    if (pull.merged) throw validation('Validation Failed', 'PullRequest', 'state');
    if (input.state === 'closed') {
      pull.closedHeadSha = pullHeadSha(repo, pull);
      pull.closedBaseSha = pullBaseSha(repo, pull);
    } else {
      pull.closedHeadSha = null;
      pull.closedBaseSha = null;
    }
    setIssueState(pull.issue, input.state);
  }
  pull.issue.updatedAt = tick().iso;
  return pull;
}

/** Three-way merge of flat trees, file by file; null when a path changed differently on both sides. */
function mergeTrees(repo: Repo, baseTree: string | null, oursTree: string, theirsTree: string): string | null {
  if (baseTree === theirsTree) return oursTree;
  if (baseTree === oursTree) return theirsTree;
  const base = baseTree === null ? new Map<string, FlatEntry>() : repo.flatten(baseTree);
  const ours = repo.flatten(oursTree);
  const theirs = repo.flatten(theirsTree);
  const result = new Map<string, FlatEntry>();
  const paths = new Set<string>([...base.keys(), ...ours.keys(), ...theirs.keys()]);
  const same = (a: FlatEntry | undefined, b: FlatEntry | undefined): boolean =>
    a === undefined ? b === undefined : b !== undefined && a.id === b.id && a.mode === b.mode;
  for (const path of [...paths].sort()) {
    const b = base.get(path);
    const o = ours.get(path);
    const t = theirs.get(path);
    let pick: FlatEntry | undefined;
    if (same(o, t)) pick = o;
    else if (same(b, o)) pick = t;
    else if (same(b, t)) pick = o;
    else return null;
    if (pick !== undefined) result.set(path, pick);
  }
  for (const path of result.keys()) {
    const slash = path.lastIndexOf('/');
    for (let cut = slash; cut > 0; cut = path.lastIndexOf('/', cut - 1)) {
      if (result.has(path.slice(0, cut))) return null; // a file where the other side made a directory
    }
  }
  return repo.writeFlat(result);
}

const mergeCache = new Map<string, string | null>();

/** The merged tree of a pull's head into its base, or null on a conflict. */
export function mergedTree(repo: Repo, baseTip: string, headTip: string): string | null {
  const key = `${repo.id}:${baseTip}:${headTip}`;
  if (mergeCache.has(key)) return mergeCache.get(key)!;
  const ancestor = repo.mergeBase(baseTip, headTip);
  const baseTree = ancestor === undefined ? null : repo.commit(ancestor)!.tree;
  const tree = mergeTrees(repo, baseTree, repo.commit(baseTip)!.tree, repo.commit(headTip)!.tree);
  mergeCache.set(key, tree);
  return tree;
}

export function pullMergeable(repo: Repo, pull: Pull): boolean | null {
  if (pull.merged || pull.issue.state !== 'open') return null;
  const head = repo.branchTip(pull.headRef);
  const base = repo.branchTip(pull.baseRef);
  if (head === undefined || base === undefined) return false;
  return mergedTree(repo, base, head) !== null;
}

export interface MergeInput {
  method?: unknown;
  title?: unknown;
  message?: unknown;
  sha?: unknown;
}

export function mergePull(repo: Repo, pull: Pull, input: MergeInput): { sha: string; message: string } {
  if (pull.merged || pull.issue.state !== 'open') throw new HttpError(405, 'Pull Request is not mergeable');
  const headTip = repo.branchTip(pull.headRef);
  const baseTip = repo.branchTip(pull.baseRef);
  if (headTip === undefined || baseTip === undefined) throw new HttpError(405, 'Pull Request is not mergeable');
  if (typeof input.sha === 'string' && input.sha !== '' && input.sha !== headTip) throw new HttpError(409, 'Head branch was modified. Review and try the merge again.');
  const method = typeof input.method === 'string' ? input.method.toLowerCase() : 'merge';
  if (method !== 'merge' && method !== 'squash' && method !== 'rebase') throw validation('Validation Failed', 'PullRequest', 'merge_method');
  const tree = mergedTree(repo, baseTip, headTip);
  if (tree === null) throw new HttpError(405, 'Pull Request is not mergeable');
  const extra = typeof input.message === 'string' && input.message !== '' ? `\n\n${input.message}` : '';
  let commitId: string;
  if (method === 'merge') {
    const title = typeof input.title === 'string' && input.title !== '' ? input.title : `Merge pull request #${pull.number} from ${repo.owner}/${pull.headRef}`;
    commitId = repo.makeCommit(tree, [baseTip, headTip], `${title}\n\n${pull.issue.title}${extra}`);
  } else {
    const title = typeof input.title === 'string' && input.title !== '' ? input.title : `${pull.issue.title} (#${pull.number})`;
    commitId = repo.makeCommit(tree, [baseTip], `${title}${extra}`);
  }
  repo.setRef(`refs/heads/${pull.baseRef}`, commitId);
  const t = tick();
  pull.merged = true;
  pull.mergedAt = t.iso;
  pull.mergeCommitSha = commitId;
  pull.closedHeadSha = headTip;
  pull.closedBaseSha = baseTip;
  setIssueState(pull.issue, 'closed');
  pull.issue.stateReason = null;
  return { sha: commitId, message: 'Pull Request successfully merged' };
}

// ------------------------------------------------------------------ pull request files

export interface ChangedFile {
  filename: string;
  status: 'added' | 'removed' | 'modified' | 'renamed';
  additions: number;
  deletions: number;
  changes: number;
  sha: string;
  previousSha: string | null;
}

function lines(data: Buffer): string[] | null {
  if (data.includes(0)) return null; // binary
  const text = data.toString('utf8');
  if (text === '') return [];
  const parts = text.split('\n');
  if (parts[parts.length - 1] === '') parts.pop();
  return parts;
}

/** Myers' shortest edit distance between two line lists (additions + deletions). */
function editDistance(a: string[], b: string[]): number {
  const n = a.length;
  const m = b.length;
  const max = n + m;
  if (max === 0) return 0;
  const limit = Math.min(max, 4000);
  const offset = limit + 1;
  const v = new Int32Array(2 * limit + 3);
  for (let d = 0; d <= limit; d++) {
    for (let k = -d; k <= d; k += 2) {
      let x = k === -d || (k !== d && v[offset + k - 1]! < v[offset + k + 1]!) ? v[offset + k + 1]! : v[offset + k - 1]! + 1;
      let y = x - k;
      while (x < n && y < m && a[x] === b[y]) {
        x++;
        y++;
      }
      v[offset + k] = x;
      if (x >= n && y >= m) return d;
    }
  }
  // Too different to diff exactly: count lines present on one side only.
  const counts = new Map<string, number>();
  for (const line of a) counts.set(line, (counts.get(line) ?? 0) + 1);
  let common = 0;
  for (const line of b) {
    const c = counts.get(line) ?? 0;
    if (c > 0) {
      common++;
      counts.set(line, c - 1);
    }
  }
  return n + m - 2 * common;
}

function lineCounts(repo: Repo, oldId: string | null, newId: string | null): { additions: number; deletions: number } {
  const oldLines = oldId === null ? [] : lines(repo.get(oldId)?.data ?? Buffer.alloc(0));
  const newLines = newId === null ? [] : lines(repo.get(newId)?.data ?? Buffer.alloc(0));
  if (oldLines === null || newLines === null) return { additions: 0, deletions: 0 };
  const d = editDistance(oldLines, newLines);
  const common = (oldLines.length + newLines.length - d) / 2;
  return { additions: newLines.length - common, deletions: oldLines.length - common };
}

export function pullFiles(repo: Repo, pull: Pull): ChangedFile[] {
  const head = pullHeadSha(repo, pull);
  const base = pullBaseSha(repo, pull);
  if (repo.commit(head) === undefined || repo.commit(base) === undefined) return [];
  const ancestor = pull.merged ? pull.closedBaseSha ?? base : (repo.mergeBase(base, head) ?? base);
  const from = repo.commit(ancestor) === undefined ? new Map<string, FlatEntry>() : repo.flatten(repo.commit(ancestor)!.tree);
  const to = repo.flatten(repo.commit(head)!.tree);
  const files: ChangedFile[] = [];
  const paths = [...new Set([...from.keys(), ...to.keys()])].sort();
  for (const path of paths) {
    const a = from.get(path);
    const b = to.get(path);
    if (a !== undefined && b !== undefined && a.id === b.id && a.mode === b.mode) continue;
    const counts = lineCounts(repo, a?.id ?? null, b?.id ?? null);
    files.push({
      filename: path,
      status: a === undefined ? 'added' : b === undefined ? 'removed' : 'modified',
      additions: counts.additions,
      deletions: counts.deletions,
      changes: counts.additions + counts.deletions,
      sha: b?.id ?? a!.id,
      previousSha: a?.id ?? null,
    });
  }
  return files;
}

/** Commits on the pull's head that its base does not have, oldest first. */
export function pullCommits(repo: Repo, pull: Pull): string[] {
  const head = pullHeadSha(repo, pull);
  const base = pull.merged ? (pull.closedBaseSha ?? pullBaseSha(repo, pull)) : pullBaseSha(repo, pull);
  if (repo.commit(head) === undefined) return [];
  const excluded = new Set(repo.commit(base) === undefined ? [] : repo.log(base));
  return repo.log(head, 250, excluded).reverse();
}
