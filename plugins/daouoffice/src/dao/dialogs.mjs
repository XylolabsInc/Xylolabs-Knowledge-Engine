/**
 * 화면을 덮는 마케팅·온보딩 다이얼로그를 닫는다.
 *
 * 결재 화면 등에 "다우오피스 4.0 시작하기" 같은 팝업이 뜨면 오버레이가
 * 클릭을 가로채 뒤쪽 버튼을 누를 수 없다. 페이지를 이동한 뒤 조작하기
 * 전에 한 번 불러 준다. API 만 쓸 때는 필요 없다.
 */
export async function dismissDialogs(page, { rounds = 3 } = {}) {
  for (let i = 0; i < rounds; i++) {
    if (!(await page.locator('[role="dialog"]').count())) return;

    await page.keyboard.press('Escape').catch(() => {});
    await page.waitForTimeout(400);
    if (!(await page.locator('[role="dialog"]').count())) return;

    // 이름 있는 닫기 버튼
    for (const closer of [
      page.getByRole('button', { name: /닫기|Close/i }),
      page.getByRole('button', { name: '다시보지 않기' }),
      page.locator('[role="dialog"] button[aria-label*="close" i]'),
    ]) {
      const btn = closer.first();
      if ((await btn.count()) && (await btn.isVisible().catch(() => false))) {
        await btn.click({ timeout: 3000 }).catch(() => {});
        await page.waitForTimeout(500);
      }
    }
    if (!(await page.locator('[role="dialog"]').count())) return;

    // 마케팅 팝오버의 닫기는 접근성 이름이 없고 X 아이콘(svg.tabler-icon-x)만 들어 있다.
    const iconClose = page.locator('[role="dialog"] button:has(svg.tabler-icon-x)').first();
    if ((await iconClose.count()) && (await iconClose.isVisible().catch(() => false))) {
      await iconClose.click({ timeout: 3000 }).catch(() => {});
      await page.waitForTimeout(500);
    }
  }
}
