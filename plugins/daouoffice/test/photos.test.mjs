import { test } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import sharp from 'sharp';
import { ingestPhoto, jpegSegments, exifDate, findTiff, isHeif, assertNoMetadata } from '../src/photos.mjs';

const tmp = () => fs.mkdtempSync(path.join(os.tmpdir(), 'dao-photos-'));

/** A phone-like JPEG: EXIF with GPS, device and capture time, XMP, and a sideways orientation. */
async function phoneJpeg({ width = 400, height = 300, orientation = 6 } = {}) {
  return sharp({ create: { width, height, channels: 3, background: { r: 200, g: 40, b: 40 } } })
    .jpeg({ quality: 90 })
    .withMetadata({ orientation })
    .withExifMerge({
      IFD0: { Make: 'TestPhone', Model: 'TP-1', Software: 'TestOS 1.0', DateTime: '2026:02:03 09:10:11' },
      IFD2: { DateTimeOriginal: '2026:01:20 08:30:00', LensModel: 'Test Lens' },
      IFD3: { GPSLatitudeRef: 'N', GPSLatitude: '37/1 33/1 0/1', GPSLongitudeRef: 'E', GPSLongitude: '126/1 58/1 0/1' },
    })
    .withXmp('<x:xmpmeta xmlns:x="adobe:ns:meta/"><rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#"/></x:xmpmeta>')
    .toBuffer();
}

test('the fixture really carries GPS and APP1 segments', async () => {
  const buf = await phoneJpeg();
  assert.ok(jpegSegments(buf).includes(0xE1));
  const meta = await sharp(buf).metadata();
  assert.equal(meta.orientation, 6);
  assert.ok(meta.exif.includes(Buffer.from('TestPhone')));
  assert.ok(meta.exif.includes(Buffer.from([0x25, 0x88])) || meta.exif.includes(Buffer.from([0x88, 0x25])), 'GPS IFD pointer');
  assert.equal(exifDate(findTiff(meta.exif)), '2026-01-20');
  assert.throws(() => assertNoMetadata(buf));
});

test('ingestPhoto strips metadata, keeps the capture date and applies the orientation', async () => {
  const dir = tmp();
  const src = path.join(dir, 'IMG_0001.JPG');
  fs.writeFileSync(src, await phoneJpeg({ width: 400, height: 300, orientation: 6 }));

  const out = await ingestPhoto(src, path.join(dir, 'out'));
  assert.equal(out.takenDate, '2026-01-20');
  assert.equal(out.created, true);
  assert.match(out.file, /^[0-9a-f]{24}\.jpg$/);
  // Orientation 6 = rotate 90°: the stored pixels are portrait.
  assert.deepEqual([out.width, out.height], [300, 400]);

  const stored = fs.readFileSync(out.path);
  const segments = jpegSegments(stored);
  assert.ok(!segments.includes(0xE1), 'no APP1 (EXIF/XMP)');
  assert.ok(!segments.includes(0xED), 'no APP13 (IPTC)');
  assert.ok(!stored.includes(Buffer.from('TestPhone')));
  assert.ok(!stored.includes(Buffer.from('GPS')));
  const meta = await sharp(stored).metadata();
  assert.equal(meta.exif, undefined);
  assert.equal(meta.orientation, undefined);

  const again = await ingestPhoto(src, path.join(dir, 'out'));
  assert.equal(again.created, false, 'the same picture is stored once');
  assert.equal(again.sha256, out.sha256);
  fs.rmSync(dir, { recursive: true, force: true });
});

test('large photos are capped at 3000 px on the long side and 4 MB', async () => {
  const dir = tmp();
  const src = path.join(dir, 'big.png');
  // Noise compresses badly, so this also exercises the size loop.
  const w = 4200; const h = 2800;
  const noise = Buffer.alloc(w * h * 3);
  for (let i = 0; i < noise.length; i++) noise[i] = (i * 2654435761) >>> 24;
  await sharp(noise, { raw: { width: w, height: h, channels: 3 } }).png({ compressionLevel: 1 }).toFile(src);
  const out = await ingestPhoto(src, path.join(dir, 'out'));
  assert.ok(Math.max(out.width, out.height) <= 3000);
  assert.ok(out.bytes <= 4 * 1024 * 1024, `${out.bytes} bytes`);
  assert.equal(out.takenDate, null);
  fs.rmSync(dir, { recursive: true, force: true });
});

test('non-images are refused as VALIDATION and missing files as NOT_FOUND', async () => {
  const dir = tmp();
  const src = path.join(dir, 'note.txt');
  fs.writeFileSync(src, 'not a photo');
  await assert.rejects(ingestPhoto(src, dir), (e) => e.code === 'VALIDATION');
  await assert.rejects(ingestPhoto(path.join(dir, 'nope.jpg'), dir), (e) => e.code === 'NOT_FOUND');
  fs.rmSync(dir, { recursive: true, force: true });
});

test('HEIF detection and raw EXIF scan', () => {
  const ftyp = Buffer.concat([Buffer.from([0, 0, 0, 24]), Buffer.from('ftypheic'), Buffer.alloc(12)]);
  assert.ok(isHeif(ftyp));
  assert.ok(!isHeif(Buffer.from('ftypavif')));
  // Little-endian TIFF with IFD0 DateTime only.
  const tiff = Buffer.alloc(64);
  tiff.write('II', 0); tiff.writeUInt16LE(42, 2); tiff.writeUInt32LE(8, 4);
  tiff.writeUInt16LE(1, 8);                        // one entry
  tiff.writeUInt16LE(0x0132, 10); tiff.writeUInt16LE(2, 12); tiff.writeUInt32LE(20, 14); tiff.writeUInt32LE(30, 18);
  tiff.write('2026:05:06 07:08:09\0', 30, 'ascii');
  const container = Buffer.concat([Buffer.from('....mdat....'), Buffer.from('Exif\0\0', 'binary'), tiff]);
  assert.equal(exifDate(findTiff(container)), '2026-05-06');
  assert.equal(exifDate(Buffer.from('garbage')), null);
});

// A real HEVC HEIC can only be produced where an encoder exists (macOS `sips`). The host runs the
// same code path with photos sent from iPhones.
const SIPS = '/usr/bin/sips';
test('HEIC from an iPhone-style encoder is converted and stripped', { skip: !fs.existsSync(SIPS) && 'needs macOS sips to make a HEIC' }, async () => {
  const { execFileSync } = await import('node:child_process');
  const dir = tmp();
  const jpg = path.join(dir, 'src.jpg');
  const heic = path.join(dir, 'IMG_0002.HEIC');
  fs.writeFileSync(jpg, await phoneJpeg({ width: 320, height: 240, orientation: 1 }));
  execFileSync(SIPS, ['-s', 'format', 'heic', jpg, '--out', heic], { stdio: 'ignore' });
  assert.ok(isHeif(fs.readFileSync(heic)));
  const out = await ingestPhoto(heic, path.join(dir, 'out'));
  const stored = fs.readFileSync(out.path);
  assert.equal((await sharp(stored).metadata()).format, 'jpeg');
  assert.ok(!jpegSegments(stored).includes(0xE1));
  assert.deepEqual([out.width, out.height], [320, 240]);
  assert.equal(out.takenDate, '2026-01-20', 'capture date read from the HEIC before conversion');
  fs.rmSync(dir, { recursive: true, force: true });
});
