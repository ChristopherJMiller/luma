package attachments

import (
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/ChristopherJMiller/luma/core/internal/protocol"
	"github.com/ChristopherJMiller/luma/core/internal/store"
)

var (
	// The fence's name is an attribute value; it can't close the quote or
	// the tag.
	fenceName = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	// The fence's content can't close the fence or open markup.
	fenceText = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
)

// Blocks renders attachments as turn.send content blocks (C2): an image as
// a base64 image block, a PDF as a base64 document block, and text fenced
// as data, `<luma-attachment name="…">` with the name and content escaped.
func Blocks(atts []store.Attachment) ([]protocol.ContentBlock, error) {
	out := make([]protocol.ContentBlock, 0, len(atts))
	for _, a := range atts {
		switch a.MediaType {
		case "image/jpeg", "image/png", "image/gif", "image/webp":
			out = append(out, protocol.ContentBlock{Type: "image", Source: base64Source(a)})
		case "application/pdf":
			out = append(out, protocol.ContentBlock{Type: "document", Source: base64Source(a)})
		case "text/plain":
			out = append(out, protocol.ContentBlock{Type: "text", Text: fence(a.Filename, a.Data)})
		default:
			return nil, fmt.Errorf("attachments: %s (%s) has no content block", a.ID, a.MediaType)
		}
	}
	return out, nil
}

func base64Source(a store.Attachment) *protocol.Base64Source {
	return &protocol.Base64Source{Type: "base64", MediaType: a.MediaType, Data: base64.StdEncoding.EncodeToString(a.Data)}
}

func fence(name string, text []byte) string {
	return `<luma-attachment name="` + fenceName.Replace(name) + "\">\n" + fenceText.Replace(string(text)) + "\n</luma-attachment>"
}
