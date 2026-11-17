// HTTP plumbing shared by every surface: body reading, JSON replies, request ids and the bearer check.

import { gunzipSync, inflateSync } from 'node:zlib';
import { config } from './config.ts';

/** A request as src/server.ts hands it over: head parsed, body already read whole. */
export interface Req {
  readonly method: string;
  readonly url: string;
  /** Lowercase names; repeated headers joined with ", ". */
  readonly headers: Readonly<Record<string, string | undefined>>;
  readonly body: Buffer;
  /** Kept for handlers written against node:http; the body is already consumed. */
  resume(): void;
}

/** A response: the head is buffered by writeHead, and end() writes head and body in one write. */
export interface Res {
  readonly headersSent: boolean;
  writeHead(status: number, headers?: Record<string, string | number>): void;
  end(body?: Buffer | string): void;
  destroy(): void;
}

type IncomingMessage = Req;
type ServerResponse = Res;

/** A request mutated the store (any method but GET/HEAD): response caches compare against this. */
export let mutationVersion = 0;
export function bumpMutationVersion(): void {
  mutationVersion += 1;
}

export const JSON_TYPE = 'application/json; charset=utf-8';

let requestCounter = 0;

/** A fresh, non-empty request id (hex counter, GitHub-like shape). */
export function nextRequestId(): string {
  requestCounter += 1;
  return `F0RG:${requestCounter.toString(16).toUpperCase().padStart(8, '0')}`;
}

/** Thrown by handlers to answer with a JSON error body. */
export class HttpError extends Error {
  readonly status: number;
  readonly extra: Record<string, unknown> | undefined;
  constructor(status: number, message: string, extra?: Record<string, unknown>) {
    super(message);
    this.status = status;
    this.extra = extra;
  }
}

export function sendJson(res: ServerResponse, status: number, body: unknown, headers: Record<string, string | number> = {}): void {
  const text = body === undefined ? '' : JSON.stringify(body);
  res.writeHead(status, {
    'content-type': JSON_TYPE,
    'content-length': Buffer.byteLength(text),
    'x-github-request-id': nextRequestId(),
    ...headers,
  });
  res.end(text);
}

export function sendError(res: ServerResponse, status: number, message: string, extra?: Record<string, unknown>): void {
  sendJson(res, status, { message, documentation_url: 'https://docs.github.com/rest', ...(extra ?? {}) });
}

export function sendEmpty(res: ServerResponse, status: number, headers: Record<string, string | number> = {}): void {
  res.writeHead(status, { 'x-github-request-id': nextRequestId(), 'content-length': 0, ...headers });
  res.end();
}

/** The request body, undoing a gzip or deflate content-encoding (real git gzips large bodies). */
export function bodyOf(req: IncomingMessage, limit = 512 * 1024 * 1024): Buffer {
  let body = req.body;
  const encoding = (req.headers['content-encoding'] ?? '').toLowerCase();
  if (encoding !== '' && encoding !== 'identity') {
    try {
      if (encoding === 'gzip' || encoding === 'x-gzip') body = gunzipSync(body);
      else if (encoding === 'deflate') body = inflateSync(body);
    } catch {
      throw new HttpError(400, 'could not decode the request body');
    }
  }
  if (body.length > limit) throw new HttpError(413, 'request body too large');
  return body;
}

/** bodyOf, for callers written to await it. */
export async function readBody(req: IncomingMessage, limit = 512 * 1024 * 1024): Promise<Buffer> {
  return bodyOf(req, limit);
}

/** Answer with JSON bytes already serialised (the response caches). */
export function sendJsonBytes(res: ServerResponse, status: number, body: Buffer, headers: Record<string, string | number> = {}): void {
  res.writeHead(status, { 'content-type': JSON_TYPE, 'content-length': body.length, 'x-github-request-id': nextRequestId(), ...headers });
  res.end(body);
}

/** Parse a JSON request body; an empty body is `{}`, malformed JSON is a 400. */
export function parseJsonBody(raw: Buffer): Record<string, unknown> {
  if (raw.length === 0) return {};
  const text = raw.toString('utf8');
  if (text.trim() === '') return {};
  let value: unknown;
  try {
    value = JSON.parse(text);
  } catch {
    throw new HttpError(400, 'Problems parsing JSON');
  }
  if (value === null || typeof value !== 'object' || Array.isArray(value)) {
    throw new HttpError(400, 'Body should be a JSON object');
  }
  return value as Record<string, unknown>;
}

/**
 * Whether the request carries the forge's credential: `Authorization: Bearer <token>` (or `token <token>`),
 * or HTTP Basic whose password is the token (what a git credential helper sends).
 */
export function isAuthorized(req: IncomingMessage): boolean {
  const header = req.headers.authorization;
  if (typeof header !== 'string' || header.trim() === '') return false;
  const space = header.indexOf(' ');
  if (space < 0) return false;
  const scheme = header.slice(0, space).toLowerCase();
  const value = header.slice(space + 1).trim();
  if (value === '') return false;
  let presented = value;
  if (scheme === 'basic') {
    const decoded = Buffer.from(value, 'base64').toString('utf8');
    const colon = decoded.indexOf(':');
    presented = colon >= 0 ? decoded.slice(colon + 1) : decoded;
  } else if (scheme !== 'bearer' && scheme !== 'token') {
    return false;
  }
  return config.token === '' ? presented !== '' : presented === config.token;
}

/** The absolute origin a client used to reach us, for Link headers and urls in bodies. */
export function originOf(req: IncomingMessage): string {
  const host = req.headers.host ?? 'localhost';
  return `http://${host}`;
}
