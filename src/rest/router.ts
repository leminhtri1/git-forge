// REST v3 under /api/v3: every route, matched on path segments. Errors are JSON {message}; every reply
// carries X-GitHub-Request-Id (src/http.ts).

import type { Req as IncomingMessage, Res as ServerResponse } from '../http.ts';
import { config } from '../config.ts';
import { isTreeMode, parseTree } from '../git/objects.ts';
import { HttpError, isAuthorized, mutationVersion, originOf, parseJsonBody, readBody, sendEmpty, sendError, sendJson, sendJsonBytes } from '../http.ts';
import {
  addComment,
  createIssue,
  createLabel,
  createPull,
  createRef,
  createRepository,
  deleteFile,
  deleteRepository,
  findComment,
  findLabel,
  labelKeys,
  mergePull,
  pullCommits,
  pullFiles,
  putFile,
  requireIssue,
  requirePull,
  requireRepo,
  updateIssue,
  updatePull,
  updateRef,
  updateRepository,
} from '../ops.ts';
import { isViewer, lookupRepo, store, tick, type Issue, type Repo } from '../store.ts';
import {
  branchView,
  commentView,
  commitView,
  contentEntryView,
  fullUserView,
  gitCommitView,
  issueView,
  labelView,
  pullView,
  refView,
  repoView,
  userView,
  type Urls,
} from './views.ts';

interface Ctx {
  req: IncomingMessage;
  res: ServerResponse;
  method: string;
  query: URLSearchParams;
  urls: Urls;
  body: () => Promise<Record<string, unknown>>;
}

const notFound = (): HttpError => new HttpError(404, 'Not Found');

// ------------------------------------------------------------------ pagination

function pageParams(query: URLSearchParams): { perPage: number; page: number } {
  const perPage = Math.min(100, Math.max(1, Number.parseInt(query.get('per_page') ?? '30', 10) || 30));
  const page = Math.max(1, Number.parseInt(query.get('page') ?? '1', 10) || 1);
  return { perPage, page };
}

/** Slice a list to the requested page and answer it with GitHub's Link header. */
function sendPage<T>(ctx: Ctx, items: readonly T[], view: (item: T) => unknown, path: string): void {
  const { perPage, page } = pageParams(ctx.query);
  const last = Math.max(1, Math.ceil(items.length / perPage));
  const slice = items.slice((page - 1) * perPage, page * perPage);
  const links: string[] = [];
  const link = (p: number, rel: string): void => {
    const params = new URLSearchParams(ctx.query);
    params.set('per_page', String(perPage));
    params.set('page', String(p));
    links.push(`<${ctx.urls.api}${path}?${params.toString()}>; rel="${rel}"`);
  };
  if (page < last) {
    link(page + 1, 'next');
    link(last, 'last');
  }
  if (page > 1) {
    link(1, 'first');
    link(Math.min(page - 1, last), 'prev');
  }
  sendJson(ctx.res, 200, slice.map(view), links.length > 0 ? { link: links.join(', ') } : {});
}

function intParam(value: string | undefined): number {
  if (value === undefined || !/^\d+$/.test(value)) throw notFound();
  return Number(value);
}

// ------------------------------------------------------------------ the single-issue read cache

const ISSUE_PATH = /^\/api\/v3\/repos\/([^/]+)\/([^/]+)\/issues\/(\d+)$/;
const issueCache = new Map<string, { version: number; body: Buffer }>();

/**
 * GET /api/v3/repos/{o}/{r}/issues/{n}, served from serialised bytes while no request has mutated the
 * store since they were made. Answers false (the normal router takes over) for anything else: no token,
 * an unknown repository or issue, or a path it does not cover.
 */
export function fastIssueGet(req: IncomingMessage, res: ServerResponse, path: string): boolean {
  if (req.method !== 'GET') return false;
  const match = ISSUE_PATH.exec(path);
  if (match === null || !isAuthorized(req)) return false;
  const key = `${req.headers.host ?? ''}|${path}`;
  const cached = issueCache.get(key);
  if (cached !== undefined && cached.version === mutationVersion) {
    sendJsonBytes(res, 200, cached.body);
    return true;
  }
  let owner: string;
  let name: string;
  try {
    owner = decodeURIComponent(match[1]!);
    name = decodeURIComponent(match[2]!);
  } catch {
    return false;
  }
  const repo = lookupRepo(owner, name);
  const issue = repo?.issues.get(Number(match[3]));
  if (repo === undefined || issue === undefined) return false;
  const origin = originOf(req);
  const body = Buffer.from(JSON.stringify(issueView(repo, issue, { api: `${origin}/api/v3`, web: origin })), 'utf8');
  if (issueCache.size > 50_000) issueCache.clear();
  issueCache.set(key, { version: mutationVersion, body });
  sendJsonBytes(res, 200, body);
  return true;
}

// ------------------------------------------------------------------ entry point

export async function handleRest(req: IncomingMessage, res: ServerResponse, path: string, query: URLSearchParams): Promise<void> {
  const origin = originOf(req);
  const urls: Urls = { api: `${origin}/api/v3`, web: origin };
  let cachedBody: Record<string, unknown> | null = null;
  const ctx: Ctx = {
    req,
    res,
    method: req.method ?? 'GET',
    query,
    urls,
    body: async () => {
      if (cachedBody === null) cachedBody = parseJsonBody(await readBody(req, 64 * 1024 * 1024));
      return cachedBody;
    },
  };
  const rest = path.slice('/api/v3'.length);
  const segments = rest.split('/').filter((s) => s !== '').map((s) => decodeURIComponent(s));

  if (segments.length === 0) {
    sendJson(res, 200, { current_user_url: `${urls.api}/user`, repository_url: `${urls.api}/repos/{owner}/{repo}`, meta_url: `${urls.api}/meta` }, { 'x-oauth-scopes': 'repo, read:org, workflow' });
    return;
  }
  if (!isAuthorized(req)) {
    req.resume();
    sendError(res, 401, 'Requires authentication');
    return;
  }
  // Read (and so validate) a JSON body up front for writes: malformed JSON is a 400 on any route.
  if (ctx.method === 'POST' || ctx.method === 'PATCH' || ctx.method === 'PUT') await ctx.body();
  else if (ctx.method === 'DELETE') await ctx.body().catch(() => ({}));
  await dispatch(ctx, segments);
}

async function dispatch(ctx: Ctx, s: string[]): Promise<void> {
  const { method, res, urls } = ctx;
  const head = s[0];
  if (head === 'meta' && s.length === 1) {
    sendJson(res, 200, { verifiable_password_authentication: false, installed_version: config.version, hooks: [], git: [], web: [], api: [] });
    return;
  }
  if (head === 'rate_limit' && s.length === 1) {
    const core = { limit: 5000, used: 0, remaining: 5000, reset: config.nowSec + 3600 };
    sendJson(res, 200, { resources: { core, graphql: core, search: core }, rate: core });
    return;
  }
  if (head === 'zen' && s.length === 1) {
    sendJson(res, 200, { message: 'Keep it logically awesome.' });
    return;
  }
  if (head === 'user' && s.length === 1 && method === 'GET') {
    sendJson(res, 200, fullUserView(urls), { 'x-oauth-scopes': 'repo, read:org, workflow' });
    return;
  }
  if (head === 'user' && s[1] === 'repos' && s.length === 2) {
    if (method === 'GET') return listRepos(ctx, '/user/repos');
    if (method === 'POST') {
      const repo = createRepository(await ctx.body());
      sendJson(res, 201, repoView(repo, urls), { location: `${urls.api}/repos/${repo.fullName}` });
      return;
    }
  }
  if (head === 'users' && s.length >= 2) {
    if (!isViewer(s[1]!)) throw notFound();
    if (s.length === 2 && method === 'GET') return sendJson(res, 200, fullUserView(urls));
    if (s.length === 3 && s[2] === 'repos' && method === 'GET') return listRepos(ctx, `/users/${s[1]}/repos`);
  }
  if (head === 'orgs' && s.length === 3 && s[2] === 'repos') throw notFound();
  if (head === 'repos' && s.length >= 3) return repoRoutes(ctx, requireRepo(s[1]!, s[2]!), s.slice(3));
  throw notFound();
}

function listRepos(ctx: Ctx, path: string): void {
  const visibility = ctx.query.get('visibility');
  let repos = store.list(config.viewer.login);
  if (visibility === 'public') repos = repos.filter((r) => !r.isPrivate);
  if (visibility === 'private') repos = repos.filter((r) => r.isPrivate);
  const sort = ctx.query.get('sort') ?? 'full_name';
  const direction = ctx.query.get('direction') ?? (sort === 'full_name' ? 'asc' : 'desc');
  repos = [...repos].sort((a, b) => (sort === 'full_name' ? (a.name.toLowerCase() < b.name.toLowerCase() ? -1 : a.name.toLowerCase() > b.name.toLowerCase() ? 1 : 0) : a.seq - b.seq));
  if (direction === 'desc') repos.reverse();
  sendPage(ctx, repos, (repo) => repoView(repo, ctx.urls), path);
}

// ------------------------------------------------------------------ /repos/{owner}/{repo}/...

async function repoRoutes(ctx: Ctx, repo: Repo, s: string[]): Promise<void> {
  const { method, res, urls } = ctx;
  const base = `/repos/${repo.fullName}`;
  if (s.length === 0) {
    if (method === 'GET') return sendJson(res, 200, repoView(repo, urls));
    if (method === 'PATCH') return sendJson(res, 200, repoView(updateRepository(repo, await ctx.body()), urls));
    if (method === 'DELETE') {
      deleteRepository(repo);
      return sendEmpty(res, 204);
    }
    throw notFound();
  }
  const [section] = s;
  switch (section) {
    case 'branches':
      return branchRoutes(ctx, repo, s.slice(1), base);
    case 'commits':
      return commitRoutes(ctx, repo, s.slice(1), base);
    case 'git':
      return gitRoutes(ctx, repo, s.slice(1));
    case 'contents':
      return contentRoutes(ctx, repo, s.slice(1).join('/'));
    case 'readme': {
      const tip = repo.resolve(ctx.query.get('ref'));
      const entries = tip === undefined ? [] : (repo.tree(repo.commit(tip)!.tree) ?? []);
      const readme = entries.find((e) => /^readme(\.|$)/i.test(e.name) && !isTreeMode(e.mode));
      if (readme === undefined) throw notFound();
      return contentRoutes(ctx, repo, readme.name);
    }
    case 'labels':
      return labelRoutes(ctx, repo, s.slice(1), base);
    case 'issues':
      return issueRoutes(ctx, repo, s.slice(1), base);
    case 'pulls':
      return pullRoutes(ctx, repo, s.slice(1), base);
    case 'collaborators':
      if (method === 'GET' && s.length === 1) return sendJson(res, 200, [userView(urls)]);
      break;
    case 'tags':
      if (method === 'GET' && s.length === 1) {
        const tags = [...repo.refs.entries()].filter(([name]) => name.startsWith('refs/tags/')).sort();
        return sendPage(ctx, tags, ([name, id]) => ({ name: name.slice(10), commit: { sha: repo.peel(id), url: `${urls.api}${base}/commits/${repo.peel(id)}` } }), `${base}/tags`);
      }
      break;
  }
  throw notFound();
}

function branchRoutes(ctx: Ctx, repo: Repo, s: string[], base: string): void {
  if (ctx.method !== 'GET') throw notFound();
  if (s.length === 0) {
    const branches = [...repo.refs.entries()].filter(([name]) => name.startsWith('refs/heads/')).map(([name, id]) => [name.slice(11), id] as const).sort((a, b) => (a[0] < b[0] ? -1 : a[0] > b[0] ? 1 : 0));
    return sendPage(ctx, branches, ([name, id]) => branchView(repo, name, id, ctx.urls, false), `${base}/branches`);
  }
  const name = s.join('/');
  const tip = repo.branchTip(name);
  if (tip === undefined) throw new HttpError(404, 'Branch not found');
  sendJson(ctx.res, 200, branchView(repo, name, tip, ctx.urls, true));
}

function commitRoutes(ctx: Ctx, repo: Repo, s: string[], base: string): void {
  if (ctx.method !== 'GET') throw notFound();
  if (s.length === 0) {
    if (repo.isEmpty()) throw new HttpError(409, 'Git Repository is empty.');
    const from = ctx.query.get('sha');
    const tip = repo.resolve(from);
    if (tip === undefined) throw new HttpError(404, `No commit found for SHA: ${from ?? ''}`);
    let commits = repo.log(tip, 10_000);
    const path = ctx.query.get('path');
    if (path !== null && path !== '') {
      commits = commits.filter((id) => {
        const commit = repo.commit(id)!;
        const here = repo.entryAt(commit.tree, path)?.id;
        if (commit.parents.length === 0) return here !== undefined;
        return commit.parents.every((p) => repo.entryAt(repo.commit(p)!.tree, path)?.id !== here);
      });
    }
    return sendPage(ctx, commits, (id) => commitView(repo, id, ctx.urls), `${base}/commits`);
  }
  const sha = repo.resolve(s.join('/'));
  if (sha === undefined || repo.commit(sha) === undefined) throw new HttpError(422, `No commit found for SHA: ${s.join('/')}`);
  const view = commitView(repo, sha, ctx.urls) as Record<string, unknown>;
  const commit = repo.commit(sha)!;
  const parentTree = commit.parents[0] === undefined ? undefined : repo.commit(commit.parents[0])?.tree;
  const before = parentTree === undefined ? new Map() : repo.flatten(parentTree);
  const after = repo.flatten(commit.tree);
  const files: unknown[] = [];
  for (const path of [...new Set([...before.keys(), ...after.keys()])].sort()) {
    const a = before.get(path);
    const b = after.get(path);
    if (a?.id === b?.id) continue;
    files.push({ filename: path, status: a === undefined ? 'added' : b === undefined ? 'removed' : 'modified', sha: b?.id ?? a?.id, additions: 0, deletions: 0, changes: 0 });
  }
  view.files = files;
  view.stats = { total: 0, additions: 0, deletions: 0 };
  sendJson(ctx.res, 200, view);
}

async function gitRoutes(ctx: Ctx, repo: Repo, s: string[]): Promise<void> {
  const { method, res, urls } = ctx;
  const kind = s[0];
  if (kind === 'refs' && s.length === 1) {
    if (method === 'POST') {
      const body = await ctx.body();
      const ref = createRef(repo, body.ref, body.sha);
      return sendJson(res, 201, refView(repo, ref, repo.refs.get(ref)!, urls));
    }
    if (method === 'GET') {
      const refs = [...repo.refs.entries()].sort();
      return sendJson(res, 200, refs.map(([name, id]) => refView(repo, name, id, urls)));
    }
  }
  if ((kind === 'refs' || kind === 'ref') && s.length >= 2) {
    const ref = `refs/${s.slice(1).join('/')}`;
    if (method === 'GET') {
      const id = repo.refs.get(ref);
      if (id !== undefined) return sendJson(res, 200, refView(repo, ref, id, urls));
      if (kind === 'refs') {
        const matches = [...repo.refs.entries()].filter(([name]) => name.startsWith(`${ref}/`)).sort();
        if (matches.length > 0) return sendJson(res, 200, matches.map(([name, sha]) => refView(repo, name, sha, urls)));
      }
      throw notFound();
    }
    if (method === 'PATCH') {
      const body = await ctx.body();
      updateRef(repo, ref, body.sha, body.force === true);
      return sendJson(res, 200, refView(repo, ref, repo.refs.get(ref)!, urls));
    }
    if (method === 'DELETE') {
      if (!repo.refs.has(ref)) throw new HttpError(422, 'Reference does not exist');
      repo.setRef(ref, null);
      return sendEmpty(res, 204);
    }
  }
  if (kind === 'commits' && s.length === 2 && method === 'GET') {
    if (repo.commit(s[1]!) === undefined) throw notFound();
    return sendJson(res, 200, gitCommitView(repo, s[1]!, urls));
  }
  if (kind === 'blobs' && s.length === 2 && method === 'GET') {
    const object = repo.get(s[1]!);
    if (object === undefined || object.type !== 'blob') throw notFound();
    return sendJson(res, 200, { sha: s[1], node_id: s[1], size: object.data.length, url: `${urls.api}/repos/${repo.fullName}/git/blobs/${s[1]}`, content: object.data.toString('base64'), encoding: 'base64' });
  }
  if (kind === 'trees' && s.length === 2 && method === 'GET') {
    const treeId = repo.get(s[1]!)?.type === 'tree' ? s[1]! : repo.commit(repo.resolve(s[1]!) ?? '')?.tree;
    if (treeId === undefined) throw notFound();
    const recursive = ctx.query.has('recursive') && ctx.query.get('recursive') !== '0' && ctx.query.get('recursive') !== 'false';
    const tree: unknown[] = [];
    const walk = (id: string, prefix: string): void => {
      for (const entry of repo.tree(id) ?? []) {
        const path = prefix === '' ? entry.name : `${prefix}/${entry.name}`;
        const dir = isTreeMode(entry.mode);
        tree.push({ path, mode: dir ? '040000' : entry.mode, type: dir ? 'tree' : entry.mode === '160000' ? 'commit' : 'blob', sha: entry.id, ...(dir ? {} : { size: repo.get(entry.id)?.data.length ?? 0 }), url: `${urls.api}/repos/${repo.fullName}/git/${dir ? 'trees' : 'blobs'}/${entry.id}` });
        if (dir && recursive) walk(entry.id, path);
      }
    };
    walk(treeId, '');
    return sendJson(res, 200, { sha: treeId, url: `${urls.api}/repos/${repo.fullName}/git/trees/${treeId}`, tree, truncated: false });
  }
  throw notFound();
}

async function contentRoutes(ctx: Ctx, repo: Repo, path: string): Promise<void> {
  const { method, res, urls } = ctx;
  if (method === 'GET') {
    if (repo.isEmpty()) throw new HttpError(404, 'This repository is empty.');
    const refName = ctx.query.get('ref') ?? repo.defaultBranch;
    const tip = repo.resolve(refName);
    if (tip === undefined) throw new HttpError(404, `No commit found for the ref ${refName}`);
    const root = repo.commit(tip)!.tree;
    const entry = repo.entryAt(root, path);
    if (entry === undefined) throw notFound();
    if (isTreeMode(entry.mode)) {
      const prefix = path.split('/').filter(Boolean).join('/');
      const listing = parseTree(repo.get(entry.id)!.data).map((child) => {
        const childPath = prefix === '' ? child.name : `${prefix}/${child.name}`;
        const size = isTreeMode(child.mode) ? 0 : (repo.get(child.id)?.data.length ?? 0);
        return contentEntryView(repo, childPath, child.mode, child.id, size, refName, urls);
      });
      return sendJson(res, 200, listing);
    }
    const blob = repo.get(entry.id)!;
    const cleanPath = path.split('/').filter(Boolean).join('/');
    return sendJson(res, 200, {
      ...contentEntryView(repo, cleanPath, entry.mode, entry.id, blob.data.length, refName, urls),
      ...(entry.mode === '120000' ? { target: blob.data.toString('utf8') } : {}),
      content: blob.data.toString('base64'),
      encoding: 'base64',
    });
  }
  if (method === 'PUT') {
    const result = putFile(repo, path, await ctx.body());
    return sendJson(res, result.created ? 201 : 200, {
      content: { ...contentEntryView(repo, result.path, '100644', result.blobId, result.size, result.branch, urls), encoding: 'base64' },
      commit: gitCommitView(repo, result.commitId, urls),
    });
  }
  if (method === 'DELETE') {
    const result = deleteFile(repo, path, await ctx.body());
    return sendJson(res, 200, { content: null, commit: gitCommitView(repo, result.commitId, urls) });
  }
  throw notFound();
}

async function labelRoutes(ctx: Ctx, repo: Repo, s: string[], base: string): Promise<void> {
  const { method, res, urls } = ctx;
  if (s.length === 0) {
    if (method === 'GET') return sendPage(ctx, [...repo.labels.values()].sort((a, b) => a.id - b.id), (l) => labelView(repo, l, urls), `${base}/labels`);
    if (method === 'POST') return sendJson(res, 201, labelView(repo, createLabel(repo, await ctx.body()), urls));
    throw notFound();
  }
  const label = findLabel(repo, s.join('/'));
  if (label === undefined) throw notFound();
  if (method === 'GET') return sendJson(res, 200, labelView(repo, label, urls));
  if (method === 'PATCH') {
    const body = await ctx.body();
    const newName = typeof body.new_name === 'string' ? body.new_name : typeof body.name === 'string' ? body.name : label.name;
    if (newName.toLowerCase() !== label.name.toLowerCase()) {
      if (findLabel(repo, newName) !== undefined) throw new HttpError(422, 'Validation Failed');
      const oldKey = label.name.toLowerCase();
      repo.labels.delete(oldKey);
      repo.labels.set(newName.toLowerCase(), label);
      for (const issue of repo.issues.values()) issue.labels = issue.labels.map((k) => (k === oldKey ? newName.toLowerCase() : k));
    }
    label.name = newName;
    if (typeof body.color === 'string') label.color = body.color.replace('#', '');
    if (body.description !== undefined) label.description = typeof body.description === 'string' ? body.description : null;
    return sendJson(res, 200, labelView(repo, label, urls));
  }
  if (method === 'DELETE') {
    const key = label.name.toLowerCase();
    repo.labels.delete(key);
    for (const issue of repo.issues.values()) issue.labels = issue.labels.filter((k) => k !== key);
    return sendEmpty(res, 204);
  }
  throw notFound();
}

function sortIssues(list: Issue[], sort: string, direction: string): Issue[] {
  const key = (issue: Issue): number => (sort === 'comments' ? issue.comments.length : sort === 'updated' ? Date.parse(issue.updatedAt) : issue.seq);
  const sorted = [...list].sort((a, b) => key(a) - key(b) || a.number - b.number);
  return direction === 'asc' ? sorted : sorted.reverse();
}

async function issueRoutes(ctx: Ctx, repo: Repo, s: string[], base: string): Promise<void> {
  const { method, res, urls, query } = ctx;
  if (s.length === 0) {
    if (method === 'POST') {
      const issue = createIssue(repo, await ctx.body());
      return sendJson(res, 201, issueView(repo, issue, urls), { location: `${urls.api}${base}/issues/${issue.number}` });
    }
    if (method === 'GET') {
      const state = query.get('state') ?? 'open';
      const labels = (query.get('labels') ?? '').split(',').map((l) => l.trim().toLowerCase()).filter(Boolean);
      let list = [...repo.issues.values()].filter((i) => !i.isPull && (state === 'all' || i.state === state));
      if (labels.length > 0) list = list.filter((i) => labels.every((l) => i.labels.includes(l)));
      list = sortIssues(list, query.get('sort') ?? 'created', query.get('direction') ?? 'desc');
      return sendPage(ctx, list, (i) => issueView(repo, i, urls), `${base}/issues`);
    }
    throw notFound();
  }
  if (s[0] === 'comments') {
    if (s.length === 1 && method === 'GET') {
      const all = [...repo.issues.values()].flatMap((issue) => issue.comments.map((comment) => ({ issue, comment }))).sort((a, b) => a.comment.id - b.comment.id);
      return sendPage(ctx, all, ({ issue, comment }) => commentView(repo, issue, comment, urls), `${base}/issues/comments`);
    }
    const found = findComment(repo, intParam(s[1]));
    if (found === undefined || s.length !== 2) throw notFound();
    if (method === 'GET') return sendJson(res, 200, commentView(repo, found.issue, found.comment, urls));
    if (method === 'PATCH') {
      const body = await ctx.body();
      if (typeof body.body !== 'string' || body.body === '') throw new HttpError(422, 'Validation Failed');
      found.comment.body = body.body;
      found.comment.updatedAt = tick().iso;
      return sendJson(res, 200, commentView(repo, found.issue, found.comment, urls));
    }
    if (method === 'DELETE') {
      found.issue.comments = found.issue.comments.filter((c) => c !== found.comment);
      return sendEmpty(res, 204);
    }
    throw notFound();
  }
  const issue = requireIssue(repo, intParam(s[0]));
  if (s.length === 1) {
    if (method === 'GET') return sendJson(res, 200, issueView(repo, issue, urls));
    if (method === 'PATCH') return sendJson(res, 200, issueView(repo, updateIssue(repo, issue, await ctx.body()), urls));
    throw notFound();
  }
  if (s[1] === 'comments' && s.length === 2) {
    if (method === 'GET') return sendPage(ctx, issue.comments, (c) => commentView(repo, issue, c, urls), `${base}/issues/${issue.number}/comments`);
    if (method === 'POST') return sendJson(res, 201, commentView(repo, issue, addComment(issue, (await ctx.body()).body), urls));
  }
  if (s[1] === 'labels' && s.length === 2) {
    const view = () => issue.labels.map((k) => repo.labels.get(k)).filter((l) => l !== undefined).map((l) => labelView(repo, l, urls));
    if (method === 'GET') return sendJson(res, 200, view());
    if (method === 'POST' || method === 'PUT') {
      const body = await ctx.body();
      const keys = labelKeys(repo, Array.isArray(body.labels) ? body.labels : []);
      issue.labels = method === 'PUT' ? keys : [...new Set([...issue.labels, ...keys])];
      return sendJson(res, 200, view());
    }
    if (method === 'DELETE') {
      issue.labels = [];
      return sendEmpty(res, 204);
    }
  }
  if (s[1] === 'labels' && s.length === 3 && method === 'DELETE') {
    const key = s[2]!.toLowerCase();
    if (!issue.labels.includes(key)) throw notFound();
    issue.labels = issue.labels.filter((k) => k !== key);
    return sendJson(res, 200, issue.labels.map((k) => repo.labels.get(k)).filter((l) => l !== undefined).map((l) => labelView(repo, l, urls)));
  }
  throw notFound();
}

async function pullRoutes(ctx: Ctx, repo: Repo, s: string[], base: string): Promise<void> {
  const { method, res, urls, query } = ctx;
  if (s.length === 0) {
    if (method === 'POST') {
      const pull = createPull(repo, await ctx.body());
      return sendJson(res, 201, pullView(repo, pull, urls), { location: `${urls.api}${base}/pulls/${pull.number}` });
    }
    if (method === 'GET') {
      const state = query.get('state') ?? 'open';
      const headFilter = query.get('head');
      const baseFilter = query.get('base');
      let list = [...repo.pulls.values()].filter((p) => state === 'all' || p.issue.state === state);
      if (headFilter !== null) list = list.filter((p) => `${repo.owner}:${p.headRef}` === headFilter || p.headRef === headFilter);
      if (baseFilter !== null) list = list.filter((p) => p.baseRef === baseFilter);
      const direction = query.get('direction') ?? 'desc';
      list.sort((a, b) => a.number - b.number);
      if (direction === 'desc') list.reverse();
      return sendPage(ctx, list, (p) => pullView(repo, p, urls, false), `${base}/pulls`);
    }
    throw notFound();
  }
  const pull = requirePull(repo, intParam(s[0]));
  if (s.length === 1) {
    if (method === 'GET') return sendJson(res, 200, pullView(repo, pull, urls));
    if (method === 'PATCH') return sendJson(res, 200, pullView(repo, updatePull(repo, pull, await ctx.body()), urls));
    throw notFound();
  }
  if (s[1] === 'files' && s.length === 2 && method === 'GET') {
    const files = pullFiles(repo, pull);
    return sendPage(
      ctx,
      files,
      (f) => ({
        sha: f.sha,
        filename: f.filename,
        status: f.status,
        additions: f.additions,
        deletions: f.deletions,
        changes: f.changes,
        blob_url: `${urls.web}/${repo.fullName}/blob/${pullHeadShaSafe(repo, pull)}/${f.filename}`,
        raw_url: `${urls.web}/${repo.fullName}/raw/${pullHeadShaSafe(repo, pull)}/${f.filename}`,
        contents_url: `${urls.api}${base}/contents/${f.filename}?ref=${pullHeadShaSafe(repo, pull)}`,
      }),
      `${base}/pulls/${pull.number}/files`,
    );
  }
  if (s[1] === 'commits' && s.length === 2 && method === 'GET') {
    return sendPage(ctx, pullCommits(repo, pull), (id) => commitView(repo, id, urls), `${base}/pulls/${pull.number}/commits`);
  }
  if (s[1] === 'merge' && s.length === 2) {
    if (method === 'GET') {
      if (pull.merged) return sendEmpty(res, 204);
      throw notFound();
    }
    if (method === 'PUT') {
      const body = await ctx.body();
      const result = mergePull(repo, pull, { method: body.merge_method ?? 'merge', title: body.commit_title, message: body.commit_message, sha: body.sha });
      return sendJson(res, 200, { sha: result.sha, merged: true, message: result.message });
    }
  }
  throw notFound();
}

function pullHeadShaSafe(repo: Repo, pull: { headRef: string; closedHeadSha: string | null }): string {
  return pull.closedHeadSha ?? repo.branchTip(pull.headRef) ?? '';
}
