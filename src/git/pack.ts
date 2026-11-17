// Packfiles (gitformat-pack): reading every entry type, including OFS_DELTA and REF_DELTA, and writing
// packs of whole objects. Compressed object bodies are cached per id, so serving the same object again
// (fifty clones of one repository) costs a buffer copy instead of a deflate.

import { createHash } from 'node:crypto';
import { constants as zc, deflateSync, inflateSync } from 'node:zlib';
import { hashObject, type GitObject, type ObjectType } from './objects.ts';

const TYPE_BY_CODE: Record<number, ObjectType> = { 1: 'commit', 2: 'tree', 3: 'blob', 4: 'tag' };
const CODE_BY_TYPE: Record<ObjectType, number> = { commit: 1, tree: 2, blob: 3, tag: 4 };
const OFS_DELTA = 6;
const REF_DELTA = 7;

export class PackError extends Error {}

export interface ParsedObject extends GitObject {
  readonly id: string;
}

/** Inflate one zlib stream at `offset`; returns the bytes and how much input the stream used. */
function inflateAt(buf: Buffer, offset: number): { data: Buffer; used: number } {
  const result = inflateSync(buf.subarray(offset), { info: true, finishFlush: zc.Z_SYNC_FLUSH }) as unknown as {
    buffer: Buffer;
    engine: { bytesWritten: number };
  };
  return { data: result.buffer, used: result.engine.bytesWritten };
}

/** Apply a git delta (gitformat-pack "deltified representation") to `base`. */
export function applyDelta(base: Buffer, delta: Buffer): Buffer {
  let at = 0;
  const readSize = (): number => {
    let size = 0;
    let shift = 0;
    let byte: number;
    do {
      byte = delta[at++]!;
      size += (byte & 0x7f) * 2 ** shift;
      shift += 7;
    } while (byte & 0x80);
    return size;
  };
  const baseSize = readSize();
  if (baseSize !== base.length) throw new PackError(`delta base size ${baseSize} does not match ${base.length}`);
  const resultSize = readSize();
  const out = Buffer.allocUnsafe(resultSize);
  let written = 0;
  while (at < delta.length) {
    const op = delta[at++]!;
    if (op & 0x80) {
      let copyOffset = 0;
      let copySize = 0;
      if (op & 0x01) copyOffset |= delta[at++]!;
      if (op & 0x02) copyOffset |= delta[at++]! << 8;
      if (op & 0x04) copyOffset |= delta[at++]! << 16;
      if (op & 0x08) copyOffset += delta[at++]! * 0x1000000;
      if (op & 0x10) copySize |= delta[at++]!;
      if (op & 0x20) copySize |= delta[at++]! << 8;
      if (op & 0x40) copySize |= delta[at++]! << 16;
      if (copySize === 0) copySize = 0x10000;
      if (copyOffset + copySize > base.length || written + copySize > resultSize) throw new PackError('delta copy out of range');
      base.copy(out, written, copyOffset, copyOffset + copySize);
      written += copySize;
    } else if (op > 0) {
      if (at + op > delta.length || written + op > resultSize) throw new PackError('delta insert out of range');
      delta.copy(out, written, at, at + op);
      at += op;
      written += op;
    } else {
      throw new PackError('delta opcode 0 is reserved');
    }
  }
  if (written !== resultSize) throw new PackError('delta result size mismatch');
  return out;
}

/**
 * Read a whole pack. `lookup` resolves a REF_DELTA base the pack itself does not carry (a thin pack's
 * base, already in the repository). Every object's id is computed from its bytes.
 */
export function readPack(pack: Buffer, lookup: (id: string) => GitObject | undefined): ParsedObject[] {
  if (pack.length < 32) throw new PackError('pack is too short');
  if (pack.toString('ascii', 0, 4) !== 'PACK') throw new PackError('missing PACK signature');
  const version = pack.readUInt32BE(4);
  if (version !== 2 && version !== 3) throw new PackError(`unsupported pack version ${version}`);
  const count = pack.readUInt32BE(8);
  const trailer = pack.subarray(pack.length - 20);
  const digest = createHash('sha1').update(pack.subarray(0, pack.length - 20)).digest();
  if (!digest.equals(trailer)) throw new PackError('pack checksum mismatch');

  interface Entry {
    offset: number;
    type: ObjectType | null;
    data: Buffer;
    baseOffset?: number;
    baseId?: string;
  }
  const entries: Entry[] = [];
  const byOffset = new Map<number, Entry>();
  let at = 12;
  const end = pack.length - 20;
  for (let i = 0; i < count; i++) {
    if (at >= end) throw new PackError('pack ends before its declared object count');
    const start = at;
    let byte = pack[at++]!;
    const code = (byte >> 4) & 7;
    let size = byte & 0x0f;
    let shift = 4;
    while (byte & 0x80) {
      byte = pack[at++]!;
      size += (byte & 0x7f) * 2 ** shift;
      shift += 7;
    }
    let entry: Entry;
    if (code === OFS_DELTA) {
      byte = pack[at++]!;
      let rel = byte & 0x7f;
      while (byte & 0x80) {
        byte = pack[at++]!;
        rel = (rel + 1) * 128 + (byte & 0x7f);
      }
      const { data, used } = inflateAt(pack, at);
      at += used;
      entry = { offset: start, type: null, data, baseOffset: start - rel };
    } else if (code === REF_DELTA) {
      const baseId = pack.toString('hex', at, at + 20);
      at += 20;
      const { data, used } = inflateAt(pack, at);
      at += used;
      entry = { offset: start, type: null, data, baseId };
    } else {
      const type = TYPE_BY_CODE[code];
      if (type === undefined) throw new PackError(`unknown pack object type ${code}`);
      const { data, used } = inflateAt(pack, at);
      at += used;
      if (data.length !== size) throw new PackError(`object at ${start} inflated to ${data.length} bytes, header says ${size}`);
      entry = { offset: start, type, data };
    }
    entries.push(entry);
    byOffset.set(start, entry);
  }

  // Resolve deltas; bases may themselves be deltas, so resolve on demand with memoisation.
  const resolved = new Map<Entry, { type: ObjectType; data: Buffer; id: string }>();
  const byId = new Map<string, { type: ObjectType; data: Buffer }>();
  const resolve = (entry: Entry, depth: number): { type: ObjectType; data: Buffer; id: string } => {
    const done = resolved.get(entry);
    if (done !== undefined) return done;
    if (depth > 10_000) throw new PackError('delta chain too deep');
    let result: { type: ObjectType; data: Buffer; id: string };
    if (entry.type !== null) {
      result = { type: entry.type, data: entry.data, id: hashObject(entry.type, entry.data) };
    } else {
      let base: { type: ObjectType; data: Buffer } | undefined;
      if (entry.baseOffset !== undefined) {
        const baseEntry = byOffset.get(entry.baseOffset);
        if (baseEntry === undefined) throw new PackError(`ofs-delta base at ${entry.baseOffset} is not in the pack`);
        base = resolve(baseEntry, depth + 1);
      } else {
        const id = entry.baseId!;
        base = byId.get(id);
        if (base === undefined) {
          for (const candidate of entries) {
            if (candidate === entry || resolved.has(candidate) || candidate.type === null) continue;
            const r = resolve(candidate, depth + 1);
            byId.set(r.id, r);
            if (r.id === id) {
              base = r;
              break;
            }
          }
        }
        base ??= lookup(id);
        if (base === undefined) throw new PackError(`ref-delta base ${id} is missing`);
      }
      const data = applyDelta(base.data, entry.data);
      result = { type: base.type, data, id: hashObject(base.type, data) };
    }
    resolved.set(entry, result);
    byId.set(result.id, result);
    return result;
  };
  const out: ParsedObject[] = [];
  for (const entry of entries) {
    const r = resolve(entry, 0);
    out.push({ id: r.id, type: r.type, data: r.data });
  }
  return out;
}

// ------------------------------------------------------------------ writing

const deflated = new Map<string, Buffer>();
let deflatedBytes = 0;
const DEFLATE_CACHE_LIMIT = 256 * 1024 * 1024;

function entryHeader(type: ObjectType, size: number): Buffer {
  const bytes: number[] = [];
  let byte = (CODE_BY_TYPE[type] << 4) | (size & 0x0f);
  let rest = Math.floor(size / 16);
  while (rest > 0) {
    bytes.push(byte | 0x80);
    byte = rest & 0x7f;
    rest = Math.floor(rest / 128);
  }
  bytes.push(byte);
  return Buffer.from(bytes);
}

/** The pack entry (header + deflated body) of one object, cached by its id. */
function packEntry(id: string, object: GitObject): Buffer {
  let cached = deflated.get(id);
  if (cached === undefined) {
    cached = Buffer.concat([entryHeader(object.type, object.data.length), deflateSync(object.data, { level: 1 })]);
    if (deflatedBytes + cached.length <= DEFLATE_CACHE_LIMIT) {
      deflated.set(id, cached);
      deflatedBytes += cached.length;
    }
  }
  return cached;
}

/** Write a version-2 pack of whole objects, in the given order. */
export function writePack(ids: readonly string[], get: (id: string) => GitObject | undefined): Buffer {
  const parts: Buffer[] = [];
  const header = Buffer.alloc(12);
  header.write('PACK', 0, 'ascii');
  header.writeUInt32BE(2, 4);
  header.writeUInt32BE(ids.length, 8);
  parts.push(header);
  const hash = createHash('sha1');
  hash.update(header);
  for (const id of ids) {
    const object = get(id);
    if (object === undefined) throw new PackError(`object ${id} is missing from the repository`);
    const entry = packEntry(id, object);
    hash.update(entry);
    parts.push(entry);
  }
  parts.push(hash.digest());
  return Buffer.concat(parts);
}
