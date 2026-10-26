import { dismissDialogs } from './dialogs.mjs';
import { PluginError } from '../errors.mjs';
import { requireExecute, markSent } from '../guard.mjs';

/**
 * 결재선 편집, 상신, 승인, 상신취소, 삭제, 문서 연결, 댓글. 원본 저장소 `approval.mjs` 의
 * DOM 흐름을 그대로 옮겼다. 바뀐 것은 실패를 `PluginError` 로 던지는 것, 실패 스크린샷
 * (`recon/`)을 남기지 않는 것, 서버를 바꾸는 동작(상신, 승인, 상신취소, 삭제, 댓글)이
 * 승인된 실행에서만 돌게 처음에 `requireExecute()` 로 막는 것, 되돌릴 수 없는 마지막 클릭 직전에
 * `markSent()` 를 부르는 것이다. 그 뒤의 실패만 결과를 모르는 것으로 보고된다. 확인 창을 여는 첫
 * 클릭, 문서 연결, 결재선 편집은 열려 있는 기안 창만 바꾸므로 쓰기로 세지 않는다.
 *
 * 결재선 편집기의 `확인` 은 화면 안에서만 유지된다. 편집과 상신을 같은 페이지 세션에서 끝낸다.
 */
const fail = (message) => new PluginError('UPSTREAM', message);
const DOC_ID = /^\d{6,25}$/;
const checkDocId = (docId) => {
  if (!DOC_ID.test(String(docId))) throw new PluginError('VALIDATION', `문서 ID 가 숫자가 아닙니다: ${docId}`);
};

/**
 * 결재 정보 편집기를 열어 조직도에서 사람을 찾아 결재선에 끌어 넣고 저장한다.
 *
 * 노드는 `draggable="true"` 지만 마우스 이벤트 드래그로 동작한다.
 * 드롭하면 "승인" 구획에 타입 `결재` 로 들어간다. 기본 결재선의
 * 다른 행(기안·확인)은 건드리지 않는다.
 */
export async function addApprover(page, name, type = '결재', dept) {
  return addApprovers(page, [{ name, type, dept }]);
}

/**
 * 여러 명을 **한 편집기 세션 안에서** 순서대로 결재선에 넣는다.
 *
 * 편집기를 사람마다 열고 닫으면 느리기도 하고, 넣은 순서가 결재 순서가
 * 되므로 한 번에 처리하는 편이 낫다.
 *
 *   addApprovers(page, [
 *     { name: '홍길동', dept: '경영지원팀', type: '합의' },
 *     { name: '김철수', type: '결재' },
 *   ]);
 *
 * **타입 선택지는 구획이 정한다.** 양식 기본 결재선에 `확인` 구획이 없으면
 * 드래그로는 `확인` 행을 만들 수 없고 `결재`/`합의` 만 고를 수 있다.
 * 구획 자체는 양식 설정이라 자동화로 못 만든다.
 */
export async function addApprovers(page, entries) {
  await openApprovalLineEditor(page);

  for (const { name, dept, type = '결재' } of entries) {
    await dropIntoApprovalLine(page, name, { dept });

    const line = await readApprovalLine(page);
    if (!line.includes(name)) {
      throw fail(`결재선에 "${name}" 이 추가되지 않았다: ${line}`);
    }
    await setApprovalLineType(page, name, type);
  }

  // 편집기를 닫으면 table.tb_approval_line 이 사라져 읽을 수 없다. 닫기 전에 읽는다.
  const saved = await readApprovalLine(page);
  await saveApprovalLineEditor(page);
  return saved;
}

/**
 * 결재선과 수신자를 **한 편집기 세션 안에서** 함께 설정한다.
 *
 *   editApprovalInfo(page, {
 *     approvers:  [{ name: '김철수', type: '결재' }],
 *     recipients: [{ name: '홍길동', dept: '경영지원팀' }],
 *   });
 *
 * 편집기의 `확인` 은 결재선·참조자·수신자·열람자를 한꺼번에 저장한다.
 * 따로 열고 닫으면 앞서 넣은 것이 날아간다.
 *
 * 결재선은 탭을 바꾸기 **전에** 읽는다. 수신자 표도 `table.tb_approval_line`
 * 이라 탭을 바꾼 뒤 읽으면 수신자를 결재선으로 착각한다.
 */
export async function editApprovalInfo(page, { approvers = [], recipients = [], referers = [] } = {}) {
  await openApprovalLineEditor(page);

  for (const { name, dept, type = '결재' } of approvers) {
    // 양식 기본 결재선에 이미 있으면 넣지 않는다. 법인카드 지출품의서 기본 결재선에
    // 대표가 들어와 있었는데, 그 위에 또 끌어 넣었더니 다음 빈 구획(수신)에 한 번 더
    // 들어간 적이 있다.
    if ((await approvalLineRows(page)).some((r) => r.name === name && r.kind !== '기안')) {
      await setApprovalLineType(page, name, type);
      continue;
    }
    await dropIntoApprovalLine(page, name, { dept });
    const line = await readApprovalLine(page);
    if (!line.includes(name)) {
      throw fail(`결재선에 "${name}" 이 추가되지 않았다: ${line}`);
    }
    await setApprovalLineType(page, name, type);
  }
  await removeDuplicateApprovers(page);
  const line = await readApprovalLine(page);

  let received = '(지정 안 함)';
  if (recipients.length) received = await addRecipients(page, recipients);
  let referred = '(지정 안 함)';
  if (referers.length) referred = await addRecipients(page, referers, 'referer');

  await saveApprovalLineEditor(page);
  return { line, recipients: received, referers: referred };
}

/**
 * 결재선을 기안자만 남기고 비운 뒤 `approvers` 를 순서대로 넣는다. 양식 기본 결재선에
 * 퇴사자나 다른 과제 담당자가 들어 있어도 남지 않는다(사업비 결재선 공지).
 * 편집기를 닫기 전에 이름, 타입, 순서가 정확히 같은지 확인하고 다르면 멈춘다.
 *
 *   setApprovalLine(page, [{ name: '홍길동', type: '합의' }, { name: '김철수', type: '결재' }]);
 */
export async function setApprovalLine(page, approvers) {
  await openApprovalLineEditor(page);
  for (let round = 0; round < 10; round++) {
    const other = (await approvalLineRows(page)).find((r) => r.kind !== '기안');
    if (!other) break;
    if (!(await removeFromApprovalLine(page, other.name))) throw fail(`결재선에서 "${other.name}" 을 빼지 못했다`);
  }
  for (const { name, dept, type = '결재' } of approvers) {
    await dropIntoApprovalLine(page, name, { dept });
    if (!(await readApprovalLine(page)).includes(name)) {
      throw fail(`결재선에 "${name}" 이 추가되지 않았다`);
    }
    await setApprovalLineType(page, name, type);
  }
  const rows = (await approvalLineRows(page)).filter((r) => r.kind !== '기안');
  const got = rows.map((r) => `${r.kind} ${r.name}`).join(' → ');
  const want = approvers.map((a) => `${a.type ?? '결재'} ${a.name}`).join(' → ');
  const line = await readApprovalLine(page);
  if (got !== want) throw fail(`결재선이 기대와 다르다: ${got} (기대 ${want})`);
  await saveApprovalLineEditor(page);
  return line;
}

/**
 * 결재선을 본인 단독(`기안 → 결재` 둘 다 나)으로 바꾼다.
 *
 * 셀프승인이 필요할 때 쓴다. 기본 결재선에 있던 사람은 전부 뺀다.
 * 편집기의 `확인` 은 **화면 안에서만 유지된다** — 페이지를 다시 열면
 * 기본 결재선으로 돌아간다. 편집과 상신을 같은 페이지 세션에서 끝내야 한다.
 */
export async function setSelfApprovalLine(page, me) {
  await openApprovalLineEditor(page);

  for (const who of await listApprovalLineNames(page)) {
    if (who === me) continue;
    await removeFromApprovalLine(page, who);
  }

  await dropIntoApprovalLine(page, me);
  await setApprovalLineType(page, me, '결재');

  const line = await readApprovalLine(page);
  await saveApprovalLineEditor(page);
  return line;
}

/**
 * 결재 정보 편집기의 상단 탭을 바꾼다. `apprline` · `referer` · `receiver` · `reader`.
 *
 * 탭마다 조직도와 대상자 표가 **따로** 있다. 탭을 바꾸면 이전 탭의 조직도는
 * 크기 0 이 되므로, `findPerson()` 의 "보이는 것만" 규칙이 그대로 먹는다.
 *
 * 탭 `li` 에는 id 가 없다. `data-tabid` 로 잡는다.
 */
export async function switchEditorTab(page, tabId) {
  const ok = await page.evaluate((id) => {
    const li = document.querySelector(`li[data-role=tab][data-tabid=${id}]`);
    if (!li) return false;
    li.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true, view: window }));
    return true;
  }, tabId);
  if (!ok) throw fail(`편집기 탭 "${tabId}" 을 찾지 못했다`);
  await page.waitForTimeout(3000);
}

/**
 * 수신자를 지정한다. 결재선과 **같은 편집기 세션 안에서** 처리해야 한다.
 *
 *   addRecipients(page, [{ name: '홍길동', dept: '경영지원팀' }]);
 *
 * 수신자 탭은 결재선 탭과 마크업이 다르다. 조직도는
 * `#aside-receiver-orgtree-tree`, 대상 패널은 `.tabview-receiver`,
 * 빈 드롭 영역은 `.empty-activity-group.empty_approval_line` 이다
 * (결재선 탭의 `.hidden_and_empty_group` 이 아니다).
 *
 * 수신자에는 결재 타입이 없다. 표 컬럼도 이름·부서뿐이다.
 */
export async function addRecipients(page, entries, tab = 'receiver') {
  await switchEditorTab(page, tab);

  for (const { name, dept } of entries) {
    // 상신취소로 돌아온 문서는 수신자가 이미 들어 있다. 다시 넣지 않는다.
    if ((await readRecipients(page, tab)).split(', ').some((r) => r.startsWith(name))) continue;
    await dropIntoReceiver(page, name, { dept, tab });
    const now = await readRecipients(page, tab);
    if (!now.includes(name)) {
      throw fail(`수신자에 "${name}" 이 추가되지 않았다: ${now}`);
    }
  }
  return readRecipients(page, tab);
}

/**
 * 수신자 탭의 조직도에서 사람을 찾아 수신자 표로 끌어다 놓는다. 참조자, 열람자 탭도 마크업이 같다
 * (`tab` 에 `referer`, `reader`). 출장 양식이 바뀌어 수신자 탭이 없어지고 참조자로 넣게 됐다.
 */
export async function dropIntoReceiver(page, name, { dept, tab = 'receiver' } = {}) {
  let source = await findPerson(page, name, dept);
  if (!source) {
    await expandOrganization(page);
    source = await findPerson(page, name, dept);
  }
  if (!source) throw fail(`수신자 조직도에서 "${name}"${dept ? ` (${dept})` : ''} 을 찾지 못했다`);
  if (source.ambiguous) {
    throw fail(`"${name}" 이 여러 부서에 있다: ${source.ambiguous.join(' / ')}. dept 로 좁힐 것.`);
  }
  await page.waitForTimeout(500);

  const target = await page.evaluate((t) => {
    const box = (el, fromTop) => {
      if (!el) return null;
      const r = el.getBoundingClientRect();
      if (r.width === 0 || r.height === 0) return null;
      return { x: r.x + r.width / 2, y: fromTop ? r.y + Math.max(r.height / 2, 10) : r.y + r.height / 2 };
    };
    const panel = document.querySelector(`.tabview-${t}`);
    if (!panel) return null;
    const hint = [...panel.querySelectorAll('tbody')].find((t) => /드래그하여/.test(t.innerText || ''));
    return box(hint, true)
      ?? box(panel.querySelector('.empty-activity-group.empty_approval_line'), false)
      ?? box(panel.querySelector('.activity-group-items.tb_approval_line_wrap'), false)
      ?? box(panel.querySelector('.activity_group.approval_line'), false);
  }, tab);
  if (!target) throw fail('수신자 드롭 영역을 찾지 못했다');

  await page.mouse.move(source.x, source.y);
  await page.mouse.down();
  for (let i = 1; i <= 20; i++) {
    await page.mouse.move(source.x + ((target.x - source.x) * i) / 20, source.y + ((target.y - source.y) * i) / 20);
    await page.waitForTimeout(40);
  }
  await page.mouse.up();
  await page.waitForTimeout(2500);
}

/**
 * 수신자 표를 읽는다. `홍길동(경영지원팀)`
 *
 * 수신자 표도 `table.tb_approval_line` 이라 결재선 표와 셀렉터가 겹친다.
 * `.tabview-receiver` 안으로 한정하지 않으면 결재선을 읽어 버린다.
 */
export async function readRecipients(page, tab = 'receiver') {
  return page.evaluate((t) => {
    const panel = document.querySelector(`.tabview-${t}`);
    if (!panel) return `(${t} 탭 없음)`;
    const rows = [];
    for (const tr of panel.querySelectorAll('table.tb_approval_line tbody tr')) {
      const cell = (sel) => tr.querySelector(sel)?.innerText?.replace(/\s+/g, ' ').trim() ?? '';
      const name = cell('td.name');
      if (!name) continue;
      const dept = cell('td.depart');
      rows.push(`${name}${dept ? `(${dept})` : ''}`);
    }
    return rows.join(', ') || '(비어 있음)';
  }, tab);
}

/** `결재 정보` 버튼을 눌러 편집기를 연다. */
export async function openApprovalLineEditor(page) {
  // 상신취소로 돌아온 문서에서는 안내 말풍선(`#popOver…`)이 버튼을 덮는다. 닫고 누른다.
  await page.keyboard.press('Escape').catch(() => {});
  await page.evaluate(() => document.querySelectorAll('[id^=popOv]').forEach((e) => e.remove())).catch(() => {});
  await page.locator('#act_edit_apprflow').first().click({ timeout: 20_000 });
  await page.waitForTimeout(5000);
}

/** 편집기의 `확인` 을 눌러 닫는다. */
export async function saveApprovalLineEditor(page) {
  const clicked = await page.evaluate(() => {
    const btn = [...document.querySelectorAll('.go_popup a, .go_popup button, [role=dialog] button')]
      .find((e) => (e.innerText || '').trim() === '확인' && e.offsetParent !== null);
    if (!btn) return false;
    btn.click();
    return true;
  });
  if (!clicked) throw fail('결재선 편집기의 확인 버튼을 찾지 못했다');
  await page.waitForTimeout(3000);
}

/** 편집기에 올라와 있는 사람 이름들. 기안자도 포함된다. */
export async function listApprovalLineNames(page) {
  return page.evaluate(() =>
    [...document.querySelectorAll('table.tb_approval_line tbody tr')]
      .map((tr) => (tr.innerText || '').replace(/\s+/g, ' ').trim())
      .filter((t) => t && !/드래그하여/.test(t))
      .map((t) => t.split(' ').find((w) => /^[가-힣]{2,4}$/.test(w) && !/기안|결재|합의|확인|예정|타입|이름|부서|상태/.test(w)))
      .filter(Boolean),
  );
}

/**
 * 결재선에서 한 명을 뺀다.
 *
 * 삭제 버튼은 `<a>` 가 아니라 `<span class="btn_bdr delete_activity">` 다.
 * jQuery 위임 핸들러라 실제 마우스 클릭으로 눌러야 한다.
 * 기안 행에는 삭제 버튼이 없다 — 기안자는 뺄 수 없다.
 */
export async function removeFromApprovalLine(page, name) {
  const box = await page.evaluate((who) => {
    const row = [...document.querySelectorAll('table.tb_approval_line tbody tr')]
      .find((tr) => (tr.innerText || '').includes(who));
    const del = row?.querySelector('span.delete_activity');
    if (!del) return null;
    const r = del.getBoundingClientRect();
    return { x: r.x + r.width / 2, y: r.y + r.height / 2 };
  }, name);
  if (!box) return false;
  await page.mouse.click(box.x, box.y);
  await page.waitForTimeout(1500);
  return true;
}

/**
 * 조직도의 접힌 부서를 펼친다. 하위 부서는 **지연 로딩**이라 펼치기 전에는
 * DOM 에 없다. 펼침 버튼은 `aria-hidden="true"` 라 Playwright 클릭이 안 먹고,
 * `dispatchEvent` 로 눌러야 한다.
 *
 * **보이는 것만 건드린다.** 편집기에는 결재선·참조자·수신자·열람자 탭마다
 * 조직도가 따로 있고, 안 보이는 탭의 노드는 크기가 0 이다. 그걸 잡으면
 * 클릭도 드래그도 조용히 실패한다.
 */
export async function expandOrganization(page, { rounds = 4 } = {}) {
  for (let i = 0; i < rounds; i++) {
    const opened = await page.evaluate(() => {
      let count = 0;
      for (const row of [...document.querySelectorAll('[data-dept-id]')]) {
        if (row.getBoundingClientRect().width === 0) continue;   // 안 보이는 탭
        if (row.closest('li')?.querySelector('ul')) continue;    // 이미 펼쳐짐
        const btn = row.querySelector('button');
        if (!btn) continue;
        btn.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true, view: window }));
        count++;
      }
      return count;
    });
    if (!opened) return;
    await page.waitForTimeout(3000);
  }
}

/**
 * 조직도에서 사람을 찾아 결재선 드롭 영역으로 끌어다 놓는다.
 *
 * 같은 이름이 여러 부서에 있을 수 있다(한 사람이 두 부서에 겸직으로 올라 있기도
 * 하다). 그럴 때는 `dept` 로 좁히지 않으면 거부한다.
 *
 * 드롭 영역은 두 가지 모습으로 나타난다.
 *
 *   1. 결재선에 사람이 있다가 빠진 자리 — "드래그하여 결재선을 추가할 수
 *      있습니다" 힌트가 든 `tbody`
 *   2. 처음부터 비어 있는 승인 구획 — `.activity_group.hidden_and_empty_group`.
 *      클래스 이름과 달리 실제로는 보이고 크기도 있다. 힌트 문구가 없다.
 *
 * 양식 기본 결재선이 바뀌면 둘 사이를 오간다. 힌트만 찾으면 깨진다.
 */
export async function dropIntoApprovalLine(page, name, { dept } = {}) {
  let source = await findPerson(page, name, dept);
  if (!source) {
    await expandOrganization(page);
    source = await findPerson(page, name, dept);
  }
  if (!source) throw fail(`조직도에서 "${name}"${dept ? ` (${dept})` : ''} 을 찾지 못했다`);
  if (source.ambiguous) {
    throw fail(`"${name}" 이 여러 부서에 있다: ${source.ambiguous.join(' / ')}. dept 로 좁힐 것.`);
  }
  // 스크롤로 화면에 들여놓지 않으면 좌표가 창 밖이라 드래그가 조용히 실패한다.
  await page.waitForTimeout(500);

  const target = await page.evaluate((t) => {
    const box = (el, fromTop) => {
      if (!el) return null;
      const r = el.getBoundingClientRect();
      if (r.width === 0 || r.height === 0) return null;
      return { x: r.x + r.width / 2, y: fromTop ? r.y + Math.max(r.height / 2, 10) : r.y + r.height / 2 };
    };
    const hint = [...document.querySelectorAll('tbody')]
      .find((t) => /드래그하여 결재선을 추가/.test(t.innerText || ''));
    const empty = document.querySelector('.activity_group.hidden_and_empty_group .empty_approval_line')
      ?? document.querySelector('.activity_group.hidden_and_empty_group');
    return box(hint, true) ?? box(empty, false);
  });
  if (!target) throw fail('결재선 드롭 영역을 찾지 못했다');

  await page.mouse.move(source.x, source.y);
  await page.mouse.down();
  for (let i = 1; i <= 20; i++) {
    await page.mouse.move(source.x + ((target.x - source.x) * i) / 20, source.y + ((target.y - source.y) * i) / 20);
    await page.waitForTimeout(40);
  }
  await page.mouse.up();
  await page.waitForTimeout(2500);
}

/**
 * 결재선 행의 타입을 맞춘다.
 *
 * 타입 `select` 의 선택지는 행마다 다르다 — 확인 행은 `결재/합의/확인`,
 * 결재 행은 `결재/합의` 다. 드롭하면 대개 `결재` 로 잡히지만 기본값에
 * 기대지 않고 확인하고 맞춘다.
 */
export async function setApprovalLineType(page, name, want) {
  const type = await page.evaluate(({ who, want }) => {
    const rows = [...document.querySelectorAll('table.tb_approval_line tbody tr')]
      .filter((tr) => (tr.innerText || '').includes(who));
    const select = rows[rows.length - 1]?.querySelector('select');
    if (!select) return null;
    if (select.options[select.selectedIndex]?.text !== want) {
      const option = [...select.options].find((o) => o.text === want);
      if (!option) return `옵션없음:${[...select.options].map((o) => o.text).join('/')}`;
      select.value = option.value;
      select.dispatchEvent(new Event('change', { bubbles: true }));
    }
    return select.options[select.selectedIndex]?.text;
  }, { who: name, want });
  if (type !== want) throw fail(`"${name}" 의 결재 타입을 ${want} 로 맞추지 못했다: ${type}`);
  return type;
}

/**
 * 편집기 안의 결재선을 읽는다. `기안 홍길동(연구개발팀) → 합의 김철수(경영지원팀)`
 *
 * 헤더·처리·승인이 각각 다른 table 로 쪼개져 있어 하나만 읽으면
 * 헤더("타입 이름 부서 상태")만 잡힌다. 전부 훑는다.
 *
 * 행의 innerText 를 그대로 쓰면 안 된다. 타입이 `select` 라 **선택지가 전부**
 * 찍혀서(`결재 합의`) 무엇이 골라졌는지 알 수 없다. 로그로 검증하려면
 * 선택된 option 만 봐야 한다.
 */
export async function readApprovalLine(page) {
  return page.evaluate(() => {
    const rows = [];
    for (const table of document.querySelectorAll('table.tb_approval_line')) {
      if (table.getBoundingClientRect().width === 0) continue;
      for (const tr of table.querySelectorAll('tbody tr')) {
        const cell = (sel) => tr.querySelector(sel)?.innerText?.replace(/\s+/g, ' ').trim() ?? '';
        const name = cell('td.name');
        if (!name) continue;
        const select = tr.querySelector('td.kind select, select');
        const kind = select ? (select.options[select.selectedIndex]?.text ?? '?') : cell('td.kind');
        const dept = cell('td.depart');
        const state = cell('td.state');
        rows.push(`${kind} ${name}${dept ? `(${dept})` : ''}${state ? ` [${state}]` : ''}`);
      }
    }
    return rows.join(' → ') || '(비어 있음)';
  });
}

/** 문서 화면(편집기 밖)에 표시된 결재선. 상신 직전 확인용이다. */
export async function readDocumentApprovalLine(page) {
  return page.evaluate(
    () => document.body.innerText.split('결재선')[1]?.split('결재요청')[0]?.replace(/\s+/g, ' ').trim() ?? '',
  );
}

/** `결재요청` 을 누르고 뒤따르는 확인 팝업까지 처리한다. */
export async function submitDocument(page) {
  requireExecute('결재요청');   // this click only opens the confirm popup
  const found = await page.evaluate(() => {
    const btn = [...document.querySelectorAll('a, button')]
      .find((e) => (e.innerText || '').trim() === '결재요청' && e.offsetParent !== null);
    if (!btn) return false;
    btn.click();
    return true;
  });
  if (!found) throw fail('결재요청 버튼을 찾지 못했다');
  await page.waitForTimeout(3000);

  const confirmed = await page.evaluate(() => {
    const btn = [...document.querySelectorAll('a.btn_major_s, .go_popup a, [role=dialog] button')]
      .find((e) => /^(확인|예|결재요청)$/.test((e.innerText || '').trim()) && e.offsetParent !== null);
    if (!btn) return null;
    window.__daoConfirm = btn;
    return btn.innerText.trim();
  });
  if (!confirmed) throw fail('상신 확인 팝업의 버튼을 찾지 못했다');
  markSent('결재요청');
  await page.evaluate(() => { window.__daoConfirm.click(); delete window.__daoConfirm; });
  await page.waitForTimeout(9000);
  return confirmed;
}

/**
 * 결재 대기 문서를 승인한다. 문서 화면의 `결재`(`a#act_approve`)를 누르고
 * 의견 팝업의 `승인`(`a.btn_major_s`)까지 처리한다.
 */
export async function approveDocument(page) {
  requireExecute('결재 승인');   // the first click opens the opinion popup
  await page.locator('a#act_approve').first().click({ timeout: 20_000 });
  await page.waitForTimeout(3000);

  const clicked = await page.evaluate(() => {
    const btn = [...document.querySelectorAll('a.btn_major_s, .go_popup a, [role=dialog] button')]
      .find((e) => /^(승인|확인|예)$/.test((e.innerText || '').trim()) && e.offsetParent !== null);
    if (!btn) return null;
    window.__daoConfirm = btn;
    return btn.innerText.trim();
  });
  if (!clicked) throw fail('결재 의견 팝업의 승인 버튼을 찾지 못했다');
  markSent('결재 승인');
  await page.evaluate(() => { window.__daoConfirm.click(); delete window.__daoConfirm; });
  await page.waitForTimeout(8000);
  return clicked;
}

/**
 * 조직도에서 사람 노드를 찾아 좌표를 준다. 못 찾으면 `null`,
 * 부서로 좁혀지지 않으면 `{ ambiguous: [...] }`.
 *
 * 찾는 김에 화면 안으로 스크롤한다 — 펼친 트리에서 아래쪽 노드는 창 밖에
 * 있어서, 좌표만 계산해 드래그하면 아무 일도 일어나지 않는다.
 */
async function findPerson(page, name, dept) {
  return page.evaluate(({ who, dept }) => {
    const cands = [...document.querySelectorAll('ul.group_organization li, ul.sub li')]
      .filter((li) => li.getBoundingClientRect().width > 0)          // 안 보이는 탭 제외
      .filter((li) => (li.innerText || '').includes(who))
      .filter((li) => !dept || (li.innerText || '').includes(dept))
      .sort((a, b) => a.innerText.length - b.innerText.length);
    if (!cands.length) return null;

    // 사람 노드는 부서 노드보다 짧다. 같은 길이가 둘이면 동명이인이다.
    if (cands.length > 1 && cands[0].innerText.length === cands[1].innerText.length) {
      return { ambiguous: cands.slice(0, 4).map((li) => (li.innerText || '').replace(/\s+/g, ' ').trim().slice(0, 60)) };
    }
    const el = cands[0];
    el.scrollIntoView({ block: 'center' });
    const r = el.getBoundingClientRect();
    return { x: r.x + r.width / 2, y: r.y + r.height / 2 };
  }, { who: name, dept });
}

/**
 * 상신한 문서를 상신취소한다. 결재자가 처리하기 전에만 된다.
 * 문서는 임시저장으로 돌아가고 같은 ID 로 고쳐 다시 올릴 수 있다.
 * 상태 문자열을 돌려준다. `임시저장` 이 아니면 실패다.
 */
export async function withdrawDocument(page, api, baseUrl, docId) {
  requireExecute('상신취소');
  checkDocId(docId);
  const status = async () => (await api(`/gw/api/approval/document/${docId}`)).data?.data?.document?.docStatusName ?? '';
  if ((await status()) === '임시저장') return '임시저장';
  // 확인 뒤에 브라우저 대화상자가 뜰 수 있다. 받아 주지 않으면 `Page.handleJavaScriptDialog` 로 멈춘다.
  const accept = (d) => { d.accept().catch(() => {}); };
  page.on('dialog', accept);
  try {
    return await withdrawOnPage(page, baseUrl, docId, status);
  } finally {
    page.off('dialog', accept);
  }
}

async function withdrawOnPage(page, baseUrl, docId, status) {
  await page.goto(`${baseUrl}/gw/app/approval/document/${docId}`, { waitUntil: 'domcontentloaded', timeout: 60_000 });
  await page.waitForLoadState('networkidle', { timeout: 30_000 }).catch(() => {});
  await page.waitForTimeout(3000);
  await dismissDialogs(page);

  const found = await page.evaluate(() => {
    const b = [...document.querySelectorAll('a')].find((e) => e.id === 'act_draft_withdrawal' && e.getBoundingClientRect().width > 0);
    if (!b) return false;
    b.click();
    return true;
  });
  if (!found) throw new PluginError('CONFLICT', '상신취소 버튼이 없습니다. 이미 결재가 진행됐을 수 있습니다.');
  await page.waitForFunction(() => {
    const el = document.querySelector('.go_popup.layer_approval');
    return el && el.getBoundingClientRect().width > 0;
  }, null, { timeout: 60_000, polling: 500 });
  markSent('상신취소');
  await page.click('.go_popup.layer_approval a.btn_major_s');
  // 원본은 8초를 기다렸다. 상태가 임시저장으로 바뀌는 것을 직접 본다.
  const until = Date.now() + 30_000;
  let now = await status();
  while (now !== '임시저장' && Date.now() < until) {
    await page.waitForTimeout(1500);
    now = await status();
  }
  return now;
}

/** 편집기의 결재선 행. `{ group, kind, name }` 배열. 구획은 `신청`·`승인`·`수신` 같은 것이다. */
export async function approvalLineRows(page) {
  return page.evaluate(() => {
    const out = [];
    for (const g of document.querySelectorAll('.activity_group')) {
      if (g.getBoundingClientRect().width === 0) continue;
      const group = (g.innerText || '').trim().split(/\s+/)[0] ?? '';
      for (const tr of g.querySelectorAll('table.tb_approval_line tbody tr')) {
        const name = tr.querySelector('td.name')?.innerText?.trim();
        if (!name) continue;
        const select = tr.querySelector('td.kind select, select');
        const kind = select ? (select.options[select.selectedIndex]?.text ?? '') : (tr.querySelector('td.kind')?.innerText?.trim() ?? '');
        out.push({ group, kind, name });
      }
    }
    return out;
  });
}

/**
 * 같은 사람이 결재선에 두 번 이상 있으면 뒤쪽 것을 지운다. 기안 행은 건드리지 않는다.
 * 한 사람이 두 번 결재하는 결재선은 의도한 적이 없다.
 */
export async function removeDuplicateApprovers(page) {
  for (let round = 0; round < 5; round++) {
    const box = await page.evaluate(() => {
      const seen = new Set();
      let last = null;
      for (const g of document.querySelectorAll('.activity_group')) {
        if (g.getBoundingClientRect().width === 0) continue;
        for (const tr of g.querySelectorAll('table.tb_approval_line tbody tr')) {
          const name = tr.querySelector('td.name')?.innerText?.trim();
          if (!name) continue;
          if (seen.has(name)) last = tr.querySelector('span.delete_activity');
          seen.add(name);
        }
      }
      if (!last) return null;
      last.scrollIntoView({ block: 'center' });
      const r = last.getBoundingClientRect();
      return { x: r.x + r.width / 2, y: r.y + r.height / 2 };
    });
    if (!box) return;
    await page.mouse.click(box.x, box.y);
    await page.waitForTimeout(1500);
  }
  throw fail('결재선의 중복 결재자를 지우지 못했다');
}

/**
 * 임시저장 문서를 삭제한다. **되돌릴 수 없다.** 사람이 지우라고 한 문서에만 쓴다.
 * 임시저장이 아니면(진행중, 완료) 거부한다. 삭제 뒤 문서를 다시 읽어 없어졌는지 본다.
 */
export async function deleteDraft(page, api, baseUrl, docId) {
  requireExecute('임시저장 문서 삭제');
  checkDocId(docId);
  const read = async () => (await api(`/gw/api/approval/document/${docId}`));
  const before = (await read()).data?.data?.document?.docStatusName ?? '';
  if (before !== '임시저장') throw fail(`${docId} 는 임시저장이 아니다 (${before || '없음'}). 지우지 않는다`);

  await page.goto(`${baseUrl}/gw/app/approval/document/${docId}`, { waitUntil: 'domcontentloaded', timeout: 60_000 });
  await page.waitForLoadState('networkidle', { timeout: 30_000 }).catch(() => {});
  await page.waitForTimeout(3000);
  await dismissDialogs(page);
  const found = await page.evaluate(() => {
    const b = [...document.querySelectorAll('a#act_delete')].find((e) => e.getBoundingClientRect().width > 0);
    if (!b) return false;
    b.click();
    return true;
  });
  if (!found) throw fail('삭제 버튼이 없다');
  // 확인 창을 특정해서 누른다. 보이는 아무 주 버튼이나 누르지 않는다.
  await page.waitForFunction(() => [...document.querySelectorAll('.go_popup')]
    .some((p) => p.getBoundingClientRect().width > 0 && /삭제/.test(p.innerText || '')), null, { timeout: 20_000, polling: 500 });
  markSent('임시저장 문서 삭제');
  await page.evaluate(() => {
    const pop = [...document.querySelectorAll('.go_popup')].find((p) => p.getBoundingClientRect().width > 0 && /삭제/.test(p.innerText || ''));
    pop.querySelector('a.btn_major_s').click();
  });
  await page.waitForTimeout(5000);
  const after = await read();
  return after.data?.data?.document?.docStatusName ?? `없음 (HTTP ${after.status})`;
}

/**
 * 기안 창의 `관련문서` 에 결재 문서를 연결한다(문서 연결). `docs` 는 `[{ id, docNum }]`.
 *
 * 회계가 지출결의서에 구매 품의서를 연결해 달라고 했다. 기안 창의
 * `문서 검색`(#addRelatedDocument) 이 여는 "결재 문서 첨부" 창은 **결재가 끝난 문서만**
 * 보여 준다(`doclist/draft/complete`). 진행중인 품의는 고를 수 없다. 문서번호로 검색해
 * 체크하고 확인을 누르면 `#refDocPart` 목록에 `li[data-id]` 로 들어가고, 저장이나 상신 때
 * 본문에 `references: [{ id }]` 로 실린다.
 *
 * 연결된 문서 ID 목록을 돌려준다. 이미 연결된 것은 건너뛴다.
 */
export async function linkDocuments(page, docs) {
  requireExecute('관련문서 연결');   // edits the open draft only; nothing is saved until the submit
  for (const d of docs) {
    if (await page.$(`#refDocPart li[data-id="${d.id}"]`)) continue;
    await page.click('#addRelatedDocument');
    await page.waitForSelector('#gpopupLayer.layer_doc_attach', { timeout: 30_000 });
    await page.selectOption('#referenceSearchType', 'docNum');
    await page.fill('#search_title', d.docNum);
    await page.click('#search_icon');
    const box = `#gpopupLayer input.doclist_item_checkbox[data-id="${d.id}"]`;
    const found = await page.waitForSelector(box, { timeout: 30_000 }).then(() => true).catch(() => false);
    if (!found) {
      await page.evaluate(() => document.querySelector('#gpopupLayer #go_popup_close_icon')?.click());
      throw fail(`연결할 문서가 고르는 창에 없다: ${d.docNum} (${d.id}). 결재가 끝난 문서만 고를 수 있다`);
    }
    await page.check(box);
    await page.evaluate(() => [...document.querySelectorAll('#gpopupLayer a, #gpopupLayer button')]
      .find((e) => (e.innerText || '').trim() === '확인')?.click());
    await page.waitForSelector(`#refDocPart li[data-id="${d.id}"]`, { timeout: 20_000 });
  }
  return page.$$eval('#refDocPart li[data-id]', (els) => els.map((e) => e.getAttribute('data-id')));
}

/**
 * 결재 문서에 댓글을 단다. 파일(사진)도 붙일 수 있다. 결재가 끝난 문서에도 달린다.
 *
 * 입력란 `textarea#formApprComment`, 첨부 `.compo_reply_create input[type=file]`(여러 장),
 * 등록 `span#apprComment`. 올라간 첨부는 `#commentAttachPart li` 로 나타난다. 등록 뒤
 * `GET …/comment` 로 새 댓글이 생겼는지 확인한다. 댓글은 결재선 사람에게 알림이 가니
 * 사람이 시킨 것만 단다.
 *
 * 새 댓글 객체를 돌려준다.
 */
export async function postComment(page, api, baseUrl, docId, text, files = [], { authorName } = {}) {
  requireExecute('결재 문서 댓글');   // the upload before the click posts nothing; markSent comes right before it
  checkDocId(docId);
  const list = async () => (await api(`/gw/api/approval/document/${docId}/comment`)).data?.data ?? [];
  const same = (c) => String(c.message ?? '').trim() === text.trim();
  // UNVERIFIED: shape not observed — the author field of a comment row; checked only when present.
  const byMe = (c) => {
    const who = c.writerName ?? c.writer?.name ?? c.userName ?? c.user?.name ?? c.author?.name ?? null;
    return !authorName || !who || who === authorName;
  };
  // 같은 글의 댓글이 이미 있으면(앞선 실행이 등록 뒤에 멈췄다) 다시 달지 않고 그 댓글을 돌려준다.
  const already = (await list()).find((c) => same(c) && byMe(c));
  if (already) return { ...already, existing: true };

  await page.goto(`${baseUrl}/gw/app/approval/document/${docId}`, { waitUntil: 'domcontentloaded', timeout: 60_000 });
  await page.waitForLoadState('networkidle', { timeout: 30_000 }).catch(() => {});
  await page.waitForTimeout(3000);
  await dismissDialogs(page);
  if (files.length) {
    await page.locator('.compo_reply_create input[type=file]').first().setInputFiles(files);
    await page.waitForFunction(
      (n) => document.querySelectorAll('#commentAttachPart li').length >= n
        && [...document.querySelectorAll('.compo_reply_create .progress')].every((p) => p.style.display === 'none'),
      files.length, { timeout: 120_000, polling: 1000 },
    ).catch(() => { throw fail(`댓글 첨부가 올라가지 않았다 (${files.length}장)`); });
  }
  await page.fill('#formApprComment', text);
  // 서버가 거절하면 상태 코드를 남긴다. 화면은 아무 말 없이 그대로다. 응답 본문은 남기지 않는다.
  const answer = page.waitForResponse((r) => r.request().method() === 'POST' && /\/comment/.test(r.url()), { timeout: 30_000 })
    .then((r) => ({ status: r.status() }))
    .catch(() => null);
  markSent('결재 문서 댓글');
  await page.evaluate(() => document.getElementById('apprComment')?.click());
  const res = await answer;
  if (res && res.status >= 400) throw fail(`댓글을 서버가 거절했다 (${docId}): HTTP ${res.status}`);
  // 글이 같은 댓글을 찾는다. 첫 댓글을 짐작으로 고르지 않는다.
  const until = Date.now() + 30_000;
  let made = (await list()).find((c) => same(c) && byMe(c));
  while (!made && Date.now() < until) {
    await page.waitForTimeout(2000);
    made = (await list()).find((c) => same(c) && byMe(c));
  }
  if (!made) throw fail(`댓글이 등록됐는지 확인하지 못했다 (${docId})`);
  // 첨부가 실제로 붙었는지 본다. 화면에 올라간 것만 보고 믿지 않는다.
  if ((made.attaches ?? []).length !== files.length) {
    throw fail(`댓글 첨부 수가 다르다: ${(made.attaches ?? []).length}/${files.length} (${docId})`);
  }
  return made;
}
