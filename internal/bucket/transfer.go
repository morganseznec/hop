package bucket

import (
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"log"
	"mime"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	tmtypes "github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/morganseznec/hop/internal/atomicfile"
)

// The transfer manager of the SDK splits a large object: an upload of PartSize parts
// once the file reaches MultipartThreshold, a download of PartSize ranges, Concurrency
// of them at a time. These are its defaults, named here for the documentation.
const (
	PartSize           = 8 * 1024 * 1024
	MultipartThreshold = 16 * 1024 * 1024
	Concurrency        = 5
)

// failTimeout bounds what the transfer manager does once a transfer has failed or was
// interrupted, with a context of its own: aborting a multipart upload, so that its
// parts do not stay in the bucket, billed and invisible.
const failTimeout = 10 * time.Second

// Progress receives the number of bytes transferred so far and the size of the object.
type Progress func(done, total int64)

// Info describes an object.
type Info struct {
	Size         int64
	Modified     time.Time
	StorageClass string
	etag         string
	sum          string
	alg          algorithm
	hasSum       bool
}

// Result is what a transfer moved, and how it was checked: Checksum names the
// algorithm compared on both sides, and is empty when S3 had no checksum to compare.
type Result struct {
	Size     int64
	Checksum string
}

func manager(c *s3.Client, optFns ...func(*transfermanager.Options)) *transfermanager.Client {
	return transfermanager.New(c, append([]func(*transfermanager.Options){func(o *transfermanager.Options) {
		o.PartSizeBytes = PartSize
		o.MultipartUploadThreshold = MultipartThreshold
		o.Concurrency = Concurrency
		o.FailTimeout = failTimeout
	}}, optFns...)...)
}

// Head reads the size, the date and the checksum of an object. cfg must be set to the
// region of the bucket. A missing object is an ErrNotFound.
func Head(ctx context.Context, cfg aws.Config, loc Location) (*Info, error) {
	return head(ctx, s3.NewFromConfig(cfg), loc)
}

func head(ctx context.Context, c *s3.Client, loc Location) (*Info, error) {
	out, err := c.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(loc.Bucket), Key: aws.String(loc.Key),
		// Without it, S3 leaves the checksums out of the answer.
		ChecksumMode: types.ChecksumModeEnabled,
	})
	if err != nil {
		return nil, explain(err, "reading", loc)
	}
	info := &Info{
		Size: aws.ToInt64(out.ContentLength), Modified: aws.ToTime(out.LastModified),
		StorageClass: string(out.StorageClass), etag: aws.ToString(out.ETag),
	}
	info.alg, info.sum, info.hasSum = stored(out)
	return info, nil
}

// Download writes the object loc to the file local, through a temporary file next to
// it that is renamed once the transfer is checked: an interrupted download leaves
// nothing under that name. A new file is readable by its owner only; an existing one is
// replaced and keeps its mode.
//
// A large object comes as ranges fetched in parallel, all of the version read first
// (S3 refuses a range once the object has changed). The size is compared with the one
// S3 announced, and the content with the checksum S3 holds for the whole object when it
// has one (see stored): Result.Checksum then names it.
func Download(ctx context.Context, cfg aws.Config, loc Location, local string, progress Progress) (Result, error) {
	c := s3.NewFromConfig(cfg)
	info, err := head(ctx, c, loc)
	if err != nil {
		return Result{}, err
	}
	tmp, err := atomicfile.Create(local, ".hop-s3-*")
	if err != nil {
		return Result{}, err
	}
	defer tmp.Discard()

	// An empty object has no range to ask for.
	if info.Size > 0 {
		w := &countingWriter{w: tmp, total: info.Size, progress: progress}
		in := &transfermanager.DownloadObjectInput{Bucket: aws.String(loc.Bucket), Key: aws.String(loc.Key), WriterAt: w}
		if info.etag != "" {
			in.IfMatch = aws.String(info.etag)
		}
		_, err := manager(c, func(o *transfermanager.Options) { o.GetObjectType = tmtypes.GetObjectRanges }).DownloadObject(ctx, in)
		if err != nil {
			if cerr := ctx.Err(); cerr != nil {
				return Result{}, cerr
			}
			if code, _, status := codeAndStatus(err); status == 412 || code == "PreconditionFailed" {
				return Result{}, fmt.Errorf("%s changed while it was downloaded: nothing was written, run the command again", loc)
			}
			return Result{}, explain(err, "reading", loc)
		}
	}
	st, err := tmp.Stat()
	if err != nil {
		return Result{}, err
	}
	if st.Size() != info.Size {
		return Result{}, fmt.Errorf("download incomplete: %d bytes received, %s holds %d", st.Size(), loc, info.Size)
	}
	res := Result{Size: info.Size}
	if info.hasSum {
		h := info.alg.new()
		if _, err := tmp.Seek(0, io.SeekStart); err != nil {
			return Result{}, err
		}
		if _, err := io.Copy(h, tmp); err != nil {
			return Result{}, err
		}
		if got := encode(h); got != info.sum {
			return Result{}, fmt.Errorf("checksum mismatch after download (%s): local %s, S3 %s", info.alg.name, got, info.sum)
		}
		res.Checksum = info.alg.name
	}
	return res, tmp.Commit()
}

// countingWriter counts the bytes written at any offset, by several goroutines, and
// reports them to progress.
type countingWriter struct {
	w        io.WriterAt
	total    int64
	progress Progress
	mu       sync.Mutex
	n        int64
}

func (c *countingWriter) WriteAt(p []byte, off int64) (int, error) {
	n, err := c.w.WriteAt(p, off)
	c.mu.Lock()
	// A range read again after a network error is counted twice: never show more than
	// the object holds.
	c.n = min(c.n+int64(n), c.total)
	if c.progress != nil {
		c.progress(c.n, c.total)
	}
	c.mu.Unlock()
	return n, err
}

// UploadOptions are the options of Upload.
type UploadOptions struct {
	// Replace lets the upload write over an existing object.
	Replace bool
	// Progress, when set, receives the bytes sent.
	Progress Progress
	// Digest, when set, receives every byte of the file, in order, as it is read: a
	// hash of what was sent.
	Digest io.Writer
}

// Upload sends the file local to loc. From MultipartThreshold on, it goes as a
// multipart upload, several parts at a time, which is aborted when it fails or is
// interrupted. Unless Replace is set, S3 refuses to write over an existing object
// (ErrExists): the check is S3's own, made when the object is written, so two uploads
// cannot both believe the key was free.
//
// S3 verifies a CRC32 of each request, and returns the CRC32 of the whole object, which
// is compared with the one computed here while the file is read: Result.Checksum then
// says CRC32. It is empty when the answer carries no checksum.
func Upload(ctx context.Context, cfg aws.Config, local string, loc Location, opts UploadOptions) (Result, error) {
	replace, progress := opts.Replace, opts.Progress
	f, err := os.Open(local)
	if err != nil {
		return Result{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return Result{}, err
	}
	if !st.Mode().IsRegular() {
		return Result{}, fmt.Errorf("%s is not a regular file", local)
	}

	h := crc32.NewIEEE()
	var read io.Writer = h
	if opts.Digest != nil {
		read = io.MultiWriter(h, opts.Digest)
	}
	in := &transfermanager.UploadObjectInput{
		Bucket: aws.String(loc.Bucket), Key: aws.String(loc.Key),
		// The file is read once, in order, while its parts are sent in parallel.
		Body:              io.TeeReader(f, read),
		ContentLength:     aws.Int64(st.Size()),
		ChecksumAlgorithm: tmtypes.ChecksumAlgorithmCrc32,
		// The checksum of the object itself, also for a multipart upload, rather than a
		// checksum of the parts' checksums.
		ChecksumType: tmtypes.ChecksumTypeFullObject,
	}
	if t := mime.TypeByExtension(filepath.Ext(local)); t != "" {
		in.ContentType = aws.String(t)
	}
	if !replace {
		in.IfNoneMatch = aws.String("*")
	}
	// The transfer manager writes to the standard logger when it cannot complete a
	// multipart upload: the error it returns says the same.
	prev := log.Writer()
	log.SetOutput(io.Discard)
	defer log.SetOutput(prev)

	out, err := manager(s3.NewFromConfig(cfg), func(o *transfermanager.Options) {
		o.ChecksumAlgorithm = tmtypes.ChecksumAlgorithmCrc32
		if progress != nil {
			o.ObjectProgressListeners.Register(&listener{progress: progress, total: st.Size()})
		}
	}).UploadObject(ctx, in)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return Result{}, cerr
		}
		if code, _, status := codeAndStatus(err); status == 412 || code == "PreconditionFailed" || code == "ConditionalRequestConflict" {
			return Result{}, fmt.Errorf("%s %w", loc, ErrExists)
		}
		// A failed multipart upload comes wrapped with its upload ID, of no use once
		// the upload is aborted: keep the cause.
		var mp transfermanager.MultipartUploadError
		if errors.As(err, &mp) {
			if cause := errors.Unwrap(mp); cause != nil {
				err = cause
			}
		}
		return Result{}, explain(err, "writing", loc)
	}
	res := Result{Size: st.Size()}
	if got := aws.ToString(out.ChecksumCRC32); got != "" && out.ChecksumType != tmtypes.ChecksumTypeComposite {
		if want := encode(h); got != want {
			return Result{}, fmt.Errorf("checksum mismatch after upload (CRC32): local %s, S3 %s: %s is not the file sent", want, got, loc)
		}
		res.Checksum = "CRC32"
	}
	return res, nil
}

// listener reports to progress the bytes the transfer manager has sent: once per part
// of a multipart upload, from several goroutines.
type listener struct {
	progress Progress
	total    int64
	mu       sync.Mutex
}

func (l *listener) OnObjectBytesTransferred(_ context.Context, e *transfermanager.ObjectBytesTransferredEvent) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.progress(min(e.BytesTransferred, l.total), l.total)
}

// Presign returns a URL that downloads the object without credentials, for as long as
// expires (and no longer than the credentials that signed it). cfg must be set to the
// region of the bucket. Nothing is sent to S3: whether the URL works is only known
// when it is used.
func Presign(ctx context.Context, cfg aws.Config, loc Location, expires time.Duration) (string, error) {
	req, err := s3.NewPresignClient(s3.NewFromConfig(cfg)).PresignGetObject(ctx,
		&s3.GetObjectInput{Bucket: aws.String(loc.Bucket), Key: aws.String(loc.Key)},
		s3.WithPresignExpires(expires))
	if err != nil {
		return "", err
	}
	return req.URL, nil
}

// Delete removes an object. hop only deletes what it wrote itself: the copy of a file
// left in a relay bucket for an instance to fetch.
func Delete(ctx context.Context, cfg aws.Config, loc Location) error {
	_, err := s3.NewFromConfig(cfg).DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(loc.Bucket), Key: aws.String(loc.Key)})
	return explain(err, "deleting", loc)
}
