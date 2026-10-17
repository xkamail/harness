package harness

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"slices"
	"strings"
	"unicode"
)

// Attachments are images and PDFs only, and every model an agent lists is
// assumed to read both. Bytes live in storage (pgharness); events and
// history carry an Attachment reference, and the loader fills Part.Data when
// the conversation is sent to a model.

// MIMEPDF is the only file type a PartFile may carry.
const MIMEPDF = "application/pdf"

// ImageMIMEs are the image types a PartImage may carry.
var ImageMIMEs = []string{"image/png", "image/jpeg", "image/webp", "image/gif"}

// Attachment is a stored image or PDF, referenced from a user message.
type Attachment struct {
	ID   string   `json:"attachmentId"`
	Type PartType `json:"type"` // PartImage or PartFile
	MIME string   `json:"mime"`
	Name string   `json:"name,omitempty"`
	Size int      `json:"size"`
	// SHA256 is the hex digest of the bytes, for dedupe and integrity.
	SHA256 string `json:"sha256"`
}

// Part returns a's reference part, without bytes.
func (a Attachment) Part() Part {
	return Part{Type: a.Type, MIME: a.MIME, Name: a.Name, AttachmentID: a.ID}
}

// AttachmentLimits bound one user message's attachments.
type AttachmentLimits struct {
	MaxCount     int
	MaxImageSize int
	MaxPDFSize   int
}

// DefaultAttachmentLimits match what OpenAI and Anthropic accept with room
// to spare.
var DefaultAttachmentLimits = AttachmentLimits{
	MaxCount:     4,
	MaxImageSize: 5 << 20,
	MaxPDFSize:   10 << 20,
}

// Attachment validation errors; callers map them to user-facing errors.
var (
	ErrTooManyAttachments    = errors.New("harness: too many attachments")
	ErrUnsupportedAttachment = errors.New("harness: unsupported attachment")
	ErrAttachmentTooLarge    = errors.New("harness: attachment too large")
	ErrEmptyAttachment       = errors.New("harness: empty attachment")
)

const maxNameLen = 255

// ValidateParts checks the image and PDF parts of a user message carrying
// bytes. The type is sniffed from the bytes; a declared MIME that disagrees
// is refused, and on success each part's MIME is set to the sniffed type.
// Text parts only need to be non-empty.
func ValidateParts(parts []Part, lim AttachmentLimits) error {
	n := 0
	for i := range parts {
		p := &parts[i]
		switch p.Type {
		case PartText:
			if p.Text == "" {
				return fmt.Errorf("%w: empty text part", ErrEmptyAttachment)
			}
			continue
		case PartImage, PartFile:
		default:
			return fmt.Errorf("%w: part type %q", ErrUnsupportedAttachment, p.Type)
		}
		n++
		if n > lim.MaxCount {
			return fmt.Errorf("%w: max %d", ErrTooManyAttachments, lim.MaxCount)
		}
		if len(p.Data) == 0 {
			return fmt.Errorf("%w: %s", ErrEmptyAttachment, partLabel(*p))
		}
		// The name is rendered into the prompt.
		if len(p.Name) > maxNameLen || strings.ContainsFunc(p.Name, unicode.IsControl) {
			return fmt.Errorf("%w: bad name", ErrUnsupportedAttachment)
		}
		sniffed, _, _ := mime.ParseMediaType(http.DetectContentType(p.Data))
		if p.MIME != "" {
			declared, _, err := mime.ParseMediaType(p.MIME)
			if err != nil || declared != sniffed {
				return fmt.Errorf("%w: %s: content is %s", ErrUnsupportedAttachment, partLabel(*p), sniffed)
			}
		}
		limit := lim.MaxImageSize
		switch {
		case p.Type == PartImage && slices.Contains(ImageMIMEs, sniffed):
		case p.Type == PartFile && sniffed == MIMEPDF:
			limit = lim.MaxPDFSize
		default:
			return fmt.Errorf("%w: %s: only images and PDF", ErrUnsupportedAttachment, partLabel(*p))
		}
		if len(p.Data) > limit {
			return fmt.Errorf("%w: %s: %d bytes, max %d", ErrAttachmentTooLarge, partLabel(*p), len(p.Data), limit)
		}
		p.MIME = sniffed
	}
	return nil
}

// Digest returns the hex SHA-256 of data.
func Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// OmittedPart is the text that replaces an attachment dropped from history
// to fit the byte budget, so the model knows it existed.
func OmittedPart(a Attachment) Part {
	return Part{
		Type: PartText,
		Text: fmt.Sprintf("[attachment omitted: %s (%s), sent earlier; ask the user to resend it if you need it]", a.Name, a.MIME),
	}
}

func partLabel(p Part) string {
	if p.Name != "" {
		return p.Name + " (" + p.MIME + ")"
	}
	return p.MIME
}
