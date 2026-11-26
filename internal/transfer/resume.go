package transfer

import (
	"context"
	"errors"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/pkg/sftp"

	"davish/internal/apperr"
)

// rewind is how far before the last safe offset a remote write starts
// again: one full window of concurrent writes (64 packets of 32 KiB).
const rewind int64 = 2 << 20

// partial is what a job's earlier attempts left at its temporary name. A
// folder is never resumed, only cleaned up.
type partial struct {
	name string // the temporary file name, the same for every attempt
	// left is true while something may still be at name.
	left bool
	// keep is true when a file stopped by a lost connection can be resumed.
	keep bool
	// offset is how much of a remote file is known to be written without
	// gaps. A local file is written in order, so its own length counts.
	offset int64
	// size and modTime describe the source the kept data came from.
	size    int64
	modTime time.Time
}

// matches reports whether the kept data can continue from source fi.
func (p *partial) matches(fi os.FileInfo) bool {
	return p.keep && fi.Size() == p.size && fi.ModTime().Equal(p.modTime)
}

func (p *partial) remember(fi os.FileInfo) {
	p.size, p.modTime = fi.Size(), fi.ModTime()
}

// lost reports whether the SFTP connection has stopped answering.
func lost(sc *sftp.Client) bool {
	_, err := sc.Getwd()
	return err != nil
}

// remoteStart is where writing the remote temporary file tmp continues; 0
// means from the beginning, after removing whatever is there.
func remoteStart(sc *sftp.Client, tmp string, source os.FileInfo, p *partial) (int64, error) {
	if p.matches(source) {
		fi, err := sc.Lstat(tmp)
		switch {
		case err == nil && fi.Mode().IsRegular():
			if from := min(p.offset, fi.Size(), source.Size()) - rewind; from > 0 {
				return from, nil
			}
		case err != nil && !errors.Is(err, os.ErrNotExist):
			return 0, err
		}
	}
	if err := sc.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	p.keep, p.left = false, false
	return 0, nil
}

// afterRemoteFailure keeps tmp for a retry when the connection was lost,
// and removes it otherwise. safe is how much of it has no gaps.
func afterRemoteFailure(ctx context.Context, sc *sftp.Client, tmp string, safe int64, connLost bool, p *partial) {
	if ctx.Err() == nil && connLost {
		p.keep, p.left, p.offset = true, true, safe
		return
	}
	err := sc.Remove(tmp)
	p.keep, p.left = false, err != nil && !errors.Is(err, os.ErrNotExist)
}

// task is a job and what it takes to run it again.
type task struct {
	job     Job
	replace bool // an upload that replaces an existing file
	part    partial
}

// bind looks at the source and destination as they are now and returns
// what runs t. It settles the job's final name, so it is called again
// before every attempt.
func (s *Service) bind(t *task) (func(context.Context, *meter) error, error) {
	switch t.job.Kind {
	case KindUpload:
		return s.bindUpload(t)
	case KindDownload:
		return s.bindDownload(t)
	case KindCopy:
		return s.bindCopy(t)
	}
	return nil, apperr.New(apperr.InvalidInput, "unknown transfer kind")
}

// newTask gives job an ID and a temporary name no other job uses, so one
// job can never remove or resume another's data.
func newTask(job Job, source string) (*task, error) {
	id, err := newID()
	if err != nil {
		return nil, err
	}
	job.ID = id
	return &task{job: job, part: partial{name: partName(source, id[:8])}}, nil
}

// Retry runs a failed job again as a new job. A single file stopped by a
// lost connection continues where it stopped if its source is unchanged;
// everything else starts over.
func (s *Service) Retry(_ context.Context, jobID string) (Job, error) {
	old, ok := s.takeFailed(jobID)
	if !ok {
		return Job{}, apperr.New(apperr.NotFound, "no such failed transfer")
	}
	next := *old
	run, err := s.bind(&next)
	if err == nil {
		next.job.ID, err = newID()
	}
	if err != nil {
		s.putFailed(old)
		return Job{}, mapErr("retry", err)
	}
	job := next.job
	s.start(&next, run) //nolint:contextcheck // a job outlives the call that starts it
	return job, nil
}

// Discard forgets a failed job and removes what it left behind.
func (s *Service) Discard(jobID string) error {
	t, ok := s.takeFailed(jobID)
	if !ok {
		return nil
	}
	return s.removePart(t)
}

func (s *Service) takeFailed(jobID string) (*task, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.failed[jobID]
	delete(s.failed, jobID)
	return t, ok
}

func (s *Service) putFailed(t *task) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failed[t.job.ID] = t
}

func (s *Service) removePart(t *task) error {
	if !t.part.left {
		return nil
	}
	if t.job.Kind == KindDownload {
		return mapErr("discard", os.RemoveAll(filepath.Join(s.opts.DownloadDir, t.part.name)))
	}
	sc, err := s.conns.SFTP(t.job.ServerID)
	if err != nil {
		return err
	}
	tmp := path.Join(path.Dir(t.job.Dst), t.part.name)
	fi, err := sc.Lstat(tmp)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
	case fi.IsDir():
		err = sc.RemoveAll(tmp)
	default:
		err = sc.Remove(tmp)
	}
	return mapErr("discard", err)
}
