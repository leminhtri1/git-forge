// The forge's HTTP server: src/server.ts (HTTP/1.1 on node:net), routed by path prefix to the surfaces.
//
//   GET  /healthz                      boot probe, no auth
//   /api/v3/...                        REST v3            (src/rest/)
//   POST /api/graphql                  GraphQL v4         (src/graphql/)
//   POST /mcp                          Model Context Protocol, streamable HTTP (src/mcp/)
//   /<owner>/<repo>.git/...            git smart HTTP     (src/git/)
//   anything else                      404 {"message":"Not Found"}
//
// Every request that is not a GET or HEAD may change the store, so it bumps the mutation version the
// response caches compare against (before and after it runs).

import { handleGit } from './src/git/smart-http.ts';
import { handleGraphql } from './src/graphql/endpoint.ts';
import { bumpMutationVersion, HttpError, sendError, sendJson, type Req, type Res } from './src/http.ts';
import { handleMcp } from './src/mcp/endpoint.ts';
import { fastIssueGet, handleRest } from './src/rest/router.ts';
import { createHttpServer } from './src/server.ts';

export interface RunningForge {
  readonly port: number;
  close(): Promise<void>;
}

function pathOf(url: string): string {
  const q = url.indexOf('?');
  return q < 0 ? url : url.slice(0, q);
}

async function route(req: Req, res: Res, path: string): Promise<void> {
  const url = req.url;
  if (path === '/healthz') {
    sendJson(res, 200, { ok: true });
    return;
  }
  const query = new URLSearchParams(url.length > path.length ? url.slice(path.length + 1) : '');
  if (path === '/api/v3' || path.startsWith('/api/v3/')) {
    await handleRest(req, res, path, query);
    return;
  }
  if (path === '/mcp' || path === '/mcp/') {
    await handleMcp(req, res);
    return;
  }
  if (path === '/api/graphql' || path === '/graphql') {
    await handleGraphql(req, res);
    return;
  }
  if (path.endsWith('/info/refs') || path.endsWith('/git-upload-pack') || path.endsWith('/git-receive-pack')) {
    if (await handleGit(req, res, path, query)) return;
  }
  sendError(res, 404, 'Not Found');
}

function fail(req: Req, res: Res, err: unknown): void {
  if (res.headersSent) {
    res.destroy();
    return;
  }
  if (err instanceof HttpError) sendError(res, err.status, err.message, err.extra);
  else {
    process.stderr.write(`[forge] ${req.method} ${req.url}: ${err instanceof Error ? err.stack : String(err)}\n`);
    sendError(res, 500, 'Internal Server Error');
  }
}

function handle(req: Req, res: Res): void {
  const path = pathOf(req.url);
  const reads = req.method === 'GET' || req.method === 'HEAD';
  // The read workload's request: answered from the serialised-response cache when it is current.
  if (reads && fastIssueGet(req, res, path)) return;
  if (!reads) bumpMutationVersion();
  route(req, res, path).then(
    () => {
      if (!reads) bumpMutationVersion();
    },
    (err: unknown) => {
      if (!reads) bumpMutationVersion();
      fail(req, res, err);
    },
  );
}

export async function startForge(port = 0): Promise<RunningForge> {
  const http = createHttpServer(handle);
  const bound = await http.listen(port);
  return { port: bound, close: () => http.close() };
}
