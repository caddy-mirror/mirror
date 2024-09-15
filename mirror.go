package mirror

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/google/renameio/v2"
	"github.com/pkg/xattr"
	"go.uber.org/zap"
	"hash"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

func init() {
	caddy.RegisterModule(Mirror{})
}

type Mirror struct {
	// The path to the root of the site. Default is `{http.vars.root}` if set,
	// or current working directory otherwise. This should be a trusted value.
	//
	// Note that a site root is not a sandbox. Although the file server does
	// sanitize the request URI to prevent directory traversal, files (including
	// links) within the site root may be directly accessed based on the request
	// path. Files and folders within the root should be secure and trustworthy.
	//
	// Responses from upstreams will be written to files within this root directory to be used as a local mirror of static content
	Root string `json:"root,omitempty"`

	// File name suffix to add to write ETags to.
	// If set, file ETags will be written to sidecar files
	// with this suffix.
	EtagFileSuffix string `json:"etag_file_suffix,omitempty"`

	UseXattr bool `json:"xattr,omitempty"`

	Sha256Xattr bool `json:"sha256_xattr,omitempty"`

	logger *zap.Logger
}

func (Mirror) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.mirror",
		New: func() caddy.Module { return new(Mirror) },
	}
}

// Provision sets up the mirror handler
func (mir *Mirror) Provision(ctx caddy.Context) error {
	mir.logger = ctx.Logger()
	if mir.Root == "" {
		mir.Root = "{http.vars.root}"
	}
	return nil
}

func (mir *Mirror) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	if r.Method != http.MethodGet {
		mir.logger.Debug("Pass through non-GET request",
			zap.String("method", r.Method),
			zap.String("path", r.URL.Path))
		return next.ServeHTTP(w, r)
	}
	urlp := r.URL.Path
	if !path.IsAbs(urlp) {
		return caddyhttp.Error(http.StatusBadRequest, fmt.Errorf("URL path %v not absolute", urlp))
	}
	if strings.HasSuffix(urlp, "/") {
		// Pass through directory requests unmodified
		mir.logger.Debug("skip directory browse",
			zap.String("request_path", urlp))
		return next.ServeHTTP(w, r)
	}

	// Replace any Caddy placeholders in Root
	repl := r.Context().Value(caddy.ReplacerCtxKey).(*caddy.Replacer)
	root := repl.ReplaceAll(mir.Root, ".")
	logger := mir.logger.With(zap.String("site_root", root),
		zap.String("request_path", urlp))
	filename := pathInsideRoot(root, urlp)
	logger.Debug("creating temp file")
	incomingFile, err := createTempFile(filename)
	if err != nil {
		logger.Error("failed to create temp file",
			zap.Error(err))
		if errors.Is(err, fs.ErrPermission) {
			return caddyhttp.Error(http.StatusForbidden, err)
		}
		return caddyhttp.Error(http.StatusInternalServerError, err)
	}
	defer incomingFile.Cleanup()
	rww := &responseWriterWrapper{
		ResponseWriterWrapper: &caddyhttp.ResponseWriterWrapper{ResponseWriter: w},
		file:                  incomingFile,
		config:                mir,
		logger:                logger.With(zap.Namespace("rww")),
	}

	if mir.EtagFileSuffix != "" {
		etagFilename := filename + mir.EtagFileSuffix
		etagFile, err := createTempFile(etagFilename)
		if err != nil {
			logger.Error("failed to create ETag temp file, continuing without writing ETag sidecar file",
				zap.Error(err))
		} else {
			defer etagFile.Cleanup()
			rww.etagFile = etagFile
		}
	}
	w = rww

	return next.ServeHTTP(w, r)
}

var ErrNotRegular = errors.New("file is not a regular file")

func pathInsideRoot(root string, urlp string) string {
	// Figure out the local path of the given URL path
	filename := strings.TrimSuffix(caddyhttp.SanitizedPathJoin(root, urlp), "/")
	return filename
}

type responseWriterWrapper struct {
	*caddyhttp.ResponseWriterWrapper
	file          *renameio.PendingFile
	etagFile      *renameio.PendingFile
	config        *Mirror
	logger        *zap.Logger
	bytesExpected int64
	bytesWritten  int64
	contentHash   hash.Hash
}

func (rww *responseWriterWrapper) writeDone(written int64) {
	rww.bytesWritten += written
	if rww.bytesExpected > 0 && rww.bytesWritten == rww.bytesExpected {
		rww.logger.Debug("responseWriterWrapper fully written",
			zap.Int64("bytes_written", rww.bytesWritten),
			zap.Int64("bytes_expected", rww.bytesExpected),
		)
		rww.finalize()
	}
}

func (rww *responseWriterWrapper) finalize() {
	if rww.contentHash != nil {
		sum := rww.contentHash.Sum(nil)
		sumText := hex.EncodeToString(sum)
		rww.logger.Debug("hash done", zap.String("sum", sumText))
		if rww.config.Sha256Xattr {
			err := xattr.FSet(rww.file.File, "user.xdg.origin.sha256", []byte(sumText))
			if err != nil {
				rww.logger.Error("failed to set sha256 xattr",
					zap.Binary("sha256", sum),
					zap.Error(err))
			}
		}
	}
	err := rww.file.CloseAtomicallyReplace()
	if err != nil {
		rww.logger.Error("failed to complete mirror file",
			zap.Error(err))
		return
	} else if rww.etagFile != nil {
		err := rww.etagFile.CloseAtomicallyReplace()
		if err != nil {
			rww.logger.Error("failed to complete etagFile",
				zap.Error(err))
		}
	}
}

// writeAll writes to w from data[], retrying until all of data[] has been consumed, unless an error other than ErrShortWrite occurs
func writeAll(w io.Writer, data []byte) (int, error) {
	written := 0
	for {
		// Keep going until we are not making any more progress
		n, err := w.Write(data[written:])
		written += n
		if written > len(data) {
			panic("wrote more than len(data)!!!")
		}
		if n == 0 {
			if err == nil {
				err = io.ErrShortWrite
			}
			return written, fmt.Errorf("not making progress: %w", err)
		}
		if written == len(data) {
			break
		}
	}
	return written, nil
}

func (rww *responseWriterWrapper) Write(data []byte) (int, error) {
	if len(data) > 0 && rww.file != nil {
		if rww.contentHash != nil {
			hashed, err := writeAll(rww.contentHash, data)
			if err != nil {
				rww.logger.Error("failed to hash data",
					zap.Int("bytes_hashed", hashed),
					zap.Error(err))
				rww.contentHash = nil
			}
		}
		written, err := writeAll(rww.file, data)
		rww.writeDone(int64(written))
		if err != nil {
			return written, err
		}
	}
	// Continue by passing the buffer on to the next ResponseWriter in the chain
	return rww.ResponseWriter.Write(data)
}

func (rww *responseWriterWrapper) WriteHeader(statusCode int) {
	rww.logger.Debug("WriteHeader", zap.Int("status_code", statusCode))
	if statusCode == http.StatusOK {
		// Get the Content-Length header to figure out how much data to expect
		cl, err := strconv.ParseInt(rww.Header().Get("Content-Length"), 10, 64)
		if err == nil {
			rww.bytesExpected = cl
		}
		etag := rww.Header().Get("ETag")
		if etag != "" {
			// Store ETag as xattr
			if rww.config.UseXattr {
				err := xattr.FSet(rww.file.File, "user.xdg.origin.etag", []byte(etag))
				if err != nil {
					rww.logger.Error("failed to write ETag to xattr",
						zap.Error(err))
				}
			}
			// Store ETag as separate file
			if rww.etagFile != nil {
				_, err := io.Copy(rww.etagFile, strings.NewReader(etag))
				if err != nil {
					rww.logger.Error("failed to write temp ETag file",
						zap.Error(err))
				}
			}
		}
		if rww.config.Sha256Xattr {
			rww.contentHash = sha256.New()
		}
	} else if rww.file != nil {
		// Avoid writing error messages and such to disk
		err := rww.file.Cleanup()
		rww.file = nil
		if err != nil {
			rww.logger.Error("failed to clean up mirror file",
				zap.Error(err))
		}
	}
	rww.ResponseWriter.WriteHeader(statusCode)
}

func createTempFile(path string) (*renameio.PendingFile, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, mkdirPerms); err != nil {
		return nil, &fs.PathError{
			Op:   "createTempFile",
			Path: path,
			Err:  err,
		}
	}
	stat, err := os.Lstat(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, &fs.PathError{
			Op:   "createTempFile",
			Path: path,
			Err:  err,
		}
	}
	if stat != nil && !stat.Mode().IsRegular() {
		return nil, &fs.PathError{
			Op:   "createTempFile",
			Path: path,
			Err:  ErrNotRegular,
		}
	}

	// Create a temporary file in the same directory as the destination named ".<name><random numbers>"
	temp, err := renameio.TempFile(dir, path)
	if err != nil {
		return nil, &fs.PathError{
			Op:   "createTempFile",
			Path: path,
			Err:  err,
		}
	}
	if stat != nil {
		// Attempt to chmod the temporary file to match the destination
		ts, err := temp.Stat()
		if err != nil {
			closeErr := temp.Cleanup()
			return nil, &fs.PathError{
				Op:   "createTempFile",
				Path: path,
				Err:  errors.Join(err, closeErr),
			}
		}
		if ts.Mode().Perm() != stat.Mode().Perm() {
			err := temp.Chmod(stat.Mode().Perm())
			if err != nil {
				closeErr := temp.Cleanup()
				return nil, &fs.PathError{
					Op:   "createTempFile",
					Path: path,
					Err:  errors.Join(err, closeErr),
				}
			}
		}
	}
	return temp, nil
}

const (
	// mode before umask is applied
	mkdirPerms fs.FileMode = 0o777
)

// Interface guards
var (
	_ caddy.Provisioner           = (*Mirror)(nil)
	_ caddyhttp.MiddlewareHandler = (*Mirror)(nil)
)
