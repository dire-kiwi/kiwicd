package server

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	"github.com/dire-kiwi/kiwicd/internal/protocol"
)

// Finished jobs' logs are moved out of the log_lines table into gzipped JSON
// lines files, one logLine per line. Each page of the log (see readLogs) is
// its own gzip member, and jobs.log_archive indexes the members by first seq
// and file offset, so reading a page decompresses at most one member before
// it. The members together are an ordinary gzip file.
//
// The index is recorded only once the file is in place, and the rows are
// deleted after that; readers check the index first, so they read a job's
// lines from the database until it is archived and from the archive after.

// archiveIndex holds {first seq, file offset} for each gzip member.
type archiveIndex [][2]int64

// logArchivePath is where a job's archived log lives. Archives are grouped
// a thousand jobs to a directory.
func (s *Server) logArchivePath(jobID int64) string {
	return filepath.Join(s.cfg.DataDir, "logs", strconv.FormatInt(jobID/1000, 10), strconv.FormatInt(jobID, 10)+".log.gz")
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// archiveJobLogs moves a finished job's log lines from the database into its
// archive file. It does nothing for jobs that are still running or have no
// lines, and for archived ones only finishes deleting the rows.
func (s *Server) archiveJobLogs(jobID int64) error {
	s.archiveMu.Lock()
	defer s.archiveMu.Unlock()
	ctx := context.Background()
	var status, archive string
	if err := s.db.QueryRow(`SELECT status, log_archive FROM jobs WHERE id = ?`, jobID).Scan(&status, &archive); err != nil {
		return err
	}
	if status != protocol.StatusCompleted {
		return nil
	}
	if archive != "" {
		return s.deleteJobLogs(ctx, jobID)
	}
	path := s.logArchivePath(jobID)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, fmt.Sprintf(".%d-*.tmp", jobID))
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // fails harmlessly once renamed
	defer f.Close()
	bw := bufio.NewWriter(f)
	cw := &countingWriter{w: bw}
	zw := gzip.NewWriter(cw)
	var index archiveIndex
	var last int64
	for {
		// A page at a time, encoded after its rows are closed.
		page, more, err := s.logPage(jobID, last, 0)
		if err != nil {
			return err
		}
		if len(page) == 0 {
			break
		}
		index = append(index, [2]int64{page[0].Seq, cw.n})
		zw.Reset(cw)
		enc := json.NewEncoder(zw)
		enc.SetEscapeHTML(false)
		for _, l := range page {
			if err := enc.Encode(l); err != nil {
				return err
			}
		}
		if err := zw.Close(); err != nil {
			return err
		}
		last = page[len(page)-1].Seq
		if !more {
			break
		}
	}
	if len(index) == 0 {
		return nil
	}
	if err := bw.Flush(); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
	// appendLogs refuses lines for completed jobs, so every row was copied.
	b, _ := json.Marshal(index)
	res, err := s.db.Exec(`UPDATE jobs SET log_archive = ? WHERE id = ?`, string(b), jobID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Deleted while it was being archived.
		s.removeLogArchives([]int64{jobID})
		return nil
	}
	if err := s.deleteJobLogs(ctx, jobID); err != nil {
		return err
	}
	s.log.Debug("job logs archived", "job", jobID, "lines", last, "path", path)
	return nil
}

// archivePendingLogs archives finished jobs whose lines are still in the
// database, e.g. because the server stopped before it got to them.
func (s *Server) archivePendingLogs() {
	rows, err := s.db.Query(`SELECT id FROM jobs j WHERE status = ?
		AND EXISTS (SELECT 1 FROM log_lines WHERE job_id = j.id)`, protocol.StatusCompleted)
	if err != nil {
		s.log.Error("find unarchived job logs", "err", err)
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		_ = rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		if err := s.archiveJobLogs(id); err != nil {
			s.log.Error("archive job logs", "job", id, "err", err)
		}
	}
}

// readLogArchive is readLogs for an archived job, given its index.
func (s *Server) readLogArchive(jobID int64, archive string, after int64, limit int, fn func(logLine)) (more bool, err error) {
	var index archiveIndex
	if err := json.Unmarshal([]byte(archive), &index); err != nil {
		return false, fmt.Errorf("log archive index for job %d: %w", jobID, err)
	}
	if len(index) == 0 {
		return false, nil
	}
	// Start at the last member that begins at or before the first line wanted.
	i := max(sort.Search(len(index), func(i int) bool { return index[i][0] > after+1 })-1, 0)
	f, err := os.Open(s.logArchivePath(jobID))
	if errors.Is(err, os.ErrNotExist) {
		s.log.Warn("job log archive is missing", "job", jobID)
		return false, nil
	} else if err != nil {
		return false, err
	}
	defer f.Close()
	if _, err := f.Seek(index[i][1], io.SeekStart); err != nil {
		return false, err
	}
	zr, err := gzip.NewReader(bufio.NewReader(f))
	if err != nil {
		return false, fmt.Errorf("log archive for job %d: %w", jobID, err)
	}
	dec := json.NewDecoder(zr)
	n, size := 0, 0
	for {
		var l logLine
		if err := dec.Decode(&l); errors.Is(err, io.EOF) {
			return false, nil
		} else if err != nil {
			return false, fmt.Errorf("log archive for job %d: %w", jobID, err)
		}
		if l.Seq <= after {
			continue
		}
		if n == limit || size >= logPageBytes {
			return true, nil
		}
		fn(l)
		n++
		size += len(l.Text)
	}
}

// removeLogArchives deletes the archive files of jobs that have been deleted
// from the database.
func (s *Server) removeLogArchives(jobIDs []int64) {
	for _, id := range jobIDs {
		if err := os.Remove(s.logArchivePath(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.log.Warn("remove job log archive", "job", id, "err", err)
		}
	}
}
