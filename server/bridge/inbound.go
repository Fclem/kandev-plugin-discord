package bridge

import (
	"context"
	"log"

	"github.com/bwmarrin/discordgo"

	"kandev-plugin-discord/server/authz"
	"kandev-plugin-discord/server/inbound"
	"kandev-plugin-discord/server/store"
)

func (b *Bridge) inboundHandler(guild store.Guild) *inbound.Handler {
	return &inbound.Handler{
		API: b.api, Host: b.host, Now: b.now, MaxAge: b.cfg.InboundCatchupMaxAge,
		Save: func(ctx context.Context, r store.Channel) error { return b.st.PutChannel(ctx, r, r.ChannelID) },
		IsOperator: func(ctx context.Context, m *discordgo.Message) bool {
			member := m.Member
			if member == nil {
				// REST-fetched messages (catch-up) carry no member roles.
				fetched, err := b.api.GuildMember(ctx, guild.GuildID, m.Author.ID)
				if err != nil {
					log.Printf("discord: reading member %s: %v", m.Author.ID, err)
				}
				member = fetched
			}
			return authz.IsOperator(b.cfg, guild.OperatorRoleID, member, m.Author.ID)
		},
	}
}

// onMessageCreate queues a human message of a mapped task channel on its
// task, in gateway order.
func (b *Bridge) onMessageCreate(ctx context.Context, m *discordgo.MessageCreate) {
	if m == nil || !inbound.Deliverable(m.Message) {
		return
	}
	guild, _, ok := b.guildByID(m.GuildID)
	if !ok {
		return
	}
	ix, found, err := b.st.ChannelIndex(ctx, m.ChannelID)
	if err != nil || !found || !b.events.add("msg:"+m.ID) {
		return
	}
	msg := m.Message
	b.sched.Do(ix.TaskID, func(ctx context.Context) {
		rec, found, err := b.st.Channel(ctx, ix.TaskID)
		if err != nil || !found || rec.Status != store.StatusActive || rec.ChannelID != msg.ChannelID {
			return
		}
		if _, err := b.inboundHandler(guild).Handle(ctx, rec, msg); err != nil {
			log.Printf("discord: delivering message %s to task %s: %v", msg.ID, ix.TaskID, err)
		}
	})
}

// catchUp queues delivery of the messages a channel received while the
// bridge was offline.
func (b *Bridge) catchUp(taskID, channelID string) {
	b.sched.Do(taskID, func(ctx context.Context) {
		rec, found, err := b.st.Channel(ctx, taskID)
		if err != nil || !found || rec.Status != store.StatusActive || rec.ChannelID != channelID {
			return
		}
		guild, ok := b.guild(rec.WorkspaceID)
		if !ok {
			return
		}
		if _, err := b.inboundHandler(guild).CatchUp(ctx, rec); err != nil {
			log.Printf("discord: catching up channel of task %s: %v", taskID, err)
		}
	})
}
