package main

import (
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	acp "github.com/gfx-labs/go-acp"
)

// maxAttachments bounds the bytes read for one prompt's files. The host
// refuses a frame over 64 MiB, and base64 grows the data by a third.
const maxAttachments = 40 << 20

// attachment is a file given with --file, read when the prompt is made so
// a queued prompt sends what the file held then.
type attachment struct {
	Path     string `json:"path"` // absolute
	MimeType string `json:"mime_type"`
	Data     []byte `json:"data"`
}

// promptInput is what one prompt sends: its text and its files.
type promptInput struct {
	Text  string       `json:"text"`
	Files []attachment `json:"files,omitempty"`
}

// readAttachments reads the --file paths.
func readAttachments(paths []string) ([]attachment, error) {
	var out []attachment
	total := 0
	for _, p := range paths {
		abs, err := filepath.Abs(p)
		if err != nil {
			return nil, err
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			return nil, fmt.Errorf("--file: %w", err)
		}
		total += len(data)
		if total > maxAttachments {
			return nil, fmt.Errorf("--file: the files add up to more than %d MiB", maxAttachments>>20)
		}
		out = append(out, attachment{Path: abs, MimeType: mimeType(abs, data), Data: data})
	}
	return out, nil
}

// mimeType is the type for a file: from its extension, else sniffed.
func mimeType(path string, data []byte) string {
	if t := mime.TypeByExtension(filepath.Ext(path)); t != "" {
		t, _, _ = strings.Cut(t, ";")
		return t
	}
	t, _, _ := strings.Cut(http.DetectContentType(data), ";")
	return t
}

// isText reports whether a file can go in a text resource.
func (f attachment) isText() bool {
	return strings.HasPrefix(f.MimeType, "text/") || (utf8.Valid(f.Data) && !strings.HasPrefix(f.MimeType, "image/"))
}

func (f attachment) uri() string {
	return (&url.URL{Scheme: "file", Path: f.Path}).String()
}

// errNoImages is an image for an agent that does not take them.
var errNoImages = errors.New("the agent does not accept images (promptCapabilities.image is false)")

// contentBlocks is the session/prompt content for in, in the forms the
// agent's prompt capabilities allow: an image block for an image, the
// file embedded as a resource with embeddedContext, else a resource link,
// which every agent has to accept.
func contentBlocks(in promptInput, caps acp.PromptCapabilities) ([]acp.ContentBlock, error) {
	blocks := []acp.ContentBlock{{Text: &acp.ContentBlockText{Type: "text", Text: in.Text}}}
	for _, f := range in.Files {
		mt := f.MimeType
		uri := f.uri()
		switch {
		case strings.HasPrefix(mt, "image/"):
			if !caps.Image {
				return nil, fmt.Errorf("%s: %w", f.Path, errNoImages)
			}
			blocks = append(blocks, acp.ContentBlock{Image: &acp.ContentBlockImage{
				Type: "image", MimeType: mt, Data: base64.StdEncoding.EncodeToString(f.Data), Uri: &uri,
			}})
		case caps.EmbeddedContext && f.isText():
			blocks = append(blocks, acp.ContentBlock{Resource: &acp.ContentBlockResource{
				Type: "resource",
				Resource: acp.EmbeddedResourceResource{TextResourceContents: &acp.TextResourceContents{
					Uri: uri, MimeType: &mt, Text: string(f.Data),
				}},
			}})
		case caps.EmbeddedContext:
			blocks = append(blocks, acp.ContentBlock{Resource: &acp.ContentBlockResource{
				Type: "resource",
				Resource: acp.EmbeddedResourceResource{BlobResourceContents: &acp.BlobResourceContents{
					Uri: uri, MimeType: &mt, Blob: base64.StdEncoding.EncodeToString(f.Data),
				}},
			}})
		default:
			size := len(f.Data)
			blocks = append(blocks, acp.ContentBlock{ResourceLink: &acp.ContentBlockResourceLink{
				Type: "resource_link", Name: filepath.Base(f.Path), Uri: uri, MimeType: &mt, Size: &size,
			}})
		}
	}
	return blocks, nil
}
