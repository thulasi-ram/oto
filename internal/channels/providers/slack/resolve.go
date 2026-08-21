package slack

import (
	"context"
	"fmt"
	"strings"

	"github.com/slack-go/slack"

	"github.com/thulasiram/oto/internal/channels/domain"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// maxResolvePages bounds how many pages of `conversations.list` one name
// lookup will walk before giving up. 200 per page (Slack's own cap) times 20
// pages is 4000 channels — generous for the self-hosted, single-workspace
// deployments this resolves for, and a real ceiling rather than an unbounded
// loop against somebody else's API.
const maxResolvePages = 20

// conversationListPageSize is Slack's documented maximum for one
// `conversations.list` page.
const conversationListPageSize = 200

// ResolveConversation implements domain.ConversationResolver.
//
// ⭐ THIS RESTORES A CAPABILITY THAT WAS DELETED FOR HAVING ZERO CALLERS. See
// the doc comment on the `API` interface in channel.go for the full history.
// The caller here is real: the settings UI, turning "the channel named
// #sre-alerts" and "the channel with id C0123456" into each other at the
// moment an operator names one destination — not the removed `Probe`, which
// oto never calls at delivery time and never will again (ADR 0008, ADR 0018).
//
// Exactly one of query.Name or query.ID is expected to be set. ID lookup is one
// call (`conversations.info`); name lookup walks `conversations.list` because
// Slack has no "look up a channel by name" method at all.
func (p *Provider) ResolveConversation(
	ctx context.Context, cred domain.Credential, query domain.ConversationQuery,
) (domain.ConversationResult, error) {
	token := botToken(cred)
	if token == "" {
		return domain.ConversationResult{}, errs.Validation("credential_missing",
			"a slack connection needs a "+CredBotToken+" credential",
			errs.Violation{Field: "credential_id", Code: "required", Message: "select a Slack bot token credential"})
	}
	api := p.newAPI(token, p.httpClient)

	if id := strings.TrimSpace(query.ID); id != "" {
		return p.resolveByID(ctx, api, id)
	}
	return p.resolveByName(ctx, api, query.Name)
}

func (p *Provider) resolveByID(ctx context.Context, api API, id string) (domain.ConversationResult, error) {
	info, err := api.GetConversationInfoContext(ctx, &slack.GetConversationInfoInput{ChannelID: id})
	if err != nil {
		return domain.ConversationResult{}, classify(err)
	}
	if info == nil {
		return domain.ConversationResult{}, errs.NotFound("conversation_not_found",
			"no channel with that id was found")
	}
	return domain.ConversationResult{ID: info.ID, Name: info.Name}, nil
}

func (p *Provider) resolveByName(ctx context.Context, api API, rawName string) (domain.ConversationResult, error) {
	name := normalizeConversationName(rawName)
	if name == "" {
		return domain.ConversationResult{}, errs.Validation("validation_failed",
			"1 field failed validation.",
			errs.Violation{Field: "conversation_name", Code: "required", Message: "a channel name is required"})
	}

	cursor := ""
	for page := 0; page < maxResolvePages; page++ {
		chans, next, err := api.GetConversationsContext(ctx, &slack.GetConversationsParameters{
			Types:  []string{"public_channel", "private_channel"},
			Limit:  conversationListPageSize,
			Cursor: cursor,
		})
		if err != nil {
			return domain.ConversationResult{}, classify(err)
		}
		for _, c := range chans {
			if strings.EqualFold(c.Name, name) {
				return domain.ConversationResult{ID: c.ID, Name: c.Name}, nil
			}
		}
		if next == "" {
			break
		}
		cursor = next
	}

	return domain.ConversationResult{}, errs.NotFound("conversation_not_found",
		fmt.Sprintf("no channel named %q was found — check the name and that oto's bot has been invited to it", rawName))
}

// normalizeConversationName strips a leading '#' and surrounding whitespace,
// so "#sre-alerts" and "sre-alerts" resolve identically.
func normalizeConversationName(raw string) string {
	return strings.TrimPrefix(strings.TrimSpace(raw), "#")
}

// Compile-time proof that the provider satisfies the optional capability.
var _ domain.ConversationResolver = (*Provider)(nil)
