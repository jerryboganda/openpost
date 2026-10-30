package adminagent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/gif"  // register GIF decoding so uploads are measured before use
	_ "image/jpeg" // register JPEG decoding so uploads are measured before use
	_ "image/png"  // register PNG decoding so uploads are measured before use
	"net/http"
	"path"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
	_ "golang.org/x/image/webp" // register WebP decoding so uploads are measured before use
)

const (
	MaxAttachmentBytes       = 4 << 20
	MaxAttachmentsPerMessage = 5
	maxAttachmentPixels      = 64_000_000
	maxAttachmentFilename    = 120
	pendingAttachmentTTL     = 24 * time.Hour
)

var (
	ErrAttachmentNotFound = errors.New("attachment not found")
	ErrInvalidAttachment  = errors.New("attachment must be a PNG, JPEG, GIF, or WebP image up to 4 MB")
	ErrTooManyAttachments = errors.New("a message can include up to 5 images")
)

// AttachmentInfo describes a stored image without its bytes.
type AttachmentInfo struct {
	ID          string    `json:"id"`
	Filename    string    `json:"filename"`
	ContentType string    `json:"content_type"`
	SizeBytes   int64     `json:"size_bytes"`
	Width       int       `json:"width"`
	Height      int       `json:"height"`
	CreatedAt   time.Time `json:"created_at"`
}

// StoredAttachment is an image with its bytes and owner.
type StoredAttachment struct {
	AttachmentInfo
	UploadedBy string
	Data       []byte
}

type attachmentRow struct {
	bun.BaseModel  `bun:"table:admin_agent_attachments"`
	ID             string    `bun:"id,pk"`
	ConversationID *string   `bun:"conversation_id"`
	MessageID      *string   `bun:"message_id"`
	UploadedBy     string    `bun:"uploaded_by_user_id,notnull"`
	Filename       string    `bun:"filename,notnull"`
	ContentType    string    `bun:"content_type,notnull"`
	SizeBytes      int64     `bun:"size_bytes,notnull"`
	Width          int       `bun:"width,notnull"`
	Height         int       `bun:"height,notnull"`
	SHA256         string    `bun:"sha256,notnull"`
	Data           []byte    `bun:"data,notnull"`
	CreatedAt      time.Time `bun:"created_at,notnull"`
}

func (r attachmentRow) info() AttachmentInfo {
	return AttachmentInfo{
		ID: r.ID, Filename: r.Filename, ContentType: r.ContentType,
		SizeBytes: r.SizeBytes, Width: r.Width, Height: r.Height, CreatedAt: r.CreatedAt,
	}
}

var attachmentInfoColumns = []string{
	"id", "message_id", "filename", "content_type", "size_bytes", "width", "height", "created_at",
}

// validateImage checks the bytes rather than the declared type. It returns the
// detected content type and the decoded dimensions, and rejects anything that
// is not a bounded raster image.
func validateImage(data []byte) (string, int, int, error) {
	if len(data) == 0 || len(data) > MaxAttachmentBytes {
		return "", 0, 0, ErrInvalidAttachment
	}
	contentType := http.DetectContentType(data)
	switch contentType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
	default:
		return "", 0, 0, ErrInvalidAttachment
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width <= 0 || config.Height <= 0 {
		return "", 0, 0, ErrInvalidAttachment
	}
	if int64(config.Width) > maxAttachmentPixels/int64(config.Height) {
		return "", 0, 0, ErrInvalidAttachment
	}
	return contentType, config.Width, config.Height, nil
}

func sanitizeAttachmentName(name string) string {
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '\\' || r == '/' {
			return -1
		}
		return r
	}, path.Base(strings.ReplaceAll(strings.TrimSpace(name), "\\", "/")))
	name = strings.TrimSpace(name)
	if name == "" || name == "." {
		return "image"
	}
	if runes := []rune(name); len(runes) > maxAttachmentFilename {
		name = string(runes[:maxAttachmentFilename])
	}
	return name
}

// SaveAttachment stores a validated image until a message claims it. Unclaimed
// images expire, so abandoned uploads do not accumulate.
func (s *Service) SaveAttachment(ctx context.Context, userID, filename string, data []byte) (*AttachmentInfo, error) {
	contentType, width, height, err := validateImage(data)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(data)
	row := &attachmentRow{
		ID:          uuid.NewString(),
		UploadedBy:  userID,
		Filename:    sanitizeAttachmentName(filename),
		ContentType: contentType,
		SizeBytes:   int64(len(data)),
		Width:       width,
		Height:      height,
		SHA256:      hex.EncodeToString(digest[:]),
		Data:        data,
		CreatedAt:   s.now().UTC(),
	}
	if _, err := s.db.NewInsert().Model(row).Exec(ctx); err != nil {
		return nil, fmt.Errorf("store admin agent attachment: %w", err)
	}
	cutoff := s.now().UTC().Add(-pendingAttachmentTTL)
	_, _ = s.db.NewDelete().Model((*attachmentRow)(nil)).
		Where("message_id IS NULL AND created_at < ?", cutoff).
		Exec(ctx)
	info := row.info()
	return &info, nil
}

// Attachment loads one stored image with its bytes.
func (s *Service) Attachment(ctx context.Context, id string) (*StoredAttachment, error) {
	row := new(attachmentRow)
	if err := s.db.NewSelect().Model(row).Where("id = ?", id).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrAttachmentNotFound
		}
		return nil, fmt.Errorf("load admin agent attachment: %w", err)
	}
	return &StoredAttachment{AttachmentInfo: row.info(), UploadedBy: row.UploadedBy, Data: row.Data}, nil
}

// bindAttachments attaches uploaded images to a message. Only unclaimed images
// uploaded by the same administrator can be bound.
func bindAttachments(ctx context.Context, tx bun.Tx, userID, conversationID, messageID string, ids []string) error {
	unique := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	if len(unique) == 0 {
		return nil
	}
	if len(unique) > MaxAttachmentsPerMessage {
		return ErrTooManyAttachments
	}
	var rows []attachmentRow
	if err := tx.NewSelect().Model(&rows).
		Column("id", "uploaded_by_user_id", "message_id").
		Where("id IN (?)", bun.List(unique)).
		Scan(ctx); err != nil {
		return fmt.Errorf("check admin agent attachments: %w", err)
	}
	if len(rows) != len(unique) {
		return ErrAttachmentNotFound
	}
	for _, row := range rows {
		if row.UploadedBy != userID || row.MessageID != nil {
			return ErrAttachmentNotFound
		}
	}
	if _, err := tx.NewUpdate().Model((*attachmentRow)(nil)).
		Set("conversation_id = ?", conversationID).
		Set("message_id = ?", messageID).
		Where("id IN (?)", bun.List(unique)).
		Exec(ctx); err != nil {
		return fmt.Errorf("bind admin agent attachments: %w", err)
	}
	return nil
}

// attachmentInfos groups attachment metadata by message, oldest first.
func (s *Service) attachmentInfos(ctx context.Context, messageIDs []string) (map[string][]AttachmentInfo, error) {
	grouped := make(map[string][]AttachmentInfo)
	if len(messageIDs) == 0 {
		return grouped, nil
	}
	var rows []attachmentRow
	if err := s.db.NewSelect().Model(&rows).
		Column(attachmentInfoColumns...).
		Where("message_id IN (?)", bun.List(messageIDs)).
		OrderExpr("created_at ASC, id ASC").
		Scan(ctx); err != nil {
		return nil, fmt.Errorf("list admin agent attachments: %w", err)
	}
	for _, row := range rows {
		if row.MessageID != nil {
			grouped[*row.MessageID] = append(grouped[*row.MessageID], row.info())
		}
	}
	return grouped, nil
}

// messageImages loads the bytes of the images bound to one message.
func (s *Service) messageImages(ctx context.Context, messageID string) ([]attachmentRow, error) {
	var rows []attachmentRow
	if err := s.db.NewSelect().Model(&rows).
		Where("message_id = ?", messageID).
		OrderExpr("created_at ASC, id ASC").
		Scan(ctx); err != nil {
		return nil, fmt.Errorf("load admin agent images: %w", err)
	}
	return rows, nil
}
