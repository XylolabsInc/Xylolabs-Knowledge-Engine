import { test } from 'node:test';
import assert from 'node:assert/strict';
import { checkWriting, checkNoteStyle } from '../src/dao/writing.mjs';

test('plain written Korean passes', () => {
  for (const ok of [
    '현장 로봇 원격조종',
    '실증 부지 인허가 협의',
    '로봇 원격제어 시 실시간 화면 표시용',
    'https://example.com/product?id=1~2',
  ]) assert.deepEqual(checkWriting(ok, '목적'), [], ok);
});

test('banned patterns are caught with the field label', () => {
  const cases = [
    ['근데 필요하다', '구어체 접속'],
    ['그냥 산다', '구어체 강조'],
    ['서버를 돌려서 확인', '개발자 은어'],
    ['카메라를 활용한 점검', '번역체'],
    ['장비를 통해 수집', '번역체'],
    ['금번 구매', '공문서체'],
    ['검토하고자 한다', '예스런 종결'],
    ['읽혀지는 문서', '이중 피동'],
    ['안 맞는다', '짧은 부정'],
    ['제어시 사용', '띄어쓰기'],
    ['할수 있다', '띄어쓰기'],
    ['카메라·마이크', '가운뎃점'],
    ['3~4일 사용', '물결표 범위'],
    ['“로봇”', '둥근 따옴표'],
    ['영수증 R12 참고', '내부 참조'],
  ];
  for (const [text, what] of cases) {
    const found = checkWriting(text, '구입목적');
    assert.ok(found.some((f) => f.startsWith('구입목적 ') && f.includes(what)), `${text} → ${found.join(' | ')}`);
  }
});

test('notes are items, not sentences', () => {
  assert.deepEqual(checkNoteStyle(['2026-01-15 카드 승인 12,000원 한 줄', '배송비 없음', 'https://example.com/a.']), []);
  const found = checkNoteStyle(['두 건을 한 건으로 작성한다.', '확인했어요']);
  assert.equal(found.length, 2);
  assert.match(found[0], /기타 문장형/);
});
