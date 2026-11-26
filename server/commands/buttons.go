package commands

import (
	"context"
	"log"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/kdlbs/kandev/pluginsdk"

	"kandev-plugin-discord/server/kandev"
	"kandev-plugin-discord/server/outbound"
	"kandev-plugin-discord/server/prompts"
)

// button answers a prompt button: a permission option or a step approval.
func (c *call) button(ctx context.Context) {
	customID := c.i.MessageComponentData().CustomID
	taskID, ok := c.taskOfChannel(ctx)
	if !ok {
		c.reply(ctx, "This prompt is no longer attached to a task channel.")
		return
	}
	if !c.requireOperator(ctx) {
		return
	}
	if interactionID, index, ok := prompts.ParsePermissionButton(customID); ok {
		c.respondPermission(ctx, taskID, interactionID, func([]pluginsdk.InteractionOption) int { return index })
		return
	}
	if sessionID, stepKey, nonce, ok := prompts.ParseStepButton(customID); ok {
		c.approveStep(ctx, taskID, sessionID, stepKey, nonce)
		return
	}
	c.reply(ctx, "Unknown button.")
}

// autocomplete lists pending requests for /approve and /deny.
func (c *call) autocomplete(ctx context.Context) {
	data := c.i.ApplicationCommandData()
	typed := ""
	for _, o := range data.Options {
		if o.Focused {
			typed = strings.ToLower(o.StringValue())
		}
	}
	var choices []*discordgo.ApplicationCommandOptionChoice
	add := func(name, value string) {
		if len(choices) < 25 && (typed == "" || strings.Contains(strings.ToLower(name), typed)) {
			if r := []rune(name); len(r) > 100 {
				name = string(r[:99]) + "…"
			}
			choices = append(choices, &discordgo.ApplicationCommandOptionChoice{Name: name, Value: value})
		}
	}
	if taskID, ok := c.taskOfChannel(ctx); ok {
		if pending, err := c.pendingPermissions(ctx, taskID); err == nil {
			for _, it := range pending {
				add("Permission: "+strings.TrimSpace(it.Title), requestPermission+it.ID)
			}
		}
		if data.Name == CmdApprove {
			c.addStepChoices(ctx, taskID, add)
		}
	}
	if err := c.API.InteractionRespond(ctx, c.i, &discordgo.InteractionResponse{
		Type: discordgo.InteractionApplicationCommandAutocompleteResult,
		Data: &discordgo.InteractionResponseData{Choices: choices},
	}); err != nil {
		log.Printf("discord: answering autocomplete %s: %v", c.i.ID, err)
	}
}

func (c *call) addStepChoices(ctx context.Context, taskID string, add func(name, value string)) {
	sessions, err := c.Host.ListSessions(ctx, taskID)
	if err != nil {
		return
	}
	for _, s := range sessions {
		if outbound.Running(s.State) {
			continue
		}
		a, err := c.Host.GetStepApproval(ctx, pluginsdk.ExactStepApprovalQuery{
			RequestID: kandev.NewRequestID(), WorkspaceID: c.binding.WorkspaceID, SessionID: s.ID,
		})
		if err != nil || !a.Awaiting {
			continue
		}
		name := "Step: " + a.FromStepName
		if a.ToStepName != "" {
			name += " → " + a.ToStepName
		}
		add(name, requestStep+s.ID+":"+prompts.StepKey(a.FromStepID, a.ToStepID))
	}
}
