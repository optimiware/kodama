package s3

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/optimiware/kodama/internal/artifacts"
)

// PresignUpload signs a PUT for exactly one staging key. The signature binds
// the method, key, content type, content length and SHA-256 checksum, so the
// instruction can only upload the reserved declaration, and it expires at the
// requested instant (capped by MaxTransfer).
func (s *Store) PresignUpload(ctx context.Context, spec artifacts.UploadSpec) (artifacts.Transfer, error) {
	key, err := s.objectKey(spec.Key)
	if err != nil {
		return artifacts.Transfer{}, err
	}
	if err := spec.Digest.Validate(); err != nil || spec.Size < 0 || spec.MediaType == "" {
		return artifacts.Transfer{}, fmt.Errorf("%w: upload declaration", artifacts.ErrInvalid)
	}
	life, err := s.lifetime(spec.ExpiresAt)
	if err != nil {
		return artifacts.Transfer{}, err
	}
	out, err := s.presign.PresignPutObject(ctx, &awss3.PutObjectInput{
		Bucket:         aws.String(s.cfg.Bucket),
		Key:            aws.String(key),
		ContentType:    aws.String(spec.MediaType),
		ContentLength:  aws.Int64(spec.Size),
		ChecksumSHA256: aws.String(spec.Digest.Base64()),
	}, awss3.WithPresignExpires(life))
	if err != nil {
		return artifacts.Transfer{}, fmt.Errorf("%w: sign upload", artifacts.ErrUnavailable)
	}
	if err := s.checkScheme(out.URL); err != nil {
		return artifacts.Transfer{}, fmt.Errorf("%w: %v", artifacts.ErrUnavailable, err)
	}
	return artifacts.Transfer{Method: out.Method, URL: out.URL, Headers: transferHeaders(out.SignedHeader), ExpiresAt: spec.ExpiresAt.UTC()}, nil
}

// PresignDownload signs a GET for exactly one sealed key.
func (s *Store) PresignDownload(ctx context.Context, spec artifacts.DownloadSpec) (artifacts.Transfer, error) {
	key, err := s.objectKey(spec.Key)
	if err != nil {
		return artifacts.Transfer{}, err
	}
	life, err := s.lifetime(spec.ExpiresAt)
	if err != nil {
		return artifacts.Transfer{}, err
	}
	in := &awss3.GetObjectInput{Bucket: aws.String(s.cfg.Bucket), Key: aws.String(key),
		ResponseContentDisposition: aws.String("attachment"), ResponseCacheControl: aws.String("private, no-store")}
	if spec.MediaType != "" {
		in.ResponseContentType = aws.String(spec.MediaType)
	}
	out, err := s.presign.PresignGetObject(ctx, in, awss3.WithPresignExpires(life))
	if err != nil {
		return artifacts.Transfer{}, fmt.Errorf("%w: sign download", artifacts.ErrUnavailable)
	}
	if err := s.checkScheme(out.URL); err != nil {
		return artifacts.Transfer{}, fmt.Errorf("%w: %v", artifacts.ErrUnavailable, err)
	}
	return artifacts.Transfer{Method: out.Method, URL: out.URL, Headers: transferHeaders(out.SignedHeader), ExpiresAt: spec.ExpiresAt.UTC()}, nil
}

// transferHeaders lists the headers the holder must send. The host header is
// implied by the URL.
func transferHeaders(h http.Header) map[string]string {
	out := map[string]string{}
	for k, v := range h {
		if len(v) == 0 {
			continue
		}
		switch http.CanonicalHeaderKey(k) {
		case "Host":
			continue
		}
		out[http.CanonicalHeaderKey(k)] = v[0]
	}
	return out
}

// Open reads an object. A missing object is artifacts.ErrObjectNotFound.
func (s *Store) Open(ctx context.Context, key string) (artifacts.Object, error) {
	k, err := s.objectKey(key)
	if err != nil {
		return artifacts.Object{}, err
	}
	out, err := s.cfg.Client.GetObject(ctx, &awss3.GetObjectInput{Bucket: aws.String(s.cfg.Bucket), Key: aws.String(k)})
	if err != nil {
		if isNotFound(err) {
			return artifacts.Object{}, artifacts.ErrObjectNotFound
		}
		return artifacts.Object{}, fmt.Errorf("%w: read object", artifacts.ErrUnavailable)
	}
	size := int64(-1)
	if out.ContentLength != nil {
		size = *out.ContentLength
	}
	return artifacts.Object{Body: out.Body, Size: size}, nil
}

// Put writes verified bytes with their SHA-256 so the store rejects corruption
// in transit.
func (s *Store) Put(ctx context.Context, spec artifacts.PutSpec) error {
	k, err := s.objectKey(spec.Key)
	if err != nil {
		return err
	}
	if _, err := spec.Body.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("%w: rewind body", artifacts.ErrUnavailable)
	}
	in := &awss3.PutObjectInput{
		Bucket: aws.String(s.cfg.Bucket), Key: aws.String(k), Body: spec.Body, ContentLength: aws.Int64(spec.Size),
		ContentType: aws.String(spec.MediaType), ChecksumAlgorithm: types.ChecksumAlgorithmSha256, ChecksumSHA256: aws.String(spec.Digest.Base64()),
		// Sealed destinations are unique; refuse to replace anything.
		IfNoneMatch: aws.String("*"),
	}
	if _, err := s.cfg.Client.PutObject(ctx, in); err != nil {
		return fmt.Errorf("%w: write object: %v", artifacts.ErrUnavailable, errClass(err))
	}
	return nil
}

func errClass(err error) string { return strconv.Quote(fmt.Sprintf("%T", err)) }
