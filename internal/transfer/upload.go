package transfer

import (
	"context"
	"errors"
	"io"
	iofs "io/fs"
	"os"
	"path"
	"path/filepath"

	"github.com/pkg/sftp"

	"davish/internal/apperr"
	"davish/internal/fs"
	"davish/internal/shellx"
)

// UploadItem is a local file or folder about to be uploaded, as the UI
// shows it before starting.
type UploadItem struct {
	Local string `json:"local"`
	Name  string `json:"name"`
	Dir   bool   `json:"dir"`
	Size  int64  `json:"size"` // 0 for folders until they are walked
	// Exists is true when the target folder already has Name.
	Exists    bool `json:"exists"`
	ExistsDir bool `json:"existsDir"`
	// ReplaceCommand is the operation replacing the existing file runs,
	// for the confirmation dialog. It is empty when replacing is not
	// offered: folders are never merged or replaced, only kept side by side.
	ReplaceCommand string `json:"replaceCommand,omitempty"`
}

// UploadRequest starts one upload. Without Replace an existing name is
// kept and the upload gets the next free "name (n)".
type UploadRequest struct {
	Local   string `json:"local"`
	Replace bool   `json:"replace"`
}

// PlanUpload describes what uploading locals into dir would do, so the
// UI can ask about names that already exist.
func (s *Service) PlanUpload(_ context.Context, serverID, dir string, locals []string) ([]UploadItem, error) {
	if err := fs.CheckPath(dir); err != nil {
		return nil, err
	}
	sc, err := s.conns.SFTP(serverID)
	if err != nil {
		return nil, err
	}
	items := make([]UploadItem, 0, len(locals))
	for _, local := range locals {
		fi, err := statLocal(local)
		if err != nil {
			return nil, err
		}
		it := UploadItem{Local: local, Name: fi.Name(), Dir: fi.IsDir()}
		if !it.Dir {
			it.Size = fi.Size()
		}
		dst := path.Join(dir, it.Name)
		rfi, err := sc.Lstat(dst)
		switch {
		case err == nil:
			it.Exists, it.ExistsDir = true, rfi.IsDir()
			if !it.Dir && rfi.Mode().IsRegular() {
				it.ReplaceCommand = shellx.Cmd("put", local, dst).String()
			}
		case !errors.Is(err, os.ErrNotExist):
			return nil, mapErr("upload", err)
		}
		items = append(items, it)
	}
	return items, nil
}

// statLocal follows a link given directly, since the user picked what it
// points to, and accepts only files and folders.
func statLocal(local string) (os.FileInfo, error) {
	if !filepath.IsAbs(local) {
		return nil, apperr.New(apperr.InvalidInput, "local path must be absolute")
	}
	fi, err := os.Stat(local)
	if err != nil {
		return nil, mapErr("upload", err)
	}
	if !fi.IsDir() && !fi.Mode().IsRegular() {
		return nil, apperr.New(apperr.InvalidInput, "only files and folders can be uploaded")
	}
	return fi, nil
}

// Upload starts one job per request, uploading into dir.
func (s *Service) Upload(_ context.Context, serverID, dir string, reqs []UploadRequest) ([]Job, error) {
	if err := fs.CheckPath(dir); err != nil {
		return nil, err
	}
	if _, err := s.conns.SFTP(serverID); err != nil {
		return nil, err
	}
	jobs := make([]Job, 0, len(reqs))
	for _, req := range reqs {
		t, run, err := s.uploadTask(serverID, dir, req)
		if err != nil {
			return jobs, err
		}
		job := t.job
		s.start(t, run) //nolint:contextcheck // a job outlives the call that starts it
		jobs = append(jobs, job)
	}
	return jobs, nil
}

func (s *Service) uploadTask(serverID, dir string, req UploadRequest) (*task, func(context.Context, *meter) error, error) {
	fi, err := statLocal(req.Local)
	if err != nil {
		return nil, nil, err
	}
	t, err := newTask(Job{
		Kind: KindUpload, ServerID: serverID, Src: req.Local,
		Dst: path.Join(dir, fi.Name()), Dir: fi.IsDir(),
	}, fi.Name())
	if err != nil {
		return nil, nil, err
	}
	t.replace = req.Replace
	run, err := s.bind(t)
	if err != nil {
		return nil, nil, err
	}
	return t, run, nil
}

func (s *Service) bindUpload(t *task) (func(context.Context, *meter) error, error) {
	sc, err := s.conns.SFTP(t.job.ServerID)
	if err != nil {
		return nil, err
	}
	local := t.job.Src
	fi, err := statLocal(local)
	if err != nil {
		return nil, err
	}
	if fi.IsDir() != t.job.Dir {
		return nil, apperr.New(apperr.InvalidInput, "the source changed between file and folder")
	}
	dir, name := path.Dir(t.job.Dst), fi.Name()
	var replaceMode *os.FileMode
	if t.replace {
		rfi, err := sc.Lstat(path.Join(dir, name))
		if err != nil || fi.IsDir() || !rfi.Mode().IsRegular() {
			return nil, apperr.New(apperr.InvalidInput, "only an existing file can be replaced")
		}
		mode := rfi.Mode().Perm()
		replaceMode = &mode
	} else {
		name, err = freeName(name, remoteExists(sc, dir))
		if err != nil {
			return nil, err
		}
	}
	t.job.Name, t.job.Dst = name, path.Join(dir, name)
	return func(ctx context.Context, m *meter) error {
		if fi.IsDir() {
			return mapErr("upload", s.uploadDir(ctx, sc, local, dir, name, &t.part, m))
		}
		return mapErr("upload", uploadFile(ctx, sc, local, fi, dir, name, replaceMode, &t.part, m))
	}, nil
}

func remoteExists(sc *sftp.Client, dir string) func(string) (bool, error) {
	return func(name string) (bool, error) {
		_, err := sc.Lstat(path.Join(dir, name))
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return err == nil, mapErr("upload", err)
	}
}

// uploadFile writes the file next to its target and renames it into
// place. replaceMode is the mode of the file it replaces, nil when the
// name is new; a new name is never allowed to replace anything.
func uploadFile(ctx context.Context, sc *sftp.Client, local string, fi os.FileInfo, dir, name string, replaceMode *os.FileMode, p *partial, m *meter) error {
	m.setTotal(fi.Size())
	tmp := path.Join(dir, p.name)
	dst := path.Join(dir, name)
	from, err := remoteStart(sc, tmp, fi, p)
	if err != nil {
		return err
	}
	p.remember(fi)
	if from > 0 {
		m.skip(from)
	}
	mode := fi.Mode().Perm()
	if replaceMode != nil {
		mode = *replaceMode
	}
	safe, err := putFile(ctx, sc, local, tmp, fi.Size(), from, mode, m)
	if err == nil {
		if replaceMode != nil {
			err = sc.PosixRename(tmp, dst)
		} else {
			err = sc.Rename(tmp, dst) // refuses to overwrite
		}
	}
	if err != nil {
		afterRemoteFailure(ctx, sc, tmp, safe, lost(sc), p)
	}
	return err
}

// putFile copies local to remote from offset from. At 0 the remote path
// must not exist yet; otherwise it is cut to from and continued. It also
// returns how much of the remote file is written without gaps.
func putFile(ctx context.Context, sc *sftp.Client, local, remote string, size, from int64, mode os.FileMode, m *meter) (int64, error) {
	if err := ctx.Err(); err != nil {
		return from, err
	}
	src, err := os.Open(local) //nolint:gosec // the user picked this file
	if err != nil {
		return from, err
	}
	defer src.Close() //nolint:errcheck // read only

	// O_EXCL: never write through a link someone planted at this name.
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if from > 0 {
		flags = os.O_WRONLY
	}
	dst, err := sc.OpenFile(remote, flags)
	if err != nil {
		return from, err
	}
	safe := from
	if from > 0 {
		err = dst.Truncate(from)
		if err == nil {
			_, err = dst.Seek(from, io.SeekStart)
		}
		if err == nil {
			_, err = src.Seek(from, io.SeekStart)
		}
	}
	if err == nil {
		_, err = dst.ReadFrom(&countingReader{r: src, size: size - from, m: m})
		// After a failed concurrent write the offset is where the first
		// gap may start, whatever was written beyond it.
		if off, serr := dst.Seek(0, io.SeekCurrent); serr == nil {
			safe = off
		}
	}
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return safe, err
	}
	return safe, sc.Chmod(remote, mode)
}

// localEntry is one item inside a local folder being uploaded.
type localEntry struct {
	rel  string // slash separated, relative to the folder
	mode iofs.FileMode
	size int64
}

func walkLocal(root string) ([]localEntry, int64, error) {
	var entries []localEntry
	var total int64
	err := filepath.WalkDir(root, func(p string, d iofs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == root {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		e := localEntry{rel: filepath.ToSlash(rel), mode: info.Mode()}
		if info.Mode().IsRegular() {
			e.size = info.Size()
			total += e.size
		}
		entries = append(entries, e)
		return nil
	})
	return entries, total, err
}

// uploadDir builds the folder under a hidden name and renames it into
// place at the end, so a canceled upload leaves nothing half done.
func (s *Service) uploadDir(ctx context.Context, sc *sftp.Client, local, dir, name string, p *partial, m *meter) error {
	entries, total, err := walkLocal(local)
	if err != nil {
		return err
	}
	m.setTotal(total)

	tmp := path.Join(dir, p.name)
	if _, err := sc.Lstat(tmp); err == nil {
		if err := sc.RemoveAll(tmp); err != nil {
			return err
		}
	}
	if err := sc.Mkdir(tmp); err != nil {
		return err
	}
	p.left = true
	err = s.fillRemoteDir(ctx, sc, local, tmp, entries, m)
	if err == nil {
		err = sc.Rename(tmp, path.Join(dir, name)) // refuses to overwrite
	}
	if err != nil {
		if rerr := sc.RemoveAll(tmp); rerr != nil {
			s.log.Warn("upload cleanup failed", "app", "transfer", "op", "upload", "err", rerr)
		} else {
			p.left = false
		}
		return err
	}
	// Modes last: a read-only folder could not have been filled.
	dst := path.Join(dir, name)
	setMode := func(p string, mode iofs.FileMode) {
		if err := sc.Chmod(p, mode.Perm()); err != nil {
			s.log.Warn("upload chmod failed", "app", "transfer", "op", "upload", "err", err)
		}
	}
	if fi, err := os.Stat(local); err == nil {
		setMode(dst, fi.Mode())
	}
	for _, e := range entries {
		if e.mode.IsDir() {
			setMode(path.Join(dst, e.rel), e.mode)
		}
	}
	return nil
}

func (s *Service) fillRemoteDir(ctx context.Context, sc *sftp.Client, local, tmp string, entries []localEntry, m *meter) error {
	var links []localEntry
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		remote := path.Join(tmp, e.rel)
		switch {
		case e.mode.IsDir():
			if err := sc.Mkdir(remote); err != nil {
				return err
			}
		case e.mode.IsRegular():
			src := filepath.Join(local, filepath.FromSlash(e.rel))
			if _, err := putFile(ctx, sc, src, remote, e.size, 0, e.mode.Perm(), m); err != nil {
				return err
			}
		case e.mode&iofs.ModeSymlink != 0:
			links = append(links, e)
		default:
			s.log.Info("upload skips special file", "app", "transfer", "op", "upload")
		}
	}
	// Links last, so nothing is ever written through one.
	for _, e := range links {
		target, err := os.Readlink(filepath.Join(local, filepath.FromSlash(e.rel)))
		if err != nil {
			return err
		}
		if err := sc.Symlink(filepath.ToSlash(target), path.Join(tmp, e.rel)); err != nil {
			return err
		}
	}
	return nil
}
