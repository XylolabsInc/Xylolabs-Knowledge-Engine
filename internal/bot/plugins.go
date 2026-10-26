package bot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xylolabsinc/xylolabs-kb/internal/plugin"
)

// Button action IDs and the form callback ID. They travel through the
// platform and come back as Action.ActionID and FormSubmission.CallbackID.
const (
	actionConnect     = "plugin_connect"      // Value: plugin name; opens the account form
	actionApprove     = "plugin_approve"      // Value: call action ID
	actionCancel      = "plugin_cancel"       // Value: action ID
	actionKick        = "plugin_kick"         // Value: login_kick action ID; signs in, then runs the message again
	actionKickApprove = "plugin_kick_approve" // Value: call action ID; signs in, then approves
	actionDisconnect  = "plugin_disconnect"   // Value: disconnect action ID; forgets the stored account
	formConnect       = "plugin_connect_form"
)

// ActionOpensForm reports whether a click is answered with a form. Discord
// must know before it acknowledges the click, because a modal has to be the
// first response.
func ActionOpensForm(actionID string) bool { return actionID == actionConnect }

const (
	// staleRunningAfter must exceed the registry's execute timeout (5 minutes).
	staleRunningAfter = 10 * time.Minute
	connectTimeout    = 3 * time.Minute
)

// approveWords and cancelWord are the thread replies that decide a pending
// action without the model.
var approveWords = map[string]bool{"승인": true, "상신": true, "진행": true, "ok": true, "OK": true}

const cancelWord = "취소"

const (
	textExpired       = "시간이 지나 취소했습니다. 다시 요청해 주세요."
	textSubmitting    = "상신 중…"
	textSubmitted     = "상신했습니다. 결과는 아래에 있습니다."
	textCancelled     = "취소했습니다."
	textNotOwner      = "이 요청은 요청한 사람만 승인하거나 취소할 수 있습니다."
	textPrivateFiles  = "개인정보가 있어 DM으로 보냈습니다"
	textConnectIntro  = "입력한 값은 암호화해 보관하고 대화에는 남기지 않습니다. 비밀번호는 이 창에만 넣어 주세요."
	textServerConfig  = "서버 설정 문제로 계정을 읽지 못했습니다. 관리자에게 알려 주세요."
	textRestarting    = "재시작 중입니다. 잠시 후 다시 눌러 주세요."
	textPausedRestart = "봇이 다시 시작하는 중이라 상신하지 않았습니다. 다시 시작한 뒤 이 스레드에 '승인'이라고 답하면 이어서 상신합니다."
	textPromptMissing = "확인이 필요한 요청이 있습니다. 아래 버튼으로 진행하거나 취소해 주세요."
)

// SetPlugins connects the plugin registry. Call it during wiring, before
// the bot handles messages.
func (b *Bot) SetPlugins(r *plugin.Registry) {
	b.plugins = r
}

// pluginInstructions is the system-prompt addition for registered plugins.
func (b *Bot) pluginInstructions() string {
	if b.plugins == nil || len(b.plugins.Plugins()) == 0 {
		return ""
	}
	s := b.plugins.Instructions()
	if s == "" {
		s = "Plugin tools that need approval only prepare a preview; nothing is submitted until the user presses the button the bot posts."
	}
	return "\n\n" + s
}

func (b *Bot) userKey(userID string) string { return b.platform.Name() + ":" + userID }

func (b *Bot) pluginTitle(name string) string {
	if b.plugins != nil {
		for _, p := range b.plugins.Plugins() {
			if p.Name() == name {
				return p.Title()
			}
		}
	}
	return name
}

func (b *Bot) accountPlugin(name string) plugin.Plugin {
	if b.plugins == nil {
		return nil
	}
	for _, p := range b.plugins.AccountPlugins() {
		if p.Name() == name {
			return p
		}
	}
	return nil
}

// afterReply posts the prompts plugins queued while answering, then sends
// the first-contact connect DM in the background so it never delays a reply.
func (b *Bot) afterReply(ctx context.Context, id plugin.Identity, prompts *plugin.Prompts) {
	ctx = context.WithoutCancel(ctx)
	for _, pr := range prompts.List() {
		b.postPluginPrompt(ctx, id, pr)
	}
	b.goTracked(func() { b.firstContact(ctx, id) })
}

func (b *Bot) postPluginPrompt(ctx context.Context, id plugin.Identity, pr plugin.Prompt) {
	switch pr.Kind {
	case plugin.PromptConnect:
		b.sendConnectPrompt(ctx, id.UserID, pr.Plugin, pr.Text)
	case plugin.PromptApproval:
		b.postFiles(ctx, id.Channel, id.ThreadID, id.UserID, pr.Files)
		b.postActionPrompt(ctx, id.Channel, id.ThreadID, pr.ActionID, pr.Text, []Button{
			{ActionID: actionApprove, Label: "상신", Value: pr.ActionID, Style: "primary"},
			{ActionID: actionCancel, Label: "취소", Value: pr.ActionID, Style: "danger"},
		})
	case plugin.PromptLoginKick:
		b.postActionPrompt(ctx, id.Channel, id.ThreadID, pr.ActionID, pr.Text, []Button{
			{ActionID: actionKick, Label: "끊고 진행", Value: pr.ActionID, Style: "primary"},
			{ActionID: actionCancel, Label: "취소", Value: pr.ActionID, Style: "danger"},
		})
	case plugin.PromptNotice:
		b.postFiles(ctx, id.Channel, id.ThreadID, id.UserID, pr.Files)
	case plugin.PromptDisconnect:
		b.postActionPrompt(ctx, id.Channel, id.ThreadID, pr.ActionID, pr.Text, []Button{
			{ActionID: actionDisconnect, Label: "연결 해제", Value: pr.ActionID, Style: "danger"},
			{ActionID: actionCancel, Label: "취소", Value: pr.ActionID},
		})
	}
}

// postActionPrompt posts buttons for an action and records where they are,
// so expiry and later decisions can update that message.
func (b *Bot) postActionPrompt(ctx context.Context, channel, threadID, actionID, text string, buttons []Button) {
	msgID, err := b.postPrompt(ctx, channel, threadID, text, buttons)
	if err != nil {
		b.logger.Error("failed to post plugin prompt", "platform", b.platform.Name(), "action", actionID, "error", err)
		return
	}
	if err := b.plugins.SetActionPrompt(ctx, actionID, channel, msgID); err != nil {
		b.logger.Warn("failed to record plugin prompt", "action", actionID, "error", err)
	}
}

// firstContact DMs a connect button for every account plugin the person has
// neither connected nor been asked about. Connect prompts only ever go by DM.
func (b *Bot) firstContact(ctx context.Context, id plugin.Identity) {
	prompts, err := b.plugins.NeedsConnectPrompt(ctx, id.Key())
	if err != nil {
		b.logger.Warn("first-contact check failed", "user", id.Key(), "error", err)
		return
	}
	for _, pr := range prompts {
		if !b.sendConnectPrompt(ctx, id.UserID, pr.Plugin, pr.Text) {
			continue
		}
		if err := b.plugins.MarkPrompted(ctx, id.Key(), pr.Plugin); err != nil {
			b.logger.Warn("failed to record first-contact prompt", "user", id.Key(), "plugin", pr.Plugin, "error", err)
		}
	}
}

// sendConnectPrompt DMs text with a connect button. It reports whether the DM went out.
func (b *Bot) sendConnectPrompt(ctx context.Context, userID, pluginName, text string) bool {
	ch, err := b.platform.OpenDM(ctx, userID)
	if err != nil {
		b.logger.Warn("failed to open DM for connect prompt", "user", userID, "plugin", pluginName, "error", err)
		return false
	}
	_, err = b.postPrompt(ctx, ch, "", text, []Button{
		{ActionID: actionConnect, Label: "연결하기", Value: pluginName, Style: "primary"},
	})
	if err != nil {
		b.logger.Warn("failed to post connect prompt", "user", userID, "plugin", pluginName, "error", err)
		return false
	}
	return true
}

// postFiles uploads plugin files to the thread. Private files go to the
// person by DM, with a note in the thread; in a DM conversation they stay there.
func (b *Bot) postFiles(ctx context.Context, channel, threadID, userID string, files []plugin.File) {
	if len(files) == 0 {
		return
	}
	var failed, private []string
	var dm string
	for _, f := range files {
		target, thread := channel, threadID
		if f.Private {
			if dm == "" {
				ch, err := b.platform.OpenDM(ctx, userID)
				if err != nil {
					b.logger.Warn("failed to open DM for a private file", "user", userID, "error", err)
					failed = append(failed, f.Name)
					continue
				}
				dm = ch
			}
			if dm != channel {
				target, thread = dm, ""
				private = append(private, f.Name)
			}
		}
		uploadCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		err := b.platform.UploadFile(uploadCtx, target, thread, f.Name, f.Data)
		cancel()
		if err != nil {
			b.logger.Warn("failed to upload plugin file", "platform", b.platform.Name(), "file", f.Name, "size_bytes", len(f.Data), "error", err)
			failed = append(failed, f.Name)
		}
	}
	if len(private) > 0 {
		b.say(ctx, channel, threadID, textPrivateFiles+": "+strings.Join(private, ", "))
	}
	if len(failed) > 0 {
		b.say(ctx, channel, threadID, b.uploadFailedMessage(failed))
	}
}

// postPrompt posts formatted text with buttons. It never posts an empty body.
func (b *Bot) postPrompt(ctx context.Context, channel, threadID, text string, buttons []Button) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		text = textPromptMissing
	}
	return b.platform.PostPrompt(ctx, channel, threadID, Prompt{Text: b.platform.FormatResponse(text), Buttons: buttons})
}

// say posts a plain message, skipping an empty one.
func (b *Bot) say(ctx context.Context, channel, threadID, text string) {
	text = strings.TrimSpace(text)
	if text == "" || channel == "" {
		return
	}
	if err := b.platform.PostReply(ctx, channel, threadID, b.platform.FormatResponse(text)); err != nil {
		b.logger.Warn("failed to post plugin message", "platform", b.platform.Name(), "channel", channel, "error", err)
	}
}

// dm sends a direct message to a person.
func (b *Bot) dm(ctx context.Context, userID, text string) {
	ch, err := b.platform.OpenDM(ctx, userID)
	if err != nil {
		b.logger.Warn("failed to open DM", "user", userID, "error", err)
		return
	}
	b.say(ctx, ch, "", text)
}

// promptRef is the prompt message of an action and the thread it belongs to.
type promptRef struct {
	b                 *Bot
	channel, message  string // the prompt message; empty when unknown
	thread, threadsCh string // the conversation the action came from
}

func (b *Bot) refFor(a *Action, act *plugin.Action) promptRef {
	ref := promptRef{b: b, channel: a.Channel, message: a.MessageID, threadsCh: act.Origin.Channel, thread: act.Origin.ThreadID}
	if ref.message == "" {
		ref.channel, ref.message = act.PromptChannel, act.PromptMessage
	}
	return ref
}

// progress shows a passing state on the prompt, if there is one.
func (r promptRef) progress(ctx context.Context, preview, status string) {
	if r.message == "" {
		return
	}
	if err := r.b.platform.UpdatePrompt(ctx, r.channel, r.message, r.b.platform.FormatResponse(withStatus(preview, status))); err != nil {
		r.b.logger.Warn("failed to update plugin prompt", "error", err)
	}
}

// final shows the outcome on the prompt, or in the thread when the prompt
// message is unknown (a text reply decided an action whose buttons never posted).
func (r promptRef) final(ctx context.Context, preview, status string) {
	if r.message == "" {
		r.b.say(ctx, r.threadsCh, r.thread, status)
		return
	}
	r.progress(ctx, preview, status)
}

func withStatus(preview, status string) string {
	preview = strings.TrimSpace(preview)
	if preview == "" {
		return status
	}
	return preview + "\n\n" + status
}

// HandleAction handles a button click. It returns at once; the work runs in
// the background, and Shutdown waits for it, so a deploy does not cut an
// approved submission short.
func (b *Bot) HandleAction(ctx context.Context, a *Action) {
	if b.plugins == nil || a == nil || a.UserID == "" {
		return
	}
	if !b.goTracked(func() { b.handleAction(ctx, a) }) {
		b.logger.Warn("button click ignored: shutting down", "action", a.ActionID, "user", a.UserID)
		b.noticeWhileClosing(ctx, a.UserID, textRestarting)
	}
}

// noticeWhileClosing tells someone their request was not taken because the
// bot is restarting. It runs untracked with a short deadline: shutdown does
// not wait for it.
func (b *Bot) noticeWhileClosing(ctx context.Context, userID, text string) {
	go func() {
		nctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		b.dm(nctx, userID, text)
	}()
}

func (b *Bot) handleAction(ctx context.Context, a *Action) {
	defer func() {
		if r := recover(); r != nil {
			b.logger.Error("panic recovered in HandleAction", "action", a.ActionID, "panic", r)
		}
	}()
	ctx = context.WithoutCancel(ctx)
	switch a.ActionID {
	case actionConnect:
		b.openConnectForm(ctx, a)
	case actionApprove:
		b.approve(ctx, a, false)
	case actionKickApprove:
		b.approve(ctx, a, true)
	case actionCancel:
		b.cancelAction(ctx, a)
	case actionKick:
		b.resumeAfterKick(ctx, a)
	case actionDisconnect:
		b.confirmDisconnect(ctx, a)
	default:
		b.logger.Debug("unknown button", "action", a.ActionID)
	}
}

// owned loads the clicked action and checks that the clicker owns it. A
// stranger gets a DM and nothing else happens.
func (b *Bot) owned(ctx context.Context, a *Action) (*plugin.Action, bool) {
	act, err := b.plugins.Action(ctx, a.Value)
	if err != nil {
		if errors.Is(err, plugin.ErrActionNotFound) {
			b.dm(ctx, a.UserID, "이미 처리되었거나 없는 요청입니다.")
		} else {
			b.logger.Warn("failed to load plugin action", "action", a.Value, "error", err)
		}
		return nil, false
	}
	if act.UserKey != b.userKey(a.UserID) {
		b.logger.Warn("plugin action click refused: not the owner", "action", act.ID, "by", b.userKey(a.UserID))
		b.dm(ctx, a.UserID, textNotOwner)
		return nil, false
	}
	return act, true
}

var statusLabels = map[plugin.ActionStatus]string{
	plugin.StatusPending:   "대기 중",
	plugin.StatusRunning:   "처리 중",
	plugin.StatusDone:      "완료",
	plugin.StatusFailed:    "실패",
	plugin.StatusCancelled: "취소됨",
	plugin.StatusExpired:   "만료됨",
}

func settledText(status plugin.ActionStatus) string {
	label := statusLabels[status]
	if label == "" {
		label = string(status)
	}
	return fmt.Sprintf("이미 처리된 요청입니다 (상태: %s).", label)
}

// settled reports whether a clicked action is no longer waiting, and tells
// the clicker so, unless this bot is still working on it: a second click
// while the first runs stays silent, the first one shows the outcome.
func (b *Bot) settled(ctx context.Context, userID string, act *plugin.Action) bool {
	if act.Status == plugin.StatusPending {
		return false
	}
	if _, busy := b.inflight.Load(act.ID); !busy {
		b.dm(ctx, userID, settledText(act.Status))
	}
	return true
}

// raced tells the clicker the current status of an action that something
// else (expiry, another decision) settled while this bot held it.
func (b *Bot) raced(ctx context.Context, userID, actionID string) {
	if act, err := b.plugins.Action(ctx, actionID); err == nil {
		b.dm(ctx, userID, settledText(act.Status))
	}
}

// begin marks an action as being handled by this bot, so a second click
// while the first is still working does nothing. The registry's atomic
// status change is what guarantees a single execute; this keeps the prompt
// from flickering back to "상신 중…" after the outcome is shown.
func (b *Bot) begin(actionID string) bool {
	_, busy := b.inflight.LoadOrStore(actionID, struct{}{})
	return !busy
}

func (b *Bot) end(actionID string) { b.inflight.Delete(actionID) }

// approve submits a pending call action. With kick it first signs in,
// ending the person's other session.
func (b *Bot) approve(ctx context.Context, a *Action, kick bool) {
	act, ok := b.owned(ctx, a)
	if !ok || act.Kind != plugin.ActionCall || b.settled(ctx, a.UserID, act) {
		return
	}
	if !b.begin(act.ID) {
		return
	}
	defer b.end(act.ID)

	ref := b.refFor(a, act)
	title := b.pluginTitle(act.Plugin)
	by := plugin.Identity{
		Platform: b.platform.Name(), UserID: a.UserID, UserName: act.Origin.UserName,
		Channel: act.Origin.Channel, ThreadID: act.Origin.ThreadID, IsDM: act.Origin.IsDM,
	}

	if kick {
		ref.progress(ctx, act.Preview, "다른 접속을 끊고 로그인하는 중…")
		if _, _, err := b.plugins.LoginAndResume(ctx, act.ID, by); err != nil {
			b.decisionFailed(ctx, ref, act, a.UserID, title, err)
			return
		}
	}

	// Shutdown waits only for a submission already running. Do not start one
	// now (a kick-approve may have spent minutes signing in): the action stays
	// pending, and "승인" in the thread picks it up after the restart.
	if b.isClosing() {
		b.logger.Info("plugin submission not started: shutting down", "action", act.ID)
		ref.final(ctx, act.Preview, textPausedRestart)
		b.dm(ctx, a.UserID, textPausedRestart)
		return
	}

	ref.progress(ctx, act.Preview, textSubmitting)
	res, _, err := b.plugins.Approve(ctx, act.ID, by)
	if err != nil {
		b.decisionFailed(ctx, ref, act, a.UserID, title, err)
		return
	}
	text := strings.TrimSpace(res.Text)
	if text == "" {
		text = "처리했습니다."
	}
	b.say(ctx, ref.threadsCh, ref.thread, text)
	b.postFiles(ctx, ref.threadsCh, ref.thread, a.UserID, res.Files)
	ref.progress(ctx, act.Preview, textSubmitted)
}

// decisionFailed shows why approving (or signing in for) an action failed.
func (b *Bot) decisionFailed(ctx context.Context, ref promptRef, act *plugin.Action, userID, title string, err error) {
	b.logger.Warn("plugin action not completed", "action", act.ID, "plugin", act.Plugin, "tool", act.Tool, "error", err)
	switch {
	case errors.Is(err, plugin.ErrNotPending):
		// Something else settled it while this bot held it (expiry, a reply).
		b.raced(ctx, userID, act.ID)
	case errors.Is(err, plugin.ErrExpired):
		ref.final(ctx, act.Preview, textExpired)
	case errors.Is(err, plugin.ErrDuplicateSession):
		ref.final(ctx, act.Preview, fmt.Sprintf("다른 곳에서 %s에 접속 중이라 상신하지 못했습니다.", title))
		b.postActionPrompt(ctx, ref.threadsCh, ref.thread, act.ID,
			fmt.Sprintf("다른 곳에서 %s에 접속 중입니다. 봇이 로그인하면 그 접속이 끊깁니다. 끊고 상신할까요?", title),
			[]Button{
				{ActionID: actionKickApprove, Label: "끊고 상신", Value: act.ID, Style: "primary"},
				{ActionID: actionCancel, Label: "취소", Value: act.ID, Style: "danger"},
			})
	case errors.Is(err, plugin.ErrVaultOpen):
		ref.final(ctx, act.Preview, textServerConfig)
	case errors.Is(err, plugin.ErrAuthFailed), errors.Is(err, plugin.ErrNotConnected):
		ref.final(ctx, act.Preview, fmt.Sprintf("%s에 로그인하지 못해 상신하지 못했습니다. DM으로 보낸 버튼으로 계정을 다시 연결한 뒤, 이 스레드에 '승인'이라고 답하면 이어서 상신합니다.", title))
		b.sendConnectPrompt(ctx, userID, act.Plugin, fmt.Sprintf("저장된 %s 비밀번호로 로그인하지 못했습니다. 비밀번호를 바꿨다면 다시 연결해 주세요.", title))
	default:
		// Without a plugin error, or with one raised after the plugin started
		// changing the other system, nobody knows whether it went through.
		msg := fmt.Sprintf("결과를 확인하지 못했습니다. %s 문서함에서 상신됐는지 직접 확인해 주세요.", title)
		var pe *plugin.Error
		if errors.As(err, &pe) && pe.Message != "" {
			if pe.OutcomeUnknown {
				msg = fmt.Sprintf("처리 중에 문제가 생겨 결과를 확인하지 못했습니다: %s %s 문서함에서 상신됐는지 직접 확인해 주세요.", pe.Message, title)
			} else {
				msg = "상신하지 못했습니다: " + pe.Message
			}
		}
		ref.progress(ctx, act.Preview, msg)
		b.say(ctx, ref.threadsCh, ref.thread, msg)
	}
}

// cancelAction cancels a pending action.
func (b *Bot) cancelAction(ctx context.Context, a *Action) {
	act, ok := b.owned(ctx, a)
	if !ok || b.settled(ctx, a.UserID, act) {
		return
	}
	if !b.begin(act.ID) {
		return
	}
	defer b.end(act.ID)
	by := plugin.Identity{Platform: b.platform.Name(), UserID: a.UserID}
	if _, err := b.plugins.Cancel(ctx, act.ID, by); err != nil {
		if errors.Is(err, plugin.ErrNotPending) {
			b.raced(ctx, a.UserID, act.ID)
		} else {
			b.logger.Warn("failed to cancel plugin action", "action", act.ID, "error", err)
		}
		return
	}
	b.refFor(a, act).final(ctx, act.Preview, textCancelled)
}

// resumeAfterKick signs in for a login_kick action, ending the person's
// other session, then runs the stored message again as that person.
func (b *Bot) resumeAfterKick(ctx context.Context, a *Action) {
	act, ok := b.owned(ctx, a)
	if !ok || act.Kind != plugin.ActionLoginKick || b.settled(ctx, a.UserID, act) {
		return
	}
	if !b.begin(act.ID) {
		return
	}
	defer b.end(act.ID)

	ref := b.refFor(a, act)
	title := b.pluginTitle(act.Plugin)
	by := plugin.Identity{Platform: b.platform.Name(), UserID: a.UserID, UserName: act.Origin.UserName}
	ref.progress(ctx, "", "다른 접속을 끊고 로그인하는 중…")
	_, files, err := b.plugins.LoginAndResume(ctx, act.ID, by)
	switch {
	case err == nil:
	case errors.Is(err, plugin.ErrNotPending):
		b.raced(ctx, a.UserID, act.ID)
		return
	case errors.Is(err, plugin.ErrExpired):
		ref.final(ctx, "", textExpired)
		return
	case errors.Is(err, plugin.ErrVaultOpen):
		ref.final(ctx, "", textServerConfig)
		return
	case errors.Is(err, plugin.ErrAuthFailed):
		ref.final(ctx, "", fmt.Sprintf("%s에 로그인하지 못했습니다. 비밀번호를 바꿨다면 DM으로 보낸 버튼으로 다시 연결해 주세요.", title))
		b.sendConnectPrompt(ctx, a.UserID, act.Plugin, fmt.Sprintf("저장된 %s 비밀번호로 로그인하지 못했습니다. 비밀번호를 바꿨다면 다시 연결해 주세요.", title))
		return
	default:
		b.logger.Warn("login with kick failed", "action", act.ID, "error", err)
		msg := fmt.Sprintf("%s에 로그인하지 못했습니다. 잠시 후 다시 요청해 주세요.", title)
		var pe *plugin.Error
		if errors.As(err, &pe) && pe.Message != "" {
			msg = fmt.Sprintf("%s에 로그인하지 못했습니다: %s", title, pe.Message)
		}
		ref.final(ctx, "", msg)
		return
	}
	ref.final(ctx, "", "로그인했습니다. 요청을 이어서 처리합니다.")

	o := act.Origin
	if strings.TrimSpace(o.Text) == "" {
		b.say(ctx, o.Channel, o.ThreadID, "처음 요청 내용을 찾지 못했습니다. 다시 요청해 주세요.")
		return
	}
	msg := &IncomingMessage{
		Platform: b.platform.Name(), Channel: o.Channel, ThreadID: o.ThreadID, MessageID: o.MessageID,
		UserID: a.UserID, Text: o.Text, IsDM: o.IsDM, preloaded: files, rerun: true,
	}
	if o.ThreadID == o.MessageID {
		msg.ThreadID = "" // the request opened its own thread
	}
	b.respond(ctx, msg, o.Text, false)
}

// confirmDisconnect forgets the owner's account after they pressed the
// button; the model can only ask for it.
func (b *Bot) confirmDisconnect(ctx context.Context, a *Action) {
	act, ok := b.owned(ctx, a)
	if !ok || act.Kind != plugin.ActionDisconnect || b.settled(ctx, a.UserID, act) {
		return
	}
	if !b.begin(act.ID) {
		return
	}
	defer b.end(act.ID)
	ref := b.refFor(a, act)
	ref.progress(ctx, act.Preview, "연결을 해제하는 중…")
	res, _, err := b.plugins.Approve(ctx, act.ID, plugin.Identity{Platform: b.platform.Name(), UserID: a.UserID})
	switch {
	case err == nil:
		ref.final(ctx, "", res.Text)
	case errors.Is(err, plugin.ErrNotPending):
		b.raced(ctx, a.UserID, act.ID)
	case errors.Is(err, plugin.ErrExpired):
		ref.final(ctx, act.Preview, textExpired)
	default:
		b.logger.Warn("plugin disconnect failed", "action", act.ID, "error", err)
		ref.final(ctx, act.Preview, "연결을 해제하지 못했습니다. 잠시 후 다시 시도해 주세요.")
	}
}

// openConnectForm opens the account form of the clicked plugin.
func (b *Bot) openConnectForm(ctx context.Context, a *Action) {
	p := b.accountPlugin(a.Value)
	if p == nil {
		b.dm(ctx, a.UserID, "연결할 수 없는 시스템입니다.")
		return
	}
	form := Form{
		CallbackID: formConnect,
		Title:      p.Title() + " 연결",
		Submit:     "연결",
		Intro:      textConnectIntro,
		Metadata:   p.Name(),
	}
	for _, f := range p.AccountFields() {
		label := f.Label
		if f.Secret {
			label += " (앱 비밀번호 권장)"
		}
		form.Fields = append(form.Fields, FormField{
			ID: f.Name, Label: label, Hint: f.Hint, Secret: f.Secret, Optional: f.Optional, Checkbox: f.Checkbox,
		})
	}
	if err := b.platform.OpenForm(ctx, a.Trigger, form); err != nil {
		b.logger.Warn("failed to open connect form", "plugin", p.Name(), "user", a.UserID, "error", err)
		b.dm(ctx, a.UserID, "연결 창을 열지 못했습니다. 버튼을 다시 눌러 주세요.")
	}
}

// HandleFormSubmission handles a submitted account form. Like HandleAction
// it returns at once and the check runs in the background. The submitted
// values are passed to the registry and never logged.
func (b *Bot) HandleFormSubmission(ctx context.Context, f *FormSubmission) {
	if b.plugins == nil || f == nil || f.CallbackID != formConnect || f.UserID == "" {
		return
	}
	if !b.goTracked(func() { b.handleFormSubmission(ctx, f) }) {
		b.logger.Warn("form submission ignored: shutting down", "user", f.UserID)
		b.noticeWhileClosing(ctx, f.UserID, "재시작 중이라 계정을 확인하지 못했습니다. 잠시 후 다시 연결해 주세요.")
	}
}

func (b *Bot) handleFormSubmission(ctx context.Context, f *FormSubmission) {
	defer func() {
		if r := recover(); r != nil {
			b.logger.Error("panic recovered in HandleFormSubmission", "panic", r)
		}
	}()
	ctx = context.WithoutCancel(ctx)
	p := b.accountPlugin(f.Metadata)
	if p == nil {
		b.dm(ctx, f.UserID, "연결할 수 없는 시스템입니다.")
		return
	}
	title := p.Title()
	values := make(map[string]string, len(f.Values))
	for k, v := range f.Values {
		values[k] = v
	}
	for _, field := range p.AccountFields() {
		if field.Checkbox {
			values[field.Name] = checkboxValue(values[field.Name])
		}
	}

	b.dm(ctx, f.UserID, fmt.Sprintf("%s 계정을 확인하는 중입니다…", title))
	id := plugin.Identity{Platform: b.platform.Name(), UserID: f.UserID, UserName: b.platform.ResolveUserName(ctx, f.UserID)}
	cctx, cancel := context.WithTimeout(ctx, connectTimeout)
	name, err := b.plugins.Connect(cctx, id, p.Name(), values)
	cancel()
	clear(values)
	if err == nil {
		b.logger.Info("plugin account connected via form", "plugin", p.Name(), "user", id.Key())
		b.dm(ctx, f.UserID, "연결했습니다: "+name)
		return
	}

	var pe *plugin.Error
	errors.As(err, &pe)
	code := ""
	if pe != nil {
		code = pe.Code
	}
	b.logger.Warn("plugin account connect failed", "plugin", p.Name(), "user", id.Key(), "code", code)
	switch {
	case errors.Is(err, plugin.ErrAuthFailed):
		b.sendConnectPrompt(ctx, f.UserID, p.Name(),
			"아이디나 비밀번호가 맞지 않습니다. 계정이 잠기지 않도록 다시 시도하지 않았습니다. 확인한 뒤 버튼을 눌러 다시 입력해 주세요.")
	case pe != nil && pe.Code == plugin.CodeValidation:
		b.sendConnectPrompt(ctx, f.UserID, p.Name(), pe.Message+" 버튼을 눌러 다시 입력해 주세요.")
	case pe != nil && pe.Code == plugin.CodeRateLimited:
		b.dm(ctx, f.UserID, "로그인 시도가 너무 잦습니다. 잠시 후 다시 시도해 주세요.")
	case errors.Is(err, plugin.ErrNoVault):
		b.dm(ctx, f.UserID, "계정 저장소가 설정되지 않아 연결할 수 없습니다. 관리자에게 알려 주세요.")
	case pe != nil && pe.Message != "":
		b.dm(ctx, f.UserID, "연결하지 못했습니다: "+pe.Message)
	default:
		b.dm(ctx, f.UserID, fmt.Sprintf("%s에 접속하지 못해 연결하지 못했습니다. 잠시 후 다시 시도해 주세요.", title))
	}
}

// checkboxValue normalizes what a platform sends for a checkbox.
func checkboxValue(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "on", "yes", "y", "예", "네", "ㅇ":
		return "true"
	}
	return ""
}

// handleTextDecision approves or cancels the sender's pending action in this
// thread when the message is exactly one of the decision words, without
// calling the model. It reports whether it handled the message.
func (b *Bot) handleTextDecision(ctx context.Context, msg *IncomingMessage, threadID, text string) bool {
	if b.plugins == nil {
		return false
	}
	word := strings.TrimSpace(text)
	approve, cancel := approveWords[word], word == cancelWord
	if !approve && !cancel {
		return false
	}
	by := plugin.Identity{Platform: b.platform.Name(), UserID: msg.UserID, Channel: msg.Channel, ThreadID: threadID}
	act, err := b.plugins.PendingInThread(ctx, by)
	if err != nil {
		b.logger.Warn("pending action lookup failed", "user", by.Key(), "error", err)
		return false
	}
	if act == nil || (approve && act.Kind != plugin.ActionCall) {
		return false
	}
	click := &Action{Platform: b.platform.Name(), UserID: msg.UserID, Channel: act.PromptChannel, MessageID: act.PromptMessage, Value: act.ID}
	b.logger.Info("plugin action decided by text reply", "action", act.ID, "approve", approve)
	ctx = context.WithoutCancel(ctx)
	decide := func() { b.cancelAction(ctx, click) }
	if approve {
		decide = func() { b.approve(ctx, click, false) }
	}
	if !b.goTracked(decide) {
		b.logger.Warn("text decision ignored: shutting down", "action", act.ID)
		b.say(ctx, msg.Channel, threadID, "재시작 중입니다. 잠시 후 다시 보내 주세요.")
	}
	return true
}

// RunPluginJanitor marks stale running actions failed and expires overdue
// pending ones, now and then every interval until done closes, and updates
// their prompts through the bot of their platform.
func RunPluginJanitor(done <-chan struct{}, reg *plugin.Registry, interval time.Duration, logger *slog.Logger, bots ...*Bot) {
	tick := func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		stale, err := reg.RecoverStale(ctx, staleRunningAfter)
		if err != nil {
			logger.Warn("plugin janitor: recover stale actions", "error", err)
		}
		expired, err := reg.Expire(ctx)
		if err != nil {
			logger.Warn("plugin janitor: expire actions", "error", err)
		}
		covered := map[string]bool{}
		for _, b := range bots {
			if b == nil {
				continue
			}
			covered[b.platform.Name()] = true
			b.notifyStale(ctx, stale)
			b.notifyExpired(ctx, expired)
		}
		// Rows still change (an expired action is expired either way), but
		// nobody on a platform without a running bot hears about it.
		for _, group := range []struct {
			what string
			acts []plugin.Action
		}{{"stale", stale}, {"expired", expired}} {
			for _, act := range group.acts {
				platform, _, _ := strings.Cut(act.UserKey, ":")
				if !covered[platform] {
					logger.Warn("plugin janitor: no bot for this platform; owner not notified",
						"kind", group.what, "action", act.ID, "platform", platform, "plugin", act.Plugin)
				}
			}
		}
	}
	tick()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			tick()
		}
	}
}

// ownUserID returns the user ID of a key on this bot's platform.
func (b *Bot) ownUserID(userKey string) (string, bool) {
	return strings.CutPrefix(userKey, b.platform.Name()+":")
}

func (b *Bot) notifyExpired(ctx context.Context, acts []plugin.Action) {
	for i := range acts {
		act := &acts[i]
		if _, ok := b.ownUserID(act.UserKey); !ok || act.PromptMessage == "" {
			continue
		}
		if err := b.platform.UpdatePrompt(ctx, act.PromptChannel, act.PromptMessage, b.platform.FormatResponse(withStatus(act.Preview, textExpired))); err != nil {
			b.logger.Warn("failed to update expired plugin prompt", "action", act.ID, "error", err)
		}
	}
}

func (b *Bot) notifyStale(ctx context.Context, acts []plugin.Action) {
	for i := range acts {
		act := &acts[i]
		userID, ok := b.ownUserID(act.UserKey)
		if !ok {
			continue
		}
		title := b.pluginTitle(act.Plugin)
		msg := fmt.Sprintf("처리 중이던 요청의 결과를 확인하지 못했습니다. %s 문서함에서 상신됐는지 직접 확인해 주세요.", title)
		if act.PromptMessage != "" {
			if err := b.platform.UpdatePrompt(ctx, act.PromptChannel, act.PromptMessage, b.platform.FormatResponse(withStatus(act.Preview, msg))); err != nil {
				b.logger.Warn("failed to update stale plugin prompt", "action", act.ID, "error", err)
			}
		}
		if first := firstLine(act.Preview); first != "" {
			msg += "\n\n요청: " + first
		}
		b.dm(ctx, userID, msg)
	}
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return truncateRunes(strings.TrimSpace(s), 200)
}

// truncateRunes cuts s to at most limit runes.
func truncateRunes(s string, limit int) string {
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	return string([]rune(s)[:limit])
}
