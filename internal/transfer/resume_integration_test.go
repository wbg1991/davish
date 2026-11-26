//go:build integration

package transfer_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"davish/internal/shellx"
	"davish/internal/transfer"
)

const resumeSize = 48 << 20

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func (e env) remoteSum(t *testing.T, p string) string {
	t.Helper()
	return strings.Fields(e.run(t, "sha256sum "+shellx.Quote(p)))[0]
}

// dropAt disconnects the server once a job has moved n bytes.
func (e env) dropAt(n int64) {
	var once sync.Once
	e.rec.mu.Lock()
	defer e.rec.mu.Unlock()
	e.rec.onProgress = func(p transfer.Progress) {
		if p.Done >= n {
			once.Do(func() { e.m.Disconnect(e.id) })
		}
	}
}

// interrupted waits for job to fail, then reconnects.
func (e env) interrupted(t *testing.T, job transfer.Job) {
	t.Helper()
	if d := e.rec.wait(t, job.ID); d.Error == nil {
		t.Fatalf("done = %+v; want a failure", d)
	}
	e.rec.mu.Lock()
	e.rec.onProgress, e.rec.progress = nil, nil
	e.rec.mu.Unlock()
	if err := e.m.Connect(context.Background(), e.id); err != nil {
		t.Fatal(err)
	}
}

// retry runs job again and returns the first byte count it reported, which
// is above 0 only when it resumed.
func (e env) retry(t *testing.T, job transfer.Job) (transfer.Job, int64) {
	t.Helper()
	next, err := e.svc.Retry(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	ok(t, e.rec.wait(t, next.ID))
	e.rec.mu.Lock()
	defer e.rec.mu.Unlock()
	for _, p := range e.rec.progress {
		// The first report carries the total, before any resumed bytes.
		if p.JobID == next.ID && p.Done > 0 {
			return next, p.Done
		}
	}
	t.Fatal("no progress reported")
	return next, 0
}

func TestUploadResumesAfterLostConnection(t *testing.T) {
	e := setup(t)
	data := randomBytes(t, resumeSize)
	src := filepath.Join(e.local, "big.bin")
	writeLocal(t, src, data, 0o640)

	e.dropAt(resumeSize / 2)
	jobs, err := e.svc.Upload(context.Background(), e.id, e.remote, []transfer.UploadRequest{{Local: src}})
	if err != nil {
		t.Fatal(err)
	}
	e.interrupted(t, jobs[0])
	if got := e.ls(t); !strings.HasSuffix(got, ".davish-part") {
		t.Fatalf("after the drop: %q; want only the partial file", got)
	}

	_, first := e.retry(t, jobs[0])
	if first < 1<<20 {
		t.Errorf("first progress = %d; want a resume", first)
	}
	dst := path.Join(e.remote, "big.bin")
	if got := e.remoteSum(t, dst); got != sum(data) {
		t.Error("resumed upload differs from the source")
	}
	if got := e.run(t, "stat -c %a "+shellx.Quote(dst)); got != "640" && os.PathSeparator == '/' {
		t.Errorf("mode = %s", got)
	}
	if got := e.ls(t); got != "big.bin" {
		t.Errorf("remote folder: %q", got)
	}
}

func TestUploadStartsOverWhenSourceChanged(t *testing.T) {
	e := setup(t)
	src := filepath.Join(e.local, "big.bin")
	writeLocal(t, src, randomBytes(t, resumeSize), 0o644)

	e.dropAt(resumeSize / 2)
	jobs, err := e.svc.Upload(context.Background(), e.id, e.remote, []transfer.UploadRequest{{Local: src}})
	if err != nil {
		t.Fatal(err)
	}
	e.interrupted(t, jobs[0])

	data := randomBytes(t, resumeSize+1)
	writeLocal(t, src, data, 0o644)
	e.retry(t, jobs[0])
	if got := e.remoteSum(t, path.Join(e.remote, "big.bin")); got != sum(data) {
		t.Error("upload of the changed source differs from it")
	}
	if got := e.ls(t); got != "big.bin" {
		t.Errorf("remote folder: %q", got)
	}
}

func TestDownloadResumesAfterLostConnection(t *testing.T) {
	e := setup(t)
	remote := path.Join(e.remote, "big.bin")
	e.run(t, "head -c 48000000 /dev/urandom > "+shellx.Quote(remote))

	e.dropAt(20 << 20)
	jobs, err := e.svc.Download(context.Background(), e.id, []string{remote})
	if err != nil {
		t.Fatal(err)
	}
	e.interrupted(t, jobs[0])

	_, first := e.retry(t, jobs[0])
	if first < 1<<20 {
		t.Errorf("first progress = %d; want a resume", first)
	}
	data, err := os.ReadFile(filepath.Join(e.dl, "big.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if sum(data) != e.remoteSum(t, remote) {
		t.Error("resumed download differs from the source")
	}
	if names := localNames(t, e.dl); names != "big.bin" {
		t.Errorf("download folder: %s", names)
	}
}

func TestCopyResumesAfterLostConnection(t *testing.T) {
	e := setup(t)
	from := path.Join(e.remote, "big.bin")
	into := path.Join(e.remote, "into")
	e.run(t, "mkdir "+shellx.Quote(into)+" && head -c 48000000 /dev/urandom > "+shellx.Quote(from))

	e.dropAt(20 << 20)
	jobs, err := e.svc.Copy(context.Background(), e.id, []string{from}, e.id, into)
	if err != nil {
		t.Fatal(err)
	}
	e.interrupted(t, jobs[0])

	_, first := e.retry(t, jobs[0])
	if first < 1<<20 {
		t.Errorf("first progress = %d; want a resume", first)
	}
	if e.remoteSum(t, path.Join(into, "big.bin")) != e.remoteSum(t, from) {
		t.Error("resumed copy differs from the source")
	}
	if got := e.run(t, "ls -A "+shellx.Quote(into)); got != "big.bin" {
		t.Errorf("destination folder: %q", got)
	}
}

func TestFolderRetryAndDiscard(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	src := filepath.Join(e.local, "site")
	data := randomBytes(t, resumeSize)
	writeLocal(t, filepath.Join(src, "big.bin"), data, 0o644)
	file := filepath.Join(e.local, "one.bin")
	writeLocal(t, file, data, 0o644)

	// A folder starts over, leaving nothing of the failed attempt.
	e.dropAt(resumeSize / 2)
	jobs, err := e.svc.Upload(ctx, e.id, e.remote, []transfer.UploadRequest{{Local: src}})
	if err != nil {
		t.Fatal(err)
	}
	e.interrupted(t, jobs[0])
	if _, err := e.svc.Retry(ctx, "nope"); err == nil {
		t.Error("retrying an unknown job succeeded")
	}
	next, err := e.svc.Retry(ctx, jobs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	ok(t, e.rec.wait(t, next.ID))
	if got := e.ls(t); got != "site" {
		t.Errorf("remote folder: %q", got)
	}

	// Discarding a failed job removes what it left.
	e.dropAt(resumeSize / 2)
	jobs, err = e.svc.Upload(ctx, e.id, e.remote, []transfer.UploadRequest{{Local: file}})
	if err != nil {
		t.Fatal(err)
	}
	e.interrupted(t, jobs[0])
	if err := e.svc.Discard(jobs[0].ID); err != nil {
		t.Fatal(err)
	}
	if got := e.ls(t); got != "site" {
		t.Errorf("after discard: %q", got)
	}
}
