package image

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
)

// casRegistry is a minimal OCI distribution server: a content-addressable
// store with blob upload, manifest put/get, and tag resolution. Just enough
// for oras-go to push and pull against in tests.
type casRegistry struct {
	blobs     map[string][]byte // digest -> content
	manifests map[string][]byte // "<repo>@<digest>" -> bytes
	tags      map[string]string // "<repo>:<tag>" -> digest
	mtypes    map[string]string // "<repo>@<digest>" -> Content-Type
	srv       *httptest.Server
}

func newCASRegistry() *casRegistry {
	c := &casRegistry{
		blobs:     map[string][]byte{},
		manifests: map[string][]byte{},
		tags:      map[string]string{},
		mtypes:    map[string]string{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/", c.serve)
	c.srv = httptest.NewServer(mux)
	return c
}

func (c *casRegistry) close() { c.srv.Close() }

// refFor returns a pullable image reference for repo:tag on this server.
func (c *casRegistry) refFor(repo, tag string) string {
	host := strings.TrimPrefix(c.srv.URL, "http://")
	return host + "/" + repo + ":" + tag
}

var (
	blobUploadRe  = regexp.MustCompile(`^/v2/(.+)/blobs/uploads/$`)
	blobGetRe     = regexp.MustCompile(`^/v2/(.+)/blobs/(sha256:[0-9a-f]{64})$`)
	manifestPutRe = regexp.MustCompile(`^/v2/(.+)/manifests/([^/]+)$`)
	uploadsDoneRe = regexp.MustCompile(`^/v2/.+/blobs/uploads/[a-z0-9-]+$`)
)

func (c *casRegistry) serve(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case path == "/v2/":
		w.WriteHeader(http.StatusOK)
	case blobUploadRe.MatchString(path) && r.Method == http.MethodPost:
		w.Header().Set("Location", path+"test-upload-"+fmt.Sprint(len(c.blobs)))
		w.WriteHeader(http.StatusAccepted)
	case uploadsDoneRe.MatchString(path) && r.Method == http.MethodPut:
		digest := r.URL.Query().Get("digest")
		if digest == "" {
			http.Error(w, "missing digest", http.StatusBadRequest)
			return
		}
		body, _ := io.ReadAll(r.Body)
		c.blobs[digest] = body
		w.Header().Set("Docker-Content-Digest", "sha256:"+strings.TrimPrefix(digest, "sha256:"))
		w.WriteHeader(http.StatusCreated)
	case blobGetRe.MatchString(path):
		parts := blobGetRe.FindStringSubmatch(path)
		if b, ok := c.blobs[parts[2]]; ok {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Docker-Content-Digest", parts[2])
			w.Write(b)
			return
		}
		http.Error(w, "blob not found", http.StatusNotFound)
	case manifestPutRe.MatchString(path) && (r.Method == http.MethodPut || r.Method == http.MethodPost):
		parts := manifestPutRe.FindStringSubmatch(path)
		body, _ := io.ReadAll(r.Body)
		sum := sha256Hex(body)
		c.manifests[parts[1]+"@sha256:"+sum] = body
		c.mtypes[parts[1]+"@sha256:"+sum] = r.Header.Get("Content-Type")
		if !strings.Contains(parts[2], ":") && !strings.HasPrefix(parts[2], "sha256:") {
			c.tags[parts[1]+":"+parts[2]] = sum
		}
		w.Header().Set("Docker-Content-Digest", "sha256:"+sum)
		w.WriteHeader(http.StatusCreated)
	case manifestPutRe.MatchString(path) && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		parts := manifestPutRe.FindStringSubmatch(path)
		ref := parts[2]
		if !strings.HasPrefix(ref, "sha256:") {
			tagged := c.tags[parts[1]+":"+ref]
			if tagged == "" {
				http.Error(w, "manifest not found", http.StatusNotFound)
				return
			}
			ref = tagged
		}
		key := parts[1] + "@sha256:" + ref
		if m, ok := c.manifests[key]; ok {
			w.Header().Set("Content-Type", c.mtypes[key])
			w.Header().Set("Docker-Content-Digest", "sha256:"+sha256Hex(m))
			w.Write(m)
			return
		}
		http.Error(w, "manifest not found", http.StatusNotFound)
	default:
		http.Error(w, "unsupported: "+path+" "+r.Method, http.StatusNotImplemented)
	}
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
