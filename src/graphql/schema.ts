// The GitHub GraphQL v4 subset this forge serves: the queries and mutations gh and github-mcp-server send
// (viewer, repository and its issues/pull requests, node lookup, search, and the issue / pull request /
// comment mutations). Runtime values are small wrappers ({__typename, repo, issue, ...}) over the store.

import { config, isoAt } from '../config.ts';
import { HttpError } from '../http.ts';
import { decodeNodeId, nodeId } from '../ids.ts';
import {
  addComment,
  createIssue,
  createPull,
  createRepository,
  labelKeys,
  mergePull,
  pullBaseSha,
  pullCommits,
  pullFiles,
  pullHeadSha,
  pullMergeable,
  setIssueState,
  updateIssue,
  updatePull,
} from '../ops.ts';
import { lookupRepo, store, type Comment, type Issue, type Label, type Pull, type Repo } from '../store.ts';
import { buildSchema, FieldError, type ArgDef, type FieldDef, type Schema, type TypeDef } from './execute.ts';

// ------------------------------------------------------------------ runtime values

interface RepoValue {
  __typename: 'Repository';
  repo: Repo;
}
interface IssueValue {
  __typename: 'Issue';
  repo: Repo;
  issue: Issue;
}
interface PullValue {
  __typename: 'PullRequest';
  repo: Repo;
  pull: Pull;
}
interface CommentValue {
  __typename: 'IssueComment';
  repo: Repo;
  issue: Issue;
  comment: Comment;
}
interface LabelValue {
  __typename: 'Label';
  repo: Repo;
  label: Label;
}
const USER = { __typename: 'User' as const };

const repoValue = (repo: Repo): RepoValue => ({ __typename: 'Repository', repo });
const issueValue = (repo: Repo, issue: Issue): IssueValue => ({ __typename: 'Issue', repo, issue });
const pullValue = (repo: Repo, pull: Pull): PullValue => ({ __typename: 'PullRequest', repo, pull });

function webUrl(path: string): string {
  return `http://forge.local/${path}`;
}

// ------------------------------------------------------------------ connections

function cursorFor(index: number): string {
  return Buffer.from(`cursor:v2:${index}`, 'utf8').toString('base64');
}

function indexOfCursor(cursor: unknown): number | null {
  if (typeof cursor !== 'string' || cursor === '') return null;
  const decoded = Buffer.from(cursor, 'base64').toString('utf8');
  const match = /^cursor:v2:(\d+)$/.exec(decoded);
  if (match === null) throw new FieldError(`\`${cursor}\` does not appear to be a valid cursor.`, 'INVALID_CURSOR_ARGUMENTS');
  return Number(match[1]);
}

const CONNECTION_ARGS: Record<string, ArgDef> = {
  first: { type: 'Int' },
  last: { type: 'Int' },
  after: { type: 'String' },
  before: { type: 'String' },
};

function connection<T>(items: readonly T[], args: Record<string, any>) {
  let start = 0;
  let end = items.length;
  const after = indexOfCursor(args.after);
  const before = indexOfCursor(args.before);
  if (after !== null) start = Math.max(start, after + 1);
  if (before !== null) end = Math.min(end, before);
  if (typeof args.first === 'number') {
    if (args.first < 0) throw new FieldError('`first` on a connection cannot be less than zero.', 'INVALID_PAGINATION');
    end = Math.min(end, start + args.first);
  }
  if (typeof args.last === 'number') {
    if (args.last < 0) throw new FieldError('`last` on a connection cannot be less than zero.', 'INVALID_PAGINATION');
    start = Math.max(start, end - args.last);
  }
  if (typeof args.first !== 'number' && typeof args.last !== 'number') end = Math.min(end, start + 100);
  if (start > end) start = end;
  const slice = items.slice(start, end);
  return {
    totalCount: items.length,
    nodes: slice,
    edges: slice.map((node, i) => ({ node, cursor: cursorFor(start + i) })),
    pageInfo: {
      hasNextPage: end < items.length,
      hasPreviousPage: start > 0,
      startCursor: slice.length > 0 ? cursorFor(start) : null,
      endCursor: slice.length > 0 ? cursorFor(end - 1) : null,
    },
  };
}

function connectionTypes(name: string, node: string): TypeDef[] {
  return [
    {
      kind: 'OBJECT',
      name: `${name}Connection`,
      fields: {
        totalCount: { type: 'Int!' },
        nodes: { type: `[${node}]` },
        edges: { type: `[${name}Edge]` },
        pageInfo: { type: 'PageInfo!' },
      },
    },
    { kind: 'OBJECT', name: `${name}Edge`, fields: { node: { type: node }, cursor: { type: 'String!' } } },
  ];
}

// ------------------------------------------------------------------ lookups

function nodeById(id: unknown): RepoValue | IssueValue | PullValue | CommentValue | LabelValue | typeof USER | null {
  if (typeof id !== 'string') return null;
  const decoded = decodeNodeId(id);
  if (decoded === null) return null;
  const numeric = Number(decoded.id);
  switch (decoded.kind) {
    case 'Repository': {
      const repo = store.byId(numeric);
      return repo === undefined ? null : repoValue(repo);
    }
    case 'User':
      return numeric === config.viewer.id ? USER : null;
    case 'PullRequest':
      for (const repo of store.all()) {
        for (const pull of repo.pulls.values()) if (pull.id === numeric) return pullValue(repo, pull);
      }
      return null;
    case 'Issue':
      for (const repo of store.all()) {
        for (const issue of repo.issues.values()) {
          if (issue.id === numeric && !issue.isPull) return issueValue(repo, issue);
        }
      }
      return null;
    case 'IssueComment':
      for (const repo of store.all()) {
        for (const issue of repo.issues.values()) {
          const comment = issue.comments.find((c) => c.id === numeric);
          if (comment !== undefined) return { __typename: 'IssueComment', repo, issue, comment };
        }
      }
      return null;
    case 'Label':
      for (const repo of store.all()) {
        for (const label of repo.labels.values()) if (label.id === numeric) return { __typename: 'Label', repo, label };
      }
      return null;
    default:
      return null;
  }
}

function notFoundNode(id: unknown): FieldError {
  return new FieldError(`Could not resolve to a node with the global id of '${String(id)}'`, 'NOT_FOUND');
}

function requireRepoNode(id: unknown): Repo {
  const node = nodeById(id);
  if (node === null || node.__typename !== 'Repository') throw notFoundNode(id);
  return node.repo;
}

function requireIssueNode(id: unknown): IssueValue {
  const node = nodeById(id);
  if (node === null || node.__typename !== 'Issue') throw notFoundNode(id);
  return node;
}

function requirePullNode(id: unknown): PullValue {
  const node = nodeById(id);
  if (node === null || node.__typename !== 'PullRequest') throw notFoundNode(id);
  return node;
}

function labelNames(repo: Repo, ids: unknown): string[] | undefined {
  if (!Array.isArray(ids)) return undefined;
  return ids.map((id) => {
    const node = nodeById(id);
    if (node === null || node.__typename !== 'Label' || node.repo !== repo) throw notFoundNode(id);
    return node.label.name;
  });
}

// ------------------------------------------------------------------ field helpers

const commonIssueFields = (pick: (v: any) => Issue): Record<string, FieldDef> => ({
  number: { type: 'Int!', resolve: (v) => pick(v).number },
  title: { type: 'String!', resolve: (v) => pick(v).title },
  body: { type: 'String!', resolve: (v) => pick(v).body ?? '' },
  bodyText: { type: 'String!', resolve: (v) => pick(v).body ?? '' },
  closed: { type: 'Boolean!', resolve: (v) => pick(v).state === 'closed' },
  closedAt: { type: 'DateTime', resolve: (v) => pick(v).closedAt },
  createdAt: { type: 'DateTime!', resolve: (v) => pick(v).createdAt },
  updatedAt: { type: 'DateTime!', resolve: (v) => pick(v).updatedAt },
  locked: { type: 'Boolean!', resolve: () => false },
  author: { type: 'Actor', resolve: () => USER },
  repository: { type: 'Repository!', resolve: (v) => repoValue(v.repo) },
  labels: {
    type: 'LabelConnection',
    args: { ...CONNECTION_ARGS },
    resolve: (v, args) => connection(pick(v).labels.map((k) => v.repo.labels.get(k)).filter((l: Label | undefined) => l !== undefined).map((label: Label) => ({ __typename: 'Label', repo: v.repo, label })), args),
  },
  comments: {
    type: 'IssueCommentConnection!',
    args: { ...CONNECTION_ARGS },
    resolve: (v, args) => connection(pick(v).comments.map((comment) => ({ __typename: 'IssueComment', repo: v.repo, issue: pick(v), comment })), args),
  },
  assignees: { type: 'UserConnection!', args: { ...CONNECTION_ARGS }, resolve: (_v, args) => connection([], args) },
  milestone: { type: 'Milestone', resolve: () => null },
  viewerCanUpdate: { type: 'Boolean!', resolve: () => true },
});

function sortIssues(list: Issue[], orderBy: { field?: string; direction?: string } | undefined): Issue[] {
  const field = orderBy?.field ?? 'CREATED_AT';
  const direction = orderBy?.direction ?? 'ASC';
  const key = (i: Issue): number => (field === 'COMMENTS' ? i.comments.length : field === 'UPDATED_AT' ? Date.parse(i.updatedAt) : i.seq);
  const sorted = [...list].sort((a, b) => key(a) - key(b) || a.number - b.number);
  return direction === 'DESC' ? sorted.reverse() : sorted;
}

function pullState(pull: Pull): 'OPEN' | 'CLOSED' | 'MERGED' {
  return pull.merged ? 'MERGED' : pull.issue.state === 'open' ? 'OPEN' : 'CLOSED';
}

// ------------------------------------------------------------------ the types

const ENUMS: TypeDef[] = [
  { kind: 'ENUM', name: 'IssueState', values: ['OPEN', 'CLOSED'] },
  { kind: 'ENUM', name: 'IssueStateReason', values: ['COMPLETED', 'NOT_PLANNED', 'REOPENED', 'DUPLICATE'] },
  { kind: 'ENUM', name: 'IssueClosedStateReason', values: ['COMPLETED', 'NOT_PLANNED', 'DUPLICATE'] },
  { kind: 'ENUM', name: 'PullRequestState', values: ['OPEN', 'CLOSED', 'MERGED'] },
  { kind: 'ENUM', name: 'MergeableState', values: ['MERGEABLE', 'CONFLICTING', 'UNKNOWN'] },
  { kind: 'ENUM', name: 'PullRequestMergeMethod', values: ['MERGE', 'SQUASH', 'REBASE'] },
  { kind: 'ENUM', name: 'RepositoryPermission', values: ['ADMIN', 'MAINTAIN', 'WRITE', 'TRIAGE', 'READ'] },
  { kind: 'ENUM', name: 'RepositoryVisibility', values: ['PUBLIC', 'PRIVATE', 'INTERNAL'] },
  { kind: 'ENUM', name: 'SearchType', values: ['ISSUE', 'ISSUE_ADVANCED', 'REPOSITORY', 'USER', 'DISCUSSION'] },
  { kind: 'ENUM', name: 'IssueOrderField', values: ['CREATED_AT', 'UPDATED_AT', 'COMMENTS'] },
  { kind: 'ENUM', name: 'IssueCommentOrderField', values: ['UPDATED_AT'] },
  { kind: 'ENUM', name: 'OrderDirection', values: ['ASC', 'DESC'] },
  { kind: 'ENUM', name: 'PatchStatus', values: ['ADDED', 'DELETED', 'RENAMED', 'COPIED', 'MODIFIED', 'CHANGED'] },
  { kind: 'ENUM', name: 'RepositoryAffiliation', values: ['OWNER', 'COLLABORATOR', 'ORGANIZATION_MEMBER'] },
  { kind: 'ENUM', name: 'RepositoryOrderField', values: ['CREATED_AT', 'UPDATED_AT', 'PUSHED_AT', 'NAME', 'STARGAZERS'] },
];

const SCALARS: TypeDef[] = [
  { kind: 'SCALAR', name: 'DateTime' },
  { kind: 'SCALAR', name: 'URI' },
  { kind: 'SCALAR', name: 'GitObjectID' },
  { kind: 'SCALAR', name: 'HTML' },
];

const INPUTS: TypeDef[] = [
  { kind: 'INPUT_OBJECT', name: 'IssueOrder', fields: { field: { type: 'IssueOrderField!' }, direction: { type: 'OrderDirection!' } } },
  { kind: 'INPUT_OBJECT', name: 'IssueCommentOrder', fields: { field: { type: 'IssueCommentOrderField!' }, direction: { type: 'OrderDirection!' } } },
  { kind: 'INPUT_OBJECT', name: 'RepositoryOrder', fields: { field: { type: 'RepositoryOrderField!' }, direction: { type: 'OrderDirection!' } } },
  {
    kind: 'INPUT_OBJECT',
    name: 'IssueFilters',
    fields: { states: { type: '[IssueState!]' }, labels: { type: '[String!]' }, assignee: { type: 'String' }, createdBy: { type: 'String' }, mentioned: { type: 'String' }, since: { type: 'DateTime' } },
  },
  {
    kind: 'INPUT_OBJECT',
    name: 'CreateIssueInput',
    fields: {
      repositoryId: { type: 'ID!' },
      title: { type: 'String!' },
      body: { type: 'String' },
      labelIds: { type: '[ID!]' },
      assigneeIds: { type: '[ID!]' },
      milestoneId: { type: 'ID' },
      projectIds: { type: '[ID!]' },
      issueTypeId: { type: 'ID' },
      issueTemplate: { type: 'String' },
      clientMutationId: { type: 'String' },
    },
  },
  { kind: 'INPUT_OBJECT', name: 'CloseIssueInput', fields: { issueId: { type: 'ID!' }, stateReason: { type: 'IssueClosedStateReason' }, duplicateIssueId: { type: 'ID' }, clientMutationId: { type: 'String' } } },
  { kind: 'INPUT_OBJECT', name: 'ReopenIssueInput', fields: { issueId: { type: 'ID!' }, clientMutationId: { type: 'String' } } },
  {
    kind: 'INPUT_OBJECT',
    name: 'UpdateIssueInput',
    fields: { id: { type: 'ID!' }, title: { type: 'String' }, body: { type: 'String' }, state: { type: 'IssueState' }, labelIds: { type: '[ID!]' }, assigneeIds: { type: '[ID!]' }, milestoneId: { type: 'ID' }, projectIds: { type: '[ID!]' }, clientMutationId: { type: 'String' } },
  },
  { kind: 'INPUT_OBJECT', name: 'AddCommentInput', fields: { subjectId: { type: 'ID!' }, body: { type: 'String!' }, clientMutationId: { type: 'String' } } },
  {
    kind: 'INPUT_OBJECT',
    name: 'CreatePullRequestInput',
    fields: {
      repositoryId: { type: 'ID!' },
      baseRefName: { type: 'String!' },
      headRefName: { type: 'String!' },
      headRepositoryId: { type: 'ID' },
      title: { type: 'String!' },
      body: { type: 'String' },
      draft: { type: 'Boolean' },
      maintainerCanModify: { type: 'Boolean' },
      clientMutationId: { type: 'String' },
    },
  },
  {
    kind: 'INPUT_OBJECT',
    name: 'MergePullRequestInput',
    fields: {
      pullRequestId: { type: 'ID!' },
      mergeMethod: { type: 'PullRequestMergeMethod' },
      commitHeadline: { type: 'String' },
      commitBody: { type: 'String' },
      expectedHeadOid: { type: 'GitObjectID' },
      authorEmail: { type: 'String' },
      clientMutationId: { type: 'String' },
    },
  },
  { kind: 'INPUT_OBJECT', name: 'ClosePullRequestInput', fields: { pullRequestId: { type: 'ID!' }, clientMutationId: { type: 'String' } } },
  { kind: 'INPUT_OBJECT', name: 'ReopenPullRequestInput', fields: { pullRequestId: { type: 'ID!' }, clientMutationId: { type: 'String' } } },
  {
    kind: 'INPUT_OBJECT',
    name: 'UpdatePullRequestInput',
    fields: { pullRequestId: { type: 'ID!' }, title: { type: 'String' }, body: { type: 'String' }, baseRefName: { type: 'String' }, state: { type: 'PullRequestUpdateState' }, labelIds: { type: '[ID!]' }, assigneeIds: { type: '[ID!]' }, clientMutationId: { type: 'String' } },
  },
  { kind: 'ENUM', name: 'PullRequestUpdateState', values: ['OPEN', 'CLOSED'] },
  {
    kind: 'INPUT_OBJECT',
    name: 'CreateRepositoryInput',
    fields: {
      name: { type: 'String!' },
      ownerId: { type: 'ID' },
      description: { type: 'String' },
      visibility: { type: 'RepositoryVisibility!' },
      template: { type: 'Boolean' },
      homepageUrl: { type: 'URI' },
      hasWikiEnabled: { type: 'Boolean' },
      hasIssuesEnabled: { type: 'Boolean' },
      teamId: { type: 'ID' },
      clientMutationId: { type: 'String' },
    },
  },
  { kind: 'INPUT_OBJECT', name: 'AddLabelsToLabelableInput', fields: { labelableId: { type: 'ID!' }, labelIds: { type: '[ID!]!' }, clientMutationId: { type: 'String' } } },
];

const OBJECTS: TypeDef[] = [
  {
    kind: 'OBJECT',
    name: 'Query',
    fields: {
      viewer: { type: 'User!', resolve: () => USER },
      user: { type: 'User', args: { login: { type: 'String!' } }, resolve: (_p, args) => (String(args.login).toLowerCase() === config.viewer.login.toLowerCase() ? USER : null) },
      repositoryOwner: { type: 'RepositoryOwner', args: { login: { type: 'String!' } }, resolve: (_p, args) => (String(args.login).toLowerCase() === config.viewer.login.toLowerCase() ? USER : null) },
      repository: {
        type: 'Repository',
        args: { owner: { type: 'String!' }, name: { type: 'String!' }, followRenames: { type: 'Boolean', defaultValue: true } },
        resolve: (_p, args) => {
          const repo = lookupRepo(args.owner, args.name);
          return repo === undefined ? null : repoValue(repo);
        },
      },
      node: { type: 'Node', args: { id: { type: 'ID!' } }, resolve: (_p, args) => nodeById(args.id) },
      nodes: { type: '[Node]!', args: { ids: { type: '[ID!]!' } }, resolve: (_p, args) => (args.ids as unknown[]).map((id) => nodeById(id)) },
      search: {
        type: 'SearchResultItemConnection!',
        args: { query: { type: 'String!' }, type: { type: 'SearchType!' }, ...CONNECTION_ARGS },
        resolve: (_p, args) => search(String(args.query), String(args.type), args),
      },
      rateLimit: { type: 'RateLimit', args: { dryRun: { type: 'Boolean', defaultValue: false } }, resolve: () => ({ limit: 5000, cost: 1, remaining: 4999, used: 1, resetAt: isoAt(3600), nodeCount: 0 }) },
    },
  },
  {
    kind: 'OBJECT',
    name: 'Mutation',
    fields: {
      createIssue: {
        type: 'CreateIssuePayload',
        args: { input: { type: 'CreateIssueInput!' } },
        resolve: (_p, { input }) => {
          const repo = requireRepoNode(input.repositoryId);
          const issue = createIssue(repo, { title: input.title, body: input.body, labels: labelNames(repo, input.labelIds) });
          return { clientMutationId: input.clientMutationId ?? null, issue: issueValue(repo, issue) };
        },
      },
      closeIssue: {
        type: 'CloseIssuePayload',
        args: { input: { type: 'CloseIssueInput!' } },
        resolve: (_p, { input }) => {
          const { repo, issue } = requireIssueNode(input.issueId);
          setIssueState(issue, 'closed', typeof input.stateReason === 'string' ? input.stateReason.toLowerCase() : 'completed');
          return { clientMutationId: input.clientMutationId ?? null, issue: issueValue(repo, issue) };
        },
      },
      reopenIssue: {
        type: 'ReopenIssuePayload',
        args: { input: { type: 'ReopenIssueInput!' } },
        resolve: (_p, { input }) => {
          const { repo, issue } = requireIssueNode(input.issueId);
          setIssueState(issue, 'open', 'reopened');
          return { clientMutationId: input.clientMutationId ?? null, issue: issueValue(repo, issue) };
        },
      },
      updateIssue: {
        type: 'UpdateIssuePayload',
        args: { input: { type: 'UpdateIssueInput!' } },
        resolve: (_p, { input }) => {
          const { repo, issue } = requireIssueNode(input.id);
          const patch: Record<string, unknown> = {};
          if (input.title !== undefined && input.title !== null) patch.title = input.title;
          if (input.body !== undefined) patch.body = input.body;
          if (typeof input.state === 'string') patch.state = input.state.toLowerCase();
          const labels = labelNames(repo, input.labelIds);
          if (labels !== undefined) patch.labels = labels;
          updateIssue(repo, issue, patch);
          return { clientMutationId: input.clientMutationId ?? null, issue: issueValue(repo, issue) };
        },
      },
      addComment: {
        type: 'AddCommentPayload',
        args: { input: { type: 'AddCommentInput!' } },
        resolve: (_p, { input }) => {
          const node = nodeById(input.subjectId);
          let repo: Repo;
          let issue: Issue;
          if (node !== null && node.__typename === 'Issue') ({ repo, issue } = node);
          else if (node !== null && node.__typename === 'PullRequest') {
            repo = node.repo;
            issue = node.pull.issue;
          } else throw notFoundNode(input.subjectId);
          const comment = addComment(issue, input.body);
          const value: CommentValue = { __typename: 'IssueComment', repo, issue, comment };
          return {
            clientMutationId: input.clientMutationId ?? null,
            commentEdge: { node: value, cursor: cursorFor(issue.comments.length - 1) },
            timelineEdge: { node: value, cursor: cursorFor(issue.comments.length - 1) },
            subject: node,
          };
        },
      },
      addLabelsToLabelable: {
        type: 'AddLabelsToLabelablePayload',
        args: { input: { type: 'AddLabelsToLabelableInput!' } },
        resolve: (_p, { input }) => {
          const node = nodeById(input.labelableId);
          if (node === null || (node.__typename !== 'Issue' && node.__typename !== 'PullRequest')) throw notFoundNode(input.labelableId);
          const issue = node.__typename === 'Issue' ? node.issue : node.pull.issue;
          const keys = labelKeys(node.repo, labelNames(node.repo, input.labelIds));
          issue.labels = [...new Set([...issue.labels, ...keys])];
          return { clientMutationId: input.clientMutationId ?? null, labelable: node };
        },
      },
      createPullRequest: {
        type: 'CreatePullRequestPayload',
        args: { input: { type: 'CreatePullRequestInput!' } },
        resolve: (_p, { input }) => {
          const repo = requireRepoNode(input.repositoryId);
          const pull = createPull(repo, { title: input.title, body: input.body, head: input.headRefName, base: input.baseRefName, draft: input.draft === true });
          return { clientMutationId: input.clientMutationId ?? null, pullRequest: pullValue(repo, pull) };
        },
      },
      mergePullRequest: {
        type: 'MergePullRequestPayload',
        args: { input: { type: 'MergePullRequestInput!' } },
        resolve: (_p, { input }) => {
          const { repo, pull } = requirePullNode(input.pullRequestId);
          try {
            mergePull(repo, pull, { method: typeof input.mergeMethod === 'string' ? input.mergeMethod.toLowerCase() : 'merge', title: input.commitHeadline, message: input.commitBody, sha: input.expectedHeadOid });
          } catch (err) {
            if (err instanceof HttpError && err.status === 405) throw new FieldError('Pull Request is not mergeable', 'UNPROCESSABLE');
            throw err;
          }
          return { clientMutationId: input.clientMutationId ?? null, pullRequest: pullValue(repo, pull) };
        },
      },
      closePullRequest: {
        type: 'ClosePullRequestPayload',
        args: { input: { type: 'ClosePullRequestInput!' } },
        resolve: (_p, { input }) => {
          const { repo, pull } = requirePullNode(input.pullRequestId);
          updatePull(repo, pull, { state: 'closed' });
          return { clientMutationId: input.clientMutationId ?? null, pullRequest: pullValue(repo, pull) };
        },
      },
      reopenPullRequest: {
        type: 'ReopenPullRequestPayload',
        args: { input: { type: 'ReopenPullRequestInput!' } },
        resolve: (_p, { input }) => {
          const { repo, pull } = requirePullNode(input.pullRequestId);
          updatePull(repo, pull, { state: 'open' });
          return { clientMutationId: input.clientMutationId ?? null, pullRequest: pullValue(repo, pull) };
        },
      },
      updatePullRequest: {
        type: 'UpdatePullRequestPayload',
        args: { input: { type: 'UpdatePullRequestInput!' } },
        resolve: (_p, { input }) => {
          const { repo, pull } = requirePullNode(input.pullRequestId);
          const patch: Record<string, unknown> = {};
          if (typeof input.title === 'string') patch.title = input.title;
          if (input.body !== undefined) patch.body = input.body;
          if (typeof input.baseRefName === 'string') patch.base = input.baseRefName;
          if (typeof input.state === 'string') patch.state = input.state.toLowerCase();
          updatePull(repo, pull, patch);
          return { clientMutationId: input.clientMutationId ?? null, pullRequest: pullValue(repo, pull) };
        },
      },
      createRepository: {
        type: 'CreateRepositoryPayload',
        args: { input: { type: 'CreateRepositoryInput!' } },
        resolve: (_p, { input }) => {
          const repo = createRepository({ name: input.name, description: input.description, visibility: String(input.visibility).toLowerCase(), homepage: input.homepageUrl });
          return { clientMutationId: input.clientMutationId ?? null, repository: repoValue(repo) };
        },
      },
    },
  },
  ...payload('CreateIssuePayload', { issue: 'Issue' }),
  ...payload('CloseIssuePayload', { issue: 'Issue' }),
  ...payload('ReopenIssuePayload', { issue: 'Issue' }),
  ...payload('UpdateIssuePayload', { issue: 'Issue' }),
  ...payload('AddCommentPayload', { commentEdge: 'IssueCommentEdge', timelineEdge: 'IssueTimelineItemEdge', subject: 'Node' }),
  ...payload('AddLabelsToLabelablePayload', { labelable: 'Labelable' }),
  ...payload('CreatePullRequestPayload', { pullRequest: 'PullRequest' }),
  ...payload('MergePullRequestPayload', { pullRequest: 'PullRequest' }),
  ...payload('ClosePullRequestPayload', { pullRequest: 'PullRequest' }),
  ...payload('ReopenPullRequestPayload', { pullRequest: 'PullRequest' }),
  ...payload('UpdatePullRequestPayload', { pullRequest: 'PullRequest' }),
  ...payload('CreateRepositoryPayload', { repository: 'Repository' }),
  { kind: 'OBJECT', name: 'IssueTimelineItemEdge', fields: { node: { type: 'IssueComment' }, cursor: { type: 'String!' } } },
  {
    kind: 'OBJECT',
    name: 'User',
    interfaces: ['Node', 'Actor', 'RepositoryOwner'],
    fields: {
      id: { type: 'ID!', resolve: () => nodeId('User', config.viewer.id) },
      databaseId: { type: 'Int', resolve: () => config.viewer.id },
      login: { type: 'String!', resolve: () => config.viewer.login },
      name: { type: 'String', resolve: () => config.viewer.name },
      email: { type: 'String!', resolve: () => config.viewer.email },
      url: { type: 'URI!', resolve: () => webUrl(config.viewer.login) },
      avatarUrl: { type: 'URI!', args: { size: { type: 'Int' } }, resolve: () => webUrl(`avatars/${config.viewer.login}`) },
      company: { type: 'String', resolve: () => null },
      bio: { type: 'String', resolve: () => null },
      createdAt: { type: 'DateTime!', resolve: () => '2025-01-01T00:00:00Z' },
      repositories: {
        type: 'RepositoryConnection!',
        args: { ...CONNECTION_ARGS, privacy: { type: 'RepositoryVisibility' }, ownerAffiliations: { type: '[RepositoryAffiliation]' }, orderBy: { type: 'RepositoryOrder' }, isFork: { type: 'Boolean' } },
        resolve: (_v, args) => {
          let repos = store.list(config.viewer.login);
          if (args.privacy === 'PUBLIC') repos = repos.filter((r) => !r.isPrivate);
          if (args.privacy === 'PRIVATE') repos = repos.filter((r) => r.isPrivate);
          return connection(repos.map(repoValue), args);
        },
      },
      repository: { type: 'Repository', args: { name: { type: 'String!' } }, resolve: (_v, args) => {
        const repo = lookupRepo(config.viewer.login, args.name);
        return repo === undefined ? null : repoValue(repo);
      } },
    },
  },
  {
    kind: 'OBJECT',
    name: 'Repository',
    interfaces: ['Node'],
    fields: {
      id: { type: 'ID!', resolve: (v: RepoValue) => nodeId('Repository', v.repo.id) },
      databaseId: { type: 'Int', resolve: (v: RepoValue) => v.repo.id },
      name: { type: 'String!', resolve: (v: RepoValue) => v.repo.name },
      nameWithOwner: { type: 'String!', resolve: (v: RepoValue) => v.repo.fullName },
      owner: { type: 'RepositoryOwner!', resolve: () => USER },
      description: { type: 'String', resolve: (v: RepoValue) => v.repo.description },
      homepageUrl: { type: 'URI', resolve: (v: RepoValue) => v.repo.homepage },
      url: { type: 'URI!', resolve: (v: RepoValue) => webUrl(v.repo.fullName) },
      sshUrl: { type: 'String!', resolve: (v: RepoValue) => `git@forge.local:${v.repo.fullName}.git` },
      isPrivate: { type: 'Boolean!', resolve: (v: RepoValue) => v.repo.isPrivate },
      visibility: { type: 'RepositoryVisibility!', resolve: (v: RepoValue) => (v.repo.isPrivate ? 'PRIVATE' : 'PUBLIC') },
      isFork: { type: 'Boolean!', resolve: () => false },
      isArchived: { type: 'Boolean!', resolve: () => false },
      isTemplate: { type: 'Boolean!', resolve: () => false },
      isEmpty: { type: 'Boolean!', resolve: (v: RepoValue) => v.repo.isEmpty() },
      isInOrganization: { type: 'Boolean!', resolve: () => false },
      hasIssuesEnabled: { type: 'Boolean!', resolve: () => true },
      hasWikiEnabled: { type: 'Boolean!', resolve: () => false },
      hasProjectsEnabled: { type: 'Boolean!', resolve: () => true },
      hasDiscussionsEnabled: { type: 'Boolean!', resolve: () => false },
      mergeCommitAllowed: { type: 'Boolean!', resolve: () => true },
      squashMergeAllowed: { type: 'Boolean!', resolve: () => true },
      rebaseMergeAllowed: { type: 'Boolean!', resolve: () => true },
      deleteBranchOnMerge: { type: 'Boolean!', resolve: () => false },
      stargazerCount: { type: 'Int!', resolve: () => 0 },
      forkCount: { type: 'Int!', resolve: () => 0 },
      viewerPermission: { type: 'RepositoryPermission', resolve: () => 'ADMIN' },
      viewerCanAdminister: { type: 'Boolean!', resolve: () => true },
      createdAt: { type: 'DateTime!', resolve: (v: RepoValue) => v.repo.createdAt },
      updatedAt: { type: 'DateTime!', resolve: (v: RepoValue) => v.repo.updatedAt },
      pushedAt: { type: 'DateTime', resolve: (v: RepoValue) => v.repo.pushedAt },
      parent: { type: 'Repository', resolve: () => null },
      defaultBranchRef: {
        type: 'Ref',
        resolve: (v: RepoValue) => {
          const tip = v.repo.branchTip(v.repo.defaultBranch);
          return tip === undefined ? null : { repo: v.repo, name: v.repo.defaultBranch, prefix: 'refs/heads/', target: tip };
        },
      },
      ref: {
        type: 'Ref',
        args: { qualifiedName: { type: 'String!' } },
        resolve: (v: RepoValue, args) => {
          const name = String(args.qualifiedName);
          const full = name.startsWith('refs/') ? name : `refs/heads/${name}`;
          const tip = v.repo.refs.get(full);
          if (tip === undefined) return null;
          const slash = full.indexOf('/', 5);
          return { repo: v.repo, name: full.slice(slash + 1), prefix: full.slice(0, slash + 1), target: tip };
        },
      },
      refs: {
        type: 'RefConnection',
        args: { ...CONNECTION_ARGS, refPrefix: { type: 'String!' }, query: { type: 'String' } },
        resolve: (v: RepoValue, args) => {
          const prefix = String(args.refPrefix);
          const refs = [...v.repo.refs.entries()]
            .filter(([name]) => name.startsWith(prefix) && (args.query == null || name.includes(String(args.query))))
            .sort()
            .map(([name, target]) => ({ repo: v.repo, name: name.slice(prefix.length), prefix, target }));
          return connection(refs, args);
        },
      },
      issue: {
        type: 'Issue',
        args: { number: { type: 'Int!' } },
        resolve: (v: RepoValue, args) => {
          const issue = v.repo.issues.get(args.number);
          if (issue === undefined || issue.isPull) {
            if (issue === undefined) return null;
            throw new FieldError(`Could not resolve to an Issue with the number of ${args.number}.`, 'NOT_FOUND');
          }
          return issueValue(v.repo, issue);
        },
      },
      issueOrPullRequest: {
        type: 'IssueOrPullRequest',
        args: { number: { type: 'Int!' } },
        resolve: (v: RepoValue, args) => {
          const issue = v.repo.issues.get(args.number);
          if (issue === undefined) return null;
          const pull = issue.isPull ? v.repo.pulls.get(issue.number) : undefined;
          return pull !== undefined ? pullValue(v.repo, pull) : issueValue(v.repo, issue);
        },
      },
      issues: {
        type: 'IssueConnection!',
        args: { ...CONNECTION_ARGS, states: { type: '[IssueState!]' }, orderBy: { type: 'IssueOrder' }, labels: { type: '[String!]' }, filterBy: { type: 'IssueFilters' } },
        resolve: (v: RepoValue, args) => {
          const states: string[] | undefined = args.states ?? args.filterBy?.states;
          const labels: string[] | undefined = args.labels ?? args.filterBy?.labels;
          let list = [...v.repo.issues.values()].filter((i) => !i.isPull);
          if (states !== undefined && states !== null && states.length > 0) list = list.filter((i) => states.includes(i.state.toUpperCase()));
          if (labels !== undefined && labels !== null && labels.length > 0) list = list.filter((i) => labels.every((l) => i.labels.includes(l.toLowerCase())));
          return connection(sortIssues(list, args.orderBy).map((i) => issueValue(v.repo, i)), args);
        },
      },
      pullRequest: {
        type: 'PullRequest',
        args: { number: { type: 'Int!' } },
        resolve: (v: RepoValue, args) => {
          const pull = v.repo.pulls.get(args.number);
          return pull === undefined ? null : pullValue(v.repo, pull);
        },
      },
      pullRequests: {
        type: 'PullRequestConnection!',
        args: { ...CONNECTION_ARGS, states: { type: '[PullRequestState!]' }, headRefName: { type: 'String' }, baseRefName: { type: 'String' }, labels: { type: '[String!]' }, orderBy: { type: 'IssueOrder' } },
        resolve: (v: RepoValue, args) => {
          let list = [...v.repo.pulls.values()];
          const states: string[] | undefined = args.states;
          if (states !== undefined && states !== null && states.length > 0) list = list.filter((p) => states.includes(pullState(p)));
          if (typeof args.headRefName === 'string') list = list.filter((p) => p.headRef === args.headRefName);
          if (typeof args.baseRefName === 'string') list = list.filter((p) => p.baseRef === args.baseRefName);
          const issues = sortIssues(list.map((p) => p.issue), args.orderBy);
          return connection(issues.map((i) => pullValue(v.repo, v.repo.pulls.get(i.number)!)), args);
        },
      },
      labels: {
        type: 'LabelConnection',
        args: { ...CONNECTION_ARGS, query: { type: 'String' } },
        resolve: (v: RepoValue, args) => {
          let labels = [...v.repo.labels.values()].sort((a, b) => a.id - b.id);
          if (typeof args.query === 'string') labels = labels.filter((l) => l.name.toLowerCase().includes(String(args.query).toLowerCase()));
          return connection(labels.map((label) => ({ __typename: 'Label', repo: v.repo, label })), args);
        },
      },
      label: { type: 'Label', args: { name: { type: 'String!' } }, resolve: (v: RepoValue, args) => {
        const label = v.repo.labels.get(String(args.name).toLowerCase());
        return label === undefined ? null : { __typename: 'Label', repo: v.repo, label };
      } },
      assignableUsers: { type: 'UserConnection!', args: { ...CONNECTION_ARGS, query: { type: 'String' } }, resolve: (_v, args) => connection([USER], args) },
      mentionableUsers: { type: 'UserConnection!', args: { ...CONNECTION_ARGS, query: { type: 'String' } }, resolve: (_v, args) => connection([USER], args) },
      milestones: { type: 'MilestoneConnection', args: { ...CONNECTION_ARGS }, resolve: (_v, args) => connection([], args) },
    },
  },
  {
    kind: 'OBJECT',
    name: 'Ref',
    interfaces: ['Node'],
    fields: {
      id: { type: 'ID!', resolve: (r) => nodeId('Ref', `${r.repo.id}:${r.prefix}${r.name}`) },
      name: { type: 'String!' },
      prefix: { type: 'String!' },
      repository: { type: 'Repository!', resolve: (r) => repoValue(r.repo) },
      target: { type: 'GitObject', resolve: (r) => ({ __typename: r.repo.get(r.target)?.type === 'tag' ? 'Tag' : 'Commit', repo: r.repo, oid: r.target }) },
    },
  },
  {
    kind: 'OBJECT',
    name: 'Commit',
    interfaces: ['Node', 'GitObject'],
    fields: {
      id: { type: 'ID!', resolve: (c) => nodeId('Commit', c.oid) },
      oid: { type: 'GitObjectID!' },
      abbreviatedOid: { type: 'String!', resolve: (c) => String(c.oid).slice(0, 7) },
      message: { type: 'String!', resolve: (c) => c.repo.commit(c.oid)?.message.replace(/\n$/, '') ?? '' },
      messageHeadline: { type: 'String!', resolve: (c) => (c.repo.commit(c.oid)?.message ?? '').split('\n')[0] },
      committedDate: { type: 'DateTime!', resolve: () => isoAt(0) },
      url: { type: 'URI!', resolve: (c) => webUrl(`${c.repo.fullName}/commit/${c.oid}`) },
    },
  },
  {
    kind: 'OBJECT',
    name: 'Tag',
    interfaces: ['Node', 'GitObject'],
    fields: {
      id: { type: 'ID!', resolve: (c) => nodeId('Commit', c.oid) },
      oid: { type: 'GitObjectID!' },
      abbreviatedOid: { type: 'String!', resolve: (c) => String(c.oid).slice(0, 7) },
    },
  },
  {
    kind: 'OBJECT',
    name: 'Issue',
    interfaces: ['Node', 'Labelable', 'Comment'],
    fields: {
      id: { type: 'ID!', resolve: (v: IssueValue) => nodeId('Issue', v.issue.id) },
      databaseId: { type: 'Int', resolve: (v: IssueValue) => v.issue.id },
      url: { type: 'URI!', resolve: (v: IssueValue) => webUrl(`${v.repo.fullName}/issues/${v.issue.number}`) },
      state: { type: 'IssueState!', resolve: (v: IssueValue) => v.issue.state.toUpperCase() },
      stateReason: { type: 'IssueStateReason', resolve: (v: IssueValue) => (v.issue.stateReason === null ? null : v.issue.stateReason.toUpperCase()) },
      ...commonIssueFields((v: IssueValue) => v.issue),
    },
  },
  {
    kind: 'OBJECT',
    name: 'PullRequest',
    interfaces: ['Node', 'Labelable', 'Comment'],
    fields: {
      id: { type: 'ID!', resolve: (v: PullValue) => nodeId('PullRequest', v.pull.id) },
      databaseId: { type: 'Int', resolve: (v: PullValue) => v.pull.id },
      url: { type: 'URI!', resolve: (v: PullValue) => webUrl(`${v.repo.fullName}/pull/${v.pull.number}`) },
      state: { type: 'PullRequestState!', resolve: (v: PullValue) => pullState(v.pull) },
      merged: { type: 'Boolean!', resolve: (v: PullValue) => v.pull.merged },
      mergedAt: { type: 'DateTime', resolve: (v: PullValue) => v.pull.mergedAt },
      mergedBy: { type: 'Actor', resolve: (v: PullValue) => (v.pull.merged ? USER : null) },
      mergeCommit: { type: 'Commit', resolve: (v: PullValue) => (v.pull.mergeCommitSha === null ? null : { __typename: 'Commit', repo: v.repo, oid: v.pull.mergeCommitSha }) },
      isDraft: { type: 'Boolean!', resolve: (v: PullValue) => v.pull.draft },
      headRefName: { type: 'String!', resolve: (v: PullValue) => v.pull.headRef },
      baseRefName: { type: 'String!', resolve: (v: PullValue) => v.pull.baseRef },
      headRefOid: { type: 'GitObjectID!', resolve: (v: PullValue) => pullHeadSha(v.repo, v.pull) },
      baseRefOid: { type: 'GitObjectID!', resolve: (v: PullValue) => pullBaseSha(v.repo, v.pull) },
      headRepository: { type: 'Repository', resolve: (v: PullValue) => repoValue(v.repo) },
      baseRepository: { type: 'Repository', resolve: (v: PullValue) => repoValue(v.repo) },
      headRepositoryOwner: { type: 'RepositoryOwner', resolve: () => USER },
      isCrossRepository: { type: 'Boolean!', resolve: () => false },
      maintainerCanModify: { type: 'Boolean!', resolve: () => false },
      mergeable: {
        type: 'MergeableState!',
        resolve: (v: PullValue) => {
          const m = pullMergeable(v.repo, v.pull);
          return m === true ? 'MERGEABLE' : m === false ? 'CONFLICTING' : 'UNKNOWN';
        },
      },
      additions: { type: 'Int!', resolve: (v: PullValue) => pullFiles(v.repo, v.pull).reduce((n, f) => n + f.additions, 0) },
      deletions: { type: 'Int!', resolve: (v: PullValue) => pullFiles(v.repo, v.pull).reduce((n, f) => n + f.deletions, 0) },
      changedFiles: { type: 'Int!', resolve: (v: PullValue) => pullFiles(v.repo, v.pull).length },
      files: {
        type: 'PullRequestChangedFileConnection',
        args: { ...CONNECTION_ARGS },
        resolve: (v: PullValue, args) =>
          connection(
            pullFiles(v.repo, v.pull).map((f) => ({
              path: f.filename,
              additions: f.additions,
              deletions: f.deletions,
              changeType: f.status === 'added' ? 'ADDED' : f.status === 'removed' ? 'DELETED' : f.status === 'renamed' ? 'RENAMED' : 'MODIFIED',
            })),
            args,
          ),
      },
      commits: {
        type: 'PullRequestCommitConnection!',
        args: { ...CONNECTION_ARGS },
        resolve: (v: PullValue, args) => connection(pullCommits(v.repo, v.pull).map((oid) => ({ commit: { __typename: 'Commit', repo: v.repo, oid } })), args),
      },
      ...commonIssueFields((v: PullValue) => v.pull.issue),
    },
  },
  {
    kind: 'OBJECT',
    name: 'PullRequestChangedFile',
    fields: { path: { type: 'String!' }, additions: { type: 'Int!' }, deletions: { type: 'Int!' }, changeType: { type: 'PatchStatus!' } },
  },
  { kind: 'OBJECT', name: 'PullRequestCommit', fields: { commit: { type: 'Commit!' } } },
  {
    kind: 'OBJECT',
    name: 'IssueComment',
    interfaces: ['Node', 'Comment'],
    fields: {
      id: { type: 'ID!', resolve: (v: CommentValue) => nodeId('IssueComment', v.comment.id) },
      databaseId: { type: 'Int', resolve: (v: CommentValue) => v.comment.id },
      body: { type: 'String!', resolve: (v: CommentValue) => v.comment.body },
      bodyText: { type: 'String!', resolve: (v: CommentValue) => v.comment.body },
      author: { type: 'Actor', resolve: () => USER },
      createdAt: { type: 'DateTime!', resolve: (v: CommentValue) => v.comment.createdAt },
      updatedAt: { type: 'DateTime!', resolve: (v: CommentValue) => v.comment.updatedAt },
      url: { type: 'URI!', resolve: (v: CommentValue) => webUrl(`${v.repo.fullName}/issues/${v.issue.number}#issuecomment-${v.comment.id}`) },
      issue: { type: 'Issue!', resolve: (v: CommentValue) => issueValue(v.repo, v.issue) },
    },
  },
  {
    kind: 'OBJECT',
    name: 'Label',
    interfaces: ['Node'],
    fields: {
      id: { type: 'ID!', resolve: (v: LabelValue) => nodeId('Label', v.label.id) },
      name: { type: 'String!', resolve: (v: LabelValue) => v.label.name },
      color: { type: 'String!', resolve: (v: LabelValue) => v.label.color },
      description: { type: 'String', resolve: (v: LabelValue) => v.label.description },
      isDefault: { type: 'Boolean!', resolve: () => false },
    },
  },
  { kind: 'OBJECT', name: 'Milestone', fields: { id: { type: 'ID!' }, title: { type: 'String!' }, number: { type: 'Int!' } } },
  { kind: 'OBJECT', name: 'PageInfo', fields: { hasNextPage: { type: 'Boolean!' }, hasPreviousPage: { type: 'Boolean!' }, startCursor: { type: 'String' }, endCursor: { type: 'String' } } },
  { kind: 'OBJECT', name: 'RateLimit', fields: { limit: { type: 'Int!' }, cost: { type: 'Int!' }, remaining: { type: 'Int!' }, used: { type: 'Int!' }, resetAt: { type: 'DateTime!' }, nodeCount: { type: 'Int!' } } },
  {
    kind: 'OBJECT',
    name: 'SearchResultItemConnection',
    fields: {
      issueCount: { type: 'Int!' },
      repositoryCount: { type: 'Int!' },
      userCount: { type: 'Int!' },
      discussionCount: { type: 'Int!' },
      nodes: { type: '[SearchResultItem]' },
      edges: { type: '[SearchResultItemEdge]' },
      pageInfo: { type: 'PageInfo!' },
    },
  },
  { kind: 'OBJECT', name: 'SearchResultItemEdge', fields: { node: { type: 'SearchResultItem' }, cursor: { type: 'String!' } } },
  ...connectionTypes('Issue', 'Issue'),
  ...connectionTypes('PullRequest', 'PullRequest'),
  ...connectionTypes('IssueComment', 'IssueComment'),
  ...connectionTypes('Label', 'Label'),
  ...connectionTypes('User', 'User'),
  ...connectionTypes('Repository', 'Repository'),
  ...connectionTypes('Ref', 'Ref'),
  ...connectionTypes('Milestone', 'Milestone'),
  ...connectionTypes('PullRequestChangedFile', 'PullRequestChangedFile'),
  ...connectionTypes('PullRequestCommit', 'PullRequestCommit'),
];

const ABSTRACT: TypeDef[] = [
  { kind: 'INTERFACE', name: 'Node', fields: { id: { type: 'ID!' } }, possibleTypes: ['Repository', 'Issue', 'PullRequest', 'IssueComment', 'User', 'Label', 'Ref', 'Commit', 'Tag'] },
  { kind: 'INTERFACE', name: 'Actor', fields: { login: { type: 'String!' }, url: { type: 'URI!' }, avatarUrl: { type: 'URI!', args: { size: { type: 'Int' } } } }, possibleTypes: ['User'] },
  {
    kind: 'INTERFACE',
    name: 'RepositoryOwner',
    fields: { id: { type: 'ID!' }, login: { type: 'String!' }, url: { type: 'URI!' }, avatarUrl: { type: 'URI!', args: { size: { type: 'Int' } } } },
    possibleTypes: ['User'],
  },
  { kind: 'INTERFACE', name: 'GitObject', fields: { id: { type: 'ID!' }, oid: { type: 'GitObjectID!' }, abbreviatedOid: { type: 'String!' } }, possibleTypes: ['Commit', 'Tag'] },
  { kind: 'INTERFACE', name: 'Labelable', fields: { labels: { type: 'LabelConnection', args: { ...CONNECTION_ARGS } } }, possibleTypes: ['Issue', 'PullRequest'] },
  { kind: 'INTERFACE', name: 'Comment', fields: { id: { type: 'ID!' }, body: { type: 'String!' }, author: { type: 'Actor' }, createdAt: { type: 'DateTime!' } }, possibleTypes: ['Issue', 'PullRequest', 'IssueComment'] },
  { kind: 'UNION', name: 'SearchResultItem', possibleTypes: ['Issue', 'PullRequest', 'Repository', 'User'] },
  { kind: 'UNION', name: 'IssueOrPullRequest', possibleTypes: ['Issue', 'PullRequest'] },
];

function payload(name: string, fields: Record<string, string>): TypeDef[] {
  const defs: Record<string, FieldDef> = { clientMutationId: { type: 'String' } };
  for (const [field, type] of Object.entries(fields)) defs[field] = { type };
  return [{ kind: 'OBJECT', name, fields: defs }];
}

// ------------------------------------------------------------------ search

function search(query: string, type: string, args: Record<string, any>) {
  const tokens = query.split(/\s+/).filter(Boolean);
  const qualifiers = new Map<string, string[]>();
  const words: string[] = [];
  for (const token of tokens) {
    const colon = token.indexOf(':');
    if (colon > 0) {
      const key = token.slice(0, colon).toLowerCase();
      const list = qualifiers.get(key) ?? [];
      list.push(token.slice(colon + 1).replace(/^"|"$/g, ''));
      qualifiers.set(key, list);
    } else words.push(token.toLowerCase());
  }
  const matchesText = (text: string): boolean => words.every((w) => text.toLowerCase().includes(w));
  let repos = store.list(config.viewer.login);
  const repoFilter = qualifiers.get('repo');
  if (repoFilter !== undefined) repos = repos.filter((r) => repoFilter.some((f) => f.toLowerCase() === r.fullName.toLowerCase()));
  const items: unknown[] = [];
  let issueCount = 0;
  let repositoryCount = 0;
  if (type === 'REPOSITORY') {
    for (const repo of repos) if (matchesText(`${repo.name} ${repo.description ?? ''}`)) items.push(repoValue(repo));
    repositoryCount = items.length;
  } else if (type === 'USER') {
    if (matchesText(config.viewer.login)) items.push(USER);
  } else if (type === 'ISSUE' || type === 'ISSUE_ADVANCED') {
    const is = qualifiers.get('is') ?? [];
    const state = qualifiers.get('state') ?? [];
    const wantPulls = is.includes('pr') || qualifiers.get('type')?.includes('pr');
    const wantIssues = is.includes('issue') || qualifiers.get('type')?.includes('issue');
    const open = is.includes('open') || state.includes('open');
    const closed = is.includes('closed') || state.includes('closed') || is.includes('merged');
    const labels = qualifiers.get('label') ?? [];
    const found: { seq: number; value: unknown }[] = [];
    for (const repo of repos) {
      for (const issue of repo.issues.values()) {
        if (wantPulls && !issue.isPull) continue;
        if (wantIssues && issue.isPull) continue;
        if (open && !closed && issue.state !== 'open') continue;
        if (closed && !open && issue.state !== 'closed') continue;
        if (labels.length > 0 && !labels.every((l) => issue.labels.includes(l.toLowerCase()))) continue;
        if (!matchesText(`${issue.title} ${issue.body ?? ''}`)) continue;
        const pull = issue.isPull ? repo.pulls.get(issue.number) : undefined;
        found.push({ seq: issue.seq, value: pull !== undefined ? pullValue(repo, pull) : issueValue(repo, issue) });
      }
    }
    found.sort((a, b) => b.seq - a.seq);
    for (const f of found) items.push(f.value);
    issueCount = items.length;
  }
  return { ...connection(items, args), issueCount, repositoryCount, userCount: type === 'USER' ? items.length : 0, discussionCount: 0 };
}

export const githubSchema: Schema = buildSchema([...SCALARS, ...ENUMS, ...INPUTS, ...OBJECTS, ...ABSTRACT]);
