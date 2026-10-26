import fs from 'node:fs/promises';
import path from 'node:path';
import { createHash } from 'node:crypto';
import sharp from 'sharp';
import { PluginError } from './errors.mjs';

/**
 * Photos for 검수조서 and other documents that leave the company. Phone photos carry the
 * location (GPS), the device, the capture time and lens data; images forwarded by messengers
 * often keep them. They are removed before anything is stored.
 *
 * - The capture date is read first and returned (the 입고일자 of an inspection report is the day
 *   the photo was taken). Time and place are never kept; once stripped they cannot be recovered.
 * - `sharp().rotate()` applies the orientation to the pixels, so no orientation tag is needed, and
 *   the colours are converted to sRGB, so dropping the profile does not wash them out.
 * - HEIC (iPhone) is converted with `heic-convert`; sharp's prebuilt libvips cannot decode HEVC.
 * - Output is JPEG, long side at most 3000 px, at most 4 MB.
 * - The result is checked: no APP1 (EXIF/XMP) and no APP13 (IPTC/Photoshop) segment may remain.
 */

export const MAX_SIDE = 3000;
export const MAX_BYTES = 4 * 1024 * 1024;

const HEIF_BRANDS = new Set(['heic', 'heix', 'hevc', 'hevx', 'heim', 'heis', 'hevm', 'hevs', 'mif1', 'msf1']);

/** ISO-BMFF `ftyp` with a HEIF brand. */
export function isHeif(buf) {
  if (buf.length < 12 || buf.toString('ascii', 4, 8) !== 'ftyp') return false;
  return HEIF_BRANDS.has(buf.toString('ascii', 8, 12));
}

/**
 * Markers of the JPEG segments before the image data (SOS). Throws on a buffer that is not a
 * well-formed JPEG header.
 */
export function jpegSegments(buf) {
  if (buf[0] !== 0xFF || buf[1] !== 0xD8) throw new Error('not a JPEG');
  const out = [];
  let o = 2;
  while (o + 4 <= buf.length) {
    if (buf[o] !== 0xFF) throw new Error(`bad JPEG marker at ${o}`);
    const marker = buf[o + 1];
    if (marker === 0xFF) { o++; continue; }               // fill byte
    o += 2;
    if (marker === 0x01 || (marker >= 0xD0 && marker <= 0xD7)) continue;   // standalone
    const len = buf.readUInt16BE(o);
    out.push(marker);
    if (marker === 0xDA) break;                           // SOS: image data follows
    o += len;
  }
  return out;
}

/** Throws unless the JPEG has no APP1 and no APP13 segment. */
export function assertNoMetadata(buf) {
  const left = jpegSegments(buf).filter((m) => m === 0xE1 || m === 0xED);
  if (left.length) throw new PluginError('INTERNAL', '사진의 메타데이터가 지워지지 않았습니다.');
}

/**
 * Reads the capture date (`YYYY-MM-DD`) from a TIFF/EXIF block: DateTimeOriginal, then
 * DateTimeDigitized, then IFD0 DateTime. Returns null when absent or malformed.
 */
export function exifDate(tiff) {
  try {
    const order = tiff.toString('ascii', 0, 2);
    if (order !== 'II' && order !== 'MM') return null;
    const le = order === 'II';
    const u16 = (o) => (le ? tiff.readUInt16LE(o) : tiff.readUInt16BE(o));
    const u32 = (o) => (le ? tiff.readUInt32LE(o) : tiff.readUInt32BE(o));
    if (u16(2) !== 42) return null;
    const entries = (off) => {
      const count = u16(off);
      return Array.from({ length: count }, (_, i) => off + 2 + i * 12);
    };
    const ascii = (e) => {
      const count = u32(e + 4);
      const at = count <= 4 ? e + 8 : u32(e + 8);
      return tiff.toString('ascii', at, at + count).replace(/\0[\s\S]*$/, '');
    };
    let exifIfd = null;
    let dateTime = null;
    for (const e of entries(u32(4))) {
      const tag = u16(e);
      if (tag === 0x8769) exifIfd = u32(e + 8);
      if (tag === 0x0132) dateTime = ascii(e);
    }
    let original = null;
    let digitized = null;
    if (exifIfd) {
      for (const e of entries(exifIfd)) {
        const tag = u16(e);
        if (tag === 0x9003) original = ascii(e);
        if (tag === 0x9004) digitized = ascii(e);
      }
    }
    const m = String(original ?? digitized ?? dateTime ?? '').match(/^(\d{4}):(\d{2}):(\d{2})/);
    if (!m || m[1] === '0000' || m[2] === '00' || m[3] === '00') return null;
    return `${m[1]}-${m[2]}-${m[3]}`;
  } catch {
    return null;
  }
}

/** The TIFF header inside an `Exif\0\0` block anywhere in `buf` (JPEG APP1, HEIF Exif item). */
export function findTiff(buf) {
  const sig = Buffer.from('Exif\0\0', 'binary');
  let at = buf.indexOf(sig);
  while (at >= 0) {
    const t = buf.subarray(at + 6);
    const head = t.toString('ascii', 0, 4);
    if (head === 'II*\0' || head === 'MM\0*') return t;
    at = buf.indexOf(sig, at + 1);
  }
  return null;
}

async function captureDate(input) {
  try {
    const meta = await sharp(input, { failOn: 'none' }).metadata();
    if (meta.exif) {
      const t = findTiff(meta.exif) ?? meta.exif;
      const d = exifDate(t);
      if (d) return d;
    }
  } catch { /* not decodable by sharp (HEVC HEIC): fall through to a raw scan */ }
  const t = findTiff(input);
  return t ? exifDate(t) : null;
}

async function decodable(input) {
  if (!isHeif(input)) return input;
  try {
    await sharp(input).metadata().then((m) => { if (!m.width) throw new Error('no size'); });
    await sharp(input).resize(8).toBuffer();   // AVIF-coded HEIF decodes; HEVC throws here
    return input;
  } catch {
    const { default: convert } = await import('heic-convert');
    try {
      return Buffer.from(await convert({ buffer: input, format: 'JPEG', quality: 0.92 }));
    } catch (cause) {
      throw new PluginError('VALIDATION', 'HEIC 사진을 읽지 못했습니다. JPEG 로 다시 보내 주세요.', { cause });
    }
  }
}

/** Re-encodes to a metadata-free JPEG within MAX_SIDE and MAX_BYTES. */
async function encode(input, { maxSide = MAX_SIDE, maxBytes = MAX_BYTES } = {}) {
  let side = maxSide;
  let quality = 88;
  for (let round = 0; round < 12; round++) {
    const { data, info } = await sharp(input, { failOn: 'none' })
      .rotate()
      .resize(side, side, { fit: 'inside', withoutEnlargement: true })
      .jpeg({ quality, chromaSubsampling: '4:2:0' })
      .toBuffer({ resolveWithObject: true });
    if (data.length <= maxBytes) return { data, width: info.width, height: info.height };
    if (quality > 72) quality -= 8;
    else side = Math.round(side * 0.85);
  }
  throw new PluginError('VALIDATION', '사진을 4MB 아래로 줄이지 못했습니다.');
}

/**
 * Strips, normalises and stores one photo.
 *
 *   const p = await ingestPhoto('/inbox/abc/IMG_1.HEIC', '/data/files/hong/photos');
 *   // { file: '3f…e1.jpg', path, sha256, takenDate: '2026-09-18' | null, width, height, bytes, created }
 *
 * The file is named after the sha256 of the stored bytes, so the same picture sent twice is stored
 * once (`created: false`).
 */
export async function ingestPhoto(inputPath, destDir, options = {}) {
  let input;
  try {
    input = await fs.readFile(inputPath);
  } catch (cause) {
    throw new PluginError('NOT_FOUND', '받은 사진 파일을 찾지 못했습니다.', { cause });
  }
  const takenDate = await captureDate(input);
  let out;
  try {
    out = await encode(await decodable(input), options);
  } catch (e) {
    if (e instanceof PluginError) throw e;
    throw new PluginError('VALIDATION', '사진으로 읽을 수 없는 파일입니다.', { cause: e });
  }
  assertNoMetadata(out.data);

  const sha256 = createHash('sha256').update(out.data).digest('hex');
  const file = `${sha256.slice(0, 24)}.jpg`;
  const full = path.join(destDir, file);
  await fs.mkdir(destDir, { recursive: true });
  let created = true;
  try {
    await fs.writeFile(full, out.data, { flag: 'wx' });
  } catch (e) {
    if (e.code !== 'EEXIST') throw e;
    created = false;
  }
  return { file, path: full, sha256, takenDate, width: out.width, height: out.height, bytes: out.data.length, created };
}
