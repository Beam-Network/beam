package action

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts"
)

const defaultMaximumActionBytes int64 = 64 << 20

type ArtifactCache struct {
	root                      string
	client                    *http.Client
	maximumBytes              int64
	requirePublisherSignature bool
	trustedPublisherKeys      map[string]struct{}
	allowedHosts              []string
	mu                        sync.Mutex
	locks                     map[string]*sync.Mutex
}

func NewArtifactCache(root string, client *http.Client, maximumBytes int64, requirePublisherSignature bool, allowedHosts, trustedPublisherKeys []string) (*ArtifactCache, error) {
	if root == "" {
		return nil, errors.New("action cache root is required")
	}
	if maximumBytes <= 0 {
		maximumBytes = defaultMaximumActionBytes
	}
	client = restrictedHTTPClient(client, allowedHosts)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	trusted := make(map[string]struct{}, len(trustedPublisherKeys))
	for _, encoded := range trustedPublisherKeys {
		key, err := base64.RawURLEncoding.DecodeString(encoded)
		if err != nil || len(key) != ed25519.PublicKeySize {
			return nil, errors.New("trusted action publisher key must be base64url Ed25519")
		}
		trusted[encoded] = struct{}{}
	}
	if requirePublisherSignature && len(trusted) == 0 {
		return nil, errors.New("at least one trusted action publisher key is required")
	}
	return &ArtifactCache{root: root, client: client, maximumBytes: maximumBytes,
		requirePublisherSignature: requirePublisherSignature, allowedHosts: append([]string(nil), allowedHosts...),
		trustedPublisherKeys: trusted, locks: make(map[string]*sync.Mutex)}, nil
}

func (c *ArtifactCache) Ensure(ctx context.Context, action contracts.ActionExecution) (string, error) {
	artifact := action.RegistryArtifact
	if artifact == nil || artifact.URL == "" {
		return "", errors.New("registry artifact download capability is required")
	}
	digest, err := normalizedSHA256(action.ArtifactSHA256)
	if err != nil {
		return "", err
	}
	if err := validateCapabilityEndpoint(artifact.URL, c.allowedHosts); err != nil {
		return "", fmt.Errorf("registry artifact: %w", err)
	}
	if !boundedHeaders(artifact.Headers) {
		return "", errors.New("registry artifact headers are invalid or too large")
	}
	if artifact.SizeBytes < 0 || artifact.SizeBytes > c.maximumBytes {
		return "", errors.New("registry artifact exceeds the Worker cache limit")
	}
	lock := c.digestLock(digest)
	lock.Lock()
	defer lock.Unlock()

	cacheDirectory := filepath.Join(c.root, "sha256", digest)
	if err := os.MkdirAll(cacheDirectory, 0o755); err != nil {
		return "", err
	}
	artifactPath := filepath.Join(cacheDirectory, "artifact")
	if err := verifyFileSHA256(artifactPath, digest); err != nil {
		if err := c.downloadWithRetry(ctx, artifactPath, digest, *artifact); err != nil {
			return "", err
		}
	}
	if err := verifyPublisherSignature(digest, *artifact, c.requirePublisherSignature, c.trustedPublisherKeys); err != nil {
		return "", err
	}
	if isArchiveMediaType(artifact.MediaType) {
		return c.ensureExtracted(artifactPath, cacheDirectory, action.Entrypoint, artifact.MediaType)
	}
	return artifactPath, nil
}

func (c *ArtifactCache) downloadWithRetry(ctx context.Context, destination, digest string, artifact contracts.ActionRegistryArtifact) error {
	var lastError error
	for attempt := 1; attempt <= 3; attempt++ {
		if err := c.download(ctx, destination, digest, artifact); err == nil {
			return nil
		} else {
			lastError = err
		}
		if attempt == 3 {
			break
		}
		timer := time.NewTimer(time.Duration(attempt) * 50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return lastError
}

func (c *ArtifactCache) download(ctx context.Context, destination, digest string, artifact contracts.ActionRegistryArtifact) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, artifact.URL, nil)
	if err != nil {
		return err
	}
	for name, value := range artifact.Headers {
		request.Header.Set(name, value)
	}
	response, err := c.client.Do(request)
	if err != nil {
		return fmt.Errorf("download action artifact: %w", err)
	}
	defer response.Body.Close()
	if err := validateCapabilityEndpoint(response.Request.URL.String(), c.allowedHosts); err != nil {
		return fmt.Errorf("registry redirect: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("action registry returned HTTP %d", response.StatusCode)
	}
	if response.ContentLength > c.maximumBytes {
		return errors.New("action registry response exceeds the Worker cache limit")
	}
	directory := filepath.Dir(destination)
	temporary, err := os.CreateTemp(directory, ".artifact-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o444); err != nil {
		temporary.Close()
		return err
	}
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(temporary, hash), io.LimitReader(response.Body, c.maximumBytes+1))
	if copyErr != nil {
		temporary.Close()
		return copyErr
	}
	if written > c.maximumBytes {
		temporary.Close()
		return errors.New("action artifact exceeds the Worker cache limit")
	}
	if artifact.SizeBytes > 0 && written != artifact.SizeBytes {
		temporary.Close()
		return fmt.Errorf("action artifact size mismatch: got %d expected %d", written, artifact.SizeBytes)
	}
	actual := hex.EncodeToString(hash.Sum(nil))
	if actual != digest {
		temporary.Close()
		return fmt.Errorf("action artifact checksum mismatch: got %s", actual)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, destination); err != nil {
		return err
	}
	return syncDirectory(directory)
}

func (c *ArtifactCache) ensureExtracted(artifactPath, cacheDirectory, entrypoint, mediaType string) (string, error) {
	if entrypoint == "" {
		return "", errors.New("archive action artifact requires an entrypoint")
	}
	expanded := filepath.Join(cacheDirectory, "expanded")
	entrypointPath, err := safeJoin(expanded, entrypoint)
	if err != nil {
		return "", err
	}
	if info, statErr := os.Stat(entrypointPath); statErr == nil && info.Mode().IsRegular() {
		return entrypointPath, nil
	}
	temporary, err := os.MkdirTemp(cacheDirectory, ".expanded-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(temporary)
	if err := os.Chmod(temporary, 0o755); err != nil {
		return "", err
	}
	file, err := os.Open(artifactPath)
	if err != nil {
		return "", err
	}
	defer file.Close()
	var reader io.Reader = file
	if strings.Contains(strings.ToLower(mediaType), "gzip") || strings.Contains(strings.ToLower(mediaType), "tgz") {
		gzipReader, gzipErr := gzip.NewReader(file)
		if gzipErr != nil {
			return "", gzipErr
		}
		defer gzipReader.Close()
		reader = gzipReader
	}
	tarReader := tar.NewReader(reader)
	var total int64
	for {
		header, readErr := tarReader.Next()
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return "", readErr
		}
		target, joinErr := safeJoin(temporary, header.Name)
		if joinErr != nil {
			return "", joinErr
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return "", err
			}
		case tar.TypeReg, tar.TypeRegA:
			if header.Size < 0 || total+header.Size > c.maximumBytes {
				return "", errors.New("expanded action artifact exceeds the Worker cache limit")
			}
			total += header.Size
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return "", err
			}
			output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o555)
			if err != nil {
				return "", err
			}
			_, copyErr := io.CopyN(output, tarReader, header.Size)
			closeErr := output.Close()
			if copyErr != nil {
				return "", copyErr
			}
			if closeErr != nil {
				return "", closeErr
			}
		default:
			return "", errors.New("action archives cannot contain links or special files")
		}
	}
	if err := os.Rename(temporary, expanded); err != nil {
		if _, statErr := os.Stat(expanded); statErr != nil {
			return "", err
		}
	}
	entrypointPath, err = safeJoin(expanded, entrypoint)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(entrypointPath)
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("action archive entrypoint is unavailable")
	}
	return entrypointPath, syncDirectory(cacheDirectory)
}

func (c *ArtifactCache) digestLock(digest string) *sync.Mutex {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.locks[digest] == nil {
		c.locks[digest] = &sync.Mutex{}
	}
	return c.locks[digest]
}

func normalizedSHA256(expected string) (string, error) {
	expected = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(expected)), "sha256:")
	decoded, err := hex.DecodeString(expected)
	if err != nil || len(decoded) != sha256.Size {
		return "", errors.New("Studio artifact SHA-256 is invalid")
	}
	return expected, nil
}

func verifyFileSHA256(path, expected string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	if hex.EncodeToString(hash.Sum(nil)) != expected {
		return errors.New("cached action artifact checksum mismatch")
	}
	return nil
}

func verifyPublisherSignature(digest string, artifact contracts.ActionRegistryArtifact, required bool, trusted map[string]struct{}) error {
	if artifact.PublisherPublicKey == "" || artifact.PublisherSignature == "" {
		if required {
			return errors.New("action publisher signature is required by Worker policy")
		}
		return nil
	}
	publicKey, keyErr := base64.RawURLEncoding.DecodeString(artifact.PublisherPublicKey)
	signature, signatureErr := base64.RawURLEncoding.DecodeString(artifact.PublisherSignature)
	message := []byte("beam:studio-action:v1\x00sha256:" + digest)
	if keyErr != nil || signatureErr != nil || len(publicKey) != ed25519.PublicKeySize ||
		!ed25519.Verify(ed25519.PublicKey(publicKey), message, signature) {
		return errors.New("invalid action publisher signature")
	}
	if _, ok := trusted[artifact.PublisherPublicKey]; len(trusted) > 0 && !ok {
		return errors.New("action publisher key is not trusted by this Worker")
	}
	return nil
}

func isArchiveMediaType(mediaType string) bool {
	mediaType = strings.ToLower(mediaType)
	return strings.Contains(mediaType, "tar") || strings.Contains(mediaType, "gzip") || strings.Contains(mediaType, "tgz")
}

func safeJoin(root, relative string) (string, error) {
	if relative == "" || filepath.IsAbs(relative) {
		return "", errors.New("action path must be relative")
	}
	clean := filepath.Clean(relative)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("action path escapes its root")
	}
	target := filepath.Join(root, clean)
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("action path escapes its root")
	}
	return target, nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
