package pgharness

import (
	"context"
	"errors"

	"github.com/acoshift/pgsql"
	"github.com/acoshift/pgsql/pgctx"
	"github.com/rs/xid"

	"github.com/xkamail/harness"
)

// ErrBlobNotFound is returned by Blobs.Get for a missing object.
var ErrBlobNotFound = errors.New("pgharness: blob not found")

// Blobs stores attachment bytes outside Postgres, such as in GCS or S3. Set
// it with WithBlobs; without one, bytes live in harness.attachments.data.
type Blobs interface {
	Put(ctx context.Context, key, mime string, data []byte) error
	// Get returns ErrBlobNotFound when the object is gone.
	Get(ctx context.Context, key string) ([]byte, error)
	DeletePrefix(ctx context.Context, prefix string) error
}

func blobPrefix(conversationID string) string { return "harness/" + conversationID + "/" }

// storeParts saves parts' attachments and returns parts with each
// attachment's bytes replaced by its AttachmentID. Text parts pass through.
func storeParts(ctx context.Context, conversationID string, parts []harness.Part) ([]harness.Part, error) {
	if len(parts) == 0 {
		return nil, nil
	}
	blobs := blobsFrom(ctx)
	out := make([]harness.Part, 0, len(parts))
	for _, p := range parts {
		if p.Type == harness.PartText {
			out = append(out, p)
			continue
		}
		a := harness.Attachment{
			ID:     xid.New().String(),
			Type:   p.Type,
			MIME:   p.MIME,
			Name:   p.Name,
			Size:   len(p.Data),
			SHA256: harness.Digest(p.Data),
		}
		data := p.Data
		if blobs != nil {
			if err := blobs.Put(ctx, blobPrefix(conversationID)+a.ID, a.MIME, p.Data); err != nil {
				return nil, err
			}
			data = nil
		}
		_, err := pgctx.Exec(ctx, `
			insert into harness.attachments (id, conversation_id, type, mime, name, size, sha256, data)
			values ($1, $2, $3, $4, $5, $6, $7, $8)
		`, a.ID, conversationID, a.Type, a.MIME, a.Name, a.Size, a.SHA256, data)
		if err != nil {
			return nil, err
		}
		out = append(out, a.Part())
	}
	return out, nil
}

// defaultAttachmentBudget bounds the attachment bytes one run loads.
const defaultAttachmentBudget = 10 << 20

// attachParts fills the Data of history's attachment parts, newest message
// first, while they fit in budget; the newest message's always load. Any
// other, and any whose blob is gone, becomes an omitted text part.
func attachParts(ctx context.Context, conversationID string, history []harness.Message, budget int) error {
	type stored struct {
		harness.Attachment
		data []byte
	}
	rows := make(map[string]stored)
	err := pgctx.Iter(ctx, func(scan pgsql.Scanner) error {
		var s stored
		if err := scan(&s.ID, &s.Type, &s.MIME, &s.Name, &s.Size, &s.SHA256, &s.data); err != nil {
			return err
		}
		rows[s.ID] = s
		return nil
	}, `
		select
			id,
			type,
			mime,
			name,
			size,
			sha256,
			data
		from harness.attachments
		where conversation_id = $1
	`, conversationID)
	if err != nil || len(rows) == 0 {
		return err
	}

	blobs := blobsFrom(ctx)
	newest := true
	for i := len(history) - 1; i >= 0; i-- {
		m := &history[i]
		if m.Role != harness.RoleUser || len(m.Parts) == 0 {
			continue
		}
		for j := range m.Parts {
			p := &m.Parts[j]
			s, ok := rows[p.AttachmentID]
			if p.AttachmentID == "" || !ok {
				continue
			}
			if !newest && s.Size > budget {
				*p = harness.OmittedPart(s.Attachment)
				continue
			}
			data := s.data
			if data == nil && blobs != nil {
				data, err = blobs.Get(ctx, blobPrefix(conversationID)+s.ID)
				if errors.Is(err, ErrBlobNotFound) {
					*p = harness.OmittedPart(s.Attachment)
					continue
				}
				if err != nil {
					return err
				}
			}
			if data == nil {
				*p = harness.OmittedPart(s.Attachment)
				continue
			}
			p.Data = data
			budget -= s.Size
		}
		newest = false
	}
	return nil
}
