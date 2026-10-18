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

// Attachments are images and PDFs only; every agent model is assumed to read
// both. Bytes live in storage; history carries references until the loader fills Part.Data.

const MIMEPDF = "application/pdf"

var ImageMIMEs = []string{"image/png", "image/jpeg", "image/webp", "image/gif"}

type Attachment struct {
	ID     string   `json:"attachmentId"`
	Type   PartType `json:"type"` // PartImage or PartFile
	MIME   string   `json:"mime"`
	Name   string   `json:"name,omitempty"`
	Size   int      `json:"size"`
	SHA256 string   `json:"sha256"` // hex
}

// Part returns a reference part without bytes.
func (a Attachment) Part() Part {
	return Part{Type: a.Type, MIME: a.MIME, Name: a.Name, AttachmentID: a.ID}
}

// AttachmentLimits apply per user message.
type AttachmentLimits struct {
	MaxCount     int
	MaxImageSize int
	MaxPDFSize   int
}

// DefaultAttachmentLimits fit OpenAI and Anthropic limits with room to spare.
var DefaultAttachmentLimits = AttachmentLimits{
	MaxCount:     4,
	MaxImageSize: 5 << 20,
	MaxPDFSize:   10 << 20,
}

var (
	ErrTooManyAttachments    = errors.New("harness: too many attachments")
	ErrUnsupportedAttachment = errors.New("harness: unsupported attachment")
	ErrAttachmentTooLarge    = errors.New("harness: attachment too large")
	ErrEmptyAttachment       = errors.New("harness: empty attachment")
)

const maxNameLen = 255

// ValidateParts sniffs each attachment's type from its bytes, rejects a
// disagreeing declared MIME, and sets MIME to the sniffed type.
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

func Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// OmittedPart replaces an attachment dropped from history to fit the byte budget.
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
