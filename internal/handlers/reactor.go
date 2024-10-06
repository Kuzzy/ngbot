package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"

	api "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/pkg/errors"
	"github.com/sashabaranov/go-openai"
	log "github.com/sirupsen/logrus"

	"github.com/iamwavecut/ngbot/internal/bot"
	"github.com/iamwavecut/ngbot/internal/config"
	"github.com/iamwavecut/ngbot/internal/db"
	"github.com/iamwavecut/ngbot/internal/i18n"
	"github.com/iamwavecut/tool"
)

var flaggedEmojis = []string{"💩", "👎", "🖕", "🤮", "🤬", "😡", "💀", "☠️", "🤢", "👿"}

type banInfo struct {
	OK         bool    `json:"ok"`
	UserID     int64   `json:"user_id"`
	Banned     bool    `json:"banned"`
	When       string  `json:"when"`
	Offenses   int     `json:"offenses"`
	SpamFactor float64 `json:"spam_factor"`
}

type Reactor struct {
	s      bot.Service
	llmAPI *openai.Client
	model  string
}

func NewReactor(s bot.Service, llmAPI *openai.Client, model string) *Reactor {
	log.WithFields(log.Fields{
		"scope":  "Reactor",
		"method": "NewReactor",
	}).Debug("creating new Reactor")
	r := &Reactor{
		s:      s,
		llmAPI: llmAPI,
		model:  model,
	}
	return r
}

func (r *Reactor) Handle(ctx context.Context, u *api.Update, chat *api.Chat, user *api.User) (bool, error) {
	entry := r.getLogEntry().
		WithFields(log.Fields{
			"method":     "Handle",
			"chat_id":    chat.ID,
			"chat_title": chat.Title,
		})
	entry.Debug("handling update")

	if u == nil {
		entry.Error("Update is nil")
		return false, errors.New("nil update")
	}

	nonNilFields := []string{}
	isNonNilPtr := func(v reflect.Value) bool {
		return v.Kind() == reflect.Ptr && !v.IsNil()
	}
	val := reflect.ValueOf(u).Elem()
	typ := val.Type()
	for i := 0; i < val.NumField(); i++ {
		field := val.Field(i)
		fieldName := typ.Field(i).Name

		if isNonNilPtr(field) {
			nonNilFields = append(nonNilFields, fieldName)
		}
	}
	entry.Debug("Checking update type")
	if u.Message == nil && u.MessageReaction == nil {
		entry.Debug("Update is not about message or reaction, not proceeding")
		return false, nil
	}
	entry.Debug("Update is about message or reaction, proceeding")

	if chat == nil {
		entry.Warn("No chat")
		entry.WithField("non_nil_fields", strings.Join(nonNilFields, ", ")).Warn("Non-nil fields")
		return true, nil
	}
	if user == nil {
		entry.Warn("No user")
		entry.WithField("non_nil_fields", strings.Join(nonNilFields, ", ")).Warn("Non-nil fields")
		return true, nil
	}

	entry.Debug("Fetching chat settings")
	settings, err := r.s.GetSettings(chat.ID)
	if err != nil {
		entry.WithError(err).Error("Failed to get chat settings")
	}
	if settings == nil {
		entry.Debug("Settings are nil, using default settings")
		settings = &db.Settings{
			Enabled:          true,
			ChallengeTimeout: defaultChallengeTimeout,
			RejectTimeout:    defaultRejectTimeout,
			Language:         "ru",
			ID:               chat.ID,
		}

		err = r.s.SetSettings(settings)
		if err != nil {
			entry.WithError(err).Error("Failed to set default chat settings")
		}
	}

	if !settings.Enabled {
		entry.Warn("reactor is disabled for this chat")
		return true, nil
	}

	b := r.s.GetBot()
	if b == nil {
		entry.Warn("Bot is nil")
		return false, errors.New("nil bot")
	}

	if u.MessageReaction != nil {
		entry.Debug("Processing message reaction")
		for _, react := range u.MessageReaction.NewReaction {
			flags := map[string]int{}
			emoji := react.Emoji
			if react.Type == api.StickerTypeCustomEmoji {
				entry.Debug("processing custom emoji")
				emojiStickers, err := b.GetCustomEmojiStickers(api.GetCustomEmojiStickersConfig{
					CustomEmojiIDs: []string{react.CustomEmoji},
				})
				if err != nil {
					entry.WithError(err).Warn("custom emoji get error")
					continue
				}
				if len(emojiStickers) > 0 {
					emoji = emojiStickers[0].Emoji
				}
			}
			if slices.Contains(flaggedEmojis, emoji) {
				entry.WithField("emoji", emoji).Debug("flagged emoji detected")
				flags[emoji]++
			}

			for _, flagged := range flags {
				if flagged >= 5 {
					entry.Warn("user reached flag threshold, attempting to ban")
					if err := bot.BanUserFromChat(b, user.ID, chat.ID); err != nil {
						entry.WithFields(log.Fields{
							"user": bot.GetFullName(user),
							"chat": chat.Title,
						}).Error("cant ban user in chat")
					}
					return true, nil
				}
			}
		}
	}

	if u.Message != nil {
		entry.Debug("handling new message")
		if err := r.handleFirstMessage(ctx, u, chat, user); err != nil {
			entry.WithError(err).Error("error handling new message")
		}
	}

	return true, nil
}

func (r *Reactor) handleFirstMessage(ctx context.Context, u *api.Update, chat *api.Chat, user *api.User) error {
	entry := r.getLogEntry().WithField("method", "handleFirstMessage")
	entry.Debug("handling first message")
	m := u.Message

	entry.Debug("checking if user is a member")
	isMember, err := r.s.IsMember(ctx, chat.ID, user.ID)
	if err != nil {
		return errors.WithMessage(err, "cant check if member")
	}
	if isMember {
		entry.Debug("user is already a member")
		return nil
	}

	entry.Debug("checking first message content")
	if err := r.checkFirstMessage(ctx, chat, user, m); err != nil {
		return errors.WithMessage(err, "cant check first message")
	}

	return nil
}

func (r *Reactor) checkFirstMessage(ctx context.Context, chat *api.Chat, user *api.User, m *api.Message) error {
	entry := r.getLogEntry().
		WithFields(log.Fields{
			"method":    "checkFirstMessage",
			"user_name": bot.GetUN(user),
			"user_id":   user.ID,
		})

	entry.Debug("checking first message")
	b := r.s.GetBot()

	messageContent := m.Text
	if messageContent == "" && m.Caption != "" {
		messageContent = m.Caption
	}

	if messageContent == "" {
		entry.Warn("empty message content, skipping spam check")
		return nil
	}

	banSpammer := func(chatID, userID int64, messageID int) (bool, error) {
		entry.Info("spam detected, banning user")
		var errs []error
		if err := bot.DeleteChatMessage(b, chatID, messageID); err != nil {
			errs = append(errs, errors.Wrap(err, "failed to delete message"))
		}
		if err := bot.BanUserFromChat(b, userID, chatID); err != nil {
			errs = append(errs, errors.Wrap(err, "failed to ban user"))
		}
		if len(errs) > 0 {
			lang := r.getLanguage(chat, user)

			entry.WithField("errors", errs).Error("failed to handle spam")
			var msgContent string
			if len(errs) == 2 {
				entry.Warn("failed to ban and delete message")
				msgContent = fmt.Sprintf(i18n.Get("I can't delete messages or ban spammer \"%s\".", lang), bot.GetUN(user))
			} else if errors.Is(errs[0], errors.New("failed to delete message")) {
				entry.Warn("failed to delete message")
				msgContent = fmt.Sprintf(i18n.Get("I can't delete messages from spammer \"%s\".", lang), bot.GetUN(user))
			} else {
				entry.Warn("failed to ban spammer")
				msgContent = fmt.Sprintf(i18n.Get("I can't ban spammer \"%s\".", lang), bot.GetUN(user))
			}
			msgContent += " " + i18n.Get("I should have the permissions to ban and delete messages here.", lang)
			msg := api.NewMessage(chat.ID, msgContent)
			msg.ParseMode = api.ModeHTML
			if _, err := b.Send(msg); err != nil {
				entry.WithError(err).Error("failed to send message about lack of permissions")
			}
			return false, errors.New("failed to handle spam")
		}
		return true, nil
	}

	entry.Debug("checking if user is banned")
	url := fmt.Sprintf("https://api.lols.bot/account?id=%d", user.ID)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		entry.WithError(err).Error("failed to create request")
		return errors.WithMessage(err, "failed to create request")
	}
	req.Header.Set("accept", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		entry.WithError(err).Error("failed to send request")
		return errors.WithMessage(err, "failed to send request")
	}
	defer resp.Body.Close()

	banCheck := banInfo{}
	if err := json.NewDecoder(resp.Body).Decode(&banCheck); err != nil {
		entry.WithError(err).Error("failed to decode response")
		return errors.WithMessage(err, "failed to decode response")
	}

	if banCheck.Banned {
		entry = entry.WithFields(log.Fields{
			"chat_id":    chat.ID,
			"user_id":    user.ID,
			"message_id": m.MessageID,
		})
		success, err := banSpammer(chat.ID, user.ID, m.MessageID)
		if err != nil {
			entry.WithError(err).Error("Failed to execute ban action on spammer")
			return errors.Wrap(err, "failed to ban spammer")
		}
		if !success {
			entry.Error("Ban action on spammer was unsuccessful")
			return errors.New("failed to ban spammer")
		}
		entry.Info("Spammer successfully banned and removed from chat")
		return nil
	}

	entry.Info("sending first message to OpenAI for spam check", r.model)
	llmResp, err := r.llmAPI.CreateChatCompletion(
		ctx,
		openai.ChatCompletionRequest{
			Model: r.model,
			Messages: []openai.ChatCompletionMessage{
				{
					Role: openai.ChatMessageRoleSystem,
					Content: `
						Вы система обнаружения спама.
						Отвечайте 'SPAM', если сообщение является спамом, или 'NOT_SPAM', если не является.
						Не предоставляйте никакой другой информации. Обращайте особое внимание на сообщения, которые 
						содержат предложения о заработке и наборы на удаленную работу или участие в операциях с 
						криптовалютами. В подавляющем большинстве они являются спамом! Спаммеры часто любят смешивать 
						буквы кириллического и латинского алфавита, чтобы обмануть спам системы, обращайте на такие 
						сообщения повышенное внимание.
					`,
				},
				{
					Role:    openai.ChatMessageRoleUser,
					Content: messageContent,
				},
			},
		},
	)

	if err != nil {
		entry.WithError(err).Error("failed to create chat completion")
		return errors.Wrap(err, "failed to create chat completion")
	}

	if len(llmResp.Choices) > 0 && llmResp.Choices[0].Message.Content == "SPAM" {
		success, err := banSpammer(chat.ID, user.ID, m.MessageID)
		if err != nil {
			entry.WithError(err).Error("failed to ban spammer")
			return errors.Wrap(err, "failed to ban spammer")
		}
		if !success {
			entry.Error("failed to ban spammer")
			return errors.New("failed to ban spammer")
		}
	}

	entry.Debug("message passed spam check, inserting member")
	if err := r.s.InsertMember(ctx, chat.ID, user.ID); err != nil {
		entry.WithError(err).Error("failed to insert member")
		return errors.Wrap(err, "failed to insert member")
	}

	entry.Info("message passed spam check, user added to members")
	return nil
}

func (r *Reactor) getLogEntry() *log.Entry {
	return log.WithField("object", "Reactor")
}

func (r *Reactor) getLanguage(chat *api.Chat, user *api.User) string {
	entry := r.getLogEntry().WithField("method", "getLanguage")
	entry.Debug("getting language for chat and user")
	if settings, err := r.s.GetDB().GetSettings(chat.ID); !tool.Try(err) {
		entry.WithField("language", settings.Language).Debug("using language from chat settings")
		return settings.Language
	}
	if user != nil && tool.In(user.LanguageCode, i18n.GetLanguagesList()...) {
		entry.WithField("language", user.LanguageCode).Debug("using user's language")
		entry.Debug("using user's language:", user.LanguageCode)
		return user.LanguageCode
	}
	entry.Debug("using default language:", config.Get().DefaultLanguage)
	return config.Get().DefaultLanguage
}
