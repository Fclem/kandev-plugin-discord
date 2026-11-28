package bridge

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/bwmarrin/discordgo"

	"kandev-plugin-discord/server/authz"
	"kandev-plugin-discord/server/discordapi"
	"kandev-plugin-discord/server/inbound"
	"kandev-plugin-discord/server/store"
)

// catchUpRetry is how long a failed catch-up waits before retrying.
const catchUpRetry = time.Minute

func (b *Bridge) inboundHandler(guild store.Guild) *inbound.Handler {
	return &inbound.Handler{
		API: b.api, Host: b.host, Now: b.now, MaxAge: b.cfg.InboundCatchupMaxAge,
		Save: func(ctx context.Context, r store.Channel) error { return b.st.PutChannel(ctx, r, r.ChannelID) },
		Authorize: func(ctx context.Context, m *discordgo.Message) (*discordgo.Member, bool, error) {
			member := m.Member
			if member == nil {
				// REST-fetched messages (catch-up) carry no member roles.
				fetched, err := b.api.GuildMember(ctx, guild.GuildID, m.Author.ID)
				switch {
				case err == nil:
					member = fetched
				case !isUnknownMember(err):
					return nil, false, err
				}
			}
			return member, authz.IsOperator(b.cfg, guild.OperatorRoleID, member, m.Author.ID), nil
		},
	}
}

// isUnknownMember is Discord's authoritative "not in this server".
func isUnknownMember(err error) bool {
	const codeUnknownMember = 10007
	return discordapi.HasCode(err, codeUnknownMember)
}

// onMessageCreate queues a message on its channel's intake queue, in
// gateway order (handlers run synchronously). The queue resolves the task
// and runs delivery on the task's queue after any pending catch-up, so a
// live message can never move the cursor past an older missed one.
func (b *Bridge) onMessageCreate(_ context.Context, m *discordgo.MessageCreate) {
	if m == nil || !inbound.Deliverable(m.Message) {
		return
	}
	if _, _, ok := b.guildByID(m.GuildID); !ok {
		if _, bound := b.cfg.BindingForGuild(m.GuildID); bound {
			// Provisioning is pending (first start, or roles unknown after
			// a failed role change): the channel catches up from its saved
			// cursor before its next live message, and on the reconcile
			// that follows provisioning.
			b.mu.Lock()
			b.caughtUp[m.ChannelID] = -1
			b.mu.Unlock()
		}
		return
	}
	if !b.events.add("msg:" + m.ID) {
		return
	}
	msg := m.Message
	b.sched.Do("chan:"+msg.ChannelID, func(ctx context.Context) {
		ix, found, err := b.st.ChannelIndex(ctx, msg.ChannelID)
		if err != nil {
			// Route unknown for now: the message stays in Discord's history
			// and the channel catches up before any later live message.
			log.Printf("discord: routing message %s: %v; retrying from history", msg.ID, err)
			b.retryCatchUp(msg.ChannelID)
			return
		}
		if !found || ix.GuildID != msg.GuildID {
			return
		}
		b.onTaskAndWait(ix.TaskID, func(ctx context.Context) {
			rec, guild, ok, err := b.activeRecord(ctx, ix.TaskID, msg.ChannelID, msg.GuildID)
			if err == nil && ok && b.ensureCaughtUp(ctx, rec, guild, msg.ID) {
				rec, _, ok, err = b.activeRecord(ctx, ix.TaskID, msg.ChannelID, msg.GuildID)
				if err == nil && ok {
					_, err = b.inboundHandler(guild).Handle(ctx, rec, msg)
				}
			}
			if err != nil {
				// The cursor stayed before this message; later messages must
				// not move it past, so the channel catches up from it first.
				log.Printf("discord: delivering message %s to task %s: %v; retrying from history", msg.ID, ix.TaskID, err)
				b.retryCatchUp(msg.ChannelID)
			}
		})
	})
}

// activeRecord returns a task's record when it is the active channel of
// the bound, provisioned guild given. err is a store failure: the answer is
// unknown, not no.
func (b *Bridge) activeRecord(ctx context.Context, taskID, channelID, guildID string) (store.Channel, store.Guild, bool, error) {
	rec, found, err := b.st.Channel(ctx, taskID)
	if err != nil {
		return rec, store.Guild{}, false, fmt.Errorf("reading the channel record: %w", err)
	}
	if !found || rec.Status != store.StatusActive || rec.ChannelID != channelID || rec.GuildID != guildID {
		return rec, store.Guild{}, false, nil
	}
	guild, binding, ok := b.guildByID(guildID)
	if !ok || binding.WorkspaceID != rec.WorkspaceID {
		return rec, store.Guild{}, false, nil
	}
	return rec, guild, true, nil
}

// ensureCaughtUp delivers what a channel received while the bridge was
// offline, once per READY, before any live message of that channel; until
// is that live message (catch-up stops before it), or "". It reports
// whether live delivery may proceed.
func (b *Bridge) ensureCaughtUp(ctx context.Context, rec store.Channel, guild store.Guild, until string) bool {
	b.mu.Lock()
	gen := b.readyGen
	b.mu.Unlock()
	if !b.needsCatchUp(rec.ChannelID) {
		return true
	}
	if caught, err := b.inboundHandler(guild).CatchUp(ctx, rec, until); err != nil {
		log.Printf("discord: catching up channel of task %s: %v", rec.TaskID, err)
		if discordapi.IsForbidden(err) {
			// The bot cannot read the channel's history: retrying every
			// minute would only pile up invalid requests. The record keeps
			// the cursor the catch-up saved so far.
			b.lostAccess(ctx, caught, caught.ChannelID, guild, err)
			return false
		}
		b.retryCatchUp(rec.ChannelID)
		return false
	}
	b.mu.Lock()
	b.caughtUp[rec.ChannelID] = gen
	b.mu.Unlock()
	return true
}

// retryCatchUp makes the channel catch up from its saved cursor before any
// further live message, and schedules that catch-up (the reconcile also
// runs it).
func (b *Bridge) retryCatchUp(channelID string) {
	b.mu.Lock()
	b.caughtUp[channelID] = -1
	b.mu.Unlock()
	b.timers.after("catchup:"+channelID, catchUpRetry, func() {
		b.sched.Do("chan:"+channelID, func(ctx context.Context) {
			ix, found, err := b.st.ChannelIndex(ctx, channelID)
			switch {
			case err != nil:
				b.retryCatchUp(channelID)
			case found:
				b.catchUp(ix.TaskID, channelID)
			}
		})
	})
}

// needsCatchUp reports whether a channel has not caught up since the last
// READY, or since a failed delivery.
func (b *Bridge) needsCatchUp(channelID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	done, ok := b.caughtUp[channelID]
	return !ok || done < b.readyGen
}

// catchUp queues the catch-up of one channel on its intake queue.
func (b *Bridge) catchUp(taskID, channelID string) {
	b.sched.Do("chan:"+channelID, func(ctx context.Context) {
		b.onTaskAndWait(taskID, func(ctx context.Context) {
			rec, found, err := b.st.Channel(ctx, taskID)
			if err == nil && !found {
				return
			}
			var guild store.Guild
			ok := false
			if err == nil {
				rec, guild, ok, err = b.activeRecord(ctx, taskID, channelID, rec.GuildID)
			}
			switch {
			case err != nil:
				log.Printf("discord: catching up channel of task %s: %v", taskID, err)
				b.retryCatchUp(channelID)
			case ok:
				b.ensureCaughtUp(ctx, rec, guild, "")
			}
		})
	})
}
