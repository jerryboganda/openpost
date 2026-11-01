package handlers

import (
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/openpost/backend/internal/models"
	"github.com/uptrace/bun"
)

const agentReadPageSize = 25

type agentPost struct {
	ID             string           `json:"id"`
	Title          string           `json:"title"`
	SourceText     string           `json:"source_text"`
	Status         string           `json:"status"`
	ContentProfile string           `json:"content_profile"`
	ScheduledAt    time.Time        `json:"scheduled_at,omitempty"`
	ActualRunAt    time.Time        `json:"actual_run_at,omitempty"`
	CreatedAt      time.Time        `json:"created_at"`
	Renditions     []agentRendition `json:"renditions"`
}

type agentRendition struct {
	ID               string    `json:"id"`
	SocialAccountID  string    `json:"social_account_id"`
	Platform         string    `json:"platform"`
	Profile          string    `json:"profile"`
	Status           string    `json:"status"`
	ScheduleOverride time.Time `json:"schedule_override,omitempty"`
	ExternalURL      string    `json:"external_url,omitempty"`
}

func (h *AdminAgentHandler) readPosts(c echo.Context) error {
	workspaceID, err := h.authorizeRead(c, "posts")
	if err != nil {
		return err
	}
	cursor, err := agentReadCursor(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	var posts []models.Publication
	query := h.db.NewSelect().Model(&posts).
		Column("id", "title", "source_text", "status", "content_profile", "scheduled_at", "actual_run_at", "created_at").
		Where("workspace_id = ?", workspaceID).
		OrderExpr("created_at DESC, id DESC").Limit(agentReadPageSize + 1)
	if cursor != nil {
		query = query.Where("(created_at < ? OR (created_at = ? AND id < ?))", cursor.Timestamp, cursor.Timestamp, cursor.ID)
	}
	if err := query.Scan(ctx); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "posts unavailable")
	}
	hasMore := len(posts) > agentReadPageSize
	if hasMore {
		posts = posts[:agentReadPageSize]
	}
	items := make([]agentPost, 0, len(posts))
	for _, post := range posts {
		item := agentPost{ID: post.ID, Title: truncateAgentField(post.Title), SourceText: truncateAgentField(post.SourceText), Status: post.Status, ContentProfile: post.ContentProfile, ScheduledAt: post.ScheduledAt, ActualRunAt: post.ActualRunAt, CreatedAt: post.CreatedAt, Renditions: []agentRendition{}}
		var renditions []models.Rendition
		if err := h.db.NewSelect().Model(&renditions).
			Column("id", "social_account_id", "platform", "profile", "status", "schedule_override", "external_url").
			Where("publication_id = ?", post.ID).OrderExpr("position ASC, id ASC").Limit(21).Scan(ctx); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, "post destinations unavailable")
		}
		for _, rendition := range renditions[:min(len(renditions), 20)] {
			item.Renditions = append(item.Renditions, agentRendition{ID: rendition.ID, SocialAccountID: rendition.SocialAccountID, Platform: rendition.Platform, Profile: rendition.Profile, Status: rendition.Status, ScheduleOverride: rendition.ScheduleOverride, ExternalURL: truncateAgentField(rendition.ExternalURL)})
		}
		items = append(items, item)
	}
	next := ""
	if hasMore && len(posts) > 0 {
		last := posts[len(posts)-1]
		next = encodeTimestampIDCursor(last.CreatedAt, last.ID)
	}
	return c.JSON(http.StatusOK, map[string]any{"workspace_id": workspaceID, "posts": items, "next_cursor": next, "has_more": hasMore, "renditions_limit_per_post": 20})
}

func (h *AdminAgentHandler) readSchedules(c echo.Context) error {
	workspaceID, err := h.authorizeRead(c, "schedules")
	if err != nil {
		return err
	}
	cursor, err := agentReadCursor(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	var posts []models.Publication
	query := h.db.NewSelect().Model(&posts).
		Column("id", "title", "status", "scheduled_at", "actual_run_at", "created_at").
		Where("workspace_id = ? AND scheduled_at IS NOT NULL AND status IN (?)", workspaceID, bun.In([]string{models.PublicationStatusReady, models.PublicationStatusScheduled, models.PublicationStatusPublishing})).
		OrderExpr("scheduled_at ASC, id ASC").Limit(agentReadPageSize + 1)
	if cursor != nil {
		query = query.Where("(scheduled_at > ? OR (scheduled_at = ? AND id > ?))", cursor.Timestamp, cursor.Timestamp, cursor.ID)
	}
	if err := query.Scan(ctx); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "schedules unavailable")
	}
	hasMore := len(posts) > agentReadPageSize
	if hasMore {
		posts = posts[:agentReadPageSize]
	}
	items := make([]map[string]any, 0, len(posts))
	for _, post := range posts {
		items = append(items, map[string]any{"id": post.ID, "title": truncateAgentField(post.Title), "status": post.Status, "scheduled_at": post.ScheduledAt})
	}
	var slots []models.PostingSchedule
	if err := h.db.NewSelect().Model(&slots).Column("id", "utc_hour", "utc_minute", "day_of_week", "label", "is_active").Where("workspace_id = ?", workspaceID).OrderExpr("day_of_week ASC, utc_hour ASC, utc_minute ASC, id ASC").Limit(101).Scan(ctx); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "posting slots unavailable")
	}
	slotsMore := len(slots) > 100
	if slotsMore {
		slots = slots[:100]
	}
	type slot struct {
		ID        string `json:"id"`
		DayOfWeek int    `json:"day_of_week"`
		UTCHour   int    `json:"utc_hour"`
		UTCMinute int    `json:"utc_minute"`
		Label     string `json:"label"`
		IsActive  bool   `json:"is_active"`
	}
	resultSlots := make([]slot, 0, len(slots))
	for _, s := range slots {
		resultSlots = append(resultSlots, slot{s.ID, s.DayOfWeek, s.UTCHour, s.UTCMinute, truncateAgentField(s.Label), s.IsActive})
	}
	next := ""
	if hasMore && len(posts) > 0 {
		last := posts[len(posts)-1]
		next = encodeTimestampIDCursor(last.ScheduledAt, last.ID)
	}
	return c.JSON(http.StatusOK, map[string]any{"workspace_id": workspaceID, "scheduled_posts": items, "next_cursor": next, "has_more": hasMore, "recurring_slots": resultSlots, "slots_has_more": slotsMore, "slot_time_basis": "UTC"})
}

func agentReadCursor(c echo.Context) (*timestampIDCursor, error) {
	value := c.QueryParam("cursor")
	if value == "" {
		return nil, nil
	}
	if len(value) > 160 || strings.Contains(value, "\n") {
		return nil, echo.NewHTTPError(http.StatusBadRequest, "invalid cursor")
	}
	cursor, err := parseTimestampIDCursor(value)
	if err != nil {
		return nil, echo.NewHTTPError(http.StatusBadRequest, "invalid cursor")
	}
	return &cursor, nil
}

func truncateAgentField(value string) string {
	const maxRunes = 1000
	runes := []rune(value)
	if len(runes) > maxRunes {
		return string(runes[:maxRunes])
	}
	return value
}
