package adminagent

import (
	"bytes"
	"context"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/gif"
	"image/jpeg"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// pngWithDeclaredSize builds a PNG header that claims dimensions without any
// pixel data, which is how a decompression bomb announces itself.
func pngWithDeclaredSize(width, height uint32) []byte {
	var buffer bytes.Buffer
	buffer.Write([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a})
	header := make([]byte, 13)
	binary.BigEndian.PutUint32(header[0:], width)
	binary.BigEndian.PutUint32(header[4:], height)
	header[8], header[9] = 8, 2
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(header)))
	buffer.Write(length[:])
	chunk := append([]byte("IHDR"), header...)
	buffer.Write(chunk)
	var sum [4]byte
	binary.BigEndian.PutUint32(sum[:], crc32.ChecksumIEEE(chunk))
	buffer.Write(sum[:])
	return buffer.Bytes()
}

func TestAttachmentValidationAcceptsBoundedRasterImagesOnly(t *testing.T) {
	canvas := image.NewRGBA(image.Rect(0, 0, 16, 9))
	var jpegBytes, gifBytes bytes.Buffer
	require.NoError(t, jpeg.Encode(&jpegBytes, canvas, nil))
	require.NoError(t, gif.Encode(&gifBytes, canvas, nil))

	accepted := []struct {
		name          string
		data          []byte
		contentType   string
		width, height int
	}{
		{"png", testPNG(t), "image/png", 8, 6},
		{"jpeg", jpegBytes.Bytes(), "image/jpeg", 16, 9},
		{"gif", gifBytes.Bytes(), "image/gif", 16, 9},
	}
	for _, candidate := range accepted {
		contentType, width, height, err := validateImage(candidate.data)
		require.NoError(t, err, candidate.name)
		require.Equal(t, candidate.contentType, contentType)
		require.Equal(t, candidate.width, width)
		require.Equal(t, candidate.height, height)
	}

	rejected := map[string][]byte{
		"empty":         nil,
		"text":          []byte("hello"),
		"html":          []byte("<html><script>alert(1)</script></html>"),
		"svg":           []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`),
		"truncated png": testPNG(t)[:20],
		"oversized":     append(testPNG(t), make([]byte, MaxAttachmentBytes)...),
		"pixel bomb":    pngWithDeclaredSize(100_000, 100_000),
		"zero size":     pngWithDeclaredSize(0, 10),
	}
	for name, data := range rejected {
		_, _, _, err := validateImage(data)
		require.ErrorIs(t, err, ErrInvalidAttachment, name)
	}
}

func TestAttachmentFilenamesAreReducedToASafeBaseName(t *testing.T) {
	cases := map[string]string{
		"shot.png":                    "shot.png",
		"../../etc/passwd":            "passwd",
		`C:\Users\me\Screen Shot.png`: "Screen Shot.png",
		"a\x00b\nc.png":               "abc.png",
		"":                            "image",
		"   ":                         "image",
		".":                           "image",
		strings.Repeat("x", 300):      strings.Repeat("x", maxAttachmentFilename),
	}
	for input, expected := range cases {
		require.Equal(t, expected, sanitizeAttachmentName(input), "%q", input)
	}
}

func TestAttachmentsBindOnlyToTheirOwnersMessagesAndOnlyOnce(t *testing.T) {
	ctx := context.Background()
	service, chat := newAgentService(t, answer("Seen."))
	conversation := newAgentConversation(t, service)
	mine, err := service.SaveAttachment(ctx, "admin", "mine.png", testPNG(t))
	require.NoError(t, err)
	theirs, err := service.SaveAttachment(ctx, "other", "theirs.png", testPNG(t))
	require.NoError(t, err)

	start := func(ids ...string) (*AgentRun, error) {
		return service.StartRun(ctx, StartRunInput{ConversationID: conversation.ID, UserID: "admin", Prompt: "Look", AttachmentIDs: ids})
	}
	_, err = start(mine.ID, theirs.ID)
	require.ErrorIs(t, err, ErrAttachmentNotFound, "another administrator's image cannot be used")
	_, err = start("missing")
	require.ErrorIs(t, err, ErrAttachmentNotFound)
	_, err = start("1", "2", "3", "4", "5", "6")
	require.ErrorIs(t, err, ErrTooManyAttachments)

	view, err := service.GetConversation(ctx, conversation.ID)
	require.NoError(t, err)
	require.Empty(t, view.Messages, "a rejected message leaves no message behind")
	require.Empty(t, view.Runs, "and no run")

	run, err := start(mine.ID, mine.ID)
	require.NoError(t, err, "a repeated id counts once")
	require.Equal(t, string(RunSucceeded), waitForTerminalRun(t, service, run.ID).Status)
	require.Len(t, chat.snapshot()[0].Messages[1].Images, 1)

	_, err = start(mine.ID)
	require.ErrorIs(t, err, ErrAttachmentNotFound, "an image belongs to one message")
}

func TestUnclaimedUploadsExpire(t *testing.T) {
	ctx := context.Background()
	service, _ := newAgentService(t)
	start := time.Date(2026, 10, 30, 8, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return start }
	first, err := service.SaveAttachment(ctx, "admin", "old.png", testPNG(t))
	require.NoError(t, err)

	service.now = func() time.Time { return start.Add(pendingAttachmentTTL + time.Hour) }
	second, err := service.SaveAttachment(ctx, "admin", "new.png", testPNG(t))
	require.NoError(t, err)

	_, err = service.Attachment(ctx, first.ID)
	require.ErrorIs(t, err, ErrAttachmentNotFound, "abandoned uploads do not accumulate")
	stored, err := service.Attachment(ctx, second.ID)
	require.NoError(t, err)
	require.Equal(t, "admin", stored.UploadedBy)
	require.Equal(t, testPNG(t), stored.Data)
}
