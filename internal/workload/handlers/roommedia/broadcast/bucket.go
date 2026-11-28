package broadcast

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Beam-Network/beam/internal/worker/planeapi/shared"
)

var ErrObjectNotFound = errors.New("bucket object not found")

var ErrNoBucket = errors.New("the Worker has no media bucket configured")

var bucketTransport = func() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = shared.TransportDial(transport.DialContext)
	return transport
}()

type Bucket interface {
	Put(ctx context.Context, key string, body []byte, contentType string) error
	Get(ctx context.Context, key string) ([]byte, error)
}

func OpenBucket(config BucketConfig) (Bucket, error) {
	if strings.TrimSpace(config.URL) == "" {
		return nil, ErrNoBucket
	}
	parsed, err := url.Parse(config.URL)
	if err != nil {
		return nil, fmt.Errorf("invalid media bucket URL: %w", err)
	}
	switch parsed.Scheme {
	case "file":
		if parsed.Path == "" {
			return nil, errors.New("file bucket URL needs a path")
		}
		return &fileBucket{root: filepath.Clean(parsed.Path)}, nil
	case "https", "http":
		segments := strings.SplitN(strings.Trim(parsed.Path, "/"), "/", 2)
		if segments[0] == "" {
			return nil, errors.New("S3 bucket URL needs /<bucket>")
		}
		secret := config.SecretAccessKey
		if secret == "" && config.SecretKeyFile != "" {
			raw, err := os.ReadFile(config.SecretKeyFile)
			if err != nil {
				return nil, fmt.Errorf("media bucket secret: %w", err)
			}
			secret = strings.TrimSpace(string(raw))
		}
		if config.AccessKeyID == "" || secret == "" {
			return nil, errors.New("S3 media bucket needs an access key id and secret")
		}
		region := config.Region
		if region == "" {
			region = "auto"
		}
		endpoint := &url.URL{Scheme: parsed.Scheme, Host: parsed.Host}
		return &s3Bucket{endpoint: endpoint, bucket: segments[0], region: region, accessKey: config.AccessKeyID,
			secret: secret, client: &http.Client{Transport: bucketTransport, Timeout: 2 * time.Minute}}, nil
	default:
		return nil, errors.New("media bucket URL must be file://, https:// or http://")
	}
}

func cleanKey(key string) (string, error) {
	cleaned := path.Clean("/" + key)[1:]
	if cleaned == "" || cleaned != strings.TrimSuffix(key, "/") || strings.Contains(key, "..") {
		return "", errors.New("invalid bucket key")
	}
	return cleaned, nil
}

type fileBucket struct{ root string }

func (b *fileBucket) Put(_ context.Context, key string, body []byte, _ string) error {
	cleaned, err := cleanKey(key)
	if err != nil {
		return err
	}
	target := filepath.Join(b.root, filepath.FromSlash(cleaned))
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		return err
	}
	return writeFileAtomic(target, body, 0o640)
}

func (b *fileBucket) Get(_ context.Context, key string) ([]byte, error) {
	cleaned, err := cleanKey(key)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(b.root, filepath.FromSlash(cleaned)))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrObjectNotFound
	}
	return data, err
}

type s3Bucket struct {
	session   string
	endpoint  *url.URL
	bucket    string
	region    string
	accessKey string
	secret    string
	client    *http.Client
}

func (b *s3Bucket) objectURL(key string, query url.Values) *url.URL {
	target := *b.endpoint
	target.Path = "/" + b.bucket + "/" + key
	target.RawPath = "/" + b.bucket + "/" + s3Escape(key, false)
	if query != nil {
		target.RawQuery = canonicalQuery(query)
	}
	return &target
}

func (b *s3Bucket) do(ctx context.Context, method string, target *url.URL, body []byte, contentType string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, method, target.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	b.sign(request, body, time.Now().UTC())
	return b.client.Do(request)
}

func (b *s3Bucket) Put(ctx context.Context, key string, body []byte, contentType string) error {
	cleaned, err := cleanKey(key)
	if err != nil {
		return err
	}
	response, err := b.do(ctx, http.MethodPut, b.objectURL(cleaned, nil), body, contentType)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<16))
	if response.StatusCode/100 != 2 {
		return fmt.Errorf("bucket put %s: HTTP %d", cleaned, response.StatusCode)
	}
	return nil
}

func (b *s3Bucket) Get(ctx context.Context, key string) ([]byte, error) {
	cleaned, err := cleanKey(key)
	if err != nil {
		return nil, err
	}
	response, err := b.do(ctx, http.MethodGet, b.objectURL(cleaned, nil), nil, "")
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return nil, ErrObjectNotFound
	}
	if response.StatusCode/100 != 2 {
		return nil, fmt.Errorf("bucket get %s: HTTP %d", cleaned, response.StatusCode)
	}
	return readLimited(response.Body, maxBucketObject)
}

func (b *s3Bucket) sign(request *http.Request, body []byte, now time.Time) {
	payloadHash := sha256Hex(body)
	amzDate := now.Format("20060102T150405Z")
	day := now.Format("20060102")
	request.Header.Set("x-amz-date", amzDate)
	request.Header.Set("x-amz-content-sha256", payloadHash)
	request.Host = request.URL.Host
	headers := map[string]string{"host": request.URL.Host, "x-amz-content-sha256": payloadHash, "x-amz-date": amzDate}
	if b.session != "" {
		request.Header.Set("x-amz-security-token", b.session)
		headers["x-amz-security-token"] = b.session
	}
	if contentType := request.Header.Get("Content-Type"); contentType != "" {
		headers["content-type"] = contentType
	}
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)
	var canonicalHeaders strings.Builder
	for _, name := range names {
		canonicalHeaders.WriteString(name + ":" + strings.TrimSpace(headers[name]) + "\n")
	}
	signedHeaders := strings.Join(names, ";")
	canonicalPath := request.URL.EscapedPath()
	canonical := strings.Join([]string{request.Method, canonicalPath, request.URL.RawQuery, canonicalHeaders.String(),
		signedHeaders, payloadHash}, "\n")
	scope := day + "/" + b.region + "/s3/aws4_request"
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + sha256Hex([]byte(canonical))
	key := hmacSHA256([]byte("AWS4"+b.secret), day)
	key = hmacSHA256(key, b.region)
	key = hmacSHA256(key, "s3")
	key = hmacSHA256(key, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(key, toSign))
	request.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+b.accessKey+"/"+scope+
		", SignedHeaders="+signedHeaders+", Signature="+signature)
}

func hmacSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(data))
	return mac.Sum(nil)
}

func sha256Hex(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func canonicalQuery(query url.Values) string {
	keys := make([]string, 0, len(query))
	for key := range query {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var parts []string
	for _, key := range keys {
		for _, value := range query[key] {
			parts = append(parts, s3Escape(key, true)+"="+s3Escape(value, true))
		}
	}
	return strings.Join(parts, "&")
}

func s3Escape(value string, encodeSlash bool) string {
	var builder strings.Builder
	for _, b := range []byte(value) {
		switch {
		case b >= 'A' && b <= 'Z', b >= 'a' && b <= 'z', b >= '0' && b <= '9', b == '-', b == '_', b == '.', b == '~':
			builder.WriteByte(b)
		case b == '/' && !encodeSlash:
			builder.WriteByte(b)
		default:
			fmt.Fprintf(&builder, "%%%02X", b)
		}
	}
	return builder.String()
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, data, mode); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}
