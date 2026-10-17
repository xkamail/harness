package harness

import (
	"errors"
	"testing"
)

var (
	pngBytes  = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	webpBytes = []byte("RIFF\x00\x00\x00\x00WEBPVP8 ")
	pdfBytes  = []byte("%PDF-1.7\n%\xe2\xe3\xcf\xd3\n")
)

func TestValidateParts(t *testing.T) {
	lim := DefaultAttachmentLimits
	cases := []struct {
		name  string
		parts []Part
		want  error
		mime  string
	}{
		{"png", []Part{{Type: PartImage, MIME: "image/png", Data: pngBytes}}, nil, "image/png"},
		{"webp, no declared mime", []Part{{Type: PartImage, Data: webpBytes}}, nil, "image/webp"},
		{"pdf", []Part{{Type: PartFile, MIME: "application/pdf", Name: "invoice.pdf", Data: pdfBytes}}, nil, MIMEPDF},
		{"lying mime", []Part{{Type: PartImage, MIME: "image/png", Data: pdfBytes}}, ErrUnsupportedAttachment, ""},
		{"pdf as image", []Part{{Type: PartImage, Data: pdfBytes}}, ErrUnsupportedAttachment, ""},
		{"text file", []Part{{Type: PartFile, Data: []byte("hello world")}}, ErrUnsupportedAttachment, ""},
		{"empty", []Part{{Type: PartImage}}, ErrEmptyAttachment, ""},
		{"control char name", []Part{{Type: PartImage, Name: "a\nb", Data: pngBytes}}, ErrUnsupportedAttachment, ""},
		{"too many", []Part{
			{Type: PartImage, Data: pngBytes}, {Type: PartImage, Data: pngBytes},
			{Type: PartImage, Data: pngBytes}, {Type: PartImage, Data: pngBytes},
			{Type: PartImage, Data: pngBytes},
		}, ErrTooManyAttachments, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateParts(c.parts, lim)
			if !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
			if c.want == nil && c.parts[0].MIME != c.mime {
				t.Fatalf("mime = %q, want %q", c.parts[0].MIME, c.mime)
			}
		})
	}

	big := make([]byte, lim.MaxImageSize+1)
	copy(big, pngBytes)
	if err := ValidateParts([]Part{{Type: PartImage, Data: big}}, lim); !errors.Is(err, ErrAttachmentTooLarge) {
		t.Fatalf("big image: err = %v", err)
	}
}
