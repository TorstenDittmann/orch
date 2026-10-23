// Package s3test is an in-memory S3 server for tests. It implements the
// requests the objectstore client sends and checks that each one is signed,
// either with an Authorization header or as a presigned URL.
package s3test

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Server holds the objects of one bucket.
type Server struct {
	*httptest.Server
	Bucket string

	mu      sync.Mutex
	objects map[string][]byte
	uploads map[string]map[int][]byte
	next    int
	// FailPuts makes every upload fail while it is set.
	FailPuts bool
}

// New starts a server for bucket. Close it when done.
func New(bucket string) *Server {
	server := &Server{Bucket: bucket, objects: make(map[string][]byte), uploads: make(map[string]map[int][]byte)}
	server.Server = httptest.NewServer(http.HandlerFunc(server.serve))
	return server
}

// Object returns a stored object.
func (s *Server) Object(key string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.objects[key]
	return value, ok
}

// Keys returns every stored key in order.
func (s *Server) Keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]string, 0, len(s.objects))
	for key := range s.objects {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// SetObject stores an object directly.
func (s *Server) SetObject(key string, value []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[key] = append([]byte(nil), value...)
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	signed := strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") || r.URL.Query().Get("X-Amz-Signature") != ""
	if !signed {
		writeError(w, http.StatusForbidden, "AccessDenied", "request is not signed")
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/")
	bucket, key, _ := strings.Cut(path, "/")
	if bucket != s.Bucket {
		writeError(w, http.StatusNotFound, "NoSuchBucket", "bucket does not exist")
		return
	}
	query := r.URL.Query()
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case key == "" && r.Method == http.MethodHead:
		w.Header().Set("X-Amz-Bucket-Region", "us-east-1")
	case key == "" && r.Method == http.MethodGet && query.Get("list-type") == "2":
		s.list(w, query.Get("prefix"))
	case r.Method == http.MethodPost && query.Has("uploads"):
		s.next++
		id := "upload-" + strconv.Itoa(s.next)
		s.uploads[id] = make(map[int][]byte)
		writeXML(w, struct {
			XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
			UploadId string
		}{UploadId: id})
	case r.Method == http.MethodPut && query.Get("uploadId") != "":
		parts, ok := s.uploads[query.Get("uploadId")]
		number, _ := strconv.Atoi(query.Get("partNumber"))
		if !ok || number < 1 {
			writeError(w, http.StatusNotFound, "NoSuchUpload", "upload does not exist")
			return
		}
		body, _ := io.ReadAll(r.Body)
		parts[number] = body
		w.Header().Set("ETag", fmt.Sprintf("\"etag-%d\"", number))
	case r.Method == http.MethodPost && query.Get("uploadId") != "":
		parts, ok := s.uploads[query.Get("uploadId")]
		if !ok {
			writeError(w, http.StatusNotFound, "NoSuchUpload", "upload does not exist")
			return
		}
		var complete struct {
			Part []struct {
				PartNumber int
				ETag       string
			}
		}
		if err := xml.NewDecoder(r.Body).Decode(&complete); err != nil {
			writeError(w, http.StatusBadRequest, "MalformedXML", err.Error())
			return
		}
		var joined []byte
		for index, part := range complete.Part {
			if part.PartNumber != index+1 || part.ETag != fmt.Sprintf("\"etag-%d\"", part.PartNumber) || parts[part.PartNumber] == nil {
				writeError(w, http.StatusBadRequest, "InvalidPart", "part does not match")
				return
			}
			joined = append(joined, parts[part.PartNumber]...)
		}
		s.objects[key] = joined
		delete(s.uploads, query.Get("uploadId"))
		writeXML(w, struct {
			XMLName xml.Name `xml:"CompleteMultipartUploadResult"`
			Key     string
		}{Key: key})
	case r.Method == http.MethodDelete && query.Get("uploadId") != "":
		delete(s.uploads, query.Get("uploadId"))
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPut:
		if s.FailPuts {
			writeError(w, http.StatusInternalServerError, "InternalError", "uploads are failing")
			return
		}
		body, _ := io.ReadAll(r.Body)
		s.objects[key] = body
	case r.Method == http.MethodGet:
		value, ok := s.objects[key]
		if !ok {
			writeError(w, http.StatusNotFound, "NoSuchKey", "key does not exist")
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(value)))
		_, _ = w.Write(value)
	case r.Method == http.MethodDelete:
		delete(s.objects, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		writeError(w, http.StatusMethodNotAllowed, "MethodNotAllowed", r.Method)
	}
}

func (s *Server) list(w http.ResponseWriter, prefix string) {
	type content struct {
		Key          string
		Size         int64
		LastModified time.Time
	}
	result := struct {
		XMLName     xml.Name `xml:"ListBucketResult"`
		Contents    []content
		IsTruncated bool
	}{}
	for key, value := range s.objects {
		if strings.HasPrefix(key, prefix) {
			result.Contents = append(result.Contents, content{Key: key, Size: int64(len(value)), LastModified: time.Unix(0, 0).UTC()})
		}
	}
	sort.Slice(result.Contents, func(i, j int) bool { return result.Contents[i].Key < result.Contents[j].Key })
	writeXML(w, result)
}

func writeXML(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_ = xml.NewEncoder(w).Encode(struct {
		XMLName xml.Name `xml:"Error"`
		Code    string
		Message string
	}{Code: code, Message: message})
}
