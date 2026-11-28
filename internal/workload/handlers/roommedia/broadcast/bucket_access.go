package broadcast

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"slices"
	"strings"
	"time"

	mc "github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
)

type BucketAccess = mc.BucketAccess

type BucketSigner = mc.BucketSigner

type BucketCredential = mc.BucketCredential

func OpenRecordingBucket(access *BucketAccess, recording Recording, vod bool, fallback BucketConfig, leaseEnd time.Time) (Bucket, error) {
	prefix, methods := mc.BucketGrantFor(recording, vod)
	if !validPrefix(prefix) {
		return nil, errors.New("invalid recording bucket prefix")
	}
	var inner Bucket
	switch {
	case access != nil:
		if err := mc.CheckNodeBucketAccess(*access, recording, vod); err != nil {
			return nil, errors.New("bucket_access is not this node's least-privilege grant")
		}
		expires := int64(0)
		if access.Signer != nil {
			expires = access.Signer.ExpiresAtMS
		} else if access.Credential != nil {
			expires = access.Credential.ExpiresAtMS
		}
		if expires <= 0 || expires < leaseEnd.UnixMilli() {
			return nil, errors.New("bucket_access expires before the node's lease ends")
		}
		switch {
		case access.Signer != nil:
			inner = &signedBucket{access: *access, client: &http.Client{Transport: bucketTransport, Timeout: 2 * time.Minute}}
		default:
			credential := access.Credential
			bucket, err := OpenBucket(BucketConfig{URL: strings.TrimRight(credential.Endpoint, "/") + "/" + credential.Bucket,
				Region: credential.Region, AccessKeyID: credential.AccessKeyID, SecretAccessKey: credential.SecretAccessKey})
			if err != nil {
				return nil, err
			}
			if s3, ok := bucket.(*s3Bucket); ok {
				s3.session = credential.SessionToken
			}
			inner = bucket
		}
	default:
		bucket, err := OpenBucket(fallback)
		if err != nil {
			return nil, err
		}
		log.Printf("WARNING media recordings: no bucket_access from Core; using the Worker's BEAM_MEDIA_BUCKET_* " +
			"configuration, a local and development fallback only (production recordings use Core-issued access)")
		inner = bucket
	}
	return &prefixBucket{prefix: prefix, methods: methods, inner: inner}, nil
}

func validPrefix(prefix string) bool {
	return mc.BucketGrantPrefixPattern.MatchString(prefix) && !strings.Contains(prefix, "..") && !strings.Contains(prefix, "//")
}

type prefixBucket struct {
	prefix  string
	methods []string
	inner   Bucket
}

func (b *prefixBucket) check(method, key string) error {
	if !slices.Contains(b.methods, method) {
		return fmt.Errorf("the recording grant does not allow %s", method)
	}
	if !strings.HasPrefix(key, b.prefix) || len(key) == len(b.prefix) || strings.Contains(key, "..") || strings.Contains(key, "//") {
		return errors.New("bucket key outside the recording prefix")
	}
	return nil
}

func (b *prefixBucket) Put(ctx context.Context, key string, body []byte, contentType string) error {
	if err := b.check(http.MethodPut, key); err != nil {
		return err
	}
	if limit, ok := mc.BucketObjectMaxBytes(key); !ok || int64(len(body)) > limit {
		return fmt.Errorf("recording object %s is not an allowed object or exceeds its size bound", key[strings.LastIndex(key, "/")+1:])
	}
	return b.inner.Put(ctx, key, body, contentType)
}

func (b *prefixBucket) Get(ctx context.Context, key string) ([]byte, error) {
	if err := b.check(http.MethodGet, key); err != nil {
		return nil, err
	}
	return b.inner.Get(ctx, key)
}

type signedBucket struct {
	access BucketAccess
	client *http.Client
}

func (b *signedBucket) sign(ctx context.Context, method, key, contentType string, length int64) (mc.BucketSignResponse, error) {
	var signed mc.BucketSignResponse
	request := mc.BucketSignRequest{Method: method, Key: key}
	if contentType != "" {
		request.ContentType = &contentType
	}
	if method == http.MethodPut {
		request.ContentLength = &length
	}
	if !mc.ValidBucketSignRequest(request, b.access) {
		return signed, fmt.Errorf("bucket signer: %s %s is outside the grant", method, key)
	}
	body, err := mc.MarshalWire(request)
	if err != nil {
		return signed, err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, b.access.Signer.URL, bytes.NewReader(body))
	if err != nil {
		return signed, err
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Authorization", "Bearer "+b.access.Signer.Token)
	response, err := b.client.Do(httpRequest)
	if err != nil {
		return signed, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return signed, fmt.Errorf("bucket signer: HTTP %d", response.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&signed); err != nil {
		return signed, err
	}
	if !mc.ValidBucketSignResponse(signed) {
		return signed, errors.New("bucket signer returned an invalid answer")
	}
	return signed, nil
}

func (b *signedBucket) do(ctx context.Context, method, key, contentType string, body []byte) (*http.Response, error) {
	signed, err := b.sign(ctx, method, key, contentType, int64(len(body)))
	if err != nil {
		return nil, err
	}
	var reader io.Reader
	if method == http.MethodPut {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, signed.URL, reader)
	if err != nil {
		return nil, err
	}
	for name, value := range signed.Headers {
		if strings.EqualFold(name, "Content-Length") || strings.EqualFold(name, "Host") {
			continue
		}
		request.Header.Set(name, value)
	}
	if method == http.MethodPut {
		request.ContentLength = int64(len(body))
		if contentType != "" && request.Header.Get("Content-Type") == "" {
			request.Header.Set("Content-Type", contentType)
		}
	}
	return b.client.Do(request)
}

func (b *signedBucket) Put(ctx context.Context, key string, body []byte, contentType string) error {
	response, err := b.do(ctx, http.MethodPut, key, contentType, body)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<16))
	if response.StatusCode/100 != 2 {
		return fmt.Errorf("bucket put: HTTP %d", response.StatusCode)
	}
	return nil
}

func (b *signedBucket) Get(ctx context.Context, key string) ([]byte, error) {
	response, err := b.do(ctx, http.MethodGet, key, "", nil)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return nil, ErrObjectNotFound
	}
	if response.StatusCode/100 != 2 {
		return nil, fmt.Errorf("bucket get: HTTP %d", response.StatusCode)
	}
	return readLimited(response.Body, maxBucketObject)
}

const maxBucketObject = 256 << 20

func readLimited(body io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("object larger than the allowed size")
	}
	return data, nil
}

func ReadLimited(body io.Reader, limit int64) ([]byte, error) { return readLimited(body, limit) }
