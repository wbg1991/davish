package transfer

import (
	"context"
	"io"
	iofs "io/fs"
	"os"
	"path"

	"github.com/pkg/sftp"

	"davish/internal/apperr"
	"davish/internal/fs"
)

// copyBuffer is how much a server-to-server copy holds in memory at once.
const copyBuffer = 256 << 10

// Copy starts one job per path, copying it from fromServer into dir on
// toServer. A name already taken in dir gets the next free "name (n)":
// nothing is ever replaced, so nothing needs confirming.
func (s *Service) Copy(_ context.Context, fromServer string, paths []string, toServer, dir string) ([]Job, error) {
	if err := fs.CheckPath(dir); err != nil {
		return nil, err
	}
	if _, err := s.conns.SFTP(fromServer); err != nil {
		return nil, err
	}
	dst, err := s.conns.SFTP(toServer)
	if err != nil {
		return nil, err
	}
	if fi, err := dst.Stat(dir); err != nil || !fi.IsDir() {
		return nil, apperr.New(apperr.NotFound, "the destination folder does not exist")
	}
	jobs := make([]Job, 0, len(paths))
	for _, p := range paths {
		t, run, err := s.copyTask(fromServer, toServer, p, dir)
		if err != nil {
			return jobs, err
		}
		job := t.job
		s.start(t, run) //nolint:contextcheck // a job outlives the call that starts it
		jobs = append(jobs, job)
	}
	return jobs, nil
}

func (s *Service) copyTask(fromServer, toServer, p, dir string) (*task, func(context.Context, *meter) error, error) {
	if err := fs.CheckPath(p); err != nil || p == "/" {
		return nil, nil, apperr.New(apperr.InvalidInput, "cannot copy this path")
	}
	// Copying a folder into itself on the same server would never end.
	if fromServer == toServer && (dir == p || len(dir) > len(p) && dir[:len(p)+1] == p+"/") {
		return nil, nil, apperr.New(apperr.InvalidInput, "cannot copy a folder into itself")
	}
	name := path.Base(p)
	if err := safeName(name); err != nil {
		return nil, nil, err
	}
	t, err := newTask(Job{
		Kind: KindCopy, ServerID: toServer, FromServerID: fromServer,
		Src: p, Dst: path.Join(dir, name),
	}, name)
	if err != nil {
		return nil, nil, err
	}
	run, err := s.bind(t)
	if err != nil {
		return nil, nil, err
	}
	return t, run, nil
}

func (s *Service) bindCopy(t *task) (func(context.Context, *meter) error, error) {
	src, err := s.conns.SFTP(t.job.FromServerID)
	if err != nil {
		return nil, err
	}
	dst, err := s.conns.SFTP(t.job.ServerID)
	if err != nil {
		return nil, err
	}
	p := t.job.Src
	fi, err := src.Lstat(p)
	if err != nil {
		return nil, mapErr("copy", err)
	}
	if !fi.IsDir() && !fi.Mode().IsRegular() {
		return nil, apperr.New(apperr.InvalidInput, "only files and folders can be copied")
	}
	// A job not bound yet has no name, so Dir says nothing until then.
	if t.job.Name != "" && fi.IsDir() != t.job.Dir {
		return nil, apperr.New(apperr.InvalidInput, "the source changed between file and folder")
	}
	dir := path.Dir(t.job.Dst)
	name, err := freeName(path.Base(p), remoteExists(dst, dir))
	if err != nil {
		return nil, err
	}
	t.job.Name, t.job.Dst, t.job.Dir = name, path.Join(dir, name), fi.IsDir()
	return func(ctx context.Context, m *meter) error {
		if fi.IsDir() {
			return mapErr("copy", s.copyDir(ctx, src, dst, p, dir, name, &t.part, m))
		}
		m.setTotal(fi.Size())
		return mapErr("copy", copyFileInto(ctx, src, dst, p, fi, dir, name, &t.part, m))
	}, nil
}

// copyFileInto writes the file next to its target and renames it into
// place, refusing to overwrite.
func copyFileInto(ctx context.Context, src, dst *sftp.Client, from string, fi os.FileInfo, dir, name string, p *partial, m *meter) error {
	tmp := path.Join(dir, p.name)
	start, err := remoteStart(dst, tmp, fi, p)
	if err != nil {
		return err
	}
	p.remember(fi)
	if start > 0 {
		m.skip(start)
	}
	safe, err := copyFile(ctx, src, dst, from, tmp, fi.Size(), start, fi.Mode().Perm(), m)
	if err == nil {
		err = dst.Rename(tmp, path.Join(dir, name)) // refuses to overwrite
	}
	if err != nil {
		afterRemoteFailure(ctx, dst, tmp, safe, lost(src) || lost(dst), p)
	}
	return err
}

// copyFile streams one file between servers from offset start. At 0 the
// destination must not exist yet; otherwise it is cut to start and
// continued. It also returns how much of it is written without gaps.
func copyFile(ctx context.Context, src, dst *sftp.Client, from, to string, size, start int64, mode os.FileMode, m *meter) (int64, error) {
	if err := ctx.Err(); err != nil {
		return start, err
	}
	in, err := src.Open(from)
	if err != nil {
		return start, err
	}
	defer in.Close() //nolint:errcheck // read only
	// O_EXCL: never write through a link someone planted at this name.
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if start > 0 {
		flags = os.O_WRONLY
	}
	out, err := dst.OpenFile(to, flags)
	if err != nil {
		return start, err
	}
	safe := start
	if start > 0 {
		err = out.Truncate(start)
		if err == nil {
			_, err = out.Seek(start, io.SeekStart)
		}
		if err == nil {
			_, err = in.Seek(start, io.SeekStart)
		}
	}
	if err == nil {
		_, err = io.CopyBuffer(out, &countingReader{r: in, size: size - start, m: m}, make([]byte, copyBuffer))
		if off, serr := out.Seek(0, io.SeekCurrent); serr == nil {
			safe = off
		}
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return safe, err
	}
	return safe, dst.Chmod(to, mode)
}

// copyDir builds the folder under a hidden name on the destination and
// renames it into place at the end, so a canceled copy leaves nothing.
func (s *Service) copyDir(ctx context.Context, src, dst *sftp.Client, from, dir, name string, p *partial, m *meter) error {
	entries, total, err := walkRemote(ctx, src, from)
	if err != nil {
		return err
	}
	m.setTotal(total)

	tmp := path.Join(dir, p.name)
	if _, err := dst.Lstat(tmp); err == nil {
		if err := dst.RemoveAll(tmp); err != nil {
			return err
		}
	}
	if err := dst.Mkdir(tmp); err != nil {
		return err
	}
	p.left = true
	err = s.fillCopy(ctx, src, dst, from, tmp, entries, m)
	if err == nil {
		err = dst.Rename(tmp, path.Join(dir, name)) // refuses to overwrite
	}
	if err != nil {
		if rerr := dst.RemoveAll(tmp); rerr != nil {
			s.log.Warn("copy cleanup failed", "app", "transfer", "op", "copy", "err", rerr)
		} else {
			p.left = false
		}
		return err
	}
	// Modes last: a read-only folder could not have been filled.
	final := path.Join(dir, name)
	setMode := func(p string, mode iofs.FileMode) {
		if err := dst.Chmod(p, mode.Perm()); err != nil {
			s.log.Warn("copy chmod failed", "app", "transfer", "op", "copy", "err", err)
		}
	}
	if fi, err := src.Stat(from); err == nil {
		setMode(final, fi.Mode())
	}
	for _, e := range entries {
		if e.mode.IsDir() {
			setMode(path.Join(final, e.rel), e.mode)
		}
	}
	return nil
}

func (s *Service) fillCopy(ctx context.Context, src, dst *sftp.Client, from, tmp string, entries []remoteEntry, m *meter) error {
	var links []remoteEntry
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		to := path.Join(tmp, e.rel)
		switch {
		case e.mode.IsDir():
			if err := dst.Mkdir(to); err != nil {
				return err
			}
		case e.mode.IsRegular():
			if _, err := copyFile(ctx, src, dst, path.Join(from, e.rel), to, e.size, 0, e.mode.Perm(), m); err != nil {
				return err
			}
		case e.mode&iofs.ModeSymlink != 0:
			links = append(links, e)
		default:
			s.log.Info("copy skips special file", "app", "transfer", "op", "copy")
		}
	}
	// Links last, so nothing is ever written through one.
	for _, e := range links {
		target, err := src.ReadLink(path.Join(from, e.rel))
		if err != nil {
			return err
		}
		if err := dst.Symlink(target, path.Join(tmp, e.rel)); err != nil {
			return err
		}
	}
	return nil
}
