// GitHub REST v3 object shapes for the forge's entities. `api` is the absolute API root
// (http://host/api/v3) and `web` the absolute web root (http://host), both from the request's Host.

import { config } from '../config.ts';
import { parseCommit, parseSignature, signatureDate } from '../git/objects.ts';
import { nodeId } from '../ids.ts';
import { pullBaseSha, pullCommits, pullFiles, pullHeadSha, pullMergeable } from '../ops.ts';
import type { Comment, Issue, Label, Pull, Repo } from '../store.ts';

export interface Urls {
  readonly api: string;
  readonly web: string;
}

const userViews = new Map<string, ReturnType<typeof buildUserView>>();

/** The viewer as a REST user object (memoised per origin: it is embedded in nearly every reply). */
export function userView(urls: Urls) {
  let view = userViews.get(urls.api);
  if (view === undefined) {
    view = buildUserView(urls);
    if (userViews.size > 64) userViews.clear();
    userViews.set(urls.api, view);
  }
  return view;
}

function buildUserView(urls: Urls) {
  const login = config.viewer.login;
  return {
    login,
    id: config.viewer.id,
    node_id: nodeId('User', config.viewer.id),
    avatar_url: `${urls.web}/avatars/${login}`,
    gravatar_id: '',
    url: `${urls.api}/users/${login}`,
    html_url: `${urls.web}/${login}`,
    repos_url: `${urls.api}/users/${login}/repos`,
    type: 'User',
    site_admin: false,
  };
}

export function fullUserView(urls: Urls) {
  return {
    ...userView(urls),
    name: config.viewer.name,
    email: config.viewer.email,
    company: null,
    blog: '',
    location: null,
    bio: null,
    public_repos: 0,
    followers: 0,
    following: 0,
    created_at: '2025-01-01T00:00:00Z',
    updated_at: '2025-01-01T00:00:00Z',
  };
}

export function repoView(repo: Repo, urls: Urls) {
  const api = `${urls.api}/repos/${repo.fullName}`;
  let openIssues = 0;
  for (const issue of repo.issues.values()) if (issue.state === 'open') openIssues++;
  return {
    id: repo.id,
    node_id: nodeId('Repository', repo.id),
    name: repo.name,
    full_name: repo.fullName,
    private: repo.isPrivate,
    owner: userView(urls),
    html_url: `${urls.web}/${repo.fullName}`,
    description: repo.description,
    fork: false,
    url: api,
    issues_url: `${api}/issues{/number}`,
    pulls_url: `${api}/pulls{/number}`,
    contents_url: `${api}/contents/{+path}`,
    branches_url: `${api}/branches{/branch}`,
    commits_url: `${api}/commits{/sha}`,
    labels_url: `${api}/labels{/name}`,
    git_url: `${urls.web}/${repo.fullName}.git`,
    clone_url: `${urls.web}/${repo.fullName}.git`,
    ssh_url: `git@forge.local:${repo.fullName}.git`,
    homepage: repo.homepage,
    size: 0,
    stargazers_count: 0,
    watchers_count: 0,
    watchers: 0,
    language: null,
    has_issues: true,
    has_projects: true,
    has_wiki: false,
    has_pages: false,
    has_downloads: true,
    has_discussions: false,
    archived: false,
    disabled: false,
    visibility: repo.isPrivate ? 'private' : 'public',
    forks_count: 0,
    forks: 0,
    open_issues_count: openIssues,
    open_issues: openIssues,
    topics: [],
    is_template: false,
    default_branch: repo.defaultBranch,
    created_at: repo.createdAt,
    updated_at: repo.updatedAt,
    pushed_at: repo.pushedAt,
    permissions: { admin: true, maintain: true, push: true, triage: true, pull: true },
    allow_squash_merge: true,
    allow_merge_commit: true,
    allow_rebase_merge: true,
    delete_branch_on_merge: false,
  };
}

export function labelView(repo: Repo, label: Label, urls: Urls) {
  return {
    id: label.id,
    node_id: nodeId('Label', label.id),
    url: `${urls.api}/repos/${repo.fullName}/labels/${encodeURIComponent(label.name)}`,
    name: label.name,
    color: label.color,
    default: false,
    description: label.description,
  };
}

export function issueView(repo: Repo, issue: Issue, urls: Urls) {
  const api = `${urls.api}/repos/${repo.fullName}`;
  const labels: unknown[] = [];
  for (const key of issue.labels) {
    const label = repo.labels.get(key);
    if (label !== undefined) labels.push(labelView(repo, label, urls));
  }
  const view: Record<string, unknown> = {
    id: issue.id,
    node_id: issue.isPull ? nodeId('PullRequest', repo.pulls.get(issue.number)?.id ?? issue.id) : nodeId('Issue', issue.id),
    url: `${api}/issues/${issue.number}`,
    repository_url: api,
    labels_url: `${api}/issues/${issue.number}/labels{/name}`,
    comments_url: `${api}/issues/${issue.number}/comments`,
    html_url: `${urls.web}/${repo.fullName}/${issue.isPull ? 'pull' : 'issues'}/${issue.number}`,
    number: issue.number,
    state: issue.state,
    state_reason: issue.stateReason,
    title: issue.title,
    body: issue.body,
    user: userView(urls),
    labels,
    assignee: null,
    assignees: [],
    milestone: null,
    locked: false,
    active_lock_reason: null,
    comments: issue.comments.length,
    created_at: issue.createdAt,
    updated_at: issue.updatedAt,
    closed_at: issue.closedAt,
    closed_by: issue.state === 'closed' ? userView(urls) : null,
    author_association: 'OWNER',
    reactions: { url: `${api}/issues/${issue.number}/reactions`, total_count: 0, '+1': 0, '-1': 0, laugh: 0, hooray: 0, confused: 0, heart: 0, rocket: 0, eyes: 0 },
  };
  if (issue.isPull) {
    const pull = repo.pulls.get(issue.number);
    view.pull_request = {
      url: `${api}/pulls/${issue.number}`,
      html_url: `${urls.web}/${repo.fullName}/pull/${issue.number}`,
      diff_url: `${urls.web}/${repo.fullName}/pull/${issue.number}.diff`,
      patch_url: `${urls.web}/${repo.fullName}/pull/${issue.number}.patch`,
      merged_at: pull?.mergedAt ?? null,
    };
  }
  return view;
}

export function commentView(repo: Repo, issue: Issue, comment: Comment, urls: Urls) {
  const api = `${urls.api}/repos/${repo.fullName}`;
  return {
    id: comment.id,
    node_id: nodeId('IssueComment', comment.id),
    url: `${api}/issues/comments/${comment.id}`,
    html_url: `${urls.web}/${repo.fullName}/issues/${issue.number}#issuecomment-${comment.id}`,
    issue_url: `${api}/issues/${issue.number}`,
    body: comment.body,
    user: userView(urls),
    created_at: comment.createdAt,
    updated_at: comment.updatedAt,
    author_association: 'OWNER',
  };
}

function branchSide(repo: Repo, ref: string, sha: string, urls: Urls) {
  return { label: `${repo.owner}:${ref}`, ref, sha, user: userView(urls), repo: repoView(repo, urls) };
}

export function pullView(repo: Repo, pull: Pull, urls: Urls, detailed = true) {
  const api = `${urls.api}/repos/${repo.fullName}`;
  const issue = pull.issue;
  const labels: unknown[] = [];
  for (const key of issue.labels) {
    const label = repo.labels.get(key);
    if (label !== undefined) labels.push(labelView(repo, label, urls));
  }
  const view: Record<string, unknown> = {
    id: pull.id,
    node_id: nodeId('PullRequest', pull.id),
    url: `${api}/pulls/${pull.number}`,
    html_url: `${urls.web}/${repo.fullName}/pull/${pull.number}`,
    diff_url: `${urls.web}/${repo.fullName}/pull/${pull.number}.diff`,
    patch_url: `${urls.web}/${repo.fullName}/pull/${pull.number}.patch`,
    issue_url: `${api}/issues/${pull.number}`,
    commits_url: `${api}/pulls/${pull.number}/commits`,
    comments_url: `${api}/issues/${pull.number}/comments`,
    number: pull.number,
    state: issue.state,
    locked: false,
    title: issue.title,
    body: issue.body,
    user: userView(urls),
    labels,
    milestone: null,
    assignee: null,
    assignees: [],
    requested_reviewers: [],
    created_at: issue.createdAt,
    updated_at: issue.updatedAt,
    closed_at: issue.closedAt,
    merged_at: pull.mergedAt,
    merge_commit_sha: pull.mergeCommitSha,
    draft: pull.draft,
    head: branchSide(repo, pull.headRef, pullHeadSha(repo, pull), urls),
    base: branchSide(repo, pull.baseRef, pullBaseSha(repo, pull), urls),
    author_association: 'OWNER',
    auto_merge: null,
    active_lock_reason: null,
  };
  if (detailed) {
    const files = pullFiles(repo, pull);
    const mergeable = pullMergeable(repo, pull);
    view.merged = pull.merged;
    view.mergeable = mergeable;
    view.rebaseable = mergeable;
    view.mergeable_state = pull.merged ? 'unknown' : mergeable === true ? 'clean' : mergeable === false ? 'dirty' : 'unknown';
    view.merged_by = pull.merged ? userView(urls) : null;
    view.comments = issue.comments.length;
    view.review_comments = 0;
    view.maintainer_can_modify = false;
    view.commits = pullCommits(repo, pull).length;
    view.additions = files.reduce((n, f) => n + f.additions, 0);
    view.deletions = files.reduce((n, f) => n + f.deletions, 0);
    view.changed_files = files.length;
  } else {
    view.merged = pull.merged;
  }
  return view;
}

function stripOneNewline(message: string): string {
  return message.endsWith('\n') ? message.slice(0, -1) : message;
}

export function commitView(repo: Repo, sha: string, urls: Urls) {
  const object = repo.get(sha)!;
  const commit = parseCommit(object.data);
  const api = `${urls.api}/repos/${repo.fullName}`;
  const author = parseSignature(commit.author);
  const committer = parseSignature(commit.committer);
  const mine = (email: string) => (email === config.viewer.email ? userView(urls) : null);
  return {
    sha,
    node_id: nodeId('Commit', sha),
    url: `${api}/commits/${sha}`,
    html_url: `${urls.web}/${repo.fullName}/commit/${sha}`,
    comments_url: `${api}/commits/${sha}/comments`,
    commit: {
      url: `${api}/git/commits/${sha}`,
      author: { name: author.name, email: author.email, date: signatureDate(commit.author) },
      committer: { name: committer.name, email: committer.email, date: signatureDate(commit.committer) },
      message: stripOneNewline(commit.message),
      tree: { sha: commit.tree, url: `${api}/git/trees/${commit.tree}` },
      comment_count: 0,
      verification: { verified: false, reason: 'unsigned', signature: null, payload: null },
    },
    author: mine(author.email),
    committer: mine(committer.email),
    parents: commit.parents.map((p) => ({ sha: p, url: `${api}/commits/${p}`, html_url: `${urls.web}/${repo.fullName}/commit/${p}` })),
  };
}

/** The git-data shape of a commit (PUT contents' `commit`, GET git/commits/{sha}). */
export function gitCommitView(repo: Repo, sha: string, urls: Urls) {
  const commit = parseCommit(repo.get(sha)!.data);
  const api = `${urls.api}/repos/${repo.fullName}`;
  const author = parseSignature(commit.author);
  const committer = parseSignature(commit.committer);
  return {
    sha,
    node_id: nodeId('Commit', sha),
    url: `${api}/git/commits/${sha}`,
    html_url: `${urls.web}/${repo.fullName}/commit/${sha}`,
    author: { name: author.name, email: author.email, date: signatureDate(commit.author) },
    committer: { name: committer.name, email: committer.email, date: signatureDate(commit.committer) },
    message: stripOneNewline(commit.message),
    tree: { sha: commit.tree, url: `${api}/git/trees/${commit.tree}` },
    parents: commit.parents.map((p) => ({ sha: p, url: `${api}/git/commits/${p}`, html_url: `${urls.web}/${repo.fullName}/commit/${p}` })),
    verification: { verified: false, reason: 'unsigned', signature: null, payload: null },
  };
}

export function contentEntryView(repo: Repo, path: string, mode: string, sha: string, size: number, ref: string, urls: Urls) {
  const api = `${urls.api}/repos/${repo.fullName}`;
  const type = mode === '40000' || mode === '040000' ? 'dir' : mode === '120000' ? 'symlink' : mode === '160000' ? 'submodule' : 'file';
  const name = path.slice(path.lastIndexOf('/') + 1);
  const self = `${api}/contents/${path}?ref=${encodeURIComponent(ref)}`;
  const git = `${api}/git/${type === 'dir' ? 'trees' : 'blobs'}/${sha}`;
  const html = `${urls.web}/${repo.fullName}/${type === 'dir' ? 'tree' : 'blob'}/${ref}/${path}`;
  return {
    type,
    size,
    name,
    path,
    sha,
    url: self,
    git_url: git,
    html_url: html,
    download_url: type === 'file' ? `${urls.web}/${repo.fullName}/raw/${ref}/${path}` : null,
    _links: { self, git, html },
  };
}

export function refView(repo: Repo, ref: string, sha: string, urls: Urls) {
  const type = repo.get(sha)?.type ?? 'commit';
  return {
    ref,
    node_id: nodeId('Ref', `${repo.id}:${ref}`),
    url: `${urls.api}/repos/${repo.fullName}/git/${ref}`,
    object: { sha, type, url: `${urls.api}/repos/${repo.fullName}/git/${type}s/${sha}` },
  };
}

export function branchView(repo: Repo, name: string, sha: string, urls: Urls, detailed: boolean) {
  const base = {
    name,
    commit: detailed ? commitView(repo, sha, urls) : { sha, url: `${urls.api}/repos/${repo.fullName}/commits/${sha}` },
    protected: false,
  };
  if (!detailed) return base;
  return {
    ...base,
    _links: { self: `${urls.api}/repos/${repo.fullName}/branches/${name}`, html: `${urls.web}/${repo.fullName}/tree/${name}` },
    protection: { enabled: false, required_status_checks: { enforcement_level: 'off', contexts: [], checks: [] } },
    protection_url: `${urls.api}/repos/${repo.fullName}/branches/${name}/protection`,
  };
}
