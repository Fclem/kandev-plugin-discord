// Package prompts posts what awaits a human in the task channel: one message
// per pending permission request (with a button per option), one notice per
// pending clarification, and one workflow step approval prompt. Host
// Interactions() and GetStepApproval are the source of truth; the stored
// records only remember what was posted.
package prompts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/kdlbs/kandev/pluginsdk"

	"kandev-plugin-discord/server/discordapi"
	"kandev-plugin-discord/server/kandev"
	"kandev-plugin-discord/server/outbound"
	"kandev-plugin-discord/server/store"
)

// Button custom IDs.
const (
	PermissionButtonPrefix = "kd:perm:" // kd:perm:<interaction id>:<option index>
	StepButtonPrefix       = "kd:step:" // kd:step:<session id>:<step key>:<prompt nonce>
)

// Discord component limits.
const (
	maxButtons      = 25
	buttonsPerRow   = 5
	maxButtonLabel  = 80
	maxPromptLength = 1800
)

// Poster keeps one task's prompts in step with Kandev.
type Poster struct {
	API     discordapi.API
	Host    kandev.Host
	Store   *store.Store
	TaskURL func(taskID string) string
	Now     func() time.Time
}

// PermissionButtonID returns the custom ID of one option's button.
func PermissionButtonID(interactionID string, option int) string {
	return PermissionButtonPrefix + interactionID + ":" + strconv.Itoa(option)
}

// ParsePermissionButton splits a permission button custom ID.
func ParsePermissionButton(customID string) (interactionID string, option int, ok bool) {
	rest, ok := strings.CutPrefix(customID, PermissionButtonPrefix)
	if !ok {
		return "", 0, false
	}
	i := strings.LastIndex(rest, ":")
	if i <= 0 {
		return "", 0, false
	}
	n, err := strconv.Atoi(rest[i+1:])
	return rest[:i], n, err == nil
}

// StepKey names one approval transition (from step, to step) in a short
// opaque form that fits a component custom ID.
func StepKey(fromStepID, toStepID string) string {
	sum := sha256.Sum256([]byte(fromStepID + "\x00" + toStepID))
	return hex.EncodeToString(sum[:6])
}

// StepButtonID binds an Approve button to the transition it displays and
// to the posted prompt, so neither a changed approval target nor a later
// visit to the same step can be approved through an old button.
func StepButtonID(sessionID, stepKey, nonce string) string {
	return StepButtonPrefix + sessionID + ":" + stepKey + ":" + nonce
}

// ParseStepButton splits a step button custom ID.
func ParseStepButton(customID string) (sessionID, stepKey, nonce string, ok bool) {
	rest, ok := strings.CutPrefix(customID, StepButtonPrefix)
	if !ok {
		return "", "", "", false
	}
	parts := strings.Split(rest, ":")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}

// Sync posts prompts for pending interactions without one, resolves prompts
// whose interaction ended, and keeps the step approval prompt current.
func (p *Poster) Sync(ctx context.Context, task *pluginsdk.Task, rec store.Channel, sessions []pluginsdk.Session) error {
	if err := p.syncInteractions(ctx, task, rec); err != nil {
		return err
	}
	return p.syncStep(ctx, task, rec, sessions)
}

func (p *Poster) syncInteractions(ctx context.Context, task *pluginsdk.Task, rec store.Channel) error {
	pending, err := p.Host.ListPendingInteractions(ctx, task.ID)
	if err != nil {
		return fmt.Errorf("listing pending interactions: %w", err)
	}
	stored, err := p.Store.Prompts(ctx, task.ID)
	if err != nil {
		return err
	}
	changed := false
	isPending := map[string]bool{}
	for _, it := range pending {
		isPending[it.ID] = true
		if slices.ContainsFunc(stored.Items, func(pr store.Prompt) bool { return pr.InteractionID == it.ID }) {
			continue
		}
		// The nonce makes a retry after a lost reply return the first post
		// instead of leaving an unrecorded duplicate with live buttons.
		msg, err := p.API.MessageSendOnce(ctx, rec.ChannelID, p.render(task, it), discordapi.Nonce(rec.ChannelID, "perm", strconv.Itoa(rec.Opens), it.ID))
		if err != nil {
			return fmt.Errorf("posting prompt: %w", err)
		}
		stored.Items = append(stored.Items, store.Prompt{InteractionID: it.ID, Kind: it.Kind, SessionID: it.SessionID, MessageID: msg.ID, PostedAt: p.Now()})
		changed = true
		if err := p.Store.PutPrompts(ctx, task.ID, stored); err != nil {
			return err
		}
	}
	kept := stored.Items[:0:0]
	for _, pr := range stored.Items {
		if isPending[pr.InteractionID] {
			kept = append(kept, pr)
			continue
		}
		it, err := p.Host.GetInteraction(ctx, pr.InteractionID)
		if err == nil && it.Status == pluginsdk.InteractionStatusPending {
			kept = append(kept, pr) // listed late; keep
			continue
		}
		if err != nil && !kandev.IsNotFound(err) {
			kept = append(kept, pr)
			continue
		}
		if err := p.Resolve(ctx, rec.ChannelID, pr.MessageID, ResolvedText(it, nil)); err != nil {
			log.Printf("discord: task %s: %v; retrying on the next sync", task.ID, err)
			kept = append(kept, pr)
			continue
		}
		changed = true
	}
	if !changed {
		return nil
	}
	return p.Store.PutPrompts(ctx, task.ID, store.Prompts{Items: kept})
}

// render builds a permission prompt with buttons, or a clarification notice.
func (p *Poster) render(task *pluginsdk.Task, it pluginsdk.Interaction) *discordgo.MessageSend {
	url := p.TaskURL(task.ID)
	if it.Kind != pluginsdk.InteractionKindPermission {
		text := "❓ **The agent asked a question.** Answer it in Kandev"
		if url != "" {
			text += ": " + url
		}
		return &discordgo.MessageSend{Content: truncate(text+"\n"+questionSummary(it), maxPromptLength)}
	}
	text := "🔐 **Permission request:** " + strings.TrimSpace(it.Title)
	if c := strings.TrimSpace(it.Context); c != "" {
		text += "\n" + c
	}
	text = truncate(text, maxPromptLength)
	var buttons []discordgo.MessageComponent
	for i, opt := range it.Options {
		if i == maxButtons {
			if url != "" {
				text += "\nMore options in Kandev: " + url
			}
			break
		}
		buttons = append(buttons, discordgo.Button{
			Label: truncate(opt.Label, maxButtonLabel), Style: buttonStyle(opt.Kind), CustomID: PermissionButtonID(it.ID, i),
		})
	}
	return &discordgo.MessageSend{Content: text, Components: rows(buttons)}
}

func questionSummary(it pluginsdk.Interaction) string {
	var lines []string
	for _, q := range it.Questions {
		if t := strings.TrimSpace(q.Prompt); t != "" {
			lines = append(lines, "> "+t)
		} else if t := strings.TrimSpace(q.Title); t != "" {
			lines = append(lines, "> "+t)
		}
	}
	return strings.Join(lines, "\n")
}

func buttonStyle(kind string) discordgo.ButtonStyle {
	switch {
	case strings.HasPrefix(kind, "allow"):
		return discordgo.SuccessButton
	case strings.HasPrefix(kind, "reject"):
		return discordgo.DangerButton
	}
	return discordgo.SecondaryButton
}

func rows(buttons []discordgo.MessageComponent) []discordgo.MessageComponent {
	var out []discordgo.MessageComponent
	for i := 0; i < len(buttons); i += buttonsPerRow {
		out = append(out, discordgo.ActionsRow{Components: buttons[i:min(i+buttonsPerRow, len(buttons))]})
	}
	return out
}

// ResolvedText describes how an interaction ended: by whom when Kandev
// knows, otherwise "resolved elsewhere".
func ResolvedText(it *pluginsdk.Interaction, actor *pluginsdk.InteractionActor) string {
	if actor == nil && it != nil {
		actor = it.ResolvedBy
	}
	verb := "Resolved"
	if it != nil {
		switch it.Status {
		case pluginsdk.InteractionStatusApproved:
			verb = "Approved"
		case pluginsdk.InteractionStatusRejected:
			verb = "Denied"
		case pluginsdk.InteractionStatusAnswered:
			verb = "Answered"
		case pluginsdk.InteractionStatusCancelled, pluginsdk.InteractionStatusExpired:
			return "⏹️ No longer pending."
		}
	}
	if actor != nil && actor.DisplayName != "" {
		via := ""
		if actor.Via != "" {
			via = " via " + actor.Via
		}
		return fmt.Sprintf("✅ %s by %s%s.", verb, actor.DisplayName, via)
	}
	return "✅ Resolved elsewhere."
}

// closedText ends a prompt whose channel closed while it was pending.
const closedText = "⏹️ The channel closed; answer in Kandev if this is still pending."

// Close resolves every posted prompt of a closed (read-only) channel, so no
// button stays live there. A prompt whose edit failed stays recorded, and
// the next call retries it.
func (p *Poster) Close(ctx context.Context, taskID, channelID string) error {
	var errs []error
	stored, err := p.Store.Prompts(ctx, taskID)
	if err != nil {
		return err
	}
	kept := stored.Items[:0:0]
	for _, pr := range stored.Items {
		if err := p.Resolve(ctx, channelID, pr.MessageID, closedText); err != nil {
			errs = append(errs, err)
			kept = append(kept, pr)
		}
	}
	if len(kept) != len(stored.Items) {
		if err := p.Store.PutPrompts(ctx, taskID, store.Prompts{Items: kept}); err != nil {
			errs = append(errs, err)
		}
	}
	step, posted, err := p.Store.StepApproval(ctx, taskID)
	switch {
	case err != nil:
		errs = append(errs, err)
	case posted:
		if err := p.Resolve(ctx, channelID, step.MessageID, closedText); err != nil {
			errs = append(errs, err)
		} else if err := p.Store.DeleteStepApproval(ctx, taskID); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Resolve appends an outcome to a prompt and removes its buttons. It
// returns an error only when the prompt may still show live buttons; a
// message or channel Discord no longer has counts as resolved.
func (p *Poster) Resolve(ctx context.Context, channelID, messageID, outcome string) error {
	original := ""
	if msgs, err := p.API.ChannelMessages(ctx, channelID, 1, prevID(messageID)); err == nil && len(msgs) > 0 && msgs[0].ID == messageID {
		original = msgs[0].Content
	}
	content := strings.TrimSpace(truncate(original, maxPromptLength) + "\n\n" + outcome)
	empty := []discordgo.MessageComponent{}
	if _, err := p.API.MessageEdit(ctx, &discordgo.MessageEdit{Channel: channelID, ID: messageID, Content: &content, Components: &empty}); err != nil && !discordapi.IsGone(err) {
		return fmt.Errorf("updating prompt %s: %w", messageID, err)
	}
	return nil
}

// prevID is the snowflake just before id, so ChannelMessages(after=prev,
// limit=1) returns the message itself.
func prevID(id string) string {
	n, err := strconv.ParseUint(id, 10, 64)
	if err != nil || n == 0 {
		return id
	}
	return strconv.FormatUint(n-1, 10)
}

// syncStep posts the step approval prompt when a session's step awaits the
// native Approve action, and clears it when that is no longer true. A
// prompt whose buttons could not be removed stays recorded, so the next
// sync retries the edit before posting another.
func (p *Poster) syncStep(ctx context.Context, task *pluginsdk.Task, rec store.Channel, sessions []pluginsdk.Session) error {
	current, posted, err := p.Store.StepApproval(ctx, task.ID)
	if err != nil {
		return err
	}
	var awaiting *pluginsdk.StepApproval
	for _, s := range sessions {
		if outbound.Running(s.State) {
			continue
		}
		approval, err := p.Host.GetStepApproval(ctx, pluginsdk.ExactStepApprovalQuery{
			RequestID: kandev.NewRequestID(), WorkspaceID: task.WorkspaceID, SessionID: s.ID,
		})
		if err != nil {
			if kandev.IsUnimplemented(err) || kandev.IsPermissionDenied(err) || kandev.IsNotFound(err) {
				continue // Office stage, missing grant, or gone: nothing to post
			}
			return fmt.Errorf("reading step approval: %w", err)
		}
		if approval.Awaiting {
			approval.SessionID = s.ID
			awaiting = &approval
			break
		}
	}
	if posted && awaiting != nil && current.SessionID == awaiting.SessionID && current.FromStepID == awaiting.FromStepID && current.ToStepID == awaiting.ToStepID {
		if current.ResourceVersion == awaiting.ResourceVersion {
			return nil
		}
		// Same transition at a new approval state (a later visit, or a
		// session update): give the prompt a fresh button, silently, so the
		// old one can no longer approve it.
		nonce := stepNonce(rec, *awaiting)[:8]
		buttons := stepButtons(*awaiting, nonce)
		if _, err := p.API.MessageEdit(ctx, &discordgo.MessageEdit{Channel: rec.ChannelID, ID: current.MessageID, Components: &buttons}); err != nil {
			if !discordapi.IsGone(err) {
				return fmt.Errorf("refreshing step approval: %w", err)
			}
			// The prompt is gone: forget it and post a new one below.
		} else {
			current.ResourceVersion, current.Nonce = awaiting.ResourceVersion, nonce
			return p.Store.PutStepApproval(ctx, task.ID, current)
		}
	} else if posted {
		if err := p.Resolve(ctx, rec.ChannelID, current.MessageID, "⏹️ No longer pending."); err != nil {
			return err
		}
	}
	if posted {
		if err := p.Store.DeleteStepApproval(ctx, task.ID); err != nil {
			return err
		}
	}
	if awaiting == nil {
		return nil
	}
	text := fmt.Sprintf("⏸️ Step **%s** awaits approval", awaiting.FromStepName)
	if awaiting.ToStepName != "" {
		text += fmt.Sprintf(" → **%s**", awaiting.ToStepName)
	}
	text += ". Approve to move on, or post a message to request changes."
	nonce := stepNonce(rec, *awaiting)
	msg, err := p.API.MessageSendOnce(ctx, rec.ChannelID, &discordgo.MessageSend{Content: text, Components: stepButtons(*awaiting, nonce[:8])}, nonce)
	if err != nil {
		return fmt.Errorf("posting step approval: %w", err)
	}
	return p.Store.PutStepApproval(ctx, task.ID, store.StepApproval{
		SessionID: awaiting.SessionID, FromStepID: awaiting.FromStepID, ToStepID: awaiting.ToStepID,
		ResourceVersion: awaiting.ResourceVersion, Nonce: nonce[:8], MessageID: msg.ID, PostedAt: p.Now(),
	})
}

func stepButtons(a pluginsdk.StepApproval, nonce string) []discordgo.MessageComponent {
	return rows([]discordgo.MessageComponent{discordgo.Button{
		Label: "Approve", Style: discordgo.SuccessButton,
		CustomID: StepButtonID(a.SessionID, StepKey(a.FromStepID, a.ToStepID), nonce),
	}})
}

// stepNonce identifies one approval state's prompt in one channel. Its
// prefix is the button's prompt nonce and the whole the message nonce, so a
// post retried after a lost reply returns the same prompt with the same
// button.
func stepNonce(rec store.Channel, a pluginsdk.StepApproval) string {
	return discordapi.Nonce(rec.ChannelID, "step", strconv.Itoa(rec.Opens), a.SessionID, a.FromStepID, a.ToStepID, a.ResourceVersion)
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
