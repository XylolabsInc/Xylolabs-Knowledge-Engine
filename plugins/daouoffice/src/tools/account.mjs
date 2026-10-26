import * as z from 'zod';

/**
 * Account tools.
 *
 * connect (hidden, role "connect")  The bot's connect form. Its input schema IS the form: the bot
 *   renders `title` as the label, `description` as the hint, `format: "password"` as a secret field
 *   and an optional boolean as a checkbox. It signs in to check the credentials and reads the
 *   person's name and department. It never kicks anyone: the duplicate-login modal only shows after
 *   a correct password, so seeing it is enough, and the plugin backs out.
 * login (hidden, role "login")      Signs in with `_meta["xylolabs/allowKick"]`; used after the person
 *   pressed "끊고 진행" on the duplicate-session prompt.
 * me                                The connected account and department.
 */

export function register(server, rt) {
  rt.tool(server, 'connect', {
    kind: 'read',
    role: 'connect',
    description: 'Checks DaouOffice credentials entered in the bot\'s connect form and stores the profile. Called by the bot, never by the model.',
    input: z.object({
      username: z.string().trim().min(1).max(128).meta({ title: '아이디', description: '다우오피스 로그인 아이디' }),
      password: z.string().min(1).max(256).meta({ title: '비밀번호', format: 'password', description: '다우오피스 비밀번호. 봇만 암호화해 보관합니다.' }),
      allowKick: z.boolean().optional().meta({
        title: '기존 접속을 끊어도 됩니다',
        description: '봇이 로그인할 때 다른 곳에 열려 있는 내 다우오피스 접속이 끊어질 수 있습니다. 허락하지 않으면 그때마다 묻습니다.',
      }),
    }),
  }, async (args, call) => {
    const v = await rt.sessions.verify(call.owner, call.creds);
    let account = rt.db.account(call.owner);
    if (!v.duplicate) {
      account = await rt.sessions.withSession(call.owner, call.creds, { allowKick: false },
        (s) => rt.refreshProfile(call.owner, s.api).catch((e) => {
          rt.logger.warn('profile read failed on connect', { owner: call.owner, error: e?.message });
          return rt.db.upsertAccount({ owner: call.owner, data: {} });
        }));
    } else if (!account) {
      account = rt.db.upsertAccount({ owner: call.owner, data: {} });
    }
    const accountName = rt.accountName(account, call.owner);
    return {
      text: v.duplicate
        ? `다우오피스 계정을 확인했습니다 (${accountName}). 지금 다른 곳에서 접속 중이라 그 접속은 끊지 않았습니다. 이름과 부서는 다음 로그인 때 읽습니다.`
        : `다우오피스 계정을 확인했습니다: ${accountName}`,
      data: { accountId: call.owner, accountName, duplicate: v.duplicate },
    };
  });

  rt.tool(server, 'login', {
    kind: 'read',
    role: 'login',
    description: 'Signs in to DaouOffice for the caller, ending another session only when _meta allowKick is true. Called by the bot after the person agreed, never by the model.',
    input: z.object({}),
  }, async (args, call) => {
    const account = await rt.session(call, async () => rt.db.account(call.owner));
    const accountName = rt.accountName(account, call.owner);
    return {
      text: `다우오피스에 로그인했습니다: ${accountName}`,
      data: { accountId: call.owner, accountName, duplicate: false },
    };
  });

  rt.tool(server, 'me', {
    kind: 'read',
    description: '연결된 다우오피스 계정(아이디, 이름, 부서, 부서 코드)을 보여 준다. 사람이 "내 계정", "연결된 계정", "내 부서" 를 물을 때 쓴다.',
    input: z.object({}),
  }, async (args, call) => {
    let account = rt.db.account(call.owner);
    if (!account?.name) account = await rt.session(call, (s) => rt.refreshProfile(call.owner, s.api));
    const lines = [
      `연결된 다우오피스 계정: ${call.owner}`,
      `- 이름: ${account?.name ?? '(모름)'}`,
      `- 부서: ${account?.deptName ?? '(모름)'}${account?.deptCode ? ` (코드 ${account.deptCode})` : ''}`,
    ];
    if (!account?.deptCode) lines.push('부서 코드를 조직도에서 읽지 못했습니다. 제목에 부서 코드가 들어가는 문서는 올리기 전에 확인이 필요합니다.');
    return {
      text: lines.join('\n'),
      data: {
        accountId: call.owner,
        name: account?.name ?? null,
        deptName: account?.deptName ?? null,
        deptCode: account?.deptCode ?? null,
      },
    };
  });
}
