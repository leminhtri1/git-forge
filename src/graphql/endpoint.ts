// POST /api/graphql: GitHub's GraphQL v4 endpoint. An unauthenticated request is 401; a body that is a
// JSON array is a batch and answers an array of results in order.

import type { IncomingMessage, ServerResponse } from 'node:http';
import { HttpError, isAuthorized, readBody, sendError, sendJson } from '../http.ts';
import { executeBody } from './execute.ts';
import { githubSchema } from './schema.ts';

export async function handleGraphql(req: IncomingMessage, res: ServerResponse): Promise<void> {
  if (!isAuthorized(req)) {
    req.resume();
    sendError(res, 401, 'This endpoint requires you to be authenticated.');
    return;
  }
  if (req.method !== 'POST') {
    req.resume();
    sendError(res, 404, 'Not Found');
    return;
  }
  const raw = await readBody(req, 16 * 1024 * 1024);
  let body: unknown;
  try {
    body = JSON.parse(raw.toString('utf8'));
  } catch {
    throw new HttpError(400, 'Problems parsing JSON');
  }
  sendJson(res, 200, executeBody(githubSchema, body));
}
