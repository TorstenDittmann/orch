// Package objectstore is a small S3 client for off-host backups. It speaks
// the subset of the S3 API that every S3-compatible service implements
// (AWS, Cloudflare R2, Backblaze B2, Hetzner, MinIO) and signs requests with
// Signature Version 4, so it needs no SDK.
package objectstore

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	unsignedPayload = "UNSIGNED-PAYLOAD"
	defaultRegion   = "us-east-1"
	// MaxPresignExpiry is the longest a presigned URL can stay valid.
	MaxPresignExpiry = 7 * 24 * time.Hour
)

var (
	bucketPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	// ErrNotFound is returned for a missing object.
	ErrNotFound = errors.New("object was not found")
)

// Destination is a bucket, an optional key prefix inside it, and the
// credentials to reach it. An empty Endpoint means AWS S3.
type Destination struct {
	Bucket          string
	Prefix          string
	Endpoint        string
	Region          string
	AccessKeyID     string
	SecretAccessKey string
}

// ParseURL splits s3://bucket/prefix into its bucket and its prefix without
// leading or trailing slashes.
func ParseURL(raw string) (bucket, prefix string, err error) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(raw), "s3://")
	if !ok {
		return "", "", fmt.Errorf("destination must look like s3://bucket/prefix")
	}
	bucket, prefix, _ = strings.Cut(rest, "/")
	if !bucketPattern.MatchString(bucket) || strings.Contains(bucket, "..") {
		return "", "", fmt.Errorf("%q is not a valid bucket name", bucket)
	}
	prefix = strings.Trim(prefix, "/")
	if strings.ContainsAny(prefix, "?#\\") || strings.Contains(prefix, "//") || len(prefix) > 512 {
		return "", "", fmt.Errorf("%q is not a valid key prefix", prefix)
	}
	for _, part := range strings.Split(prefix, "/") {
		if part == "." || part == ".." {
			return "", "", fmt.Errorf("%q is not a valid key prefix", prefix)
		}
	}
	return bucket, prefix, nil
}

// NormalizeEndpoint turns host or https://host into https://host. AWS needs
// no endpoint, so an empty one stays empty.
func NormalizeEndpoint(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || strings.Trim(parsed.Path, "/") != "" {
		return "", fmt.Errorf("endpoint must be an https URL such as https://ACCOUNT.r2.cloudflarestorage.com")
	}
	return parsed.Scheme + "://" + parsed.Host, nil
}

// URL is the destination as s3://bucket/prefix.
func (d Destination) URL() string {
	if d.Prefix == "" {
		return "s3://" + d.Bucket
	}
	return "s3://" + d.Bucket + "/" + d.Prefix
}

// Key joins the destination prefix and name.
func (d Destination) Key(name string) string {
	if d.Prefix == "" {
		return name
	}
	return d.Prefix + "/" + name
}

// RegionFromEndpoint reads the signing region from the hostnames of services
// that put it there, so most users never have to name one.
func RegionFromEndpoint(endpoint string) string {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return ""
	}
	labels := strings.Split(strings.ToLower(parsed.Hostname()), ".")
	switch {
	case strings.HasSuffix(parsed.Hostname(), ".r2.cloudflarestorage.com"):
		return "auto"
	case len(labels) == 4 && labels[0] == "s3" && labels[2] == "backblazeb2":
		return labels[1]
	case len(labels) == 3 && labels[1] == "your-objectstorage":
		return labels[0]
	case len(labels) >= 4 && labels[0] == "s3" && labels[len(labels)-2] == "amazonaws":
		return labels[1]
	case len(labels) == 3 && labels[1] == "digitaloceanspaces":
		return labels[0]
	}
	return ""
}

// Client talks to one destination.
type Client struct {
	destination Destination
	http        *http.Client
	now         func() time.Time
}

// New returns a client for destination. A destination without a region
// signs for us-east-1, which AWS redirects from and most other services
// accept; call DetectRegion to find the bucket's real one.
func New(destination Destination, httpClient *http.Client) *Client {
	if destination.Region == "" {
		destination.Region = defaultRegion
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 0}
	}
	return &Client{destination: destination, http: httpClient, now: time.Now}
}

// Destination returns the destination with the region the client signs for.
func (c *Client) Destination() Destination { return c.destination }

// DetectRegion asks the bucket for its region. AWS answers in the
// x-amz-bucket-region header, even when the request is refused. Services
// that do not answer keep the current region.
func (c *Client) DetectRegion(ctx context.Context) (string, error) {
	request, err := c.request(ctx, http.MethodHead, "", nil, nil)
	if err != nil {
		return "", err
	}
	c.sign(request, unsignedPayload)
	noRedirects := *c.http
	noRedirects.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := noRedirects.Do(request)
	if err != nil {
		return "", err
	}
	_ = response.Body.Close()
	if region := response.Header.Get("X-Amz-Bucket-Region"); region != "" {
		c.destination.Region = region
	}
	return c.destination.Region, nil
}

// Object is one listed object.
type Object struct {
	Key          string
	Size         int64
	LastModified time.Time
}

// Put uploads body as key. size must be the exact body length.
func (c *Client) Put(ctx context.Context, key string, body io.Reader, size int64) error {
	request, err := c.request(ctx, http.MethodPut, key, nil, body)
	if err != nil {
		return err
	}
	request.ContentLength = size
	if size == 0 {
		request.Body = http.NoBody
	}
	c.sign(request, unsignedPayload)
	return c.do(request, nil)
}

// Get downloads key. The caller closes the body.
func (c *Client) Get(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	request, err := c.request(ctx, http.MethodGet, key, nil, nil)
	if err != nil {
		return nil, 0, err
	}
	c.sign(request, unsignedPayload)
	response, err := c.http.Do(request)
	if err != nil {
		return nil, 0, err
	}
	if response.StatusCode != http.StatusOK {
		defer response.Body.Close()
		return nil, 0, responseError(response)
	}
	return response.Body, response.ContentLength, nil
}

// Delete removes key. A missing key is not an error.
func (c *Client) Delete(ctx context.Context, key string) error {
	request, err := c.request(ctx, http.MethodDelete, key, nil, nil)
	if err != nil {
		return err
	}
	c.sign(request, unsignedPayload)
	err = c.do(request, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// List returns every object whose key starts with prefix, in key order.
func (c *Client) List(ctx context.Context, prefix string) ([]Object, error) {
	var objects []Object
	token := ""
	for {
		query := url.Values{"list-type": {"2"}, "prefix": {prefix}}
		if token != "" {
			query.Set("continuation-token", token)
		}
		request, err := c.request(ctx, http.MethodGet, "", query, nil)
		if err != nil {
			return nil, err
		}
		c.sign(request, emptyHash)
		var page struct {
			Contents []struct {
				Key          string
				Size         int64
				LastModified time.Time
			}
			IsTruncated           bool
			NextContinuationToken string
		}
		if err := c.do(request, &page); err != nil {
			return nil, err
		}
		for _, item := range page.Contents {
			objects = append(objects, Object{Key: item.Key, Size: item.Size, LastModified: item.LastModified})
		}
		if !page.IsTruncated || page.NextContinuationToken == "" {
			break
		}
		token = page.NextContinuationToken
	}
	sort.Slice(objects, func(i, j int) bool { return objects[i].Key < objects[j].Key })
	return objects, nil
}

// CreateMultipartUpload starts a multipart upload of key.
func (c *Client) CreateMultipartUpload(ctx context.Context, key string) (string, error) {
	request, err := c.request(ctx, http.MethodPost, key, url.Values{"uploads": {""}}, nil)
	if err != nil {
		return "", err
	}
	c.sign(request, emptyHash)
	var result struct{ UploadId string }
	if err := c.do(request, &result); err != nil {
		return "", err
	}
	if result.UploadId == "" {
		return "", errors.New("s3 returned no upload id")
	}
	return result.UploadId, nil
}

// CompleteMultipartUpload joins the uploaded parts, given by their ETags in
// part order.
func (c *Client) CompleteMultipartUpload(ctx context.Context, key, uploadID string, etags []string) error {
	var body bytes.Buffer
	body.WriteString("<CompleteMultipartUpload>")
	for index, etag := range etags {
		body.WriteString("<Part><PartNumber>" + strconv.Itoa(index+1) + "</PartNumber><ETag>")
		_ = xml.EscapeText(&body, []byte(etag))
		body.WriteString("</ETag></Part>")
	}
	body.WriteString("</CompleteMultipartUpload>")
	payload := body.Bytes()
	request, err := c.request(ctx, http.MethodPost, key, url.Values{"uploadId": {uploadID}}, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.ContentLength = int64(len(payload))
	c.sign(request, hashHex(payload))
	// S3 can answer 200 with an error document after it started streaming.
	var result struct {
		XMLName xml.Name
		Code    string
		Message string
	}
	if err := c.do(request, &result); err != nil {
		return err
	}
	if result.XMLName.Local == "Error" {
		return fmt.Errorf("s3: %s: %s", result.Code, result.Message)
	}
	return nil
}

// AbortMultipartUpload drops an unfinished multipart upload and its parts.
func (c *Client) AbortMultipartUpload(ctx context.Context, key, uploadID string) error {
	request, err := c.request(ctx, http.MethodDelete, key, url.Values{"uploadId": {uploadID}}, nil)
	if err != nil {
		return err
	}
	c.sign(request, unsignedPayload)
	return c.do(request, nil)
}

// PresignGet returns a URL that downloads key without credentials until it
// expires.
func (c *Client) PresignGet(key string, expires time.Duration) (string, error) {
	return c.presign(http.MethodGet, key, nil, expires)
}

// PresignPut returns a URL that uploads key without credentials.
func (c *Client) PresignPut(key string, expires time.Duration) (string, error) {
	return c.presign(http.MethodPut, key, nil, expires)
}

// PresignUploadPart returns a URL that uploads one part, numbered from 1, of
// a multipart upload.
func (c *Client) PresignUploadPart(key, uploadID string, part int, expires time.Duration) (string, error) {
	return c.presign(http.MethodPut, key, url.Values{"partNumber": {strconv.Itoa(part)}, "uploadId": {uploadID}}, expires)
}

func (c *Client) presign(method, key string, query url.Values, expires time.Duration) (string, error) {
	if expires <= 0 || expires > MaxPresignExpiry {
		return "", fmt.Errorf("presigned URLs expire after 1s to 7 days")
	}
	target, err := c.objectURL(key)
	if err != nil {
		return "", err
	}
	now := c.now().UTC()
	values := url.Values{}
	for name, items := range query {
		values[name] = items
	}
	values.Set("X-Amz-Algorithm", "AWS4-HMAC-SHA256")
	values.Set("X-Amz-Credential", c.destination.AccessKeyID+"/"+c.scope(now))
	values.Set("X-Amz-Date", now.Format("20060102T150405Z"))
	values.Set("X-Amz-Expires", strconv.Itoa(int(expires/time.Second)))
	values.Set("X-Amz-SignedHeaders", "host")
	canonical := strings.Join([]string{method, escapePath(target.Path), canonicalQuery(values),
		"host:" + target.Host + "\n", "host", unsignedPayload}, "\n")
	values.Set("X-Amz-Signature", c.signature(now, canonical))
	target.RawQuery = canonicalQuery(values)
	return target.String(), nil
}

// objectURL addresses key in the bucket: virtual-hosted on AWS, path style
// everywhere else, which every compatible service supports.
func (c *Client) objectURL(key string) (*url.URL, error) {
	d := c.destination
	bucketPath := "/" + d.Bucket
	if key != "" {
		bucketPath += "/" + key
	}
	if d.Endpoint == "" {
		host := "s3." + d.Region + ".amazonaws.com"
		if d.Region == defaultRegion {
			host = "s3.amazonaws.com"
		}
		if !strings.Contains(d.Bucket, ".") {
			return &url.URL{Scheme: "https", Host: d.Bucket + "." + host, Path: "/" + key}, nil
		}
		return &url.URL{Scheme: "https", Host: host, Path: bucketPath}, nil
	}
	base, err := url.Parse(d.Endpoint)
	if err != nil {
		return nil, err
	}
	return &url.URL{Scheme: base.Scheme, Host: base.Host, Path: bucketPath}, nil
}

func (c *Client) request(ctx context.Context, method, key string, query url.Values, body io.Reader) (*http.Request, error) {
	target, err := c.objectURL(key)
	if err != nil {
		return nil, err
	}
	target.RawQuery = canonicalQuery(query)
	request, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return nil, err
	}
	// Go would re-escape the path its own way; send exactly what was signed.
	request.URL.RawPath = escapePath(target.Path)
	return request, nil
}

func (c *Client) sign(request *http.Request, payloadHash string) {
	now := c.now().UTC()
	request.Header.Set("X-Amz-Date", now.Format("20060102T150405Z"))
	request.Header.Set("X-Amz-Content-Sha256", payloadHash)
	headers := []string{"host", "x-amz-content-sha256", "x-amz-date"}
	values := map[string]string{"host": request.URL.Host, "x-amz-content-sha256": payloadHash,
		"x-amz-date": request.Header.Get("X-Amz-Date")}
	var canonicalHeaders strings.Builder
	for _, name := range headers {
		canonicalHeaders.WriteString(name + ":" + values[name] + "\n")
	}
	signed := strings.Join(headers, ";")
	canonical := strings.Join([]string{request.Method, escapePath(request.URL.Path), canonicalQuery(request.URL.Query()),
		canonicalHeaders.String(), signed, payloadHash}, "\n")
	request.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+c.destination.AccessKeyID+"/"+c.scope(now)+
		", SignedHeaders="+signed+", Signature="+c.signature(now, canonical))
}

func (c *Client) scope(now time.Time) string {
	return now.Format("20060102") + "/" + c.destination.Region + "/s3/aws4_request"
}

func (c *Client) signature(now time.Time, canonicalRequest string) string {
	toSign := "AWS4-HMAC-SHA256\n" + now.Format("20060102T150405Z") + "\n" + c.scope(now) + "\n" + hashHex([]byte(canonicalRequest))
	key := hmacSHA256([]byte("AWS4"+c.destination.SecretAccessKey), now.Format("20060102"))
	key = hmacSHA256(key, c.destination.Region)
	key = hmacSHA256(key, "s3")
	key = hmacSHA256(key, "aws4_request")
	return hex.EncodeToString(hmacSHA256(key, toSign))
}

func (c *Client) do(request *http.Request, result any) error {
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return responseError(response)
	}
	if result == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
		return nil
	}
	if err := xml.NewDecoder(io.LimitReader(response.Body, 64<<20)).Decode(result); err != nil {
		return fmt.Errorf("decode s3 response: %w", err)
	}
	return nil
}

func responseError(response *http.Response) error {
	var document struct {
		Code    string
		Message string
	}
	body, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	_ = xml.Unmarshal(body, &document)
	if response.StatusCode == http.StatusNotFound && (document.Code == "" || document.Code == "NoSuchKey") {
		return ErrNotFound
	}
	if document.Code == "" {
		return fmt.Errorf("s3 answered %s", response.Status)
	}
	if document.Message == "" {
		return fmt.Errorf("s3: %s (%s)", document.Code, response.Status)
	}
	return fmt.Errorf("s3: %s: %s", document.Code, document.Message)
}

var emptyHash = hashHex(nil)

func hashHex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(data))
	return mac.Sum(nil)
}

// escape is S3's URI encoding: everything but unreserved characters is
// percent-encoded, and a slash too unless it separates path segments.
func escape(value string, keepSlash bool) string {
	var out strings.Builder
	for _, b := range []byte(value) {
		switch {
		case 'A' <= b && b <= 'Z', 'a' <= b && b <= 'z', '0' <= b && b <= '9', b == '-', b == '_', b == '.', b == '~':
			out.WriteByte(b)
		case b == '/' && keepSlash:
			out.WriteByte(b)
		default:
			fmt.Fprintf(&out, "%%%02X", b)
		}
	}
	return out.String()
}

func escapePath(path string) string { return escape(path, true) }

func canonicalQuery(values url.Values) string {
	if len(values) == 0 {
		return ""
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	var parts []string
	for _, name := range names {
		items := append([]string(nil), values[name]...)
		sort.Strings(items)
		for _, item := range items {
			parts = append(parts, escape(name, false)+"="+escape(item, false))
		}
	}
	return strings.Join(parts, "&")
}
