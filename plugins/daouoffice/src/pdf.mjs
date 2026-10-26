import { PDFDocument, PDFName, PDFDict, PDFRef } from 'pdf-lib';
import { PluginError } from './errors.mjs';

/**
 * Removes the metadata a PDF carries before it is stored and attached to documents that leave the
 * company: the document information dictionary (Author, Creator, Producer, Title, Subject, Keywords,
 * dates), XMP metadata streams (on the catalog, pages or any object) and application private data
 * (`/PieceInfo`). The file is rewritten in full, so earlier incremental revisions are dropped too.
 * Page content, fonts and images are kept as they are.
 */

const NAME = {
  metadata: PDFName.of('Metadata'),
  pieceInfo: PDFName.of('PieceInfo'),
  type: PDFName.of('Type'),
  subtype: PDFName.of('Subtype'),
  xml: PDFName.of('XML'),
};

const dictOf = (obj) => (obj instanceof PDFDict ? obj : obj?.dict instanceof PDFDict ? obj.dict : null);

export async function stripPdf(bytes) {
  let doc;
  try {
    doc = await PDFDocument.load(bytes, { updateMetadata: false });
  } catch (cause) {
    if (/encrypt/i.test(String(cause?.message))) {
      throw new PluginError('VALIDATION', '암호가 걸린 PDF 입니다. 암호를 푼 PDF 나 이미지로 다시 보내 주세요.', { cause });
    }
    throw new PluginError('VALIDATION', 'PDF 를 읽지 못했습니다. 다시 저장한 PDF 나 이미지로 보내 주세요.', { cause });
  }
  const ctx = doc.context;
  const drop = (value) => { if (value instanceof PDFRef) ctx.delete(value); };

  const info = ctx.trailerInfo.Info;
  drop(info);
  ctx.trailerInfo.Info = undefined;

  for (const [ref, obj] of ctx.enumerateIndirectObjects()) {
    const dict = dictOf(obj);
    if (!dict) continue;
    // A stream that is itself XMP metadata, referenced or not.
    if (dict.get(NAME.type) === NAME.metadata || dict.get(NAME.subtype) === NAME.xml) {
      ctx.delete(ref);
      continue;
    }
    for (const key of [NAME.metadata, NAME.pieceInfo]) {
      const v = dict.get(key);
      if (v === undefined) continue;
      drop(v);
      dict.delete(key);
    }
  }
  for (const key of [NAME.metadata, NAME.pieceInfo]) {
    const v = doc.catalog.get(key);
    if (v !== undefined) { drop(v); doc.catalog.delete(key); }
  }
  return Buffer.from(await doc.save({ useObjectStreams: false, updateFieldAppearances: false }));
}
