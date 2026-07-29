package handlers

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/notify"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/store"
)

// notificationChannelToAPI renders a channel. The configuration blob is never
// part of the representation: it holds webhook URLs and bot tokens, and
// nothing outside the dispatcher needs them (INV-003).
func notificationChannelToAPI(c store.NotificationChannel) api.NotificationChannel {
	return api.NotificationChannel{
		Uuid:      uuidString(c.Uuid),
		Kind:      api.NotificationChannelKind(c.Kind),
		Name:      c.Name,
		Enabled:   c.Enabled,
		CreatedAt: c.CreatedAt.Time.UTC(),
		UpdatedAt: timePtr(c.UpdatedAt),
		Version:   int(c.Version),
	}
}

// hhmm renders a time column as HH:MM; absent when the rule has no window.
func hhmm(t pgtype.Time) *string {
	if !t.Valid {
		return nil
	}
	total := t.Microseconds / 1_000_000
	return ptr(time.Unix(total, 0).UTC().Format("15:04"))
}

// parseHHMM reads a quiet-hour bound.
func parseHHMM(s string) (pgtype.Time, bool) {
	t, err := time.Parse("15:04", strings.TrimSpace(s))
	if err != nil {
		return pgtype.Time{}, false
	}
	micros := int64(t.Hour()*3600+t.Minute()*60) * 1_000_000
	return pgtype.Time{Microseconds: micros, Valid: true}, true
}

func (a *API) notificationRuleToAPI(r *http.Request, rule store.NotificationRule) api.NotificationRule {
	out := api.NotificationRule{
		Uuid:                  uuidString(rule.Uuid),
		EventType:             rule.EventType,
		Enabled:               rule.Enabled,
		MinSeverity:           api.NotificationRuleMinSeverity(rule.MinSeverity),
		DebounceSeconds:       ptr(int(rule.DebounceSeconds)),
		QuietHoursStart:       hhmm(rule.QuietHoursStart),
		QuietHoursEnd:         hhmm(rule.QuietHoursEnd),
		DigestEnabled:         ptr(rule.DigestEnabled),
		DigestIntervalMinutes: ptr(int(rule.DigestIntervalMinutes)),
		CreatedAt:             timePtr(rule.CreatedAt),
	}
	if rule.ProjectID != nil {
		if p, err := a.Store.GetProjectByID(r.Context(), *rule.ProjectID); err == nil {
			out.ProjectUuid = ptr(uuidString(p.Uuid))
		}
	}
	if rule.EnvironmentID != nil {
		if e, err := a.Store.GetEnvironmentByID(r.Context(), *rule.EnvironmentID); err == nil {
			out.EnvironmentUuid = ptr(uuidString(e.Uuid))
		}
	}
	return out
}

func (a *API) resolveChannel(w http.ResponseWriter, r *http.Request, id *auth.Identity, channelUUID string) (store.NotificationChannel, bool) {
	var u pgtype.UUID
	if err := u.Scan(channelUUID); err == nil {
		c, err := a.Store.GetNotificationChannelByUUID(r.Context(), store.GetNotificationChannelByUUIDParams{
			Uuid: u, TeamID: id.TeamID,
		})
		if err == nil {
			return c, true
		}
	}
	httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "notification channel not found")
	return store.NotificationChannel{}, false
}

// ListNotificationChannels implements GET /notification-channels.
func (a *API) ListNotificationChannels(w http.ResponseWriter, r *http.Request, params api.ListNotificationChannelsParams) {
	id, ok := a.require(w, r, auth.PermNotificationsRead)
	if !ok {
		return
	}
	limit, ok := pageLimit(w, r, params.Limit)
	if !ok {
		return
	}
	after, ok := afterID(w, r, params.Cursor)
	if !ok {
		return
	}
	rows, err := a.Store.ListNotificationChannelsPage(r.Context(), store.ListNotificationChannelsPageParams{
		TeamID: id.TeamID, AfterID: after, PageLimit: limit + 1,
	})
	if err != nil {
		a.internalError(w, r, "list notification channels", err)
		return
	}
	rows, cursor := nextCursor(rows, limit, func(c store.NotificationChannel) int64 { return c.ID })

	data := make([]api.NotificationChannel, 0, len(rows))
	for _, c := range rows {
		data = append(data, notificationChannelToAPI(c))
	}
	httpapi.WriteJSON(w, http.StatusOK, struct {
		Data       []api.NotificationChannel `json:"data"`
		NextCursor *string                   `json:"next_cursor"`
	}{data, cursor})
}

// channelConfig maps the contract's per-kind blocks onto the single decrypted
// config the sender speaks. Only the block the kind needs is ever read —
// ValidateConfig is what refuses an SMTP channel carrying a Slack URL.
func channelConfig(u *string, smtp *api.SmtpConfig, resend *api.ResendConfig, telegram *api.TelegramConfig, pushover *api.PushoverConfig) notify.Config {
	cfg := notify.Config{}
	if u != nil {
		cfg.URL = *u
	}
	if smtp != nil {
		c := notify.SMTPConfig{
			Host: smtp.Host, From: smtp.From, To: smtp.To,
		}
		if smtp.Port != nil {
			c.Port = *smtp.Port
		}
		if smtp.Username != nil {
			c.Username = *smtp.Username
		}
		if smtp.Password != nil {
			c.Password = *smtp.Password
		}
		if smtp.Encryption != nil {
			c.Encryption = string(*smtp.Encryption)
		}
		cfg.SMTP = &c
	}
	if resend != nil {
		cfg.Resend = &notify.ResendConfig{APIKey: resend.ApiKey, From: resend.From, To: resend.To}
	}
	if telegram != nil {
		c := notify.TelegramConfig{BotToken: telegram.BotToken, ChatID: telegram.ChatId}
		if telegram.TopicId != nil {
			c.TopicID = *telegram.TopicId
		}
		cfg.Telegram = &c
	}
	if pushover != nil {
		cfg.Pushover = &notify.PushoverConfig{Token: pushover.Token, UserKey: pushover.UserKey}
	}
	return cfg
}

// CreateNotificationChannel implements POST /notification-channels.
func (a *API) CreateNotificationChannel(w http.ResponseWriter, r *http.Request, params api.CreateNotificationChannelParams) {
	id, ok := a.require(w, r, auth.PermNotificationsManage)
	if !ok {
		return
	}
	var body api.NotificationChannelCreate
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid JSON body")
		return
	}
	if strings.TrimSpace(body.Name) == "" || len(body.Name) > 255 {
		httpapi.WriteValidationError(w, r, []api.ErrorDetail{{Field: ptr("name"), Code: ptr("required"), Message: "name must be non-empty and at most 255 characters"}})
		return
	}
	cfg := channelConfig(body.Url, body.Smtp, body.Resend, body.Telegram, body.Pushover)
	if err := notify.ValidateConfig(string(body.Kind), cfg); err != nil {
		httpapi.WriteValidationError(w, r, []api.ErrorDetail{{Field: ptr("kind"), Code: ptr("invalid"), Message: err.Error()}})
		return
	}

	u, err := pguuid.New()
	if err != nil {
		a.internalError(w, r, "create notification channel", err)
		return
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		a.internalError(w, r, "create notification channel", err)
		return
	}
	enc, err := a.Keyring.Encrypt("notification_channels", "config_enc", pguuid.String(u), raw)
	if err != nil {
		a.internalError(w, r, "create notification channel", err)
		return
	}
	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	channel, err := a.Store.CreateNotificationChannel(r.Context(), store.CreateNotificationChannelParams{
		Uuid: u, TeamID: id.TeamID, Kind: store.NotificationChannelKind(body.Kind),
		Name: body.Name, ConfigEnc: enc, Enabled: enabled,
	})
	if err != nil {
		if isUniqueViolation(err) {
			httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, "a channel with this name already exists in this team")
			return
		}
		a.internalError(w, r, "create notification channel", err)
		return
	}
	a.recordAudit(r, id, "notification_channel.create", "notification_channel", channel.Uuid)
	w.Header().Set("ETag", etagFor(channel.Version))
	httpapi.WriteJSON(w, http.StatusCreated, notificationChannelToAPI(channel))
}

// GetNotificationChannel implements GET /notification-channels/{uuid}.
func (a *API) GetNotificationChannel(w http.ResponseWriter, r *http.Request, channelUuid api.ChannelUuid) {
	id, ok := a.require(w, r, auth.PermNotificationsRead)
	if !ok {
		return
	}
	channel, ok := a.resolveChannel(w, r, id, channelUuid)
	if !ok {
		return
	}
	w.Header().Set("ETag", etagFor(channel.Version))
	httpapi.WriteJSON(w, http.StatusOK, notificationChannelToAPI(channel))
}

// UpdateNotificationChannel implements PATCH /notification-channels/{uuid}.
func (a *API) UpdateNotificationChannel(w http.ResponseWriter, r *http.Request, channelUuid api.ChannelUuid, params api.UpdateNotificationChannelParams) {
	id, ok := a.require(w, r, auth.PermNotificationsManage)
	if !ok {
		return
	}
	channel, ok := a.resolveChannel(w, r, id, channelUuid)
	if !ok {
		return
	}
	expected, err := strconv.Atoi(strings.Trim(strings.TrimSpace(params.IfMatch), `"`))
	if err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid If-Match header")
		return
	}
	var body api.NotificationChannelUpdate
	if _, ok := decodePatch(w, r, &body); !ok {
		return
	}

	name, enc, enabled := channel.Name, channel.ConfigEnc, channel.Enabled
	if body.Name != nil {
		if strings.TrimSpace(*body.Name) == "" || len(*body.Name) > 255 {
			httpapi.WriteValidationError(w, r, []api.ErrorDetail{{Field: ptr("name"), Code: ptr("required"), Message: "name must be non-empty and at most 255 characters"}})
			return
		}
		name = *body.Name
	}
	// A PATCH that carries any part of the configuration replaces it whole: the
	// configuration of a channel is one credential, not a bag of fields, and a
	// half-updated SMTP relay is a channel that fails at the moment it is needed.
	if body.Url != nil || body.Smtp != nil || body.Resend != nil || body.Telegram != nil || body.Pushover != nil {
		cfg := channelConfig(body.Url, body.Smtp, body.Resend, body.Telegram, body.Pushover)
		if err := notify.ValidateConfig(string(channel.Kind), cfg); err != nil {
			httpapi.WriteValidationError(w, r, []api.ErrorDetail{{Field: ptr("config"), Code: ptr("invalid"), Message: err.Error()}})
			return
		}
		raw, err := json.Marshal(cfg)
		if err != nil {
			a.internalError(w, r, "update notification channel", err)
			return
		}
		if enc, err = a.Keyring.Encrypt("notification_channels", "config_enc", uuidString(channel.Uuid), raw); err != nil {
			a.internalError(w, r, "update notification channel", err)
			return
		}
	}
	if body.Enabled != nil {
		enabled = *body.Enabled
	}

	rows, err := a.Store.UpdateNotificationChannel(r.Context(), store.UpdateNotificationChannelParams{
		ID: channel.ID, Name: name, ConfigEnc: enc, Enabled: enabled, ExpectedVersion: int32(expected),
	})
	if err != nil {
		if isUniqueViolation(err) {
			httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, "a channel with this name already exists in this team")
			return
		}
		a.internalError(w, r, "update notification channel", err)
		return
	}
	if rows == 0 {
		writeVersionConflict(w, r, channel.Version)
		return
	}
	updated, err := a.Store.GetNotificationChannelByUUID(r.Context(), store.GetNotificationChannelByUUIDParams{
		Uuid: channel.Uuid, TeamID: id.TeamID,
	})
	if err != nil {
		a.internalError(w, r, "update notification channel", err)
		return
	}
	a.recordAudit(r, id, "notification_channel.update", "notification_channel", channel.Uuid)
	w.Header().Set("ETag", etagFor(updated.Version))
	httpapi.WriteJSON(w, http.StatusOK, notificationChannelToAPI(updated))
}

// DeleteNotificationChannel implements DELETE /notification-channels/{uuid}.
func (a *API) DeleteNotificationChannel(w http.ResponseWriter, r *http.Request, channelUuid api.ChannelUuid) {
	id, ok := a.require(w, r, auth.PermNotificationsManage)
	if !ok {
		return
	}
	channel, ok := a.resolveChannel(w, r, id, channelUuid)
	if !ok {
		return
	}
	if _, err := a.Store.DeleteNotificationChannel(r.Context(), channel.ID); err != nil {
		a.internalError(w, r, "delete notification channel", err)
		return
	}
	a.recordAuditNamed(r, id, "notification_channel.delete", "notification_channel", channel.Uuid, channel.Name)
	w.WriteHeader(http.StatusNoContent)
}

// TestNotificationChannel implements POST /notification-channels/{uuid}/test:
// a bad configuration must be visible now, not at the first outage.
func (a *API) TestNotificationChannel(w http.ResponseWriter, r *http.Request, channelUuid api.ChannelUuid) {
	id, ok := a.require(w, r, auth.PermNotificationsManage)
	if !ok {
		return
	}
	channel, ok := a.resolveChannel(w, r, id, channelUuid)
	if !ok {
		return
	}
	raw, err := a.Keyring.Decrypt("notification_channels", "config_enc", uuidString(channel.Uuid), channel.ConfigEnc)
	if err != nil {
		a.internalError(w, r, "test notification channel", err)
		return
	}
	var cfg notify.Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		a.internalError(w, r, "test notification channel", err)
		return
	}

	event := notify.Event{
		Type:       "notification.test.v1",
		Severity:   notify.SeverityInfo.String(),
		OccurredAt: time.Now().UTC(),
		TeamUUID:   id.TeamUUID,
		Payload:    map[string]any{"channel": channel.Name},
	}
	type result struct {
		Delivered bool    `json:"delivered"`
		Error     *string `json:"error"`
	}
	if err := notify.New().Send(r.Context(), string(channel.Kind), cfg, event); err != nil {
		// The error names the cause (a 404 from the provider, a DNS failure);
		// the URL that carries the token is never echoed back.
		httpapi.WriteJSON(w, http.StatusOK, result{Delivered: false, Error: ptr(err.Error())})
		return
	}
	a.recordAudit(r, id, "notification_channel.test", "notification_channel", channel.Uuid)
	httpapi.WriteJSON(w, http.StatusOK, result{Delivered: true})
}

// ListNotificationRules implements GET /notification-channels/{uuid}/rules.
func (a *API) ListNotificationRules(w http.ResponseWriter, r *http.Request, channelUuid api.ChannelUuid) {
	id, ok := a.require(w, r, auth.PermNotificationsRead)
	if !ok {
		return
	}
	channel, ok := a.resolveChannel(w, r, id, channelUuid)
	if !ok {
		return
	}
	rules, err := a.Store.ListNotificationRules(r.Context(), channel.ID)
	if err != nil {
		a.internalError(w, r, "list notification rules", err)
		return
	}
	data := make([]api.NotificationRule, 0, len(rules))
	for _, rule := range rules {
		data = append(data, a.notificationRuleToAPI(r, rule))
	}
	httpapi.WriteJSON(w, http.StatusOK, struct {
		Data []api.NotificationRule `json:"data"`
	}{data})
}

// CreateNotificationRule implements POST /notification-channels/{uuid}/rules.
func (a *API) CreateNotificationRule(w http.ResponseWriter, r *http.Request, channelUuid api.ChannelUuid) {
	id, ok := a.require(w, r, auth.PermNotificationsManage)
	if !ok {
		return
	}
	channel, ok := a.resolveChannel(w, r, id, channelUuid)
	if !ok {
		return
	}
	var body api.NotificationRuleCreate
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid JSON body")
		return
	}
	if strings.TrimSpace(body.EventType) == "" {
		httpapi.WriteValidationError(w, r, []api.ErrorDetail{{Field: ptr("event_type"), Code: ptr("required"), Message: "event_type is required"}})
		return
	}

	// A rule may narrow to a project or an environment of this team (INV-002).
	var projectID, environmentID *int64
	if body.ProjectUuid != nil && *body.ProjectUuid != "" {
		project, ok := a.resolveProject(w, r, id, *body.ProjectUuid)
		if !ok {
			return
		}
		projectID = ptr(project.ID)
		if body.EnvironmentUuid != nil && *body.EnvironmentUuid != "" {
			env, ok := a.resolveEnvironment(w, r, project, *body.EnvironmentUuid)
			if !ok {
				return
			}
			environmentID = ptr(env.ID)
		}
	} else if body.EnvironmentUuid != nil && *body.EnvironmentUuid != "" {
		httpapi.WriteValidationError(w, r, []api.ErrorDetail{{
			Field: ptr("project_uuid"), Code: ptr("required"),
			Message: "an environment-scoped rule must name its project",
		}})
		return
	}

	debounce := 0
	if body.DebounceSeconds != nil {
		if *body.DebounceSeconds < 0 {
			httpapi.WriteValidationError(w, r, []api.ErrorDetail{{Field: ptr("debounce_seconds"), Code: ptr("out_of_range"), Message: "debounce_seconds must be >= 0"}})
			return
		}
		debounce = *body.DebounceSeconds
	}
	var quietStart, quietEnd pgtype.Time
	if body.QuietHoursStart != nil && *body.QuietHoursStart != "" {
		t, valid := parseHHMM(*body.QuietHoursStart)
		if !valid {
			httpapi.WriteValidationError(w, r, []api.ErrorDetail{{Field: ptr("quiet_hours_start"), Code: ptr("invalid"), Message: "quiet hours must be HH:MM"}})
			return
		}
		quietStart = t
	}
	if body.QuietHoursEnd != nil && *body.QuietHoursEnd != "" {
		t, valid := parseHHMM(*body.QuietHoursEnd)
		if !valid {
			httpapi.WriteValidationError(w, r, []api.ErrorDetail{{Field: ptr("quiet_hours_end"), Code: ptr("invalid"), Message: "quiet hours must be HH:MM"}})
			return
		}
		quietEnd = t
	}
	// A half-open window would silence either everything or nothing.
	if quietStart.Valid != quietEnd.Valid {
		httpapi.WriteValidationError(w, r, []api.ErrorDetail{{
			Field: ptr("quiet_hours_end"), Code: ptr("invalid"),
			Message: "quiet hours need both a start and an end",
		}})
		return
	}

	u, err := pguuid.New()
	if err != nil {
		a.internalError(w, r, "create notification rule", err)
		return
	}
	severity := store.NotificationSeverityInfo
	if body.MinSeverity != nil {
		severity = store.NotificationSeverity(*body.MinSeverity)
	}
	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	digest := false
	if body.DigestEnabled != nil {
		digest = *body.DigestEnabled
	}
	digestInterval := 60
	if body.DigestIntervalMinutes != nil {
		if *body.DigestIntervalMinutes < 1 {
			httpapi.WriteValidationError(w, r, []api.ErrorDetail{{Field: ptr("digest_interval_minutes"), Code: ptr("out_of_range"), Message: "digest_interval_minutes must be >= 1"}})
			return
		}
		digestInterval = *body.DigestIntervalMinutes
	}

	rule, err := a.Store.CreateNotificationRule(r.Context(), store.CreateNotificationRuleParams{
		Uuid: u, ChannelID: channel.ID, EventType: body.EventType, Enabled: enabled,
		ProjectID: projectID, EnvironmentID: environmentID,
		MinSeverity: severity, DebounceSeconds: int32(debounce),
		QuietHoursStart: quietStart, QuietHoursEnd: quietEnd, DigestEnabled: digest,
		DigestIntervalMinutes: int32(digestInterval),
	})
	if err != nil {
		if isUniqueViolation(err) {
			httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, "this channel already has a rule for this event and scope")
			return
		}
		a.internalError(w, r, "create notification rule", err)
		return
	}
	a.recordAudit(r, id, "notification_rule.create", "notification_channel", channel.Uuid)
	httpapi.WriteJSON(w, http.StatusCreated, a.notificationRuleToAPI(r, rule))
}

// DeleteNotificationRule implements DELETE
// /notification-channels/{uuid}/rules/{rule_uuid}.
func (a *API) DeleteNotificationRule(w http.ResponseWriter, r *http.Request, channelUuid api.ChannelUuid, ruleUuid string) {
	id, ok := a.require(w, r, auth.PermNotificationsManage)
	if !ok {
		return
	}
	channel, ok := a.resolveChannel(w, r, id, channelUuid)
	if !ok {
		return
	}
	var u pgtype.UUID
	if err := u.Scan(ruleUuid); err != nil {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "notification rule not found")
		return
	}
	rule, err := a.Store.GetNotificationRuleByUUID(r.Context(), store.GetNotificationRuleByUUIDParams{
		Uuid: u, ChannelID: channel.ID,
	})
	if err != nil {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "notification rule not found")
		return
	}
	if _, err := a.Store.DeleteNotificationRule(r.Context(), rule.ID); err != nil {
		a.internalError(w, r, "delete notification rule", err)
		return
	}
	a.recordAudit(r, id, "notification_rule.delete", "notification_channel", channel.Uuid)
	w.WriteHeader(http.StatusNoContent)
}
