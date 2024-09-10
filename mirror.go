package mirror

import (
	"errors"
	"fmt"
	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
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

	// File name suffix to add to write Etags to.
	// If set, file Etags will be written to sidecar files
	// with this suffix.
	EtagFileSuffix string `json:"etag_file_suffix,omitempty"`

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
		mir.logger.Debug("Ignore non-GET request",
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
	filename := mir.locateMirrorFile(root, urlp)
	logger := mir.logger.With(zap.String("site_root", root),
		zap.String("request_path", urlp),
		zap.String("filename", filename))
	mirrorFileExists, err := mir.validateMirrorTarget(filename)
	if err != nil {
		if errors.Is(err, ErrIsDir) {
			logger.Debug("skip directory")
			return next.ServeHTTP(w, r)
		} else if errors.Is(err, fs.ErrPermission) {
			logger.Error("mirror file permission error, return 403",
				zap.Error(err))
			return caddyhttp.Error(http.StatusForbidden, err)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return caddyhttp.Error(http.StatusInternalServerError, err)
		}
	}
	if mirrorFileExists {
		logger.Debug("mirror file exists")
	} else {
		logger.Debug("creating temp file")
		incomingFile, err := NewIncomingFile(filename)
		if err != nil {
			logger.Error("failed to create temp file", zap.Error(err))
			if errors.Is(err, fs.ErrPermission) {
				return caddyhttp.Error(http.StatusForbidden, err)
			}
			return caddyhttp.Error(http.StatusInternalServerError, err)
		}
		defer func(f *IncomingFile) {
			logger.Debug("closing temp file")
			err := f.Close()
			if err != nil && !errors.Is(err, fs.ErrClosed) {
				logger.Error("error when closing temp file",
					zap.Object("file", loggableIncomingFile{f}),
					zap.Error(err))
			}
		}(incomingFile)
		rww := &responseWriterWrapper{
			ResponseWriterWrapper: &caddyhttp.ResponseWriterWrapper{ResponseWriter: w},
			file:                  incomingFile,
			config:                mir,
			logger:                logger.With(zap.Namespace("rww")),
		}

		if mir.EtagFileSuffix != "" {
			etagFilename := filename + mir.EtagFileSuffix
			etagFile, err := NewIncomingFile(etagFilename)
			if err != nil {
				logger.Error("failed to create Etag temp file", zap.Error(err))
			} else if etagFile != nil {
				defer func(f *IncomingFile) {
					logger.Debug("closing Etag temp file")
					err := f.Close()
					if err != nil && !errors.Is(err, fs.ErrClosed) {
						logger.Error("error when closing Etag temp file",
							zap.Object("etag_file", loggableIncomingFile{f}),
							zap.Error(err))
					}
				}(etagFile)
				rww.etagFile = etagFile
			}
		}
		w = rww
	}

	return next.ServeHTTP(w, r)
}

var ErrIsDir = errors.New("file is a directory")
var ErrNotRegular = errors.New("file is not a regular file")

// Returns true if the file exists and is a regular file, false otherwise
func (mir *Mirror) validateMirrorTarget(filename string) (bool, error) {
	stat, err := os.Lstat(filename)
	if err != nil {
		// fs.ErrNotExist is expected when we have not mirrored this path before
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	} else if stat.Mode().IsDir() {
		// Skip directories
		return false, &fs.PathError{
			Op:   "locate mirror copy",
			Path: filename,
			Err:  ErrIsDir,
		}
	} else if !stat.Mode().IsRegular() {
		mir.logger.Error("local mirror is not a file!!",
			zap.String("filename", filename),
			zap.String("fileinfo", fs.FormatFileInfo(stat)))

		return false, caddyhttp.Error(http.StatusForbidden,
			&fs.PathError{
				Op:   "locate mirror copy",
				Path: filename,
				Err:  ErrNotRegular,
			})
	}
	mir.logger.Debug("mirror file info",
		zap.String("filename", filename),
		zap.String("fileinfo", fs.FormatFileInfo(stat)))
	return true, nil
}

func (mir *Mirror) locateMirrorFile(root string, urlp string) string {
	// Figure out the local path of the given URL path
	filename := strings.TrimSuffix(caddyhttp.SanitizedPathJoin(root, urlp), "/")
	mir.logger.Debug("sanitized path join",
		zap.String("site_root", root),
		zap.String("request_path", urlp),
		zap.String("result", filename))
	return filename
}

type responseWriterWrapper struct {
	*caddyhttp.ResponseWriterWrapper
	file          *IncomingFile
	etagFile      *IncomingFile
	config        *Mirror
	logger        *zap.Logger
	bytesExpected int64
	bytesWritten  int64
}

// loggableMirrorWriter makes the response writer wrapper loggable with zap.Object().
type loggableMirrorWriter struct {
	*responseWriterWrapper
}

func (rww *responseWriterWrapper) Write(data []byte) (int, error) {
	// ignore zero data writes
	if rww.file.closed || len(data) == 0 {
		return rww.ResponseWriter.Write(data)
	}
	var written = 0
	defer func() {
		rww.bytesWritten += int64(written)
		if rww.bytesExpected > 0 && rww.bytesWritten == rww.bytesExpected {
			rww.logger.Debug("responseWriterWrapper fully written",
				zap.Int64("bytes_written", rww.bytesWritten),
				zap.Int64("bytes_expected", rww.bytesExpected),
			)
			err := rww.file.Complete()
			if err != nil {
				rww.logger.Error("failed to complete mirror file",
					zap.Object("file", loggableIncomingFile{rww.file}),
					zap.Error(err))
			} else if rww.etagFile != nil {
				err := rww.etagFile.Complete()
				if err != nil {
					rww.logger.Error("failed to complete etagFile",
						zap.Object("file", loggableIncomingFile{rww.etagFile}),
						zap.Error(err))
				}
			}
		}
	}()
	for {
		// Write out the data buffer to the mirror file first
		n, err := rww.file.Write(data[written:])
		written += n
		if err != nil && !errors.Is(err, io.ErrShortWrite) {
			return written, err
		}

		if written == len(data) {
			// Continue by passing the buffer on to the next ResponseWriter in the chain
			return rww.ResponseWriter.Write(data)
		}
	}
}

func (rww *responseWriterWrapper) WriteHeader(statusCode int) {
	if statusCode == http.StatusOK {
		// Get the Content-Length header to figure out how much data to expect
		cl, err := strconv.ParseInt(rww.Header().Get("Content-Length"), 10, 64)
		if err == nil {
			rww.bytesExpected = cl
		}
		// Attempt to generate ETag sidecar file
		if rww.etagFile != nil {
			etag := rww.Header().Get("Etag")
			if etag != "" {
				_, err := io.Copy(rww.etagFile, strings.NewReader(etag))
				if err != nil {
					rww.logger.Error("failed to write temp ETag file",
						zap.Object("file", loggableIncomingFile{rww.etagFile}),
						zap.Error(err))
				}
			}
		}
	} else {
		// Avoid writing error messages and such to disk
		err := rww.file.Abort()
		if err != nil {
			rww.logger.Error("failed to abort mirror file",
				zap.Object("file", loggableIncomingFile{rww.file}),
				zap.Error(err))
		}
	}
	rww.ResponseWriter.WriteHeader(statusCode)
}

// MarshalLogObject satisfies the zapcore.ObjectMarshaler interface.
func (w loggableMirrorWriter) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	enc.AddInt64("bytes_written", w.bytesWritten)
	enc.AddInt64("bytes_expected", w.bytesExpected)
	enc.AddObject("file", loggableIncomingFile{w.file})
	if w.etagFile != nil {
		enc.AddObject("etag_file", loggableIncomingFile{w.etagFile})
	}
	return nil
}

func (f *IncomingFile) Close() error {
	if !f.completed {
		return f.Abort()
	}
	return f.File.Close()
}

// Complete will close and rename the temporary file into its target name
func (f *IncomingFile) Complete() error {
	if f.aborted {
		return &fs.PathError{
			Op:   "IncomingFile.Complete()",
			Path: f.Name(),
			Err:  errors.New("file already aborted"),
		}
	}
	if f.done {
		return nil
	}
	if !f.closed {
		// Important to fsync before renaming to avoid risking a 0 byte destination file
		syncErr := f.Sync()
		closeErr := f.File.Close()
		f.closed = true
		if (syncErr != nil) || (closeErr != nil) {
			// If syncing and closing fails we will abort instead of complete this file.
			abortErr := f.Abort()
			return errors.Join(syncErr, closeErr, abortErr)
		}
	}
	err := os.Rename(f.File.Name(), f.target)
	if err != nil {
		return err
	}
	f.completed = true
	return nil
}

func (f *IncomingFile) Abort() error {
	if f.completed {
		return &fs.PathError{
			Op:   "IncomingFile.Abort()",
			Path: f.Name(),
			Err:  errors.New("file already completed"),
		}
	}
	if f.done {
		return nil
	}
	f.aborted = true
	// Defer error handling for Close until after we have attempted to remove the temp file
	closeErr := f.File.Close()
	f.closed = true
	removeErr := os.Remove(f.File.Name())
	f.done = true
	return errors.Join(removeErr, closeErr)
}

func NewIncomingFile(path string) (*IncomingFile, error) {
	dir, base := filepath.Split(path)
	if err := os.MkdirAll(dir, mkdirPerms); err != nil {
		return nil, &fs.PathError{
			Op:   "NewIncomingFile",
			Path: path,
			Err:  err,
		}
	}
	stat, err := os.Lstat(path)
	if err == nil {
		if stat.Mode().IsDir() {
			return nil, &fs.PathError{
				Op:   "NewIncomingFile",
				Path: path,
				Err:  ErrIsDir,
			}
		}
		if !stat.Mode().IsRegular() {
			return nil, &fs.PathError{
				Op:   "NewIncomingFile",
				Path: path,
				Err:  ErrNotRegular,
			}
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, &fs.PathError{
			Op:   "NewIncomingFile",
			Path: path,
			Err:  err,
		}
	}

	temp, err := os.CreateTemp(dir, "._tmp_"+base)
	if err != nil {
		return nil, &fs.PathError{
			Op:   "NewIncomingFile",
			Path: path,
			Err:  err,
		}
	}
	if stat != nil {
		ts, err := temp.Stat()
		if err != nil {
			closeErr := temp.Close()
			return nil, &fs.PathError{
				Op:   "NewIncomingFile",
				Path: path,
				Err:  errors.Join(err, closeErr),
			}
		}
		if ts.Mode().Perm() != stat.Mode().Perm() {
			err := temp.Chmod(stat.Mode().Perm())
			if err != nil {
				closeErr := temp.Close()
				return nil, &fs.PathError{
					Op:   "NewIncomingFile",
					Path: path,
					Err:  errors.Join(err, closeErr),
				}
			}
		}
	}
	return &IncomingFile{
		File:   temp,
		target: path,
	}, nil
}

type IncomingFile struct {
	*os.File
	target    string
	completed bool
	done      bool
	closed    bool
	aborted   bool
}

// loggableIncomingFile makes an IncomingFile loggable with zap.Object().
type loggableIncomingFile struct {
	*IncomingFile
}

func (f loggableIncomingFile) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	enc.AddString("target", f.target)
	enc.AddString("temp_file", f.File.Name())
	enc.AddBool("completed", f.completed)
	enc.AddBool("done", f.done)
	enc.AddBool("aborted", f.aborted)
	enc.AddBool("closed", f.closed)
	return nil
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
