package main

import (
	"context"
	"fmt"
	"io"

	"cloud.google.com/go/storage"
)

// objReader provides a seekable reader for a GCS bucket object.
type objReader struct {
	obj              *storage.ObjectHandle
	ctx              context.Context
	r                *storage.Reader
	pos, size, nread int64
}

func (r *objReader) Read(dest []byte) (int, error) {
	if r.r == nil && r.pos < r.size {
		var err error
		r.r, err = r.obj.NewRangeReader(r.ctx, r.pos, -1)
		if err != nil {
			return 0, err
		}
	}
	if r.r == nil {
		return 0, io.EOF
	}
	n, err := r.r.Read(dest)
	r.pos += int64(n)
	r.nread += int64(n)
	return n, err
}

func (r *objReader) Seek(offset int64, whence int) (int64, error) {
	err := r.Close()
	if err != nil {
		return 0, err
	}

	switch whence {
	case io.SeekStart:
		r.pos = offset
	case io.SeekCurrent:
		r.pos += offset
	case io.SeekEnd:
		r.pos = r.size + offset
	default:
		return 0, fmt.Errorf("illegal whence value %d", whence)
	}

	return r.pos, nil
}

func (r *objReader) Close() error {
	if r.r == nil {
		return nil
	}
	err := r.r.Close()
	r.r = nil
	return err
}
