package transfer

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
	"github.com/Beam-Network/beam/internal/workload/storagehttp"
)

// SourceDigestHandler executes transfer.source_digest: one GET of the whole source
// object, streamed through SHA-1 and SHA-256 in a single pass and never buffered.
type SourceDigestHandler struct {
	client *http.Client
}

// NewSourceDigestHandler uses the public-address storage client unless a client is
// given. The assignment lease, not a client timeout, bounds the whole-object read.
func NewSourceDigestHandler(client *http.Client) *SourceDigestHandler {
	if client == nil {
		client = storagehttp.NewClient(0)
	}
	return &SourceDigestHandler{client: client}
}

func (h *SourceDigestHandler) Kind() domain.Kind { return domain.KindTransferSourceDigest }

func (h *SourceDigestHandler) Validate(spec domain.Spec) error {
	_, err := decodeSourceDigest(spec)
	return err
}

func decodeSourceDigest(spec domain.Spec) (contracts.SourceDigest, error) {
	var digest contracts.SourceDigest
	if err := json.Unmarshal(spec.Payload, &digest); err != nil {
		return digest, fmt.Errorf("decode source digest: %w", err)
	}
	return digest, digest.Validate()
}

// Execute reports the outputs sha1, sha256 and bytes on success, and the failure
// class as the output error and the returned error otherwise.
func (h *SourceDigestHandler) Execute(ctx context.Context, spec domain.Spec) (domain.Result, error) {
	digest, err := decodeSourceDigest(spec)
	if err != nil {
		return domain.Result{}, err
	}
	sha1Hash, sha256Hash := sha1.New(), sha256.New()
	read, failure := h.stream(ctx, digest, io.MultiWriter(sha1Hash, sha256Hash))
	if failure != "" {
		return domain.Result{BytesProcessed: read, Outputs: map[string]string{"error": failure}}, errors.New(failure)
	}
	return domain.Result{BytesProcessed: read, Outputs: map[string]string{
		"sha1":   hex.EncodeToString(sha1Hash.Sum(nil)),
		"sha256": hex.EncodeToString(sha256Hash.Sum(nil)),
		"bytes":  strconv.FormatInt(read, 10),
	}}, nil
}

// stream copies the source body into hashes and returns the bytes read and, on
// failure, its class. Redirects are never followed.
func (h *SourceDigestHandler) stream(ctx context.Context, digest contracts.SourceDigest, hashes io.Writer) (int64, string) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, digest.Source.URL, nil)
	if err != nil {
		return 0, classSourceReadFailed
	}
	copyHeaders(request.Header, digest.Source.Headers)
	// An explicit Accept-Encoding stops the transport from transparently decoding a
	// compressed response, so the digest covers the stored bytes.
	if request.Header.Get("Accept-Encoding") == "" {
		request.Header.Set("Accept-Encoding", "identity")
	}
	client := *h.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		return 0, sourceReadFailure(ctx)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return 0, classSourceReadFailed
	}
	if response.ContentLength >= 0 && response.ContentLength != digest.Size {
		return 0, classSourceLengthMismatch
	}
	// One byte past the size is enough to detect a longer object without reading it.
	read, err := io.Copy(hashes, io.LimitReader(response.Body, digest.Size+1))
	if err != nil {
		return read, sourceReadFailure(ctx)
	}
	if read != digest.Size {
		return read, classSourceLengthMismatch
	}
	return read, ""
}

func sourceReadFailure(ctx context.Context) string {
	if ctx.Err() != nil {
		return classDeliveryCancelled
	}
	return classSourceReadFailed
}
