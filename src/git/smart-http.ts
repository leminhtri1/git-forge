// Git smart HTTP (gitprotocol-http): info/refs, git-upload-pack (protocol v2, with a v0 fallback) and
// git-receive-pack (v0, report-status, side-band-64k, delete-refs). No git binary: the object graph is
// walked here and packs are written by pack.ts.

import type { IncomingMessage, ServerResponse } from 'node:http';
import { config } from '../config.ts';
import { isAuthorized, nextRequestId, readBody } from '../http.ts';
import { lookupRepo, type Repo } from '../store.ts';
import { isTreeMode, parseCommit, parseTag, parseTree, ZERO_ID, isObjectId } from './objects.ts';
import { readPack, writePack } from './pack.ts';
import { DELIM, FLUSH, decodePackets, lineOf, pkt, sideband, type Packet } from './pktline.ts';

const AGENT = 'agent=git-forge/1.0';
const GIT_PATH = /^\/([^/]+)\/([^/]+?)(?:\.git)?\/(info\/refs|git-upload-pack|git-receive-pack)$/;

type Service = 'git-upload-pack' | 'git-receive-pack';

function sendGit(res: ServerResponse, status: number, contentType: string, body: Buffer): void {
  res.writeHead(status, {
    'content-type': contentType,
    'content-length': body.length,
    'cache-control': 'no-cache, max-age=0, must-revalidate',
    expires: 'Fri, 01 Jan 1980 00:00:00 GMT',
    pragma: 'no-cache',
    'x-github-request-id': nextRequestId(),
  });
  res.end(body);
}

function sendText(res: ServerResponse, status: number, text: string, extra: Record<string, string> = {}): void {
  const body = Buffer.from(`${text}\n`, 'utf8');
  res.writeHead(status, {
    'content-type': 'text/plain; charset=utf-8',
    'content-length': body.length,
    'x-github-request-id': nextRequestId(),
    ...extra,
  });
  res.end(body);
}

function wantsV2(req: IncomingMessage): boolean {
  const header = req.headers['git-protocol'];
  const value = Array.isArray(header) ? header.join(':') : (header ?? '');
  return /(^|:)version=2(:|$)/.test(value);
}

/** Handle a git smart-HTTP request; false when the path is not a git path. */
export async function handleGit(req: IncomingMessage, res: ServerResponse, path: string, query: URLSearchParams): Promise<boolean> {
  const match = GIT_PATH.exec(path);
  if (match === null) return false;
  const owner = decodeURIComponent(match[1]!);
  const name = decodeURIComponent(match[2]!);
  const action = match[3]!;
  const method = req.method ?? 'GET';

  if (!isAuthorized(req)) {
    sendText(res, 401, 'Authentication required', { 'www-authenticate': 'Basic realm="git-forge"' });
    return true;
  }
  const repo = lookupRepo(owner, name);
  if (repo === undefined) {
    sendText(res, 404, 'Repository not found');
    return true;
  }

  if (action === 'info/refs') {
    if (method !== 'GET' && method !== 'HEAD') {
      sendText(res, 405, 'Method Not Allowed');
      return true;
    }
    const service = query.get('service');
    if (service !== 'git-upload-pack' && service !== 'git-receive-pack') {
      sendText(res, 403, 'dumb HTTP is not served; use a smart git client');
      return true;
    }
    const body = service === 'git-upload-pack' && wantsV2(req) ? advertiseV2() : advertiseV0(repo, service);
    sendGit(res, 200, `application/x-${service}-advertisement`, body);
    return true;
  }

  if (method !== 'POST') {
    sendText(res, 405, 'Method Not Allowed');
    return true;
  }
  const service = action as Service;
  const raw = await readBody(req);
  try {
    if (service === 'git-upload-pack') {
      const v2 = wantsV2(req) || raw.subarray(4, 12).toString('ascii') === 'command=';
      const out = v2 ? uploadPackV2(repo, raw) : uploadPackV0(repo, raw);
      sendGit(res, 200, 'application/x-git-upload-pack-result', out);
    } else {
      sendGit(res, 200, 'application/x-git-receive-pack-result', receivePack(repo, raw));
    }
  } catch (err) {
    sendText(res, 400, `bad ${service} request: ${err instanceof Error ? err.message : String(err)}`);
  }
  return true;
}

// ------------------------------------------------------------------ advertisements

/** The v2 capability advertisement never changes: built once. */
const V2_ADVERTISEMENT = Buffer.concat([
  pkt('version 2\n'),
  pkt(`${AGENT}\n`),
  pkt('ls-refs=unborn\n'),
  pkt('fetch=shallow\n'),
  pkt('server-option\n'),
  pkt('object-format=sha1\n'),
  FLUSH,
]);

function advertiseV2(): Buffer {
  return V2_ADVERTISEMENT;
}

/** Refs in advertisement order: sorted by name. */
function sortedRefs(repo: Repo): [string, string][] {
  return [...repo.refs.entries()].sort((a, b) => (a[0] < b[0] ? -1 : a[0] > b[0] ? 1 : 0));
}

function advertiseV0(repo: Repo, service: Service): Buffer {
  const parts: Buffer[] = [pkt(`# service=${service}\n`), FLUSH];
  const caps =
    service === 'git-upload-pack'
      ? ['multi_ack_detailed', 'side-band-64k', 'thin-pack', 'ofs-delta', 'shallow', 'no-progress', 'include-tag', 'allow-tip-sha1-in-want', 'allow-reachable-sha1-in-want', `symref=HEAD:${repo.headRefName}`, 'object-format=sha1', AGENT]
      : ['report-status', 'delete-refs', 'side-band-64k', 'quiet', 'ofs-delta', 'object-format=sha1', AGENT];
  const lines: string[] = [];
  if (service === 'git-upload-pack') {
    const head = repo.refs.get(repo.headRefName);
    if (head !== undefined) lines.push(`${head} HEAD`);
  }
  for (const [name, id] of sortedRefs(repo)) {
    lines.push(`${id} ${name}`);
    if (service === 'git-upload-pack' && name.startsWith('refs/tags/')) {
      const peeled = repo.peel(id);
      if (peeled !== id) lines.push(`${peeled} ${name}^{}`);
    }
  }
  if (lines.length === 0) lines.push(`${ZERO_ID} capabilities^{}`);
  lines.forEach((line, index) => parts.push(pkt(index === 0 ? `${line}\0${caps.join(' ')}\n` : `${line}\n`)));
  parts.push(FLUSH);
  return Buffer.concat(parts);
}

// ------------------------------------------------------------------ upload-pack v2

function splitCommand(packets: Packet[]): { command: string; capabilities: string[]; args: string[] } {
  let command = '';
  const capabilities: string[] = [];
  const args: string[] = [];
  let inArgs = false;
  for (const packet of packets) {
    if (packet.kind === 'delim') {
      inArgs = true;
      continue;
    }
    if (packet.kind === 'flush' || packet.kind === 'end') break;
    const line = lineOf(packet)!;
    if (!inArgs) {
      if (line.startsWith('command=')) command = line.slice(8);
      else capabilities.push(line);
    } else {
      args.push(line);
    }
  }
  return { command, capabilities, args };
}

function uploadPackV2(repo: Repo, raw: Buffer): Buffer {
  const { packets } = decodePackets(raw);
  const { command, args } = splitCommand(packets);
  if (command === 'ls-refs') return lsRefs(repo, args);
  if (command === 'fetch') return fetchV2(repo, args);
  throw new Error(`unknown command ${JSON.stringify(command)}`);
}

const lsRefsCache = new Map<string, Buffer>();

function lsRefs(repo: Repo, args: string[]): Buffer {
  const key = `${repo.id}|${repo.refsVersion}|${repo.defaultBranch}|${args.join('\n')}`;
  const cached = lsRefsCache.get(key);
  if (cached !== undefined) return cached;
  const body = buildLsRefs(repo, args);
  if (lsRefsCache.size > 4096) lsRefsCache.clear();
  lsRefsCache.set(key, body);
  return body;
}

function buildLsRefs(repo: Repo, args: string[]): Buffer {
  const peel = args.includes('peel');
  const symrefs = args.includes('symrefs');
  const unborn = args.includes('unborn');
  const prefixes = args.filter((a) => a.startsWith('ref-prefix ')).map((a) => a.slice(11));
  const wanted = (name: string): boolean => prefixes.length === 0 || prefixes.some((p) => name.startsWith(p));
  const out: Buffer[] = [];
  if (wanted('HEAD')) {
    const head = repo.refs.get(repo.headRefName);
    const target = symrefs ? ` symref-target:${repo.headRefName}` : '';
    if (head !== undefined) out.push(pkt(`${head} HEAD${target}\n`));
    else if (unborn) out.push(pkt(`unborn HEAD${target}\n`));
  }
  for (const [name, id] of sortedRefs(repo)) {
    if (!wanted(name)) continue;
    let line = `${id} ${name}`;
    if (peel) {
      const peeled = repo.peel(id);
      if (peeled !== id) line += ` peeled:${peeled}`;
    }
    out.push(pkt(`${line}\n`));
  }
  out.push(FLUSH);
  return Buffer.concat(out);
}

interface FetchArgs {
  wants: string[];
  haves: string[];
  shallow: string[];
  depth: number | null;
  done: boolean;
  includeTag: boolean;
  sideband: boolean;
}

function parseFetchArgs(args: string[]): FetchArgs {
  const parsed: FetchArgs = { wants: [], haves: [], shallow: [], depth: null, done: false, includeTag: false, sideband: true };
  for (const arg of args) {
    if (arg.startsWith('want ')) parsed.wants.push(arg.slice(5, 45));
    else if (arg.startsWith('have ')) parsed.haves.push(arg.slice(5, 45));
    else if (arg.startsWith('shallow ')) parsed.shallow.push(arg.slice(8, 48));
    else if (arg.startsWith('deepen ')) parsed.depth = Math.max(1, Number.parseInt(arg.slice(7), 10) || 1);
    else if (arg === 'done') parsed.done = true;
    else if (arg === 'include-tag') parsed.includeTag = true;
    else if (arg.startsWith('want-ref ')) {
      // resolved by the caller through the repo's refs
      parsed.wants.push(`ref:${arg.slice(9)}`);
    }
  }
  return parsed;
}

interface PackPlan {
  ids: string[];
  shallow: string[];
  unshallow: string[];
  common: string[];
}

/**
 * Decide which objects a fetch sends. `haves` (and everything they reach) are on the client, except
 * that history behind the client's shallow commits is not. With a depth, the commits within `depth`
 * generations of the wants are sent and the generation at the edge becomes the new shallow boundary.
 */
function planFetch(repo: Repo, request: FetchArgs): PackPlan {
  const clientShallow = new Set(request.shallow.filter((id) => repo.objects.has(id)));
  const common = request.haves.filter((id) => repo.objects.has(id));

  // What the client already holds: have-commits (not walking past its shallow boundary) and their trees.
  const haveCommits = new Set<string>();
  const haveObjects = new Set<string>();
  const stack: string[] = [];
  for (const have of common) {
    const peeled = repo.peel(have);
    if (peeled !== have) haveObjects.add(have);
    stack.push(peeled);
  }
  while (stack.length > 0) {
    const id = stack.pop()!;
    if (haveCommits.has(id)) continue;
    const commit = repo.commit(id);
    if (commit === undefined) {
      haveObjects.add(id);
      continue;
    }
    haveCommits.add(id);
    markTree(repo, commit.tree, haveObjects);
    if (!clientShallow.has(id)) for (const parent of commit.parents) stack.push(parent);
  }

  // The wants: tag objects ride along and are peeled to the commits whose history is sent.
  const send: string[] = [];
  const sent = new Set<string>();
  const add = (id: string): void => {
    if (!sent.has(id) && !haveObjects.has(id) && !haveCommits.has(id)) {
      sent.add(id);
      send.push(id);
    }
  };
  const wantCommits: string[] = [];
  for (const want of request.wants) {
    let id = want;
    while (true) {
      const object = repo.get(id);
      if (object === undefined || object.type !== 'tag') break;
      add(id);
      id = parseTag(object.data).object;
    }
    const object = repo.get(id);
    if (object === undefined) continue;
    if (object.type === 'commit') wantCommits.push(id);
    else if (object.type === 'tree') addTree(repo, id, add, haveObjects);
    else add(id);
  }

  const shallow: string[] = [];
  const unshallow: string[] = [];
  const commits: string[] = [];
  if (request.depth !== null) {
    const depthOf = new Map<string, number>();
    let frontier = wantCommits.filter((id, i, all) => all.indexOf(id) === i);
    for (const id of frontier) depthOf.set(id, 1);
    for (let level = 1; frontier.length > 0 && level <= request.depth; level++) {
      const next: string[] = [];
      for (const id of frontier) {
        const commit = repo.commit(id);
        if (commit === undefined) continue;
        commits.push(id);
        if (level === request.depth) {
          if (commit.parents.length > 0 && !clientShallow.has(id)) shallow.push(id);
          continue;
        }
        if (clientShallow.has(id) && commit.parents.length > 0) unshallow.push(id);
        for (const parent of commit.parents) {
          if (!depthOf.has(parent)) {
            depthOf.set(parent, level + 1);
            next.push(parent);
          }
        }
      }
      frontier = next;
    }
    // A client-shallow commit that stays at the boundary is still shallow; one now deeper is unshallowed.
  } else {
    const seen = new Set<string>();
    const walk = [...wantCommits];
    while (walk.length > 0) {
      const id = walk.pop()!;
      if (seen.has(id) || haveCommits.has(id)) continue;
      seen.add(id);
      const commit = repo.commit(id);
      if (commit === undefined) continue;
      commits.push(id);
      for (const parent of commit.parents) walk.push(parent);
    }
  }

  for (const id of commits) {
    if (haveCommits.has(id) && !unshallow.includes(id)) continue;
    const commit = repo.commit(id)!;
    if (!haveCommits.has(id)) add(id);
    addTree(repo, commit.tree, add, haveObjects);
  }

  if (request.includeTag) {
    for (const [name, id] of repo.refs) {
      if (!name.startsWith('refs/tags/') || sent.has(id)) continue;
      if (sent.has(repo.peel(id))) {
        const object = repo.get(id);
        if (object?.type === 'tag') add(id);
      }
    }
  }
  return { ids: send, shallow, unshallow, common };
}

function markTree(repo: Repo, treeId: string, into: Set<string>): void {
  if (into.has(treeId)) return;
  into.add(treeId);
  const object = repo.get(treeId);
  if (object === undefined || object.type !== 'tree') return;
  for (const entry of parseTree(object.data)) {
    if (entry.mode === '160000') continue; // submodule commit: not ours
    if (isTreeMode(entry.mode)) markTree(repo, entry.id, into);
    else into.add(entry.id);
  }
}

function addTree(repo: Repo, treeId: string, add: (id: string) => void, have: Set<string>): void {
  const visit = (id: string): void => {
    if (have.has(id)) return;
    const object = repo.get(id);
    if (object === undefined) return;
    add(id);
    if (object.type !== 'tree') return;
    for (const entry of parseTree(object.data)) {
      if (entry.mode === '160000') continue;
      if (isTreeMode(entry.mode)) visit(entry.id);
      else if (!have.has(entry.id) && repo.objects.has(entry.id)) add(entry.id);
    }
  };
  visit(treeId);
}

const fetchCache = new Map<string, Buffer>();
let fetchCacheBytes = 0;
const FETCH_CACHE_LIMIT = 128 * 1024 * 1024;

function resolveWants(repo: Repo, wants: string[]): string[] {
  return wants.map((want) => (want.startsWith('ref:') ? (repo.refs.get(want.slice(4)) ?? '') : want)).filter(isObjectId);
}

function fetchV2(repo: Repo, args: string[]): Buffer {
  const request = parseFetchArgs(args);
  request.wants = resolveWants(repo, request.wants);
  const key = [
    'v2',
    repo.id,
    repo.refsVersion,
    repo.objects.size,
    request.done ? 1 : 0,
    request.includeTag ? 1 : 0,
    request.depth ?? '-',
    [...request.wants].sort().join(','),
    [...request.haves].sort().join(','),
    [...request.shallow].sort().join(','),
  ].join('|');
  const cached = fetchCache.get(key);
  if (cached !== undefined) return cached;

  for (const want of request.wants) {
    if (!repo.objects.has(want)) throw new Error(`not our ref ${want}`);
  }
  const plan = planFetch(repo, request);
  const out: Buffer[] = [];
  if (!request.done) {
    out.push(pkt('acknowledgments\n'));
    if (plan.common.length === 0) out.push(pkt('NAK\n'));
    for (const id of plan.common) out.push(pkt(`ACK ${id}\n`));
    out.push(pkt('ready\n'), DELIM);
  }
  if (plan.shallow.length > 0 || plan.unshallow.length > 0) {
    out.push(pkt('shallow-info\n'));
    for (const id of plan.shallow) out.push(pkt(`shallow ${id}\n`));
    for (const id of plan.unshallow) out.push(pkt(`unshallow ${id}\n`));
    out.push(DELIM);
  }
  out.push(pkt('packfile\n'));
  sideband(1, writePack(plan.ids, (id) => repo.get(id)), out);
  out.push(FLUSH);
  const body = Buffer.concat(out);
  if (fetchCacheBytes + body.length > FETCH_CACHE_LIMIT) {
    fetchCache.clear();
    fetchCacheBytes = 0;
  }
  fetchCache.set(key, body);
  fetchCacheBytes += body.length;
  return body;
}

// ------------------------------------------------------------------ upload-pack v0

function uploadPackV0(repo: Repo, raw: Buffer): Buffer {
  const { packets } = decodePackets(raw);
  const request: FetchArgs = { wants: [], haves: [], shallow: [], depth: null, done: false, includeTag: false, sideband: false };
  for (const packet of packets) {
    const line = lineOf(packet);
    if (line === null) continue;
    if (line.startsWith('want ')) {
      request.wants.push(line.slice(5, 45));
      const caps = line.slice(46).split(' ');
      if (caps.includes('side-band-64k') || caps.includes('side-band')) request.sideband = true;
      if (caps.includes('include-tag')) request.includeTag = true;
    } else if (line.startsWith('have ')) request.haves.push(line.slice(5, 45));
    else if (line.startsWith('shallow ')) request.shallow.push(line.slice(8, 48));
    else if (line.startsWith('deepen ')) request.depth = Math.max(1, Number.parseInt(line.slice(7), 10) || 1);
    else if (line === 'done') request.done = true;
  }
  if (request.wants.length === 0) return Buffer.alloc(0);
  for (const want of request.wants) {
    if (!repo.objects.has(want)) throw new Error(`not our ref ${want}`);
  }
  const plan = planFetch(repo, request);
  const out: Buffer[] = [];
  if (request.depth !== null || request.shallow.length > 0) {
    for (const id of plan.shallow) out.push(pkt(`shallow ${id}\n`));
    for (const id of plan.unshallow) out.push(pkt(`unshallow ${id}\n`));
    out.push(FLUSH);
  }
  if (!request.done && request.haves.length > 0 && plan.common.length > 0) {
    for (const id of plan.common) out.push(pkt(`ACK ${id} common\n`));
    out.push(pkt(`ACK ${plan.common[plan.common.length - 1]} ready\n`));
  }
  out.push(plan.common.length > 0 ? pkt(`ACK ${plan.common[plan.common.length - 1]}\n`) : pkt('NAK\n'));
  const pack = writePack(plan.ids, (id) => repo.get(id));
  if (request.sideband) {
    sideband(1, pack, out);
    out.push(FLUSH);
  } else {
    out.push(pack);
  }
  return Buffer.concat(out);
}

// ------------------------------------------------------------------ receive-pack

interface RefCommand {
  oldId: string;
  newId: string;
  ref: string;
}

function receivePack(repo: Repo, raw: Buffer): Buffer {
  const { packets, end } = decodePackets(raw, true);
  const commands: RefCommand[] = [];
  let caps: string[] = [];
  for (const packet of packets) {
    if (packet.kind !== 'data') continue;
    let line = packet.data.toString('utf8');
    if (line.endsWith('\n')) line = line.slice(0, -1);
    const nul = line.indexOf('\0');
    if (nul >= 0) {
      caps = line.slice(nul + 1).split(' ').filter(Boolean);
      line = line.slice(0, nul);
    }
    if (line.startsWith('shallow ')) continue;
    const [oldId, newId, ref] = line.split(' ');
    if (!isObjectId(oldId) || !isObjectId(newId) || ref === undefined || ref === '') throw new Error(`bad command ${JSON.stringify(line)}`);
    commands.push({ oldId, newId, ref });
  }
  const useSideband = caps.includes('side-band-64k') || caps.includes('side-band');
  const reportStatus = caps.includes('report-status') || caps.includes('report-status-v2');

  let unpackError: string | null = null;
  const packData = raw.subarray(end);
  if (packData.length > 0) {
    try {
      const objects = readPack(packData, (id) => repo.get(id));
      for (const object of objects) {
        if (!repo.objects.has(object.id)) repo.objects.set(object.id, { type: object.type, data: object.data });
      }
    } catch (err) {
      unpackError = err instanceof Error ? err.message : String(err);
    }
  }

  const hadBranches = !repo.isEmpty();
  const results: string[] = [];
  for (const command of commands) {
    if (unpackError !== null) {
      results.push(`ng ${command.ref} unpacker error`);
      continue;
    }
    const reason = applyCommand(repo, command);
    results.push(reason === null ? `ok ${command.ref}` : `ng ${command.ref} ${reason}`);
  }
  // A first push that does not create the default branch makes its first branch the default (as GitHub does).
  if (!hadBranches && !repo.refs.has(repo.headRefName)) {
    const firstBranch = commands.find((c) => c.ref.startsWith('refs/heads/') && repo.refs.has(c.ref));
    if (firstBranch !== undefined) repo.defaultBranch = firstBranch.ref.slice('refs/heads/'.length);
  }

  if (!reportStatus) return Buffer.alloc(0);
  const report = Buffer.concat([
    pkt(unpackError === null ? 'unpack ok\n' : `unpack ${unpackError.replace(/\n/g, ' ')}\n`),
    ...results.map((line) => pkt(`${line}\n`)),
    FLUSH,
  ]);
  if (!useSideband) return report;
  const out: Buffer[] = [];
  sideband(1, report, out);
  out.push(FLUSH);
  return Buffer.concat(out);
}

/** Apply one ref update; null on success, else the `ng` reason. */
function applyCommand(repo: Repo, command: RefCommand): string | null {
  const { oldId, newId, ref } = command;
  if (!ref.startsWith('refs/') || ref.includes('..') || /[\s~^:?*[\\]/.test(ref) || ref.endsWith('/') || ref.endsWith('.lock')) {
    return 'funny refname';
  }
  const current = repo.refs.get(ref) ?? ZERO_ID;
  if (current !== oldId) return current === ZERO_ID ? 'stale info' : oldId === ZERO_ID ? 'already exists' : 'stale info';
  if (newId === ZERO_ID) {
    if (current === ZERO_ID) return 'no such ref';
    if (ref === repo.headRefName && [...repo.refs.keys()].some((name) => name.startsWith('refs/heads/') && name !== ref)) {
      return 'refusing to delete the current branch';
    }
    repo.setRef(ref, null);
    return null;
  }
  const object = repo.get(newId);
  if (object === undefined) return 'missing necessary objects';
  if (ref.startsWith('refs/heads/') && object.type !== 'commit') return 'not a commit';
  if (current !== ZERO_ID) {
    const oldCommit = repo.peel(current);
    const newCommit = repo.peel(newId);
    if (repo.get(oldCommit)?.type !== 'commit' || repo.get(newCommit)?.type !== 'commit' || !repo.isAncestor(oldCommit, newCommit)) {
      return 'non-fast-forward';
    }
  } else if (!hasClosure(repo, newId)) {
    return 'missing necessary objects';
  }
  repo.setRef(ref, newId);
  return null;
}

/** Whether a new ref's tip has its commit, tree and parent links present (a cheap connectivity check). */
function hasClosure(repo: Repo, id: string): boolean {
  const peeled = repo.peel(id);
  const object = repo.get(peeled);
  if (object === undefined) return false;
  if (object.type !== 'commit') return true;
  const commit = parseCommit(object.data);
  return repo.objects.has(commit.tree) && commit.parents.every((p) => repo.objects.has(p));
}

void config;
