package transfer

import (
	"context"
	"errors"
	"io"
	iofs "io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/pkg/sftp"

	"davish/internal/apperr"
	"davish/internal/desktop"
	"davish/internal/fs"
)

// Download starts one job per remote path, saving into the download
// folder. Existing local files are never replaced: a taken name gets the
// next free "name (n)".
func (s *Service) Download(_ context.Context, serverID string, paths []string) ([]Job, error) {
	if _, err := s.conns.SFTP(serverID); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(s.opts.DownloadDir, 0o750); err != nil {
		return nil, mapErr("download", err)
	}
	jobs := make([]Job, 0, len(paths))
	for _, p := range paths {
		t, run, err := s.downloadTask(serverID, p)
		if err != nil {
			return jobs, err
		}
		job := t.job
		s.start(t, run) //nolint:contextcheck // a job outlives the call that starts it
		jobs = append(jobs, job)
	}
	return jobs, nil
}

func (s *Service) downloadTask(serverID, remote string) (*task, func(context.Context, *meter) error, error) {
	if err := fs.CheckPath(remote); err != nil {
		return nil, nil, err
	}
	if err := safeName(path.Base(remote)); err != nil {
		return nil, nil, err
	}
	t, err := newTask(Job{Kind: KindDownload, ServerID: serverID, Src: remote}, path.Base(remote))
	if err != nil {
		return nil, nil, err
	}
	run, err := s.bind(t)
	if err != nil {
		return nil, nil, err
	}
	return t, run, nil
}

func (s *Service) bindDownload(t *task) (func(context.Context, *meter) error, error) {
	sc, err := s.conns.SFTP(t.job.ServerID)
	if err != nil {
		return nil, err
	}
	remote := t.job.Src
	// Follow a link given directly: the user wants what it points to.
	fi, err := sc.Stat(remote)
	if err != nil {
		return nil, mapErr("download", err)
	}
	if !fi.IsDir() && !fi.Mode().IsRegular() {
		return nil, apperr.New(apperr.InvalidInput, "only files and folders can be downloaded")
	}
	// A job not bound yet has no name, so Dir says nothing until then.
	if t.job.Name != "" && fi.IsDir() != t.job.Dir {
		return nil, apperr.New(apperr.InvalidInput, "the source changed between file and folder")
	}
	dir := s.opts.DownloadDir
	name, err := freeName(path.Base(remote), localExists(dir))
	if err != nil {
		return nil, err
	}
	t.job.Name, t.job.Dst, t.job.Dir = name, filepath.Join(dir, name), fi.IsDir()
	return func(ctx context.Context, m *meter) error {
		if fi.IsDir() {
			return mapErr("download", s.downloadDir(ctx, sc, remote, dir, name, &t.part, m))
		}
		return mapErr("download", downloadFile(ctx, sc, remote, fi, dir, name, &t.part, m))
	}, nil
}

func localExists(dir string) func(string) (bool, error) {
	return func(name string) (bool, error) {
		_, err := os.Lstat(filepath.Join(dir, name))
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return err == nil, mapErr("download", err)
	}
}

// claim renames part to final unless final appeared in the meantime.
func claim(part, final string) error {
	if _, err := os.Lstat(final); err == nil {
		return apperr.New(apperr.FSExists, "download target already exists")
	}
	return os.Rename(part, final)
}

func downloadFile(ctx context.Context, sc *sftp.Client, remote string, fi os.FileInfo, dir, name string, p *partial, m *meter) error {
	m.setTotal(fi.Size())
	part := filepath.Join(dir, p.name)
	var from int64
	if p.matches(fi) {
		if lfi, err := os.Lstat(part); err == nil && lfi.Mode().IsRegular() && lfi.Size() <= fi.Size() {
			from = lfi.Size()
		}
	}
	if from == 0 {
		if err := os.Remove(part); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		p.keep, p.left = false, false
	} else {
		m.skip(from)
	}
	p.remember(fi)
	err := getFile(ctx, sc, remote, part, from, fi.Mode().Perm(), m)
	if err == nil {
		err = claim(part, filepath.Join(dir, name))
	}
	if err == nil {
		return nil
	}
	if ctx.Err() == nil && lost(sc) {
		p.keep, p.left = true, true
		return err
	}
	rerr := os.Remove(part)
	p.keep, p.left = false, rerr != nil && !errors.Is(rerr, os.ErrNotExist)
	return err
}

// getFile copies remote to a local path from offset from. At 0 the local
// path must not exist yet; otherwise it holds exactly from bytes. The file
// stays writable until it is complete, so that it can be continued.
func getFile(ctx context.Context, sc *sftp.Client, remote, local string, from int64, perm os.FileMode, m *meter) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	src, err := sc.Open(remote)
	if err != nil {
		return err
	}
	defer src.Close() //nolint:errcheck // read only

	// O_EXCL: never write through a link or over a file already there.
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if from > 0 {
		flags = os.O_WRONLY
	}
	dst, err := os.OpenFile(local, flags, perm|0o200) //nolint:gosec // path built from validated names
	if err != nil {
		return err
	}
	if from > 0 {
		_, err = src.Seek(from, io.SeekStart)
		if err == nil {
			_, err = dst.Seek(from, io.SeekStart)
		}
	}
	if err == nil {
		_, err = src.WriteTo(&countingWriter{w: dst, m: m})
	}
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	if err != nil || perm&0o200 != 0 {
		return err
	}
	fi, err := os.Stat(local)
	if err != nil {
		return err
	}
	return os.Chmod(local, fi.Mode().Perm()&^0o200)
}

// remoteEntry is one item inside a remote folder being downloaded.
type remoteEntry struct {
	rel  string // slash separated, relative to the folder; every part checked by safeName
	mode iofs.FileMode
	size int64
}

func walkRemote(ctx context.Context, sc *sftp.Client, root string) ([]remoteEntry, int64, error) {
	var entries []remoteEntry
	var total int64
	w := sc.Walk(root)
	for w.Step() {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		if err := w.Err(); err != nil {
			return nil, 0, err
		}
		if w.Path() == root {
			continue
		}
		rel := strings.TrimPrefix(w.Path(), root+"/")
		for _, part := range strings.Split(rel, "/") {
			if err := safeName(part); err != nil {
				return nil, 0, err
			}
		}
		fi := w.Stat() // not following links
		e := remoteEntry{rel: rel, mode: fi.Mode()}
		if fi.Mode().IsRegular() {
			e.size = fi.Size()
			total += e.size
		}
		entries = append(entries, e)
	}
	return entries, total, nil
}

// downloadDir builds the folder under a hidden name and renames it into
// place at the end, so a canceled download leaves nothing half done.
func (s *Service) downloadDir(ctx context.Context, sc *sftp.Client, remote, dir, name string, p *partial, m *meter) error {
	entries, total, err := walkRemote(ctx, sc, remote)
	if err != nil {
		return err
	}
	m.setTotal(total)

	part := filepath.Join(dir, p.name)
	if err := os.RemoveAll(part); err != nil {
		return err
	}
	if err := os.Mkdir(part, 0o750); err != nil {
		return err
	}
	p.left = true
	final := filepath.Join(dir, name)
	err = s.fillLocalDir(ctx, sc, remote, part, entries, m)
	if err == nil {
		err = claim(part, final)
	}
	if err != nil {
		if rerr := os.RemoveAll(part); rerr != nil {
			s.log.Warn("download cleanup failed", "app", "transfer", "op", "download", "err", rerr)
		} else {
			p.left = false
		}
		return err
	}
	// Modes last: a read-only folder could not have been filled.
	for _, e := range entries {
		if e.mode.IsDir() {
			if err := os.Chmod(filepath.Join(final, filepath.FromSlash(e.rel)), e.mode.Perm()); err != nil {
				s.log.Warn("download chmod failed", "app", "transfer", "op", "download", "err", err)
			}
		}
	}
	return nil
}

func (s *Service) fillLocalDir(ctx context.Context, sc *sftp.Client, remote, part string, entries []remoteEntry, m *meter) error {
	var links []remoteEntry
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		local := filepath.Join(part, filepath.FromSlash(e.rel))
		switch {
		case e.mode.IsDir():
			if err := os.Mkdir(local, 0o750); err != nil {
				return err
			}
		case e.mode.IsRegular():
			if err := getFile(ctx, sc, path.Join(remote, e.rel), local, 0, e.mode.Perm(), m); err != nil {
				return err
			}
		case e.mode&iofs.ModeSymlink != 0:
			links = append(links, e)
		default:
			s.log.Info("download skips special file", "app", "transfer", "op", "download")
		}
	}
	// Links last, so nothing is ever written through one; on a
	// case-insensitive disk a link could otherwise alias a folder. Links
	// that cannot be made (Windows without privilege) are skipped.
	for _, e := range links {
		target, err := sc.ReadLink(path.Join(remote, e.rel))
		if err != nil {
			return err
		}
		if err := os.Symlink(filepath.FromSlash(target), filepath.Join(part, filepath.FromSlash(e.rel))); err != nil {
			s.log.Warn("download skips link", "app", "transfer", "op", "download", "err", err)
		}
	}
	return nil
}

// RevealDownload shows a finished download in the system file manager.
// Only paths inside the download folder are accepted, so the frontend
// cannot use this to open anything else.
func (s *Service) RevealDownload(ctx context.Context, p string) error {
	if !filepath.IsAbs(p) {
		return apperr.New(apperr.InvalidInput, "path must be absolute")
	}
	rel, err := filepath.Rel(s.opts.DownloadDir, filepath.Clean(p))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return apperr.New(apperr.InvalidInput, "not in the download folder")
	}
	if _, err := os.Lstat(p); err != nil {
		return mapErr("reveal", err)
	}
	if err := desktop.Reveal(ctx, p); err != nil {
		return apperr.Wrap(apperr.Internal, "cannot open the file manager", err)
	}
	return nil
}
