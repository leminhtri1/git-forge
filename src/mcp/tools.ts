// The MCP tools: github-mcp-server's names and argument shapes, acting on the same store as REST, and
// answering with the same JSON objects REST answers (so ids and fields agree across surfaces).

import { config } from '../config.ts';
import { HttpError } from '../http.ts';
import {
  addComment,
  createIssue,
  createPull,
  createRef,
  createRepository,
  mergePull,
  pullFiles,
  putFile,
  requireIssue,
  requirePull,
  requireRepo,
  updateIssue,
} from '../ops.ts';
import { store } from '../store.ts';
import { commentView, contentEntryView, gitCommitView, issueView, labelView, pullView, repoView, fullUserView, type Urls } from '../rest/views.ts';
import { isTreeMode, parseTree } from '../git/objects.ts';

type JsonSchema = Record<string, unknown>;

export interface ToolDef {
  name: string;
  description: string;
  inputSchema: JsonSchema;
  annotations?: Record<string, unknown>;
  run: (args: Record<string, unknown>, urls: Urls) => unknown;
}

/** A tools/call whose arguments do not satisfy the tool's schema (answered as JSON-RPC -32602). */
export class InvalidArguments extends Error {}

const str = (description: string): JsonSchema => ({ type: 'string', description });
const num = (description: string): JsonSchema => ({ type: 'number', description });
const bool = (description: string): JsonSchema => ({ type: 'boolean', description });
const OWNER = str('Repository owner (the user login)');
const REPO = str('Repository name');

function schema(properties: Record<string, JsonSchema>, required: string[]): JsonSchema {
  return { type: 'object', properties, required, additionalProperties: false };
}

function text(args: Record<string, unknown>, key: string, required = true): string | undefined {
  const value = args[key];
  if (value === undefined || value === null || value === '') {
    if (required) throw new InvalidArguments(`missing required argument: ${key}`);
    return undefined;
  }
  if (typeof value !== 'string') throw new InvalidArguments(`argument ${key} must be a string`);
  return value;
}

function int(args: Record<string, unknown>, key: string, required = true): number | undefined {
  const value = args[key];
  if (value === undefined || value === null || value === '') {
    if (required) throw new InvalidArguments(`missing required argument: ${key}`);
    return undefined;
  }
  const n = typeof value === 'string' && /^\d+$/.test(value) ? Number(value) : value;
  if (typeof n !== 'number' || !Number.isInteger(n)) throw new InvalidArguments(`argument ${key} must be an integer`);
  return n;
}

function repoOf(args: Record<string, unknown>) {
  return requireRepo(text(args, 'owner')!, text(args, 'repo')!);
}

function page<T>(items: T[], args: Record<string, unknown>): T[] {
  const perPage = Math.min(100, Math.max(1, int(args, 'perPage', false) ?? 30));
  const p = Math.max(1, int(args, 'page', false) ?? 1);
  return items.slice((p - 1) * perPage, p * perPage);
}

export const TOOLS: ToolDef[] = [
  {
    name: 'get_me',
    description: 'Get the authenticated user: login, id and profile.',
    inputSchema: schema({}, []),
    annotations: { title: 'Get my user profile', readOnlyHint: true },
    run: (_args, urls) => fullUserView(urls),
  },
  {
    name: 'create_repo',
    description: 'Create a new repository owned by the authenticated user.',
    inputSchema: schema({ name: str('Repository name'), description: str('Repository description'), private: bool('Whether the repository is private'), autoInit: bool('Initialize with a README') }, ['name']),
    annotations: { title: 'Create repository', readOnlyHint: false },
    run: (args, urls) => repoView(createRepository({ name: text(args, 'name'), description: args.description, private: args.private, auto_init: args.autoInit }), urls),
  },
  {
    name: 'get_repo',
    description: 'Get a repository by owner and name: its id, full name, visibility and default branch.',
    inputSchema: schema({ owner: OWNER, repo: REPO }, ['owner', 'repo']),
    annotations: { title: 'Get repository', readOnlyHint: true },
    run: (args, urls) => repoView(repoOf(args), urls),
  },
  {
    name: 'list_repos',
    description: "List the authenticated user's repositories, oldest first.",
    inputSchema: schema({ page: num('Page number (1-based)'), perPage: num('Results per page (max 100)') }, []),
    annotations: { title: 'List repositories', readOnlyHint: true },
    run: (args, urls) => ({ repositories: page(store.list(config.viewer.login), args).map((r) => repoView(r, urls)) }),
  },
  {
    name: 'list_branches',
    description: 'List the branches of a repository with their tip commits.',
    inputSchema: schema({ owner: OWNER, repo: REPO }, ['owner', 'repo']),
    annotations: { title: 'List branches', readOnlyHint: true },
    run: (args) => {
      const repo = repoOf(args);
      const branches = [...repo.refs.entries()].filter(([n]) => n.startsWith('refs/heads/')).sort().map(([n, sha]) => ({ name: n.slice(11), commit: { sha } }));
      return { branches };
    },
  },
  {
    name: 'create_branch',
    description: 'Create a branch from another branch (the default branch when from_branch is omitted).',
    inputSchema: schema({ owner: OWNER, repo: REPO, branch: str('Name of the new branch'), from_branch: str('Source branch (defaults to the default branch)') }, ['owner', 'repo', 'branch']),
    annotations: { title: 'Create branch', readOnlyHint: false },
    run: (args) => {
      const repo = repoOf(args);
      const from = text(args, 'from_branch', false) ?? repo.defaultBranch;
      const sha = repo.branchTip(from);
      if (sha === undefined) throw new HttpError(404, `Branch ${from} not found`);
      const ref = createRef(repo, `refs/heads/${text(args, 'branch')}`, sha);
      return { ref, object: { sha, type: 'commit' } };
    },
  },
  {
    name: 'get_file_contents',
    description: 'Get the contents of a file (base64) or a directory listing at a path and optional ref.',
    inputSchema: schema({ owner: OWNER, repo: REPO, path: str('Path to the file or directory'), ref: str('Branch, tag or commit (defaults to the default branch)') }, ['owner', 'repo', 'path']),
    annotations: { title: 'Get file or directory contents', readOnlyHint: true },
    run: (args, urls) => {
      const repo = repoOf(args);
      const ref = text(args, 'ref', false) ?? repo.defaultBranch;
      const tip = repo.resolve(ref);
      if (tip === undefined) throw new HttpError(404, `No commit found for the ref ${ref}`);
      const path = (text(args, 'path', false) ?? '').split('/').filter(Boolean).join('/');
      const entry = repo.entryAt(repo.commit(tip)!.tree, path);
      if (entry === undefined) throw new HttpError(404, 'Not Found');
      if (isTreeMode(entry.mode)) {
        return {
          entries: parseTree(repo.get(entry.id)!.data).map((c) => contentEntryView(repo, path === '' ? c.name : `${path}/${c.name}`, c.mode, c.id, isTreeMode(c.mode) ? 0 : (repo.get(c.id)?.data.length ?? 0), ref, urls)),
        };
      }
      const blob = repo.get(entry.id)!;
      return { ...contentEntryView(repo, path, entry.mode, entry.id, blob.data.length, ref, urls), encoding: 'base64', content: blob.data.toString('base64') };
    },
  },
  {
    name: 'create_or_update_file',
    description: 'Create or update a single file with a commit. Updating an existing file requires its current blob sha.',
    inputSchema: schema(
      { owner: OWNER, repo: REPO, path: str('Path of the file'), content: str('New file content (plain text)'), message: str('Commit message'), branch: str('Branch to commit to'), sha: str('Blob sha of the file being replaced') },
      ['owner', 'repo', 'path', 'content', 'message'],
    ),
    annotations: { title: 'Create or update file', readOnlyHint: false },
    run: (args, urls) => {
      const repo = repoOf(args);
      const result = putFile(repo, text(args, 'path')!, {
        content: Buffer.from(text(args, 'content')!, 'utf8').toString('base64'),
        message: text(args, 'message'),
        branch: text(args, 'branch', false),
        sha: text(args, 'sha', false),
      });
      return { content: contentEntryView(repo, result.path, '100644', result.blobId, result.size, result.branch, urls), commit: gitCommitView(repo, result.commitId, urls) };
    },
  },
  {
    name: 'create_issue',
    description: 'Create an issue in a repository. Labels must already exist in the repository.',
    inputSchema: schema({ owner: OWNER, repo: REPO, title: str('Issue title'), body: str('Issue body (Markdown)'), labels: { type: 'array', items: { type: 'string' }, description: 'Label names to apply' } }, ['owner', 'repo', 'title']),
    annotations: { title: 'Open new issue', readOnlyHint: false },
    run: (args, urls) => {
      const repo = repoOf(args);
      const title = text(args, 'title')!;
      return issueView(repo, createIssue(repo, { title, body: text(args, 'body', false), labels: args.labels }), urls);
    },
  },
  {
    name: 'get_issue',
    description: 'Get one issue of a repository by its number.',
    inputSchema: schema({ owner: OWNER, repo: REPO, issue_number: num('Issue number') }, ['owner', 'repo', 'issue_number']),
    annotations: { title: 'Get issue details', readOnlyHint: true },
    run: (args, urls) => {
      const repo = repoOf(args);
      return issueView(repo, requireIssue(repo, int(args, 'issue_number')!), urls);
    },
  },
  {
    name: 'list_issues',
    description: 'List the issues of a repository, filtered by state (open, closed or all; default open), newest first.',
    inputSchema: schema(
      {
        owner: OWNER,
        repo: REPO,
        state: { type: 'string', enum: ['open', 'closed', 'all'], description: 'Filter by state' },
        labels: { type: 'array', items: { type: 'string' }, description: 'Only issues carrying all of these labels' },
        direction: { type: 'string', enum: ['asc', 'desc'], description: 'Sort direction by creation' },
        page: num('Page number (1-based)'),
        perPage: num('Results per page (max 100)'),
      },
      ['owner', 'repo'],
    ),
    annotations: { title: 'List issues', readOnlyHint: true },
    run: (args, urls) => {
      const repo = repoOf(args);
      const state = (text(args, 'state', false) ?? 'open').toLowerCase();
      const labels = Array.isArray(args.labels) ? args.labels.map((l) => String(l).toLowerCase()) : [];
      let list = [...repo.issues.values()].filter((i) => !i.isPull && (state === 'all' || i.state === state) && labels.every((l) => i.labels.includes(l)));
      list.sort((a, b) => a.seq - b.seq || a.number - b.number);
      if ((text(args, 'direction', false) ?? 'desc') === 'desc') list = list.reverse();
      const issues = page(list, args).map((i) => issueView(repo, i, urls));
      return { issues, totalCount: list.length };
    },
  },
  {
    name: 'update_issue',
    description: 'Update an issue: title, body, state (open or closed) or labels.',
    inputSchema: schema(
      { owner: OWNER, repo: REPO, issue_number: num('Issue number'), title: str('New title'), body: str('New body'), state: { type: 'string', enum: ['open', 'closed'], description: 'New state' }, labels: { type: 'array', items: { type: 'string' }, description: 'Replace the labels' } },
      ['owner', 'repo', 'issue_number'],
    ),
    annotations: { title: 'Edit issue', readOnlyHint: false },
    run: (args, urls) => {
      const repo = repoOf(args);
      const issue = requireIssue(repo, int(args, 'issue_number')!);
      const patch: Record<string, unknown> = {};
      for (const key of ['title', 'body', 'state', 'labels']) if (args[key] !== undefined) patch[key] = args[key];
      return issueView(repo, updateIssue(repo, issue, patch), urls);
    },
  },
  {
    name: 'add_issue_comment',
    description: 'Add a comment to an issue or pull request.',
    inputSchema: schema({ owner: OWNER, repo: REPO, issue_number: num('Issue or pull request number'), body: str('Comment text') }, ['owner', 'repo', 'issue_number', 'body']),
    annotations: { title: 'Add comment to issue', readOnlyHint: false },
    run: (args, urls) => {
      const repo = repoOf(args);
      const issue = requireIssue(repo, int(args, 'issue_number')!);
      return commentView(repo, issue, addComment(issue, text(args, 'body')), urls);
    },
  },
  {
    name: 'list_labels',
    description: 'List the labels defined in a repository.',
    inputSchema: schema({ owner: OWNER, repo: REPO }, ['owner', 'repo']),
    annotations: { title: 'List labels', readOnlyHint: true },
    run: (args, urls) => {
      const repo = repoOf(args);
      return { labels: [...repo.labels.values()].sort((a, b) => a.id - b.id).map((l) => labelView(repo, l, urls)) };
    },
  },
  {
    name: 'create_pull_request',
    description: 'Open a pull request from a head branch into a base branch.',
    inputSchema: schema(
      { owner: OWNER, repo: REPO, title: str('Pull request title'), head: str('Branch with the changes'), base: str('Branch to merge into'), body: str('Pull request description'), draft: bool('Open as a draft') },
      ['owner', 'repo', 'title', 'head', 'base'],
    ),
    annotations: { title: 'Open new pull request', readOnlyHint: false },
    run: (args, urls) => {
      const repo = repoOf(args);
      const pull = createPull(repo, { title: text(args, 'title'), head: text(args, 'head'), base: text(args, 'base'), body: text(args, 'body', false), draft: args.draft === true });
      return pullView(repo, pull, urls);
    },
  },
  {
    name: 'get_pull_request',
    description: 'Get one pull request by its number, with its head, base and merge state.',
    inputSchema: schema({ owner: OWNER, repo: REPO, pull_number: num('Pull request number') }, ['owner', 'repo', 'pull_number']),
    annotations: { title: 'Get pull request details', readOnlyHint: true },
    run: (args, urls) => {
      const repo = repoOf(args);
      return pullView(repo, requirePull(repo, int(args, 'pull_number')!), urls);
    },
  },
  {
    name: 'list_pull_requests',
    description: 'List the pull requests of a repository, filtered by state (open, closed or all; default open).',
    inputSchema: schema({ owner: OWNER, repo: REPO, state: { type: 'string', enum: ['open', 'closed', 'all'], description: 'Filter by state' }, page: num('Page number (1-based)'), perPage: num('Results per page (max 100)') }, ['owner', 'repo']),
    annotations: { title: 'List pull requests', readOnlyHint: true },
    run: (args, urls) => {
      const repo = repoOf(args);
      const state = (text(args, 'state', false) ?? 'open').toLowerCase();
      const list = [...repo.pulls.values()].filter((p) => state === 'all' || p.issue.state === state).sort((a, b) => b.number - a.number);
      return { pull_requests: page(list, args).map((p) => pullView(repo, p, urls, false)) };
    },
  },
  {
    name: 'get_pull_request_files',
    description: 'List the files a pull request changes, with their status and line counts.',
    inputSchema: schema({ owner: OWNER, repo: REPO, pull_number: num('Pull request number') }, ['owner', 'repo', 'pull_number']),
    annotations: { title: 'Get pull request files', readOnlyHint: true },
    run: (args) => {
      const repo = repoOf(args);
      const pull = requirePull(repo, int(args, 'pull_number')!);
      return { files: pullFiles(repo, pull).map((f) => ({ filename: f.filename, status: f.status, additions: f.additions, deletions: f.deletions, changes: f.changes, sha: f.sha })) };
    },
  },
  {
    name: 'merge_pull_request',
    description: 'Merge a pull request (merge, squash or rebase; default squash). A conflicted pull request is refused.',
    inputSchema: schema(
      {
        owner: OWNER,
        repo: REPO,
        pull_number: num('Pull request number'),
        merge_method: { type: 'string', enum: ['merge', 'squash', 'rebase'], description: 'Merge method' },
        commit_title: str('Title of the merge commit'),
        commit_message: str('Extra detail for the merge commit'),
      },
      ['owner', 'repo', 'pull_number'],
    ),
    annotations: { title: 'Merge pull request', readOnlyHint: false },
    run: (args) => {
      const repo = repoOf(args);
      const pull = requirePull(repo, int(args, 'pull_number')!);
      const result = mergePull(repo, pull, { method: text(args, 'merge_method', false) ?? 'squash', title: text(args, 'commit_title', false), message: text(args, 'commit_message', false) });
      return { sha: result.sha, merged: true, message: result.message, number: pull.number };
    },
  },
];

/** The tools/list answer: computed once, so every call is byte-for-byte the same. */
export const TOOL_LIST = TOOLS.map((tool) => ({
  name: tool.name,
  title: (tool.annotations?.title as string | undefined) ?? tool.name,
  description: tool.description,
  inputSchema: tool.inputSchema,
  annotations: tool.annotations ?? {},
}));

export function findTool(name: string): ToolDef | undefined {
  return TOOLS.find((tool) => tool.name === name);
}
