// Packfiles (gitprotocol-pack): reading packs the client pushes (deltas resolved, trailer
// verified) and writing packs the server fetches (full objects only, no deltas — the client
// resolves deltas, but nothing in the suite requires the server to send any, and plain objects
// keep the fetch path fast and simple).
//
// Layout: "PACK" + version u32be (2) + count u32be, then one entry per object: a type/size varint
// header + a zlib stream, then a 20-byte SHA-1 trailer over everything before it. Type codes:
// commit 1, tree 2, blob 3, tag 4, ofs-delta 6, ref-delta 7.

import { hash as cryptoHash, type BinaryLike } from 'node:crypto';
import { deflateSync, inflateSync } from 'node:zlib';
import { hashObject, type GitObject, type GitObjectType } from './git-objects.ts';

/** The packfile type codes. */
const TYPE_CODE: Record<GitObjectType, number> = { commit: 1, tree: 2, blob: 3, tag: 4 };
const CODE_TYPE: Record<number, GitObjectType> = { 1: 'commit', 2: 'tree', 3: 'blob', 4: 'tag' };

/** One parsed pack entry, before delta resolution. */
interface PackEntry {
  readonly offset: number;
  readonly typeCode: number;
  readonly size: number;
  /** The raw (delta) payload, inflated. */
  readonly data: Buffer;
  readonly delta: { kind: 'ofs'; baseOffset: number } | { kind: 'ref'; baseOid: string } | null;
}

/** What readPack returns: the resolved objects and the pack's own metadata. */
export interface PackReadResult {
  readonly version: number;
  readonly declaredCount: number;
  readonly objects: Map<string, GitObject>;
  readonly deltaCount: number;
  readonly byteLength: number;
  readonly checksumOk: boolean;
}

/** Read the type/size varint header at `offset` into the reused scratch object `out`
 * ({typeCode, size, nextOffset}): the caller passes one object for the whole pack, so the hot
 * path allocates no per-entry header object; the logic is the same varint scan. */
function readEntryHeaderInto(
  pack: Buffer,
  offset: number,
  out: { typeCode: number; size: number; next: number },
): void {
  if (offset >= pack.length) throw new Error('pack-truncated: entry header past the end');
  const first = pack[offset];
  out.typeCode = (first >> 4) & 0x07;
  out.size = first & 0x0f;
  let shift = 4;
  let at = offset + 1;
  let byte = first;
  while ((byte & 0x80) !== 0) {
    if (at >= pack.length) throw new Error('pack-truncated: size varint past the end');
    byte = pack[at];
    out.size |= (byte & 0x7f) << shift;
    shift += 7;
    at += 1;
    if (shift > 64) throw new Error('pack-corrupt: size varint too long');
  }
  out.next = at;
}

/** Read the OFS_DELTA offset varint at `offset`: {value, nextOffset}. */
function readOffsetVarint(pack: Buffer, offset: number): { value: number; next: number } {
  if (offset >= pack.length) throw new Error('pack-truncated: offset varint past the end');
  let byte = pack[offset];
  let value = byte & 0x7f;
  let at = offset + 1;
  while ((byte & 0x80) !== 0) {
    if (at >= pack.length) throw new Error('pack-truncated: offset varint past the end');
    byte = pack[at];
    value = ((value + 1) * 128) + (byte & 0x7f);
    at += 1;
  }
  return { value, next: at };
}

/** Read one delta-stream size varint (7-bit little-endian groups). */
function readDeltaVarint(data: Buffer, offset: number): { value: number; next: number } {
  let value = 0;
  let shift = 0;
  let at = offset;
  while (true) {
    if (at >= data.length) throw new Error('delta-corrupt: size varint past the end');
    const byte = data[at];
    value |= (byte & 0x7f) << shift;
    shift += 7;
    at += 1;
    if ((byte & 0x80) === 0) break;
    if (shift > 64) throw new Error('delta-corrupt: size varint too long');
  }
  return { value, next: at };
}

/** Apply a delta stream to its base bytes, returning the reconstructed object. */
export function applyDelta(base: Buffer, delta: Buffer): Buffer {
  const source = readDeltaVarint(delta, 0);
  if (source.value !== base.length) throw new Error('delta-base-size: the delta names another base size');
  const target = readDeltaVarint(delta, source.next);
  const out: Buffer[] = [];
  let produced = 0;
  let at = target.next;
  while (at < delta.length) {
    const opcode = delta[at];
    at += 1;
    if (opcode === 0) throw new Error('delta-corrupt: opcode 0');
    if ((opcode & 0x80) !== 0) {
      // Copy from the base: bits 0-3 select offset bytes, bits 4-6 select size bytes.
      let offset = 0;
      let size = 0;
      for (let bit = 0; bit < 4; bit += 1) {
        if ((opcode & (1 << bit)) !== 0) {
          if (at >= delta.length) throw new Error('delta-corrupt: copy offset past the end');
          offset |= delta[at] << (bit * 8);
          at += 1;
        }
      }
      for (let bit = 0; bit < 3; bit += 1) {
        if ((opcode & (0x10 << bit)) !== 0) {
          if (at >= delta.length) throw new Error('delta-corrupt: copy size past the end');
          size |= delta[at] << (bit * 8);
          at += 1;
        }
      }
      if (size === 0) size = 0x10000;
      if (offset + size > base.length) throw new Error('delta-corrupt: copy past the base');
      out.push(base.subarray(offset, offset + size));
      produced += size;
    } else {
      // Insert the next `opcode` literal bytes.
      if (at + opcode > delta.length) throw new Error('delta-corrupt: insert past the end');
      out.push(delta.subarray(at, at + opcode));
      produced += opcode;
      at += opcode;
    }
  }
  if (produced !== target.value) throw new Error('delta-size: the reconstructed size differs');
  return Buffer.concat(out);
}

/**
 * Parse a packfile and resolve every entry (deltas included) to its object. `resolveBase` supplies
 * a REF_DELTA base the pack does not hold (a thin pack's base, held by the reader). Throws on any
 * structural fault: bad header, bad version, bad trailer, bad zlib, size mismatch, stray bytes,
 * an unresolvable delta, or a delta cycle.
 */
export function readPack(pack: Buffer, options: { resolveBase?: (oid: string) => GitObject | undefined } = {}): PackReadResult {
  if (pack.length < 32) throw new Error('pack-truncated: a pack is at least 32 bytes');
  if (pack[0] !== 0x50 || pack[1] !== 0x41 || pack[2] !== 0x43 || pack[3] !== 0x4b) {
    throw new Error('pack-header: not a packfile');
  }
  const version = pack.readUInt32BE(4);
  if (version !== 2 && version !== 3) throw new Error(`pack-version: ${version}`);
  const declaredCount = pack.readUInt32BE(8);
  const trailerStart = pack.length - 20;
  const trailer = pack.subarray(trailerStart);
  // One-shot hash over the single input buffer: no Hash object. The encoding is passed
  // explicitly ('hex'): the no-encoding default differs between Node versions (a Buffer on
  // Node 22, a hex string on Node 24), so the comparison is on hex strings on both.
  const computed = cryptoHash('sha1', pack.subarray(0, trailerStart) as BinaryLike, 'hex');
  if (computed !== trailer.toString('hex')) throw new Error('pack-checksum: the trailer does not match the bytes');

  const entries: PackEntry[] = [];
  // One scratch header object, reused for every entry (never escapes the loop).
  const header = { typeCode: 0, size: 0, next: 0 };
  let offset = 12;
  let deltaCount = 0;
  for (let index = 0; index < declaredCount; index += 1) {
    const entryOffset = offset;
    readEntryHeaderInto(pack, offset, header);
    offset = header.next;
    let delta: PackEntry['delta'] = null;
    if (header.typeCode === 6) {
      const base = readOffsetVarint(pack, offset);
      const baseOffset = entryOffset - base.value;
      if (baseOffset <= 0 || baseOffset >= entryOffset) throw new Error('pack-delta: bad ofs-delta base offset');
      delta = { kind: 'ofs', baseOffset };
      offset = base.next;
      deltaCount += 1;
    } else if (header.typeCode === 7) {
      if (offset + 20 > trailerStart) throw new Error('pack-truncated: ref-delta base oid past the end');
      delta = { kind: 'ref', baseOid: pack.subarray(offset, offset + 20).toString('hex') };
      offset += 20;
      deltaCount += 1;
    } else if (CODE_TYPE[header.typeCode] === undefined) {
      throw new Error(`pack-type: unknown type code ${header.typeCode}`);
    }
    // inflateSync stops at the first zlib stream's end and ignores trailing bytes, and Node exposes
    // no consumed-length for it. The fast path reads the engine's remaining-input count
    // (_writeState[1]) from an info:true inflate. The first entry of the process verifies that
    // count two ways — the slice must inflate to the same bytes (catches a short length) and one
    // byte less must fail (catches a long one) — and once it passes, later entries trust the
    // validated mechanism (its meaning is a property of the Node version, not the data). When
    // the fast path is unavailable or unverifiable, binary search finds the length: inflate
    // throws on a truncated stream and succeeds on any slice at least as long as the stream.
    const rest = pack.subarray(offset, trailerStart);
    const { data: inflated, consumed } = inflateEntry(rest);
    if (inflated.length !== header.size) throw new Error('pack-size: the inflated size differs from the header');
    if (consumed === 0) throw new Error('pack-zlib: the entry consumed no bytes');
    entries.push({ offset: entryOffset, typeCode: header.typeCode, size: header.size, data: inflated, delta });
    offset += consumed;
  }
  if (offset !== trailerStart) throw new Error('pack-trailing: bytes after the last entry');

  // The common case (every pack the suite pushes is delta-free): no delta machinery at all,
  // just hash each entry straight into the result.
  if (deltaCount === 0) {
    const objects = new Map<string, GitObject>();
    for (const entry of entries) {
      const object: GitObject = { type: CODE_TYPE[entry.typeCode], data: entry.data };
      objects.set(hashObject(object.type, object.data), object);
    }
    return { version, declaredCount, objects, deltaCount, byteLength: pack.length, checksumOk: true };
  }

  // Resolve deltas, recursively, with cycle detection.
  const byOffset = new Map<number, PackEntry>();
  for (const entry of entries) byOffset.set(entry.offset, entry);
  const resolved = new Map<number, GitObject>();
  const resolving = new Set<number>();
  const resolve = (entry: PackEntry): GitObject => {
    const done = resolved.get(entry.offset);
    if (done) return done;
    if (resolving.has(entry.offset)) throw new Error('pack-delta-cycle');
    resolving.add(entry.offset);
    let object: GitObject;
    if (entry.delta === null) {
      object = { type: CODE_TYPE[entry.typeCode], data: entry.data };
    } else {
      let base: GitObject | undefined;
      if (entry.delta.kind === 'ofs') {
        const baseEntry = byOffset.get(entry.delta.baseOffset);
        if (!baseEntry) throw new Error('pack-delta-base: ofs-delta base not in the pack');
        base = resolve(baseEntry);
      } else {
        base = options.resolveBase?.(entry.delta.baseOid);
        if (!base) {
          // A ref-delta base may also be an entry of this pack: find it by its resolved oid.
          for (const candidate of entries) {
            if (candidate.delta !== null) continue;
            if (hashObject(CODE_TYPE[candidate.typeCode], candidate.data) === entry.delta.baseOid) {
              base = resolve(candidate);
              break;
            }
          }
        }
        if (!base) throw new Error(`pack-delta-base: ref-delta base ${entry.delta.baseOid} is nowhere`);
      }
      object = { type: base.type, data: applyDelta(base.data, entry.data) };
    }
    resolving.delete(entry.offset);
    resolved.set(entry.offset, object);
    return object;
  };
  const objects = new Map<string, GitObject>();
  for (const entry of entries) {
    const object = resolve(entry);
    objects.set(hashObject(object.type, object.data), object);
  }
  return { version, declaredCount, objects, deltaCount, byteLength: pack.length, checksumOk: true };
}

/**
 * Whether the _writeState fast path has been validated on this Node runtime. The meaning of
 * engine._writeState is a property of the Node version, not of the data, so one full validation
 * per process is enough; after that, each entry costs a single inflation instead of three.
 */
let writeStateValidated = false;

/**
 * Inflate the zlib stream at the start of `bytes` and find its exact length. The fast path uses
 * the engine's remaining-input count; the fallback binary-searches the smallest succeeding
 * prefix. Throws when the stream does not inflate at all.
 */
function inflateEntry(bytes: Buffer): { data: Buffer; consumed: number } {
  let data: Buffer;
  let engine: { _writeState?: Uint32Array } | undefined;
  try {
    const result = inflateSync(bytes, { info: true }) as unknown as {
      buffer: Buffer;
      engine?: { _writeState?: Uint32Array };
    };
    data = result.buffer;
    engine = result.engine;
  } catch {
    throw new Error('pack-zlib: the entry does not inflate');
  }
  const remaining = engine?._writeState?.[1];
  if (typeof remaining === 'number' && remaining >= 0 && remaining < bytes.length) {
    const consumed = bytes.length - remaining;
    if (consumed > 0) {
      // Once the mechanism has been validated on this runtime, one inflation per entry
      // suffices; the plausibility checks above still guard every entry.
      if (writeStateValidated) return { data, consumed };
      try {
        const sliced = inflateSync(bytes.subarray(0, consumed));
        if (sliced.equals(data)) {
          if (consumed === 1) {
            writeStateValidated = true;
            return { data, consumed };
          }
          try {
            inflateSync(bytes.subarray(0, consumed - 1));
          } catch {
            writeStateValidated = true;
            return { data, consumed };
          }
        }
      } catch {
        // The fast-path length did not verify; fall through to the binary search.
      }
    }
  }
  return { data, consumed: zlibStreamLength(bytes) };
}

/**
 * The length of the zlib stream at the start of `bytes`: the smallest prefix length whose inflate
 * succeeds. The caller has already inflated the whole remainder successfully, so a length exists.
 */
function zlibStreamLength(bytes: Buffer): number {
  let lo = 1;
  let hi = bytes.length;
  while (lo < hi) {
    const mid = Math.floor((lo + hi) / 2);
    try {
      inflateSync(bytes.subarray(0, mid));
      hi = mid;
    } catch {
      lo = mid + 1;
    }
  }
  return lo;
}

/** Encode one entry's type/size varint header. */
function entryHeader(typeCode: number, size: number): Buffer {
  const bytes: number[] = [];
  let first = (typeCode << 4) | (size & 0x0f);
  let rest = size >> 4;
  if (rest > 0) first |= 0x80;
  bytes.push(first);
  while (rest > 0) {
    let next = rest & 0x7f;
    rest >>= 7;
    if (rest > 0) next |= 0x80;
    bytes.push(next);
  }
  return Buffer.from(bytes);
}

/**
 * Build a packfile (version 2) from full objects, in the given order, each zlib-deflated on its
 * own. No deltas: every entry is a complete object, so the reader never needs a base.
 */
export function writePack(objects: readonly GitObject[]): Buffer {
  const parts: Buffer[] = [];
  const header = Buffer.alloc(12);
  header.write('PACK', 0, 'ascii');
  header.writeUInt32BE(2, 4);
  header.writeUInt32BE(objects.length, 8);
  parts.push(header);
  for (const object of objects) {
    parts.push(entryHeader(TYPE_CODE[object.type], object.data.length));
    parts.push(deflateSync(object.data));
  }
  const body = Buffer.concat(parts);
  const trailer = Buffer.from(cryptoHash('sha1', body as BinaryLike, 'hex'), 'hex');
  return Buffer.concat([body, trailer]);
}

/** The trailer of a pack (its last 20 bytes) as hex, and whether it matches the bytes. */
export function verifyPackChecksum(pack: Buffer): { ok: boolean; expected: string; actual: string } {
  if (pack.length < 20) return { ok: false, expected: '', actual: '' };
  const actual = pack.subarray(pack.length - 20).toString('hex');
  const expected = cryptoHash('sha1', pack.subarray(0, pack.length - 20) as BinaryLike, 'hex');
  return { ok: actual === expected, expected, actual };
}
